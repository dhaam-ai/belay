//go:build fixrate

package fixrate

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func TestSnapshotWorkspace_HashesTestFilesAndTrackedSources(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "policy.go", "package backoff\n")
	writeFile(t, dir, "policy_test.go", "package backoff\nfunc TestX(t *testing.T){}\n")
	writeFile(t, dir, "helpers_test.go", "package backoff\n")
	writeFile(t, dir, "README.md", "not go\n")

	snap, err := snapshotWorkspace(dir, []string{"policy.go"})
	if err != nil {
		t.Fatalf("snapshotWorkspace() error = %v", err)
	}
	if len(snap.testFiles) != 2 {
		t.Errorf("testFiles = %v, want exactly the two *_test.go files", snap.testFiles)
	}
	if _, ok := snap.testFiles["policy_test.go"]; !ok {
		t.Errorf("testFiles missing policy_test.go: %v", snap.testFiles)
	}
	if _, ok := snap.sourceFiles["policy.go"]; !ok {
		t.Errorf("sourceFiles missing policy.go: %v", snap.sourceFiles)
	}
}

func TestSnapshotWorkspace_SkipsBelayDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "policy.go", "package backoff\n")
	writeFile(t, dir, ".belay/runs/x/artifacts/foo_test.go", "should not be scanned\n")

	snap, err := snapshotWorkspace(dir, nil)
	if err != nil {
		t.Fatalf("snapshotWorkspace() error = %v", err)
	}
	if len(snap.testFiles) != 0 {
		t.Errorf("testFiles = %v, want none: .belay/ must be skipped", snap.testFiles)
	}
}

func TestAnyTestFileChanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "policy_test.go", "v1\n")
	before, err := snapshotWorkspace(dir, nil)
	if err != nil {
		t.Fatalf("snapshotWorkspace() error = %v", err)
	}

	// Unchanged.
	after, err := snapshotWorkspace(dir, nil)
	if err != nil {
		t.Fatalf("snapshotWorkspace() error = %v", err)
	}
	if anyTestFileChanged(before, after) {
		t.Error("anyTestFileChanged() = true for an untouched tree, want false")
	}

	// Content changed.
	writeFile(t, dir, "policy_test.go", "v2\n")
	after, err = snapshotWorkspace(dir, nil)
	if err != nil {
		t.Fatalf("snapshotWorkspace() error = %v", err)
	}
	if !anyTestFileChanged(before, after) {
		t.Error("anyTestFileChanged() = false after editing a test file's content, want true")
	}

	// A new test file appearing counts as changed too.
	dir2 := t.TempDir()
	writeFile(t, dir2, "policy_test.go", "v1\n")
	before2, err := snapshotWorkspace(dir2, nil)
	if err != nil {
		t.Fatalf("snapshotWorkspace() error = %v", err)
	}
	writeFile(t, dir2, "new_test.go", "new\n")
	after2, err := snapshotWorkspace(dir2, nil)
	if err != nil {
		t.Fatalf("snapshotWorkspace() error = %v", err)
	}
	if !anyTestFileChanged(before2, after2) {
		t.Error("anyTestFileChanged() = false after adding a new test file, want true")
	}
}

func TestSourceChanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "policy.go", "v1\n")
	before, err := snapshotWorkspace(dir, []string{"policy.go"})
	if err != nil {
		t.Fatalf("snapshotWorkspace() error = %v", err)
	}

	after, err := snapshotWorkspace(dir, []string{"policy.go"})
	if err != nil {
		t.Fatalf("snapshotWorkspace() error = %v", err)
	}
	if sourceChanged(before, after, "policy.go") {
		t.Error("sourceChanged() = true for an untouched file, want false")
	}

	writeFile(t, dir, "policy.go", "v2\n")
	after, err = snapshotWorkspace(dir, []string{"policy.go"})
	if err != nil {
		t.Fatalf("snapshotWorkspace() error = %v", err)
	}
	if !sourceChanged(before, after, "policy.go") {
		t.Error("sourceChanged() = false after editing the file, want true")
	}

	// A path never tracked in either snapshot is treated as changed
	// (fail safe), never silently "unchanged".
	if !sourceChanged(before, after, "never-tracked.go") {
		t.Error("sourceChanged() for an untracked path = false, want true (fail safe)")
	}
}
