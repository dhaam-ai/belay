package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCreateRunKeepsRunStateOutOfCommits: the first run in a repository
// writes .belay/.gitignore holding "*", so nothing belay keeps there — a
// run's journal, its manifest, its worktree — is ever offered for a commit.
// The proof is git's own: straight after the run is created, `git status`
// shows nothing at all, not even the .belay directory.
func TestCreateRunKeepsRunStateOutOfCommits(t *testing.T) {
	dir := gitWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if _, err := createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: "20260101T000000Z-c0ffee000000", Goal: "add tests"}); err != nil {
		t.Fatalf("createRun: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, ".belay", ".gitignore")) //nolint:gosec // under the test's own t.TempDir()
	if err != nil {
		t.Fatalf("read .belay/.gitignore: %v", err)
	}
	if !slices.Contains(strings.Split(string(body), "\n"), "*") {
		t.Errorf(".belay/.gitignore holds %q, with no line ignoring everything", body)
	}
	if got := gitIn(t, dir, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("git status shows belay's run state after a run was created:\n%s", got)
	}
}

// TestCreateRunLeavesAPersonsOwnGitignoreAlone: a .belay/.gitignore that is
// already there is somebody's decision, even if it is not belay's, and a
// second run changes nothing either.
func TestCreateRunLeavesAPersonsOwnGitignoreAlone(t *testing.T) {
	ws := goWorkspace(t)
	theirs := filepath.Join(ws, ".belay", ".gitignore")
	if err := os.MkdirAll(filepath.Dir(theirs), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const mine = "runs/\n!cassette.json\n"
	if err := os.WriteFile(theirs, []byte(mine), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	for _, id := range []string{"20260101T000000Z-c0ffee000001", "20260101T000000Z-c0ffee000002"} {
		if _, err := createRun(t.Context(), plan, runSpec{Workspace: ws, RunID: id, Goal: "add tests"}); err != nil {
			t.Fatalf("createRun %s: %v", id, err)
		}
	}

	body, err := os.ReadFile(theirs) //nolint:gosec // under the test's own t.TempDir()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != mine {
		t.Errorf(".belay/.gitignore was rewritten to %q; it was somebody's own", body)
	}
}
