//go:build unix

package queue

import (
	"errors"
	"fmt"
	"time"
)

// ErrCorruptQueue reports a queue log whose records do not form a history any
// belay writes: a gap in its sequence, an id given out twice, or a goal moved
// in a way it cannot move, such as a second claim. errors.Is matches every
// *CorruptionError against it.
//
// A log in that state is refused rather than read around. Reading around a
// second claim is exactly how a goal would run twice.
var ErrCorruptQueue = errors.New("queue: corrupt")

// CorruptionError names the record at which a queue log stops making sense,
// and why. errors.Is matches it against ErrCorruptQueue.
type CorruptionError struct {
	// Seq is the record at which the history went wrong.
	Seq uint64
	// Reason says what was wrong with it, in words safe to show a person.
	Reason string
}

func (e *CorruptionError) Error() string {
	return fmt.Sprintf("queue: corrupt at seq %d: %s", e.Seq, e.Reason)
}

// Unwrap exposes ErrCorruptQueue for errors.Is.
func (e *CorruptionError) Unwrap() error { return ErrCorruptQueue }

// corrupt builds a *CorruptionError.
func corrupt(seq uint64, format string, a ...any) error {
	return &CorruptionError{Seq: seq, Reason: fmt.Sprintf(format, a...)}
}

// Entry is one goal in the queue, as its log reads.
type Entry struct {
	// ID names the goal, as `belay queue add` printed it and `belay queue
	// approve` and `belay queue rm` take it.
	ID int
	// Goal is what the person asked for, exactly as they typed it.
	Goal string
	// NoApproval is whether the person asked, when they added the goal, for
	// its plan not to wait for review.
	NoApproval bool
	// State is where the goal stands.
	State State
	// AddedAt is when the goal was added, and UpdatedAt when anything last
	// happened to it.
	AddedAt   time.Time
	UpdatedAt time.Time
	// RunID and Dir are the goal's run and the copy it works in, from the
	// claim, and empty until then.
	RunID string
	Dir   string
	// Note is what the last settle said about the goal.
	Note string
}

// apply moves e on by one record about it, or says why the record cannot
// apply to e as it stands.
//
// Each case is one edge of the goal's life, and every other edge is refused.
// The claim is the one that matters most: only a queued goal can be claimed,
// so no history in which a goal was claimed twice survives a fold.
func (e *Entry) apply(r Record) error {
	switch r.Event {
	case EventRemoved:
		if e.State != StateQueued {
			return fmt.Errorf("goal %d was removed while %s; only a queued goal can be", e.ID, e.State)
		}
		e.State = StateRemoved
	case EventClaimed:
		if e.State != StateQueued {
			return fmt.Errorf("goal %d was claimed while %s; a goal is claimed once, from the queue", e.ID, e.State)
		}
		e.State, e.RunID, e.Dir = StateClaimed, r.RunID, r.Dir
	case EventSettled:
		if e.State != StateClaimed && e.State != StateApproved {
			return fmt.Errorf("goal %d was settled while %s; only a goal a worker has in hand can be", e.ID, e.State)
		}
		e.State, e.Note = r.State, r.Note
	case EventApproved:
		if e.State != StateWaiting {
			return fmt.Errorf("goal %d was approved while %s; only a goal waiting at its plan can be", e.ID, e.State)
		}
		e.State = StateApproved
	default:
		return fmt.Errorf("%s does not change a goal", r.Event)
	}
	e.UpdatedAt = r.Time
	return nil
}

// Start is the last time `belay queue start` began working through the
// queue, and how that ended, if it did.
type Start struct {
	// At is when it began, and PID which process it was. Both are zero when
	// no worker has ever started.
	At  time.Time
	PID int
	// PreviousCutOff is set when the start before this one never recorded
	// stopping: a crash, a kill or a reboot ended it first.
	PreviousCutOff bool
	// Ended is set once it recorded stopping, at EndedAt, for the reason in
	// Stop, with Note saying more. A start that has not ended is either
	// still working, or was cut off; only the worker lock can tell which.
	Ended   bool
	EndedAt time.Time
	Stop    Stop
	Note    string
}

// Queue is a queue as its log reads: every goal ever added, in the order it
// was added, and what the last worker did.
type Queue struct {
	// Entries holds every goal, removed ones included, in id order.
	Entries []Entry
	// Last is the most recent start of a worker.
	Last Start
}

// Entry returns the goal with id, and whether there is one.
func (q Queue) Entry(id int) (Entry, bool) {
	for _, e := range q.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// fold reads a queue's history into the queue it describes.
//
// It refuses, with a *CorruptionError, any history no belay writes: a record
// out of sequence, a record that is not valid on its own, an id added twice
// or out of order, an event about a goal nobody added, and any event that
// moves a goal in a way it cannot move (see Entry.apply). It never repairs or
// skips one. Every decision the queue makes is a reading of this fold, so a
// fold that guessed would make the guess on the queue's behalf, and the guess
// that matters is whether a goal has already been claimed.
func fold(records []Record) (Queue, error) {
	var (
		q       Queue
		prevSeq uint64
		lastID  int
		index   = map[int]int{}
	)
	for _, r := range records {
		if r.Seq != prevSeq+1 {
			return Queue{}, corrupt(r.Seq, "seq %d out of order: expected %d", r.Seq, prevSeq+1)
		}
		prevSeq = r.Seq
		if err := r.validate(); err != nil {
			return Queue{}, corrupt(r.Seq, "%v", err)
		}

		switch r.Event {
		case EventWorkerStarted:
			previous := q.Last
			q.Last = Start{At: r.Time, PID: r.PID, PreviousCutOff: !previous.At.IsZero() && !previous.Ended}
		case EventWorkerStopped:
			q.Last.Ended, q.Last.EndedAt, q.Last.Stop, q.Last.Note = true, r.Time, r.Stop, r.Note
		case EventAdded:
			// Ids only ever grow, which is what keeps one from naming two
			// goals: a later goal never inherits an earlier one's copy, run
			// or approval.
			if r.ID <= lastID {
				return Queue{}, corrupt(r.Seq, "goal %d was added after goal %d; ids only ever grow", r.ID, lastID)
			}
			lastID = r.ID
			index[r.ID] = len(q.Entries)
			q.Entries = append(q.Entries, Entry{
				ID: r.ID, Goal: r.Goal, NoApproval: r.NoApproval, State: StateQueued,
				AddedAt: r.Time, UpdatedAt: r.Time,
			})
		default:
			i, ok := index[r.ID]
			if !ok {
				return Queue{}, corrupt(r.Seq, "%s for goal %d, which was never added", r.Event, r.ID)
			}
			if err := q.Entries[i].apply(r); err != nil {
				return Queue{}, corrupt(r.Seq, "%v", err)
			}
		}
	}
	return q, nil
}
