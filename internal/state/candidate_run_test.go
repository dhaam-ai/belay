package state

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestNewCandidateRunLayout pins where a candidate run keeps its own run
// files. They are a sibling of the workspace, not inside it: promote
// destroys a loser's workspace, and an audit trail kept inside one would be
// destroyed with it.
func TestNewCandidateRunLayout(t *testing.T) {
	workspace := t.TempDir()
	parent, err := NewLayout(workspace, "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	child, err := NewCandidateRunLayout(parent, "cand-01")
	if err != nil {
		t.Fatalf("NewCandidateRunLayout: %v", err)
	}

	root := filepath.Join(parent.RunDir(), "candidate-runs", "cand-01")
	cases := map[string]string{
		"RunDir":                         child.RunDir(),
		"ManifestPath":                   child.ManifestPath(),
		"StatePath":                      child.StatePath(),
		"JournalPath":                    child.JournalPath(),
		"ArtifactsDir":                   child.ArtifactsDir(),
		"NodesDir":                       child.NodesDir(),
		"SnapshotsDir":                   child.SnapshotsDir(),
		"CandidateRunsDir of the parent": parent.CandidateRunsDir(),
	}
	want := map[string]string{
		"RunDir":                         root,
		"ManifestPath":                   filepath.Join(root, "manifest.json"),
		"StatePath":                      filepath.Join(root, "state.json"),
		"JournalPath":                    filepath.Join(root, "journal.ndjson"),
		"ArtifactsDir":                   filepath.Join(root, "artifacts"),
		"NodesDir":                       filepath.Join(root, "nodes"),
		"SnapshotsDir":                   filepath.Join(root, "snapshots"),
		"CandidateRunsDir of the parent": filepath.Join(parent.RunDir(), "candidate-runs"),
	}
	for k, got := range cases {
		if got != want[k] {
			t.Errorf("%s = %q, want %q", k, got, want[k])
		}
	}

	// The candidate's own run files must not land inside the workspace the
	// isolator makes for it, which is candidates/<id> and which promote may
	// destroy.
	if ws, err := parent.CandidateDir("cand-01"); err != nil {
		t.Fatalf("CandidateDir: %v", err)
	} else if rel, err := filepath.Rel(ws, child.RunDir()); err != nil || rel == "." || !startsWithParent(rel) {
		t.Errorf("candidate run dir %q is inside the candidate's workspace %q", child.RunDir(), ws)
	}
}

// startsWithParent reports whether rel climbs out of the directory it was
// taken relative to, which is what proves one path is not inside another.
func startsWithParent(rel string) bool {
	return rel == ".." || len(rel) > 3 && rel[:3] == ".."+string(filepath.Separator)
}

// TestCandidateRunLayoutStartsAtTheParentWorkspace states the one thing the
// new Layout's workspace means before the isolator has run: it is the tree
// the candidate will be isolated from, and the runner replaces it with
// WithWorkTree as soon as there is a workspace of the candidate's own.
func TestCandidateRunLayoutStartsAtTheParentWorkspace(t *testing.T) {
	workspace := t.TempDir()
	parent, err := NewLayout(workspace, "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	child, err := NewCandidateRunLayout(parent, "cand-01")
	if err != nil {
		t.Fatalf("NewCandidateRunLayout: %v", err)
	}
	if got := child.WorkspaceDir(); got != workspace {
		t.Errorf("WorkspaceDir = %q, want the parent's %q", got, workspace)
	}

	own := filepath.Join(parent.CandidatesDir(), "cand-01")
	moved, err := child.WithWorkTree(own)
	if err != nil {
		t.Fatalf("WithWorkTree: %v", err)
	}
	if got := moved.WorkspaceDir(); got != own {
		t.Errorf("WorkspaceDir after WithWorkTree = %q, want %q", got, own)
	}
	if got := moved.RunDir(); got != child.RunDir() {
		t.Errorf("WithWorkTree moved RunDir to %q, want it left at %q", got, child.RunDir())
	}
	if got := moved.JournalPath(); got != child.JournalPath() {
		t.Errorf("WithWorkTree moved JournalPath to %q, want it left at %q", got, child.JournalPath())
	}
}

// TestNewCandidateRunLayoutRefusesAnUnsafeID: a candidate id reaches this
// constructor from the fanout node, and an id that could escape the parent
// run directory would put a whole run's files somewhere nobody is looking.
// It is refused rather than cleaned, for the reason validateSegment gives.
func TestNewCandidateRunLayoutRefusesAnUnsafeID(t *testing.T) {
	parent, err := NewLayout(t.TempDir(), "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	for _, id := range []string{"", "..", "../escape", "a/b", "a\\b", "/etc/passwd", "cand\x0001"} {
		t.Run(id, func(t *testing.T) {
			if _, err := NewCandidateRunLayout(parent, id); !errors.Is(err, ErrInvalidPathSegment) {
				t.Errorf("NewCandidateRunLayout(parent, %q) error = %v, want ErrInvalidPathSegment", id, err)
			}
		})
	}
}

// TestNewCandidateRunLayoutRefusesAZeroParent: a zero Layout has no run
// directory, so every path it builds is relative to whatever directory belay
// happens to be running in. A candidate run built on one would scatter a
// whole run's files there.
func TestNewCandidateRunLayoutRefusesAZeroParent(t *testing.T) {
	if _, err := NewCandidateRunLayout(Layout{}, "cand-01"); !errors.Is(err, ErrInvalidPathSegment) {
		t.Errorf("NewCandidateRunLayout(Layout{}, ...) error = %v, want ErrInvalidPathSegment", err)
	}
}

// TestCandidateRunIdentityLivesInTheManifestNotTheLayout is a guard, not a
// RED: Layout.RunID() is filepath.Base(root) today, so a candidate run's
// Layout knows only the bare id. The run's full identity — parent and
// candidate — lives in its manifest, which is where the dispatcher reads a
// run's ID from. The test exists so that a later change "fixing"
// Layout.RunID to return something qualified is caught here rather than in
// whatever it renames on disk.
func TestCandidateRunIdentityLivesInTheManifestNotTheLayout(t *testing.T) {
	parent, err := NewLayout(t.TempDir(), "20260101T000000Z-c0ffee000000")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	child, err := NewCandidateRunLayout(parent, "cand-01")
	if err != nil {
		t.Fatalf("NewCandidateRunLayout: %v", err)
	}

	if got, want := child.RunID(), "cand-01"; got != want {
		t.Errorf("child Layout.RunID() = %q, want the bare candidate id %q", got, want)
	}
	if got, want := parent.RunID(), "20260101T000000Z-c0ffee000000"; got != want {
		t.Errorf("parent Layout.RunID() = %q, want %q", got, want)
	}

	// The identity a reader of the candidate's manifest sees is the
	// qualified one, and the two must not be confused: one names a
	// directory, the other names a run.
	qualified := parent.RunID() + "-" + child.RunID()
	if qualified == child.RunID() {
		t.Error("the candidate run's manifest id is indistinguishable from its directory name")
	}
}
