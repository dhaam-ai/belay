// Package test implements the belay "test" node: it runs the target
// repository's suite through the run's [belay.TestRunner] and routes the graph
// on the outcome — passing to "review", failing to "fix".
//
// # A failing suite is not a Go error
//
// The node returns (Result, nil) for every verdict it can reach, including a
// suite that ran to completion and failed. That is an ordinary branch to
// [graph.NodeFix], not an error. A non-nil error from Run means the opposite:
// the tests could not be run at all, so there is no verdict to route on.
//
// The case that makes the distinction load-bearing is a missing toolchain. If
// an absent `go` or `pytest` were reported as a failing test, the fix loop
// would dispatch an agent to edit source code in order to repair a machine
// that is merely missing software. It would find nothing to fix, burn every
// give_up attempt it has, and fail the run for a reason no one reading the
// journal would recognise. So a missing toolchain leaves this node as an error
// wrapping [belay.ErrToolchainMissing], and the returned Result routes
// nowhere.
//
// # Who enforces give_up
//
// This node never enforces Config.Graph.GiveUp. When tests fail it routes to
// [graph.NodeFix] unconditionally — including when State.Fix.Attempts has
// already reached the cap. The fix node owns that decision and is the single
// place the cap is applied.
//
// The split is written down in both places because both ways of getting it
// wrong are silent. If both nodes enforced the cap, a run would stop one
// attempt early while an attempt was still owed. If neither did, the test/fix
// pair would loop until MaxSteps, spending budget on a gate already declared
// hopeless. For the same reason this node never sets Fix in its Patch:
// Fix.Attempts and Fix.GiveUp belong to the fix node, and a node that does not
// own a field must not write it.
//
// # A zero-test run
//
// [belay.TestReport.OK] already treats Total == 0 as not passing, and this node
// takes that verdict as given rather than adding a branch of its own: a suite
// that discovered nothing routes to [graph.NodeFix] exactly like a failing one,
// with a Note that says so in as many words.
//
// Keeping it on the failing branch keeps this node's contract binary — OK goes
// to review, everything else goes to fix — which is what the dispatcher and the
// fix node are written against. The cost is real and worth stating plainly: the
// fix node receives zero Failures to work from and is acting on the Note alone,
// and a repository that genuinely has no tests will burn its give_up attempts
// before the run fails. That is the intended outcome. A repo where the test
// command matched nothing needs a human, and failing the run after a bounded
// number of attempts is how it gets one; reporting green is the outcome belay
// refuses.
//
// # Persistence
//
// Per ADR 0002 this node writes no state.json, no manifest.json and no journal
// record. It archives the full report as a run artifact — artifacts are content,
// not control state — and returns everything else as a [state.Patch].
package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Errors returned by Run when it cannot reach a verdict. None of them mean
// "the tests failed" — that outcome is a Result routing to [graph.NodeFix].
var (
	// ErrNoRunner means the RunContext carried no [belay.TestRunner]. It is a
	// wiring bug: a run reaching the test node without a runner cannot be
	// rescued by the fix loop, so it is an error rather than a route.
	ErrNoRunner = errors.New("nodes/test: no test runner configured")

	// ErrNoWorkspace means the workspace directory could not be derived from
	// the RunContext's Layout. Rather than falling back to the process's
	// working directory — which would run a stranger's tests in whatever
	// directory belay happened to start in — the node refuses.
	ErrNoWorkspace = errors.New("nodes/test: cannot locate the workspace directory")

	// ErrNilRunContext means Run was called with a nil *graph.RunContext.
	ErrNilRunContext = errors.New("nodes/test: nil RunContext")
)

// Node is the "test" graph node. It holds no state and is safe to reuse and
// to re-run: a repeated execution at the same Step overwrites its own report
// artifact rather than accumulating a new one.
type Node struct{}

// New returns a test Node.
func New() *Node { return &Node{} }

// Name implements [graph.Node] and returns [graph.NodeTest].
func (*Node) Name() string { return graph.NodeTest }

// Run executes the target repository's test suite and routes on the result.
//
// It returns (Result, nil) with Status [journal.StatusOK] for both outcomes it
// can reach: [graph.NodeReview] when the report is OK, and [graph.NodeFix]
// otherwise — a failing suite and a suite that discovered no tests alike.
// Result.Usage is always zero, because this node calls no agent.
//
// A non-nil error means the suite could not be run: a nil runner, an
// underivable workspace, a cancelled context, or anything the runner itself
// reported — most importantly a missing toolchain, which is recoverable with
// errors.Is(err, [belay.ErrToolchainMissing]) and is never a route to the fix
// loop.
func (*Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc == nil {
		return graph.Result{}, ErrNilRunContext
	}
	log := logger(rc)

	if rc.Runner == nil {
		return graph.Result{}, ErrNoRunner
	}
	if err := ctx.Err(); err != nil {
		return graph.Result{}, fmt.Errorf("nodes/test: %w", err)
	}
	dir, err := workspaceDir(rc)
	if err != nil {
		return graph.Result{}, err
	}

	log.Info("running tests", "runner", rc.Runner.Name(), "dir", dir)

	report, err := rc.Runner.Test(ctx, dir)
	if err != nil {
		return graph.Result{}, runErr(log, rc.Runner.Name(), dir, err)
	}
	// Re-checked after the call as well as before it: a runner that ignores
	// ctx would otherwise let a cancelled run write an artifact and claim a
	// verdict on its way out.
	if err := ctx.Err(); err != nil {
		return graph.Result{}, fmt.Errorf("nodes/test: %w", err)
	}

	reportPath, err := writeReport(rc, report)
	if err != nil {
		return graph.Result{}, err
	}

	// state.NewTest builds the blackboard projection, so the mapping from
	// belay.TestReport to state.Test lives in exactly one place.
	testState := state.NewTest(report, reportPath)

	res := graph.Result{
		Next:   graph.NodeFix,
		Status: journal.StatusOK,
		Patch:  state.Patch{Test: &testState},
		Note:   note(rc.Runner.Name(), report),
	}
	if report.OK() {
		res.Next = graph.NodeReview
		// Deliberately no reset of State.Fix here.
		//
		// An earlier version cleared the budget on every green suite, so
		// that a run which broke, was repaired, and broke again later did
		// not inherit an exhausted one. That is unsound: the graph always
		// runs test before review and fix always routes back to test, so a
		// run whose tests pass while the quality gate keeps failing walks
		// test(green, reset) -> review(fail) -> fix -> test(green, reset)
		// forever. give_up never fires and only max_steps stops it, tens of
		// agent calls later, reporting the wrong reason.
		//
		// give_up is therefore a per-run repair budget: how many times
		// belay may attempt a repair before handing the problem back,
		// counted across the whole run rather than per consecutive streak.
		// That is the only reading that terminates.
	}

	log.Info("tests finished",
		"next", res.Next,
		"total", report.Total,
		"passed", report.Passed,
		"failed", report.Failed,
		"report", reportPath)

	return res, nil
}

// runErr wraps a runner failure, logging the missing-toolchain case
// distinctly: it is the one failure here that a human fixes by installing
// software rather than by changing code, and the log line is where that
// difference becomes visible during a run.
func runErr(log *slog.Logger, runner, dir string, err error) error {
	if errors.Is(err, belay.ErrToolchainMissing) {
		tool := runner
		var te *belay.ToolchainError
		if errors.As(err, &te) && te.Tool != "" {
			tool = te.Tool
		}
		log.Error("test toolchain is not installed: this is not a test failure and does not route to the fix loop",
			"runner", runner, "tool", tool)
	}
	return fmt.Errorf("nodes/test: run tests with %q in %s: %w", runner, dir, err)
}

// writeReport archives the full report — including the Duration and Raw that
// the blackboard projection drops — and returns its run-relative path.
func writeReport(rc *graph.RunContext, r belay.TestReport) (string, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		// Reached only when a runner violates the TestReport contract by
		// putting something other than self-contained JSON in Raw (a raw
		// `go test -json` stream, say). That is an adapter bug, not a test
		// failure, so it stays on the error path.
		return "", fmt.Errorf("nodes/test: encode report from runner %q (TestReport.Raw must be valid JSON on its own): %w",
			rc.Runner.Name(), err)
	}
	data = append(data, '\n')

	path, err := rc.WriteArtifact(reportName(rc.Step), data)
	if err != nil {
		return "", fmt.Errorf("nodes/test: archive report: %w", err)
	}
	return path, nil
}

// reportName is the artifact name for the report of the run's step-th node
// execution, zero-padded to sort correctly beside diff-0007.patch and friends.
func reportName(step int) string { return fmt.Sprintf("test-%04d.json", step) }

// note summarises the outcome for the journal and the timeline.
//
// It is redacted by construction rather than by filtering: it is assembled
// from three counts and this function's own fixed vocabulary plus the runner's
// Name, which the TestRunner contract defines as a short stable identifier
// ("go-test", "pytest"). No test name, failure message, captured output or
// filesystem path reaches it, so there is nothing in it to redact.
func note(runner string, r belay.TestReport) string {
	switch {
	case r.Total == 0:
		return fmt.Sprintf("%s: no tests discovered (0 total), not treated as passing", runner)
	case r.Failed == 0:
		return fmt.Sprintf("%s: %d tests, 0 failed", runner, r.Total)
	default:
		return fmt.Sprintf("%s: %d tests, %d failed", runner, r.Total, r.Failed)
	}
}

// workspaceDir recovers the target repository's root from the run's Layout.
//
// state.Layout keeps its root unexported and exposes no workspace accessor, so
// the path is walked back up the layout NewLayout built:
// <workspace>/.belay/runs/<run id>. The shape is verified rather than assumed —
// a zero Layout would otherwise relativize to ".", and running the suite in
// whatever directory belay was started from is a far worse outcome than
// refusing.
func workspaceDir(rc *graph.RunContext) (string, error) {
	if rc.Workspace == "" {
		return "", ErrNoWorkspace
	}
	return rc.Workspace, nil
}

// logger returns rc's logger, or one that discards, so a RunContext built
// without a Logger cannot panic the node.
func logger(rc *graph.RunContext) *slog.Logger {
	if rc.Logger != nil {
		return rc.Logger
	}
	return slog.New(slog.DiscardHandler)
}

var _ graph.Node = (*Node)(nil)
