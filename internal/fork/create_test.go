//go:build unix

package fork_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/fork"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/isolate/dircopy"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

const forkID = "20260902T000000Z-f0f0f0f0f0f0"

var forkedAt = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)

// copies returns the isolator `belay fork` uses, rooted at the source
// workspace's .belay/forks.
func copies(t *testing.T, src *source) *dircopy.Isolator {
	t.Helper()
	root, err := state.ForksDir(src.ws)
	if err != nil {
		t.Fatalf("ForksDir: %v", err)
	}
	iso, err := dircopy.New(root)
	if err != nil {
		t.Fatalf("dircopy.New: %v", err)
	}
	return iso
}

// prepare decides a fork of src's visit of node, accepting the files as
// they are.
func prepare(t *testing.T, src *source, node string, visit int) *fork.Spec {
	t.Helper()
	req := src.request(node, visit)
	req.AcceptCurrentFiles = true
	spec, err := fork.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	return spec
}

// runFork dispatches f once with PauseAfter 1 under cfg, as `belay fork`
// does, over a fresh copy of the script, and returns the outcome and the
// nodes.
func runFork(t *testing.T, f fork.Fork, cfg config.Config) (graph.Outcome, map[string]*scripted) {
	t.Helper()
	nodes := map[string]*scripted{}
	reg := graph.NewRegistry()
	for _, n := range script(t) {
		nodes[n.Name()] = n.(*scripted)
		reg.MustRegister(n)
	}
	d, err := graph.NewDispatcher(graph.Options{
		Store: state.NewStore(f.Layout), Registry: reg, Config: cfg,
		Fingerprint: survey, PauseAfter: 1,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	out, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return out, nodes
}

// TestCreateMakesARunTheDispatcherStartsAtTheVisit is the whole promise of a
// fork: an ordinary run, in its own copy, whose dispatcher runs the forked
// node once, at attempt 1, as the same step, from exactly the blackboard the
// visit was given (less the agent's conversation), and then pauses. It owes
// nothing for the history it inherited, which it reports.
func TestCreateMakesARunTheDispatcherStartsAtTheVisit(t *testing.T) {
	src := newSource(t)
	spec := prepare(t, src, graph.NodeFix, 2)

	f, err := spec.Create(context.Background(), forkID, copies(t, src), forkedAt)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	forks, _ := state.ForksDir(src.ws)
	if want := filepath.Join(forks, forkID); f.Workspace.Dir != want || f.Layout.WorkspaceDir() != want {
		t.Errorf("fork works in %q (layout %q), want its own copy at %q", f.Workspace.Dir, f.Layout.WorkspaceDir(), want)
	}
	if f.Layout.RunID() != forkID || !strings.HasPrefix(f.Layout.RunDir(), f.Workspace.Dir) {
		t.Errorf("fork's run is at %q, want %s inside its copy", f.Layout.RunDir(), forkID)
	}

	// The journal: the inherited history, then the boundary, and nothing
	// of the visit itself.
	read, err := journal.ReadFile(f.Layout.JournalPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(read.Records) == 0 {
		t.Fatalf("the fork's journal at %s holds no records", f.Layout.JournalPath())
	}
	if diff := cmp.Diff(spec.Inherited, read.Records[:len(read.Records)-1]); diff != "" {
		t.Errorf("inherited records (-want +got):\n%s", diff)
	}
	boundary := read.Records[len(read.Records)-1]
	if boundary.Event != journal.EventRunStarted || boundary.Seq != spec.Visit.StartSeq {
		t.Errorf("last record = %s at seq %d, want run_started at %d", boundary.Event, boundary.Seq, spec.Visit.StartSeq)
	}

	// The blackboard: the snapshot, with no conversation.
	st, err := state.LoadState(f.Layout.StatePath())
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	want := spec.Snapshot.State
	want.Code.SessionID = ""
	if diff := cmp.Diff(want, st); diff != "" {
		t.Errorf("blackboard (-want +got):\n%s", diff)
	}

	// The manifest: a run of its own, in place, archiving nothing, owing
	// nothing, and saying where it came from.
	man, err := state.LoadManifest(f.Layout.ManifestPath())
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if man.RunID != forkID || man.Workspace != f.Workspace.Dir || man.Goal != goal {
		t.Errorf("manifest names run %q in %q for %q", man.RunID, man.Workspace, man.Goal)
	}
	if man.Step != spec.Snapshot.Step-1 || man.CurrentNode != graph.NodeFix || man.Status != state.RunStatusRunning {
		t.Errorf("manifest at step %d, node %q, %v; want step %d, fix, running",
			man.Step, man.CurrentNode, man.Status, spec.Snapshot.Step-1)
	}
	if man.Output.Branched() || man.ArchivedPlan != (state.ArchivedPlan{}) || man.Budget.SpentUSD != 0 {
		t.Errorf("manifest output %+v, archived plan %+v, spent %v; want in place, nothing, 0",
			man.Output, man.ArchivedPlan, man.Budget.SpentUSD)
	}
	wantOrigin := &state.Origin{
		RunID: sourceID, Workspace: src.ws, Tree: src.ws, Node: graph.NodeFix, Visit: 2,
		StartSeq: spec.Visit.StartSeq, InheritedUSD: spec.InheritedUSD, Deviations: spec.Deviations,
	}
	if diff := cmp.Diff(wantOrigin, man.Origin); diff != "" {
		t.Errorf("Origin (-want +got):\n%s", diff)
	}

	// The dispatch: fix once, as step 7 at attempt 1, from that blackboard,
	// in the copy, then a pause before test.
	out, nodes := runFork(t, f, src.cfg)
	if out.Status != state.RunStatusPaused || out.Node != graph.NodeTest || out.Steps != 1 {
		t.Errorf("Outcome = {%v, node %q, steps %d}, want {paused, test, 1}", out.Status, out.Node, out.Steps)
	}
	fix := nodes[graph.NodeFix]
	if fix.calls != 1 || nodes[graph.NodeTest].calls != 0 {
		t.Fatalf("fix ran %d times and test %d, want once and not at all", fix.calls, nodes[graph.NodeTest].calls)
	}
	rc := fix.seen[0]
	if rc.Step != spec.Snapshot.Step || rc.Attempt != 1 || rc.Workspace != f.Workspace.Dir {
		t.Errorf("fix ran as step %d, attempt %d, in %q; want step %d, attempt 1, in the copy",
			rc.Step, rc.Attempt, rc.Workspace, spec.Snapshot.Step)
	}
	if diff := cmp.Diff(want, rc.State); diff != "" {
		t.Errorf("fix was given (-want +got):\n%s", diff)
	}
	man, err = state.LoadManifest(f.Layout.ManifestPath())
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if man.Budget.SpentUSD != cost.USD {
		t.Errorf("fork spent %v after one node, want %v: only what it spent itself", man.Budget.SpentUSD, cost.USD)
	}
}

// fingerprintTree hashes every file and link under dir, skipping the
// directories named, so a test can say a tree did not change at all.
func fingerprintTree(t *testing.T, dir string, skip ...string) map[string]string {
	t.Helper()
	sums := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		for _, s := range skip {
			if rel == s {
				return fs.SkipDir
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			sums[rel] = "dir " + info.Mode().String()
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // the test's own tree
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		sums[rel] = info.Mode().String() + " " + info.ModTime().String() + " " + hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	return sums
}

// TestCreateLeavesTheSourceUntouched: preparing a fork, making it and running
// it changes no byte, mode or modification time of the source workspace or
// the source run, apart from the copy itself under .belay/forks.
func TestCreateLeavesTheSourceUntouched(t *testing.T) {
	src := newSource(t)
	forks := filepath.Join(".belay", "forks")
	before := fingerprintTree(t, src.ws, forks)

	spec := prepare(t, src, graph.NodeFix, 2)
	f, err := spec.Create(context.Background(), forkID, copies(t, src), forkedAt)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	runFork(t, f, src.cfg)

	if diff := cmp.Diff(before, fingerprintTree(t, src.ws, forks)); diff != "" {
		t.Errorf("the source changed (-before +after):\n%s", diff)
	}
}

// TestCreateCopiesOnlyTheArtifactsTheBlackboardNames: the fork's run holds
// the plan, and the reports its blackboard points at, and nothing the source
// wrote after the visit: a fork must not hold evidence from its source's
// future.
func TestCreateCopiesOnlyTheArtifactsTheBlackboardNames(t *testing.T) {
	src := newSource(t)
	spec := prepare(t, src, graph.NodeFix, 2)

	f, err := spec.Create(context.Background(), forkID, copies(t, src), forkedAt)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	entries, err := os.ReadDir(f.Layout.ArtifactsDir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	// fix #2 is step 7, after test #2 at step 6; test #3 at step 8 came later.
	want := []string{"plan.md", "test-0006.json"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("fork artifacts (-want +got):\n%s", diff)
	}
	data, err := os.ReadFile(filepath.Join(f.Layout.ArtifactsDir(), "plan.md")) //nolint:gosec // the test's own run
	if err != nil || string(data) != planText {
		t.Errorf("fork's plan = %q, %v; want the source's", data, err)
	}
}

// failing is an isolator whose copies cannot hold a run: each is created
// with a file where the run's .belay directory would go. It counts the
// workspaces it is asked to destroy.
type failing struct {
	root      string
	destroyed []belay.Workspace
}

func (f *failing) Name() string { return "failing" }

func (f *failing) Create(_ context.Context, _, id string) (belay.Workspace, error) {
	dir := filepath.Join(f.root, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return belay.Workspace{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, ".belay"), nil, 0o600); err != nil {
		return belay.Workspace{}, err
	}
	return belay.Workspace{ID: id, Dir: dir, Ephemeral: true}, nil
}

func (f *failing) Destroy(_ context.Context, ws belay.Workspace) error {
	f.destroyed = append(f.destroyed, ws)
	return os.RemoveAll(ws.Dir)
}

// TestCreateUnwindsWhatItMade: a fork that cannot be finished is not left
// half made. The copy is destroyed, so no run without a manifest is left for
// a person to puzzle over.
func TestCreateUnwindsWhatItMade(t *testing.T) {
	src := newSource(t)
	spec := prepare(t, src, graph.NodeFix, 2)
	iso := &failing{root: t.TempDir()}

	if _, err := spec.Create(context.Background(), forkID, iso, forkedAt); err == nil {
		t.Fatal("Create() error = nil, want the failure to make the run")
	}
	if len(iso.destroyed) != 1 || iso.destroyed[0].ID != forkID {
		t.Errorf("destroyed %+v, want the one copy Create made", iso.destroyed)
	}
	if _, err := os.Lstat(filepath.Join(iso.root, forkID)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the copy is still there: %v", err)
	}
}

// changing is dircopy, except that the tree changes while it copies.
type changing struct {
	*dircopy.Isolator
	t   *testing.T
	src string
}

func (c *changing) Create(ctx context.Context, src, id string) (belay.Workspace, error) {
	ws, err := c.Isolator.Create(ctx, src, id)
	writeFile(c.t, c.src, "main.go", "changed while it was being copied\n")
	return ws, err
}

// TestCreateRefusesACopyTakenWhileTheFilesChanged: Prepare checked the tree,
// and the copy is only that tree if nothing changed it in between. A copy
// that may hold some of each is destroyed rather than run.
func TestCreateRefusesACopyTakenWhileTheFilesChanged(t *testing.T) {
	src := newSource(t)
	spec := prepare(t, src, graph.NodeTest, 3)
	iso := &changing{Isolator: copies(t, src), t: t, src: src.ws}

	_, err := spec.Create(context.Background(), forkID, iso, forkedAt)
	if !errors.Is(err, fork.ErrFilesChanged) {
		t.Fatalf("Create() error = %v, want ErrFilesChanged", err)
	}
	forks, _ := state.ForksDir(src.ws)
	if _, err := os.Lstat(filepath.Join(forks, forkID)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the copy is still there: %v", err)
	}
}

// TestAForkCanBeForked: a fork's own visits fork like any run's, and its
// inherited ones are pointed at the run that holds their snapshots, under
// the number they have there. A fork of a fork reports the whole cost of the
// history it starts from.
func TestAForkCanBeForked(t *testing.T) {
	src := newSource(t)
	spec := prepare(t, src, graph.NodeFix, 2)
	f, err := spec.Create(context.Background(), forkID, copies(t, src), forkedAt)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	runFork(t, f, src.cfg)
	again := &source{ws: f.Workspace.Dir, layout: f.Layout, cfg: src.cfg}

	// The fork paused before test; forking needs a run that has stopped.
	_, err = fork.Prepare(context.Background(), again.request(graph.NodeTest, 1))
	if !errors.Is(err, fork.ErrNoSnapshot) {
		t.Fatalf("Prepare(inherited test #1) error = %v, want ErrNoSnapshot", err)
	}
	for _, phrase := range []string{sourceID, "fork " + sourceID + " test --visit 1"} {
		if !strings.Contains(err.Error(), phrase) {
			t.Errorf("Prepare(inherited) error = %q, want it to say %q", err, phrase)
		}
	}

	req := again.request(graph.NodeFix, 2)
	req.AcceptCurrentFiles = true
	second, err := fork.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("Prepare(the fork's own fix #2) error = %v", err)
	}
	if second.InheritedUSD != spec.InheritedUSD {
		t.Errorf("InheritedUSD = %v, want %v: all of it from the first source, none from the fork", second.InheritedUSD, spec.InheritedUSD)
	}
}

// A fork's copy is taken part-way through its source run's change, so
// nothing in it can say where that change began: neither git (the copy has
// no .git) nor a record of the copy, which would read the source's change as
// the starting point. The fork's manifest says so, and its gate says it in
// every verdict, rather than judging against a starting point that is wrong.
func TestAForkRecordsThatItCanJudgeOnlyTheAgentsReport(t *testing.T) {
	src := newSource(t)
	spec := prepare(t, src, graph.NodeFix, 2)

	f, err := spec.Create(context.Background(), forkID, copies(t, src), forkedAt)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	man, err := state.LoadManifest(f.Layout.ManifestPath())
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	want := state.Changes{From: state.ChangesReported, Why: fork.Unrecorded}
	if man.Changes == nil || *man.Changes != want {
		t.Errorf("fork manifest Changes = %+v, want %+v", man.Changes, want)
	}
}

// TestAForkPinsTheRoundItWasGiven: a fork of a fix visit that used every
// round of its loop runs under graph.give_up one above the rounds used
// (TestPrepareGivesAFixThatGaveUpOneMoreRound). The round is the fork's own
// decision, not anything its copy's belay.yaml says, so it is pinned in the
// fork's manifest with the capability a run depending on it needs: resumed
// after a Ctrl-C or a crash, the fork keeps it. A fork of a visit with
// rounds left was given nothing, and pins nothing.
func TestAForkPinsTheRoundItWasGiven(t *testing.T) {
	tests := []struct {
		name      string
		attempts  int
		wantFloor int
	}{
		{name: "every round used", attempts: 3, wantFloor: 4},
		{name: "rounds left", attempts: 1, wantFloor: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newSource(t)
			src.rewriteSnapshot(t, graph.NodeFix, 2, func(s *state.Snapshot) { s.State.Fix.Attempts = tt.attempts })
			spec := prepare(t, src, graph.NodeFix, 2)

			f, err := spec.Create(context.Background(), forkID, copies(t, src), forkedAt)
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			man, err := state.LoadManifest(f.Layout.ManifestPath())
			if err != nil {
				t.Fatalf("LoadManifest: %v", err)
			}
			if man.GiveUpFloor != tt.wantFloor {
				t.Errorf("the fork pins graph.give_up %d, want %d", man.GiveUpFloor, tt.wantFloor)
			}
			if required := slices.Contains(man.Requires, state.CapabilityForkGiveUp); required != (tt.wantFloor > 0) {
				t.Errorf("the fork requires %q = %v, want %v; requires %v",
					state.CapabilityForkGiveUp, required, tt.wantFloor > 0, man.Requires)
			}
		})
	}
}
