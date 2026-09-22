//go:build unix

package git

import (
	"testing"
)

// TestFileAtReadsAFileAsACommitHoldsIt is what `belay plan` asks of a
// finished branch run: its plan lives in the run's commit and, once the
// run's worktree is gone, nowhere else on disk. The commit is named by its
// full id, never a branch, because a branch can have moved on to somebody
// else's version of the file since.
func TestFileAtReadsAFileAsACommitHoldsIt(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("r")
	f.write(dir, "docs/plans/x.md", "## Steps\n\n1. First.\n")
	f.git(dir, "add", "-A")
	f.git(dir, "commit", "-qm", "with the plan")
	commit := f.git(dir, "rev-parse", "HEAD")
	// The checkout moves on; the commit does not.
	f.write(dir, "docs/plans/x.md", "somebody's edit\n")
	f.git(dir, "commit", "-qam", "edited")
	repo := f.open(dir)

	got, err := repo.FileAt(t.Context(), commit, "docs/plans/x.md")
	if err != nil {
		t.Fatalf("FileAt: %v", err)
	}
	if string(got) != "## Steps\n\n1. First.\n" {
		t.Errorf("FileAt = %q, want the file as the commit holds it", got)
	}

	for _, tc := range []struct {
		name, commit, path string
	}{
		{name: "a path the commit does not have", commit: commit, path: "docs/plans/missing.md"},
		{name: "a branch rather than a commit", commit: "main", path: "docs/plans/x.md"},
		{name: "an abbreviated commit", commit: commit[:12], path: "docs/plans/x.md"},
		{name: "a path climbing out", commit: commit, path: "../x.md"},
		{name: "an absolute path", commit: commit, path: "/etc/passwd"},
		{name: "no path", commit: commit, path: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := repo.FileAt(t.Context(), tc.commit, tc.path); err == nil {
				t.Errorf("FileAt(%q, %q) = %q, want an error", tc.commit, tc.path, got)
			}
		})
	}
}
