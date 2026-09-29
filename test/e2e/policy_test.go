//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/policy"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// guard pins rules on the run the harness built and rebuilds its registry
// with the policy gate, which is what the CLI does between resolving a run
// and creating it. The baseline is surveyed from the workspace as it stands
// now, exactly as createRun surveys it before the agent has touched anything.
func (h *loopHarness) guard(rules policy.Policy) {
	h.t.Helper()

	baseline, err := policy.Survey(context.Background(), h.workspace)
	if err != nil {
		h.t.Fatalf("policy.Survey(%s) error = %v", h.workspace, err)
	}
	man := h.manifest()
	man.SetPolicy(state.PolicyPin{Policy: rules, Baseline: baseline})
	if err := h.store.SaveManifest(man); err != nil {
		h.t.Fatalf("SaveManifest() error = %v", err)
	}
	registry, err := nodes.Default(h.cfg, nodes.WithPolicyGate())
	if err != nil {
		h.t.Fatalf("nodes.Default() error = %v", err)
	}
	h.registry = registry
}

// policyArtifact reads the report the gate archived at the run-relative path
// the blackboard recorded.
func (h *loopHarness) policyArtifact(rel string) belay.QualityReport {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.layout.RunDir(), rel)) //nolint:gosec // a path the run itself recorded.
	if err != nil {
		h.t.Fatalf("read %s: %v", rel, err)
	}
	var report belay.QualityReport
	if err := json.Unmarshal(raw, &report); err != nil {
		h.t.Fatalf("the archived report at %s is not a belay.QualityReport: %v", rel, err)
	}
	return report
}

// A change that touches a path the repository forbids ends the run at the
// gate, before the tests are ever run. This is the whole promise of the
// feature end to end: the agent really reported the file, the write node
// really verified it against the disk, and belay stopped anyway.
func TestPolicy_AForbiddenPathEndsTheRunBeforeTheTests(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = false

	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")
	h.seedFile("infra/main.tf", "resource \"aws_s3_bucket\" \"b\" {}\n")
	h.guard(policy.Policy{ForbiddenPaths: []string{"infra/**"}})

	agent := &belaytest.FakeAgent{Responses: []belay.AgentResponse{
		loopPlanResponse(),
		loopCodeResponse("sess-1", "src/app.go", "infra/main.tf"),
	}}
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{loopPassingTestReport(4)}}
	linter := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopPassingQualityReport("fake-linter")}}

	out, err := h.dispatcher(graph.Options{Agent: agent, Runner: runner, Linter: linter}).
		Run(context.Background())
	// The gate's own return is a Result and not an error -- a broken rule is
	// a verdict. The dispatcher is what turns a StatusFailed node into a
	// failed run, and it wraps ErrNodeFailed for every node alike.
	if !errors.Is(err, graph.ErrNodeFailed) {
		t.Fatalf("Run() error = %v, want one wrapping graph.ErrNodeFailed", err)
	}
	if out.Status != state.RunStatusFailed {
		t.Errorf("Outcome.Status = %v, want %v", out.Status, state.RunStatusFailed)
	}
	if out.Node != graph.NodePolicy {
		t.Errorf("Outcome.Node = %q, want %q", out.Node, graph.NodePolicy)
	}

	want := []string{
		"run_started",
		"plan#1 started", "plan#1 finished ok -> code",
		"code#1 started", "code#1 finished ok -> write",
		"write#1 started", "write#1 finished ok -> policy",
		"policy#1 started", "policy#1 finished failed",
		"run_failed",
	}
	if diff := cmp.Diff(want, h.timeline()); diff != "" {
		t.Errorf("journal timeline mismatch (-want +got):\n%s", diff)
	}
	// Nothing after the gate ran, so nothing after it was paid for.
	if got := runner.CallCount(); got != 0 {
		t.Errorf("the test runner was invoked %d times, want 0: the gate stops a run before it spends further", got)
	}
	if got := linter.CallCount(); got != 0 {
		t.Errorf("the linter was invoked %d times, want 0", got)
	}

	st := h.state()
	if st.Policy.Gate != belay.GateFail {
		t.Errorf("state.Policy.Gate = %v, want %v", st.Policy.Gate, belay.GateFail)
	}
	if len(st.Policy.Violations) != 1 {
		t.Fatalf("state.Policy.Violations = %+v, want exactly one", st.Policy.Violations)
	}
	v := st.Policy.Violations[0]
	if v.RuleID != string(policy.RuleForbiddenPath) {
		t.Errorf("violation rule = %q, want %q", v.RuleID, policy.RuleForbiddenPath)
	}
	if v.File != "infra/main.tf" {
		t.Errorf("violation file = %q, want %q", v.File, "infra/main.tf")
	}
	if v.Severity != belay.SeverityBlocker {
		t.Errorf("violation severity = %v, want %v: a run-ending violation must not ship",
			v.Severity, belay.SeverityBlocker)
	}
	if st.Policy.ReportPath == "" {
		t.Fatal("state.Policy.ReportPath is empty, want the archived report's path")
	}
	report := h.policyArtifact(st.Policy.ReportPath)
	if report.Gate != belay.GateFail || report.Issues == nil || len(report.Issues) != 1 {
		t.Errorf("the archived report = %+v, want one failing issue and a non-nil Issues", report)
	}
}

// A violation the agent can put right routes into the fix loop, and the
// repair goes back through write and through the gate again. The run
// completes, which is what proves the gate is re-entered on a later pass
// rather than judged once and forgotten.
func TestPolicy_ARepairableViolationGoesRoundTheFixLoopAndPasses(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = false

	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")
	h.seedFile("src/app_test.go", "package src\n")
	h.guard(policy.Policy{
		RequiredTests: []policy.TestRule{{When: "**/*.go", Require: "**/*_test.go"}},
	})

	agent := &belaytest.FakeAgent{Responses: []belay.AgentResponse{
		loopPlanResponse(),
		// The first attempt changes the source and no test, which the gate
		// refuses; the repair adds the test beside it.
		loopCodeResponse("sess-1", "src/app.go"),
		loopFixResponse("sess-1", 1, "src/app_test.go"),
	}}
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{loopPassingTestReport(4)}}
	linter := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopPassingQualityReport("fake-linter")}}

	out, err := h.dispatcher(graph.Options{Agent: agent, Runner: runner, Linter: linter}).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want %v (ended at %q)", out.Status, state.RunStatusCompleted, out.Node)
	}

	want := []string{
		"run_started",
		"plan#1 started", "plan#1 finished ok -> code",
		"code#1 started", "code#1 finished ok -> write",
		"write#1 started", "write#1 finished ok -> policy",
		"policy#1 started", "policy#1 finished ok -> fix",
		"fix#1 started", "fix#1 finished ok -> write",
		"write#1 started", "write#1 finished ok -> policy",
		"policy#1 started", "policy#1 finished ok -> test",
		"test#1 started", "test#1 finished ok -> review",
		"review#1 started", "review#1 finished ok -> END",
		"run_completed",
	}
	if diff := cmp.Diff(want, h.timeline()); diff != "" {
		t.Errorf("journal timeline mismatch (-want +got):\n%s", diff)
	}

	st := h.state()
	if st.Policy.Gate != belay.GatePass {
		t.Errorf("state.Policy.Gate = %v, want %v: the second pass found nothing", st.Policy.Gate, belay.GatePass)
	}
	if len(st.Policy.Violations) != 0 {
		t.Errorf("state.Policy.Violations = %+v, want none left", st.Policy.Violations)
	}

	// The repair was told what it broke, in the words the rule was written
	// in. fix is step 5: plan, code, write, policy, fix.
	promptPath := filepath.Join(h.nodeDir(5, graph.NodeFix), "prompt.txt")
	raw, err := os.ReadFile(promptPath) //nolint:gosec // a path the run itself wrote.
	if err != nil {
		t.Fatalf("read the repair prompt at %s: %v", promptPath, err)
	}
	for _, fragment := range []string{"Policy violations", "policy:required_tests", "**/*_test.go", "belay.yaml"} {
		if !strings.Contains(string(raw), fragment) {
			t.Errorf("the repair prompt never says %q; got:\n%s", fragment, raw)
		}
	}
}

// A run that never pinned a policy gets no gate, so guardrails added to
// belay.yaml after it started cannot judge it. This is the hazard the pin
// exists for, proven at the level that matters: the dispatcher walks the
// graph the manifest describes, not the one the file describes today.
func TestPolicy_ARunWithNoPinIsJudgedByNothing(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = false
	// A policy in the live configuration, and no pin in the manifest.
	cfg.Policy = config.Policy{ForbiddenPaths: []string{"infra/**"}}

	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")
	h.seedFile("infra/main.tf", "resource \"aws_s3_bucket\" \"b\" {}\n")

	agent := &belaytest.FakeAgent{Responses: []belay.AgentResponse{
		loopPlanResponse(),
		loopCodeResponse("sess-1", "src/app.go", "infra/main.tf"),
	}}
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{loopPassingTestReport(4)}}
	linter := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopPassingQualityReport("fake-linter")}}

	out, err := h.dispatcher(graph.Options{Agent: agent, Runner: runner, Linter: linter}).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want %v (ended at %q)", out.Status, state.RunStatusCompleted, out.Node)
	}
	for _, entry := range h.timeline() {
		if strings.HasPrefix(entry, graph.NodePolicy) {
			t.Fatalf("an unguarded run reached the gate: %v", h.timeline())
		}
	}
	if st := h.state(); st.Policy.Gate != belay.GateUnknown {
		t.Errorf("state.Policy.Gate = %v, want %v: nothing judged this run", st.Policy.Gate, belay.GateUnknown)
	}
}
