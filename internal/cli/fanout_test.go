package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
)

// fanoutConfig is a belay.yaml for an in-place run that fans out to three
// candidates, chosen by fewest issues.
const fanoutConfig = `version: 1
output:
  mode: in_place
budget:
  max_usd: 3.00
fanout:
  enabled: true
  candidates: 3
  select: fewest_issues
`

// singleConfig is the same run without the fanout.
const singleConfig = "version: 1\noutput:\n  mode: in_place\nbudget:\n  max_usd: 3.00\n"

// fannedRoute is the walk of an in-place run that fans out, with the gate.
var fannedRoute = []string{graph.NodePlan, graph.NodeApprove, graph.NodeFanout, graph.NodeJoin}

// TestCreateRunPinsTheFanout: whether a run fans out, how many candidates it
// has and how the winner is chosen are decided when it is created and
// recorded in its manifest, with the capability that says an older belay
// must refuse it rather than resume it as a single attempt and spend the
// ceiling again.
func TestCreateRunPinsTheFanout(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, fanoutConfig)

	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
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
	want := state.FanoutPin{Candidates: 3, Select: "fewest_issues"}
	if manifest.Fanout == nil || *manifest.Fanout != want {
		t.Fatalf("manifest.Fanout = %+v, want %+v", manifest.Fanout, want)
	}
	if !slices.Contains(manifest.Requires, state.CapabilityFanoutCandidateRuns) {
		t.Errorf("Requires = %v, want it to name %q", manifest.Requires, state.CapabilityFanoutCandidateRuns)
	}
	if !slices.Equal(plan.route, fannedRoute) {
		t.Errorf("route = %v, want %v", plan.route, fannedRoute)
	}
}

// TestCreateRunPinsNoFanoutWhenThereIsNone: a run that does not fan out
// records nothing and requires nothing, so any belay that reads its schema
// can resume it.
func TestCreateRunPinsNoFanoutWhenThereIsNone(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, singleConfig)

	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
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
	if manifest.Fanout != nil {
		t.Errorf("manifest.Fanout = %+v, want nil", *manifest.Fanout)
	}
	if slices.Contains(manifest.Requires, state.CapabilityFanoutCandidateRuns) {
		t.Errorf("Requires = %v, want no %q", manifest.Requires, state.CapabilityFanoutCandidateRuns)
	}
}

// TestResumeUsesThePinnedFanoutNotTheConfig is the rule the pin exists for,
// in all three directions belay.yaml can drift after a run starts. A resumed
// run walks the graph its manifest describes, and the person is told when
// the file now says something else.
//
//   - Turned off: the run keeps fanning out. Abandoning it would leave the
//     candidates it has already paid for unjudged, and their workspaces with
//     nothing to collect them.
//   - Turned on: the run stays a single attempt. It never claimed any
//     candidates, and its plan was approved for one.
//   - Reshaped: the run keeps its candidate count and its strategy. A new
//     count would change the candidate ids and the envelope split under
//     candidates already claimed; a new strategy could pick a different
//     winner than the attempt that crashed reported.
func TestResumeUsesThePinnedFanoutNotTheConfig(t *testing.T) {
	reshaped := `version: 1
output:
  mode: in_place
budget:
  max_usd: 3.00
fanout:
  enabled: true
  candidates: 5
  select: fastest
`
	standard := []string{
		graph.NodePlan, graph.NodeApprove, graph.NodeCode,
		graph.NodeWrite, graph.NodeTest, graph.NodeFix, graph.NodeReview,
	}
	for _, tc := range []struct {
		name       string
		created    string
		resumed    string
		wantPin    *state.FanoutPin
		wantRoute  []string
		wantWarned bool
	}{
		{
			name: "fanout turned off after the run started", created: fanoutConfig, resumed: singleConfig,
			wantPin: &state.FanoutPin{Candidates: 3, Select: "fewest_issues"}, wantRoute: fannedRoute,
			wantWarned: true,
		},
		{
			name: "fanout turned on after the run started", created: singleConfig, resumed: fanoutConfig,
			wantPin: nil, wantRoute: standard, wantWarned: true,
		},
		{
			name: "the fanout reshaped after the run started", created: fanoutConfig, resumed: reshaped,
			wantPin: &state.FanoutPin{Candidates: 3, Select: "fewest_issues"}, wantRoute: fannedRoute,
			wantWarned: true,
		},
		{
			name: "nothing changed", created: fanoutConfig, resumed: fanoutConfig,
			wantPin: &state.FanoutPin{Candidates: 3, Select: "fewest_issues"}, wantRoute: fannedRoute,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := goWorkspace(t)
			writeConfig(t, ws, tc.created)
			plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			const runID = "20260101T000000Z-abcdef123456"
			if _, err := createRun(context.Background(), plan, runSpec{
				Workspace: ws, RunID: runID, Goal: "add tests",
			}); err != nil {
				t.Fatalf("createRun: %v", err)
			}

			writeConfig(t, ws, tc.resumed)
			resumed, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
			if err != nil {
				t.Fatalf("resolve after the file changed: %v", err)
			}
			layout, err := state.NewLayout(ws, runID)
			if err != nil {
				t.Fatalf("NewLayout: %v", err)
			}
			if err := resumed.resumePreflight(context.Background(), state.NewStore(layout)); err != nil {
				t.Fatalf("resumePreflight: %v", err)
			}

			switch {
			case tc.wantPin == nil && resumed.fanout != nil:
				t.Errorf("the resumed plan fans out (%+v), but the run was started as one attempt", *resumed.fanout)
			case tc.wantPin != nil && (resumed.fanout == nil || *resumed.fanout != *tc.wantPin):
				t.Errorf("the resumed plan's fanout = %+v, want the pinned %+v", resumed.fanout, *tc.wantPin)
			}
			if !slices.Equal(resumed.route, tc.wantRoute) {
				t.Errorf("route = %v, want %v", resumed.route, tc.wantRoute)
			}
			for _, name := range tc.wantRoute {
				if _, err := resumed.registry.Get(name); err != nil {
					t.Errorf("the resumed registry cannot resolve %q, which its route names: %v", name, err)
				}
			}
			if warned := fanoutDriftWarned(resumed.warnings); warned != tc.wantWarned {
				t.Errorf("warned about fanout drift = %v, want %v; warnings: %v", warned, tc.wantWarned, resumed.warnings)
			}
		})
	}
}

// fanoutDriftWarned reports whether any warning tells the person that this
// run's fanout is not what belay.yaml now says.
func fanoutDriftWarned(warnings []string) bool {
	return slices.ContainsFunc(warnings, func(w string) bool {
		return strings.Contains(w, "fanout") && strings.Contains(w, "the way it started")
	})
}
