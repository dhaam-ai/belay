package journal

import (
	"errors"
	"testing"
)

// TestResolveStartTable covers the four rows of the resume-decision table
// from the T4 task description, plus the three run-lifecycle terminal
// states (completed/failed/aborted) that Result.State must also represent.
func TestResolveStartTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []Record
		want    Result
	}{
		{
			name: "node_finished starts at its next",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 2, Node: "plan", Attempt: 1, Event: EventNodeStarted},
				{Seq: 3, Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "approve"},
			},
			want: Result{State: RunStateRunnable, Node: "approve", Attempt: 1},
		},
		{
			name: "orphan node_started dies mid-node and is retried at attempt+1",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 2, Node: "plan", Attempt: 1, Event: EventNodeStarted},
			},
			want: Result{State: RunStateRunnable, Node: "plan", Attempt: 2},
		},
		{
			name: "orphan node_started deeper in the run",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 2, Node: "plan", Attempt: 1, Event: EventNodeStarted},
				{Seq: 3, Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "approve"},
				{Seq: 4, Node: "approve", Attempt: 1, Event: EventNodeStarted},
			},
			want: Result{State: RunStateRunnable, Node: "approve", Attempt: 2},
		},
		{
			name: "orphan attempt increments across repeated crash recoveries",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 2, Node: "plan", Attempt: 1, Event: EventNodeStarted},
				{Seq: 3, Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusAborted},
				{Seq: 4, Node: "plan", Attempt: 2, Event: EventNodeStarted},
				{Seq: 5, Node: "plan", Attempt: 2, Event: EventNodeFinished, Status: StatusAborted},
				{Seq: 6, Node: "plan", Attempt: 3, Event: EventNodeStarted},
			},
			want: Result{State: RunStateRunnable, Node: "plan", Attempt: 4},
		},
		{
			name: "run_paused blocks a plain run but still resolves a position",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 2, Node: "plan", Attempt: 1, Event: EventNodeStarted},
				{Seq: 3, Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "approve"},
				{Seq: 4, Event: EventRunPaused},
			},
			want: Result{State: RunStatePaused, Node: "approve", Attempt: 1},
		},
		{
			name:    "nil records start at the first node",
			records: nil,
			want:    Result{State: RunStateRunnable, Node: FirstNode, Attempt: 1},
		},
		{
			name:    "empty (non-nil) records start at the first node",
			records: []Record{},
			want:    Result{State: RunStateRunnable, Node: FirstNode, Attempt: 1},
		},
		{
			name: "only run_started, nothing resumable yet",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
			},
			want: Result{State: RunStateRunnable, Node: FirstNode, Attempt: 1},
		},
		{
			name: "run_completed reports Completed but still shows the last position",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 2, Node: "review", Attempt: 1, Event: EventNodeStarted},
				{Seq: 3, Node: "review", Attempt: 1, Event: EventNodeFinished, Status: StatusOK},
				{Seq: 4, Event: EventRunCompleted},
			},
			want: Result{State: RunStateCompleted, Node: "", Attempt: 1},
		},
		{
			name: "run_failed reports Failed",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 2, Node: "test", Attempt: 1, Event: EventNodeStarted},
				{Seq: 3, Node: "test", Attempt: 1, Event: EventNodeFinished, Status: StatusFailed, Next: "fix"},
				{Seq: 4, Event: EventRunFailed},
			},
			want: Result{State: RunStateFailed, Node: "fix", Attempt: 1},
		},
		{
			name: "run_aborted reports Aborted",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 2, Event: EventRunAborted},
			},
			want: Result{State: RunStateAborted, Node: FirstNode, Attempt: 1},
		},
		{
			name: "run_resumed falls through to a normal runnable position",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 2, Node: "plan", Attempt: 1, Event: EventNodeStarted},
				{Seq: 3, Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "approve"},
				{Seq: 4, Event: EventRunPaused},
				{Seq: 5, Event: EventRunResumed},
			},
			want: Result{State: RunStateRunnable, Node: "approve", Attempt: 1},
		},
		{
			name: "a terminal node_finished with no Next resolves to an empty Node",
			records: []Record{
				{Seq: 1, Node: "review", Attempt: 1, Event: EventNodeStarted},
				{Seq: 2, Node: "review", Attempt: 1, Event: EventNodeFinished, Status: StatusOK},
			},
			want: Result{State: RunStateRunnable, Node: "", Attempt: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ResolveStart(tt.records)
			if err != nil {
				t.Fatalf("ResolveStart returned error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ResolveStart() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestResolveStartCorruption covers ResolveStart's adversarial cases: it
// must report every one of these as a *CorruptionError wrapping
// ErrCorruptJournal, and it must never guess a plausible-looking Result for
// them.
func TestResolveStartCorruption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []Record
	}{
		{
			name: "node_started for A followed by node_started for B",
			records: []Record{
				{Seq: 1, Node: "plan", Attempt: 1, Event: EventNodeStarted},
				{Seq: 2, Node: "approve", Attempt: 1, Event: EventNodeStarted},
			},
		},
		{
			name: "interleaved attempts of the same node",
			records: []Record{
				{Seq: 1, Node: "test", Attempt: 1, Event: EventNodeStarted},
				{Seq: 2, Node: "test", Attempt: 2, Event: EventNodeStarted},
			},
		},
		{
			name: "duplicate seq",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 1, Event: EventRunCompleted},
			},
		},
		{
			name: "out-of-order seq skips ahead",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 3, Event: EventRunCompleted},
			},
		},
		{
			name: "out-of-order seq goes backwards",
			records: []Record{
				{Seq: 2, Event: EventRunStarted},
				{Seq: 1, Event: EventRunCompleted},
			},
		},
		{
			name: "seq does not start at 1",
			records: []Record{
				{Seq: 2, Event: EventRunStarted},
			},
		},
		{
			name: "node_finished with no matching node_started",
			records: []Record{
				{Seq: 1, Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "approve"},
			},
		},
		{
			name: "node_finished for the wrong node",
			records: []Record{
				{Seq: 1, Node: "plan", Attempt: 1, Event: EventNodeStarted},
				{Seq: 2, Node: "approve", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "code"},
			},
		},
		{
			name: "node_finished for the wrong attempt",
			records: []Record{
				{Seq: 1, Node: "plan", Attempt: 1, Event: EventNodeStarted},
				{Seq: 2, Node: "plan", Attempt: 2, Event: EventNodeFinished, Status: StatusOK, Next: "approve"},
			},
		},
		{
			name: "a run-level event appears while a node is still open",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 2, Node: "plan", Attempt: 1, Event: EventNodeStarted},
				{Seq: 3, Event: EventRunPaused},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ResolveStart(tt.records)
			if err == nil {
				t.Fatalf("ResolveStart(%+v) = %+v, nil, want an error", tt.records, got)
			}
			if !errors.Is(err, ErrCorruptJournal) {
				t.Errorf("ResolveStart error = %v, want it to wrap ErrCorruptJournal", err)
			}
			if got != (Result{}) {
				t.Errorf("ResolveStart returned non-zero Result %+v alongside an error", got)
			}
			var corrupt *CorruptionError
			if !errors.As(err, &corrupt) {
				t.Errorf("ResolveStart error = %v (%T), want *CorruptionError", err, err)
			}
		})
	}
}

func TestValidateAcceptsAWellFormedRun(t *testing.T) {
	t.Parallel()

	if err := Validate(buildSampleRun(t)); err != nil {
		t.Errorf("Validate(buildSampleRun()) = %v, want nil", err)
	}
	if err := Validate(nil); err != nil {
		t.Errorf("Validate(nil) = %v, want nil", err)
	}
}

func TestValidateRejectsCorruptionAndNamesTheOffendingSeq(t *testing.T) {
	t.Parallel()

	records := []Record{
		{Seq: 1, Event: EventRunStarted},
		{Seq: 2, Node: "plan", Attempt: 1, Event: EventNodeStarted},
		{Seq: 5, Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "approve"},
	}
	err := Validate(records)
	if err == nil {
		t.Fatal("Validate on a journal with a seq gap = nil, want error")
	}
	var corrupt *CorruptionError
	if !errors.As(err, &corrupt) {
		t.Fatalf("Validate error = %v (%T), want *CorruptionError", err, err)
	}
	if corrupt.Seq != 5 {
		t.Errorf("CorruptionError.Seq = %d, want 5 (the offending record)", corrupt.Seq)
	}
}

func TestRunStateString(t *testing.T) {
	t.Parallel()

	want := map[RunState]string{
		RunStateRunnable:  "runnable",
		RunStatePaused:    "paused",
		RunStateCompleted: "completed",
		RunStateFailed:    "failed",
		RunStateAborted:   "aborted",
	}
	for state, spelling := range want {
		if got := state.String(); got != spelling {
			t.Errorf("RunState(%d).String() = %q, want %q", int(state), got, spelling)
		}
	}
	for _, state := range []RunState{-1, 99} {
		if got := state.String(); got == "" {
			t.Errorf("RunState(%d).String() = empty, want a placeholder", int(state))
		}
	}
}
