package state

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRegeneration_JSONKeySetMatchesSpec(t *testing.T) {
	assertExactJSONKeys(t, Regeneration{}, []string{"request", "guidance", "served"})
}

// TestRegenerationPending pins the one comparison the approve gate routes
// on. It is Request > Served, never Request != Served: a state.json whose
// Served ran ahead of Request — a hand edit, or a future belay's document
// read by an older one — would otherwise look like a request nobody made,
// and the gate would send the run back to the plan node on every pass until
// graph.max_steps ended it.
func TestRegenerationPending(t *testing.T) {
	tests := []struct {
		name string
		r    Regeneration
		want bool
	}{
		{"nobody asked", Regeneration{}, false},
		{"asked once, unserved", Regeneration{Request: 1, Guidance: "fewer steps"}, true},
		{"asked once, served", Regeneration{Request: 1, Guidance: "fewer steps", Served: 1}, false},
		{"asked twice, one served", Regeneration{Request: 2, Served: 1}, true},
		{"served ahead of the request", Regeneration{Request: 1, Served: 2}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.Pending(); got != tt.want {
				t.Errorf("Regeneration%+v.Pending() = %v, want %v", tt.r, got, tt.want)
			}
		})
	}
}

// TestSetRegenerationKeepsRequiresInStep is the capability half of
// ADR-0011: while a request is unserved the run's behaviour depends on a
// field an older belay would drop, so the document says so; once the
// request is served nothing depends on it any more, and the requirement
// goes away rather than following the run for the rest of its life.
func TestSetRegenerationKeepsRequiresInStep(t *testing.T) {
	tests := []struct {
		name string
		r    Regeneration
		want []string
	}{
		{"an unserved request is declared", Regeneration{Request: 1, Guidance: "fewer steps"}, []string{CapabilityPlanRegenerate}},
		{"a served request is not", Regeneration{Request: 1, Guidance: "fewer steps", Served: 1}, []string{}},
		{"no request at all", Regeneration{}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := NewState("ship the widget")

			st.SetRegeneration(tt.r)

			if diff := cmp.Diff(tt.want, st.Requires); diff != "" {
				t.Errorf("Requires (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.r, st.Plan.Regenerate); diff != "" {
				t.Errorf("Plan.Regenerate (-want +got):\n%s", diff)
			}
		})
	}
}

// A request that is declared and then served must leave the list exactly as
// it found it — empty, and not nil, so state.json keeps writing
// "requires": [] rather than null.
func TestSetRegenerationRestoresTheEmptyList(t *testing.T) {
	st := NewState("ship the widget")
	st.SetRegeneration(Regeneration{Request: 1, Guidance: "fewer steps"})

	st.SetRegeneration(Regeneration{Request: 1, Guidance: "fewer steps", Served: 1})

	if st.Requires == nil {
		t.Fatal("Requires became nil; state.json would write null where every other run writes []")
	}
	if len(st.Requires) != 0 {
		t.Errorf("Requires = %v, want empty once the request is served", st.Requires)
	}
}

// TestSetRegenerationLeavesOtherCapabilitiesAlone: the list is shared with
// every other control field a run depends on, and serving a plan request
// must not clear somebody else's requirement.
func TestSetRegenerationLeavesOtherCapabilitiesAlone(t *testing.T) {
	st := NewState("ship the widget")
	st.Requires = addCapability(st.Requires, "policy.baseline")

	st.SetRegeneration(Regeneration{Request: 1})
	st.SetRegeneration(Regeneration{Request: 1, Served: 1})

	if diff := cmp.Diff([]string{"policy.baseline"}, st.Requires); diff != "" {
		t.Errorf("Requires (-want +got):\n%s", diff)
	}
}

// TestApplyPatchKeepsRequiresInStep is the same invariant on the path the
// plan node uses. A node never writes state.json (ADR-0002): it hands back
// a Patch, so the Patch is where serving the request has to drop the
// requirement, or every run that ever asked for a new plan would go on
// demanding a capability from every belay that reads it.
func TestApplyPatchKeepsRequiresInStep(t *testing.T) {
	st := NewState("ship the widget")
	st.SetRegeneration(Regeneration{Request: 1, Guidance: "fewer steps"})
	if len(st.Requires) != 1 {
		t.Fatalf("test setup: Requires = %v, want the request declared", st.Requires)
	}

	served := st.Plan
	served.Regenerate.Served = 1
	if err := (Patch{Plan: &served}).Apply(&st); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if diff := cmp.Diff([]string{}, st.Requires); diff != "" {
		t.Errorf("Requires after the plan node served the request (-want +got):\n%s", diff)
	}

	// The dispatcher can apply the same Patch twice — a crash between
	// applying it and journalling the finish is exactly that — so the
	// second application must land on the same document.
	before := st
	if err := (Patch{Plan: &served}).Apply(&st); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if diff := cmp.Diff(before, st); diff != "" {
		t.Errorf("applying the patch twice changed the state (-first +second):\n%s", diff)
	}
}

// A run that asks for a new plan has to be readable by the belay that wrote
// it: the capability it declares must be one this binary knows, or the
// first resume after the request would refuse the run it just created.
func TestStateWithAPendingRequestReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := NewState("ship the widget")
	st.Plan = Plan{Path: "artifacts/plan.md", Digest: "sha256:aaa"}
	st.SetRegeneration(Regeneration{Request: 1, Guidance: "fewer steps, and name the files"})

	if err := SaveState(path, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	if diff := cmp.Diff(st, got); diff != "" {
		t.Errorf("state.json round trip (-saved +loaded):\n%s", diff)
	}
}

// TestPlanWithoutRegenerateDecodesToZero is a guard, not a RED: it states
// what a state.json written before this field existed has to mean, so that
// nobody later gives Regeneration a non-zero default and turns every older
// run into one asking for a new plan.
func TestPlanWithoutRegenerateDecodesToZero(t *testing.T) {
	var p Plan
	if err := json.Unmarshal([]byte(`{"path":"artifacts/plan.md","approved":true,"digest":"sha256:aaa"}`), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Regenerate.Pending() {
		t.Errorf("a plan written before regeneration existed reads as a pending request: %+v", p.Regenerate)
	}
}
