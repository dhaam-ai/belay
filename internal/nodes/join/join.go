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
// this node's job — see "Promotion is manual" below.
//
// # The selection table
//
// The three rules live in select.go as an explicit table, one row per
// fanout.select value, each carrying the sentence that describes it. Read
// Strategies to see them, and Select for the invariants every row inherits:
// only candidates whose tests passed are eligible, candidates are offered in
// the order fanout recorded them, and the outcome is deterministic for a
// given input.
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
// # "No candidate passed" is a result, not an error
//
// When every candidate's tests failed, Run returns journal.StatusFailed with
// a note saying so, and explicitly clears state.Winner. It does not pick the
// least-bad loser. N attempts that all failed is real, useful information
// about the goal — most likely that the goal, the plan or the test suite is
// the problem rather than any candidate — and naming the best of N failures
// "the winner" would hide exactly that, while inviting someone to promote
// code whose tests do not pass. Clearing the winner rather than leaving the
// field untouched is deliberate too: state.Patch fields are absolute sets,
// and a stale winner from an earlier attempt outliving a join that found
// none would be a lie the blackboard told forever.
//
// # Promotion is manual
//
// The winning candidate's changes are NOT copied back into the original
// repository, by this node or by anything else in belay. ADR-0004 chose
// plain directory copies over git worktrees and accepted "manual merge of
// winning code" as the price: with worktrees, promoting a winner would be a
// branch merge; with directory copies there is no merge algorithm, and
// inventing one here — copy every file that differs? every file the agent
// reported changing? what about deletions? — would be a silent, unreviewable
// three-way merge written by a node that has never seen the original tree.
//
// So the honest cost, which Run states in its Note, logs at Info, and this
// doc states here, is that a human must do it:
//
//  1. Take the winner's directory from state.Candidates[i].Dir, where i is
//     the candidate whose ID equals state.Winner.
//  2. Diff it against the original repository — for a git checkout,
//     "git -C <repo> diff --no-index -- <repo> <winner-dir>" or simply
//     copying the winner's tree into a scratch branch and reviewing it.
//  3. Copy the wanted changes back by hand, resolving anything that
//     conflicts with work done in the original tree since the run started.
//  4. Delete the candidate directories under <run-dir>/candidates when
//     finished; nothing reclaims them automatically.
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

	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/state"
)

// Node is the join graph node. It holds no state: everything it needs
// arrives in the graph.RunContext, so one value is safe to register once and
// reuse for the life of a process.
type Node struct{}

// New returns the join node.
func New() *Node { return &Node{} }

var _ graph.Node = (*Node)(nil)

// Name returns graph.NodeJoin.
func (n *Node) Name() string { return graph.NodeJoin }

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
	return winner(log, outcome), nil
}

// noWinner reports that every candidate failed.
func noWinner(log *slog.Logger, outcome Outcome) graph.Result {
	log.Warn("join: every candidate failed its tests, so this run has no winner",
		"candidates", outcome.Total,
		"strategy", string(outcome.Strategy.Name),
	)

	// Set rather than left alone: see the package doc. An empty Winner is
	// the accurate absolute value for a run that selected nobody.
	none := ""
	return graph.Result{
		Status: journal.StatusFailed,
		Patch:  state.Patch{Winner: &none},
		Note: fmt.Sprintf("no winner: all %d candidates failed their tests, so %s had nothing to choose between",
			outcome.Total, outcome.Strategy.Name),
	}
}

// winner reports the selected candidate and ends the run.
func winner(log *slog.Logger, outcome Outcome) graph.Result {
	id := outcome.Winner.ID
	dir := outcome.Winner.Dir

	if dir == "" {
		// Not fatal: the selection itself only needs an ID, and the run's
		// record of who won is still correct. It is worth saying out loud
		// because promotion is manual, and a winner with no directory
		// leaves the user nothing to promote from.
		log.Warn("join: the winning candidate records no directory, so there is nothing to promote by hand", "winner", id)
	}
	if !outcome.Strategy.Exact() {
		log.Warn("join: the selection strategy is a documented approximation",
			"strategy", string(outcome.Strategy.Name),
			"approximation", outcome.Strategy.Approximation,
		)
	}
	log.Info("join: selected a winning candidate; its changes are NOT promoted automatically (ADR-0004)",
		"winner", id,
		"dir", dir,
		"strategy", string(outcome.Strategy.Name),
		"passed", outcome.Passed,
		"candidates", outcome.Total,
		"issues", outcome.Winner.IssueCount,
	)

	return graph.Result{
		Next:   graph.End,
		Status: journal.StatusOK,
		Patch:  state.Patch{Winner: &id},
		Note: fmt.Sprintf("%s selected candidate %s of %d (%d passed); belay does not merge, so copy its changes back from %s by hand",
			outcome.Strategy.Name, id, outcome.Total, outcome.Passed, dirOrUnknown(dir)),
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
