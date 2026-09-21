package journal

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// history builds a journal from compact steps, assigning Seq from 1 the way
// Journal.Append does, so a table row reads as the run it describes.
type history struct{ records []Record }

func (h *history) add(r Record) *history {
	// #nosec G115 -- len is this fixture's own small, non-negative count.
	r.Seq = uint64(len(h.records) + 1)
	h.records = append(h.records, r)
	return h
}

func (h *history) run(e Event) *history { return h.add(Record{Event: e}) }

func (h *history) started(node string, attempt int) *history {
	return h.add(Record{Event: EventNodeStarted, Node: node, Attempt: attempt})
}

func (h *history) finished(node string, attempt int, status Status, next string) *history {
	return h.add(Record{Event: EventNodeFinished, Node: node, Attempt: attempt, Status: status, Next: next})
}

// ran is one execution that started and finished.
func (h *history) ran(node string, attempt int, status Status, next string) *history {
	return h.started(node, attempt).finished(node, attempt, status, next)
}

func TestVisits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []Record
		want    []Visit
	}{
		{name: "no records", records: nil, want: nil},
		{
			name: "a linear run visits each node once",
			records: new(history).run(EventRunStarted).
				ran("plan", 1, StatusOK, "code"). // 2,3
				ran("code", 1, StatusOK, "END").  // 4,5
				run(EventRunCompleted).records,
			want: []Visit{
				{Node: "plan", Ordinal: 1, StartSeq: 2, EndSeq: 3, Executions: 1, Status: StatusOK},
				{Node: "code", Ordinal: 1, StartSeq: 4, EndSeq: 5, Executions: 1, Status: StatusOK},
			},
		},
		{
			name: "the fix loop visits test twice and fix once",
			records: new(history).run(EventRunStarted).
				ran("test", 1, StatusOK, "fix"). // 2,3
				ran("fix", 1, StatusOK, "test"). // 4,5
				ran("test", 1, StatusOK, "END"). // 6,7
				records,
			want: []Visit{
				{Node: "test", Ordinal: 1, StartSeq: 2, EndSeq: 3, Executions: 1, Status: StatusOK},
				{Node: "fix", Ordinal: 1, StartSeq: 4, EndSeq: 5, Executions: 1, Status: StatusOK},
				{Node: "test", Ordinal: 2, StartSeq: 6, EndSeq: 7, Executions: 1, Status: StatusOK},
			},
		},
		{
			// heal closes the attempt a crash left open as interrupted; the
			// re-run at attempt 2 is the same visit, so a fork's --visit and
			// the timeline's number do not move because belay was killed.
			name: "an interrupted attempt and its re-run are one visit",
			records: new(history).run(EventRunStarted).
				ran("fix", 1, StatusAborted, "fix"). // 2,3
				ran("fix", 2, StatusOK, "test").     // 4,5
				records,
			want: []Visit{
				{Node: "fix", Ordinal: 1, StartSeq: 2, EndSeq: 5, Executions: 2, Status: StatusOK},
			},
		},
		{
			// The gate pausing for a person and running again when they
			// resume is one visit, at attempt 1 both times (commit 8f46bac).
			name: "a gate's pause and its resume are one visit",
			records: new(history).run(EventRunStarted).
				ran("approve", 1, StatusPaused, "approve"). // 2,3
				run(EventRunPaused).                        // 4
				run(EventRunResumed).                       // 5
				ran("approve", 1, StatusOK, "code").        // 6,7
				records,
			want: []Visit{
				{Node: "approve", Ordinal: 1, StartSeq: 2, EndSeq: 7, Executions: 2, Status: StatusOK},
			},
		},
		{
			// A node that routes to itself is the graph deciding to go round
			// again: a StatusOK verdict, and a loop.
			name: "a node routing to itself is a second visit",
			records: new(history).run(EventRunStarted).
				ran("review", 1, StatusOK, "review"). // 2,3
				ran("review", 1, StatusOK, "END").    // 4,5
				records,
			want: []Visit{
				{Node: "review", Ordinal: 1, StartSeq: 2, EndSeq: 3, Executions: 1, Status: StatusOK},
				{Node: "review", Ordinal: 2, StartSeq: 4, EndSeq: 5, Executions: 1, Status: StatusOK},
			},
		},
		{
			// A limit or a Ctrl-C between nodes pauses the run before the
			// next node starts; the node that starts on resume begins its
			// first visit, since nothing of it had run.
			name: "a pause between nodes does not join the visits either side of it",
			records: new(history).run(EventRunStarted).
				ran("test", 1, StatusOK, "fix"). // 2,3
				run(EventRunPaused).             // 4
				run(EventRunResumed).            // 5
				ran("fix", 1, StatusOK, "END").  // 6,7
				records,
			want: []Visit{
				{Node: "test", Ordinal: 1, StartSeq: 2, EndSeq: 3, Executions: 1, Status: StatusOK},
				{Node: "fix", Ordinal: 1, StartSeq: 6, EndSeq: 7, Executions: 1, Status: StatusOK},
			},
		},
		{
			// An abort that ends the run names no Next, so it is not the
			// shape of an attempt to be run again.
			name: "an abort that ends the run closes its visit",
			records: new(history).run(EventRunStarted).
				ran("code", 1, StatusAborted, ""). // 2,3
				run(EventRunAborted).records,
			want: []Visit{
				{Node: "code", Ordinal: 1, StartSeq: 2, EndSeq: 3, Executions: 1, Status: StatusAborted},
			},
		},
		{
			name: "a visit whose last execution never finished is still open",
			records: new(history).run(EventRunStarted).
				ran("plan", 1, StatusOK, "code"). // 2,3
				started("code", 1).               // 4
				records,
			want: []Visit{
				{Node: "plan", Ordinal: 1, StartSeq: 2, EndSeq: 3, Executions: 1, Status: StatusOK},
				{Node: "code", Ordinal: 1, StartSeq: 4, Executions: 1, Status: StatusUnknown},
			},
		},
		{
			name: "a re-run left open reopens the visit it continues",
			records: new(history).run(EventRunStarted).
				ran("code", 1, StatusAborted, "code"). // 2,3
				started("code", 2).                    // 4
				records,
			want: []Visit{
				{Node: "code", Ordinal: 1, StartSeq: 2, Executions: 2, Status: StatusUnknown},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Visits(tt.records)
			if err != nil {
				t.Fatalf("Visits() error = %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Visits() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestVisitsRefusesABrokenHistory: a history whose executions do not pair up
// has no single right grouping, and the grouping is what a fork's --visit and
// a snapshot's key are chosen by, so Visits reports the corruption instead of
// guessing.
func TestVisitsRefusesABrokenHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []Record
	}{
		{
			name: "a node_started opened over another",
			records: new(history).run(EventRunStarted).
				started("plan", 1).started("code", 1).records,
		},
		{
			name: "a node_finished with nothing open",
			records: new(history).run(EventRunStarted).
				finished("plan", 1, StatusOK, "code").records,
		},
		{
			name: "a seq out of order",
			records: []Record{
				{Seq: 1, Event: EventRunStarted},
				{Seq: 3, Event: EventNodeStarted, Node: "plan", Attempt: 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Visits(tt.records)
			if !errors.Is(err, ErrCorruptJournal) {
				t.Fatalf("Visits() error = %v, want ErrCorruptJournal", err)
			}
			if got != nil {
				t.Errorf("Visits() = %+v alongside the error, want nil", got)
			}
		})
	}
}
