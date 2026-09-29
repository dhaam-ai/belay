package state

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/policy"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/google/go-cmp/cmp"
)

// samplePin is a policy pin with a rule and a baseline in it.
func samplePin() PolicyPin {
	return PolicyPin{
		Policy:   policy.Policy{ForbiddenPaths: []string{"infra/**"}, NoNewDependencies: true},
		Baseline: policy.Baseline{Dependencies: []policy.Dependency{{Manifest: "go.mod", Name: "github.com/spf13/cobra"}}},
	}
}

// ADR-0011: a control field and the capability that protects it are one
// decision, so the pin is only ever set through the setter that records both.
// A belay that did not understand Manifest.Policy would decode a guarded
// run's manifest, drop the block, and resume the run with no guardrails at
// all and nothing said about it — which is the whole failure this slice
// exists to prevent, arriving by a different door.
func TestManifest_SetPolicyRecordsTheCapabilityAGuardedRunNeeds(t *testing.T) {
	m := NewManifest(fixedTime(), "run-1", "/w", "goal", config.Default())
	pin := samplePin()
	m.SetPolicy(pin)

	if m.Policy == nil {
		t.Fatal("SetPolicy left Manifest.Policy nil")
	}
	if diff := cmp.Diff(pin, *m.Policy); diff != "" {
		t.Errorf("Manifest.Policy (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{CapabilityPolicyGate}, m.Requires); diff != "" {
		t.Errorf("Requires (-want +got):\n%s", diff)
	}

	m.SetPolicy(pin)
	if diff := cmp.Diff([]string{CapabilityPolicyGate}, m.Requires); diff != "" {
		t.Errorf("Requires after a second SetPolicy (-want +got):\n%s", diff)
	}
}

func TestManifest_SetPolicyLeavesOtherCapabilitiesAlone(t *testing.T) {
	m := NewManifest(fixedTime(), "run-1", "/w", "goal", config.Default())
	m.SetOutput(Output{Mode: OutputBranch, Branch: "belay/run-1"})
	m.SetPolicy(samplePin())

	want := []string{CapabilityOutputBranch, CapabilityPolicyGate}
	if diff := cmp.Diff(want, m.Requires); diff != "" {
		t.Errorf("Requires (-want +got):\n%s", diff)
	}
}

// The capability has to be one this binary knows, or every guarded run this
// belay writes is one it then refuses to read.
func TestPolicyGateIsAKnownCapability(t *testing.T) {
	if !knownCapabilities[CapabilityPolicyGate] {
		t.Errorf("%q is set in requires but is not in knownCapabilities; this belay would refuse every run it started",
			CapabilityPolicyGate)
	}
}

func TestPolicyPinRoundTripsThroughTheManifest(t *testing.T) {
	m := NewManifest(fixedTime(), "run-1", "/w", "goal", config.Default())
	m.SetPolicy(samplePin())

	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := decodeManifest(raw)
	if err != nil {
		t.Fatalf("decodeManifest: %v", err)
	}
	if got.Policy == nil {
		t.Fatal("the policy pin did not survive a round trip through manifest.json")
	}
	if diff := cmp.Diff(samplePin(), *got.Policy); diff != "" {
		t.Errorf("policy pin (-want +got):\n%s", diff)
	}
}

// A run with no pin is a run with no policy, and a manifest written before
// this field existed is exactly that. It must not decode as a run whose
// guardrails are all switched off by an empty rule set — it must decode as
// having none at all, which is what the gate reads to say so.
func TestAManifestWithNoPolicyPinRequiresNothing(t *testing.T) {
	m := NewManifest(fixedTime(), "run-1", "/w", "goal", config.Default())
	if m.Policy != nil {
		t.Error("NewManifest invented a policy pin")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, err := decodeManifest(raw); err != nil {
		t.Fatalf("decodeManifest: %v", err)
	} else if got.Policy != nil {
		t.Error("a manifest with no policy block decoded as one with a pin")
	}
	if len(m.Requires) != 0 {
		t.Errorf("Requires = %v, want empty: an unguarded run is resumable by any belay that reads this schema", m.Requires)
	}
}

// PinnedConfig decides what a node reads. The policy a run is held to comes
// from its manifest and never from the file, so the live section is cleared
// rather than merely ignored: a node that read rc.Config.Policy by accident
// would be judging the change against a belay.yaml somebody rewrote after
// the run began, and this makes that node impossible to write.
func TestPinnedConfigClearsTheLivePolicySection(t *testing.T) {
	live := config.Default()
	live.Policy = config.Policy{ForbiddenPaths: []string{"added/after/the/run/started/**"}}

	for _, tt := range []struct {
		name string
		pin  *PolicyPin
	}{
		{name: "a run with no pin"},
		{name: "a guarded run", pin: ptr(samplePin())},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManifest(fixedTime(), "run-1", "/w", "goal", config.Default())
			if tt.pin != nil {
				m.SetPolicy(*tt.pin)
			}
			got := m.PinnedConfig(live)
			if diff := cmp.Diff(config.Policy{}, got.Policy); diff != "" {
				t.Errorf("PinnedConfig left a live policy section a node could read (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(live.Budget, got.Budget); diff != "" {
				t.Errorf("budget.* must stay live (-live +got):\n%s", diff)
			}
		})
	}
}

// The blackboard keeps the fields the graph acts on and not the adapter's
// whole report, exactly as state.Review does for a quality gate.
func TestNewPolicyProjectsAQualityReport(t *testing.T) {
	report := policy.Report{Violations: []policy.Violation{
		{Rule: policy.RuleForbiddenPath, Class: policy.ClassFail, Path: "infra/a.tf", Message: "not yours"},
	}}.QualityReport()

	got := NewPolicy(report, "artifacts/policy-0007.json")
	if got.Gate != belay.GateFail {
		t.Errorf("Gate = %s, want %s", got.Gate, belay.GateFail)
	}
	if diff := cmp.Diff(report.Issues, got.Violations); diff != "" {
		t.Errorf("Violations (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(report.Counts, got.Counts); diff != "" {
		t.Errorf("Counts (-want +got):\n%s", diff)
	}
	if got.Summary != report.Summary {
		t.Errorf("Summary = %q, want %q", got.Summary, report.Summary)
	}
	if got.ReportPath != "artifacts/policy-0007.json" {
		t.Errorf("ReportPath = %q", got.ReportPath)
	}

	back := got.QualityReport()
	if diff := cmp.Diff(report, back); diff != "" {
		t.Errorf("the projection is not lossless for the fields the graph acts on (-want +got):\n%s", diff)
	}
	if empty := (Policy{}).QualityReport(); empty.Issues == nil {
		t.Error("an empty block's QualityReport has nil Issues; the contract's shape guarantee breaks first for the emptiest report")
	}
}

func TestPatchSetsThePolicyBlock(t *testing.T) {
	st := NewState("goal")
	block := Policy{Gate: belay.GateFail, Summary: "1 policy violation the run cannot continue past"}
	if err := (Patch{Policy: &block}).Apply(&st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if diff := cmp.Diff(block, st.Policy); diff != "" {
		t.Errorf("State.Policy after a Patch (-want +got):\n%s", diff)
	}

	// Absolute sets, not deltas: re-applying the same Patch after a crash
	// must land on the same blackboard.
	if err := (Patch{Policy: &block}).Apply(&st); err != nil {
		t.Fatalf("Apply again: %v", err)
	}
	if diff := cmp.Diff(block, st.Policy); diff != "" {
		t.Errorf("State.Policy after re-applying (-want +got):\n%s", diff)
	}

	clean := Policy{Gate: belay.GatePass}
	if err := (Patch{Policy: &clean}).Apply(&st); err != nil {
		t.Fatalf("Apply clean: %v", err)
	}
	if len(st.Policy.Violations) != 0 {
		t.Error("a passing gate left the previous attempt's violations on the blackboard")
	}
}

func TestCandidateCarriesItsPolicyViolationCount(t *testing.T) {
	raw := []byte(`{"id":"cand-01","test_passed":true,"policy_violations":2}`)
	var got Candidate
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PolicyViolations != 2 {
		t.Errorf("Candidate.PolicyViolations = %d, want 2", got.PolicyViolations)
	}
}

func ptr[T any](v T) *T { return &v }

// fixedTime is a stable creation time, so nothing here depends on the clock.
func fixedTime() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
