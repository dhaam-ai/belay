package nodes_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// TestRouteMatchesRegistry keeps the route belay shows a person and the graph
// the dispatcher walks from drifting apart. It moved here from internal/cli
// with the route itself, so that each change to the graph adds a row here
// rather than a second route function somewhere else.
func TestRouteMatchesRegistry(t *testing.T) {
	t.Parallel()

	// The route every release so far has walked and shown, with the gate the
	// default configuration asks for.
	standard := []string{
		graph.NodePlan, graph.NodeApprove, graph.NodeCode,
		graph.NodeWrite, graph.NodeTest, graph.NodeFix, graph.NodeReview,
	}
	fanoutOn := config.Default()
	fanoutOn.Fanout.Enabled = true
	fanoutOn.Fanout.Candidates = 3
	// graph.approval false: the plan node routes straight to code, so the
	// gate is not on the walk. It stays registered — a re-run of a run
	// started with the gate must never route to a node nobody registered.
	approvalOff := config.Default()
	approvalOff.Graph.Approval = false

	for _, tc := range []struct {
		name string
		cfg  config.Config
		want []string
		// offRoute are registered but not on the route: nothing this run
		// walks routes into them.
		offRoute []string
	}{
		{name: "defaults", cfg: config.Default(), want: standard},
		{
			// The file alone registers nothing: whether a run fans out is
			// its manifest's pin, which reaches Default as WithFanout.
			// fanout_wire_test.go has the rows for a run that has the pin.
			name: "fanout.enabled in the file alone", cfg: fanoutOn, want: standard,
		},
		{
			name: "graph.approval false", cfg: approvalOff,
			want: []string{
				graph.NodePlan, graph.NodeCode, graph.NodeWrite,
				graph.NodeTest, graph.NodeFix, graph.NodeReview,
			},
			offRoute: []string{graph.NodeApprove},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := nodes.Route(tc.cfg)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Route() = %v, want %v", got, tc.want)
			}
			if got[0] != journal.FirstNode {
				t.Errorf("the route starts at %q but a run starts at %q", got[0], journal.FirstNode)
			}

			registry, err := nodes.Default(tc.cfg)
			if err != nil {
				t.Fatalf("Default: %v", err)
			}
			for _, name := range got {
				if _, err := registry.Get(name); err != nil {
					t.Errorf("the route names %q, which Default does not register", name)
				}
			}
			// The other direction: a registered node that is neither on
			// the route nor known to be off it is one a person is never
			// shown and a run may still reach.
			registered := registry.Names()
			slices.Sort(registered)
			accounted := slices.Concat(got, tc.offRoute)
			slices.Sort(accounted)
			if !slices.Equal(registered, accounted) {
				t.Errorf("Default registers %v; the route and the off-route nodes account for %v", registered, accounted)
			}
		})
	}
}

// TestBranchOutputWiresPromote: a run that delivers its result on a branch
// has one more node than one that edits in place, and two edges that lead
// to it. The route a person is shown and the registry the dispatcher
// resolves names through must gain it together.
func TestBranchOutputWiresPromote(t *testing.T) {
	t.Parallel()

	branch := []string{
		graph.NodePlan, graph.NodeApprove, graph.NodeCode, graph.NodeWrite,
		graph.NodeTest, graph.NodeFix, graph.NodeReview, graph.NodePromote,
	}
	fanoutOn := config.Default()
	fanoutOn.Fanout.Enabled = true
	fanoutOn.Fanout.Candidates = 3

	for _, tc := range []struct {
		name     string
		cfg      config.Config
		want     []string
		offRoute []string
		wantLen  int
	}{
		{name: "branch output", cfg: config.Default(), want: branch, wantLen: 8},
		{
			// fanout.enabled in the file adds nothing to a branch run
			// either; a pinned fanout with branch output is in
			// fanout_wire_test.go.
			name: "branch output with fanout.enabled in the file", cfg: fanoutOn, want: branch, wantLen: 8,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := nodes.Route(tc.cfg, nodes.WithBranchOutput())
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Route() = %v, want %v", got, tc.want)
			}

			registry, err := nodes.Default(tc.cfg, nodes.WithBranchOutput())
			if err != nil {
				t.Fatalf("Default: %v", err)
			}
			if registry.Len() != tc.wantLen {
				t.Errorf("Len() = %d, want %d: %v", registry.Len(), tc.wantLen, registry.Names())
			}
			registered := registry.Names()
			slices.Sort(registered)
			accounted := slices.Concat(got, tc.offRoute)
			slices.Sort(accounted)
			if !slices.Equal(registered, accounted) {
				t.Errorf("Default registers %v; the route and the off-route nodes account for %v", registered, accounted)
			}
		})
	}
}

// TestBranchOutputRoutesEveryEdgeIntoPromote: registering the node is half
// of it. Nothing routes to promote unless the nodes that finish a run are
// told to, and a run that reached promote through neither edge would end
// without committing anything it did.
func TestBranchOutputRoutesEveryEdgeIntoPromote(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	registry, err := nodes.Default(cfg, nodes.WithBranchOutput())
	if err != nil {
		t.Fatalf("Default: %v", err)
	}

	// The quality gate's passing edge.
	reviewed := runNode(t, registry, graph.NodeReview, passingGate(t))
	if reviewed.Next != graph.NodePromote {
		t.Errorf("a passing gate routes to %q, want %q", reviewed.Next, graph.NodePromote)
	}

	// The winner's edge, which only a run that fans out has. Its review is
	// the candidates', not its own, so this is a second graph.
	fanned, err := nodes.Default(cfg, nodes.WithBranchOutput(), nodes.WithFanout())
	if err != nil {
		t.Fatalf("Default with the fanout pinned: %v", err)
	}
	joined := runNode(t, fanned, graph.NodeJoin, selectedWinner(t))
	if joined.Next != graph.NodePromote {
		t.Errorf("a selected winner routes to %q, want %q", joined.Next, graph.NodePromote)
	}
}

// TestInPlaceOutputIsUnchanged: branch output is opt-in, and nothing selects
// it yet. A run built without the option must be exactly the run belay has
// always had -- no promote node, a passing gate ending the run, and a winner
// promoted by hand.
func TestInPlaceOutputIsUnchanged(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	registry, err := nodes.Default(cfg)
	if err != nil {
		t.Fatalf("Default: %v", err)
	}

	if _, err := registry.Get(graph.NodePromote); err == nil {
		t.Error("promote is registered in a run that was not given WithBranchOutput")
	}
	if slices.Contains(nodes.Route(cfg), graph.NodePromote) {
		t.Error("promote is on the route of a run that was not given WithBranchOutput")
	}
	if got := runNode(t, registry, graph.NodeReview, passingGate(t)).Next; got != graph.End {
		t.Errorf("a passing gate routes to %q, want %q", got, graph.End)
	}

	// A fanout run in place: join is there, and its winner ends the run.
	fanned, err := nodes.Default(cfg, nodes.WithFanout())
	if err != nil {
		t.Fatalf("Default with the fanout pinned: %v", err)
	}
	if _, err := fanned.Get(graph.NodePromote); err == nil {
		t.Error("promote is registered in a fanout run that was not given WithBranchOutput")
	}
	if got := runNode(t, fanned, graph.NodeJoin, selectedWinner(t)).Next; got != graph.End {
		t.Errorf("a selected winner routes to %q, want %q", got, graph.End)
	}
}

// runNode executes one registered node against rc, which is how these tests
// see the edge a node really takes rather than the edge the wiring meant.
func runNode(t *testing.T, registry *graph.Registry, name string, rc *graph.RunContext) graph.Result {
	t.Helper()
	node, err := registry.Get(name)
	if err != nil {
		t.Fatalf("Get(%q): %v", name, err)
	}
	res, err := node.Run(t.Context(), rc)
	if err != nil {
		t.Fatalf("%s.Run: %v", name, err)
	}
	return res
}

// passingGate is a run context whose quality gate passes.
func passingGate(t *testing.T) *graph.RunContext {
	t.Helper()
	ws := t.TempDir()
	layout, err := state.NewLayout(ws, "20260101T000000Z-c0ffee000000")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	cfg := config.Default()
	cfg.Review.Mode = config.ReviewModeLint
	return &graph.RunContext{
		Goal: "ship it", State: state.NewState("ship it"), Config: cfg, Layout: layout,
		Workspace: layout.WorkspaceDir(), RunID: layout.RunID(),
		NodeName: graph.NodeReview, Step: 7, Attempt: 1,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Linter: &belaytest.FakeLinter{
			NameValue: "golangci-lint",
			Responses: []belay.QualityReport{{
				Source: "golangci-lint", Gate: belay.GatePass, Issues: []belay.Issue{},
			}},
		},
	}
}

// selectedWinner is a run context whose candidates give join a winner.
func selectedWinner(t *testing.T) *graph.RunContext {
	t.Helper()
	cfg := config.Default()
	cfg.Fanout.Enabled = true
	cfg.Fanout.Candidates = 2
	st := state.NewState("ship it")
	st.Candidates = []state.Candidate{
		{ID: "c1", Dir: "/work/c1", TestPassed: true, IssueCount: 3, Ephemeral: true},
		{ID: "c2", Dir: "/work/c2", TestPassed: true, IssueCount: 1, Ephemeral: true},
	}
	return &graph.RunContext{
		Goal: "ship it", State: st, Config: cfg, RunID: "20260101T000000Z-c0ffee000000",
		NodeName: graph.NodeJoin, Step: 8, Attempt: 1,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// Every name a node can route to must be registered. The dispatcher resolves
// Result.Next by exact string match, so an unregistered target is not a
// compile error -- it is a run that dies partway through with nothing to show
// for what it already spent.
func TestEveryRoutableNameIsRegistered(t *testing.T) {
	t.Parallel()

	r, err := nodes.Default(config.Default())
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	for _, name := range []string{
		graph.NodePlan, graph.NodeApprove, graph.NodeCode,
		graph.NodeWrite, graph.NodeTest, graph.NodeFix, graph.NodeReview,
	} {
		if _, err := r.Get(name); err != nil {
			t.Errorf("Get(%q): %v", name, err)
		}
	}
	if got, want := r.Len(), 7; got != want {
		t.Errorf("Len() = %d, want %d: %v", got, want, r.Names())
	}
}

// The entry point the resume path picks for a fresh run must exist in the
// registry, or every first run fails immediately.
func TestEntryPointIsRegistered(t *testing.T) {
	t.Parallel()

	r, err := nodes.Default(config.Default())
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if _, err := r.Get(graph.NodePlan); err != nil {
		t.Fatalf("the graph does not register its own entry point: %v", err)
	}
}

// A run that fans out must carry both halves of best-of-N. Registering fanout
// without join would create N candidate runs, spend N times the budget, and
// then die routing to a node nobody registered. The pin decides it, not
// fanout.enabled, and the run's own graph is the two halves and the plan in
// front of them: the work is the candidates'.
func TestFanoutRegistersBothHalves(t *testing.T) {
	t.Parallel()

	r, err := nodes.Default(config.Default(), nodes.WithFanout())
	if err != nil {
		t.Fatalf("Default with the fanout pinned: %v", err)
	}
	for _, name := range []string{graph.NodeFanout, graph.NodeJoin} {
		if _, err := r.Get(name); err != nil {
			t.Errorf("Get(%q): %v", name, err)
		}
	}
	if got, want := r.Len(), 4; got != want {
		t.Errorf("Len() = %d, want %d (plan, approve, fanout, join): %v", got, want, r.Names())
	}
}

// A node that is implemented but unreachable is dead weight the graph cannot
// use; a name registered twice would depend on registration order. Neither
// should be possible.
func TestNoUnreachableOrDuplicateNodes(t *testing.T) {
	t.Parallel()

	r, err := nodes.Default(config.Default())
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	seen := map[string]bool{}
	for _, n := range r.Names() {
		if seen[n] {
			t.Errorf("%q registered twice", n)
		}
		seen[n] = true
	}
	// With fanout disabled -- the default -- neither half is registered, so
	// a graph cannot route into a best-of-N the user did not ask to pay for.
	for _, absent := range []string{graph.NodeFanout, graph.NodeJoin} {
		if seen[absent] {
			t.Errorf("%q is registered with fanout disabled", absent)
		}
	}
}

// TestPolicyGateWiresTheGateAndTheEdgeIntoIt: a run whose change is held to
// a policy has one more node, between write and test, and one edge leading
// into it. The route a person is shown and the registry the dispatcher
// resolves names through must gain both together, or belay walks a graph it
// is not showing.
func TestPolicyGateWiresTheGateAndTheEdgeIntoIt(t *testing.T) {
	t.Parallel()

	guarded := []string{
		graph.NodePlan, graph.NodeApprove, graph.NodeCode, graph.NodeWrite,
		graph.NodePolicy, graph.NodeTest, graph.NodeFix, graph.NodeReview,
	}
	guardedBranch := append(slices.Clone(guarded), graph.NodePromote)

	for _, tc := range []struct {
		name string
		opts []nodes.Option
		want []string
	}{
		{name: "a guarded run", opts: []nodes.Option{nodes.WithPolicyGate()}, want: guarded},
		{
			// The two options compose whichever way round they are given:
			// one adds a node after write, the other after review.
			name: "a guarded run on a branch",
			opts: []nodes.Option{nodes.WithPolicyGate(), nodes.WithBranchOutput()},
			want: guardedBranch,
		},
		{
			name: "the same two options in the other order",
			opts: []nodes.Option{nodes.WithBranchOutput(), nodes.WithPolicyGate()},
			want: guardedBranch,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := nodes.Route(config.Default(), tc.opts...); !slices.Equal(got, tc.want) {
				t.Fatalf("Route() = %v, want %v", got, tc.want)
			}
			registry, err := nodes.Default(config.Default(), tc.opts...)
			if err != nil {
				t.Fatalf("Default: %v", err)
			}
			registered := registry.Names()
			slices.Sort(registered)
			accounted := slices.Clone(tc.want)
			slices.Sort(accounted)
			if !slices.Equal(registered, accounted) {
				t.Errorf("Default registers %v, the route accounts for %v", registered, accounted)
			}
		})
	}
}

// The edge into the gate is the whole of what makes it reachable. A registry
// that held the gate while write still routed to test would walk past it
// every time, which is exactly the silently-does-nothing failure the gate
// exists to prevent.
func TestThePolicyGateIsReachedFromWrite(t *testing.T) {
	t.Parallel()

	registry, err := nodes.Default(config.Default(), nodes.WithPolicyGate())
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	write, err := registry.Get(graph.NodeWrite)
	if err != nil {
		t.Fatalf("Get(write): %v", err)
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatalf("write a.go: %v", err)
	}
	layout, err := state.NewLayout(ws, "20260101T000000Z-c0ffee000000")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := os.MkdirAll(layout.ArtifactsDir(), 0o750); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	st := state.NewState("ship it")
	st.Code.ChangedFiles = []string{"a.go"}

	res, err := write.Run(context.Background(), &graph.RunContext{
		Goal: "ship it", State: st, Config: config.Default(), Layout: layout, Workspace: ws,
		RunID: layout.RunID(), NodeName: graph.NodeWrite, Step: 4, Attempt: 1,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("write.Run: %v", err)
	}
	if res.Next != graph.NodePolicy {
		t.Fatalf("write routes to %q in a guarded run, want %q", res.Next, graph.NodePolicy)
	}
	if _, err := registry.Get(res.Next); err != nil {
		t.Errorf("write routes to %q, which the registry does not hold: %v", res.Next, err)
	}
}

// A run with no policy is exactly the run belay has always had: no gate in
// the registry, and write still routing to test. Guardrails are opt-in, and
// nothing about the graph of a repository that declares none may change.
func TestAnUnguardedRunHasNoGateAtAll(t *testing.T) {
	t.Parallel()

	registry, err := nodes.Default(config.Default())
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if _, err := registry.Get(graph.NodePolicy); err == nil {
		t.Error("Default registered a policy gate for a run that declared no policy")
	}
	if slices.Contains(nodes.Route(config.Default()), graph.NodePolicy) {
		t.Error("Route shows a policy gate for a run that declared no policy")
	}
}
