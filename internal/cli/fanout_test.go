package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/agent/replay"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
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

// TestOptionsHandAFanoutRunItsCandidates: the dispatcher can only run
// candidates it is given the means to run. Until options() passes a candidate
// registry, RunContext.CandidateRuns is nil and the fanout node refuses with
// graph.ErrNoCandidateRunner, which is the dormant state the pool shipped in.
//
// The registry is the candidates' graph, with the policy gate in it when the
// run has a policy, because the candidates are where a fanout run's change is
// made. Concurrency is fanout.concurrency as the file says it now, never a
// pinned value: lowering it on a resume is how a person backs off.
func TestOptionsHandAFanoutRunItsCandidates(t *testing.T) {
	work := []string{graph.NodeCode, graph.NodeFix, graph.NodeReview, graph.NodeTest, graph.NodeWrite}
	guarded := []string{graph.NodeCode, graph.NodeFix, graph.NodePolicy, graph.NodeReview, graph.NodeTest, graph.NodeWrite}

	for _, tc := range []struct {
		name            string
		config          string
		wantRegistry    []string
		wantConcurrency int
	}{
		{
			name: "a run that fans out", config: fanoutConfig + "  concurrency: 2\n",
			wantRegistry: work, wantConcurrency: 2,
		},
		{
			name: "a guarded run that fans out", config: fanoutConfig + "policy:\n  max_changed_files: 25\n",
			wantRegistry: guarded, wantConcurrency: 3,
		},
		{name: "a run that does not fan out", config: singleConfig, wantConcurrency: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := goWorkspace(t)
			writeConfig(t, ws, tc.config)
			plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if len(plan.blockers) > 0 {
				t.Fatalf("the plan is blocked: %s", blockerText(plan))
			}
			store, err := createRun(context.Background(), plan, runSpec{
				Workspace: ws, RunID: "20260101T000000Z-abcdef123456", Goal: "add tests",
			})
			if err != nil {
				t.Fatalf("createRun: %v", err)
			}
			opts := plan.options(store)

			switch {
			case tc.wantRegistry == nil && opts.CandidateRegistry != nil:
				t.Errorf("CandidateRegistry = %v, want nil for a run that does not fan out",
					opts.CandidateRegistry.Names())
			case tc.wantRegistry != nil && opts.CandidateRegistry == nil:
				t.Fatal("CandidateRegistry is nil: the fanout node would refuse with ErrNoCandidateRunner")
			case tc.wantRegistry != nil && !slices.Equal(opts.CandidateRegistry.Names(), tc.wantRegistry):
				t.Errorf("CandidateRegistry = %v, want %v", opts.CandidateRegistry.Names(), tc.wantRegistry)
			}
			if (opts.CandidateAgent != nil) != (tc.wantRegistry != nil) {
				t.Errorf("CandidateAgent set = %v, want %v", opts.CandidateAgent != nil, tc.wantRegistry != nil)
			}
			if opts.Concurrency != tc.wantConcurrency {
				t.Errorf("Concurrency = %d, want %d", opts.Concurrency, tc.wantConcurrency)
			}
		})
	}
}

// TestEachCandidateReplaysItsOwnAnswers is item 4 of the switch, end to end
// through the CLI's wiring: N candidates given one goal ask the agent the
// same question, so an unscoped cassette hands whichever candidate asks first
// whichever answer was recorded first. Each candidate must be given
// Backend.Scoped(its id), and then it gets its own answer back whatever order
// the candidates run in.
//
// The replay below runs in the opposite order to the recording, which is what
// a concurrent fanout does whenever the scheduler likes.
func TestEachCandidateReplaysItsOwnAnswers(t *testing.T) {
	ws := goWorkspace(t)
	answers := &belaytest.FakeAgent{
		NameValue: "claude-code",
		Func: func(_ context.Context, r belay.AgentRequest) (belay.AgentResponse, error) {
			return belay.AgentResponse{Text: "work from " + filepath.Base(r.WorkDir)}, nil
		},
	}
	rec := replay.Record(answers, replay.Provenance{BelayVersion: "v-test"})
	ask := func(dir string) belay.AgentRequest {
		return belay.AgentRequest{Prompt: "implement the plan", WorkDir: dir, Model: "sonnet"}
	}
	for _, id := range []string{"cand-01", "cand-02"} {
		if _, err := rec.Scoped(id).Invoke(t.Context(), ask("/recorded/"+id)); err != nil {
			t.Fatalf("record %s: %v", id, err)
		}
	}
	path := cassettePath(ws, "")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cassette := rec.Cassette()
	if err := cassette.Save(path); err != nil {
		t.Fatalf("save the cassette: %v", err)
	}

	writeConfig(t, ws, fanoutConfig+"agent:\n  mode: replay\n")
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.blockers) > 0 {
		t.Fatalf("the plan is blocked: %s", blockerText(plan))
	}
	store, err := createRun(context.Background(), plan, runSpec{
		Workspace: ws, RunID: "20260101T000000Z-abcdef123456", Goal: "add tests",
	})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}
	opts := plan.options(store)
	if opts.CandidateAgent == nil {
		t.Fatal("CandidateAgent is nil, so every candidate would share one unscoped cassette")
	}
	for _, id := range []string{"cand-02", "cand-01"} {
		resp, err := opts.CandidateAgent(id).Invoke(t.Context(), ask("/somewhere/else/"+id))
		if err != nil {
			t.Fatalf("%s replay: %v", id, err)
		}
		if want := "work from " + id; resp.Text != want {
			t.Errorf("%s was replayed %q, want its own %q", id, resp.Text, want)
		}
	}
}

// fanoutReviewBlocked counts the blockers that refuse a fanout over its review
// mode, so a test can tell "refused once" from "refused twice" -- a resume
// re-derives this blocker after resolve has already judged the file.
func fanoutReviewBlocked(p *runPlan) int {
	n := 0
	for _, b := range p.blockers {
		if strings.Contains(b.problem, "fans out") && strings.Contains(b.problem, "review.mode") {
			n++
		}
	}
	return n
}

// TestFanoutRefusesAReviewItCannotRunPerCandidate: SonarQube and AI review
// both name the SonarQube project after the directory they review, and a
// candidate's directory is cand-01, cand-02 and so on in every run in every
// repository. Every candidate would share projects with every other run's
// candidates, and each quality gate would read results somebody else wrote.
// Until project keys are scoped per candidate, a fanout with either mode is
// refused before anything is spent.
//
// The fanout is the run's pin and the review mode is read live, so a resume
// judges the pair the run will actually walk: a fanout run resumed into
// SonarQube is refused, once, and a single run resumed while the file
// enables fanout is not refused for a fanout it does not have.
func TestFanoutRefusesAReviewItCannotRunPerCandidate(t *testing.T) {
	const aiReview = "review:\n  mode: ai\n  ai:\n    mcp:\n      mode: docker\n"
	for _, tc := range []struct {
		name   string
		config string
		want   int
	}{
		{name: "lint review fans out", config: fanoutConfig, want: 0},
		{name: "SonarQube review cannot fan out", config: fanoutConfig + "review:\n  mode: sonar\n", want: 1},
		{name: "AI review cannot fan out", config: fanoutConfig + aiReview, want: 1},
		{name: "SonarQube review without a fanout", config: singleConfig + "review:\n  mode: sonar\n", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := goWorkspace(t)
			writeConfig(t, ws, tc.config)
			plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got := fanoutReviewBlocked(plan); got != tc.want {
				t.Errorf("refused over the review mode %d times, want %d; blockers: %s", got, tc.want, blockerText(plan))
			}
		})
	}

	for _, tc := range []struct {
		name             string
		created, resumed string
		want             int
	}{
		{
			name:    "a fanout run resumed into SonarQube is refused once",
			created: fanoutConfig, resumed: fanoutConfig + "review:\n  mode: sonar\n", want: 1,
		},
		{
			name:    "a single run resumed while the file fans out into SonarQube is not refused for it",
			created: singleConfig, resumed: fanoutConfig + "review:\n  mode: sonar\n", want: 0,
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
			if got := fanoutReviewBlocked(resumed); got != tc.want {
				t.Errorf("refused over the review mode %d times, want %d; blockers: %s", got, tc.want, blockerText(resumed))
			}
		})
	}
}

// TestAFanoutRunsModelsAreCheckedWhereTheyAreSpent: a model belay cannot
// price stops a run before it starts rather than in the middle of it, and
// the check walks the nodes a run reaches. A fanout run's code and fix
// nodes are on its candidates' walk, not its own, so a check that walked
// only the parent's route would wave through a bad agent.models.code -- and
// then fail inside every candidate, N times over.
func TestAFanoutRunsModelsAreCheckedWhereTheyAreSpent(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, fanoutConfig)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var got []string
	for _, m := range plan.models {
		got = append(got, m.node)
	}
	if want := []string{graph.NodePlan, graph.NodeCode, graph.NodeFix}; !slices.Equal(got, want) {
		t.Errorf("models are checked for %v, want %v", got, want)
	}
}

// TestDryRunShowsEachCandidatesEnvelope: a fanout multiplies a run's bill by
// N, so the dry run -- the place a person checks what they are about to pay
// for -- says how the work is divided and what each candidate may spend:
// the ceiling, divided once, evenly, before any candidate runs. It never
// invents a total; it states the share, the graph each candidate walks, the
// strategy that picks the winner, and the one way the run can pass
// budget.max_usd, so nobody reads the ceiling as a hard cap it is not.
func TestDryRunShowsEachCandidatesEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   []string
		absent []string
	}{
		{
			name:   "three at a time",
			config: fanoutConfig,
			want: []string{
				"its candidates",
				"3 candidates, 3 at a time, up to $1.00 and 666,666 tokens each",
				"each walks code → write → test → fix → review → done, in a workspace of its own",
				"the winner is the candidate with the fewest review issues",
				"up to one step per candidate over budget.max_usd",
				"each candidate gives up after 3 rounds of its test-and-fix loop, or 60 steps of its own",
			},
		},
		{
			name:   "one at a time, and never more at a time than there are candidates",
			config: fanoutConfig + "  concurrency: 1\n",
			want:   []string{"3 candidates, one at a time, up to $1.00 and 666,666 tokens each"},
		},
		{
			name:   "concurrency above the candidate count behaves as the count",
			config: fanoutConfig + "  concurrency: 9\n",
			want:   []string{"3 candidates, 3 at a time"},
		},
		{
			// join's fastest compares the time each candidate's own steps
			// took, not when it finished: under contention, finishing
			// first is partly a fact about the pool's scheduling.
			name:   "fastest is the least time spent, not the first to finish",
			config: strings.Replace(fanoutConfig, "select: fewest_issues", "select: fastest", 1),
			want:   []string{"the winner is the candidate whose own steps took the least time"},
			absent: []string{"finished in the least time"},
		},
		{
			name:   "a run that does not fan out has no candidates to describe",
			config: singleConfig,
			absent: []string{"its candidates"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := goWorkspace(t)
			writeConfig(t, ws, tc.config)
			out, _, err := runCLI(t, "--dry-run", "run", "--workspace", ws, "add tests")
			if err != nil {
				t.Fatalf("belay --dry-run run: %v\n%s", err, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("the dry run never says %q; got:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Errorf("the dry run says %q, and should not; got:\n%s", absent, out)
				}
			}
		})
	}
}

// TestAFanoutRunAnnouncesItsCandidates: the lines a real run prints as it
// starts say what each candidate may spend, beside the ceiling they are a
// share of, because that is the figure a person watching N candidates spend
// at once wants to hold them to.
func TestAFanoutRunAnnouncesItsCandidates(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, fanoutConfig)
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	plan, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	store := seedRunDir(t, plan, "add tests")
	announceStart(u, plan, store.Layout(), "add tests")

	got := out.String()
	for _, want := range []string{
		"candidates:          3, 3 at a time, up to $1.00 and 666,666 tokens each",
		"route:  plan → approve → fanout → join → done",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the run never says %q; got:\n%s", want, got)
		}
	}
}

// TestNextStepsAfterAFanout: what a person is told to do next has to match
// where a fanout run's work actually is.
//
//   - Paused at the fanout, some candidates could not be finished -- a full
//     disk, a git lock. Telling the person to read a plan nobody is waiting
//     on sends them to the wrong file; what they need to know is that the
//     finished candidates are kept and resuming retries only the others.
//   - Completed in place, the winner's change is in its own workspace, not in
//     the person's checkout: belay does not merge an in-place run, and join's
//     own note says to copy the change back by hand. "Review the change in
//     your repository" would point at a checkout that does not have it.
//   - Failed with no winner, every candidate's own run is where the answer
//     is, and the command that opens one is named.
func TestNextStepsAfterAFanout(t *testing.T) {
	const runID = "20260101T000000Z-c0ffee000000"
	layout, err := state.NewLayout("/repos/app", runID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	winnerDir := "/repos/app/.belay/runs/" + runID + "/candidates/cand-02"

	for _, tc := range []struct {
		name   string
		view   outcomeView
		want   []string
		absent []string
	}{
		{
			name: "paused inside the fanout",
			view: outcomeView{layout: layout, fanout: true, outcome: graph.Outcome{
				Status: state.RunStatusPaused, Node: graph.NodeFanout,
				Note: "1 of 3 candidates could not be finished: cand-02: no space left on device",
			}},
			want: []string{
				"some candidates could not be finished",
				"the ones that finished are kept, and resuming runs only the others",
				"belay resume " + runID,
				"belay timeline " + runID,
			},
			absent: []string{"read the plan"},
		},
		{
			name: "completed in place with a winner",
			view: outcomeView{
				layout: layout, fanout: true,
				winner:  state.Candidate{ID: "cand-02", Dir: winnerDir},
				outcome: graph.Outcome{Status: state.RunStatusCompleted, Node: graph.End},
			},
			want: []string{
				"the winner is cand-02",
				winnerDir,
				"belay timeline " + runID + " --candidate cand-02",
			},
			absent: []string{"review the change in your repository"},
		},
		{
			name: "failed with no winner",
			view: outcomeView{layout: layout, fanout: true, outcome: graph.Outcome{
				Status: state.RunStatusFailed, Node: graph.NodeJoin,
				Note: "no winner: all 3 candidates failed their tests",
			}},
			want: []string{
				"belay timeline " + runID,
				"belay timeline " + runID + " --candidate cand-01",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := strings.Join(nextSteps(tc.view), "\n")
			for _, want := range tc.want {
				if !strings.Contains(lines, want) {
					t.Errorf("next steps do not mention %q:\n%s", want, lines)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(lines, absent) {
					t.Errorf("next steps say %q, and should not:\n%s", absent, lines)
				}
			}
		})
	}
}

// TestWinnerOfReadsTheBlackboardAfterTheDispatch: next steps name the
// winner's workspace, which only the blackboard records, as join's Patch put
// it there. A run that does not fan out, or has no winner, gets the zero
// Candidate, and a winner the candidate list does not hold is still named.
func TestWinnerOfReadsTheBlackboardAfterTheDispatch(t *testing.T) {
	layout, err := state.NewLayout(t.TempDir(), "20260101T000000Z-c0ffee000000")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	store := state.NewStore(layout)
	st := state.NewState("add tests")
	st.Candidates = []state.Candidate{
		{ID: "cand-01", Dir: "/w/cand-01"},
		{ID: "cand-02", Dir: "/w/cand-02", TestPassed: true},
	}
	st.Winner = "cand-02"
	if err := store.SaveState(st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if got := winnerOf(store, true); got.ID != "cand-02" || got.Dir != "/w/cand-02" {
		t.Errorf("winnerOf = %+v, want cand-02 in /w/cand-02", got)
	}
	if got := winnerOf(store, false); !reflect.DeepEqual(got, state.Candidate{}) {
		t.Errorf("winnerOf a run that does not fan out = %+v, want the zero Candidate", got)
	}

	st.Winner = "cand-09"
	if err := store.SaveState(st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if got := winnerOf(store, true); got.ID != "cand-09" || got.Dir != "" {
		t.Errorf("winnerOf an unlisted winner = %+v, want cand-09 with no directory", got)
	}
}

// TestNarratorPrefixesCandidateLines: with fanout.concurrency above 1,
// several candidate runs narrate at once, and five "→ test step 3" lines from
// five candidates are indistinguishable. A candidate's dispatcher logs with a
// candidate attribute -- the runner attaches it to every candidate logger --
// so the narrator puts the id in front of that candidate's lines, and the
// parent's own lines stay exactly as they were.
func TestNarratorPrefixesCandidateLines(t *testing.T) {
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	logger := slog.New(&narrator{ui: u})

	parent := logger.With(slog.String("node", graph.NodeFanout), slog.Int("step", 3), slog.Int("attempt", 1))
	parent.Info(msgNodeStarted)

	cand := logger.With(slog.String("candidate", "cand-02")).
		With(slog.String("node", graph.NodeTest), slog.Int("step", 3), slog.Int("attempt", 1))
	cand.Info(msgNodeStarted)
	cand.Info(msgNodeFinished,
		slog.String("status", "ok"), slog.String("next", graph.NodeReview), slog.Duration("elapsed", time.Second))

	got := out.String()
	for _, want := range []string{
		"  → fanout    step 3",
		"  [cand-02] → test      step 3",
		"    [cand-02] test      took 1s → review",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("narration never says %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[] ") || strings.Contains(got, "[cand-02] → fanout") {
		t.Errorf("the parent's own lines carry a candidate prefix; got:\n%s", got)
	}
}
