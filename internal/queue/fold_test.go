//go:build unix

package queue

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// at is a fixed instant, so a test can say exactly when each record happened.
var at = time.Date(2026, 9, 30, 22, 0, 0, 0, time.UTC)

// history numbers records 1, 2, 3, ... in order and gives each one a time a
// minute after the last, the way the store would have written them.
func history(records ...Record) []Record {
	out := make([]Record, len(records))
	for i, r := range records {
		r.Seq = uint64(i + 1)
		r.Time = at.Add(time.Duration(i) * time.Minute)
		out[i] = r
	}
	return out
}

func added(id int, goal string) Record { return Record{Event: EventAdded, ID: id, Goal: goal} }
func removed(id int) Record            { return Record{Event: EventRemoved, ID: id} }
func claimed(id int) Record {
	return Record{Event: EventClaimed, ID: id, RunID: runIDFor(id), Dir: "/ws/.belay/queue/work/" + runIDFor(id)}
}
func settled(id int, s State) Record {
	return Record{Event: EventSettled, ID: id, State: s, Note: "note " + s.String()}
}
func approved(id int) Record { return Record{Event: EventApproved, ID: id} }

// runIDFor names the run a test claims for goal id.
func runIDFor(id int) string { return fmt.Sprintf("20260930T220000Z-%012d", id) }

func TestFoldEntryLifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []Record
		want    []State // by goal, in id order
		corrupt uint64  // the seq at which the history goes wrong, or 0
	}{
		{name: "nothing yet", records: nil, want: nil},
		{name: "queued", records: history(added(1, "a"), added(2, "b")), want: []State{StateQueued, StateQueued}},
		{name: "removed while queued", records: history(added(1, "a"), removed(1)), want: []State{StateRemoved}},
		{name: "claimed", records: history(added(1, "a"), claimed(1)), want: []State{StateClaimed}},
		{
			name:    "parked at the gate",
			records: history(added(1, "a"), claimed(1), settled(1, StateWaiting)),
			want:    []State{StateWaiting},
		},
		{
			name:    "approved after waiting",
			records: history(added(1, "a"), claimed(1), settled(1, StateWaiting), approved(1)),
			want:    []State{StateApproved},
		},
		{
			name: "approved, then finished",
			records: history(added(1, "a"), claimed(1), settled(1, StateWaiting), approved(1),
				settled(1, StateFinished)),
			want: []State{StateFinished},
		},
		{
			name: "approved, then parked again",
			records: history(added(1, "a"), claimed(1), settled(1, StateWaiting), approved(1),
				settled(1, StateWaiting)),
			want: []State{StateWaiting},
		},
		{name: "failed", records: history(added(1, "a"), claimed(1), settled(1, StateFailed)), want: []State{StateFailed}},
		{name: "stopped", records: history(added(1, "a"), claimed(1), settled(1, StateStopped)), want: []State{StateStopped}},
		{name: "paused", records: history(added(1, "a"), claimed(1), settled(1, StatePaused)), want: []State{StatePaused}},
		{
			name:    "worker records change no goal",
			records: history(Record{Event: EventWorkerStarted, PID: 7}, added(1, "a"), Record{Event: EventWorkerStopped, Stop: StopDrained}),
			want:    []State{StateQueued},
		},

		// The histories no belay writes. The first is the one that matters:
		// a second claim is the only way a goal could run twice.
		{name: "a second claim", records: history(added(1, "a"), claimed(1), claimed(1)), corrupt: 3},
		{name: "a claim after a removal", records: history(added(1, "a"), removed(1), claimed(1)), corrupt: 3},
		{name: "a removal after a claim", records: history(added(1, "a"), claimed(1), removed(1)), corrupt: 3},
		{name: "a settle with no claim", records: history(added(1, "a"), settled(1, StateFinished)), corrupt: 2},
		{
			name:    "a settle after the goal ended",
			records: history(added(1, "a"), claimed(1), settled(1, StateFinished), settled(1, StateFailed)),
			corrupt: 4,
		},
		{name: "an approval of a goal not waiting", records: history(added(1, "a"), claimed(1), approved(1)), corrupt: 3},
		{name: "an id added twice", records: history(added(1, "a"), added(1, "b")), corrupt: 2},
		{name: "an id going backwards", records: history(added(2, "a"), added(1, "b")), corrupt: 2},
		{name: "a goal nobody added", records: history(claimed(7)), corrupt: 1},
		{
			name:    "a gap in the sequence",
			records: []Record{{Seq: 1, Event: EventAdded, ID: 1, Goal: "a"}, {Seq: 3, Event: EventAdded, ID: 2, Goal: "b"}},
			corrupt: 3,
		},
		{
			name:    "a record that is not valid on its own",
			records: history(added(1, "a"), Record{Event: EventClaimed, ID: 1}),
			corrupt: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q, err := fold(tt.records)
			if tt.corrupt != 0 {
				var bad *CorruptionError
				if !errors.As(err, &bad) || !errors.Is(err, ErrCorruptQueue) {
					t.Fatalf("fold() = %v, want a *CorruptionError wrapping ErrCorruptQueue", err)
				}
				if bad.Seq != tt.corrupt {
					t.Errorf("fold() blamed seq %d, want %d (%v)", bad.Seq, tt.corrupt, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("fold() = %v, want nil", err)
			}
			if len(q.Entries) != len(tt.want) {
				t.Fatalf("fold() gave %d goals, want %d: %+v", len(q.Entries), len(tt.want), q.Entries)
			}
			for i, e := range q.Entries {
				if e.ID != i+1 {
					t.Errorf("goal %d has id %d; goals must come back in the order they were added", i+1, e.ID)
				}
				if e.State != tt.want[i] {
					t.Errorf("goal %d is %s, want %s", e.ID, e.State, tt.want[i])
				}
			}
		})
	}
}

// TestFoldKeepsWhatEachGoalCarries: the claim names a goal's run and copy for
// the rest of its life, --no-approval travels with the goal from the moment it
// is added, and the last settle's note is what a person reads about it.
func TestFoldKeepsWhatEachGoalCarries(t *testing.T) {
	t.Parallel()

	noReview := added(1, "make the CLI accept --json")
	noReview.NoApproval = true
	records := history(noReview, claimed(1), settled(1, StateWaiting), approved(1), settled(1, StatePaused))

	q, err := fold(records)
	if err != nil {
		t.Fatalf("fold() = %v", err)
	}
	got, ok := q.Entry(1)
	if !ok {
		t.Fatalf("fold() lost goal 1: %+v", q.Entries)
	}
	want := Entry{
		ID: 1, Goal: "make the CLI accept --json", NoApproval: true, State: StatePaused,
		AddedAt: at, UpdatedAt: at.Add(4 * time.Minute),
		RunID: runIDFor(1), Dir: "/ws/.belay/queue/work/" + runIDFor(1), Note: "note paused",
	}
	if got != want {
		t.Errorf("goal 1 =\n  %+v\nwant\n  %+v", got, want)
	}
	if _, ok := q.Entry(2); ok {
		t.Error("Entry(2) found a goal nobody added")
	}
}

// TestFoldRemembersTheLastStart: `belay queue status` tells a person whether
// the last `belay queue start` finished, was stopped, or was cut off by a
// crash or a reboot that left it no chance to say.
func TestFoldRemembersTheLastStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []Record
		want    Start
	}{
		{name: "never started", records: nil, want: Start{}},
		{
			name: "drained",
			records: history(Record{Event: EventWorkerStarted, PID: 7},
				Record{Event: EventWorkerStopped, Stop: StopDrained}),
			want: Start{At: at, PID: 7, Ended: true, EndedAt: at.Add(time.Minute), Stop: StopDrained},
		},
		{
			name: "cut off, then started again",
			records: history(Record{Event: EventWorkerStarted, PID: 7}, added(1, "a"),
				Record{Event: EventWorkerStarted, PID: 8}),
			want: Start{At: at.Add(2 * time.Minute), PID: 8, PreviousCutOff: true},
		},
		{
			name:    "cut off, and not started since",
			records: history(Record{Event: EventWorkerStarted, PID: 7}, added(1, "a")),
			want:    Start{At: at, PID: 7},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q, err := fold(tt.records)
			if err != nil {
				t.Fatalf("fold() = %v", err)
			}
			if q.Last != tt.want {
				t.Errorf("last start = %+v, want %+v", q.Last, tt.want)
			}
		})
	}
}
