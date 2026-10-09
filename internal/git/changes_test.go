//go:build unix

package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// Changes is the ground truth the policy gate judges, so every way a file
// can differ from the run's base has to be in it, whoever reported what: a
// commit the agent made itself, a staged edit, an unstaged one, both sides
// of a rename, a deletion and a new file. What git does not record — an
// ignored file, belay's own .belay — is not.
func TestChangesListsEverythingThatDiffersFromTheBase(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("changes")
	f.write(dir, ".gitignore", "*.log\n")
	f.write(dir, "old.txt", "old\n")
	f.write(dir, "docs/gone.md", "gone\n")
	f.git(dir, "add", "-A")
	f.git(dir, "commit", "-qm", "base")
	base := f.git(dir, "rev-parse", "HEAD")

	f.write(dir, "committed.txt", "an agent can commit\n")
	f.git(dir, "add", "committed.txt")
	f.git(dir, "commit", "-qm", "the agent's own commit")
	f.write(dir, "a.txt", "staged\n")
	f.git(dir, "add", "a.txt")
	f.write(dir, "sub/b.txt", "unstaged\n")
	f.git(dir, "mv", "old.txt", "new.txt")
	f.write(dir, "untracked.txt", "new\n")
	f.write(dir, "with space\nand newline.txt", "odd\n")
	f.write(dir, "build.log", "ignored\n")
	f.write(dir, ".belay/runs/r/journal.ndjson", "belay's own\n")
	if err := os.Remove(filepath.Join(dir, "docs", "gone.md")); err != nil {
		t.Fatalf("remove docs/gone.md: %v", err)
	}

	got, err := f.open(dir).Changes(t.Context(), dir, base)
	if err != nil {
		t.Fatalf("Changes() error = %v", err)
	}
	want := []string{
		"a.txt", "committed.txt", "docs/gone.md", "new.txt", "old.txt",
		"sub/b.txt", "untracked.txt", "with space\nand newline.txt",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Changes() (-want +got):\n%s", diff)
	}
}

// A run started in a subdirectory is judged on that subdirectory, by paths
// relative to it, and keeps its .belay there rather than at the top.
func TestChangesIsScopedToTheDirectoryItIsGiven(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("changes-sub")
	base := f.git(dir, "rev-parse", "HEAD")
	sub := filepath.Join(dir, "sub")

	f.write(dir, "a.txt", "outside the run's directory\n")
	f.write(sub, "b.txt", "inside\n")
	f.write(sub, "deeper/c.txt", "new\n")
	f.write(sub, ".belay/runs/r/state.json", "the run's own\n")

	got, err := f.open(sub).Changes(t.Context(), sub, base)
	if err != nil {
		t.Fatalf("Changes() error = %v", err)
	}
	if diff := cmp.Diff([]string{"b.txt", "deeper/c.txt"}, got); diff != "" {
		t.Errorf("Changes(sub) (-want +got):\n%s", diff)
	}
}

// An untouched tree has no change, and says so with an empty list.
func TestChangesOfACleanTreeIsEmpty(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("changes-clean")
	got, err := f.open(dir).Changes(t.Context(), dir, f.git(dir, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatalf("Changes() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Changes() of a clean tree = %q, want none", got)
	}
}

// Changes refuses a question whose answer could be a false "nothing
// changed": a base that is not a full commit id (a branch can move), and a
// directory belay itself made inside the repository's .belay, where git
// sees the repository around it and excludes every file in it.
func TestChangesRefusesAQuestionItCannotAnswerTruthfully(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("changes-refuse")
	base := f.git(dir, "rev-parse", "HEAD")
	// A copy at the top's .belay, and one in the .belay of a run started in
	// a subdirectory, which belay's own .belay/.gitignore hides entirely.
	topCopy := filepath.Join(dir, ".belay", "queue", "work", "goal-1")
	subCopy := filepath.Join(dir, "sub", ".Belay", "forks", "f1")
	f.write(topCopy, "a.txt", "a copy belay made\n")
	f.write(dir, "sub/.Belay/.gitignore", "*\n")
	f.write(subCopy, "a.txt", "another copy belay made\n")
	r := f.open(dir)

	if _, err := r.Changes(t.Context(), dir, "main"); !errors.Is(err, ErrInvalidObjectID) {
		t.Errorf("Changes(base=main) error = %v, want one wrapping ErrInvalidObjectID", err)
	}
	for _, copyDir := range []string{topCopy, subCopy} {
		if _, err := r.Changes(t.Context(), copyDir, base); !errors.Is(err, ErrInsideBelay) {
			t.Errorf("Changes(%s) error = %v, want one wrapping ErrInsideBelay", copyDir, err)
		}
	}
}
