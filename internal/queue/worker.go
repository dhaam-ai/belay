//go:build unix

package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Copier makes the copy of the workspace one goal's run works in, and clears
// away what a crash left of one.
//
// internal/isolate/dircopy's Isolator is one, rooted at Store.WorkRoot.
// Create must put the copy at that root joined with id: that is the path the
// goal's claim records, before the copy exists.
type Copier interface {
	// Create copies src into a new directory named id. It refuses one that
	// already holds anything, rather than blend two trees.
	Create(ctx context.Context, src, id string) (belay.Workspace, error)
	// Reclaim removes whatever is under id, so that id can be created
	// again. Finding nothing there is not an error.
	Reclaim(ctx context.Context, id string) error
}

// Driver is what a worker needs from the rest of belay to run one goal.
//
// The split is the point. The worker knows the queue: which goal is next, and
// the order its steps must happen in for a crash at any moment to be
// recoverable. The driver knows runs: how one is made in a copy, how it reads
// on disk, and how it is dispatched, with the configuration, the adapters,
// the dispatcher and the words a person is told, all of which live in
// internal/cli. Neither has to trust the other's memory, because each step
// starts from the disk.
type Driver interface {
	// Observe reports what goal e's run looks like on disk. An error means
	// it could not tell, and stops the worker with e still in hand.
	Observe(e Entry) (RunView, error)
	// Create makes goal e's run, e.RunID, in its copy, e.Dir, which the
	// worker has just made. It must refuse a run that already exists rather
	// than start it over.
	Create(ctx context.Context, e Entry) error
	// Drive dispatches goal e's run once, continuing it from wherever it
	// stands, and reports how it ended. An error means the run could not be
	// dispatched at all, and stops the worker with e still in hand.
	Drive(ctx context.Context, e Entry) (Outcome, error)
}

// Outcome is how one drive of a goal's run ended.
type Outcome struct {
	// Interrupted is set when the worker's context was cancelled while the
	// run was driven. The run is paused where it stood, and the goal stays in
	// hand for the next worker, which continues it before anything else.
	Interrupted bool
	// State is what the goal settles as otherwise, and Note is what a person
	// reading the queue needs to know about it.
	State State
	Note  string
}

// Worker works through one queue: one goal at a time, in order, each in a
// copy of its own with a run of its own, until nothing is left that it may
// run. See the package documentation for the order of its steps and why.
type Worker struct {
	// Store is the queue.
	Store *Store
	// Source is the workspace each goal's copy is made from, as it stands
	// when the goal is claimed.
	Source string
	// Copier makes each goal's copy, rooted at Store.WorkRoot.
	Copier Copier
	// Driver makes and drives each goal's run.
	Driver Driver
	// NewRunID names a goal's run when it is claimed. Nil uses
	// state.NewRunID.
	NewRunID func() (string, error)
	// Logger receives the worker's own log. Nil uses slog.Default.
	Logger *slog.Logger
}

// Report is what one Work did.
type Report struct {
	// Settled holds each goal this Work settled, as it settled it, in order.
	Settled []Entry
	// Stop is why it stopped.
	Stop Stop
	// PreviousCutOff is set when the worker before this one never recorded
	// stopping: a crash, a kill or a reboot ended it first, and this one
	// picked up whatever it had in hand.
	PreviousCutOff bool
}

// Work takes the queue and works through it until nothing is left it may run,
// ctx is cancelled, or a step cannot be done.
//
// It refuses, with an *ActiveError and without writing anything, while
// another worker holds the queue. It records worker_started, then repeats one
// step at a time — read the queue, look at the run of the goal in hand, ask
// Next, act — until Next says it is done. Every step begins from the disk,
// never from what the step before it left in memory, which is what makes a
// worker that is killed and a new one started over the same disk the same
// worker.
//
// A cancelled ctx is an interruption, not an error: Work records
// worker_stopped and returns a nil error, with the goal in hand still in hand.
// A step that cannot be done stops it with that step's error, recorded in the
// log, and again leaves the goal in hand, so the next start tries the same
// goal rather than losing it. A worker that is killed records nothing at all,
// and the next one reports it was cut off.
func (w *Worker) Work(ctx context.Context) (Report, error) {
	lock, err := w.Store.LockWorker()
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = lock.Release() }()

	before, err := w.Store.Read()
	if err != nil {
		return Report{}, err
	}
	report := Report{PreviousCutOff: !before.Last.At.IsZero() && !before.Last.Ended}
	if err := w.Store.workerStarted(os.Getpid()); err != nil {
		return report, err
	}

	for {
		if ctx.Err() != nil {
			return w.end(report, StopInterrupted, "")
		}
		stop, done, err := w.step(ctx, &report)
		switch {
		case err != nil && ctx.Err() != nil:
			// The step failed because it was cancelled part way, which is
			// an interruption like any other: what it left is in hand for
			// the next worker.
			return w.end(report, StopInterrupted, "")
		case err != nil:
			return w.fail(report, err)
		case done:
			return w.end(report, stop, "")
		}
	}
}

// step takes one step: reads the queue, looks at the runs of the goals in
// hand, and does what Next says. done is set when the worker is to stop, for
// the reason in stop.
func (w *Worker) step(ctx context.Context, report *Report) (stop Stop, done bool, err error) {
	q, err := w.Store.Read()
	if err != nil {
		return StopUnknown, false, err
	}
	runs, err := w.observe(q)
	if err != nil {
		return StopUnknown, false, err
	}

	d := Next(q, runs)
	w.logger().Debug("queue: next step", slog.String("action", d.Action.String()), slog.Int("goal", d.Entry.ID))
	switch d.Action {
	case ActionDone:
		return StopDrained, true, nil
	case ActionClaim:
		return StopUnknown, false, w.claim(d.Entry)
	case ActionCreate:
		return StopUnknown, false, w.create(ctx, d.Entry)
	case ActionSettle:
		return StopUnknown, false, w.settle(report, d.Entry, d.Settle, d.Note)
	case ActionDrive:
		out, err := w.Driver.Drive(ctx, d.Entry)
		if err != nil {
			return StopUnknown, false, fmt.Errorf("goal %d: %w", d.Entry.ID, err)
		}
		if out.Interrupted {
			return StopInterrupted, true, nil
		}
		return StopUnknown, false, w.settle(report, d.Entry, out.State, out.Note)
	default:
		return StopUnknown, false, fmt.Errorf("queue: no step for %s", d.Action)
	}
}

// observe reads the run of every goal in hand: claimed, or approved and not
// yet driven. No other goal's run is read, since Next acts on no other.
func (w *Worker) observe(q Queue) (map[int]RunView, error) {
	runs := map[int]RunView{}
	for _, e := range q.Entries {
		if e.State != StateClaimed && e.State != StateApproved {
			continue
		}
		view, err := w.Driver.Observe(e)
		if err != nil {
			return nil, fmt.Errorf("goal %d: cannot read its run %s: %w", e.ID, e.RunID, err)
		}
		runs[e.ID] = view
	}
	return runs, nil
}

// claim takes goal e, recording the run it will be and the copy it will run
// in before either exists. Nothing is made here; the next step makes them.
func (w *Worker) claim(e Entry) error {
	runID, err := w.newRunID()
	if err != nil {
		return err
	}
	// Named for its run, which is unique, rather than for the goal's id: a
	// queue whose log was deleted and begun again gives out ids from 1, and
	// a copy named for an id could then be one an earlier goal left, holding
	// work nobody has taken yet, which the next step would clear away.
	dir := filepath.Join(w.Store.WorkRoot(), runID)
	claimed, err := w.Store.claim(e.ID, runID, dir)
	if err != nil {
		return err
	}
	w.logger().Info("queue: claimed a goal", slog.Int("goal", claimed.ID), slog.String("run", runID))
	return nil
}

// create makes the copy and the run a claim names. It is called only for a
// claimed goal whose run does not exist, which means nothing has run in its
// copy, whatever is there: so whatever a crash left there, a copy cut short
// or a whole copy whose run was never finished being made, is cleared away,
// and both are made afresh, from the workspace as it stands now.
//
// Clearing deletes a directory, so create looks at the run again itself
// first, rather than trusting what was seen when the step was decided, and
// refuses if the run is there by now. Under the worker lock nothing else
// makes one in between; the second look is what keeps that an assumption
// this function does not have to be right about.
func (w *Worker) create(ctx context.Context, e Entry) error {
	if view, err := w.Driver.Observe(e); err != nil {
		return fmt.Errorf("goal %d: cannot read its run %s: %w", e.ID, e.RunID, err)
	} else if view.Exists {
		return fmt.Errorf("goal %d: its run %s exists, so belay will not clear %s to make it again", e.ID, e.RunID, e.Dir)
	}
	if err := w.Copier.Reclaim(ctx, e.RunID); err != nil {
		return fmt.Errorf("goal %d: cannot clear what was left at %s: %w", e.ID, e.Dir, err)
	}
	if _, err := w.Copier.Create(ctx, w.Source, e.RunID); err != nil {
		return fmt.Errorf("goal %d: cannot copy %s for its run: %w", e.ID, w.Source, err)
	}
	if err := w.Driver.Create(ctx, e); err != nil {
		return fmt.Errorf("goal %d: cannot make its run %s: %w", e.ID, e.RunID, err)
	}
	return nil
}

// settle records goal e as s, and adds it to the report.
func (w *Worker) settle(report *Report, e Entry, s State, note string) error {
	settled, err := w.Store.settle(e.ID, s, note)
	if err != nil {
		return err
	}
	w.logger().Info("queue: settled a goal", slog.Int("goal", settled.ID), slog.String("state", s.String()))
	report.Settled = append(report.Settled, settled)
	return nil
}

// end records that the worker stopped, for stop, and returns the report.
func (w *Worker) end(report Report, stop Stop, note string) (Report, error) {
	report.Stop = stop
	if err := w.Store.workerStopped(stop, note); err != nil {
		return report, err
	}
	return report, nil
}

// fail records that the worker could not go on, and why, and returns cause.
func (w *Worker) fail(report Report, cause error) (Report, error) {
	report, err := w.end(report, StopFailed, cause.Error())
	return report, errors.Join(cause, err)
}

// newRunID names a run.
func (w *Worker) newRunID() (string, error) {
	if w.NewRunID != nil {
		return w.NewRunID()
	}
	return state.NewRunID()
}

// logger returns the worker's logger.
func (w *Worker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}
