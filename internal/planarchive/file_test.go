package planarchive

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/state"
)

const (
	testRunID = "20260905T142530Z-1f2e3d4c5b6a"
	testGoal  = "add tests"
)

// run is a run on disk for File to act on, with the paths a test checks.
type run struct {
	store *state.Store
	// source is the run's own copy of the plan, under its artifacts.
	source string
	// root is the tree the run's nodes worked in: the workspace, or a
	// branch run's worktree.
	root string
	// dest is where File puts the plan, and rel the same path relative to
	// root, as the manifest records it.
	dest, rel string
}

// runOpts describes the run newRun lays down. The zero value is a completed
// in-place run that pinned docs/plans, whose plan is the approved one.
type runOpts struct {
	status state.RunStatus
	// dir is the pinned archive directory; "-" pins none.
	dir string
	// branchTree, when set, makes the run a branch run whose nodes worked
	// in that directory, as its worktree.
	branchTree string
}

// newRun lays down a finished run the way the dispatcher leaves one: its
// plan artifact, a state.json whose Plan names and fingerprints it, and a
// manifest.
func newRun(t *testing.T, workspace string, o runOpts) run {
	t.Helper()
	layout, err := state.NewLayout(workspace, testRunID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := state.ReplaceArtifact(layout, "plan.md", testPlan); err != nil {
		t.Fatalf("write the plan artifact: %v", err)
	}
	st := state.NewState(testGoal)
	st.Plan = state.Plan{Path: "artifacts/plan.md", Approved: true, Digest: approve.Digest(testPlan)}
	if err := state.SaveState(layout.StatePath(), st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	cfg := config.Default()
	switch o.dir {
	case "":
	case "-":
		cfg.Plan.Archive = false
	default:
		cfg.Plan.ArchiveDir = o.dir
	}
	man := state.NewManifest(time.Now().UTC(), testRunID, workspace, testGoal, cfg)
	man.Status = state.RunStatusCompleted
	if o.status != state.RunStatusUnknown {
		man.Status = o.status
	}
	root := workspace
	if o.branchTree != "" {
		man.SetOutput(state.Output{
			Mode: state.OutputBranch, Repo: workspace, Branch: "belay/" + testRunID,
			Base: "1111111", WorkTree: o.branchTree,
		})
		if layout, err = layout.WithWorkTree(o.branchTree); err != nil {
			t.Fatalf("WithWorkTree: %v", err)
		}
		root = o.branchTree
	}
	if err := state.SaveManifest(layout.ManifestPath(), man); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	rel := Rel(man.ArchivedPlan.Dir, testRunID, testGoal)
	source, err := layout.ArtifactPath("plan.md")
	if err != nil {
		t.Fatalf("ArtifactPath: %v", err)
	}
	return run{
		store: state.NewStore(layout), source: source, root: root, rel: rel,
		dest: filepath.Join(root, filepath.FromSlash(rel)),
	}
}

// pointer returns the archived plan path the run's manifest records.
func (r run) pointer(t *testing.T) string {
	t.Helper()
	man, err := r.store.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	return man.ArchivedPlan.Path
}

// exists reports whether path exists.
func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return err == nil
}

// TestFileMovesThePlanIntoTheWorkspace is the whole move for a run that
// edits its workspace in place: afterwards the plan is in the repository,
// byte for byte the plan the run's code was written from, the manifest says
// where, and the run's own copy is gone. A second call finds the work done.
func TestFileMovesThePlanIntoTheWorkspace(t *testing.T) {
	r := newRun(t, t.TempDir(), runOpts{})

	res, err := File(r.store)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if res.Outcome != Filed || res.Rel != r.rel || res.Path != r.dest {
		t.Errorf("File = %+v, want Filed at %s (%s)", res, r.rel, r.dest)
	}
	if got := readFile(t, r.dest); approve.Digest(got) != approve.Digest(testPlan) {
		t.Errorf("the archived plan holds %q, not the plan the run approved", got)
	}
	if got := r.pointer(t); got != r.rel {
		t.Errorf("manifest archived_plan.path = %q, want %q", got, r.rel)
	}
	if exists(t, r.source) {
		t.Errorf("the run's copy %s is still there; the plan was copied, not moved", r.source)
	}

	again, err := File(r.store)
	if err != nil {
		t.Fatalf("File again: %v", err)
	}
	if again.Outcome != AlreadyFiled || again.Rel != r.rel {
		t.Errorf("File again = %+v, want AlreadyFiled at %s", again, r.rel)
	}
	if got := listTree(t, filepath.Dir(r.dest)); slices.Compare(got, []string{filepath.Base(r.dest)}) != 0 {
		t.Errorf("docs/plans holds %v, want only the one plan", got)
	}
}

// TestFileConvergesFromEveryCrashPoint stops File after each durable step,
// the way a kill -9 would, and runs it again. At every stopping point the
// plan exists somewhere, and the re-run ends where an uninterrupted call
// would: one archived copy, never a second, the pointer set, the run's copy
// gone. The first row is the case the move is built around: the copy is in
// the repository and the manifest does not know yet.
func TestFileConvergesFromEveryCrashPoint(t *testing.T) {
	for _, stop := range []fileStage{stagePlaced, stageRecorded} {
		t.Run(stop.String(), func(t *testing.T) {
			r := newRun(t, t.TempDir(), runOpts{})

			if _, err := fileUpTo(r.store, stop); err != nil {
				t.Fatalf("fileUpTo(%s): %v", stop, err)
			}
			if !exists(t, r.dest) && !exists(t, r.source) {
				t.Fatalf("after stopping at %s the plan is in neither place", stop)
			}

			res, err := File(r.store)
			if err != nil {
				t.Fatalf("File after stopping at %s: %v", stop, err)
			}
			if res.Outcome != Filed {
				t.Errorf("File after stopping at %s = %+v, want Filed", stop, res)
			}
			if got := listTree(t, filepath.Dir(r.dest)); slices.Compare(got, []string{filepath.Base(r.dest)}) != 0 {
				t.Errorf("docs/plans holds %v, want exactly one archived plan", got)
			}
			if got := readFile(t, r.dest); string(got) != string(testPlan) {
				t.Errorf("the archived plan holds %q, want %q", got, testPlan)
			}
			if got := r.pointer(t); got != r.rel {
				t.Errorf("manifest archived_plan.path = %q, want %q", got, r.rel)
			}
			if exists(t, r.source) {
				t.Errorf("the run's copy %s is still there", r.source)
			}
		})
	}
}

// TestFileKeepsThePlanWhenItCannotMoveIt covers every reason the plan stays
// where it is. Each one leaves the run's copy untouched and records no
// pointer, and each gives a reason naming what stopped it, because the plan
// staying behind is only harmless if the person is told why.
func TestFileKeepsThePlanWhenItCannotMoveIt(t *testing.T) {
	tests := []struct {
		name string
		opts runOpts
		// arrange changes the run before File is called.
		arrange func(t *testing.T, r run)
		// reason is a phrase the Result's Reason must contain.
		reason string
	}{
		{
			name:    "somebody's file is already where the plan would go",
			arrange: func(t *testing.T, r run) { writeFile(t, r.dest, []byte("my own notes\n")) },
			reason:  "already",
		},
		{
			name: "docs leads out of the workspace",
			arrange: func(t *testing.T, r run) {
				if err := os.Symlink(t.TempDir(), filepath.Join(r.root, "docs")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			reason: "outside",
		},
		{
			name:    "the plan changed after the run last read it",
			arrange: func(t *testing.T, r run) { writeFile(t, r.source, []byte("## Steps\n\n1. Something else.\n")) },
			reason:  "changed",
		},
		{name: "the run is paused", opts: runOpts{status: state.RunStatusPaused}, reason: "not finished"},
		{name: "the run failed", opts: runOpts{status: state.RunStatusFailed}, reason: "not finished"},
		{name: "the run was stopped", opts: runOpts{status: state.RunStatusAborted}, reason: "not finished"},
		{name: "the run is still going", opts: runOpts{status: state.RunStatusRunning}, reason: "not finished"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRun(t, t.TempDir(), tt.opts)
			if tt.arrange != nil {
				tt.arrange(t, r)
			}
			sourceBefore := readFile(t, r.source)
			destBefore := exists(t, r.dest)

			res, err := File(r.store)
			if err != nil {
				t.Fatalf("File: %v", err)
			}
			if res.Outcome != Kept {
				t.Fatalf("File = %+v, want Kept", res)
			}
			if !strings.Contains(res.Reason, tt.reason) {
				t.Errorf("Reason = %q, want it to say %q", res.Reason, tt.reason)
			}
			if res.Source != r.source {
				t.Errorf("Source = %q, want the run's copy %s", res.Source, r.source)
			}
			if got := readFile(t, r.source); string(got) != string(sourceBefore) {
				t.Errorf("the run's copy changed from %q to %q", sourceBefore, got)
			}
			if got := r.pointer(t); got != "" {
				t.Errorf("manifest archived_plan.path = %q for a plan that did not move", got)
			}
			if !destBefore && exists(t, r.dest) {
				t.Errorf("File wrote %s although it kept the plan", r.dest)
			}
		})
	}
}

// A run whose manifest pins no directory was never told its plan would
// move: plan.archive was off when it started, or it started before belay
// archived plans. File leaves it alone and says so, and says nothing else.
func TestFileLeavesARunThatDidNotAskAlone(t *testing.T) {
	r := newRun(t, t.TempDir(), runOpts{dir: "-"})

	res, err := File(r.store)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if res.Outcome != NotAsked {
		t.Errorf("File = %+v, want NotAsked", res)
	}
	if !exists(t, r.source) {
		t.Error("the run's copy of the plan is gone")
	}
	if exists(t, filepath.Join(r.root, "docs")) {
		t.Error("File created docs/ for a run that did not ask for archival")
	}
}

// The destination is the run's workspace, never the directory belay happens
// to be running in: `belay run --workspace ../other-repo` must file the plan
// in ../other-repo.
func TestFileUsesTheRunsWorkspaceNotTheCurrentDirectory(t *testing.T) {
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)
	r := newRun(t, t.TempDir(), runOpts{})

	if _, err := File(r.store); err != nil {
		t.Fatalf("File: %v", err)
	}
	if !exists(t, r.dest) {
		t.Errorf("the plan is not at %s", r.dest)
	}
	if got := listTree(t, elsewhere); len(got) != 0 {
		t.Errorf("File wrote into the current directory: %v", got)
	}
}

// TestFileInABranchRunOnlyAdoptsWhatTheCommitHas is the branch half. The
// promote node writes the plan into the run's worktree before its one
// commit, so after the run the worktree already holds it and File only has
// to record where it went and remove the run's copy. File never writes the
// plan into a worktree itself: after the commit, a file written there would
// be in no commit, and the worktree it sits in is removed as the run
// finishes, so moving the plan there would lose it.
func TestFileInABranchRunOnlyAdoptsWhatTheCommitHas(t *testing.T) {
	t.Run("the commit has it", func(t *testing.T) {
		ws := t.TempDir()
		r := newRun(t, ws, runOpts{branchTree: filepath.Join(t.TempDir(), "worktree")})
		writeFile(t, r.dest, testPlan)

		res, err := File(r.store)
		if err != nil {
			t.Fatalf("File: %v", err)
		}
		if res.Outcome != Filed || res.Rel != r.rel {
			t.Errorf("File = %+v, want Filed at %s", res, r.rel)
		}
		if got := r.pointer(t); got != r.rel {
			t.Errorf("manifest archived_plan.path = %q, want %q", got, r.rel)
		}
		if exists(t, r.source) {
			t.Errorf("the run's copy %s is still there", r.source)
		}
		if exists(t, filepath.Join(ws, "docs")) {
			t.Error("File wrote into the person's checkout, which a branch run never touches")
		}
	})
	t.Run("the commit does not have it", func(t *testing.T) {
		tree := filepath.Join(t.TempDir(), "worktree")
		if err := os.MkdirAll(tree, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		r := newRun(t, t.TempDir(), runOpts{branchTree: tree})

		res, err := File(r.store)
		if err != nil {
			t.Fatalf("File: %v", err)
		}
		if res.Outcome != Kept || !strings.Contains(res.Reason, "commit") {
			t.Errorf("File = %+v, want Kept, saying the commit does not have the plan", res)
		}
		if got := listTree(t, tree); len(got) != 0 {
			t.Errorf("File wrote into the worktree after its commit: %v", got)
		}
		if !exists(t, r.source) {
			t.Error("the run's copy of the plan is gone although the commit does not have it")
		}
	})
}
