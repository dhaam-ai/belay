package graph_test

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/policy"
	"github.com/dhaam-ai/belay/internal/state"
)

// TestACandidateRunIsHeldToItsParentsPolicy pins that a guarded run's
// guardrails reach every candidate it fans out to.
//
// A node reads the policy it judges against from RunContext.Policy, which the
// dispatcher fills from the manifest of the run it is driving -- and a
// candidate run has a manifest of its own. If that manifest does not carry
// the parent's pin, the candidate's policy gate finds no baseline and passes
// the change straight through to test, so join's refusal of a candidate that
// broke a guardrail never has anything to refuse: best-of-N becomes the way
// around the policy, which is exactly what join's eligibility rule exists to
// stop.
//
// So the pin is copied into the candidate's manifest when it is claimed,
// through SetPolicy, which records the capability with it: an older belay
// that could not hold a candidate to the policy must refuse to resume one
// rather than walk its gate unguarded.
func TestACandidateRunIsHeldToItsParentsPolicy(t *testing.T) {
	h := newRun(t, budgetCfg(3.00, 0), "ship it")
	pin := state.PolicyPin{
		Policy: policy.Policy{ForbiddenPaths: []string{"secrets/**"}, MaxChangedFiles: 4},
		Baseline: policy.Baseline{Dependencies: []policy.Dependency{
			{Manifest: "go.mod", Name: "example.com/dep"},
		}},
	}
	man, err := h.store.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	man.SetPolicy(pin)
	if err := h.store.SaveManifest(man); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}

	var (
		mu   sync.Mutex
		seen = map[string]*state.PolicyPin{}
	)
	code := newCandidateNode(graph.NodeCode, func(ctx context.Context, id string, rc *graph.RunContext) (graph.Result, error) {
		mu.Lock()
		seen[id] = rc.Policy
		mu.Unlock()
		return finishes(0.10)(ctx, id, rc)
	})
	node := &runsCandidates{specs: poolSpecs(h, 2)}
	iso := newFakeIsolator(h.layout.CandidatesDir())

	if _, err := h.poolDispatcher(t, pool{registry: candidateRegistry(t, code), isolator: iso}, node).
		Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if node.err != nil {
		t.Fatalf("CandidateRuns.Run() error = %v, want nil", node.err)
	}

	for _, id := range []string{"cand-01", "cand-02"} {
		got := seen[id]
		if got == nil {
			t.Errorf("%s ran with no policy: its gate would pass any change it made", id)
			continue
		}
		if !reflect.DeepEqual(*got, pin) {
			t.Errorf("%s ran with policy %+v, want its parent's %+v", id, *got, pin)
		}
		cm := candidateManifest(t, h, id)
		if cm.Policy == nil || !reflect.DeepEqual(*cm.Policy, pin) {
			t.Errorf("%s manifest Policy = %+v, want the parent's pin", id, cm.Policy)
		}
		if !slices.Contains(cm.Requires, state.CapabilityPolicyGate) {
			t.Errorf("%s manifest requires %v, want it to name %q", id, cm.Requires, state.CapabilityPolicyGate)
		}
	}
}

// TestAnUnguardedRunsCandidatesRequireNoPolicy is the other half: a run with
// no policy hands its candidates none, and their manifests declare no
// capability for it -- the same rule as for any run, that a guardrail nobody
// configured costs nothing.
func TestAnUnguardedRunsCandidatesRequireNoPolicy(t *testing.T) {
	h := newRun(t, budgetCfg(3.00, 0), "ship it")
	code := newCandidateNode(graph.NodeCode, finishes(0.10))
	node := &runsCandidates{specs: poolSpecs(h, 1)}
	iso := newFakeIsolator(h.layout.CandidatesDir())

	if _, err := h.poolDispatcher(t, pool{registry: candidateRegistry(t, code), isolator: iso}, node).
		Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	cm := candidateManifest(t, h, "cand-01")
	if cm.Policy != nil {
		t.Errorf("manifest Policy = %+v, want nil for an unguarded parent", *cm.Policy)
	}
	if slices.Contains(cm.Requires, state.CapabilityPolicyGate) {
		t.Errorf("manifest requires %v, want no %q", cm.Requires, state.CapabilityPolicyGate)
	}
}
