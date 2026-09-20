//go:build unix

// Package procgroup records the process groups belay starts, so that a belay
// killed with SIGKILL can find and kill them when its run is resumed.
//
// # Why this exists
//
// internal/exec starts every process belay runs (the coding agent, the test
// suite, a linter) as the leader of a process group of its own, and kills that
// group when the process's context is cancelled or its deadline passes. That
// holds only while belay is alive to do it. A belay killed with SIGKILL, or
// quit by a second Ctrl-C, dies with those groups still running. They are not
// in the terminal's foreground group, so the terminal's SIGINT never reaches
// them, and the kernel hands them to init. An agent CLI left that way keeps
// spending, with nobody watching it and no budget ledger counting.
//
// So every process belay starts for a node is recorded on disk, in that node
// execution's evidence directory, until belay has reaped it. When the
// dispatcher heals a run whose last attempt a crash left open, it calls Reap,
// which kills every group still recorded.
//
// # When the record is written
//
// A reservation naming the command is written before the process starts, and
// the group is filled in as soon as it has started, before belay waits on it.
// The group cannot be written any earlier: its id is the new process's pid,
// and the kernel only assigns that at fork. A belay killed between the fork
// and the second write leaves a reservation with no pid in it. Reap reports
// it and does nothing else, because it names a command that may still be
// running but no process belay can prove is the one it started.
//
// The writes are not fsynced. The threat is belay being killed, and what a
// killed process wrote is already in the kernel's page cache, which outlives
// it. Only a crash of the whole machine loses those writes, and that ends the
// recorded processes too.
//
// # Never signal a process belay did not start
//
// A pid is handed out again once its process has exited and been reaped.
// Between belay's death and the resume that reaps, a recorded process may
// have ended, and its pid, which is also its group id, may now belong to
// something unrelated. Killing that would be far worse than the leak this
// package exists to close. So each record carries the process's start time as
// the platform reports it, and Reap kills only when the process now holding
// the pid reports the same start time. A record it cannot check (one with no
// pid, no start time, or damaged) is reported and removed, never acted on.
//
// On Linux the start time is the starttime field of /proc/<pid>/stat, in clock
// ticks since boot, qualified by the boot id so that a reboot cannot make two
// processes agree. Elsewhere it is what `ps -o lstart=` prints in UTC, which
// has one-second resolution. That is enough: a pid is reused within the same
// second only if every other pid was handed out in between, because Linux and
// macOS both allocate pids in sequence, and neither can start that many
// processes in a second. Reading the start time through sysctl on macOS would
// give microseconds without a subprocess, but the standard library cannot
// address kern.proc.pid.<pid>, and golang.org/x/sys is not a dependency.
//
// A process that belay started but that shares a group it does not lead,
// which happens only if the platform refused Setpgid, is killed alone. Its
// group may be belay's own.
//
// # Limits
//
//   - A group whose leader has exited is not chased, even if others in it live
//     on: with the leader gone, nothing proves the group is still the one belay
//     started. internal/exec makes the same choice for a child that exits
//     cleanly and leaves descendants behind.
//   - Only the processes belay starts are recorded. One that the agent moves
//     into a session of its own is outside every group belay knows about.
//
// # Why a context value
//
// The directory reaches internal/exec on the context (WithDir), not in a
// field. Every process belay starts goes through internal/exec, but by way of
// an adapter (the agent backend, a test runner, a linter) behind interfaces
// that pkg/belay promises to keep stable. A field would have to be threaded
// through all of them, and an adapter written later that forgot it would
// reopen the leak without a word. On the context, the dispatcher sets it once
// per node execution, and every process the node starts is recorded.
package procgroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// recordPrefix and recordGlob name a record file inside a node's evidence
// directory.
const (
	recordPrefix = "pgroup-"
	recordGlob   = recordPrefix + "*.json"
)

// errNoProcess reports that no process holds a pid. startTime returns it, so
// that Reap can tell a process that is gone from one it could not check.
var errNoProcess = errors.New("procgroup: no such process")

// dirKey is the context key WithDir stores the directory under.
type dirKey struct{}

// WithDir returns a copy of ctx that tells internal/exec to record, in dir,
// every process it starts under that context.
func WithDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, dirKey{}, dir)
}

// DirFrom returns the directory WithDir put in ctx, or "" when there is none,
// in which case nothing is recorded.
func DirFrom(ctx context.Context) string {
	dir, ok := ctx.Value(dirKey{}).(string)
	if !ok {
		return ""
	}
	return dir
}

// Record is one process belay started, as it is written to disk.
type Record struct {
	// PID is the process belay started, or 0 in a reservation written before
	// it started.
	PID int `json:"pid"`
	// PGID is the process group PID belongs to. It equals PID when the
	// process leads its own group, as internal/exec asks every process to.
	PGID int `json:"pgid"`
	// Start is the process's start time as the platform reports it. Reap
	// compares it, as an opaque string, with what the process holding PID
	// reports then.
	Start string `json:"start,omitempty"`
	// Argv is the command, already redacted by internal/exec, so that a
	// report can say what was left running.
	Argv []string `json:"argv"`
}

// Tracked is the record of one process from before it starts until belay has
// reaped it. Track makes one.
type Tracked struct {
	path string
	rec  Record
}

// Track writes a reservation for a process about to start with argv, in dir,
// which it creates if need be. Call Started once the process has started, and
// Done once it has been reaped.
//
// internal/exec refuses to start a process Track could not record, because
// belay could not stop that process after a crash.
func Track(dir string, argv []string) (*Tracked, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("procgroup: create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, recordPrefix+"*.json")
	if err != nil {
		return nil, fmt.Errorf("procgroup: create record in %s: %w", dir, err)
	}
	t := &Tracked{path: f.Name(), rec: Record{Argv: slices.Clone(argv)}}
	data, err := json.Marshal(t.rec)
	if err == nil {
		_, err = f.Write(data)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("procgroup: write %s: %w", t.path, err), t.Done())
	}
	return t, nil
}

// Started fills in the process's group and start time, replacing the
// reservation.
//
// It writes whatever it learned even when it returns an error: a record with
// a pid and no start time still lets a report name the process, although Reap
// will not act on it.
func (t *Tracked) Started(pid int) error {
	t.rec.PID = pid
	var errs []error
	if pgid, err := syscall.Getpgid(pid); err != nil {
		errs = append(errs, fmt.Errorf("procgroup: process group of %d: %w", pid, err))
	} else {
		t.rec.PGID = pgid
	}
	if start, err := startTime(pid); err != nil {
		errs = append(errs, fmt.Errorf("procgroup: start time of %d: %w", pid, err))
	} else {
		t.rec.Start = start
	}
	if err := t.write(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// write replaces the record through a rename, so that a belay killed part-way
// leaves the previous version whole rather than a torn one.
func (t *Tracked) write() error {
	data, err := json.Marshal(t.rec)
	if err != nil {
		return fmt.Errorf("procgroup: encode record: %w", err)
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("procgroup: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, t.path); err != nil {
		return errors.Join(fmt.Errorf("procgroup: replace %s: %w", t.path, err), removeIfPresent(tmp))
	}
	return nil
}

// Done removes the record. Call it once the process has been reaped, and not
// before: the record is what finds the group if belay dies first.
func (t *Tracked) Done() error {
	return removeIfPresent(t.path)
}

// Outcome is what Reap did about one record.
type Outcome int

// The outcomes of reaping one record.
const (
	// OutcomeKilled means the recorded process was still the one belay
	// started, and its group, or the process alone if it did not lead one,
	// was sent SIGKILL.
	OutcomeKilled Outcome = iota + 1
	// OutcomeGone means no process holds the recorded pid any more.
	OutcomeGone
	// OutcomeRecycled means a process holds the recorded pid but started at
	// another time: it is not the one belay started, and was not signalled.
	OutcomeRecycled
	// OutcomeUnverifiable means the record names no process belay can check
	// (a reservation, one with no start time, or a damaged file), or the check
	// or the kill failed; Reaped.Err says which. Nothing was signalled.
	OutcomeUnverifiable
)

// String names o for a log line.
func (o Outcome) String() string {
	switch o {
	case OutcomeKilled:
		return "killed"
	case OutcomeGone:
		return "gone"
	case OutcomeRecycled:
		return "pid reused by another process"
	case OutcomeUnverifiable:
		return "unverifiable"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// Reaped is Reap's report on one record.
type Reaped struct {
	// Path is the record file.
	Path string
	// Record is what the file held, as far as it could be read.
	Record Record
	// Outcome is what Reap did.
	Outcome Outcome
	// Err says why a record was unverifiable, or why its file could not be
	// removed.
	Err error
}

// Reap kills every process group recorded in a directory directly under root
// whose leader is still the process belay started, and removes every record
// it finds, whatever it did about it: a record left behind would be acted on
// again by the next resume. root is a run's nodes/ directory, whose children
// are the node executions' evidence directories.
//
// It reports what it did rather than failing. Reaping is part of healing a
// run, and a record belay cannot act on is no reason to refuse to continue
// the run; the caller logs the report.
func Reap(root string) []Reaped {
	// The pattern is a constant, and Glob's only error is ErrBadPattern.
	paths, err := filepath.Glob(filepath.Join(root, "*", recordGlob))
	if err != nil {
		return []Reaped{{Path: root, Outcome: OutcomeUnverifiable, Err: err}}
	}
	out := make([]Reaped, 0, len(paths))
	for _, path := range paths {
		r := reapOne(path)
		if err := removeIfPresent(path); err != nil {
			r.Err = errors.Join(r.Err, err)
		}
		out = append(out, r)
	}
	return out
}

// reapOne acts on the record at path, and says what it did.
func reapOne(path string) Reaped {
	r := Reaped{Path: path, Outcome: OutcomeUnverifiable}
	data, err := os.ReadFile(path) //nolint:gosec // path came from a glob under the run's own nodes/ directory
	if err != nil {
		r.Err = fmt.Errorf("procgroup: read %s: %w", path, err)
		return r
	}
	if err := json.Unmarshal(data, &r.Record); err != nil {
		r.Err = fmt.Errorf("procgroup: parse %s: %w", path, err)
		return r
	}
	rec := r.Record
	if rec.PID <= 0 || rec.Start == "" {
		r.Err = fmt.Errorf("procgroup: %s names no process whose start time was recorded", path)
		return r
	}

	now, err := startTime(rec.PID)
	switch {
	case errors.Is(err, errNoProcess):
		r.Outcome = OutcomeGone
		return r
	case err != nil:
		r.Err = fmt.Errorf("procgroup: start time of %d: %w", rec.PID, err)
		return r
	case now != rec.Start:
		r.Outcome = OutcomeRecycled
		return r
	}

	// A negative pid signals the whole group, which is only safe for a
	// group the process leads.
	target := rec.PID
	if rec.PGID == rec.PID {
		target = -rec.PID
	}
	switch err := syscall.Kill(target, syscall.SIGKILL); {
	case errors.Is(err, syscall.ESRCH):
		r.Outcome = OutcomeGone
	case err != nil:
		r.Err = fmt.Errorf("procgroup: kill %d: %w", target, err)
	default:
		r.Outcome = OutcomeKilled
	}
	return r
}

// removeIfPresent removes path, treating a file that is already gone as
// removed.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("procgroup: remove %s: %w", path, err)
	}
	return nil
}
