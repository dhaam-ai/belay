//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
	"github.com/belay-dev/belay/pkg/belay/belaytest"
)

// TestLoop_HappyPathCompletesRun proves scenario 1: a full happy path —
// plan -> code -> write -> test -> review -> END — completes a run with
// graph.Approval disabled, so no human gate blocks it.
func TestLoop_HappyPathCompletesRun(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = false

	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")

	agent := &belaytest.FakeAgent{
		Responses: []belay.AgentResponse{
			loopPlanResponse(),
			loopCodeResponse("sess-1", "src/app.go"),
		},
	}
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{loopPassingTestReport(4)}}
	linter := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopPassingQualityReport("fake-linter")}}

	d := h.dispatcher(graph.Options{Agent: agent, Runner: runner, Linter: linter})

	out, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Errorf("Outcome.Status = %v, want %v", out.Status, state.RunStatusCompleted)
	}
	if out.ExitCode != graph.ExitOK {
		t.Errorf("Outcome.ExitCode = %d, want %d", out.ExitCode, graph.ExitOK)
	}
	if out.Node != graph.End {
		t.Errorf("Outcome.Node = %q, want %q", out.Node, graph.End)
	}

	if got := h.manifest().Status; got != state.RunStatusCompleted {
		t.Errorf("manifest.Status = %v, want %v", got, state.RunStatusCompleted)
	}

	st := h.state()
	if st.Plan.Path == "" {
		t.Error("state.Plan.Path is empty, want the archived plan artifact's path")
	}
	if st.Test.ReportPath == "" {
		t.Error("state.Test.ReportPath is empty, want the archived test report's path")
	}
	if !st.Test.Report().OK() {
		t.Errorf("state.Test = %+v, want a recorded, passing test report", st.Test)
	}

	want := []string{
		"run_started",
		"plan#1 started", "plan#1 finished ok -> code",
		"code#1 started", "code#1 finished ok -> write",
		"write#1 started", "write#1 finished ok -> test",
		"test#1 started", "test#1 finished ok -> review",
		"review#1 started", "review#1 finished ok -> END",
		"run_completed",
	}
	if diff := cmp.Diff(want, h.timeline()); diff != "" {
		t.Errorf("journal timeline mismatch (-want +got):\n%s", diff)
	}

	if got := agent.CallCount(); got != 2 {
		t.Errorf("agent invoked %d times, want 2 (plan, code)", got)
	}
	if got := runner.CallCount(); got != 1 {
		t.Errorf("runner invoked %d times, want 1", got)
	}
	if got := linter.CallCount(); got != 1 {
		t.Errorf("linter invoked %d times, want 1", got)
	}
}

// TestLoop_FixLoopConverges proves scenario 2: a test runner that fails
// twice and then passes drives the graph through
// test -> fix -> test -> fix -> test -> review -> END, with fix running
// exactly twice and State.Fix.Attempts reset to 0 by the time the run
// completes — the reset the test node performs on a green run, verified
// here end to end rather than only in internal/nodes/test's own unit test.
func TestLoop_FixLoopConverges(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = false
	cfg.Graph.GiveUp = 3 // ample headroom; this run converges in exactly 2.

	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")

	agent := &belaytest.FakeAgent{
		Responses: []belay.AgentResponse{
			loopPlanResponse(),
			loopCodeResponse("sess-1", "src/app.go"),
			loopFixResponse("sess-1", 1),
			loopFixResponse("sess-1", 2),
		},
	}
	runner := &belaytest.FakeRunner{
		Responses: []belay.TestReport{
			loopFailingTestReport(4, 1, "TestAdd", "Add(2, 2) = 3, want 4"),
			loopFailingTestReport(4, 1, "TestAdd", "Add(2, 2) = 3, want 4"),
			loopPassingTestReport(4),
		},
	}
	linter := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopPassingQualityReport("fake-linter")}}

	d := h.dispatcher(graph.Options{Agent: agent, Runner: runner, Linter: linter})
	out, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want %v", out.Status, state.RunStatusCompleted)
	}

	// Attempt is the dispatcher's crash-retry counter, not a loop-iteration
	// counter (see loopHarness.timeline): every fresh visit to test or fix
	// in this ordinary graph loop is attempt 1, so "test#1" and "fix#1" each
	// appear three and two times respectively, not numbered up.
	want := []string{
		"run_started",
		"plan#1 started", "plan#1 finished ok -> code",
		"code#1 started", "code#1 finished ok -> write",
		"write#1 started", "write#1 finished ok -> test",
		"test#1 started", "test#1 finished ok -> fix",
		"fix#1 started", "fix#1 finished ok -> test",
		"test#1 started", "test#1 finished ok -> fix",
		"fix#1 started", "fix#1 finished ok -> test",
		"test#1 started", "test#1 finished ok -> review",
		"review#1 started", "review#1 finished ok -> END",
		"run_completed",
	}
	timeline := h.timeline()
	if diff := cmp.Diff(want, timeline); diff != "" {
		t.Errorf("journal timeline mismatch (-want +got):\n%s", diff)
	}

	fixVisits := loopCountPrefix(timeline, "fix#1 started")
	if fixVisits != 2 {
		t.Errorf("fix ran %d times, want exactly 2", fixVisits)
	}
	if got := runner.CallCount(); got != 3 {
		t.Errorf("runner invoked %d times, want 3 (fail, fail, pass)", got)
	}

	fix := h.state().Fix
	// The counter is NOT reset on the green run. give_up is a per-run repair
	// budget: resetting on green would make a loop whose tests pass while the
	// quality gate keeps failing run until max_steps instead of stopping at
	// give_up. See TestLoop_GiveUpBoundsAReviewOnlyLoop.
	if fix.Attempts != 2 {
		t.Errorf("state.Fix.Attempts = %d, want 2: two repairs happened and the budget counts them for the whole run", fix.Attempts)
	}
	if fix.GiveUp {
		t.Error("state.Fix.GiveUp = true, want false after converging")
	}
}

// TestLoop_GiveUpTerminatesRun proves scenario 3: a test runner that never
// passes exhausts graph.give_up and fails the run after exactly give_up fix
// attempts — not one more (which would mean the loop never terminates) and
// not one fewer (which would mean the run gives up early, wasting a fix
// attempt it was owed).
func TestLoop_GiveUpTerminatesRun(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = false
	cfg.Graph.GiveUp = 2

	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")

	agent := &belaytest.FakeAgent{
		Responses: []belay.AgentResponse{
			loopPlanResponse(),
			loopCodeResponse("sess-1", "src/app.go"),
			loopFixResponse("sess-1", 1),
			loopFixResponse("sess-1", 2),
		},
	}
	// The runner never passes: give_up must trigger on the attempt count
	// alone, never on the loop somehow making progress.
	runner := &belaytest.FakeRunner{
		Responses: []belay.TestReport{loopFailingTestReport(4, 4, "TestAdd", "Add(2, 2) = 3, want 4")},
	}
	linter := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopPassingQualityReport("fake-linter")}}

	d := h.dispatcher(graph.Options{Agent: agent, Runner: runner, Linter: linter})
	out, err := d.Run(context.Background())

	if !errors.Is(err, graph.ErrNodeFailed) {
		t.Fatalf("Run() error = %v, want one wrapping graph.ErrNodeFailed", err)
	}
	if out.Status != state.RunStatusFailed {
		t.Errorf("Outcome.Status = %v, want %v", out.Status, state.RunStatusFailed)
	}
	if out.ExitCode != graph.ExitFailed {
		t.Errorf("Outcome.ExitCode = %d, want %d", out.ExitCode, graph.ExitFailed)
	}
	if !strings.Contains(out.Note, "give_up_exhausted") {
		t.Errorf("Outcome.Note = %q, want it to name give_up_exhausted", out.Note)
	}

	// fix is visited give_up+1 times: give_up of them call the agent and
	// route back to test, and the final one is the exhausting visit that
	// fails the run without spending anything.
	fixVisits := loopCountPrefix(h.timeline(), "fix#1 started")
	if want := cfg.Graph.GiveUp + 1; fixVisits != want {
		t.Errorf("fix visited %d times, want %d (give_up successful attempts, plus the exhausting visit)", fixVisits, want)
	}
	if got, want := agent.CallCount(), 2+cfg.Graph.GiveUp; got != want {
		t.Errorf("agent invoked %d times, want %d (plan, code, and exactly give_up fix attempts)", got, want)
	}

	fix := h.state().Fix
	if fix.Attempts != cfg.Graph.GiveUp {
		t.Errorf("state.Fix.Attempts = %d, want %d (exactly give_up)", fix.Attempts, cfg.Graph.GiveUp)
	}
	if !fix.GiveUp {
		t.Error("state.Fix.GiveUp = false, want true once the retry budget is exhausted")
	}
}

// TestLoop_FailingQualityGateRoutesToFix proves scenario 4: a linter that
// reports a blocker issue under fail_on: major routes to fix exactly like a
// failing test — a successful, informative Result, not a Go error — and the
// run still completes once the gate clears.
func TestLoop_FailingQualityGateRoutesToFix(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = false
	cfg.Review.FailOn = config.SeverityMajor

	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")

	agent := &belaytest.FakeAgent{
		Responses: []belay.AgentResponse{
			loopPlanResponse(),
			loopCodeResponse("sess-1", "src/app.go"),
			loopFixResponse("sess-1", 1),
		},
	}
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{loopPassingTestReport(4)}}
	linter := &belaytest.FakeLinter{
		Responses: []belay.QualityReport{
			loopFailingQualityReport("fake-linter"),
			loopPassingQualityReport("fake-linter"),
		},
	}

	d := h.dispatcher(graph.Options{Agent: agent, Runner: runner, Linter: linter})
	out, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil: a failing quality gate is a route, not an error", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want %v", out.Status, state.RunStatusCompleted)
	}

	want := []string{
		"run_started",
		"plan#1 started", "plan#1 finished ok -> code",
		"code#1 started", "code#1 finished ok -> write",
		"write#1 started", "write#1 finished ok -> test",
		"test#1 started", "test#1 finished ok -> review",
		"review#1 started", "review#1 finished ok -> fix",
		"fix#1 started", "fix#1 finished ok -> test",
		"test#1 started", "test#1 finished ok -> review",
		"review#1 started", "review#1 finished ok -> END",
		"run_completed",
	}
	if diff := cmp.Diff(want, h.timeline()); diff != "" {
		t.Errorf("journal timeline mismatch (-want +got):\n%s", diff)
	}
	if got := linter.CallCount(); got != 2 {
		t.Errorf("linter invoked %d times, want 2", got)
	}
}

// TestLoop_MissingToolchainIsNotAFixRoute proves scenario 5: a test runner
// that cannot even run — reporting an error satisfying
// belay.ErrToolchainMissing — fails the run without ever routing to fix.
// Collapsing this into an ordinary test failure would send a coding agent
// to repair a missing binary, which is exactly the mistake this distinction
// exists to prevent.
func TestLoop_MissingToolchainIsNotAFixRoute(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = false

	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")

	agent := &belaytest.FakeAgent{
		Responses: []belay.AgentResponse{
			loopPlanResponse(),
			loopCodeResponse("sess-1", "src/app.go"),
		},
	}
	runner := &belaytest.FakeRunner{
		NameValue: "go-test",
		Errs:      []error{&belay.ToolchainError{Tool: "go"}},
	}
	linter := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopPassingQualityReport("fake-linter")}}

	d := h.dispatcher(graph.Options{Agent: agent, Runner: runner, Linter: linter})
	out, err := d.Run(context.Background())

	if !errors.Is(err, belay.ErrToolchainMissing) {
		t.Fatalf("Run() error = %v, want one wrapping belay.ErrToolchainMissing", err)
	}
	if out.Status != state.RunStatusFailed {
		t.Errorf("Outcome.Status = %v, want %v", out.Status, state.RunStatusFailed)
	}
	if out.ExitCode != graph.ExitFailed {
		t.Errorf("Outcome.ExitCode = %d, want %d", out.ExitCode, graph.ExitFailed)
	}

	if got := agent.CallCount(); got != 2 {
		t.Errorf("agent invoked %d times, want exactly 2 (plan, code); fix must never run for a missing toolchain", got)
	}
	for _, r := range h.timeline() {
		if strings.HasPrefix(r, "fix#") {
			t.Errorf("journal shows a fix node execution (%q); a missing toolchain must never route to fix", r)
		}
	}
}

// TestLoop_ApprovalGatePausesAndResumes proves scenario 6: with
// graph.Approval enabled, the first Run pauses at the approve gate before
// reaching code — a clean, successful invocation (exit 0, nil error), not a
// failure — a second plain Run explicitly refuses to advance it (exit
// graph.ExitPaused, wrapping graph.ErrRunPaused), and Resume carries it to
// completion once the plan is approved.
func TestLoop_ApprovalGatePausesAndResumes(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = true

	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")

	agent := &belaytest.FakeAgent{
		Responses: []belay.AgentResponse{
			loopPlanResponse(),
			loopCodeResponse("sess-1", "src/app.go"),
		},
	}
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{loopPassingTestReport(4)}}
	linter := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopPassingQualityReport("fake-linter")}}

	d := h.dispatcher(graph.Options{Agent: agent, Runner: runner, Linter: linter})

	// 1. Plan runs, approve pauses. Pausing is a normal, successful outcome
	// of an invocation (see graph.ExitOK's own doc comment) — the run has
	// not failed, it is waiting.
	out, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() [pause] error = %v, want nil", err)
	}
	if out.Status != state.RunStatusPaused || out.ExitCode != graph.ExitOK {
		t.Fatalf("Outcome = {%v, exit %d}, want {%v, exit %d}",
			out.Status, out.ExitCode, state.RunStatusPaused, graph.ExitOK)
	}
	if got := agent.CallCount(); got != 1 {
		t.Fatalf("agent invoked %d times, want 1 (plan only); code must not run before approval", got)
	}

	// 2. A plain Run refuses to step past the pause, and runs nothing.
	out, err = d.Run(context.Background())
	if !errors.Is(err, graph.ErrRunPaused) {
		t.Fatalf("Run() [refused] error = %v, want one wrapping graph.ErrRunPaused", err)
	}
	if out.ExitCode != graph.ExitPaused {
		t.Errorf("Outcome.ExitCode = %d, want %d", out.ExitCode, graph.ExitPaused)
	}
	if got := agent.CallCount(); got != 1 {
		t.Errorf("agent invoked %d times after a refused Run, want still 1", got)
	}

	// 3. Approve the plan exactly as `belay resume` would find it after a
	// human acted, then resume.
	h.approvePlan()
	out, err = d.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted || out.ExitCode != graph.ExitOK {
		t.Fatalf("Outcome = {%v, exit %d}, want {%v, exit %d}",
			out.Status, out.ExitCode, state.RunStatusCompleted, graph.ExitOK)
	}
	if got := agent.CallCount(); got != 2 {
		t.Errorf("agent invoked %d times, want 2 (plan, code)", got)
	}
}

// TestLoop_BudgetCeilingStopsRun proves scenario 7: usage that pushes
// cumulative spend over budget.max_usd aborts the run with
// belay.ErrBudgetExceeded before the next node runs, not after it — the
// ceiling is checked before every node dispatch, precisely so a run that
// has already crossed it never pays for one more.
func TestLoop_BudgetCeilingStopsRun(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = false
	cfg.Budget.MaxUSD = 1.00
	cfg.Budget.OnExceed = config.OnExceedAbort

	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")

	agent := &belaytest.FakeAgent{
		Responses: []belay.AgentResponse{{
			Text:  loopPlanResponse().Text,
			Usage: belay.Usage{InputTokens: 100_000, OutputTokens: 20_000, USD: 5.00},
		}},
	}
	runner := &belaytest.FakeRunner{}
	linter := &belaytest.FakeLinter{}

	d := h.dispatcher(graph.Options{Agent: agent, Runner: runner, Linter: linter})
	out, err := d.Run(context.Background())

	if !errors.Is(err, belay.ErrBudgetExceeded) {
		t.Fatalf("Run() error = %v, want one wrapping belay.ErrBudgetExceeded", err)
	}
	if out.Status != state.RunStatusAborted {
		t.Errorf("Outcome.Status = %v, want %v", out.Status, state.RunStatusAborted)
	}
	if out.ExitCode != graph.ExitAborted {
		t.Errorf("Outcome.ExitCode = %d, want %d", out.ExitCode, graph.ExitAborted)
	}
	if out.Node != graph.NodeCode {
		t.Errorf("Outcome.Node = %q, want %q: the run must report the node it never ran", out.Node, graph.NodeCode)
	}
	if !strings.Contains(out.Note, "budget") {
		t.Errorf("Outcome.Note = %q, want it to mention the budget ceiling", out.Note)
	}

	if got := agent.CallCount(); got != 1 {
		t.Fatalf("agent invoked %d times, want exactly 1 (plan); the budget guard must stop the run before code spends anything else", got)
	}
	if got := runner.CallCount(); got != 0 {
		t.Errorf("runner invoked %d times, want 0: the run never reached the test node", got)
	}

	// No "code#1 started" line: the abort happens before the next node is
	// even dispatched, not after it runs and is then judged too expensive.
	want := []string{
		"run_started",
		"plan#1 started", "plan#1 finished ok -> code",
		"run_aborted",
	}
	if diff := cmp.Diff(want, h.timeline()); diff != "" {
		t.Errorf("journal timeline mismatch (-want +got):\n%s", diff)
	}
}

// TestLoop_ArtifactsAndEvidenceExist proves scenario 8: after a completed
// run, artifacts/ holds the plan and a test report, and nodes/<seq>-<name>/
// holds prompt and response evidence for every agent-calling node.
func TestLoop_ArtifactsAndEvidenceExist(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = false

	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")

	agent := &belaytest.FakeAgent{
		Responses: []belay.AgentResponse{
			loopPlanResponse(),
			loopCodeResponse("sess-1", "src/app.go"),
		},
	}
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{loopPassingTestReport(4)}}
	linter := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopPassingQualityReport("fake-linter")}}

	d := h.dispatcher(graph.Options{Agent: agent, Runner: runner, Linter: linter})
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if info, err := os.Stat(h.artifactPath("plan.md")); err != nil {
		t.Errorf("artifacts/plan.md: %v", err)
	} else if info.Size() == 0 {
		t.Error("artifacts/plan.md is empty")
	}

	reports, err := filepath.Glob(filepath.Join(h.layout.ArtifactsDir(), "test-*.json"))
	if err != nil {
		t.Fatalf("Glob(test-*.json) error = %v", err)
	}
	if len(reports) == 0 {
		t.Error("artifacts/ has no test-*.json report")
	}

	reviews, err := filepath.Glob(filepath.Join(h.layout.ArtifactsDir(), "review-*.json"))
	if err != nil {
		t.Fatalf("Glob(review-*.json) error = %v", err)
	}
	if len(reviews) == 0 {
		t.Error("artifacts/ has no review-*.json report")
	}

	for _, evidence := range []struct {
		seq  int
		node string
	}{
		{1, graph.NodePlan},
		{2, graph.NodeCode},
	} {
		dir := h.nodeDir(evidence.seq, evidence.node)
		for _, name := range []string{"prompt.txt", "response.json"} {
			path := filepath.Join(dir, name)
			info, err := os.Stat(path)
			if err != nil {
				t.Errorf("%s: %v", path, err)
				continue
			}
			if info.Size() == 0 {
				t.Errorf("%s is empty", path)
			}
		}
	}
}

// loopCountPrefix counts how many entries in lines start with prefix.
func loopCountPrefix(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}
