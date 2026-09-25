package graph_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// The seam these tests pin is the one the fanout node is written against: a
// node decides what to ask for, hands it to RunContext.CandidateRuns, and
// turns what comes back into a Result. What really runs candidates is a
// later piece of work, so what matters here is that the contract is complete
// enough to write that node — and to table-test it — against a fake, which
// is what miniFanout below is.

// fakeCandidateRunner stands in for the pool the dispatcher builds. It
// records what it was asked for and replays what it was scripted with,
// including in an order no caller may depend on.
type fakeCandidateRunner struct {
	asked    []graph.CandidateSpec
	outcomes []graph.CandidateOutcome
	err      error
}

func (f *fakeCandidateRunner) Run(_ context.Context, specs []graph.CandidateSpec) ([]graph.CandidateOutcome, error) {
	f.asked = specs
	return f.outcomes, f.err
}

// miniFanout is the shape of the node this seam exists for, reduced to the
// three things it does with the runner: refuse when there is none, ask for
// the specs it decided on, and record what comes back by index.
type miniFanout struct{ specs []graph.CandidateSpec }

func (miniFanout) Name() string { return graph.NodeFanout }

func (m miniFanout) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc.CandidateRuns == nil {
		return graph.Result{}, graph.ErrNoCandidateRunner
	}
	outcomes, err := rc.CandidateRuns.Run(ctx, m.specs)
	if err != nil {
		return graph.Result{}, err
	}
	cands := make([]state.Candidate, len(m.specs))
	var total belay.Usage
	for _, o := range outcomes {
		cands[o.Index-1] = state.Candidate{
			ID: o.ID, Dir: o.Workspace.Dir, RunDir: o.RunDir,
			Outcome:    o.Status.String(),
			TestPassed: o.State.Test.Total > 0 && o.State.Test.Failed == 0,
			IssueCount: len(o.State.Review.Issues),
			SpentUSD:   o.Spent.USD, Estimated: o.Spent.Estimated,
			DurationMS: o.DurationMS, Ephemeral: o.Workspace.Ephemeral, Note: o.Note,
		}
		total.USD += o.Spent.USD
	}
	return graph.Result{
		Next: graph.NodeJoin, Status: journal.StatusOK,
		Patch: state.Patch{Candidates: &cands}, Usage: total,
	}, nil
}

// candidateOutcome is a candidate that ended the way its index says, so a
// test can tell one slot from another.
func candidateOutcome(id string, index int, usd float64) graph.CandidateOutcome {
	st := state.NewState("ship it")
	st.Test.Total, st.Test.Passed = index, index
	st.Review.Issues = make([]belay.Issue, index)
	return graph.CandidateOutcome{
		ID: id, Index: index, RunDir: "candidate-runs/" + id,
		Status: state.RunStatusCompleted, State: st,
		Spent:      belay.Usage{USD: usd},
		Envelope:   state.Envelope{LimitUSD: 1.00, LimitTokens: 1000},
		Steps:      3,
		DurationMS: int64(index) * 100,
		Workspace:  belay.Workspace{ID: id, Dir: "/tmp/" + id, Ephemeral: true},
		Note:       id + " finished",
		Provision:  state.ProvisionOK,
	}
}

// candidateSpecs is what miniFanout decided to ask for: three candidates,
// each with its own envelope and the goal and plan alone to work from.
func candidateSpecs() []graph.CandidateSpec {
	specs := make([]graph.CandidateSpec, 0, 3)
	for _, id := range []string{"cand-01", "cand-02", "cand-03"} {
		specs = append(specs, graph.CandidateSpec{
			ID: id, Index: len(specs) + 1, Source: "/repo",
			Envelope: state.Envelope{LimitUSD: 1.00, LimitTokens: 1000},
			Seed:     state.SeedCandidateState(state.NewState("ship it")),
			Plan:     []byte("# plan\n"),
		})
	}
	return specs
}

func TestACandidateRunnerIsSomethingANodeCanBeTestedAgainst(t *testing.T) {
	specs := candidateSpecs()
	diskFull := errors.New("disk is full")

	tests := []struct {
		name    string
		runner  graph.CandidateRunner
		wantIDs []string
		wantUSD float64
		wantErr error
	}{
		{
			// The node cannot run candidates itself, so a context with no
			// runner is a wiring bug it must name rather than work around.
			name:    "a node with no runner refuses by name",
			runner:  nil,
			wantErr: graph.ErrNoCandidateRunner,
		},
		{
			// The order outcomes come back in is the order goroutines
			// happened to finish. Index, not position, is what fixes a
			// candidate's slot on the blackboard.
			name: "outcomes are recorded by index, not in the order they arrive",
			runner: &fakeCandidateRunner{outcomes: []graph.CandidateOutcome{
				candidateOutcome("cand-03", 3, 0.30),
				candidateOutcome("cand-01", 1, 0.10),
				candidateOutcome("cand-02", 2, 0.20),
			}},
			wantIDs: []string{"cand-01", "cand-02", "cand-03"},
			wantUSD: 0.60,
		},
		{
			name:    "a runner that could not run the candidates reports why",
			runner:  &fakeCandidateRunner{err: diskFull},
			wantErr: diskFull,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := miniFanout{specs: specs}
			rc := &graph.RunContext{Goal: "ship it", CandidateRuns: tt.runner}

			res, err := node.Run(context.Background(), rc)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Run() error = %v, want one matching %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}

			var gotIDs []string
			for _, c := range *res.Patch.Candidates {
				gotIDs = append(gotIDs, c.ID)
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Errorf("candidate ids = %v, want %v", gotIDs, tt.wantIDs)
			}
			if got := (*res.Patch.Candidates)[0]; got.IssueCount != 1 || !got.TestPassed || got.DurationMS != 100 {
				t.Errorf("candidate 1 = %+v, want the outcome whose Index is 1", got)
			}
			if !closeTo(res.Usage.USD, tt.wantUSD) {
				t.Errorf("Result.Usage.USD = %v, want %v", res.Usage.USD, tt.wantUSD)
			}

			// The specs the node decided on reach the runner unchanged:
			// ids, envelopes, seeds and the plan a candidate reads.
			fake, ok := tt.runner.(*fakeCandidateRunner)
			if !ok {
				t.Fatalf("runner is %T, want the fake", tt.runner)
			}
			if !reflect.DeepEqual(fake.asked, specs) {
				t.Errorf("the runner was asked for\n  %+v\nwant\n  %+v", fake.asked, specs)
			}
		})
	}
}

// TestCandidateRunSentinelsAreDistinct pins the two failures a candidate
// runner has to report separately: no runner at all, which is a wiring bug,
// and a directory under candidate-runs/ that belay did not write, which is a
// refusal to touch someone else's files.
func TestCandidateRunSentinelsAreDistinct(t *testing.T) {
	if errors.Is(graph.ErrNoCandidateRunner, graph.ErrCandidateRunCorrupt) {
		t.Error("ErrNoCandidateRunner matches ErrCandidateRunCorrupt; a caller cannot tell the two apart")
	}
	for _, err := range []error{graph.ErrNoCandidateRunner, graph.ErrCandidateRunCorrupt} {
		if !strings.HasPrefix(err.Error(), "graph: ") {
			t.Errorf("%v does not name its package; every sentinel in this package does", err)
		}
	}
}

// ---------------------------------------------------------------------------
// The pool itself: a real parent run, real candidate runs on disk.
//
// Everything below drives graph.Dispatcher's own candidate runner rather than
// a fake, because what these tests are about is the durable shape it leaves
// behind — one run directory, one journal and one manifest per candidate —
// which no fake can stand in for.
// ---------------------------------------------------------------------------

// planArtifact is the name the seed blackboard's plan is filed under, and so
// the name a candidate's code node reads it back by.
const planArtifact = "artifacts/plan.md"

// candidateNode is the one node a test's candidate registry holds. One
// instance serves every candidate goroutine, so it looks up what to do by
// the candidate whose run is executing it, and counts its executions under
// a mutex.
type candidateNode struct {
	name string
	act  func(ctx context.Context, id string, rc *graph.RunContext) (graph.Result, error)

	mu    sync.Mutex
	calls map[string]int
}

func newCandidateNode(
	name string, act func(ctx context.Context, id string, rc *graph.RunContext) (graph.Result, error),
) *candidateNode {
	return &candidateNode{name: name, act: act, calls: map[string]int{}}
}

func (n *candidateNode) Name() string { return n.name }

func (n *candidateNode) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	// A candidate run's Layout is rooted at candidate-runs/<id>, so its
	// RunID is the bare candidate id — which is how one node instance tells
	// the goroutines apart.
	id := rc.Layout.RunID()
	n.mu.Lock()
	n.calls[id]++
	n.mu.Unlock()
	return n.act(ctx, id, rc)
}

// ran reports how many times this node executed for one candidate.
func (n *candidateNode) ran(id string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[id]
}

// finishes is the commonest candidate node: it records a test result and
// ends the candidate's run.
func finishes(usd float64) func(context.Context, string, *graph.RunContext) (graph.Result, error) {
	return func(_ context.Context, id string, _ *graph.RunContext) (graph.Result, error) {
		return Result{
			Next: graph.End, Status: journal.StatusOK,
			Patch: state.Patch{Test: &state.Test{Total: 2, Passed: 2}},
			Usage: belay.Usage{USD: usd, InputTokens: 100, OutputTokens: 20},
			Note:  id + " finished its work",
		}, nil
	}
}

// candidateRegistry is the inner graph a candidate run is driven over. The
// real one is code → write → test → fix → review; a test registers only the
// nodes the run it scripts walks.
func candidateRegistry(t *testing.T, nodes ...graph.Node) *graph.Registry {
	t.Helper()
	reg := graph.NewRegistry()
	reg.MustRegister(nodes...)
	return reg
}

// runsCandidates is the supervising node a pool is reached through: the only
// way any node ever reaches one is RunContext.CandidateRuns.
type runsCandidates struct {
	specs    []graph.CandidateSpec
	outcomes []graph.CandidateOutcome
	err      error
	// before runs just before the pool does, so a test can arrange the disk
	// with the run already under way.
	before func(rc *graph.RunContext)
}

func (*runsCandidates) Name() string { return graph.NodeFanout }

// SupervisesCandidates exempts this node from graph.node_timeout, as the
// real fanout node is exempt.
func (*runsCandidates) SupervisesCandidates() {}

func (n *runsCandidates) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc.CandidateRuns == nil {
		return graph.Result{}, graph.ErrNoCandidateRunner
	}
	if n.before != nil {
		n.before(rc)
	}
	n.outcomes, n.err = rc.CandidateRuns.Run(ctx, n.specs)
	return Result{Next: graph.End, Status: journal.StatusOK, Note: "candidates done"}, nil
}

// fakeIsolator makes a directory per candidate under the parent run's
// candidates/ root, which is where the CLI roots both real isolators.
type fakeIsolator struct {
	root string

	mu        sync.Mutex
	created   []string
	reclaimed []string
	// order records every Create and Reclaim in the order they happened,
	// which is what pins "reclaim strictly before create".
	order []string
	// fail scripts what Create returns for one candidate id, and
	// reclaimFail what Reclaim returns.
	fail        map[string]error
	reclaimFail map[string]error
	// emptyDir makes Create succeed but hand back a Workspace with no
	// directory, which no caller can work in.
	emptyDir map[string]bool
	// block holds Create until the channel for that id is closed.
	block map[string]chan struct{}
}

func newFakeIsolator(root string) *fakeIsolator {
	return &fakeIsolator{
		root: root, fail: map[string]error{}, reclaimFail: map[string]error{},
		emptyDir: map[string]bool{}, block: map[string]chan struct{}{},
	}
}

func (f *fakeIsolator) Create(ctx context.Context, _, id string) (belay.Workspace, error) {
	f.mu.Lock()
	f.created = append(f.created, id)
	f.order = append(f.order, "create "+id)
	err, failing := f.fail[id]
	empty := f.emptyDir[id]
	gate := f.block[id]
	f.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return belay.Workspace{}, ctx.Err()
		}
	}
	if failing {
		return belay.Workspace{}, err
	}
	if empty {
		return belay.Workspace{ID: id, Ephemeral: true}, nil
	}
	dir := filepath.Join(f.root, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return belay.Workspace{}, err
	}
	return belay.Workspace{ID: id, Dir: dir, Ephemeral: true}, nil
}

func (f *fakeIsolator) Destroy(context.Context, belay.Workspace) error { return nil }

// creates reports how many times Create was called for one candidate.
func (f *fakeIsolator) creates(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, seen := range f.created {
		if seen == id {
			n++
		}
	}
	return n
}

// calls returns every Create and Reclaim in the order they happened, as
// "create cand-01" / "reclaim cand-01", so a test can assert the order of
// the two rather than only that both happened.
func (f *fakeIsolator) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

// reclaimingIsolator is a fakeIsolator that also implements graph.Reclaimer,
// which is the optional, consumer-side interface both real isolators satisfy
// structurally.
type reclaimingIsolator struct{ *fakeIsolator }

func (r reclaimingIsolator) Reclaim(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reclaimed = append(r.reclaimed, id)
	r.order = append(r.order, "reclaim "+id)
	return r.reclaimFail[id]
}

// pool is how a test wires the parent dispatcher that owns a candidate
// runner. Every field mirrors one graph.Options field.
type pool struct {
	registry    *graph.Registry
	isolator    belay.Isolator
	concurrency int
	agentFor    func(string) belay.AgentBackend
}

// poolDispatcher builds the parent dispatcher for a run whose fanout node
// reaches a real candidate runner.
func (h *harness) poolDispatcher(t *testing.T, p pool, nodes ...graph.Node) *graph.Dispatcher {
	t.Helper()
	reg := graph.NewRegistry()
	reg.MustRegister(nodes...)
	d, err := graph.NewDispatcher(graph.Options{
		Store: h.store, Registry: reg, Config: h.cfg,
		// The parent run starts at its fanout node: the plan and the
		// approval that come before it in the real graph have nothing to
		// do with what a pool does.
		Entry:             graph.NodeFanout,
		Now:               func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) },
		Isolator:          p.isolator,
		CandidateRegistry: p.registry,
		CandidateAgent:    p.agentFor,
		Concurrency:       p.concurrency,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	return d
}

// poolSpecs is what a fanout node would decide for n candidates of the run
// h describes: stable ids, equal envelopes, and the goal and plan alone to
// work from.
func poolSpecs(h *harness, n int) []graph.CandidateSpec {
	parent := state.NewState("ship it")
	parent.Plan = state.Plan{Path: planArtifact, Approved: true, Digest: "sha256:plan"}
	specs := make([]graph.CandidateSpec, 0, n)
	for i := 1; i <= n; i++ {
		specs = append(specs, graph.CandidateSpec{
			ID: fmt.Sprintf("cand-%02d", i), Index: i,
			Source:   h.layout.WorkspaceDir(),
			Envelope: state.Envelope{LimitUSD: 1.00, LimitTokens: 1000},
			Seed:     state.SeedCandidateState(parent),
			Plan:     []byte("# plan\n\nship it\n"),
		})
	}
	return specs
}

// candidateLayout is one candidate run's Layout under the parent h owns.
func candidateLayout(t *testing.T, h *harness, id string) state.Layout {
	t.Helper()
	l, err := state.NewCandidateRunLayout(h.layout, id)
	if err != nil {
		t.Fatalf("NewCandidateRunLayout(%q) error = %v", id, err)
	}
	return l
}

// candidateRecords reads one candidate's own journal, without healing it:
// journal.Open rewrites a torn tail, and a test asserting what a crash left
// behind must not be the thing that repairs it.
func candidateRecords(t *testing.T, h *harness, id string) []journal.Record {
	t.Helper()
	res, err := journal.ReadFile(candidateLayout(t, h, id).JournalPath())
	if err != nil {
		t.Fatalf("ReadFile(%s journal) error = %v", id, err)
	}
	return res.Records
}

// candidateManifest reads one candidate run's own manifest.
func candidateManifest(t *testing.T, h *harness, id string) state.Manifest {
	t.Helper()
	m, err := state.LoadManifest(candidateLayout(t, h, id).ManifestPath())
	if err != nil {
		t.Fatalf("LoadManifest(%s) error = %v", id, err)
	}
	return m
}

// TestCandidateRunnerGivesEachCandidateARunOfItsOwn is the shape the whole
// slice rests on: a candidate is a full run, with its own directory,
// journal, blackboard, manifest and envelope, driven over the inner graph
// from code, in the workspace its isolator made.
func TestCandidateRunnerGivesEachCandidateARunOfItsOwn(t *testing.T) {
	h := newRun(t, budgetCfg(3.00, 0), "ship it")
	iso := newFakeIsolator(h.layout.CandidatesDir())
	code := newCandidateNode(graph.NodeCode, finishes(0.25))
	node := &runsCandidates{specs: poolSpecs(h, 2)}

	out, err := h.poolDispatcher(t, pool{registry: candidateRegistry(t, code), isolator: iso}, node).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("parent Outcome.Status = %v, want completed (note %q)", out.Status, out.Note)
	}
	if node.err != nil {
		t.Fatalf("CandidateRuns.Run() error = %v, want nil", node.err)
	}
	if len(node.outcomes) != 2 {
		t.Fatalf("got %d outcomes, want 2: %+v", len(node.outcomes), node.outcomes)
	}

	for i, o := range node.outcomes {
		id := fmt.Sprintf("cand-%02d", i+1)
		if o.ID != id || o.Index != i+1 {
			t.Errorf("outcome %d = {%q, %d}, want {%q, %d}", i, o.ID, o.Index, id, i+1)
		}
		if o.Err != nil {
			t.Fatalf("%s outcome carries Err = %v, want nil", id, o.Err)
		}
		if o.Status != state.RunStatusCompleted {
			t.Errorf("%s Status = %v, want completed", id, o.Status)
		}
		if o.Provision != state.ProvisionOK {
			t.Errorf("%s Provision = %q, want %q", id, o.Provision, state.ProvisionOK)
		}
		if want := "candidate-runs/" + id; o.RunDir != want {
			t.Errorf("%s RunDir = %q, want %q", id, o.RunDir, want)
		}
		if o.State.Test.Total != 2 || o.State.Test.Passed != 2 {
			t.Errorf("%s State.Test = %+v, want the candidate's own blackboard", id, o.State.Test)
		}
		if !closeTo(o.Spent.USD, 0.25) {
			t.Errorf("%s Spent.USD = %v, want 0.25 rebuilt from its own journal", id, o.Spent.USD)
		}
		if o.Steps != 1 {
			t.Errorf("%s Steps = %d, want 1", id, o.Steps)
		}
		if o.Envelope != (state.Envelope{LimitUSD: 1.00, LimitTokens: 1000}) {
			t.Errorf("%s Envelope = %+v, want the one it was claimed with", id, o.Envelope)
		}
		if o.Workspace.Dir != filepath.Join(iso.root, id) || !o.Workspace.Ephemeral {
			t.Errorf("%s Workspace = %+v, want the one the isolator made", id, o.Workspace)
		}
		if o.Note == "" {
			t.Errorf("%s Note is empty; a reader would have to open the journal", id)
		}
		if code.ran(id) != 1 {
			t.Errorf("%s ran its code node %d times, want 1", id, code.ran(id))
		}

		// Its own run directory, holding everything a run is made of.
		l := candidateLayout(t, h, id)
		for _, p := range []string{l.ManifestPath(), l.StatePath(), l.JournalPath(),
			filepath.Join(l.ArtifactsDir(), "plan.md")} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s is missing from %s: %v", filepath.Base(p), id, err)
			}
		}
		man := candidateManifest(t, h, id)
		if want := testRunID + "-" + id; man.RunID != want {
			t.Errorf("%s manifest RunID = %q, want %q", id, man.RunID, want)
		}
		if man.Candidate == nil {
			t.Fatalf("%s manifest has no candidate block", id)
		}
		if man.Candidate.ParentRunID != testRunID || man.Candidate.Index != i+1 {
			t.Errorf("%s candidate block = %+v, want parent %q index %d", id, *man.Candidate, testRunID, i+1)
		}
		if man.Candidate.Envelope.LimitUSD != 1.00 || man.Candidate.Envelope.LimitTokens != 1000 {
			t.Errorf("%s persisted envelope = %+v, want the claim's", id, man.Candidate.Envelope)
		}
		if man.Budget.LimitUSD != 1.00 {
			t.Errorf("%s manifest budget ceiling = %v, want its envelope 1.00", id, man.Budget.LimitUSD)
		}
	}
}

// startGate lets a test hold every candidate inside its node and release
// them one at a time, so how many run together is something the test
// observes rather than times.
type startGate struct {
	started chan string
	done    chan string

	mu      sync.Mutex
	release map[string]chan struct{}
	live    int
	peak    int
}

func newStartGate(t *testing.T, ids ...string) *startGate {
	g := &startGate{
		started: make(chan string, len(ids)), done: make(chan string, len(ids)),
		release: make(map[string]chan struct{}, len(ids)),
	}
	for _, id := range ids {
		g.release[id] = make(chan struct{})
	}
	// However the test ends, nothing is left blocked inside a node.
	t.Cleanup(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		for _, ch := range g.release {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	})
	return g
}

// hold is the candidate node's body: announce, wait to be let go, announce
// again.
func (g *startGate) hold(_ context.Context, id string, _ *graph.RunContext) (graph.Result, error) {
	g.mu.Lock()
	g.live++
	if g.live > g.peak {
		g.peak = g.live
	}
	ch := g.release[id]
	g.mu.Unlock()

	g.started <- id
	<-ch

	g.mu.Lock()
	g.live--
	g.mu.Unlock()
	g.done <- id

	return Result{
		Next: graph.End, Status: journal.StatusOK,
		Patch: state.Patch{Test: &state.Test{Total: 1, Passed: 1}},
		Note:  id + " finished",
	}, nil
}

// awaitStart waits for the next candidate to reach its node, and fails the
// test rather than hanging when none does.
func (g *startGate) awaitStart(t *testing.T) string {
	t.Helper()
	select {
	case id := <-g.started:
		return id
	case <-time.After(10 * time.Second):
		g.mu.Lock()
		peak := g.peak
		g.mu.Unlock()
		t.Fatalf("no further candidate reached its node; at most %d ran at once", peak)
		return ""
	}
}

// let releases one candidate and waits for it to leave its node, so the
// order candidates finish in is the test's to choose.
func (g *startGate) let(t *testing.T, id string) {
	t.Helper()
	g.mu.Lock()
	ch := g.release[id]
	g.mu.Unlock()
	close(ch)
	select {
	case <-g.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("candidate %q never left its node after it was released", id)
	}
}

// TestCandidateRunnerHonoursItsConcurrencyLimit: candidates run several at a
// time, never more than the limit, and are admitted in index order — and an
// outcome lands in its own slot however the goroutines happen to finish, so
// what the node sees does not depend on which candidate was quickest.
func TestCandidateRunnerHonoursItsConcurrencyLimit(t *testing.T) {
	h := newRun(t, budgetCfg(5.00, 0), "five ways to ship it")
	ids := []string{"cand-01", "cand-02", "cand-03", "cand-04", "cand-05"}
	gate := newStartGate(t, ids...)
	code := newCandidateNode(graph.NodeCode, gate.hold)
	node := &runsCandidates{specs: poolSpecs(h, 5)}
	d := h.poolDispatcher(t, pool{
		registry: candidateRegistry(t, code), isolator: newFakeIsolator(h.layout.CandidatesDir()),
		concurrency: 2,
	}, node)

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if _, err := d.Run(context.Background()); err != nil {
			t.Errorf("Run() error = %v, want nil", err)
		}
	}()

	// Two at a time, in index order, each batch released highest id first
	// so that the order they finish in is never the order of their slots.
	var admitted, completed []string
	for done := 0; done < len(ids); {
		batch := []string{gate.awaitStart(t)}
		if done+2 <= len(ids) {
			batch = append(batch, gate.awaitStart(t))
		}
		sort.Strings(batch)
		admitted = append(admitted, batch...)
		for i := len(batch) - 1; i >= 0; i-- {
			gate.let(t, batch[i])
			completed = append(completed, batch[i])
		}
		done += len(batch)
	}

	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("Run() never returned after every candidate finished")
	}

	if gate.peak != 2 {
		t.Errorf("at most %d candidates ran at once, want exactly the limit of 2", gate.peak)
	}
	if !equalStrings(admitted, ids) {
		t.Errorf("candidates were admitted as %v, want index order %v", admitted, ids)
	}
	if want := []string{"cand-02", "cand-01", "cand-04", "cand-03", "cand-05"}; !equalStrings(completed, want) {
		t.Fatalf("candidates finished as %v, want %v; the test did not arrange what it meant to", completed, want)
	}

	var got []string
	for _, o := range node.outcomes {
		if o.Err != nil {
			t.Fatalf("%s outcome carries Err = %v, want nil", o.ID, o.Err)
		}
		got = append(got, o.ID)
	}
	if !equalStrings(got, ids) {
		t.Errorf("outcomes came back as %v, want them in index order %v although they finished as %v",
			got, ids, completed)
	}
}

// TestEveryCandidateIsClaimedBeforeAnyOfThemRuns: the split between claiming
// and running is what makes the envelope division a single, one-shot
// computation — nothing later re-divides the parent's remaining ceiling — and
// what makes a Ctrl-C find every candidate durable, including the ones that
// never reached a goroutine.
func TestEveryCandidateIsClaimedBeforeAnyOfThemRuns(t *testing.T) {
	h := newRun(t, budgetCfg(3.00, 0), "claim first, run second")
	ids := []string{"cand-01", "cand-02", "cand-03"}

	var mu sync.Mutex
	sawAll := map[string]bool{}
	code := newCandidateNode(graph.NodeCode, func(_ context.Context, id string, _ *graph.RunContext) (graph.Result, error) {
		all := true
		for _, other := range ids {
			if _, err := os.Stat(candidateLayout(t, h, other).ManifestPath()); err != nil {
				all = false
			}
		}
		mu.Lock()
		sawAll[id] = all
		mu.Unlock()
		return Result{Next: graph.End, Status: journal.StatusOK}, nil
	})

	node := &runsCandidates{specs: poolSpecs(h, 3)}
	if _, err := h.poolDispatcher(t, pool{
		registry: candidateRegistry(t, code), isolator: newFakeIsolator(h.layout.CandidatesDir()),
		concurrency: 1,
	}, node).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	for _, id := range ids {
		if !sawAll[id] {
			t.Errorf("when %s ran, some candidate had not been claimed yet", id)
		}
	}
}

// TestACandidateThatCannotBeClaimedStopsTheFanout: claiming is cheap and
// spends nothing, so a fanout that cannot be whole is refused before it
// costs anything. The candidates that were claimed keep their directories,
// so a resume adopts them rather than paying for them twice.
func TestACandidateThatCannotBeClaimedStopsTheFanout(t *testing.T) {
	h := newRun(t, budgetCfg(3.00, 0), "refuse a fanout that cannot be whole")

	// cand-03's run directory cannot be created: something that is not a
	// directory is already in its place.
	if err := os.MkdirAll(h.layout.CandidateRunsDir(), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	blocked := filepath.Join(h.layout.CandidateRunsDir(), "cand-03")
	if err := os.WriteFile(blocked, []byte("not a run directory\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	code := newCandidateNode(graph.NodeCode, finishes(0.25))
	iso := newFakeIsolator(h.layout.CandidatesDir())
	node := &runsCandidates{specs: poolSpecs(h, 3)}

	if _, err := h.poolDispatcher(t, pool{
		registry: candidateRegistry(t, code), isolator: iso, concurrency: 3,
	}, node).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if node.err != nil {
		t.Fatalf("CandidateRuns.Run() error = %v; a fault one candidate hit is reported on its outcome", node.err)
	}

	for _, id := range []string{"cand-01", "cand-02"} {
		if code.ran(id) != 0 {
			t.Errorf("%s ran its code node %d times, want 0: no candidate runs when one cannot be claimed", id, code.ran(id))
		}
		if iso.creates(id) != 0 {
			t.Errorf("%s had a workspace made for it; nothing should have been provisioned", id)
		}
		// Claimed all the same, so a resume adopts it instead of paying
		// for a second one beside it.
		if _, err := os.Stat(candidateLayout(t, h, id).ManifestPath()); err != nil {
			t.Errorf("%s was not claimed: %v", id, err)
		}
	}

	for i, o := range node.outcomes {
		switch o.ID {
		case "cand-03":
			if o.Err == nil {
				t.Errorf("cand-03 outcome carries no Err, so nothing names the fault a resume has to retry")
			}
		default:
			if o.Err != nil {
				t.Errorf("%s outcome carries Err = %v, want nil: it was claimed, it simply did not run", o.ID, o.Err)
			}
			if o.Status != state.RunStatusUnknown || o.Provision != state.ProvisionPending {
				t.Errorf("%s = {%v, %q}, want a candidate that is claimed and no further", o.ID, o.Status, o.Provision)
			}
		}
		if o.Index != i+1 {
			t.Errorf("outcome %d has Index %d; every slot is still identified", i, o.Index)
		}
	}

	// And it was left exactly where it was, not deleted.
	if data, err := os.ReadFile(blocked); err != nil || string(data) != "not a run directory\n" {
		t.Errorf("the file in cand-03's place reads %q (err %v), want it untouched", data, err)
	}
}
