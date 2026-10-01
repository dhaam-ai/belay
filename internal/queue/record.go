//go:build unix

package queue

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ErrInvalidRecord reports a line of the queue log that is not a record on its
// own terms: an event, state or stop reason nothing declares, or an event
// without a field it cannot mean anything without. It says nothing about
// whether the record fits the history around it; that is ErrCorruptQueue's.
var ErrInvalidRecord = errors.New("queue: invalid record")

// Event classifies one line of the queue log: what happened at Record.Time.
//
// The set is closed and fixed by the log's format, as internal/journal's is
// for a run's journal, and for the same reason. A reader that shrugs at an
// event it does not know cannot be trusted to say which goal runs next, and
// for this log the wrong answer is a goal run twice, or never.
type Event int

const (
	// EventUnknown is the zero value. It never appears in a valid record.
	EventUnknown Event = iota
	// EventAdded is a person adding a goal, with `belay queue add`.
	EventAdded
	// EventRemoved is a person taking back a goal nothing had claimed, with
	// `belay queue rm`.
	EventRemoved
	// EventClaimed is a worker taking a goal, naming the run the goal will be
	// and the copy it will run in, before either exists. See "Claim before
	// create" in the package documentation.
	EventClaimed
	// EventSettled is a worker recording how a goal's run ended, or that it
	// is parked at its plan. Record.State says which.
	EventSettled
	// EventApproved is a person approving a parked goal's plan, with `belay
	// queue approve`.
	EventApproved
	// EventWorkerStarted is `belay queue start` beginning to work through the
	// queue.
	EventWorkerStarted
	// EventWorkerStopped is that worker stopping, and Record.Stop says why. A
	// start with no stop after it was cut off, by a crash, a kill or a
	// reboot, before it could say.
	EventWorkerStopped
)

var eventNames = [...]string{
	EventUnknown:       "unknown",
	EventAdded:         "entry_added",
	EventRemoved:       "entry_removed",
	EventClaimed:       "entry_claimed",
	EventSettled:       "entry_settled",
	EventApproved:      "entry_approved",
	EventWorkerStarted: "worker_started",
	EventWorkerStopped: "worker_stopped",
}

// String returns the spelling of e the log holds, such as "entry_claimed".
func (e Event) String() string { return spelled(eventNames[:], int(e), "Event") }

// MarshalText implements encoding.TextMarshaler, so an Event is written as
// its spelling.
func (e Event) MarshalText() ([]byte, error) { return marshalName(eventNames[:], int(e), "event") }

// UnmarshalText implements encoding.TextUnmarshaler. A spelling nothing
// declares is an error wrapping ErrInvalidRecord.
func (e *Event) UnmarshalText(text []byte) error {
	i, err := parseName(eventNames[:], string(text), "event")
	*e = Event(i)
	return err
}

// namesAGoal reports whether a record of this event is about one goal, and
// so must carry that goal's id.
func (e Event) namesAGoal() bool { return e >= EventAdded && e <= EventApproved }

// State is where one goal stands in the queue.
//
// A goal moves through these in one direction only, which the fold enforces
// (see Entry): queued, then claimed, then settled in one of the states a run
// can end in, except that a goal parked at its plan goes back to the worker
// when a person approves it. Removed is reachable only from queued.
type State int

const (
	// StateUnknown is the zero value: a goal nothing has happened to.
	StateUnknown State = iota
	// StateQueued is a goal waiting its turn. Nothing exists for it yet,
	// and `belay queue rm` can still take it back.
	StateQueued
	// StateRemoved is a goal a person took back before any worker claimed
	// it.
	StateRemoved
	// StateClaimed is a goal a worker has taken: its run and copy are named
	// in the log, and may or may not exist yet. A worker that finds one
	// continues it before it claims anything else.
	StateClaimed
	// StateWaiting is a goal parked at the approve gate: its run wrote a plan
	// and stopped for a person to read it. The queue moved on without it.
	StateWaiting
	// StateApproved is a parked goal a person has approved. The next worker
	// continues its run past the gate.
	StateApproved
	// StateFinished is a goal whose run completed.
	StateFinished
	// StateFailed is a goal whose run ended in failure.
	StateFailed
	// StateStopped is a goal whose run was stopped for good: it ended as
	// aborted.
	StateStopped
	// StatePaused is a goal whose run stopped at one of belay's own limits,
	// such as budget.max_usd. The run can be continued, but only by a person
	// who has decided the limit should be raised; the queue never retries
	// it.
	StatePaused
)

var stateNames = [...]string{
	StateUnknown:  "unknown",
	StateQueued:   "queued",
	StateRemoved:  "removed",
	StateClaimed:  "claimed",
	StateWaiting:  "waiting",
	StateApproved: "approved",
	StateFinished: "finished",
	StateFailed:   "failed",
	StateStopped:  "stopped",
	StatePaused:   "paused",
}

// String returns the spelling of s the log holds, such as "waiting".
func (s State) String() string { return spelled(stateNames[:], int(s), "State") }

// MarshalText implements encoding.TextMarshaler, so a State is written as its
// spelling.
func (s State) MarshalText() ([]byte, error) { return marshalName(stateNames[:], int(s), "state") }

// UnmarshalText implements encoding.TextUnmarshaler. A spelling nothing
// declares is an error wrapping ErrInvalidRecord.
func (s *State) UnmarshalText(text []byte) error {
	i, err := parseName(stateNames[:], string(text), "state")
	*s = State(i)
	return err
}

// settles reports whether a worker may settle a goal in s: parked at its
// plan, or ended the way its run ended.
func (s State) settles() bool {
	switch s {
	case StateWaiting, StateFinished, StateFailed, StateStopped, StatePaused:
		return true
	default:
		return false
	}
}

// Stop is why a worker stopped working through the queue.
type Stop int

const (
	// StopUnknown is the zero value. It never appears in a valid record.
	StopUnknown Stop = iota
	// StopDrained is a worker that ran out of goals it could run.
	StopDrained
	// StopInterrupted is a worker a person asked to stop, with Ctrl-C or
	// SIGTERM. The goal it had in hand is paused where it stood, and the
	// next worker continues it.
	StopInterrupted
	// StopFailed is a worker that could not go on, and Record.Note says why.
	// The goal it had in hand stays claimed, so the next worker picks it up
	// again rather than losing it.
	StopFailed
)

var stopNames = [...]string{
	StopUnknown:     "unknown",
	StopDrained:     "drained",
	StopInterrupted: "interrupted",
	StopFailed:      "failed",
}

// String returns the spelling of s the log holds, such as "drained".
func (s Stop) String() string { return spelled(stopNames[:], int(s), "Stop") }

// MarshalText implements encoding.TextMarshaler, so a Stop is written as its
// spelling.
func (s Stop) MarshalText() ([]byte, error) { return marshalName(stopNames[:], int(s), "stop") }

// UnmarshalText implements encoding.TextUnmarshaler. A spelling nothing
// declares is an error wrapping ErrInvalidRecord.
func (s *Stop) UnmarshalText(text []byte) error {
	i, err := parseName(stopNames[:], string(text), "stop")
	*s = Stop(i)
	return err
}

// Record is one line of the queue log: a single fact about the queue, in the
// shape it is written to and read from disk.
//
// Which fields an event carries is fixed per event (see validate); the zero
// value of every field it does not carry is left out of the line.
type Record struct {
	// Seq is the record's place in the log, counting from 1 with no gap. The
	// store assigns it under the lock every writer takes, so a gap or a
	// repeat is proof that something wrote without it.
	Seq uint64 `json:"seq"`
	// Event classifies the record.
	Event Event `json:"event"`
	// Time is when the record was written.
	Time time.Time `json:"ts"`
	// ID is the goal the record is about, for every entry_* event. Ids start
	// at 1 and only ever grow, so an id is never given to two goals.
	ID int `json:"id,omitempty"`
	// Goal is what the person asked for, exactly as they typed it, on
	// entry_added.
	Goal string `json:"goal,omitempty"`
	// NoApproval is set on entry_added when the person asked, as they added
	// the goal, for its plan not to wait for review. It is the only way a
	// queued goal skips the approve gate.
	NoApproval bool `json:"no_approval,omitempty"`
	// RunID is the run a goal will be, on entry_claimed.
	RunID string `json:"run_id,omitempty"`
	// Dir is the absolute path of the copy the goal's run works in, on
	// entry_claimed. The run's own files live inside it.
	Dir string `json:"dir,omitempty"`
	// State is what a goal settled as, on entry_settled.
	State State `json:"state,omitempty"`
	// Stop is why a worker stopped, on worker_stopped.
	Stop Stop `json:"stop,omitempty"`
	// PID is the process id of the worker, on worker_started. It is for a
	// person reading the log; nothing decides anything on it.
	PID int `json:"pid,omitempty"`
	// Note is what a person needs to know about a settle or a stop: where a
	// parked goal's plan is, which limit a paused run met, why a worker
	// could not go on.
	Note string `json:"note,omitempty"`
}

// validate reports whether r is a record on its own terms: a known event,
// carrying the fields that event needs. It does not check Seq, which the
// store assigns, or whether r fits the history before it, which is fold's.
func (r Record) validate() error {
	switch r.Event {
	case EventAdded:
		if strings.TrimSpace(r.Goal) == "" {
			return invalid("entry_added for goal %d names no goal", r.ID)
		}
	case EventClaimed:
		if r.RunID == "" {
			return invalid("entry_claimed for goal %d names no run", r.ID)
		}
		// Absolute, because the copy is found from this record alone, by
		// whichever process reads it next and from whatever directory it
		// was started in.
		if !filepath.IsAbs(r.Dir) {
			return invalid("entry_claimed for goal %d names the copy %q, which is not an absolute path", r.ID, r.Dir)
		}
	case EventSettled:
		if !r.State.settles() {
			return invalid("entry_settled for goal %d names %s, which no goal settles as", r.ID, r.State)
		}
	case EventWorkerStopped:
		if r.Stop == StopUnknown {
			return invalid("worker_stopped gives no reason")
		}
	case EventRemoved, EventApproved, EventWorkerStarted:
	default:
		return invalid("event %s is not one the queue log holds", r.Event)
	}
	if r.Event.namesAGoal() && r.ID <= 0 {
		return invalid("%s names goal %d; goal ids start at 1", r.Event, r.ID)
	}
	return nil
}

// invalid builds an error wrapping ErrInvalidRecord.
func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRecord, fmt.Sprintf(format, a...))
}

// spelled returns names[i], or a placeholder naming kind for a value no name
// declares, rather than panicking on it.
func spelled(names []string, i int, kind string) string {
	if i < 0 || i >= len(names) {
		return fmt.Sprintf("%s(%d)", kind, i)
	}
	return names[i]
}

// marshalName is MarshalText for one of this package's named values.
func marshalName(names []string, i int, kind string) ([]byte, error) {
	if i < 0 || i >= len(names) {
		return nil, invalid("%s %d has no spelling", kind, i)
	}
	return []byte(names[i]), nil
}

// parseName is UnmarshalText for one of this package's named values.
func parseName(names []string, s, kind string) (int, error) {
	for i, name := range names {
		if name == s {
			return i, nil
		}
	}
	return 0, invalid("%s %q is not one the queue log holds", kind, s)
}
