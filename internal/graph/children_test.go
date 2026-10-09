package graph_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/procgroup"
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

func (f *fakeCandidateRunner) Claimed([]string) (bool, error) { return f.asked != nil, nil }

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
	// rounds is how many times to ask the pool for the same candidates,
	// which is what a resume does: the second round finds on disk whatever
	// the first left. Zero means once.
	rounds int
	// between runs before every round after the first, so a test can mend
	// whatever made the first round fail.
	between func(round int)
	// results holds every round's outcomes; outcomes is the last one's.
	results [][]graph.CandidateOutcome
	// digest, when set, is called immediately before and immediately after
	// the pool runs, so a test can prove nothing in the parent run changed
	// while it did. The parent's own files are quiet for exactly this
	// window: the dispatcher appends node_started before the node and
	// node_finished after it.
	digest       func() map[string]string
	digestBefore map[string]string
	digestAfter  map[string]string
}

func (*runsCandidates) Name() string { return graph.NodeFanout }

// SupervisesCandidates exempts this node from graph.node_timeout, as the
// real fanout node is exempt.
func (*runsCandidates) SupervisesCandidates() {}

func (n *runsCandidates) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc.CandidateRuns == nil {
		return graph.Result{}, graph.ErrNoCandidateRunner
	}
	if n.digest != nil {
		n.digestBefore = n.digest()
	}
	for round := 1; round <= max(n.rounds, 1); round++ {
		if round > 1 && n.between != nil {
			n.between(round)
		}
		n.outcomes, n.err = rc.CandidateRuns.Run(ctx, n.specs)
		n.results = append(n.results, n.outcomes)
		if n.err != nil {
			break
		}
	}
	if n.digest != nil {
		n.digestAfter = n.digest()
	}
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
	registry      *graph.Registry
	isolator      belay.Isolator
	concurrency   int
	agentFor      func(string) belay.AgentBackend
	pauseOnCancel bool
	changes       func(*state.Manifest, state.Layout) graph.ChangeSource
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
		PauseOnCancel:     p.pauseOnCancel,
		Changes:           p.changes,
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
	if data := readFile(t, blocked); string(data) != "not a run directory\n" {
		t.Errorf("the file in cand-03's place reads %q, want it untouched", data)
	}
}

// seeded describes the candidate run directory a previous attempt of the
// fanout node left behind, so a test can start from the disk a crash
// produced rather than from one this code wrote.
type seeded struct {
	id        string
	index     int
	provision state.Provision
	note      string
	envelope  state.Envelope
	// status is what the manifest says, which after a kill between the run
	// marker and SaveManifest may not be what the journal says.
	status  state.RunStatus
	step    int
	records []journal.Record
	// blackboard edits the state.json this candidate had already reached.
	blackboard func(*state.State)
}

// seedCandidate writes one candidate run directory as a previous attempt
// would have left it.
func seedCandidate(t *testing.T, h *harness, iso *fakeIsolator, s seeded) state.Layout {
	t.Helper()
	l := candidateLayout(t, h, s.id)
	if err := os.MkdirAll(l.RunDir(), 0o750); err != nil {
		t.Fatalf("MkdirAll(%s): %v", l.RunDir(), err)
	}

	st := state.NewState("ship it")
	st.Plan = state.Plan{Path: planArtifact, Approved: true, Digest: "sha256:plan"}
	if s.blackboard != nil {
		s.blackboard(&st)
	}
	if err := state.SaveState(l.StatePath(), st); err != nil {
		t.Fatalf("SaveState(%s): %v", s.id, err)
	}

	abs, err := l.ArtifactPath("plan.md")
	if err != nil {
		t.Fatalf("ArtifactPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		t.Fatalf("MkdirAll(artifacts): %v", err)
	}
	if err := os.WriteFile(abs, []byte("# plan\n\nship it\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(plan): %v", err)
	}

	if len(s.records) > 0 {
		j, err := journal.Open(l.JournalPath())
		if err != nil {
			t.Fatalf("journal.Open(%s): %v", s.id, err)
		}
		for _, rec := range s.records {
			if _, err := j.Append(rec); err != nil {
				t.Fatalf("Append(%v) to %s: %v", rec.Event, s.id, err)
			}
		}
		if err := j.Close(); err != nil {
			t.Fatalf("journal.Close(%s): %v", s.id, err)
		}
	}

	var ws belay.Workspace
	if s.provision == state.ProvisionOK {
		dir := filepath.Join(iso.root, s.id)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("MkdirAll(workspace): %v", err)
		}
		ws = belay.Workspace{ID: s.id, Dir: dir, Ephemeral: true}
	}

	cfg := h.cfg
	cfg.Budget.MaxUSD, cfg.Budget.MaxTokens = s.envelope.LimitUSD, s.envelope.LimitTokens
	man := state.NewManifest(
		time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC),
		testRunID+"-"+s.id, h.layout.WorkspaceDir(), "ship it", cfg)
	man.Status, man.Step = s.status, s.step
	man.SetCandidate(state.CandidateRun{
		ParentRunID: testRunID, ID: s.id, Index: s.index, Workspace: ws,
		Provision: s.provision, ProvisionNote: s.note, Envelope: s.envelope,
	})
	if err := state.SaveManifest(l.ManifestPath(), man); err != nil {
		t.Fatalf("SaveManifest(%s): %v", s.id, err)
	}
	return l
}

// oneEnvelope is the envelope every seeded candidate in these tests holds.
var oneEnvelope = state.Envelope{LimitUSD: 1.00, LimitTokens: 1000}

// TestTerminalCandidatesAreAdoptedAndInterruptedOnesResume is the resume the
// whole slice exists for: the parent died with one candidate finished, one
// failed and one inside a node. Adoption is what makes that recoverable —
// the finished ones execute nothing and keep their own blackboards, and only
// the interrupted one runs, from the node it died in.
func TestTerminalCandidatesAreAdoptedAndInterruptedOnesResume(t *testing.T) {
	h := newRun(t, budgetCfg(3.00, 0), "resume a half-finished fanout")
	iso := newFakeIsolator(h.layout.CandidatesDir())

	// cand-01 completed, and its blackboard holds what it achieved.
	seedCandidate(t, h, iso, seeded{
		id: "cand-01", index: 1, provision: state.ProvisionOK, envelope: oneEnvelope,
		status: state.RunStatusCompleted, step: 1,
		blackboard: func(st *state.State) { st.Test = state.Test{Total: 7, Passed: 7} },
		records: []journal.Record{
			{Event: journal.EventRunStarted},
			{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1},
			{Event: journal.EventNodeFinished, Node: graph.NodeCode, Attempt: 1,
				Status: journal.StatusOK, Next: graph.End,
				Usage: &journal.Usage{USD: 0.40}, Note: "cand-01 is done"},
			{Event: journal.EventRunCompleted, Note: "cand-01 is done"},
		},
	})
	// cand-02 failed, and a kill between the run marker and SaveManifest
	// left its manifest still saying "running".
	seedCandidate(t, h, iso, seeded{
		id: "cand-02", index: 2, provision: state.ProvisionOK, envelope: oneEnvelope,
		status: state.RunStatusRunning, step: 1,
		records: []journal.Record{
			{Event: journal.EventRunStarted},
			{Event: journal.EventNodeStarted, Node: graph.NodeTest, Attempt: 1},
			{Event: journal.EventNodeFinished, Node: graph.NodeTest, Attempt: 1,
				Status: journal.StatusFailed, Usage: &journal.Usage{USD: 0.30}, Note: "the fix loop gave up"},
			{Event: journal.EventRunFailed, Note: "the fix loop gave up"},
		},
	})
	// cand-03 was inside test when the process died.
	seedCandidate(t, h, iso, seeded{
		id: "cand-03", index: 3, provision: state.ProvisionOK, envelope: oneEnvelope,
		status: state.RunStatusRunning, step: 1,
		records: []journal.Record{
			{Event: journal.EventRunStarted},
			{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1},
			{Event: journal.EventNodeFinished, Node: graph.NodeCode, Attempt: 1,
				Status: journal.StatusOK, Next: graph.NodeTest, Usage: &journal.Usage{USD: 0.20}},
			{Event: journal.EventNodeStarted, Node: graph.NodeTest, Attempt: 1},
		},
	})

	code := newCandidateNode(graph.NodeCode, finishes(0.25))
	var mu sync.Mutex
	attempts := map[string]int{}
	testNode := newCandidateNode(graph.NodeTest, func(_ context.Context, id string, rc *graph.RunContext) (graph.Result, error) {
		mu.Lock()
		attempts[id] = rc.Attempt
		mu.Unlock()
		return Result{
			Next: graph.End, Status: journal.StatusOK,
			Patch: state.Patch{Test: &state.Test{Total: 3, Passed: 3}},
			Note:  id + " passed on the re-run",
		}, nil
	})

	node := &runsCandidates{specs: poolSpecs(h, 3)}
	if _, err := h.poolDispatcher(t, pool{
		registry: candidateRegistry(t, code, testNode), isolator: iso, concurrency: 3,
	}, node).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if node.err != nil {
		t.Fatalf("CandidateRuns.Run() error = %v, want nil", node.err)
	}

	// Nothing is provisioned twice: every candidate already had a workspace.
	for _, id := range []string{"cand-01", "cand-02", "cand-03"} {
		if iso.creates(id) != 0 {
			t.Errorf("%s had its workspace made again (%d times); it was already provisioned", id, iso.creates(id))
		}
	}
	// A terminal candidate executes nothing at all.
	for _, id := range []string{"cand-01", "cand-02"} {
		if code.ran(id)+testNode.ran(id) != 0 {
			t.Errorf("%s executed %d nodes, want 0: it had already ended", id, code.ran(id)+testNode.ran(id))
		}
	}
	// The interrupted one re-runs only the node it died in, at the next
	// attempt, and never an earlier one.
	if code.ran("cand-03") != 0 {
		t.Errorf("cand-03 re-ran code %d times; only the node its journal left open runs again", code.ran("cand-03"))
	}
	if testNode.ran("cand-03") != 1 || attempts["cand-03"] != 2 {
		t.Errorf("cand-03 ran test %d times at attempt %d, want once at attempt 2",
			testNode.ran("cand-03"), attempts["cand-03"])
	}
	healed := 0
	for _, rec := range candidateRecords(t, h, "cand-03") {
		if rec.Interrupted() {
			healed++
		}
	}
	if healed != 1 {
		t.Errorf("cand-03's journal holds %d interrupted attempts, want exactly 1", healed)
	}

	// The end states are read from each candidate's own journal, and the
	// blackboard a finished candidate reached is still its own.
	want := []struct {
		id     string
		status state.RunStatus
		total  int
	}{
		{"cand-01", state.RunStatusCompleted, 7},
		{"cand-02", state.RunStatusFailed, 0},
		{"cand-03", state.RunStatusCompleted, 3},
	}
	for i, w := range want {
		o := node.outcomes[i]
		if o.ID != w.id || o.Status != w.status {
			t.Errorf("outcome %d = {%q, %v}, want {%q, %v}", i, o.ID, o.Status, w.id, w.status)
		}
		if o.State.Test.Total != w.total {
			t.Errorf("%s State.Test.Total = %d, want %d: adoption must not wipe a candidate's blackboard",
				w.id, o.State.Test.Total, w.total)
		}
		if o.Err != nil {
			t.Errorf("%s outcome carries Err = %v, want nil", w.id, o.Err)
		}
	}
	if got := node.outcomes[1].Spent.USD; !closeTo(got, 0.30) {
		t.Errorf("cand-02 Spent.USD = %v, want 0.30 rebuilt from its own journal", got)
	}
}

// TestATornClaimIsRemadeAndAnythingElseIsRefused covers the window between
// the first file a claim writes and the manifest it writes last. What is
// there is either debris belay recognises as its own, which it clears and
// makes again, or something it cannot prove it wrote — and then it refuses,
// because the alternative is writing an agent's workspace into a directory
// somebody else put there.
func TestATornClaimIsRemadeAndAnythingElseIsRefused(t *testing.T) {
	tests := []struct {
		name string
		// arrange puts something at candidate-runs/cand-01 and returns a
		// check to run afterwards.
		arrange func(t *testing.T, h *harness, dir string) func(t *testing.T)
		wantRun bool
		wantErr error
	}{
		{
			name: "debris a claim left behind is cleared and claimed again",
			arrange: func(t *testing.T, _ *harness, dir string) func(t *testing.T) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(dir, "snapshots"), 0o750); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				stale := filepath.Join(dir, "snapshots", "0001-code.json")
				if err := os.WriteFile(stale, []byte("{}\n"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}\n"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return func(t *testing.T) {
					t.Helper()
					if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
						t.Errorf("the torn claim's snapshot survived (%v); the directory is remade, not written over", err)
					}
				}
			},
			wantRun: true,
		},
		{
			name: "a symlink is refused and what it points at is untouched",
			arrange: func(t *testing.T, _ *harness, dir string) func(t *testing.T) {
				t.Helper()
				victim := t.TempDir()
				if err := os.WriteFile(filepath.Join(victim, "victim.txt"), []byte("mine\n"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				if err := os.Symlink(victim, dir); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
				return func(t *testing.T) {
					t.Helper()
					entries, err := os.ReadDir(victim)
					if err != nil {
						t.Fatalf("ReadDir(victim): %v", err)
					}
					if len(entries) != 1 || entries[0].Name() != "victim.txt" {
						names := make([]string, 0, len(entries))
						for _, e := range entries {
							names = append(names, e.Name())
						}
						t.Errorf("the symlink's target holds %v, want only victim.txt", names)
					}
				}
			},
			wantErr: graph.ErrCandidateRunCorrupt,
		},
		{
			name: "a directory holding anything no claim writes is refused",
			arrange: func(t *testing.T, _ *harness, dir string) func(t *testing.T) {
				t.Helper()
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				mine := filepath.Join(dir, "notes.txt")
				if err := os.WriteFile(mine, []byte("mine\n"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return func(t *testing.T) {
					t.Helper()
					if _, err := os.Stat(mine); err != nil {
						t.Errorf("notes.txt is gone (%v); belay removed a directory it could not prove it wrote", err)
					}
				}
			},
			wantErr: graph.ErrCandidateRunCorrupt,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, budgetCfg(3.00, 0), "a torn claim")
			if err := os.MkdirAll(h.layout.CandidateRunsDir(), 0o750); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			check := tt.arrange(t, h, filepath.Join(h.layout.CandidateRunsDir(), "cand-01"))

			iso := newFakeIsolator(h.layout.CandidatesDir())
			code := newCandidateNode(graph.NodeCode, finishes(0.25))
			node := &runsCandidates{specs: poolSpecs(h, 1)}
			if _, err := h.poolDispatcher(t, pool{
				registry: candidateRegistry(t, code), isolator: iso,
			}, node).Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}

			got := node.outcomes[0]
			switch {
			case tt.wantErr != nil:
				if !errors.Is(got.Err, tt.wantErr) {
					t.Fatalf("cand-01 Err = %v, want one matching %v", got.Err, tt.wantErr)
				}
				if code.ran("cand-01") != 0 {
					t.Errorf("cand-01 ran although its run directory was refused")
				}
			default:
				if got.Err != nil {
					t.Fatalf("cand-01 Err = %v, want nil", got.Err)
				}
				if got.Status != state.RunStatusCompleted {
					t.Errorf("cand-01 Status = %v, want completed", got.Status)
				}
			}
			if ran := code.ran("cand-01") > 0; ran != tt.wantRun {
				t.Errorf("cand-01 ran = %v, want %v", ran, tt.wantRun)
			}
			check(t)
		})
	}
}

// TestADurableRefusalIsRecordedAndAMachineFaultIsRetried draws the one
// distinction that decides whether a candidate can ever come back.
//
// An isolator saying "not here, not ever" (belay.ErrUnsupported) is written
// into the candidate's own manifest and reproduced from it for good, without
// the isolator being asked again. Anything else — a full disk, a lock, a
// deadline — has nothing durable written about it at all, so the candidate
// stays claimed and unprovisioned and the next attempt tries exactly that
// provisioning again. Making a transient fault permanent would lose a
// candidate, and money, to a disk that was full for a minute.
func TestADurableRefusalIsRecordedAndAMachineFaultIsRetried(t *testing.T) {
	unsupported := fmt.Errorf("%w: the source is not inside this repository", belay.ErrUnsupported)
	diskFull := errors.New("write: no space left on device")

	tests := []struct {
		name string
		// cause is what Create returns for cand-02 in the first round.
		cause error
		// durable says the refusal is recorded and never retried.
		durable bool
	}{
		{name: "an isolator that can never work here is recorded for good", cause: unsupported, durable: true},
		{name: "a machine fault records nothing and is retried", cause: diskFull},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, budgetCfg(3.00, 0), "one candidate cannot be given a workspace")
			// A reclaiming isolator, as both real ones are: retrying the
			// provisioning of a candidate that was claimed and never given
			// a workspace means clearing whatever the first attempt left.
			iso := reclaimingIsolator{newFakeIsolator(h.layout.CandidatesDir())}
			iso.fail["cand-02"] = tt.cause
			code := newCandidateNode(graph.NodeCode, finishes(0.25))
			node := &runsCandidates{
				specs: poolSpecs(h, 3), rounds: 2,
				// A durable refusal must survive a mended isolator; a
				// machine fault must be retried against it.
				between: func(int) {
					iso.mu.Lock()
					defer iso.mu.Unlock()
					delete(iso.fail, "cand-02")
				},
			}

			if _, err := h.poolDispatcher(t, pool{
				registry: candidateRegistry(t, code), isolator: iso, concurrency: 3,
			}, node).Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if len(node.results) != 2 {
				t.Fatalf("the pool was asked %d times, want 2", len(node.results))
			}

			first, second := node.results[0][1], node.results[1][1]
			man := candidateManifest(t, h, "cand-02")
			if man.Candidate == nil {
				t.Fatal("cand-02 has no candidate block")
			}

			if tt.durable {
				if first.Provision != state.ProvisionRefused || first.Err != nil {
					t.Errorf("first round cand-02 = {%q, err %v}, want a durable refusal", first.Provision, first.Err)
				}
				if man.Candidate.Provision != state.ProvisionRefused || man.Candidate.ProvisionNote == "" {
					t.Errorf("cand-02 manifest records {%q, %q}, want the refusal and its reason",
						man.Candidate.Provision, man.Candidate.ProvisionNote)
				}
				if !strings.Contains(first.Note, "not inside this repository") {
					t.Errorf("cand-02 Note = %q, want the isolator's reason", first.Note)
				}
				if second.Provision != state.ProvisionRefused || second.Note != first.Note {
					t.Errorf("second round cand-02 = {%q, %q}, want the same refusal reproduced",
						second.Provision, second.Note)
				}
				if iso.creates("cand-02") != 1 {
					t.Errorf("the isolator was asked for cand-02's workspace %d times, want 1: "+
						"a refusal is reproduced from the manifest, never asked again", iso.creates("cand-02"))
				}
				if code.ran("cand-02") != 0 {
					t.Errorf("cand-02 ran %d nodes, want 0", code.ran("cand-02"))
				}
			} else {
				if first.Err == nil || first.Provision != state.ProvisionPending {
					t.Errorf("first round cand-02 = {%q, err %v}, want a fault and nothing durable",
						first.Provision, first.Err)
				}
				if man.Candidate.Provision != state.ProvisionOK {
					t.Errorf("cand-02 manifest records %q after the retry, want %q",
						man.Candidate.Provision, state.ProvisionOK)
				}
				if second.Err != nil || second.Status != state.RunStatusCompleted {
					t.Errorf("second round cand-02 = {%v, err %v}, want it completed on the retry",
						second.Status, second.Err)
				}
				if iso.creates("cand-02") != 2 {
					t.Errorf("the isolator was asked for cand-02's workspace %d times, want 2: "+
						"exactly that provisioning is retried", iso.creates("cand-02"))
				}
				if code.ran("cand-02") != 1 {
					t.Errorf("cand-02 ran %d nodes, want 1", code.ran("cand-02"))
				}
			}

			// Its siblings ended durably in the first round either way, and
			// the second round adopts them without running anything again.
			for _, i := range []int{0, 2} {
				id := node.results[0][i].ID
				if node.results[0][i].Status != state.RunStatusCompleted {
					t.Errorf("%s = %v in the first round, want completed", id, node.results[0][i].Status)
				}
				if code.ran(id) != 1 {
					t.Errorf("%s ran %d nodes across both rounds, want 1", id, code.ran(id))
				}
				if iso.creates(id) != 1 {
					t.Errorf("%s had its workspace made %d times, want 1", id, iso.creates(id))
				}
			}
		})
	}
}

// TestAnUnprovisionedClaimIsReclaimedBeforeItsWorkspaceIsMadeAgain covers
// the crash that lands between the claim and the workspace: the manifest
// exists, provision is still empty, and whatever Isolator.Create had begun
// is lying in the candidate root. Both isolators refuse to create over that
// debris, so it has to go first — and Destroy cannot be what removes it,
// because its contract is "at most once per Workspace Create returned" and
// here Create never returned one.
func TestAnUnprovisionedClaimIsReclaimedBeforeItsWorkspaceIsMadeAgain(t *testing.T) {
	t.Run("an isolator that can reclaim does so strictly before it creates", func(t *testing.T) {
		h := newRun(t, budgetCfg(3.00, 0), "reclaim before create")
		iso := reclaimingIsolator{newFakeIsolator(h.layout.CandidatesDir())}
		seedCandidate(t, h, iso.fakeIsolator, seeded{
			id: "cand-01", index: 1, provision: state.ProvisionPending,
			envelope: oneEnvelope, status: state.RunStatusRunning,
		})

		code := newCandidateNode(graph.NodeCode, finishes(0.25))
		node := &runsCandidates{specs: poolSpecs(h, 2)}
		if _, err := h.poolDispatcher(t, pool{
			registry: candidateRegistry(t, code), isolator: iso, concurrency: 1,
		}, node).Run(context.Background()); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}

		want := []string{"reclaim cand-01", "create cand-01", "create cand-02"}
		if got := iso.calls(); !equalStrings(got, want) {
			t.Errorf("the isolator was called as\n  %v\nwant\n  %v", got, want)
		}
		for i, o := range node.outcomes {
			if o.Err != nil || o.Status != state.RunStatusCompleted {
				t.Errorf("outcome %d = {%v, err %v}, want completed", i, o.Status, o.Err)
			}
		}
	})

	t.Run("an isolator that cannot reclaim reports a fault naming the directory", func(t *testing.T) {
		h := newRun(t, budgetCfg(3.00, 0), "nothing can clear the debris")
		iso := newFakeIsolator(h.layout.CandidatesDir())
		l := seedCandidate(t, h, iso, seeded{
			id: "cand-01", index: 1, provision: state.ProvisionPending,
			envelope: oneEnvelope, status: state.RunStatusRunning,
		})

		code := newCandidateNode(graph.NodeCode, finishes(0.25))
		node := &runsCandidates{specs: poolSpecs(h, 2)}
		if _, err := h.poolDispatcher(t, pool{
			registry: candidateRegistry(t, code), isolator: iso, concurrency: 1,
		}, node).Run(context.Background()); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}

		got := node.outcomes[0]
		if got.Err == nil {
			t.Fatal("cand-01 outcome carries no Err; nothing would pause the fanout")
		}
		if !strings.Contains(got.Err.Error(), l.RunDir()) {
			t.Errorf("cand-01 Err = %v, want it to name %s so a person can clear it", got.Err, l.RunDir())
		}
		if iso.creates("cand-01") != 0 {
			t.Errorf("the isolator was asked to create over debris it refuses to create over")
		}
		if man := candidateManifest(t, h, "cand-01"); man.Candidate.Provision != state.ProvisionPending {
			t.Errorf("cand-01 manifest records %q, want it left unprovisioned for the next attempt",
				man.Candidate.Provision)
		}
		// Its sibling still ran: one candidate's fault is not the others'.
		if node.outcomes[1].Status != state.RunStatusCompleted {
			t.Errorf("cand-02 = %v, want completed", node.outcomes[1].Status)
		}
	})
}

// TestEveryCandidateGetsAnOutcomeCarryingItsOwnID pins the contract the
// fanout node places outcomes by: one per candidate, in the order the specs
// came in, each naming the candidate it belongs to — whatever happened, and
// including the rounds the pool could not carry out at all.
//
// The node refuses a partial set rather than settling it, because
// state.Candidates is an absolute set: outcomes only for the candidates that
// reached an end state would quietly drop one the run had already paid for.
func TestEveryCandidateGetsAnOutcomeCarryingItsOwnID(t *testing.T) {
	h := newRun(t, budgetCfg(3.00, 0), "one outcome each, always")
	iso := reclaimingIsolator{newFakeIsolator(h.layout.CandidatesDir())}
	// One candidate a fault stops, one an isolator refuses for good, one
	// that simply works.
	iso.fail["cand-01"] = errors.New("write: no space left on device")
	iso.fail["cand-02"] = fmt.Errorf("%w: the source is not inside this repository", belay.ErrUnsupported)

	code := newCandidateNode(graph.NodeCode, finishes(0.25))
	specs := poolSpecs(h, 3)
	node := &runsCandidates{
		specs: specs, rounds: 2,
		// The second round cannot even begin: the parent's own manifest is
		// gone, so the pool has nothing to claim against.
		between: func(int) {
			if err := os.Remove(h.layout.ManifestPath()); err != nil {
				t.Errorf("Remove(parent manifest): %v", err)
			}
		},
	}

	if _, err := h.poolDispatcher(t, pool{
		registry: candidateRegistry(t, code), isolator: iso, concurrency: 3,
	}, node).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(node.results) != 2 {
		t.Fatalf("the pool was asked %d times, want 2", len(node.results))
	}
	if node.err == nil {
		t.Error("the round that could not begin reported no error")
	}

	for round, outcomes := range node.results {
		if len(outcomes) != len(specs) {
			t.Fatalf("round %d returned %d outcomes, want one per candidate (%d)",
				round+1, len(outcomes), len(specs))
		}
		for i, o := range outcomes {
			if o.ID != specs[i].ID || o.Index != specs[i].Index {
				t.Errorf("round %d outcome %d = {%q, %d}, want {%q, %d}",
					round+1, i, o.ID, o.Index, specs[i].ID, specs[i].Index)
			}
		}
	}

	// And in the round that did run, each of the three ended its own way.
	first := node.results[0]
	if first[0].Err == nil {
		t.Errorf("cand-01 = %+v, want the fault that stopped it", first[0])
	}
	if first[1].Provision != state.ProvisionRefused {
		t.Errorf("cand-02 Provision = %q, want %q", first[1].Provision, state.ProvisionRefused)
	}
	if first[2].Status != state.RunStatusCompleted {
		t.Errorf("cand-03 Status = %v, want completed", first[2].Status)
	}
}

// TestOneCancelLeavesEveryCandidateResumable is the Ctrl-C promise, N+1
// journals over: `belay run` says one interrupt leaves a run `belay resume`
// picks up, and a fanout keeps that promise only if every candidate keeps it
// too. A candidate ended as aborted could never be continued, and the money
// its run had already spent would buy nothing.
func TestOneCancelLeavesEveryCandidateResumable(t *testing.T) {
	h := newRun(t, budgetCfg(5.00, 0), "one Ctrl-C, five candidates")
	iso := reclaimingIsolator{newFakeIsolator(h.layout.CandidatesDir())}
	specs := poolSpecs(h, 5)

	var blocking atomic.Bool
	var live atomic.Int64
	blocking.Store(true)
	arrived := make(chan string, len(specs))
	code := newCandidateNode(graph.NodeCode, func(ctx context.Context, id string, _ *graph.RunContext) (graph.Result, error) {
		live.Add(1)
		defer live.Add(-1)
		if !blocking.Load() {
			return finishes(0.20)(ctx, id, nil)
		}
		arrived <- id
		<-ctx.Done()
		return Result{}, ctx.Err()
	})

	// The interrupted attempt.
	ctx, cancel := context.WithCancel(context.Background())
	interrupted := &runsCandidates{specs: specs}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		if _, err := h.poolDispatcher(t, pool{
			registry: candidateRegistry(t, code), isolator: iso, concurrency: 3, pauseOnCancel: true,
		}, interrupted).Run(ctx); err == nil {
			t.Error("the interrupted parent run reported no error")
		}
	}()

	inFlight := make([]string, 0, 3)
	for range 3 {
		select {
		case id := <-arrived:
			inFlight = append(inFlight, id)
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d candidates reached their node, want 3", len(inFlight))
		}
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupted run never returned")
	}

	if !errors.Is(interrupted.err, context.Canceled) {
		t.Errorf("CandidateRuns.Run() error = %v, want one wrapping context.Canceled", interrupted.err)
	}
	if n := live.Load(); n != 0 {
		t.Errorf("%d candidates were still inside their node when Run returned; it must wait for all of them", n)
	}

	sort.Strings(inFlight)
	if want := []string{"cand-01", "cand-02", "cand-03"}; !equalStrings(inFlight, want) {
		t.Fatalf("the candidates in flight were %v, want %v", inFlight, want)
	}

	// Every candidate that started is paused, with its interrupted attempt
	// closed: never a bare open node_started, which journal.Validate would
	// reject a later record over, and never run_aborted, which no resume
	// recovers.
	for _, id := range inFlight {
		recs := candidateRecords(t, h, id)
		if err := journal.Validate(recs); err != nil {
			t.Errorf("%s journal.Validate() = %v, want nil", id, err)
		}
		if len(recs) == 0 {
			t.Fatalf("%s has an empty journal", id)
		}
		last := recs[len(recs)-1]
		if last.Event != journal.EventRunPaused {
			t.Errorf("%s journal ends with %v, want run_paused", id, last.Event)
		}
		if prev := recs[len(recs)-2]; !prev.Interrupted() {
			t.Errorf("%s closes its attempt as %v -> %q, want an interrupted attempt to run again",
				id, prev.Status, prev.Next)
		}
	}
	// The two the cancellation reached before a goroutine did are claimed
	// and no further, which is what a resume adopts.
	for _, id := range []string{"cand-04", "cand-05"} {
		man := candidateManifest(t, h, id)
		if man.Candidate == nil || man.Candidate.Provision != state.ProvisionPending {
			t.Errorf("%s = %+v, want a candidate claimed and not yet provisioned", id, man.Candidate)
		}
		if iso.creates(id) != 0 {
			t.Errorf("%s was provisioned although it never started", id)
		}
		if code.ran(id) != 0 {
			t.Errorf("%s ran %d nodes although it never started", id, code.ran(id))
		}
	}

	// And the resume finishes all five.
	blocking.Store(false)
	resumed := &runsCandidates{specs: specs}
	out, err := h.poolDispatcher(t, pool{
		registry: candidateRegistry(t, code), isolator: iso, concurrency: 3, pauseOnCancel: true,
	}, resumed).Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("resumed Outcome.Status = %v, want completed (note %q)", out.Status, out.Note)
	}
	if resumed.err != nil {
		t.Fatalf("CandidateRuns.Run() error on the resume = %v, want nil", resumed.err)
	}
	for i, o := range resumed.outcomes {
		if o.Err != nil || o.Status != state.RunStatusCompleted {
			t.Errorf("outcome %d (%s) = {%v, err %v}, want completed", i, o.ID, o.Status, o.Err)
		}
	}
	// The three that were interrupted kept their workspaces and re-ran the
	// node they were stopped in; the two that never started were given one.
	for _, id := range inFlight {
		if iso.creates(id) != 1 {
			t.Errorf("%s had its workspace made %d times, want 1", id, iso.creates(id))
		}
		if code.ran(id) != 2 {
			t.Errorf("%s ran code %d times, want 2: once interrupted, once on the resume", id, code.ran(id))
		}
	}
	for _, id := range []string{"cand-04", "cand-05"} {
		if iso.creates(id) != 1 || code.ran(id) != 1 {
			t.Errorf("%s was created %d times and ran %d nodes, want 1 and 1",
				id, iso.creates(id), code.ran(id))
		}
	}
}

// TestAnAdoptedEnvelopeIsNeverLoweredAndMayRise: an envelope is a
// reservation, and part of it may already be spent, so taking it back
// returns no money. It may rise, because the settled rule for every limit
// belay enforces is that raising it and resuming continues the run that met
// it — and a candidate that stopped on its envelope has no other way
// forward.
func TestAnAdoptedEnvelopeIsNeverLoweredAndMayRise(t *testing.T) {
	t.Run("the persisted envelope and the current share, whichever is larger", func(t *testing.T) {
		cases := []struct {
			id        string
			persisted state.Envelope
			share     state.Envelope
			want      state.Envelope
		}{
			{"cand-01", state.Envelope{LimitUSD: 1.00, LimitTokens: 1000},
				state.Envelope{LimitUSD: 2.00, LimitTokens: 5000},
				state.Envelope{LimitUSD: 2.00, LimitTokens: 5000}},
			{"cand-02", state.Envelope{LimitUSD: 1.00, LimitTokens: 1000},
				state.Envelope{LimitUSD: 0.50, LimitTokens: 400},
				state.Envelope{LimitUSD: 1.00, LimitTokens: 1000}},
			// Zero is uncapped, here as everywhere else belay reads a
			// ceiling, so it is the larger of the two rather than the
			// smaller: a candidate claimed while budget.max_tokens was
			// unset is not capped by one set afterwards.
			{"cand-03", state.Envelope{LimitUSD: 1.00, LimitTokens: 0},
				state.Envelope{LimitUSD: 1.00, LimitTokens: 500},
				state.Envelope{LimitUSD: 1.00, LimitTokens: 0}},
			{"cand-04", state.Envelope{LimitUSD: 1.00, LimitTokens: 1000},
				state.Envelope{LimitUSD: 1.00, LimitTokens: 0},
				state.Envelope{LimitUSD: 1.00, LimitTokens: 0}},
		}

		h := newRun(t, budgetCfg(8.00, 0), "envelopes only ever rise")
		iso := reclaimingIsolator{newFakeIsolator(h.layout.CandidatesDir())}
		specs := poolSpecs(h, len(cases))
		for i, c := range cases {
			specs[i].Envelope = c.share
			seedCandidate(t, h, iso.fakeIsolator, seeded{
				id: c.id, index: i + 1, provision: state.ProvisionOK,
				envelope: c.persisted, status: state.RunStatusRunning,
			})
		}

		code := newCandidateNode(graph.NodeCode, finishes(0.10))
		node := &runsCandidates{specs: specs}
		if _, err := h.poolDispatcher(t, pool{
			registry: candidateRegistry(t, code), isolator: iso, concurrency: 4,
		}, node).Run(context.Background()); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}

		for i, c := range cases {
			man := candidateManifest(t, h, c.id)
			if man.Candidate == nil {
				t.Fatalf("%s has no candidate block", c.id)
			}
			if man.Candidate.Envelope != c.want {
				t.Errorf("%s persisted envelope = %+v, want %+v (was %+v, share %+v)",
					c.id, man.Candidate.Envelope, c.want, c.persisted, c.share)
			}
			if got := node.outcomes[i].Envelope; got != c.want {
				t.Errorf("%s outcome envelope = %+v, want %+v", c.id, got, c.want)
			}
			if man.Budget.LimitUSD != c.persisted.LimitUSD && man.Budget.LimitUSD != c.want.LimitUSD {
				t.Errorf("%s manifest budget ceiling = %v, want its envelope", c.id, man.Budget.LimitUSD)
			}
		}
	})

	t.Run("a candidate that met its envelope continues when the ceiling rises", func(t *testing.T) {
		h := newRun(t, budgetCfg(9.00, 0), "raise it and resume")
		iso := reclaimingIsolator{newFakeIsolator(h.layout.CandidatesDir())}
		// cand-01 stopped at its own ceiling: it has spent 1.20 against an
		// envelope of 1.00, and its run is paused before test.
		seedCandidate(t, h, iso.fakeIsolator, seeded{
			id: "cand-01", index: 1, provision: state.ProvisionOK,
			envelope: state.Envelope{LimitUSD: 1.00}, status: state.RunStatusPaused, step: 1,
			records: []journal.Record{
				{Event: journal.EventRunStarted},
				{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1},
				{Event: journal.EventNodeFinished, Node: graph.NodeCode, Attempt: 1,
					Status: journal.StatusOK, Next: graph.NodeTest, Usage: &journal.Usage{USD: 1.20}},
				{Event: journal.EventRunPaused, Note: "budget ceiling reached before \"test\""},
			},
		})

		code := newCandidateNode(graph.NodeCode, finishes(0.10))
		testNode := newCandidateNode(graph.NodeTest, finishes(0.10))
		specs := poolSpecs(h, 1)
		specs[0].Envelope = state.Envelope{LimitUSD: 0.50}
		node := &runsCandidates{
			specs: specs, rounds: 2,
			// The second round is the resume after budget.max_usd was
			// raised: the share is now bigger than what cand-01 holds.
			between: func(int) { specs[0].Envelope = state.Envelope{LimitUSD: 3.00} },
		}

		if _, err := h.poolDispatcher(t, pool{
			registry: candidateRegistry(t, code, testNode), isolator: iso,
		}, node).Run(context.Background()); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}

		// A smaller share changes nothing: it keeps the 1.00 it was
		// promised, and stops at it again having run nothing.
		first := node.results[0][0]
		if first.Envelope.LimitUSD != 1.00 {
			t.Errorf("first round envelope = %v, want the 1.00 it was claimed with", first.Envelope.LimitUSD)
		}
		if first.Status != state.RunStatusPaused {
			t.Errorf("first round Status = %v, want paused at its own ceiling", first.Status)
		}
		if !closeTo(first.Spent.USD, 1.20) {
			t.Errorf("first round Spent.USD = %v, want the 1.20 it had already spent and nothing more",
				first.Spent.USD)
		}

		// A bigger one lets it finish.
		second := node.results[1][0]
		if second.Envelope.LimitUSD != 3.00 {
			t.Errorf("second round envelope = %v, want it raised to the current share of 3.00",
				second.Envelope.LimitUSD)
		}
		if second.Status != state.RunStatusCompleted || second.Err != nil {
			t.Errorf("second round = {%v, err %v}, want completed", second.Status, second.Err)
		}
		if testNode.ran("cand-01") != 1 {
			t.Errorf("cand-01 ran test %d times, want 1 once its ceiling rose", testNode.ran("cand-01"))
		}
		if code.ran("cand-01") != 0 {
			t.Errorf("cand-01 re-ran code; only the node its journal stopped before runs")
		}
		if man := candidateManifest(t, h, "cand-01"); man.Candidate.Envelope.LimitUSD != 3.00 {
			t.Errorf("cand-01 persisted envelope = %v, want 3.00", man.Candidate.Envelope.LimitUSD)
		}
	})
}

// TestEveryCandidateWritesOnlyItsOwnFiles is the reason a candidate is a run
// and not a set of records in the parent's journal. internal/journal rejects
// a node_started opened while another is still open, so N concurrent
// candidates sharing one journal would corrupt it on the second append and
// journal.Open would refuse the file for good. Nesting by directory makes
// that unreachable rather than merely avoided.
func TestEveryCandidateWritesOnlyItsOwnFiles(t *testing.T) {
	h := newRun(t, budgetCfg(5.00, 0), "one journal each")
	iso := newFakeIsolator(h.layout.CandidatesDir())
	ids := []string{"cand-01", "cand-02", "cand-03"}

	code := newCandidateNode(graph.NodeCode, func(_ context.Context, id string, _ *graph.RunContext) (graph.Result, error) {
		return Result{Next: graph.NodeTest, Status: journal.StatusOK, Usage: belay.Usage{USD: 0.10}, Note: id}, nil
	})
	testNode := newCandidateNode(graph.NodeTest, finishes(0.05))

	node := &runsCandidates{
		specs: poolSpecs(h, 3),
		digest: func() map[string]string {
			out := map[string]string{}
			for _, name := range []string{"manifest.json", "state.json", "journal.ndjson"} {
				// #nosec G304 -- path is inside the test's own run directory.
				data, err := os.ReadFile(filepath.Join(h.layout.RunDir(), name))
				if err != nil {
					out[name] = "missing"
					continue
				}
				out[name] = fmt.Sprintf("%x", sha256.Sum256(data))
			}
			return out
		},
	}
	if _, err := h.poolDispatcher(t, pool{
		registry: candidateRegistry(t, code, testNode), isolator: iso, concurrency: 3,
	}, node).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	// Not one byte of the parent run changed while the candidates ran.
	// The digests are checked for substance first, so that a comparison of
	// two sets of "missing" cannot pass for proof.
	if len(node.digestBefore) != 3 {
		t.Fatalf("the parent was digested as %v, want all three of its files", node.digestBefore)
	}
	for name, sum := range node.digestBefore {
		if sum == "missing" {
			t.Fatalf("the parent's %s was not there to digest", name)
		}
	}
	if !reflect.DeepEqual(node.digestBefore, node.digestAfter) {
		t.Errorf("the parent's files changed while the candidates ran:\n  before %v\n  after  %v",
			node.digestBefore, node.digestAfter)
	}

	// The parent's journal knows only about its own node.
	for _, rec := range h.records(t) {
		if rec.Node != "" && rec.Node != graph.NodeFanout {
			t.Errorf("the parent journal holds a record for %q; candidate records belong in candidate journals", rec.Node)
		}
	}

	// And each candidate's journal is a valid run of its own, holding its
	// own records and nobody else's.
	for _, id := range ids {
		recs := candidateRecords(t, h, id)
		if err := journal.Validate(recs); err != nil {
			t.Errorf("%s journal.Validate() = %v, want nil", id, err)
		}
		var nodes []string
		for _, rec := range recs {
			if rec.Node != "" {
				nodes = append(nodes, rec.Node)
			}
			if rec.Note != "" && rec.Note != id && rec.Note != id+" finished its work" &&
				rec.Note != "run completed" {
				t.Errorf("%s journal holds the note %q, which is not its own", id, rec.Note)
			}
		}
		want := []string{graph.NodeCode, graph.NodeCode, graph.NodeTest, graph.NodeTest}
		if !equalStrings(nodes, want) {
			t.Errorf("%s journal names %v, want %v", id, nodes, want)
		}
	}
}

// TestAPanickingCandidateDoesNotTakeDownItsSiblings: a bug in one
// candidate's node must not cost the others work that has already been paid
// for. Its outcome carries the fault, so the fanout pauses rather than
// settling a verdict about it, and its journal is left open, so a resume
// heals it and runs that node again.
//
// The isolation is honest but partial, and the ADR says so: it covers a
// panic on the candidate's own goroutine. A panic in a goroutine a node
// itself starts still ends the process, exactly as it does for a single run
// today.
func TestAPanickingCandidateDoesNotTakeDownItsSiblings(t *testing.T) {
	h := newRun(t, budgetCfg(3.00, 0), "one candidate has a bug in it")
	iso := newFakeIsolator(h.layout.CandidatesDir())
	code := newCandidateNode(graph.NodeCode, func(ctx context.Context, id string, rc *graph.RunContext) (graph.Result, error) {
		if id == "cand-02" {
			panic("the candidate node dereferenced nothing")
		}
		return finishes(0.25)(ctx, id, rc)
	})

	node := &runsCandidates{specs: poolSpecs(h, 3)}
	if _, err := h.poolDispatcher(t, pool{
		registry: candidateRegistry(t, code), isolator: iso, concurrency: 3,
	}, node).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	got := node.outcomes[1]
	if got.Err == nil {
		t.Fatal("cand-02 outcome carries no Err; the fanout would settle a verdict about a candidate that crashed")
	}
	if !strings.Contains(got.Err.Error(), "dereferenced nothing") {
		t.Errorf("cand-02 Err = %v, want it to carry what the panic said", got.Err)
	}

	// Its siblings finished.
	for _, i := range []int{0, 2} {
		o := node.outcomes[i]
		if o.Err != nil || o.Status != state.RunStatusCompleted {
			t.Errorf("%s = {%v, err %v}, want completed", o.ID, o.Status, o.Err)
		}
	}

	// And cand-02's journal is left open, which is what makes it resumable:
	// the next attempt heals it and runs that node again.
	recs := candidateRecords(t, h, "cand-02")
	if len(recs) == 0 {
		t.Fatal("cand-02 has an empty journal")
	}
	if last := recs[len(recs)-1]; last.Event != journal.EventNodeStarted {
		t.Errorf("cand-02 journal ends with %v, want the node_started its panic left open", last.Event)
	}
}

// TestAResumeKillsACandidatesOrphanedProcessBeforeItsNodeRunsAgain is a
// guard, not a failing test first: internal/procgroup and the dispatcher's
// heal already do this for a run, and a candidate is a run, so it passed the
// moment it was written. It is here because the inheritance is the whole
// argument for the shape — it would fail for any design that gave the
// candidates a shared evidence tree, or that drove them from the parent's
// own Layout — and because an agent CLI left running after a kill -9 keeps
// spending the user's money.
func TestAResumeKillsACandidatesOrphanedProcessBeforeItsNodeRunsAgain(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // where procgroup keeps the records
	h := newRun(t, budgetCfg(3.00, 0), "kill what the dead process left running")
	iso := newFakeIsolator(h.layout.CandidatesDir())
	l := seedCandidate(t, h, iso, seeded{
		id: "cand-01", index: 1, provision: state.ProvisionOK, envelope: oneEnvelope,
		status: state.RunStatusRunning,
		records: []journal.Record{
			{Event: journal.EventRunStarted},
			{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1},
		},
	})

	// The agent the dead belay had started, in a process group of its own,
	// recorded exactly as internal/exec records one — under the candidate's
	// evidence tree, not the parent's.
	sleeper := exec.Command("sleep", "60")
	sleeper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sleeper.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		if err := sleeper.Wait(); err != nil {
			t.Logf("sleeper exited: %v", err) // expected: killed
		}
		close(exited)
	}()
	t.Cleanup(func() {
		if err := syscall.Kill(-sleeper.Process.Pid, syscall.SIGKILL); err != nil &&
			!errors.Is(err, syscall.ESRCH) {
			t.Errorf("clean up process group: %v", err)
		}
		<-exited
	})

	nodeDir, err := l.NodeDir(1, graph.NodeCode)
	if err != nil {
		t.Fatalf("NodeDir: %v", err)
	}
	// The command it records is the one that runs: procgroup kills only a
	// process still running the program its record names.
	tracked, err := procgroup.Track(nodeDir, sleeper.Args)
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := tracked.Started(sleeper.Process.Pid); err != nil {
		t.Fatalf("Started: %v", err)
	}

	var aliveWhenItRan bool
	code := newCandidateNode(graph.NodeCode, func(ctx context.Context, id string, rc *graph.RunContext) (graph.Result, error) {
		aliveWhenItRan = syscall.Kill(sleeper.Process.Pid, 0) == nil
		return finishes(0.25)(ctx, id, rc)
	})

	node := &runsCandidates{specs: poolSpecs(h, 1)}
	if _, err := h.poolDispatcher(t, pool{
		registry: candidateRegistry(t, code), isolator: iso,
	}, node).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got := node.outcomes[0]; got.Err != nil || got.Status != state.RunStatusCompleted {
		t.Fatalf("cand-01 = {%v, err %v}, want completed", got.Status, got.Err)
	}

	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("process %d is still running: adopting the candidate did not kill what its crash left behind",
			sleeper.Process.Pid)
	}
	if aliveWhenItRan {
		t.Error("the orphan was still alive when the candidate's node ran again; it would have been writing " +
			"into the same workspace")
	}
	left, err := filepath.Glob(filepath.Join(nodeDir, "pgroup-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("records left behind: %v; a later resume could signal a pid that by then is somebody else's", left)
	}
}

// TestACancelledPoolAdmitsNoFurtherCandidate: a candidate that has not been
// admitted must stay claimed and untouched, because that is exactly what a
// resume adopts. Admitting one after the cancellation would copy a whole
// tree for a workspace nobody asked for and write a run_paused into an
// otherwise empty journal, which the next resume would then have to reclaim.
//
// The window is the moment a worker is between candidates: the goroutine
// feeding the pool and the cancellation are both ready, and which one the
// runtime picks is a coin toss. So the cancellation is thrown from inside
// the first candidate, when a worker is about to come back for the next one,
// and there are enough candidates behind it that the toss cannot come up
// heads every time.
func TestACancelledPoolAdmitsNoFurtherCandidate(t *testing.T) {
	h := newRun(t, budgetCfg(9.00, 0), "admit nothing more once it is stopping")
	iso := newFakeIsolator(h.layout.CandidatesDir())
	specs := poolSpecs(h, 9)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := newCandidateNode(graph.NodeCode, func(_ context.Context, id string, _ *graph.RunContext) (graph.Result, error) {
		if id == "cand-01" {
			cancel()
		}
		return Result{Next: graph.End, Status: journal.StatusOK, Note: id}, nil
	})

	node := &runsCandidates{specs: specs}
	if _, err := h.poolDispatcher(t, pool{
		registry: candidateRegistry(t, code), isolator: iso, concurrency: 1, pauseOnCancel: true,
	}, node).Run(ctx); err == nil {
		t.Fatal("Run() error = nil, want the cancellation")
	}
	if !errors.Is(node.err, context.Canceled) {
		t.Fatalf("CandidateRuns.Run() error = %v, want one wrapping context.Canceled", node.err)
	}

	for _, spec := range specs[1:] {
		if n := iso.creates(spec.ID); n != 0 {
			t.Errorf("%s had a workspace made (%d times) after the pool was cancelled", spec.ID, n)
		}
		if n := code.ran(spec.ID); n != 0 {
			t.Errorf("%s ran %d nodes after the pool was cancelled", spec.ID, n)
		}
		if recs := candidateRecords(t, h, spec.ID); len(recs) != 0 {
			t.Errorf("%s has %d journal records; a candidate that was never admitted has no run yet",
				spec.ID, len(recs))
		}
		// Claimed all the same, which is what makes the resume cheap.
		man := candidateManifest(t, h, spec.ID)
		if man.Candidate == nil || man.Candidate.Provision != state.ProvisionPending {
			t.Errorf("%s = %+v, want it claimed and not provisioned", spec.ID, man.Candidate)
		}
	}
}

// TestACandidateCutShortInItsFirstPaidStepIsChargedItsParentsCostliestStep:
// a candidate's own ledger is empty until its first paid step finishes, so
// the estimate an attempt cut short is charged (the costliest step the run
// has paid for) was zero for every Ctrl-C, timeout or kill during a
// candidate's first step, which is when most of them land. The parent has
// already paid for its plan by then, and that is evidence of what one step
// of this run costs, so a candidate run is charged at least that. Without it
// interrupting a fanout and resuming it, again and again, bought one paid
// first step per candidate per cycle and recorded none of them.
func TestACandidateCutShortInItsFirstPaidStepIsChargedItsParentsCostliestStep(t *testing.T) {
	planSpend := journal.Usage{InputTokens: 1000, OutputTokens: 200, USD: 0.40}
	h := newRun(t, budgetCfg(5.00, 0), "charge every cut, from the first")
	h.seedJournal(t,
		journal.Record{Event: journal.EventRunStarted},
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodePlan, Attempt: 1},
		journal.Record{
			Event: journal.EventNodeFinished, Node: graph.NodePlan, Attempt: 1,
			Status: journal.StatusOK, Next: graph.NodeFanout, Usage: &planSpend,
		},
	)
	iso := reclaimingIsolator{newFakeIsolator(h.layout.CandidatesDir())}
	specs := poolSpecs(h, 1)
	specs[0].Envelope = state.Envelope{LimitUSD: 1.00}

	// The code node cancels the dispatch it is running in, as a Ctrl-C
	// would, and reports nothing, as a killed agent CLI does.
	var cancelCurrent atomic.Pointer[context.CancelFunc]
	code := newCandidateNode(graph.NodeCode, func(ctx context.Context, _ string, _ *graph.RunContext) (graph.Result, error) {
		(*cancelCurrent.Load())()
		<-ctx.Done()
		return Result{}, ctx.Err()
	})

	var budgetStopped bool
	for range 10 {
		ctx, cancel := context.WithCancel(context.Background())
		cancelCurrent.Store(&cancel)
		node := &runsCandidates{specs: specs}
		_, _ = h.poolDispatcher(t, pool{
			registry: candidateRegistry(t, code), isolator: iso, concurrency: 1, pauseOnCancel: true,
		}, node).Resume(ctx)
		cancel()
		if node.outcomes != nil && node.outcomes[0].Status == state.RunStatusPaused && node.err == nil {
			budgetStopped = true
			break
		}
	}

	recs := candidateRecords(t, h, "cand-01")
	var first *journal.Record
	for i := range recs {
		if recs[i].Interrupted() {
			first = &recs[i]
			break
		}
	}
	want := journal.Usage{InputTokens: 1000, OutputTokens: 200, USD: 0.40, Estimated: true}
	if first == nil || first.Usage == nil || *first.Usage != want {
		t.Fatalf("the candidate's first cut-short attempt was charged %+v, want the parent's plan %+v", first, want)
	}
	if !budgetStopped {
		t.Fatalf("ten Ctrl-C and resume cycles never brought the candidate to its envelope; code ran %d times", code.ran("cand-01"))
	}
	// $0.40 a cut against a $1.00 envelope: the guard lets a third cut start
	// at $0.80 and stops the fourth at $1.20.
	if n := code.ran("cand-01"); n != 3 {
		t.Errorf("the candidate's first step started %d times, want 3 before its envelope stopped it", n)
	}
}

// TestClaimedSaysWhetherAFanoutWouldAdopt: the fanout node asks before it
// projects what N candidates will cost, because the projection is about
// launching and means nothing once the candidates hold envelopes. A torn
// claim, a directory with no manifest, is not a claim: the next Run clears
// it and claims afresh.
func TestClaimedSaysWhetherAFanoutWouldAdopt(t *testing.T) {
	h := newRun(t, budgetCfg(5.00, 0), "adopt or launch")
	iso := newFakeIsolator(h.layout.CandidatesDir())
	code := newCandidateNode(graph.NodeCode, finishes(0.10))
	specs := poolSpecs(h, 2)
	ids := []string{"cand-01", "cand-02"}

	var before, torn, after bool
	probe := &probingFanout{specs: specs, ids: ids, before: &before, torn: &torn, after: &after,
		tear: func() {
			if err := os.MkdirAll(candidateLayout(t, h, "cand-01").RunDir(), 0o750); err != nil {
				t.Fatalf("make a torn claim: %v", err)
			}
		}}
	if _, err := h.poolDispatcher(t, pool{registry: candidateRegistry(t, code), isolator: iso}, probe).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if before {
		t.Error("Claimed() = true before anything was claimed")
	}
	if torn {
		t.Error("Claimed() = true for a run directory with no manifest, which is a torn claim")
	}
	if !after {
		t.Error("Claimed() = false after the candidates were claimed")
	}
}

// probingFanout asks Claimed before anything exists, after a torn claim is
// left behind, and after the runner has claimed and run every candidate.
type probingFanout struct {
	specs               []graph.CandidateSpec
	ids                 []string
	before, torn, after *bool
	tear                func()
}

func (*probingFanout) Name() string          { return graph.NodeFanout }
func (*probingFanout) SupervisesCandidates() {}

func (p *probingFanout) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	var err error
	if *p.before, err = rc.CandidateRuns.Claimed(p.ids); err != nil {
		return graph.Result{}, err
	}
	p.tear()
	if *p.torn, err = rc.CandidateRuns.Claimed(p.ids); err != nil {
		return graph.Result{}, err
	}
	if _, err := rc.CandidateRuns.Run(ctx, p.specs); err != nil {
		return graph.Result{}, err
	}
	if *p.after, err = rc.CandidateRuns.Claimed(p.ids); err != nil {
		return graph.Result{}, err
	}
	return Result{Next: graph.End, Status: journal.StatusOK}, nil
}

// TestACandidateStoppedAtALimitSaysWhich: a candidate run meets its envelope,
// graph.max_steps or graph.node_timeout like any run, and pauses. The runner
// used to report only that it was paused, which the fanout node could not
// tell from a candidate waiting on a fault, so it said "its run is paused, not
// ended" and exited 0. The outcome now carries the limit the candidate's own
// dispatcher stopped it at, and the reason its journal gives.
func TestACandidateStoppedAtALimitSaysWhich(t *testing.T) {
	h := newRun(t, budgetCfg(5.00, 0), "stop at the envelope")
	iso := newFakeIsolator(h.layout.CandidatesDir())
	code := newCandidateNode(graph.NodeCode, func(context.Context, string, *graph.RunContext) (graph.Result, error) {
		return Result{Next: graph.NodeTest, Status: journal.StatusOK, Usage: belay.Usage{USD: 1.20}}, nil
	})
	testNode := newCandidateNode(graph.NodeTest, finishes(0.10))
	specs := poolSpecs(h, 1)
	specs[0].Envelope = state.Envelope{LimitUSD: 1.00}

	node := &runsCandidates{specs: specs}
	if _, err := h.poolDispatcher(t, pool{registry: candidateRegistry(t, code, testNode), isolator: iso}, node).
		Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	o := node.outcomes[0]
	if o.Status != state.RunStatusPaused || o.Err != nil {
		t.Fatalf("outcome = {%v, err %v}, want a candidate paused with no fault", o.Status, o.Err)
	}
	var budgetErr *belay.BudgetError
	if !errors.As(o.Stopped, &budgetErr) || !closeTo(budgetErr.LimitUSD, 1.00) {
		t.Errorf("Stopped = %v, want the *belay.BudgetError of its $1.00 envelope", o.Stopped)
	}
	if !strings.Contains(o.Note, "budget ceiling reached before \"test\"") {
		t.Errorf("Note = %q, want the reason the candidate's own journal gives", o.Note)
	}
	if testNode.ran("cand-01") != 0 {
		t.Errorf("test ran %d times past the envelope", testNode.ran("cand-01"))
	}
}

// TestACandidatesDurationDoesNotDependOnHowItWasInterrupted: `fastest`
// compares candidates by their summed node durations. An attempt a Ctrl-C or
// a timeout cut short is closed with the time it ran, and one a kill cut
// short is closed by heal with none, so the winner depended on how the run
// had been interrupted rather than on how fast each candidate did its work.
// Only the attempts that settled are counted now: an attempt cut short
// produced nothing the candidate kept, and its re-run is the work that did.
func TestACandidatesDurationDoesNotDependOnHowItWasInterrupted(t *testing.T) {
	h := newRun(t, budgetCfg(5.00, 0), "time the work, not the interruptions")
	iso := newFakeIsolator(h.layout.CandidatesDir())
	history := func(cutShortMS int64) []journal.Record {
		return []journal.Record{
			{Event: journal.EventRunStarted},
			{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1},
			{
				Event: journal.EventNodeFinished, Node: graph.NodeCode, Attempt: 1, DurationMS: cutShortMS,
				Status: journal.StatusAborted, Next: graph.NodeCode, Usage: &journal.Usage{},
			},
			{Event: journal.EventRunPaused},
			{Event: journal.EventRunResumed},
			{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 2},
			{
				Event: journal.EventNodeFinished, Node: graph.NodeCode, Attempt: 2, DurationMS: 100,
				Status: journal.StatusOK, Next: graph.End, Usage: &journal.Usage{},
			},
			{Event: journal.EventRunCompleted, Note: "run completed"},
		}
	}
	// cand-01 was stopped by a Ctrl-C 400ms into its first attempt; cand-02
	// by a kill at the same moment, which heal closes with no duration.
	seedCandidate(t, h, iso, seeded{id: "cand-01", index: 1, provision: state.ProvisionOK,
		envelope: oneEnvelope, status: state.RunStatusCompleted, step: 1, records: history(400)})
	seedCandidate(t, h, iso, seeded{id: "cand-02", index: 2, provision: state.ProvisionOK,
		envelope: oneEnvelope, status: state.RunStatusCompleted, step: 1, records: history(0)})

	node := &runsCandidates{specs: poolSpecs(h, 2)}
	code := newCandidateNode(graph.NodeCode, finishes(0.10))
	if _, err := h.poolDispatcher(t, pool{registry: candidateRegistry(t, code), isolator: iso}, node).
		Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, o := range node.outcomes {
		if o.DurationMS != 100 {
			t.Errorf("%s DurationMS = %d, want 100: the attempt that did the work, however the other was cut short",
				o.ID, o.DurationMS)
		}
	}
}
