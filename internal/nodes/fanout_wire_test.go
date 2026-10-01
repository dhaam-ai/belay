package nodes_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// TestFanoutRouteFollowsThePinNotTheFile pins the route of a run that fans
// out, and that WithFanout -- the pin -- decides it, never fanout.enabled.
//
// A fanout run's own walk is plan, approve, fanout, join, and promote for a
// run that delivers on a branch. The work nodes -- code, write, the policy
// gate, test, fix, review -- are not on it and not registered in it: they
// are the candidates' graph (see Candidate), and a parent registry holding
// them would turn a wiring bug that routed the parent into code into one
// silent single attempt in the person's own workspace, rather than an
// unknown node that stops the run.
//
// The options compose in either order. WithBranchOutput and WithPolicyGate
// were written before a run could fan out, and both anchor on nodes the
// fanout takes off the route; a run's graph must not depend on which of its
// pins happened to be read first.
func TestFanoutRouteFollowsThePinNotTheFile(t *testing.T) {
	t.Parallel()

	fileSaysFanout := config.Default()
	fileSaysFanout.Fanout.Enabled = true
	fileSaysFanout.Fanout.Candidates = 3
	approvalOff := config.Default()
	approvalOff.Graph.Approval = false

	standard := []string{
		graph.NodePlan, graph.NodeApprove, graph.NodeCode,
		graph.NodeWrite, graph.NodeTest, graph.NodeFix, graph.NodeReview,
	}
	fanned := []string{graph.NodePlan, graph.NodeApprove, graph.NodeFanout, graph.NodeJoin}
	promoted := append(slices.Clone(fanned), graph.NodePromote)

	for _, tc := range []struct {
		name     string
		cfg      config.Config
		opts     []nodes.Option
		want     []string
		offRoute []string
	}{
		{
			name: "fanout.enabled in the file alone routes nothing to fanout",
			cfg:  fileSaysFanout, want: standard,
		},
		{
			name: "the pin routes through fanout and join",
			cfg:  config.Default(), opts: []nodes.Option{nodes.WithFanout()}, want: fanned,
		},
		{
			name: "with approval off the gate leaves the route and stays registered",
			cfg:  approvalOff, opts: []nodes.Option{nodes.WithFanout()},
			want:     []string{graph.NodePlan, graph.NodeFanout, graph.NodeJoin},
			offRoute: []string{graph.NodeApprove},
		},
		{
			name: "a branch run promotes after join, fanout pinned first",
			cfg:  config.Default(), opts: []nodes.Option{nodes.WithFanout(), nodes.WithBranchOutput()},
			want: promoted,
		},
		{
			name: "a branch run promotes after join, fanout pinned last",
			cfg:  config.Default(), opts: []nodes.Option{nodes.WithBranchOutput(), nodes.WithFanout()},
			want: promoted,
		},
		{
			name: "the policy gate is the candidates', not the parent's, fanout first",
			cfg:  config.Default(), opts: []nodes.Option{nodes.WithFanout(), nodes.WithPolicyGate()},
			want: fanned,
		},
		{
			name: "the policy gate is the candidates', not the parent's, fanout last",
			cfg:  config.Default(),
			opts: []nodes.Option{nodes.WithPolicyGate(), nodes.WithBranchOutput(), nodes.WithFanout()},
			want: promoted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := nodes.Route(tc.cfg, tc.opts...)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Route() = %v, want %v", got, tc.want)
			}
			registry, err := nodes.Default(tc.cfg, tc.opts...)
			if err != nil {
				t.Fatalf("Default: %v", err)
			}
			registered := slices.Sorted(slices.Values(registry.Names()))
			accounted := slices.Sorted(slices.Values(slices.Concat(got, tc.offRoute)))
			if !slices.Equal(registered, accounted) {
				t.Errorf("Default registers %v; the route and the off-route nodes account for %v",
					registered, accounted)
			}
		})
	}
}

// TestAFanoutRunsEdgesLeadIntoTheFanout runs the registered nodes, so what is
// checked is the edge each node really takes rather than the edge the wiring
// meant: the plan and the gate open onto fanout, and join hands its winner to
// promote in a branch run and ends an in-place one.
func TestAFanoutRunsEdgesLeadIntoTheFanout(t *testing.T) {
	t.Parallel()

	t.Run("a plan with no gate to stop at goes to the fanout", func(t *testing.T) {
		t.Parallel()
		cfg := config.Default()
		cfg.Graph.Approval = false
		registry, err := nodes.Default(cfg, nodes.WithFanout())
		if err != nil {
			t.Fatalf("Default: %v", err)
		}
		rc := planContext(t, cfg)
		if got := runNode(t, registry, graph.NodePlan, rc).Next; got != graph.NodeFanout {
			t.Errorf("plan routes to %q, want %q", got, graph.NodeFanout)
		}
	})

	t.Run("an approved plan goes to the fanout", func(t *testing.T) {
		t.Parallel()
		registry, err := nodes.Default(config.Default(), nodes.WithFanout())
		if err != nil {
			t.Fatalf("Default: %v", err)
		}
		rc := planContext(t, config.Default())
		writePlan(t, rc)
		rc.State.Plan.Approved = true
		if got := runNode(t, registry, graph.NodeApprove, rc).Next; got != graph.NodeFanout {
			t.Errorf("approve routes to %q, want %q", got, graph.NodeFanout)
		}
	})

	for _, tc := range []struct {
		name string
		opts []nodes.Option
		want string
	}{
		{name: "in place, a winner ends the run", opts: []nodes.Option{nodes.WithFanout()}, want: graph.End},
		{
			name: "on a branch, a winner is promoted (fanout pinned first)",
			opts: []nodes.Option{nodes.WithFanout(), nodes.WithBranchOutput()}, want: graph.NodePromote,
		},
		{
			name: "on a branch, a winner is promoted (fanout pinned last)",
			opts: []nodes.Option{nodes.WithBranchOutput(), nodes.WithFanout()}, want: graph.NodePromote,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			registry, err := nodes.Default(config.Default(), tc.opts...)
			if err != nil {
				t.Fatalf("Default: %v", err)
			}
			if got := runNode(t, registry, graph.NodeJoin, selectedWinner(t)).Next; got != tc.want {
				t.Errorf("join routes a winner to %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCandidateGraph pins the graph one candidate's own run is driven over:
// the work, and nothing a candidate cannot do. No plan and no gate -- the
// parent's plan was approved before any candidate existed -- no fanout of
// its own, and no promote, which is the parent's, after join.
func TestCandidateGraph(t *testing.T) {
	t.Parallel()

	work := []string{graph.NodeCode, graph.NodeWrite, graph.NodeTest, graph.NodeFix, graph.NodeReview}
	guarded := []string{
		graph.NodeCode, graph.NodeWrite, graph.NodePolicy, graph.NodeTest, graph.NodeFix, graph.NodeReview,
	}

	for _, tc := range []struct {
		name string
		opts []nodes.Option
		want []string
	}{
		{name: "an unguarded candidate", want: work},
		{name: "a candidate held to a policy", opts: []nodes.Option{nodes.WithPolicyGate()}, want: guarded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := nodes.CandidateRoute(tc.opts...); !slices.Equal(got, tc.want) {
				t.Fatalf("CandidateRoute() = %v, want %v", got, tc.want)
			}
			registry, err := nodes.Candidate(tc.opts...)
			if err != nil {
				t.Fatalf("Candidate: %v", err)
			}
			if got, want := registry.Names(), slices.Sorted(slices.Values(tc.want)); !slices.Equal(got, want) {
				t.Errorf("Candidate registers %v, want exactly %v", got, want)
			}
			// A candidate's passing gate ends the candidate's run. Promoting
			// is the parent's, once join has chosen.
			if got := runNode(t, registry, graph.NodeReview, passingGate(t)).Next; got != graph.End {
				t.Errorf("a candidate's passing gate routes to %q, want %q", got, graph.End)
			}
		})
	}

	// An option that adds a node a candidate cannot run is refused rather
	// than wired: a candidate whose review routed to a promote node it does
	// not have would end every candidate on an unknown node, after paying
	// for all of its work.
	for _, tc := range []struct {
		name string
		opt  nodes.Option
	}{
		{name: "branch output", opt: nodes.WithBranchOutput()},
		{name: "a fanout of its own", opt: nodes.WithFanout()},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := nodes.Candidate(tc.opt); err == nil {
				t.Error("Candidate accepted an option no candidate run can carry")
			}
		})
	}
}

// TestFanoutIsReachableWhenPinned is the finding this slice exists to fix,
// reproduced and then closed: a real dispatcher over the graph a pinned
// fanout run gets walks plan, fanout and join, in that order, and never
// code.
//
// Before slice 7g nothing could route into fanout -- plan and the gate both
// named code -- so a run with fanout.enabled took one attempt at its goal
// and said nothing. Every candidate here is a real candidate run, driven by
// the dispatcher's own runner; only its work is a stand-in, because what is
// under test is the parent's walk.
func TestFanoutIsReachableWhenPinned(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Graph.Approval = false
	cfg.Fanout.Candidates = 2
	cfg.Budget.MaxUSD = 2.00

	ws := t.TempDir()
	const runID = "20260101T000000Z-fa0007000000"
	layout, err := state.NewLayout(ws, runID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	store := state.NewStore(layout)
	man := state.NewManifest(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), runID, ws, "ship it", cfg)
	man.SetFanout(state.FanoutPin{Candidates: 2, Select: string(config.SelectFewestIssues)})
	if err := store.SaveManifest(man); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	if err := store.SaveState(state.NewState("ship it")); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	registry, err := nodes.Default(cfg, nodes.WithFanout())
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	// The candidates' work: one step that passes its tests and ends the
	// candidate run.
	candidates := graph.NewRegistry()
	candidates.MustRegister(passesItsTests{})
	iso := &belaytest.FakeIsolator{
		CreateFunc: func(_ context.Context, _, id string) (belay.Workspace, error) {
			dir := filepath.Join(layout.CandidatesDir(), id)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return belay.Workspace{}, err
			}
			return belay.Workspace{ID: id, Dir: dir, Ephemeral: true}, nil
		},
	}
	agent := &belaytest.FakeAgent{Responses: []belay.AgentResponse{{
		Text: "# Plan\n\nDo the thing.\n", Usage: belay.Usage{USD: 0.01, InputTokens: 10, OutputTokens: 5},
	}}}

	d, err := graph.NewDispatcher(graph.Options{
		Store: store, Registry: registry, Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agent:  agent, Isolator: iso,
		CandidateRegistry: candidates, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	out, err := d.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v (note %q)", err, out.Note)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Status = %v, want completed (note %q)", out.Status, out.Note)
	}

	res, err := journal.ReadFile(layout.JournalPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var walked []string
	for _, r := range res.Records {
		if r.Event == journal.EventNodeStarted {
			walked = append(walked, r.Node)
		}
	}
	if want := []string{graph.NodePlan, graph.NodeFanout, graph.NodeJoin}; !slices.Equal(walked, want) {
		t.Errorf("the parent walked %v, want %v", walked, want)
	}
	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(st.Candidates) != 2 {
		t.Fatalf("Candidates = %+v, want both candidates recorded", st.Candidates)
	}
	if st.Winner != "cand-01" {
		t.Errorf("Winner = %q, want cand-01: both passed with no issues, and a tie goes to the lowest id", st.Winner)
	}
}

// passesItsTests is a candidate's whole run: it records a passing test run
// and ends the candidate.
type passesItsTests struct{}

func (passesItsTests) Name() string { return graph.NodeCode }

func (passesItsTests) Run(context.Context, *graph.RunContext) (graph.Result, error) {
	return graph.Result{
		Next: graph.End, Status: journal.StatusOK,
		Patch: state.Patch{Test: &state.Test{Total: 3, Passed: 3}},
		Usage: belay.Usage{USD: 0.05, InputTokens: 50, OutputTokens: 10},
	}, nil
}

// planContext is a run context the plan and approve nodes can run in.
func planContext(t *testing.T, cfg config.Config) *graph.RunContext {
	t.Helper()
	layout, err := state.NewLayout(t.TempDir(), "20260101T000000Z-c0ffee000000")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	return &graph.RunContext{
		Goal: "ship it", State: state.NewState("ship it"), Config: cfg, Layout: layout,
		Workspace: layout.WorkspaceDir(), RunID: layout.RunID(),
		NodeName: graph.NodePlan, Step: 1, Attempt: 1,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agent: &belaytest.FakeAgent{Responses: []belay.AgentResponse{{Text: "# Plan\n\nDo it.\n"}}},
	}
}

// writePlan puts an approvable plan where the blackboard says it is.
func writePlan(t *testing.T, rc *graph.RunContext) {
	t.Helper()
	path, err := rc.WriteArtifact("plan.md", []byte("# Plan\n\nDo it.\n"))
	if err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	rc.State.Plan.Path = path
}
