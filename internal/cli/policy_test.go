package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/policy"
	"github.com/dhaam-ai/belay/internal/state"
)

// guardedConfig is a belay.yaml declaring one rule of each class.
const guardedConfig = `version: 1
output:
  mode: in_place
policy:
  forbidden_paths:
    - "infra/**"
  max_changed_files: 25
  no_new_dependencies: true
  required_tests:
    - when: "**/*.go"
      require: "**/*_test.go"
  forbidden_content:
    - pattern: "AKIA[0-9A-Z]{16}"
      reason: "an AWS access key id"
`

// A dry run is where a person checks that belay is about to do what they
// meant. Guardrails are the setting they would most want confirmed and the
// one they cannot confirm any other way -- a policy that loads but never
// fires looks exactly like one that works until something gets through.
func TestDryRunSaysWhatTheGuardrailsAre(t *testing.T) {
	t.Setenv(anthropicKeyEnv, "sk-ant-test")
	ws := goWorkspace(t)
	writeConfig(t, ws, guardedConfig)

	out, _, err := runCLI(t, "run", "--dry-run", "--workspace", ws, "add tests")
	if err != nil {
		t.Fatalf("belay run --dry-run: %v\n%s", err, out)
	}

	for _, want := range []string{
		// Every rule, in the words the person wrote them in.
		"infra/**",
		"25",
		"**/*.go",
		"**/*_test.go",
		"AKIA[0-9A-Z]{16}",
		// And which of them would end the run rather than ask for a repair,
		// because that is the difference a person most needs to know before
		// they leave belay running unattended.
		"ends the run",
		// The gate is a step of the route, so the route must show it.
		graph.NodePolicy,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run never says %q; got:\n%s", want, out)
		}
	}
}

// A repository that declares no policy must read exactly as it did before
// guardrails existed: no section, and no gate on the route.
func TestDryRunSaysNothingAboutGuardrailsWhenThereAreNone(t *testing.T) {
	t.Setenv(anthropicKeyEnv, "sk-ant-test")
	ws := goWorkspace(t)

	out, _, err := runCLI(t, "run", "--dry-run", "--workspace", ws, "add tests")
	if err != nil {
		t.Fatalf("belay run --dry-run: %v\n%s", err, out)
	}
	if strings.Contains(out, "guardrails") {
		t.Errorf("the dry run talks about guardrails for a run that has none:\n%s", out)
	}
	if strings.Contains(out, graph.NodePolicy) {
		t.Errorf("the dry run shows a policy gate on the route of an unguarded run:\n%s", out)
	}
}

// The rules and the baseline are pinned when the run is created, which is
// the whole safety property: a policy added after a run began could only
// fail that run for something it did not do.
func TestCreateRunPinsThePolicyAndItsBaseline(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, guardedConfig)
	if err := os.WriteFile(filepath.Join(ws, "go.mod"),
		[]byte("module example.com/scratch\n\ngo 1.26.3\n\nrequire github.com/spf13/cobra v1.8.0\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	plan, err := resolve(resolveRequest{workspace: ws})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const runID = "20260101T000000Z-abcdef123456"
	store, err := createRun(context.Background(), plan, runSpec{
		Workspace: ws, RunID: runID, Goal: "add tests",
	})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}
	manifest, err := store.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest.Policy == nil {
		t.Fatal("createRun pinned no policy for a run whose belay.yaml declares one")
	}
	if got := manifest.Policy.Policy.ForbiddenPaths; len(got) != 1 || got[0] != "infra/**" {
		t.Errorf("pinned forbidden_paths = %v, want [infra/**]", got)
	}
	if !manifest.Policy.Policy.NoNewDependencies {
		t.Error("pinned no_new_dependencies = false, want true")
	}
	want := policy.Dependency{Manifest: "go.mod", Name: "github.com/spf13/cobra"}
	if got := manifest.Policy.Baseline.Dependencies; len(got) != 1 || got[0] != want {
		t.Errorf("pinned baseline = %v, want [%v]", got, want)
	}
	// ADR-0011: a run whose guardrails an older belay would silently drop
	// says so, so that belay refuses it instead.
	if !containsString(manifest.Requires, state.CapabilityPolicyGate) {
		t.Errorf("Requires = %v, want it to name %q", manifest.Requires, state.CapabilityPolicyGate)
	}
	// The gate is in the graph the dispatcher will walk.
	if _, err := plan.registry.Get(graph.NodePolicy); err != nil {
		t.Errorf("the resolved registry has no policy gate: %v", err)
	}
	if !containsString(plan.route, graph.NodePolicy) {
		t.Errorf("route = %v, want it to include %q", plan.route, graph.NodePolicy)
	}
}

// A run with no policy must be pinned as having none, not as having an empty
// one: the gate reads the difference, and an empty pin would put a gate on
// the route of a run that never took a baseline.
func TestCreateRunPinsNoPolicyWhenThereIsNone(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\noutput:\n  mode: in_place\n")

	plan, err := resolve(resolveRequest{workspace: ws})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	store, err := createRun(context.Background(), plan, runSpec{
		Workspace: ws, RunID: "20260101T000000Z-abcdef123456", Goal: "add tests",
	})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}
	manifest, err := store.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest.Policy != nil {
		t.Errorf("createRun pinned a policy for a run whose belay.yaml declares none: %+v", manifest.Policy)
	}
	if len(manifest.Requires) != 0 {
		t.Errorf("Requires = %v, want empty: an unguarded run is resumable by any belay", manifest.Requires)
	}
	if _, err := plan.registry.Get(graph.NodePolicy); err == nil {
		t.Error("the resolved registry has a policy gate for a run that declares no policy")
	}
}

// This is the hazard the pin exists for. A run starts unguarded, crashes, and
// somebody adds a policy before resuming it. The resumed run must finish the
// way it started -- with no gate -- because it never took a baseline, and a
// gate would judge its change against rules that arrived afterwards.
func TestAResumeTakesThePolicyFromTheRunAndNotTheFile(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\noutput:\n  mode: in_place\n")

	plan, err := resolve(resolveRequest{workspace: ws})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const runID = "20260101T000000Z-abcdef123456"
	if _, err := createRun(context.Background(), plan, runSpec{
		Workspace: ws, RunID: runID, Goal: "add tests",
	}); err != nil {
		t.Fatalf("createRun: %v", err)
	}

	// The policy arrives after the run began.
	writeConfig(t, ws, guardedConfig)
	resumed, err := resolve(resolveRequest{workspace: ws})
	if err != nil {
		t.Fatalf("resolve after the policy was added: %v", err)
	}
	// A fresh plan from the new file does have a gate, which is what makes
	// the assertions below mean something.
	if _, err := resumed.registry.Get(graph.NodePolicy); err != nil {
		t.Fatalf("resolve gave a fresh plan no gate, so this test proves nothing: %v", err)
	}
	layout, err := state.NewLayout(ws, runID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := resumed.resumePreflight(context.Background(), state.NewStore(layout)); err != nil {
		t.Fatalf("resumePreflight: %v", err)
	}
	if resumed.policy != nil {
		t.Errorf("the resumed plan took a policy the run never had: %+v", resumed.policy)
	}
	if _, err := resumed.registry.Get(graph.NodePolicy); err == nil {
		t.Error("the resumed run gained a policy gate it never started with")
	}
	if containsString(resumed.route, graph.NodePolicy) {
		t.Errorf("route = %v, want no policy gate", resumed.route)
	}
	// And the person is told their new policy is not what this run is doing.
	if !anyContains(resumed.warnings, "policy") {
		t.Errorf("warnings = %v, want one saying the run was started with no policy", resumed.warnings)
	}
}

// The other direction, which is just as wrong: a guarded run resumed after
// the policy was deleted must keep the gate and the rules it started under,
// or it finishes without the check it was started with and says nothing.
func TestAGuardedRunKeepsItsGateWhenThePolicyIsDeleted(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, guardedConfig)

	plan, err := resolve(resolveRequest{workspace: ws})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const runID = "20260101T000000Z-abcdef123456"
	if _, err := createRun(context.Background(), plan, runSpec{
		Workspace: ws, RunID: runID, Goal: "add tests",
	}); err != nil {
		t.Fatalf("createRun: %v", err)
	}

	writeConfig(t, ws, "version: 1\noutput:\n  mode: in_place\n")
	resumed, err := resolve(resolveRequest{workspace: ws})
	if err != nil {
		t.Fatalf("resolve after the policy was deleted: %v", err)
	}
	layout, err := state.NewLayout(ws, runID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := resumed.resumePreflight(context.Background(), state.NewStore(layout)); err != nil {
		t.Fatalf("resumePreflight: %v", err)
	}
	if resumed.policy == nil {
		t.Fatal("the resumed run lost the guardrails it was started under")
	}
	if got := resumed.policy.Policy.ForbiddenPaths; len(got) != 1 || got[0] != "infra/**" {
		t.Errorf("resumed forbidden_paths = %v, want [infra/**]", got)
	}
	if _, err := resumed.registry.Get(graph.NodePolicy); err != nil {
		t.Errorf("the resumed run has no policy gate: %v", err)
	}
	// Somebody who deleted the policy and resumed is entitled to know their
	// change is not what this run is doing.
	if !anyContains(resumed.warnings, "policy") {
		t.Errorf("warnings = %v, want one saying the run keeps the policy it started with", resumed.warnings)
	}
}

func containsString(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

func anyContains(list []string, want string) bool {
	for _, got := range list {
		if strings.Contains(got, want) {
			return true
		}
	}
	return false
}
