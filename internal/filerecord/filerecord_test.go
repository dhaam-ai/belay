package filerecord_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/filerecord"
)

// tree writes files under a fresh directory and returns it.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	for rel, body := range files {
		write(t, dir, rel, body)
	}
	return dir
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// A workspace with no git repository still has a ground truth: what its
// files were when the run started. Every way a file can differ is found,
// whatever the agent reported, and belay's own .belay and any .git are not
// part of it.
func TestChangedFindsEveryDifferenceFromTheRecord(t *testing.T) {
	t.Parallel()
	dir := tree(t, map[string]string{
		"src/app.py":    "def add(a, b):\n    return a + b\n",
		"infra/main.tf": "resource \"x\" \"y\" {}\n",
		"old.txt":       "old\n",
		"gone.txt":      "gone\n",
		"same.txt":      "same\n",
	})
	rec, err := filerecord.Take(t.Context(), dir, nil)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}

	write(t, dir, "src/app.py", "def add(a, b):\n    return a + b + 0\n")
	write(t, dir, "infra/main.tf", "resource \"evil\" { backdoor = true }\n")
	if err := os.Rename(filepath.Join(dir, "old.txt"), filepath.Join(dir, "new.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "brand/new.txt", "new\n")
	if err := os.Chmod(filepath.Join(dir, "same.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, ".belay/runs/r/journal.ndjson", "belay's own\n")
	write(t, dir, ".git/HEAD", "not a change\n")
	write(t, dir, "nested/.git/config", "not a change either\n")

	got, err := rec.Changed(t.Context(), dir)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	want := []string{"brand/new.txt", "gone.txt", "infra/main.tf", "link", "new.txt", "old.txt", "same.txt", "src/app.py"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Changed (-want +got):\n%s", diff)
	}
}

// The check that a deliberately misreporting agent would try to beat: put
// the bytes back to the same size and the modification time back to what
// it was. The content is compared, so the edit is still found.
func TestChangedSeesThroughARestoredModificationTime(t *testing.T) {
	t.Parallel()
	dir := tree(t, map[string]string{"infra/main.tf": "resource \"x\" \"y\" {}\n"})
	path := filepath.Join(dir, "infra", "main.tf")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := filerecord.Take(t.Context(), dir, nil)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}

	write(t, dir, "infra/main.tf", "resource \"z\" \"w\" {}\n") // same length
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}

	got, err := rec.Changed(t.Context(), dir)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if diff := cmp.Diff([]string{"infra/main.tf"}, got); diff != "" {
		t.Errorf("Changed (-want +got):\n%s", diff)
	}
}

// A file rewritten with exactly the bytes it had is not a change, however
// its timestamps moved: a formatter that rewrites a file it did not alter,
// or a touch, must not read as the run having touched a forbidden path.
func TestChangedIgnoresAFileRewrittenUnchanged(t *testing.T) {
	t.Parallel()
	dir := tree(t, map[string]string{"infra/main.tf": "same\n"})
	rec, err := filerecord.Take(t.Context(), dir, nil)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	write(t, dir, "infra/main.tf", "same\n")

	got, err := rec.Changed(t.Context(), dir)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Changed = %q, want nothing: the bytes are the same", got)
	}
}

// A record of some paths only — the files a person had already changed in
// a git checkout before the run began — answers for those paths and no
// others: a file it never recorded is git's to report.
func TestAPartialRecordAnswersOnlyForItsOwnPaths(t *testing.T) {
	t.Parallel()
	dir := tree(t, map[string]string{"dirty.txt": "the person's edit\n", "kept.txt": "untouched\n", "other.txt": "x\n"})
	rec, err := filerecord.Take(t.Context(), dir, []string{"dirty.txt", "kept.txt", "absent.txt"})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got := rec.Paths(); !cmp.Equal(got, []string{"absent.txt", "dirty.txt", "kept.txt"}) {
		t.Errorf("Paths = %q, want the three it was asked to record", got)
	}

	write(t, dir, "dirty.txt", "the agent's edit on top\n")
	write(t, dir, "absent.txt", "created by the run\n")
	write(t, dir, "other.txt", "changed, but not recorded here\n")

	got, err := rec.Changed(t.Context(), dir)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if diff := cmp.Diff([]string{"absent.txt", "dirty.txt"}, got); diff != "" {
		t.Errorf("Changed (-want +got):\n%s", diff)
	}
}

// The record outlives the process that took it: a resumed run's gate judges
// against the one taken when the run started.
func TestARecordSurvivesASaveAndALoad(t *testing.T) {
	t.Parallel()
	dir := tree(t, map[string]string{"a.txt": "a\n"})
	rec, err := filerecord.Take(t.Context(), dir, nil)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	path := filepath.Join(t.TempDir(), "record.json")
	if err := rec.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := filerecord.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	write(t, dir, "a.txt", "changed\n")
	got, err := loaded.Changed(t.Context(), dir)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if diff := cmp.Diff([]string{"a.txt"}, got); diff != "" {
		t.Errorf("Changed after a load (-want +got):\n%s", diff)
	}
	if _, err := filerecord.Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("Load of a missing record succeeded; a gate with no record must not read as one with no change")
	}
}
