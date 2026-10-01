//go:build unix

package queue

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRecordValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rec     Record
		wantErr bool
	}{
		{name: "added", rec: Record{Event: EventAdded, ID: 1, Goal: "add tests"}},
		{name: "added with no goal", rec: Record{Event: EventAdded, ID: 1, Goal: " \t"}, wantErr: true},
		{name: "added with no id", rec: Record{Event: EventAdded, Goal: "add tests"}, wantErr: true},
		{name: "added with a negative id", rec: Record{Event: EventAdded, ID: -1, Goal: "add tests"}, wantErr: true},
		{name: "removed", rec: Record{Event: EventRemoved, ID: 2}},
		{name: "claimed", rec: Record{Event: EventClaimed, ID: 1, RunID: "20260930T000000Z-abcdef123456", Dir: "/ws/.belay/queue/work/x"}},
		{name: "claimed with no run id", rec: Record{Event: EventClaimed, ID: 1, Dir: "/ws/copy"}, wantErr: true},
		{name: "claimed with no copy", rec: Record{Event: EventClaimed, ID: 1, RunID: "r"}, wantErr: true},
		{name: "claimed with a relative copy", rec: Record{Event: EventClaimed, ID: 1, RunID: "r", Dir: "copy"}, wantErr: true},
		{name: "settled waiting", rec: Record{Event: EventSettled, ID: 1, State: StateWaiting}},
		{name: "settled finished", rec: Record{Event: EventSettled, ID: 1, State: StateFinished}},
		{name: "settled failed", rec: Record{Event: EventSettled, ID: 1, State: StateFailed}},
		{name: "settled stopped", rec: Record{Event: EventSettled, ID: 1, State: StateStopped}},
		{name: "settled paused", rec: Record{Event: EventSettled, ID: 1, State: StatePaused}},
		{name: "settled as queued", rec: Record{Event: EventSettled, ID: 1, State: StateQueued}, wantErr: true},
		{name: "settled as approved", rec: Record{Event: EventSettled, ID: 1, State: StateApproved}, wantErr: true},
		{name: "settled with no state", rec: Record{Event: EventSettled, ID: 1}, wantErr: true},
		{name: "approved", rec: Record{Event: EventApproved, ID: 3}},
		{name: "approved with no id", rec: Record{Event: EventApproved}, wantErr: true},
		{name: "worker started", rec: Record{Event: EventWorkerStarted, PID: 4242}},
		{name: "worker stopped", rec: Record{Event: EventWorkerStopped, Stop: StopDrained}},
		{name: "worker stopped with no reason", rec: Record{Event: EventWorkerStopped}, wantErr: true},
		{name: "no event", rec: Record{ID: 1, Goal: "add tests"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.rec.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() = %v, want error %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidRecord) {
				t.Errorf("validate() = %v, want it to wrap ErrInvalidRecord", err)
			}
		})
	}
}

// TestNamesAreWhatTheLogHolds pins the spellings on disk. The log outlives
// the belay that wrote it, so a renamed constant must not quietly rename the
// line a later belay reads, and a spelling nothing declares must be refused
// rather than read as the zero value.
func TestNamesAreWhatTheLogHolds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rec  Record
		want string
	}{
		{name: "entry_added", rec: Record{Event: EventAdded}, want: `"event":"entry_added"`},
		{name: "entry_removed", rec: Record{Event: EventRemoved}, want: `"event":"entry_removed"`},
		{name: "entry_claimed", rec: Record{Event: EventClaimed}, want: `"event":"entry_claimed"`},
		{name: "entry_settled", rec: Record{Event: EventSettled}, want: `"event":"entry_settled"`},
		{name: "entry_approved", rec: Record{Event: EventApproved}, want: `"event":"entry_approved"`},
		{name: "worker_started", rec: Record{Event: EventWorkerStarted}, want: `"event":"worker_started"`},
		{name: "worker_stopped", rec: Record{Event: EventWorkerStopped}, want: `"event":"worker_stopped"`},
		{name: "waiting", rec: Record{Event: EventSettled, State: StateWaiting}, want: `"state":"waiting"`},
		{name: "finished", rec: Record{Event: EventSettled, State: StateFinished}, want: `"state":"finished"`},
		{name: "failed", rec: Record{Event: EventSettled, State: StateFailed}, want: `"state":"failed"`},
		{name: "stopped", rec: Record{Event: EventSettled, State: StateStopped}, want: `"state":"stopped"`},
		{name: "paused", rec: Record{Event: EventSettled, State: StatePaused}, want: `"state":"paused"`},
		{name: "drained", rec: Record{Event: EventWorkerStopped, Stop: StopDrained}, want: `"stop":"drained"`},
		{name: "interrupted", rec: Record{Event: EventWorkerStopped, Stop: StopInterrupted}, want: `"stop":"interrupted"`},
		{name: "broke", rec: Record{Event: EventWorkerStopped, Stop: StopFailed}, want: `"stop":"failed"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(tt.rec)
			if err != nil {
				t.Fatalf("json.Marshal(%+v): %v", tt.rec, err)
			}
			if !strings.Contains(string(data), tt.want) {
				t.Errorf("json.Marshal(%+v) = %s, want it to contain %s", tt.rec, data, tt.want)
			}
			var back Record
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatalf("json.Unmarshal(%s): %v", data, err)
			}
			if back.Event != tt.rec.Event || back.State != tt.rec.State || back.Stop != tt.rec.Stop {
				t.Errorf("round trip of %s = %+v, want %+v", data, back, tt.rec)
			}
		})
	}

	for _, line := range []string{
		`{"seq":1,"event":"entry_exploded","ts":"2026-09-30T00:00:00Z"}`,
		`{"seq":1,"event":"entry_settled","id":1,"state":"vanished","ts":"2026-09-30T00:00:00Z"}`,
		`{"seq":1,"event":"worker_stopped","stop":"bored","ts":"2026-09-30T00:00:00Z"}`,
	} {
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("json.Unmarshal(%s) = %v, want an error wrapping ErrInvalidRecord", line, err)
		}
	}
}
