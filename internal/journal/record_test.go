package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// cmpTimeOpt lets cmp.Diff compare time.Time by instant (via Equal) instead
// of panicking on its unexported fields.
var cmpTimeOpt = cmp.Comparer(func(a, b time.Time) bool { return a.Equal(b) })

var allEvents = []Event{
	EventUnknown, EventRunStarted, EventNodeStarted, EventNodeFinished,
	EventRunPaused, EventRunResumed, EventRunCompleted, EventRunFailed, EventRunAborted,
}

func TestEventRoundTrip(t *testing.T) {
	t.Parallel()

	for _, want := range allEvents {
		t.Run(want.String(), func(t *testing.T) {
			t.Parallel()

			got, err := ParseEvent(want.String())
			if err != nil {
				t.Fatalf("ParseEvent(%q) returned error: %v", want.String(), err)
			}
			if got != want {
				t.Errorf("ParseEvent(%q) = %v, want %v", want.String(), got, want)
			}
		})
	}
}

func TestEventStringIsCanonicalWireSpelling(t *testing.T) {
	t.Parallel()

	// These are the exact spellings the on-disk format uses; changing one is
	// a breaking change to every journal belay has ever written.
	want := map[Event]string{
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
	for event, spelling := range want {
		if got := event.String(); got != spelling {
			t.Errorf("Event(%d).String() = %q, want %q", int(event), got, spelling)
		}
	}
}

func TestParseEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    Event
		wantErr bool
	}{
		{name: "exact", input: "node_finished", want: EventNodeFinished},
		{name: "exact run_started", input: "run_started", want: EventRunStarted},
		{name: "uppercase is not accepted", input: "NODE_STARTED", wantErr: true},
		{name: "surrounding whitespace is not accepted", input: " node_started ", wantErr: true},
		{name: "empty", input: "", wantErr: true},
		{name: "unknown word", input: "node_retried", wantErr: true},
		{name: "near miss", input: "nodestarted", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseEvent(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseEvent(%q) = %v, want error", tt.input, got)
				}
				if !errors.Is(err, ErrUnknownEvent) {
					t.Errorf("ParseEvent(%q) error = %v, want it to wrap ErrUnknownEvent", tt.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseEvent(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseEvent(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestEventOutOfRangeDoesNotPanic(t *testing.T) {
	t.Parallel()

	for _, event := range []Event{-1, 99} {
		if got := event.String(); got == "" {
			t.Errorf("Event(%d).String() = empty, want a placeholder", int(event))
		}
		if _, err := event.MarshalText(); err == nil {
			t.Errorf("Event(%d).MarshalText() = nil error, want error", int(event))
		}
	}
}

func TestEventJSONRoundTrip(t *testing.T) {
	t.Parallel()

	for _, want := range allEvents {
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("json.Marshal(%v) returned error: %v", want, err)
		}

		var got Event
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
		}
		if got != want {
			t.Errorf("round trip of %v through %s = %v", want, encoded, got)
		}
	}
}

func TestEventUnmarshalTextRejectsGarbage(t *testing.T) {
	t.Parallel()

	var e Event
	if err := e.UnmarshalText([]byte("node_retried")); err == nil {
		t.Fatal(`UnmarshalText("node_retried") = nil error, want error`)
	} else if !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("UnmarshalText error = %v, want it to wrap ErrUnknownEvent", err)
	}
}

var allStatuses = []Status{StatusUnknown, StatusOK, StatusFailed, StatusPaused, StatusAborted}

func TestStatusRoundTrip(t *testing.T) {
	t.Parallel()

	for _, want := range allStatuses {
		t.Run(want.String(), func(t *testing.T) {
			t.Parallel()

			got, err := ParseStatus(want.String())
			if err != nil {
				t.Fatalf("ParseStatus(%q) returned error: %v", want.String(), err)
			}
			if got != want {
				t.Errorf("ParseStatus(%q) = %v, want %v", want.String(), got, want)
			}
		})
	}
}

func TestStatusStringIsCanonicalWireSpelling(t *testing.T) {
	t.Parallel()

	want := map[Status]string{
		StatusUnknown: "unknown",
		StatusOK:      "ok",
		StatusFailed:  "failed",
		StatusPaused:  "paused",
		StatusAborted: "aborted",
	}
	for status, spelling := range want {
		if got := status.String(); got != spelling {
			t.Errorf("Status(%d).String() = %q, want %q", int(status), got, spelling)
		}
	}
}

func TestParseStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    Status
		wantErr bool
	}{
		{name: "ok", input: "ok", want: StatusOK},
		{name: "failed", input: "failed", want: StatusFailed},
		{name: "paused", input: "paused", want: StatusPaused},
		{name: "aborted", input: "aborted", want: StatusAborted},
		{name: "uppercase is not accepted", input: "OK", wantErr: true},
		{name: "empty", input: "", wantErr: true},
		{name: "unknown word", input: "skipped", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseStatus(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseStatus(%q) = %v, want error", tt.input, got)
				}
				if !errors.Is(err, ErrUnknownStatus) {
					t.Errorf("ParseStatus(%q) error = %v, want it to wrap ErrUnknownStatus", tt.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseStatus(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseStatus(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestStatusOutOfRangeDoesNotPanic(t *testing.T) {
	t.Parallel()

	for _, status := range []Status{-1, 99} {
		if got := status.String(); got == "" {
			t.Errorf("Status(%d).String() = empty, want a placeholder", int(status))
		}
		if _, err := status.MarshalText(); err == nil {
			t.Errorf("Status(%d).MarshalText() = nil error, want error", int(status))
		}
	}
}

// TestRecordJSONFieldPresence pins down exactly which JSON keys appear for
// each event shape. This is the contract downstream tools (jq pipelines,
// `belay timeline`, another agent's task) will be written against.
func TestRecordJSONFieldPresence(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 8, 29, 10, 31, 4, 221_000_000, time.UTC)

	tests := []struct {
		name    string
		rec     Record
		want    []string
		absent  []string
	}{
		{
			name: "node_started",
			rec: Record{
				Seq: 7, Attempt: 1, Node: "test", Event: EventNodeStarted, Time: ts,
			},
			want:   []string{"seq", "attempt", "node", "event", "ts"},
			absent: []string{"duration_ms", "status", "next", "usage", "note"},
		},
		{
			name: "node_finished with usage",
			rec: Record{
				Seq: 8, Attempt: 1, Node: "test", Event: EventNodeFinished, Time: ts,
				DurationMS: 8421, Status: StatusOK, Next: "fix",
				Usage: &Usage{},
			},
			want:   []string{"seq", "attempt", "node", "event", "ts", "duration_ms", "status", "next", "usage"},
			absent: []string{"note"},
		},
		{
			name: "run-level event carries no node or attempt",
			rec: Record{
				Seq: 1, Event: EventRunStarted, Time: ts,
			},
			want:   []string{"seq", "event", "ts"},
			absent: []string{"attempt", "node", "duration_ms", "status", "next", "usage", "note"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.rec)
			if err != nil {
				t.Fatalf("json.Marshal returned error: %v", err)
			}

			var obj map[string]json.RawMessage
			if err := json.Unmarshal(data, &obj); err != nil {
				t.Fatalf("re-parsing marshaled record: %v", err)
			}

			for _, key := range tt.want {
				if _, ok := obj[key]; !ok {
					t.Errorf("marshaled record %s missing key %q, got keys %v", data, key, keysOf(obj))
				}
			}
			for _, key := range tt.absent {
				if _, ok := obj[key]; ok {
					t.Errorf("marshaled record %s unexpectedly has key %q", data, key)
				}
			}
		})
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestRecordRoundTrip proves Record survives Marshal -> Unmarshal unchanged
// for every event shape, independent of the exact key set.
func TestRecordRoundTrip(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 8, 29, 10, 31, 4, 221_000_000, time.UTC)

	tests := []Record{
		{Seq: 1, Event: EventRunStarted, Time: ts},
		{Seq: 2, Attempt: 1, Node: "plan", Event: EventNodeStarted, Time: ts},
		{
			Seq: 3, Attempt: 1, Node: "plan", Event: EventNodeFinished, Time: ts,
			DurationMS: 812, Status: StatusOK, Next: "approve",
			Usage: &Usage{InputTokens: 100, OutputTokens: 50, USD: 0.0123, Estimated: true},
			Note:  "looks good",
		},
		{Seq: 4, Event: EventRunPaused, Time: ts},
		{
			Seq: 5, Attempt: 3, Node: "test", Event: EventNodeFinished, Time: ts,
			DurationMS: 0, Status: StatusFailed, Next: "fix", Usage: &Usage{},
		},
	}

	for _, want := range tests {
		t.Run(want.Event.String(), func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(want)
			if err != nil {
				t.Fatalf("json.Marshal returned error: %v", err)
			}

			var got Record
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal(%s) returned error: %v", data, err)
			}

			if diff := cmp.Diff(want, got, cmpTimeOpt); diff != "" {
				t.Errorf("round trip through %s changed the record (-want +got):\n%s", data, diff)
			}
		})
	}
}

// TestSpecExampleParses parses the exact two-line example from the T4 task
// description (saved verbatim in testdata/spec_example.ndjson) and checks
// the fields that matter for correctness. It is a fidelity check against the
// documented wire format, not a claim that Journal.Append produces these
// exact bytes — in particular Append assigns Seq per line (see Journal
// doc), so a real journal never repeats a Seq across a node_started/
// node_finished pair the way this illustrative example does.
func TestSpecExampleParses(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/spec_example.ndjson")
	if err != nil {
		t.Fatalf("reading testdata/spec_example.ndjson: %v", err)
	}

	result, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if result.Incomplete {
		t.Fatalf("Read reported Incomplete for a file ending in a newline")
	}
	if len(result.Records) != 2 {
		t.Fatalf("got %d records, want 2", len(result.Records))
	}

	started, finished := result.Records[0], result.Records[1]

	if started.Event != EventNodeStarted || started.Node != "test" || started.Attempt != 1 {
		t.Errorf("started = %+v, want node_started/test/attempt=1", started)
	}
	if started.Seq != 7 {
		t.Errorf("started.Seq = %d, want 7", started.Seq)
	}
	if started.Usage != nil {
		t.Errorf("started.Usage = %+v, want nil (node_started carries no usage)", started.Usage)
	}

	if finished.Event != EventNodeFinished || finished.Node != "test" || finished.Attempt != 1 {
		t.Errorf("finished = %+v, want node_finished/test/attempt=1", finished)
	}
	if finished.DurationMS != 8421 {
		t.Errorf("finished.DurationMS = %d, want 8421", finished.DurationMS)
	}
	if finished.Status != StatusOK {
		t.Errorf("finished.Status = %v, want StatusOK", finished.Status)
	}
	if finished.Next != "fix" {
		t.Errorf("finished.Next = %q, want %q", finished.Next, "fix")
	}
	if finished.Usage == nil {
		t.Fatal("finished.Usage = nil, want a present (if zero-valued) Usage")
	}
	if *finished.Usage != (Usage{}) {
		t.Errorf("finished.Usage = %+v, want the zero Usage", *finished.Usage)
	}
}

func TestRecordValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rec     Record
		wantErr bool
	}{
		{
			name: "valid node_started",
			rec:  Record{Node: "plan", Attempt: 1, Event: EventNodeStarted},
		},
		{
			name: "valid node_finished",
			rec:  Record{Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK},
		},
		{
			name: "valid run-level event",
			rec:  Record{Event: EventRunStarted},
		},
		{
			name:    "zero value event is rejected",
			rec:     Record{},
			wantErr: true,
		},
		{
			name:    "out of range event is rejected",
			rec:     Record{Event: Event(99)},
			wantErr: true,
		},
		{
			name:    "node_started missing node",
			rec:     Record{Attempt: 1, Event: EventNodeStarted},
			wantErr: true,
		},
		{
			name:    "node_started blank node",
			rec:     Record{Node: "   ", Attempt: 1, Event: EventNodeStarted},
			wantErr: true,
		},
		{
			name:    "node_started attempt zero",
			rec:     Record{Node: "plan", Attempt: 0, Event: EventNodeStarted},
			wantErr: true,
		},
		{
			name:    "node_started negative attempt",
			rec:     Record{Node: "plan", Attempt: -1, Event: EventNodeStarted},
			wantErr: true,
		},
		{
			name:    "run-level event must not carry a node",
			rec:     Record{Node: "plan", Event: EventRunStarted},
			wantErr: true,
		},
		{
			name:    "node_finished missing status",
			rec:     Record{Node: "plan", Attempt: 1, Event: EventNodeFinished},
			wantErr: true,
		},
		{
			name:    "node_finished negative duration",
			rec:     Record{Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, DurationMS: -1},
			wantErr: true,
		},
		{
			name:    "non-finished event must not carry a status",
			rec:     Record{Node: "plan", Attempt: 1, Event: EventNodeStarted, Status: StatusOK},
			wantErr: true,
		},
		{
			name:    "usage with NaN usd",
			rec:     Record{Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Usage: &Usage{USD: math.NaN()}},
			wantErr: true,
		},
		{
			name:    "usage with infinite usd",
			rec:     Record{Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Usage: &Usage{USD: math.Inf(1)}},
			wantErr: true,
		},
		{
			name:    "usage with negative input tokens",
			rec:     Record{Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Usage: &Usage{InputTokens: -1}},
			wantErr: true,
		},
		{
			name:    "usage with negative output tokens",
			rec:     Record{Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Usage: &Usage{OutputTokens: -1}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.rec.validate()
			if tt.wantErr && err == nil {
				t.Fatalf("validate() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
		})
	}
}
