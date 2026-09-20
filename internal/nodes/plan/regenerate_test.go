package plan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

const testGuidance = "fewer steps, and leave the parser alone"

// writePlanArtifact puts a plan on disk the way a previous execution of
// this node would have, and records it on the blackboard.
func writePlanArtifact(t *testing.T, rc *graph.RunContext, body string) {
	t.Helper()
	rel, err := rc.WriteArtifact(ArtifactName, []byte(body))
	if err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	rc.State.Plan.Path = rel
	rc.State.Plan.Digest = Digest([]byte(body))
}

// readPlanArtifact returns what is in plan.md right now.
func readPlanArtifact(t *testing.T, rc *graph.RunContext) string {
	t.Helper()
	path, err := rc.Layout.ArtifactPath(ArtifactName)
	if err != nil {
		t.Fatalf("ArtifactPath: %v", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is built from the test's own Layout
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return string(data)
}

func TestReplacedArtifactNamesOneSnapshotPerRequest(t *testing.T) {
	for i, want := range map[int]string{1: "plan-replaced-1.md", 2: "plan-replaced-2.md", 17: "plan-replaced-17.md"} {
		if got := ReplacedArtifact(i); got != want {
			t.Errorf("ReplacedArtifact(%d) = %q, want %q", i, got, want)
		}
	}
}

// TestPromptCarriesGuidanceOnlyWhenThereIsAny pins both halves. A first
// plan's prompt must not gain a byte: it is what every recorded cassette
// was recorded against, and what the node's evidence is compared with. A
// revision's prompt must carry the person's own words, or asking for a new
// plan means asking the same question twice and hoping.
func TestPromptCarriesGuidanceOnlyWhenThereIsAny(t *testing.T) {
	const workDir = "/work/repo"

	first := userPrompt(testGoal, workDir, state.Regeneration{})
	if want := fmt.Sprintf(planTemplate, testGoal, workDir); first != want {
		t.Errorf("the first plan's prompt changed shape:\n got %q\nwant %q", first, want)
	}
	if strings.Contains(first, "# Revision") {
		t.Errorf("a first plan's prompt asks for a revision:\n%s", first)
	}

	again := userPrompt(testGoal, workDir, state.Regeneration{Request: 1, Guidance: testGuidance})
	if !strings.HasPrefix(again, first) {
		t.Error("a revision's prompt is not the ordinary prompt plus something; the goal or the rules moved")
	}
	for _, want := range []string{"# Revision", testGuidance} {
		if !strings.Contains(again, want) {
			t.Errorf("a revision's prompt does not carry %q:\n%s", want, again)
		}
	}

	// Asking again with nothing to add is a legitimate request: the person
	// wants another try, not a repeat of the same conversation.
	blank := userPrompt(testGoal, workDir, state.Regeneration{Request: 2, Served: 1})
	if !strings.Contains(blank, "# Revision") {
		t.Errorf("a request with no guidance does not say the plan is being rewritten:\n%s", blank)
	}
}

// TestServesTheRequestInItsOwnPatch is the heart of the edge: the node that
// answers a request is the node that records it answered, in the Patch the
// dispatcher applies, because a node writes no state itself (ADR-0002).
func TestServesTheRequestInItsOwnPatch(t *testing.T) {
	agent := planAgent("# Summary\n\nA shorter plan.\n\n## Steps\n\n1. Do less.\n")
	rc := newRunContext(t, agent)
	writePlanArtifact(t, rc, testPlan)
	rc.State.Plan.Approved = true
	rc.State.SetRegeneration(state.Regeneration{Request: 1, Guidance: testGuidance})

	res, err := (&Node{}).Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Patch.Plan == nil {
		t.Fatalf("Patch carries no Plan: %+v", res.Patch)
	}
	want := state.Regeneration{Request: 1, Guidance: testGuidance, Served: 1}
	if got := res.Patch.Plan.Regenerate; got != want {
		t.Errorf("Patch.Plan.Regenerate = %+v, want %+v", got, want)
	}
	if res.Patch.Plan.Regenerate.Pending() {
		t.Error("the request is still outstanding, so the gate would send the run round again")
	}
	if res.Patch.Plan.Approved {
		t.Error("the new plan arrived approved; nobody has read it")
	}
	if got, want := res.Next, graph.NodeApprove; got != want {
		t.Errorf("Next = %q, want %q: the person who asked is waiting at the gate", got, want)
	}
	sent, _ := agent.LastCall()
	if !strings.Contains(sent.Prompt, testGuidance) {
		t.Errorf("the agent was not told what to do differently:\n%s", sent.Prompt)
	}
	if got := readPlanArtifact(t, rc); !strings.Contains(got, "Do less") {
		t.Errorf("plan.md still holds the old plan:\n%s", got)
	}
}

// TestRegenerationSurvivesAReRunAfterItsOwnPatch is the crash that the two
// counters exist for: the dispatcher applied the Patch and died before
// journalling the finish, so the node runs again over a blackboard where
// the request already reads as served. It must ask the agent exactly what
// it asked the first time — the same guidance, the same route — or the
// re-run quietly loses the one thing the person said was wrong.
func TestRegenerationSurvivesAReRunAfterItsOwnPatch(t *testing.T) {
	agent := planAgent("# Summary\n\nA shorter plan.\n\n## Steps\n\n1. Do less.\n")
	rc := newRunContext(t, agent)
	writePlanArtifact(t, rc, testPlan)
	rc.State.SetRegeneration(state.Regeneration{Request: 1, Guidance: testGuidance})

	first, err := (&Node{}).Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := first.Patch.Apply(&rc.State); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rc.Attempt = 2

	second, err := (&Node{}).Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}

	calls := agent.Calls()
	if len(calls) != 2 {
		t.Fatalf("agent was called %d time(s), want 2", len(calls))
	}
	if calls[0].Prompt != calls[1].Prompt {
		t.Errorf("the re-run asked a different question:\n first %q\nsecond %q", calls[0].Prompt, calls[1].Prompt)
	}
	for i, call := range calls {
		if !strings.Contains(call.Prompt, testGuidance) {
			t.Errorf("call %d does not carry the guidance, so this comparison would pass vacuously:\n%s", i+1, call.Prompt)
		}
	}
	if got, want := second.Next, first.Next; got != want {
		t.Errorf("Next = %q, want %q", got, want)
	}
	if got, want := *second.Patch.Plan, *first.Patch.Plan; got != want {
		t.Errorf("Patch.Plan = %+v, want %+v", got, want)
	}
}

// TestFailedRegenerationKeepsThePlanThePersonHad: a first plan has no
// fallback, which is why a backend failure there is an error and the run
// ends. A revision does have one — the plan already on disk, which the
// person was reading when they asked for another. Ending the run then
// would take that plan away as punishment for an agent that timed out.
func TestFailedRegenerationKeepsThePlanThePersonHad(t *testing.T) {
	tests := []struct {
		name  string
		agent *belaytest.FakeAgent
	}{
		{
			name: "the backend failed",
			agent: &belaytest.FakeAgent{
				Errs:      []error{errors.New("connection reset")},
				Responses: []belay.AgentResponse{{Usage: testUsage}},
			},
		},
		{
			name:  "the backend answered with nothing",
			agent: planAgent("   \n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := newRunContext(t, tt.agent)
			writePlanArtifact(t, rc, testPlan)
			rc.State.SetRegeneration(state.Regeneration{Request: 1, Guidance: testGuidance})

			res, err := (&Node{}).Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run: %v; a failed revision must not end a run whose plan is intact", err)
			}
			if verr := res.Validate(); verr != nil {
				t.Fatalf("Result the dispatcher cannot act on: %v", verr)
			}
			if got, want := res.Status, journal.StatusOK; got != want {
				t.Errorf("Status = %v, want %v", got, want)
			}
			if got, want := res.Next, graph.NodeApprove; got != want {
				t.Errorf("Next = %q, want %q", got, want)
			}
			if got := readPlanArtifact(t, rc); got != testPlan {
				t.Errorf("the plan the person had is gone:\n got %q\nwant %q", got, testPlan)
			}
			if res.Patch.Plan == nil {
				t.Fatalf("Patch carries no Plan: %+v", res.Patch)
			}
			if got := res.Patch.Plan.Regenerate.Served; got != 1 {
				t.Errorf("Served = %d, want 1: an unserved request sends the run round the same loop again", got)
			}
			if got, want := res.Patch.Plan.Digest, rc.State.Plan.Digest; got != want {
				t.Errorf("Digest = %q, want %q: the plan on disk did not change", got, want)
			}
			if res.Usage != testUsage {
				t.Errorf("Usage = %+v, want %+v: the ledger pays for the attempt either way", res.Usage, testUsage)
			}
			if !strings.Contains(res.Note, "plan") {
				t.Errorf("Note does not say what happened: %q", res.Note)
			}
		})
	}
}

// With no plan to fall back to there is nothing to keep, and a failure is
// a failure. This is the run whose plan.md a person deleted before asking
// for a new one.
func TestFailedRegenerationWithNoFallbackIsStillAnError(t *testing.T) {
	agent := &belaytest.FakeAgent{Errs: []error{errors.New("connection reset")}}
	rc := newRunContext(t, agent)
	rc.State.SetRegeneration(state.Regeneration{Request: 1, Guidance: testGuidance})

	if _, err := (&Node{}).Run(context.Background(), rc); err == nil {
		t.Fatal("Run returned no error although there is no plan to fall back to")
	}
}
