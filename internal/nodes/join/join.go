// Package join implements belay's best-of-N join node: the step that turns
// the N candidate workspaces fanout prepared into one winner, records it on
// the blackboard, and ends the run.
//
// # What this node does
//
// Run reads state.Candidates, applies the rule named by fanout.select, and
// returns a Patch setting state.Winner to the chosen candidate's ID. That is
// all it does. It copies no files, merges nothing, and destroys no
// workspaces: a winner is a decision, and acting on that decision is not
// this node's job — see "Promotion" below.
//
// # The selection table
//
// The three rules live in select.go as an explicit table, one row per
// fanout.select value, each carrying the sentence that describes it. Read
// Strategies to see them, and Select for the invariants every row inherits:
// only eligible candidates are offered to a rule, candidates are offered in
// the order fanout recorded them, and the outcome is deterministic for a
// given input.
//
// Eligible means the candidate's tests passed AND its own candidate run
// completed — see eligible, which argues the second half. It is one
// predicate rather than a term inside each row, so no rule can opt out of
// it and the next thing that disqualifies a candidate is added in one
// place.
//
// Determinism is not a nicety here. The dispatcher re-runs a node whose
// start has no matching finish, which is exactly how a crash inside join
// presents on resume; because selection is a pure function of the
// candidates, the re-run lands on the same winner as the attempt that
// crashed. A rule that could pick differently would let a resumed run
// promote a different candidate than the one the crashed run had already
// reported, and no best-of-N whose result cannot be reproduced can be
// audited.
//
// # "No candidate was eligible" is a result, not an error
//
// When no candidate is eligible, Run returns journal.StatusFailed with a
// note saying so, and explicitly clears state.Winner. It does not pick the
// least-bad loser. N attempts that all failed is real, useful information
// about the goal — most likely that the goal, the plan or the test suite is
// the problem rather than any candidate — and naming the best of N failures
// "the winner" would hide exactly that, while inviting someone to promote
// code whose tests do not pass. Clearing the winner rather than leaving the
// field untouched is deliberate too: state.Patch fields are absolute sets,
// and a stale winner from an earlier attempt outliving a join that found
// none would be a lie the blackboard told forever.
//
// The note distinguishes three runs, because a person acts on them very
// differently: every candidate ran and failed its tests (the goal, the plan
// or the test suite is wrong); no candidate ever completed a run (the
// machine, the isolator or the budget stopped them, and nothing has been
// learned about the code); and a mixture. Reporting the second as the first
// would send someone to debug a test suite no candidate reached.
//
// # Promotion
//
// Who acts on the decision depends on what the candidates are, and it is
// never this node.
//
// When they are branches — a run that delivers its result on a branch, whose
// candidates are git worktrees — the winner goes to the promote node, which
// merges its branch into the run's. WithOnSelected is how that edge is wired,
// from the run's manifest, and the Note then says so instead of asking for a
// copy by hand.
//
// When they are directory copies, nothing in belay promotes them. ADR-0004
// chose plain directory copies over git worktrees and accepted "manual merge
// of winning code" as the price: with worktrees, promoting a winner is a
// branch merge; with directory copies there is no merge algorithm, and
// inventing one here — copy every file that differs? every file the agent
// reported changing? what about deletions? — would be a silent, unreviewable
// three-way merge written by a node that has never seen the original tree.
//
// So for those runs the honest cost, which Run states in its Note, logs at
// Info, and this doc states here, is that a human must do it:
//
//  1. Take the winner's directory from state.Candidates[i].Dir, where i is
//     the candidate whose ID equals state.Winner.
//  2. Diff it against the original repository — for a git checkout,
//     "git -C <repo> diff --no-index -- <repo> <winner-dir>" or simply
//     copying the winner's tree into a scratch branch and reviewing it.
//  3. Copy the wanted changes back by hand, resolving anything that
//     conflicts with work done in the original tree since the run started.
//  4. Delete the candidate directories under <run-dir>/candidates when
//     finished; nothing reclaims them automatically. (A run that promotes by
//     merge has its candidates destroyed by the promote node instead.)
//
// # Errors versus results
//
// Per ADR-0002, a Result reports an outcome this node understands and an
// error reports that it could not reach a verdict at all. Reaching join with
// no candidates, with candidates that cannot be named by ID, or with a
// fanout.select value that has no rule are all in the second category: they
// are wiring or configuration bugs in the caller, not verdicts about the
// run's code. See ErrNoCandidates, ErrInvalidCandidate and
// ErrUnknownStrategy.
package join

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
)

// Node is the join graph node. What it selects comes entirely from the
// graph.RunContext; the one thing it holds is where a winner goes, which is
// wiring, fixed when the node is built and the same for every execution.
type Node struct {
	// onSelected is where a selected winner goes: graph.End for a run that
	// promotes by hand, or the node that merges it.
	onSelected string
}

// Option configures where a join Node sends the winner it selected.
//
// The value it carries is decided once, when the run is created, and pinned
// in the run's manifest; internal/nodes explains why an edge may not be read
// from live configuration.
type Option func(*Node)

// WithOnSelected sends the selected winner to next instead of ending the
// run. A run whose candidates are branches passes graph.NodePromote, which
// merges the winner; the Note then stops telling the person to copy it by
// hand, because belay is about to do it.
func WithOnSelected(next string) Option {
	return func(n *Node) { n.onSelected = next }
}

// New returns the join node. Without options a selected winner ends the run
// and its changes are promoted by hand.
func New(opts ...Option) *Node {
	n := &Node{onSelected: graph.End}
	for _, opt := range opts {
		opt(n)
	}
	return n
}

var (
	_ graph.Node     = (*Node)(nil)
	_ graph.Unpriced = (*Node)(nil)
)

// Name returns graph.NodeJoin.
func (n *Node) Name() string { return graph.NodeJoin }

// SpendsNothing exempts this node from the dispatcher's budget ceiling
// check.
//
// It is not an exemption from the ceiling but the observation that the
// ceiling has nothing to say about a node that cannot spend: join compares
// numbers other nodes produced, calls no agent, and its Usage is zero by
// construction. Checking before it would stop a fanout whose candidates
// spent the ceiling from collecting the winner it had already selected and
// paid for — money saved: none; work lost: all of it. See graph.Unpriced.
func (n *Node) SpendsNothing() {}

// Run selects the winning candidate and ends the run.
//
// A selected winner routes to graph.End with journal.StatusOK and a Patch
// setting state.Winner. Every candidate failing routes nowhere with
// journal.StatusFailed and a Patch clearing state.Winner. A wiring bug
// returns an error and no Patch at all.
//
// Usage stays zero: this node compares numbers that other nodes already
// produced, and calls no agent.
func (n *Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc == nil {
		return graph.Result{Status: journal.StatusFailed}, errors.New("join: nil run context")
	}
	if err := ctx.Err(); err != nil {
		return graph.Result{Status: journal.StatusAborted}, fmt.Errorf("join: %w", err)
	}

	outcome, err := Select(rc.State.Candidates, rc.Config.Fanout.Select)
	if err != nil {
		return graph.Result{Status: journal.StatusFailed}, err
	}

	log := logger(rc)
	if !outcome.Selected() {
		return noWinner(log, outcome), nil
	}
	return winner(log, outcome, n.onSelected), nil
}

// noWinner reports that no candidate was eligible, and why.
//
// The why is the whole of this function. "No winner" covers three runs a
// person would act on very differently — the goal or the test suite is
// wrong; the machine could not run anything; some candidates got nowhere —
// and reporting them identically tells two of the three something false.
func noWinner(log *slog.Logger, outcome Outcome) graph.Result {
	log.Warn("join: no candidate was eligible, so this run has no winner",
		"candidates", outcome.Total,
		"completed", outcome.Completed,
		"strategy", string(outcome.Strategy.Name),
	)

	// Set rather than left alone: see the package doc. An empty Winner is
	// the accurate absolute value for a run that selected nobody.
	none := ""
	return graph.Result{
		Status: journal.StatusFailed,
		Patch:  state.Patch{Winner: &none},
		Note:   noWinnerNote(outcome),
	}
}

// noWinnerNote says which of the three "no winner" runs this was.
func noWinnerNote(o Outcome) string {
	tail := fmt.Sprintf(", so %s had nothing to choose between", o.Strategy.Name)
	switch o.Completed {
	case 0:
		// Nothing ran, so nothing can be said about the code. Saying
		// "failed their tests" here would send someone to debug a test
		// suite no candidate reached.
		return fmt.Sprintf("no winner: none of the %d candidates completed its run%s", o.Total, tail)
	case o.Total:
		return fmt.Sprintf("no winner: all %d candidates failed their tests%s", o.Total, tail)
	default:
		return fmt.Sprintf("no winner: of %d candidates, %d failed the tests and %d never completed%s",
			o.Total, o.Completed, o.Total-o.Completed, tail)
	}
}

// winner reports the selected candidate and routes it to next, which is
// graph.End for a run nothing can merge for.
func winner(log *slog.Logger, outcome Outcome, next string) graph.Result {
	id := outcome.Winner.ID
	dir := outcome.Winner.Dir

	if dir == "" {
		// Not fatal: the selection itself only needs an ID, and the run's
		// record of who won is still correct. It is worth saying out loud
		// because a winner with no directory leaves nothing to promote
		// from, by hand or by merge.
		log.Warn("join: the winning candidate records no directory, so there is nothing to promote", "winner", id)
	}
	if !outcome.Strategy.Exact() {
		log.Warn("join: the selection strategy is a documented approximation",
			"strategy", string(outcome.Strategy.Name),
			"approximation", outcome.Strategy.Approximation,
		)
	}
	log.Info("join: selected a winning candidate",
		"winner", id,
		"dir", dir,
		"next", next,
		"strategy", string(outcome.Strategy.Name),
		"passed", outcome.Passed,
		"candidates", outcome.Total,
		"issues", outcome.Winner.IssueCount,
	)

	selected := fmt.Sprintf("%s selected candidate %s of %d (%d passed)",
		outcome.Strategy.Name, id, outcome.Total, outcome.Passed)
	note := selected + "; " + graph.NodePromote + " merges its branch into the run's"
	if next == graph.End {
		note = selected + "; belay does not merge, so copy its changes back from " + dirOrUnknown(dir) + " by hand"
	}
	return graph.Result{
		Next:   next,
		Status: journal.StatusOK,
		Patch:  state.Patch{Winner: &id},
		Note:   note,
	}
}

// dirOrUnknown keeps the Note readable when a candidate carries no
// directory, rather than telling a user to copy changes back from "".
func dirOrUnknown(dir string) string {
	if dir == "" {
		return "its workspace (which the blackboard does not record)"
	}
	return dir
}

// logger returns rc's logger, falling back to the default one. The
// dispatcher always sets Logger; a test building a RunContext by hand often
// does not, and a nil-pointer panic is never the right answer to a missing
// log destination.
func logger(rc *graph.RunContext) *slog.Logger {
	if rc.Logger != nil {
		return rc.Logger
	}
	return slog.Default()
}
