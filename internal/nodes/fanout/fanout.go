// Package fanout implements belay's best-of-N fanout node: the step that
// gives a run N independent attempts at the same goal so a later join node
// can keep the best one.
//
// # What this node does, and what it does not
//
// Run creates one isolated workspace per candidate through rc.Isolator,
// records each as a state.Candidate in the returned Patch, and routes to
// graph.NodeJoin. That is all it does. It does not drive a candidate's
// code/test/review cycle, and it does not select a winner — the plan gives
// every candidate its own journal, and the v0.1 dispatcher does not yet
// provide one. Until it does, candidates are prepared SEQUENTIALLY, in one
// goroutine, in candidate order: this node starts nothing concurrent.
// Concurrency is a dispatcher capability, not a node one, and pretending
// otherwise here would produce N candidates whose journals interleave into
// a single run journal that no resume could untangle.
//
// # Cost
//
// Fanout is the one node that multiplies a run's bill by N, which is why
// ADR-0007 put a budget ceiling in belay on day one and why
// fanout.enabled defaults to false. Run therefore performs a mandatory
// budget PRE-check before it creates anything — see ProjectCandidateCost
// for the projection and its justification. Discovering the overrun after
// four of five workspaces have already been paid for is exactly the
// failure the ceiling exists to prevent, so the check happens while
// refusing is still free.
//
// # Failure and cleanup
//
// Every failure path this node owns leaves nothing behind: if candidate 4
// of 5 cannot be created, the three already created are destroyed before
// the error is returned, and the same holds for a cancelled context. Any
// destroy failure is joined onto the returned error rather than swallowed,
// because a leaked workspace is disk that nothing will ever reclaim
// (ADR-0004: fanout already costs O(N x repo size)).
//
// # Errors versus results
//
// Per ADR-0002 a Result reports an outcome this node understands and an
// error reports that it could not reach a verdict at all. Every guard here
// is in the second category: fanout reached with fanout.enabled false, or
// with fewer than two candidates, or without an Isolator, is a wiring or
// configuration bug in the caller, not a verdict about the run's code.
package fanout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dhaam-ai/belay/internal/budget"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// minCandidates is the smallest candidate count that means anything:
// "best of one" is just a run. config.Config.Validate already rejects a
// smaller value when fanout is enabled; Run re-checks rather than trusting
// that validation ran, because the cost of being wrong is N workspaces.
const minCandidates = 2

// Sentinel errors returned by Run. Callers detect them with errors.Is.
//
// A budget refusal is not among them: it is reported with the
// *belay.BudgetError that internal/budget produced, so
// errors.Is(err, belay.ErrBudgetExceeded) and errors.As(err, &budgetErr)
// both keep working through this node.
var (
	// ErrDisabled reports that the fanout node ran while
	// fanout.enabled was false.
	//
	// This is deliberately an error rather than a quiet pass-through to
	// join. Reaching this node means the graph routed to it, and the only
	// way that happens with fanout disabled is a wiring bug. Passing
	// through would hand join zero candidates and turn a clear bug at its
	// source into a confusing failure one node later; creating the
	// workspaces anyway would spend N times a run's budget the user
	// explicitly did not ask to spend. Failing here costs nothing and
	// names the actual problem.
	ErrDisabled = errors.New("fanout: node reached while fanout.enabled is false")

	// ErrTooFewCandidates reports a candidate count below minCandidates
	// while fanout is enabled.
	ErrTooFewCandidates = errors.New("fanout: candidate count is too low")

	// ErrNoIsolator reports that rc.Isolator was nil. Fanout has no
	// meaning without isolation: every candidate would edit the same
	// files.
	ErrNoIsolator = errors.New("fanout: no isolator configured")

	// ErrRunMetadata reports that the run's manifest could not be read, or
	// does not carry what fanout needs from it: the path of the repository
	// candidates are copied from, and the run's spend so far, which is the
	// only evidence the budget pre-check has to work from. Failing here is
	// deliberate — a fanout that cannot see the ledger must not launch N
	// candidates on the assumption that it is affordable.
	ErrRunMetadata = errors.New("fanout: run metadata is unusable")

	// ErrIsolatorContract reports an Isolator that returned success but not
	// a usable workspace — today, one with an empty Dir. belay.Workspace
	// documents Dir as the directory an agent, runner and linter treat as
	// the repository root, so a candidate without one is a candidate no
	// later node can do anything with. Catching it here, while unwinding is
	// still possible, beats handing join a workspace that points nowhere.
	ErrIsolatorContract = errors.New("fanout: isolator returned an unusable workspace")
)

// Node is the fanout graph node. It holds no state: everything it needs
// arrives in the graph.RunContext, so one value is safe to register once
// and reuse for the life of a process.
type Node struct{}

// New returns the fanout node.
func New() *Node { return &Node{} }

var _ graph.Node = (*Node)(nil)

// Name returns graph.NodeFanout.
func (n *Node) Name() string { return graph.NodeFanout }

// Run prepares this run's candidate workspaces and routes to
// graph.NodeJoin.
//
// The order of operations is the point of this function: every check that
// could refuse the fanout runs before anything is created, so a refusal
// costs nothing. Configuration and adapter guards come first because they
// are free, the budget pre-check second because it is the one that stops
// real money, and only then is a single workspace allowed to exist.
func (n *Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	count, err := checkConfig(rc)
	if err != nil {
		return graph.Result{}, err
	}

	src, spent, err := runMetadata(rc)
	if err != nil {
		return graph.Result{}, err
	}

	projected, err := affordable(rc, count, spent)
	if err != nil {
		return graph.Result{}, err
	}

	log := logger(rc)
	log.Info("fanout: budget pre-check passed, preparing candidates",
		"candidates", count,
		"src", src,
		"spent_usd", spent.USD,
		"projected_usd_per_candidate", projected.USD,
		"projected_usd_total", projected.USD*float64(count),
		"max_usd", rc.Config.Budget.MaxUSD,
	)

	candidates, err := createCandidates(ctx, rc, log, src, count)
	if err != nil {
		return graph.Result{}, err
	}
	log.Info("fanout: candidate workspaces ready", "candidates", len(candidates), "next", graph.NodeJoin)

	// Usage stays zero: this node copies directories, it never calls an
	// agent. The projection above is what a candidate is expected to cost
	// once something else runs it, not a cost incurred here.
	return graph.Result{
		Next:   graph.NodeJoin,
		Status: journal.StatusOK,
		Patch:  state.Patch{Candidates: &candidates},
		Note: fmt.Sprintf("prepared %d candidate workspaces sequentially, projected at $%.4f each",
			len(candidates), projected.USD),
	}, nil
}

// createCandidates creates one workspace per candidate, in order, and
// records each on the blackboard shape join will read.
//
// It is all-or-nothing: any failure — a rejected id, a failed Create, a
// cancelled context — unwinds every workspace created so far before
// returning, so this node never leaves a partial fanout behind for
// something else to notice and clean up.
func createCandidates(ctx context.Context, rc *graph.RunContext, log *slog.Logger, src string, count int) ([]state.Candidate, error) {
	created := make([]belay.Workspace, 0, count)
	candidates := make([]state.Candidate, 0, count)

	for i := 1; i <= count; i++ {
		id := candidateID(i, rc.Attempt)

		// Every id goes through Layout, which already rejects anything that
		// could escape the run directory, so this node inherits that
		// protection instead of reimplementing it — and a future id scheme
		// cannot quietly slip past a check that lives somewhere else.
		layoutDir, err := rc.Layout.CandidateDir(id)
		if err != nil {
			return nil, unwind(ctx, rc.Isolator, log, created, fmt.Errorf("fanout: candidate %d of %d: %w", i, count, err))
		}
		if err := ctx.Err(); err != nil {
			return nil, unwind(ctx, rc.Isolator, log, created, fmt.Errorf("fanout: cancelled before creating candidate %q: %w", id, err))
		}

		ws, err := rc.Isolator.Create(ctx, src, id)
		if err != nil {
			// belay.Isolator requires Create to leave nothing behind on
			// error, so ws is not added to the unwind list here — only the
			// candidates that actually exist are.
			return nil, unwind(ctx, rc.Isolator, log, created, fmt.Errorf("fanout: create candidate %q of %d: %w", id, count, err))
		}
		created = append(created, ws)
		if ws.Dir == "" {
			return nil, unwind(ctx, rc.Isolator, log, created, fmt.Errorf("%w: candidate %q has no directory", ErrIsolatorContract, id))
		}
		if !ws.Ephemeral {
			// Not fatal: a "no isolation" isolator is a legitimate local
			// debugging mode. It is still worth saying out loud, because N
			// candidates sharing one tree cannot be independent attempts.
			log.Warn("fanout: candidate workspace is not ephemeral, so candidates may share one tree and overwrite each other",
				"id", id, "dir", ws.Dir)
		}
		log.Debug("fanout: candidate workspace created",
			"id", id, "dir", ws.Dir, "layout_dir", layoutDir, "ephemeral", ws.Ephemeral)

		// Dir is the workspace the isolator actually produced, not the
		// layout path above: state.Candidate.Dir is what later nodes hand to
		// an agent as a repository root, and only the isolator knows where
		// that ended up. TestPassed and IssueCount stay zero — this node
		// creates candidates, it does not run them.
		candidates = append(candidates, state.Candidate{ID: id, Dir: ws.Dir})
	}

	// A context cancelled after the last Create still has to unwind: a
	// cancelled fanout that returns N live workspaces has leaked all of
	// them.
	if err := ctx.Err(); err != nil {
		return nil, unwind(ctx, rc.Isolator, log, created,
			fmt.Errorf("fanout: cancelled after creating %d candidate workspaces: %w", len(created), err))
	}
	return candidates, nil
}

// unwind destroys every workspace created so far, newest first, and
// returns cause with any destroy failure joined onto it. Joining rather
// than logging-and-discarding matters: a workspace that could not be
// destroyed is disk nothing will reclaim, and ADR-0004 already accepted
// O(N x repo size) for a fanout that cleans up after itself.
func unwind(ctx context.Context, iso belay.Isolator, log *slog.Logger, created []belay.Workspace, cause error) error {
	if len(created) == 0 {
		return cause
	}

	// Cleanup runs on a context detached from ctx's cancellation. The
	// commonest reason to be unwinding at all is that ctx was cancelled,
	// and an Isolator is entitled to refuse work on a cancelled context —
	// internal/isolate/dircopy's Destroy returns immediately on one.
	// Passing the cancelled context here would turn "clean up after a
	// cancelled fanout" into "leak everything a cancelled fanout created".
	cleanupCtx := context.WithoutCancel(ctx)

	errs := make([]error, 0, len(created)+1)
	errs = append(errs, cause)
	destroyed := 0
	for i := len(created) - 1; i >= 0; i-- {
		ws := created[i]
		if err := iso.Destroy(cleanupCtx, ws); err != nil {
			log.Error("fanout: could not destroy a candidate workspace while unwinding",
				"id", ws.ID, "dir", ws.Dir, "error", err)
			errs = append(errs, fmt.Errorf("fanout: destroy candidate %q: %w", ws.ID, err))
			continue
		}
		destroyed++
	}
	log.Warn("fanout: unwound candidate workspaces after a failure",
		"created", len(created), "destroyed", destroyed, "cause", cause.Error())

	return errors.Join(errs...)
}

// candidateID names candidate index (1-based) of this node's attempt-th
// execution.
//
// The attempt suffix is what makes a re-run safe. The dispatcher re-runs a
// node whose start has no matching finish, which is exactly how a crash
// inside fanout presents on resume — and by then the previous attempt's
// candidate directories are still on disk. An isolator may refuse to
// create a workspace over a non-empty directory (dircopy does, because
// copying over an existing tree hands an agent a blend of two checkouts as
// if it were one), so reusing ids would make every resumed fanout fail on
// its own debris. Distinct ids per attempt avoid that; the stale
// directories from the crashed attempt are the dispatcher's to reclaim,
// since this node never saw them.
func candidateID(index, attempt int) string {
	if attempt < 1 {
		attempt = 1
	}
	return fmt.Sprintf("cand-%02d-a%d", index, attempt)
}

// ProjectCandidateCost projects what one fanout candidate will cost, given
// spent, the run's cumulative usage up to this node.
//
// # The estimate, and why it is this one
//
// A candidate is a re-run of the agent work that has already happened:
// this run reached fanout by planning and coding once, and each candidate
// now repeats a comparable code/test/fix/review cycle in its own
// workspace. So the best evidence for what one candidate costs is what
// this run has cost so far, and the projection is exactly that — the run
// to date, replayed once per candidate. Equivalently, and in the units the
// ledger records: the run's mean spend per node execution multiplied by
// the number of node executions a candidate repeats.
//
// It is deliberately biased high rather than low. A per-node average would
// project a fraction of a candidate's real cost, because a candidate is
// many nodes and not one, and a guard that under-projects does not guard.
// The result is always marked Estimated: it is an extrapolation from this
// run's own history, never a quote. No price table lives here on purpose —
// pricing belongs to the agent backend, which is the only layer that knows
// which model ran, and a second table here would drift out of agreement
// with it (see internal/budget's Record).
//
// # Its blind spot
//
// A run that has spent nothing yet — replay mode, or a graph that reaches
// fanout before any agent call — projects zero, and zero is affordable
// against any ceiling. With no spend recorded there is nothing to
// extrapolate from, and inventing a number would be a price table by
// another name. Run logs a warning in that case rather than refusing,
// because a $0 run so far is a legitimate way to run belay, not evidence
// of an impending overrun.
func ProjectCandidateCost(spent belay.Usage) belay.Usage {
	return belay.Usage{
		// Negative components cannot come from a well-behaved ledger, but
		// they can come from a hand-edited manifest.json, and a negative
		// projection would make every fanout look affordable.
		InputTokens:  max(0, spent.InputTokens),
		OutputTokens: max(0, spent.OutputTokens),
		USD:          max(0, spent.USD),
		Estimated:    true,
	}
}

// affordable is the mandatory budget pre-check: it refuses count
// candidates whose projected cost would not fit under the run's ceiling,
// before a single workspace exists.
//
// It refuses regardless of budget.on_exceed. That is a deliberate
// departure from Ledger.Check, which honours OnExceedWarn and lets a run
// continue past its ceiling: "warn" means the user accepted watching a run
// drift over budget, not that they accepted multiplying its burn rate by N
// at the moment it is already there. Ledger.CanAfford — the committed
// contract this calls — takes the same position.
func affordable(rc *graph.RunContext, count int, spent belay.Usage) (belay.Usage, error) {
	projected := ProjectCandidateCost(spent)
	if projected.USD == 0 && projected.InputTokens == 0 && projected.OutputTokens == 0 {
		logger(rc).Warn("fanout: no spend recorded yet, so the per-candidate cost projection is zero and the budget pre-check cannot bound this fanout",
			"candidates", count,
			"max_usd", rc.Config.Budget.MaxUSD,
		)
	}

	snapshot := budget.Snapshot{
		LimitUSD:  rc.Config.Budget.MaxUSD,
		SpentUSD:  spent.USD,
		TokensIn:  spent.InputTokens,
		TokensOut: spent.OutputTokens,
		Estimated: spent.Estimated,
	}
	if err := budget.Restore(snapshot).CanAfford(count, projected, rc.Config.Budget); err != nil {
		return belay.Usage{}, fmt.Errorf("fanout: refusing to launch %d candidates projected at $%.4f each on top of $%.4f already spent: %w",
			count, projected.USD, spent.USD, err)
	}
	return projected, nil
}

// runMetadata reads the two things this node needs from the run's
// manifest: the repository candidates are copied from, and the run's spend
// so far.
//
// This is the node's only read of manifest.json, and it never writes it:
// ADR-0002 makes the dispatcher the sole writer of manifest.json,
// state.json and the journal, and that invariant is untouched here. The
// read exists because the budget pre-check ADR-0007 calls for needs the
// run's spend to date, and today manifest.json is the only place a node
// can see it — graph.RunContext carries Config (the ceiling) and State
// (the blackboard), neither of which records what has been spent. If the
// dispatcher ever carries a budget.Snapshot on RunContext, this function
// becomes one field access and the node stops touching the filesystem
// entirely.
func runMetadata(rc *graph.RunContext) (src string, spent belay.Usage, err error) {
	// Both facts arrive on the RunContext. This node used to read
	// manifest.json itself, which worked but made a node depend on a file
	// the dispatcher owns -- and ADR 0002 keeps that ownership in one
	// place precisely so a node cannot disagree with it.
	if rc.Workspace == "" {
		return "", belay.Usage{}, fmt.Errorf("%w: run context names no workspace to copy candidates from", ErrRunMetadata)
	}
	return rc.Workspace, belay.Usage{
		InputTokens:  rc.Budget.TokensIn,
		OutputTokens: rc.Budget.TokensOut,
		USD:          rc.Budget.SpentUSD,
		Estimated:    rc.Budget.Estimated,
	}, nil
}

// checkConfig validates everything Run can check without touching disk or
// spending anything, and returns the candidate count to create.
func checkConfig(rc *graph.RunContext) (int, error) {
	if !rc.Config.Fanout.Enabled {
		return 0, ErrDisabled
	}
	if n := rc.Config.Fanout.Candidates; n < minCandidates {
		return 0, fmt.Errorf("%w: fanout.candidates is %d, want at least %d", ErrTooFewCandidates, n, minCandidates)
	}
	if rc.Isolator == nil {
		return 0, ErrNoIsolator
	}
	return rc.Config.Fanout.Candidates, nil
}

// logger returns rc's logger, falling back to the default one. The
// dispatcher always sets Logger; a test building a RunContext by hand
// often does not, and a nil-pointer panic is never the right answer to a
// missing log destination.
func logger(rc *graph.RunContext) *slog.Logger {
	if rc.Logger != nil {
		return rc.Logger
	}
	return slog.Default()
}
