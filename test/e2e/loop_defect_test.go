//go:build e2e

package e2e

import (
	"context"
	"errors"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// TestLoop_GiveUpBoundsAReviewOnlyLoop documents a real defect found
// while writing this suite: graph.give_up does not bound a fix loop whose
// only recurring cause is a failing quality gate, when the test suite
// itself keeps passing.
//
// # The mechanism
//
// internal/nodes/test.Run resets State.Fix unconditionally whenever the
// test suite it just ran is green (internal/nodes/test/node.go:168,
// `res.Patch.Fix = &state.Fix{Attempts: 0, GiveUp: false}`), with no check
// on why the run arrived at the test node — a first pass, or a fix attempt
// that was actually repairing a failing review gate, look identical to it.
// internal/nodes/review.verdict never sets Patch.Fix at all (review.go has
// no `patch.Fix = ...` anywhere), even on belay.GatePass, even though the
// fix package's own doc comment (internal/nodes/fix/fix.go, "Attempts is an
// absolute count, and this node never resets it") describes the reset as
// belonging jointly to "the test node on a passing suite, the review node
// on a passing gate" — a contract only half implemented.
//
// The graph always runs test before review (write -> test -> review), and
// fix always routes back to test, never straight to review. So in a loop
// where tests pass but review keeps failing:
//
//	test (passes, resets Fix.Attempts=0) -> review (fails) -> fix (sees
//	Attempts=0, proceeds, sets Attempts=1) -> test (passes, resets
//	Fix.Attempts=0 again) -> review (fails again) -> fix (sees Attempts=0
//	again, not 2) -> ...
//
// fix.Run's give-up check (`if attempts >= giveUp`) never observes a count
// higher than 1, because test's reset fires on every iteration before fix
// gets a chance to accumulate past it. The loop is bounded only by
// config.Graph.MaxSteps — a much coarser, differently-purposed ceiling — and
// the run ends with graph.ErrMaxSteps ("the graph is not converging")
// instead of the give_up_exhausted verdict a user configuring
// graph.give_up: N would reasonably expect after N fix attempts. In this
// suite's own numbers that is 30 node executions and roughly 15 wasted
// agent invocations before the run stops, for a give_up of 2.
//
// # Why this belongs in an e2e suite, not a unit test
//
// internal/nodes/test's own unit tests correctly show its reset firing in
// isolation, and internal/nodes/fix's own unit tests correctly show
// give_up's boundary in isolation. Neither can see this: the defect is
// only observable across three nodes and several real loop iterations,
// which is exactly the gap an end-to-end suite exists to close.
//
// FIXED. internal/nodes/test no longer resets State.Fix on a green suite.
// give_up is a per-run repair budget — how many repairs belay may attempt
// before handing the problem back, counted across the whole run — because any
// reset on green makes this loop unbounded, and a bounded budget that
// occasionally stops one repair early is strictly better than one that never
// stops at all. This test is now the regression guard for that.
func TestLoop_GiveUpBoundsAReviewOnlyLoop(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = false
	cfg.Graph.GiveUp = 2
	cfg.Graph.MaxSteps = 30 // generous, so a non-converging run is caught by give_up first if the defect is fixed

	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")

	agent := &belaytest.FakeAgent{Func: func(_ context.Context, _ belay.AgentRequest) (belay.AgentResponse, error) {
		return loopCodeResponse("sess-1", "src/app.go"), nil
	}}
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{loopPassingTestReport(4)}}
	// The gate never clears: give_up must be what stops this run, not
	// MaxSteps.
	linter := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopFailingQualityReport("fake-linter")}}

	d := h.dispatcher(graph.Options{Agent: agent, Runner: runner, Linter: linter})
	out, err := d.Run(context.Background())

	if errors.Is(err, graph.ErrMaxSteps) {
		t.Fatalf("Run() error = %v: the run hit graph.max_steps instead of graph.give_up, "+
			"confirming the defect this test documents", err)
	}
	if !errors.Is(err, graph.ErrNodeFailed) {
		t.Fatalf("Run() error = %v, want one wrapping graph.ErrNodeFailed (give_up_exhausted)", err)
	}
	if out.Status != state.RunStatusFailed {
		t.Errorf("Outcome.Status = %v, want failed", out.Status)
	}

	fixVisits := loopCountPrefix(h.timeline(), "fix#1 started")
	if want := cfg.Graph.GiveUp + 1; fixVisits != want {
		t.Errorf("fix visited %d times, want %d (give_up attempts, plus the exhausting visit)", fixVisits, want)
	}
}
