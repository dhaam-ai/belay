//go:build unix

package queue

import (
	"fmt"

	"github.com/dhaam-ai/belay/internal/state"
)

// RunView is what a worker can see of one goal's run on disk.
type RunView struct {
	// Exists is set when the run's manifest is there: the run was made.
	// createRun writes the manifest last, so a run without one was never
	// finished being made, and nothing in it has run.
	Exists bool
	// Status is the run's lifecycle state, from its manifest.
	Status state.RunStatus
	// GateAsked is set when the run is paused at the approve gate because
	// the gate asked a person about its plan. Only the run's journal can
	// tell that: a Ctrl-C while the gate runs pauses the run at the gate too,
	// and nobody was asked anything then.
	GateAsked bool
}

// Action is what a worker does next.
type Action int

const (
	// ActionDone is a queue with nothing left a worker may run: every goal
	// is ended, parked, or removed.
	ActionDone Action = iota
	// ActionClaim is taking Decision.Entry, the first queued goal.
	ActionClaim
	// ActionCreate is making Decision.Entry's copy and its run, which its
	// claim names and which do not exist yet.
	ActionCreate
	// ActionDrive is dispatching Decision.Entry's run, continuing it from
	// wherever it stands.
	ActionDrive
	// ActionSettle is recording Decision.Entry as Decision.Settle, which its
	// run's state already says, without dispatching anything.
	ActionSettle
)

var actionNames = [...]string{
	ActionDone:   "done",
	ActionClaim:  "claim",
	ActionCreate: "create",
	ActionDrive:  "drive",
	ActionSettle: "settle",
}

// String names a, such as "drive".
func (a Action) String() string {
	if a < 0 || int(a) >= len(actionNames) {
		return fmt.Sprintf("Action(%d)", int(a))
	}
	return actionNames[a]
}

// Decision is what a worker does next, and to which goal.
type Decision struct {
	Action Action
	// Entry is the goal the action is for. It is the zero Entry for
	// ActionDone.
	Entry Entry
	// Settle and Note are what ActionSettle records.
	Settle State
	Note   string
}

// Next decides what a worker does next, from the queue and what each goal's
// run looks like on disk. runs is keyed by goal id and needs an entry only
// for goals in hand, claimed or approved; a goal with none reads as one
// whose run was never made.
//
// It is pure, so a worker asks it again after every step, from what is on
// disk then, and a worker that was killed between any two steps is a new
// worker asking the same question of the same disk. That is the whole of
// crash recovery here: there is no recovery mode, only the next decision.
//
// The order is the rule "finish what you started before you start anything
// new":
//
//  1. A claimed goal, in id order. There is at most one, because nothing is
//     claimed while one is in hand. Its run is made if it was not, settled if
//     it ended or its gate asked, and driven otherwise.
//  2. An approved goal, in id order: a person released it from its gate, and
//     its plan is already paid for.
//  3. The first queued goal, which is claimed.
//
// A parked goal is never driven. Its run is paused at the gate because the
// gate asked a person about its plan, and continuing it without them would be
// belay removing a gate on its own. Only an approval moves it on.
//
// A run that is paused but whose gate did not ask is driven. That is the
// state a Ctrl-C leaves (the dispatcher pauses on cancel, and the worker
// settles nothing when it is interrupted), and continuing it is what `belay
// queue start` promises after one. It is also the state of a run paused at
// one of belay's own limits when the worker was killed before it could
// settle the goal. Driving that run makes the dispatcher check the limit
// again: met again at the budget or the step cap, it pauses at once having
// spent nothing, and the goal is settled; a run paused at graph.node_timeout
// runs the step that overran once more, within the goal's budget. Telling the
// two apart would mean reading the journal's notes, which say so only in
// words.
func Next(q Queue, runs map[int]RunView) Decision {
	for _, e := range q.Entries {
		if e.State == StateClaimed {
			return claimedNext(e, runs[e.ID])
		}
	}
	for _, e := range q.Entries {
		if e.State == StateApproved {
			return approvedNext(e, runs[e.ID])
		}
	}
	for _, e := range q.Entries {
		if e.State == StateQueued {
			return Decision{Action: ActionClaim, Entry: e}
		}
	}
	return Decision{Action: ActionDone}
}

// claimedNext decides for a goal a worker claimed and has not settled.
func claimedNext(e Entry, run RunView) Decision {
	if !run.Exists {
		// The claim is durable and nothing it names exists yet, or only a
		// copy a crash cut short does: make both.
		return Decision{Action: ActionCreate, Entry: e}
	}
	if s, ended := endedAs(run.Status); ended {
		return Decision{Action: ActionSettle, Entry: e, Settle: s}
	}
	if run.Status == state.RunStatusPaused && run.GateAsked {
		return Decision{Action: ActionSettle, Entry: e, Settle: StateWaiting}
	}
	return Decision{Action: ActionDrive, Entry: e}
}

// approvedNext decides for a goal a person released from its gate.
func approvedNext(e Entry, run RunView) Decision {
	if !run.Exists {
		// Making the run again would write and pay for a new plan the person
		// never saw, behind an approval they gave a different one.
		return Decision{Action: ActionSettle, Entry: e, Settle: StateStopped,
			Note: fmt.Sprintf("its run, %s, is gone, so there is nothing to continue", e.RunID)}
	}
	if s, ended := endedAs(run.Status); ended {
		return Decision{Action: ActionSettle, Entry: e, Settle: s}
	}
	return Decision{Action: ActionDrive, Entry: e}
}

// endedAs maps a run that has ended onto the state its goal settles in.
func endedAs(s state.RunStatus) (State, bool) {
	switch s {
	case state.RunStatusCompleted:
		return StateFinished, true
	case state.RunStatusFailed:
		return StateFailed, true
	case state.RunStatusAborted:
		return StateStopped, true
	default:
		return StateUnknown, false
	}
}
