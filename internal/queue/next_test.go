//go:build unix

package queue

import (
	"testing"

	"github.com/dhaam-ai/belay/internal/state"
)

func TestNext(t *testing.T) {
	t.Parallel()

	goal := func(id int, s State) Entry { return Entry{ID: id, Goal: "a goal", State: s, RunID: runIDFor(id)} }
	none := RunView{}
	running := RunView{Exists: true, Status: state.RunStatusRunning}
	asked := RunView{Exists: true, Status: state.RunStatusPaused, GateAsked: true}
	interrupted := RunView{Exists: true, Status: state.RunStatusPaused}
	completed := RunView{Exists: true, Status: state.RunStatusCompleted}
	failed := RunView{Exists: true, Status: state.RunStatusFailed}
	aborted := RunView{Exists: true, Status: state.RunStatusAborted}

	tests := []struct {
		name    string
		entries []Entry
		runs    map[int]RunView
		want    Action
		id      int
		settle  State
	}{
		{name: "an empty queue", want: ActionDone},
		{name: "the first queued goal is claimed", entries: []Entry{goal(1, StateQueued), goal(2, StateQueued)}, want: ActionClaim, id: 1},
		{name: "a removed goal is passed over", entries: []Entry{goal(1, StateRemoved), goal(2, StateQueued)}, want: ActionClaim, id: 2},
		{
			name: "nothing left that may run",
			entries: []Entry{goal(1, StateFinished), goal(2, StateFailed), goal(3, StateStopped),
				goal(4, StatePaused), goal(5, StateWaiting), goal(6, StateRemoved)},
			want: ActionDone,
		},

		// A claimed goal: whatever its run says, it is finished before
		// anything new is started.
		{name: "claimed, and its run not made yet", entries: []Entry{goal(1, StateClaimed)}, runs: map[int]RunView{1: none}, want: ActionCreate, id: 1},
		{name: "claimed, and its run cut off", entries: []Entry{goal(1, StateClaimed)}, runs: map[int]RunView{1: running}, want: ActionDrive, id: 1},
		{name: "claimed, and its run stopped by Ctrl-C", entries: []Entry{goal(1, StateClaimed)}, runs: map[int]RunView{1: interrupted}, want: ActionDrive, id: 1},
		{
			name: "claimed, and its gate asked: parked, never driven", entries: []Entry{goal(1, StateClaimed)},
			runs: map[int]RunView{1: asked}, want: ActionSettle, id: 1, settle: StateWaiting,
		},
		{
			name: "claimed, and its run completed", entries: []Entry{goal(1, StateClaimed)},
			runs: map[int]RunView{1: completed}, want: ActionSettle, id: 1, settle: StateFinished,
		},
		{
			name: "claimed, and its run failed", entries: []Entry{goal(1, StateClaimed)},
			runs: map[int]RunView{1: failed}, want: ActionSettle, id: 1, settle: StateFailed,
		},
		{
			name: "claimed, and its run aborted", entries: []Entry{goal(1, StateClaimed)},
			runs: map[int]RunView{1: aborted}, want: ActionSettle, id: 1, settle: StateStopped,
		},

		// An approved goal: driven past its gate, which reads the approval.
		{name: "approved, at its gate", entries: []Entry{goal(1, StateApproved)}, runs: map[int]RunView{1: asked}, want: ActionDrive, id: 1},
		{name: "approved, and cut off since", entries: []Entry{goal(1, StateApproved)}, runs: map[int]RunView{1: running}, want: ActionDrive, id: 1},
		{
			name: "approved, and finished by hand since", entries: []Entry{goal(1, StateApproved)},
			runs: map[int]RunView{1: completed}, want: ActionSettle, id: 1, settle: StateFinished,
		},
		{
			name: "approved, and its run gone", entries: []Entry{goal(1, StateApproved)},
			runs: map[int]RunView{1: none}, want: ActionSettle, id: 1, settle: StateStopped,
		},

		// Order: started work before new work, and a claim before an approval.
		{
			name: "a parked goal is passed over", entries: []Entry{goal(1, StateWaiting), goal(2, StateQueued)},
			want: ActionClaim, id: 2,
		},
		{
			name: "a goal in hand before a new claim", entries: []Entry{goal(1, StateClaimed), goal(2, StateQueued)},
			runs: map[int]RunView{1: running}, want: ActionDrive, id: 1,
		},
		{
			name: "an approved goal before a new claim", entries: []Entry{goal(1, StateQueued), goal(2, StateApproved)},
			runs: map[int]RunView{2: asked}, want: ActionDrive, id: 2,
		},
		{
			name: "the goal in hand before an approved one", entries: []Entry{goal(1, StateApproved), goal(2, StateClaimed)},
			runs: map[int]RunView{1: asked, 2: running}, want: ActionDrive, id: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Next(Queue{Entries: tt.entries}, tt.runs)
			if got.Action != tt.want || got.Entry.ID != tt.id || got.Settle != tt.settle {
				t.Errorf("Next() = %v goal %d (settle %s), want %v goal %d (settle %s)",
					got.Action, got.Entry.ID, got.Settle, tt.want, tt.id, tt.settle)
			}
		})
	}
}
