package journal

import (
	"encoding"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// ErrUnknownEvent reports a string that does not name an Event.
var ErrUnknownEvent = errors.New("unknown journal event")

// Event classifies one journal line: what happened at Record.Time.
//
// The event set is closed and fixed by belay's on-disk format. Read rejects
// any spelling outside it as a malformed record rather than accepting an
// unrecognized event silently — a journal reader that shrugs at unknown
// events cannot be trusted to compute a correct resume decision.
type Event int

const (
	// EventUnknown is the zero value. It never appears in a valid record;
	// Record.validate rejects it.
	EventUnknown Event = iota
	// EventRunStarted marks the first record of a fresh run.
	EventRunStarted
	// EventNodeStarted marks the beginning of one execution attempt of a
	// node. A well-formed journal closes every EventNodeStarted with exactly
	// one matching EventNodeFinished before another node begins.
	EventNodeStarted
	// EventNodeFinished marks the end of one execution attempt of a node,
	// carrying its outcome in Record.Status and its successor in
	// Record.Next.
	EventNodeFinished
	// EventRunPaused marks a run explicitly suspended. A plain `belay run`
	// on a journal ending in EventRunPaused must refuse; only `belay resume`
	// may advance past it.
	EventRunPaused
	// EventRunResumed marks a paused run continuing.
	EventRunResumed
	// EventRunCompleted marks a run that finished successfully. Nothing
	// remains to resume.
	EventRunCompleted
	// EventRunFailed marks a run that ended in failure.
	EventRunFailed
	// EventRunAborted marks a run stopped before it could finish, such as by
	// user request.
	EventRunAborted
)

var eventNames = [...]string{
	EventUnknown:      "unknown",
	EventRunStarted:   "run_started",
	EventNodeStarted:  "node_started",
	EventNodeFinished: "node_finished",
	EventRunPaused:    "run_paused",
	EventRunResumed:   "run_resumed",
	EventRunCompleted: "run_completed",
	EventRunFailed:    "run_failed",
	EventRunAborted:   "run_aborted",
}

// String returns the canonical on-disk spelling of e, such as "node_started".
func (e Event) String() string {
	if !e.valid() {
		return fmt.Sprintf("Event(%d)", int(e))
	}
	return eventNames[e]
}

// valid reports whether e is one of the declared Event constants.
func (e Event) valid() bool { return e >= 0 && int(e) < len(eventNames) }

// nodeScoped reports whether e describes one execution attempt of a node
// (as opposed to a run-level lifecycle marker).
func (e Event) nodeScoped() bool { return e == EventNodeStarted || e == EventNodeFinished }

// ParseEvent is the inverse of Event.String, accepting the canonical
// spellings such as "node_finished" or "run_paused". It returns an error
// wrapping ErrUnknownEvent for any other input.
func ParseEvent(s string) (Event, error) {
	for i, name := range eventNames {
		if name == s {
			return Event(i), nil
		}
	}
	return EventUnknown, fmt.Errorf("%w: %q", ErrUnknownEvent, s)
}

// MarshalText implements encoding.TextMarshaler so an Event round-trips
// through JSON as its canonical spelling.
func (e Event) MarshalText() ([]byte, error) {
	if !e.valid() {
		return nil, fmt.Errorf("%w: %d", ErrUnknownEvent, int(e))
	}
	return []byte(e.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (e *Event) UnmarshalText(text []byte) error {
	parsed, err := ParseEvent(string(text))
	if err != nil {
		return err
	}
	*e = parsed
	return nil
}

// ErrUnknownStatus reports a string that does not name a Status.
var ErrUnknownStatus = errors.New("unknown journal status")

// Status is the outcome of one node_finished record.
type Status int

const (
	// StatusUnknown is the zero value. It never appears on a node_finished
	// record; Record.validate rejects it there. It is the correct value for
	// every other event, which carries no status.
	StatusUnknown Status = iota
	// StatusOK means the node completed successfully.
	StatusOK
	// StatusFailed means the node ran and produced a failing result.
	StatusFailed
	// StatusPaused means the node's own logic (such as an approval gate)
	// suspended the run before its successor would begin.
	StatusPaused
	// StatusAborted means the node's execution was cut short.
	StatusAborted
)

var statusNames = [...]string{
	StatusUnknown: "unknown",
	StatusOK:      "ok",
	StatusFailed:  "failed",
	StatusPaused:  "paused",
	StatusAborted: "aborted",
}

// String returns the canonical on-disk spelling of s, such as "failed".
func (s Status) String() string {
	if !s.valid() {
		return fmt.Sprintf("Status(%d)", int(s))
	}
	return statusNames[s]
}

// valid reports whether s is one of the declared Status constants.
func (s Status) valid() bool { return s >= 0 && int(s) < len(statusNames) }

// ParseStatus is the inverse of Status.String, accepting the canonical
// spellings "ok", "failed", "paused" and "aborted" (and "unknown"). It
// returns an error wrapping ErrUnknownStatus for any other input.
func ParseStatus(s string) (Status, error) {
	for i, name := range statusNames {
		if name == s {
			return Status(i), nil
		}
	}
	return StatusUnknown, fmt.Errorf("%w: %q", ErrUnknownStatus, s)
}

// MarshalText implements encoding.TextMarshaler so a Status round-trips
// through JSON as its canonical spelling.
func (s Status) MarshalText() ([]byte, error) {
	if !s.valid() {
		return nil, fmt.Errorf("%w: %d", ErrUnknownStatus, int(s))
	}
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Status) UnmarshalText(text []byte) error {
	parsed, err := ParseStatus(string(text))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// Usage records token consumption and estimated spend for one node
// execution.
//
// Usage is deliberately local and minimal rather than imported from
// pkg/belay: this package must not depend on the agent adapter layer being
// built in parallel. A later task adapts between the two shapes.
type Usage struct {
	// InputTokens is the number of tokens sent to the model.
	InputTokens int64 `json:"input_tokens"`
	// OutputTokens is the number of tokens the model produced.
	OutputTokens int64 `json:"output_tokens"`
	// USD is the spend attributed to this node execution.
	USD float64 `json:"usd"`
	// Estimated is true when USD is a heuristic rather than a figure
	// reported by the provider.
	Estimated bool `json:"estimated"`
}

// Record is one line of a journal: a single, self-contained fact about a
// run, in the exact shape that is written to and read from disk.
//
// Field presence follows the event: Node and Attempt are populated for
// EventNodeStarted and EventNodeFinished and empty for run-level events;
// Status, DurationMS, Next and Usage are populated only on EventNodeFinished.
// Callers are not required to zero unused fields explicitly — the zero
// value of every optional field is omitted from the encoded JSON.
type Record struct {
	// Seq is the record's position in the journal, assigned by
	// Journal.Append as a strictly increasing counter starting at 1. It is
	// the basis of corruption detection: a gap, repeat or inversion in Seq
	// across a journal's records is never valid.
	Seq uint64 `json:"seq"`
	// Attempt is the 1-based execution attempt of Node. It increments only
	// when a previous attempt of the same visit to Node died without a
	// matching EventNodeFinished (see ResolveStart); a fresh visit to Node
	// later in the graph — a loop iteration, not a crash recovery — starts
	// over at attempt 1.
	Attempt int `json:"attempt,omitempty"`
	// Node is the node this record concerns. Empty for run-level events.
	Node string `json:"node,omitempty"`
	// Event classifies the record. Every record has one.
	Event Event `json:"event"`
	// Time is when the record was produced.
	Time time.Time `json:"ts"`
	// DurationMS is how long the node execution took, in milliseconds.
	// Populated only on EventNodeFinished.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Status is the outcome of the node execution. Populated only on
	// EventNodeFinished.
	Status Status `json:"status,omitempty"`
	// Next is the node that should run after this one, as decided by the
	// dispatcher's graph and gates. Populated only on EventNodeFinished, and
	// empty when nothing follows (the node is terminal). ResolveStart reads
	// it verbatim: it does not know the graph's shape and never guesses a
	// successor.
	Next string `json:"next,omitempty"`
	// Usage records token and cost accounting for the node execution.
	// Populated only on EventNodeFinished. A non-nil Usage is written even
	// when every field is zero, so its presence in the JSON line signals
	// "this node's cost was accounted for" rather than "cost was not
	// tracked" — callers that never measure cost should leave it nil.
	Usage *Usage `json:"usage,omitempty"`
	// Note is free-form operator- or agent-facing text, such as a failure
	// summary. Callers MUST pass already-redacted text: Note is persisted
	// and displayed verbatim, and redaction is internal/exec's
	// responsibility, not this package's.
	Note string `json:"note,omitempty"`
}

// validate reports whether r is well-formed enough to append: a known
// Event, node fields present exactly when Event needs them, and a status
// and non-negative duration on every node_finished record. It does not and
// cannot check Seq, which Journal.Append assigns after validate succeeds.
func (r Record) validate() error {
	if !r.Event.valid() || r.Event == EventUnknown {
		return fmt.Errorf("invalid event %v", r.Event)
	}

	if r.Event.nodeScoped() {
		if strings.TrimSpace(r.Node) == "" {
			return fmt.Errorf("%s record missing node", r.Event)
		}
		if r.Attempt < 1 {
			return fmt.Errorf("%s record has invalid attempt %d, want >= 1", r.Event, r.Attempt)
		}
	} else if r.Node != "" {
		return fmt.Errorf("%s record must not set node (got %q): only node_started and node_finished are node-scoped", r.Event, r.Node)
	}

	if r.Event == EventNodeFinished {
		if !r.Status.valid() || r.Status == StatusUnknown {
			return fmt.Errorf("node_finished record missing status")
		}
		if r.DurationMS < 0 {
			return fmt.Errorf("node_finished record has negative duration_ms %d", r.DurationMS)
		}
	} else if r.Status != StatusUnknown {
		return fmt.Errorf("%s record must not set status: only node_finished carries an outcome", r.Event)
	}

	if r.Usage != nil {
		if math.IsNaN(r.Usage.USD) || math.IsInf(r.Usage.USD, 0) {
			return fmt.Errorf("usage.usd is not finite: %v", r.Usage.USD)
		}
		if r.Usage.InputTokens < 0 || r.Usage.OutputTokens < 0 {
			return fmt.Errorf("usage token counts must be non-negative, got input=%d output=%d", r.Usage.InputTokens, r.Usage.OutputTokens)
		}
	}

	return nil
}

// Compile-time proof that Event and Status round-trip through the stdlib
// text interfaces, exactly as their JSON encoding relies on.
var (
	_ encoding.TextMarshaler   = EventRunStarted
	_ encoding.TextUnmarshaler = (*Event)(nil)
	_ fmt.Stringer             = EventRunStarted
	_ encoding.TextMarshaler   = StatusOK
	_ encoding.TextUnmarshaler = (*Status)(nil)
	_ fmt.Stringer             = StatusOK
)
