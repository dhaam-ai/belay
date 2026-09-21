//go:build unix

package dircopy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// surveyBase is the fixed mtime every fixture file starts with, so a row can
// move a file's mtime deliberately, or put it back.
var surveyBase = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// surveyTree builds a small checkout: source files, an executable, a
// directory, an internal symlink, and the directories a copy skips, with
// something in each.
func surveyTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n", 0o644)
	writeFile(t, filepath.Join(root, "src", "lib.go"), "package src\n", 0o644)
	writeFile(t, filepath.Join(root, "run.sh"), "#!/bin/sh\n", 0o755)
	symlink(t, "src/lib.go", filepath.Join(root, "lib-link"))
	writeFile(t, filepath.Join(root, ".belay", "runs", "r1", "journal.ndjson"), "{}\n", 0o600)
	writeFile(t, filepath.Join(root, "node_modules", "dep", "index.js"), "x\n", 0o644)
	for _, rel := range []string{"main.go", "src/lib.go", "run.sh"} {
		setMtime(t, filepath.Join(root, rel), surveyBase)
	}
	return root
}

func setMtime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("Chtimes(%q) error = %v", path, err)
	}
}

func survey(t *testing.T, root string) Survey {
	t.Helper()
	s, err := SurveyTree(context.Background(), root, DefaultSkip())
	if err != nil {
		t.Fatalf("SurveyTree(%q) error = %v", root, err)
	}
	return s
}

// TestSurveyTree: the digest changes when the tree a copy would contain
// changes, and only then. In particular a write belay makes to its own run
// directory, or anything under a directory the copy skips, must leave it
// alone — otherwise every run's own journal would read as a changed tree.
func TestSurveyTree(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mutate      func(t *testing.T, root string)
		wantChanged bool
	}{
		{name: "nothing changes", mutate: func(*testing.T, string) {}},
		{
			name: "a content edit",
			mutate: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n", 0o644)
			},
			wantChanged: true,
		},
		{
			name: "an edit that keeps the size",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "main.go")
				writeFile(t, p, "package mian\n", 0o644)
				setMtime(t, p, surveyBase.Add(time.Second))
			},
			wantChanged: true,
		},
		{
			name: "a new file",
			mutate: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "src", "new.go"), "package src\n", 0o644)
			},
			wantChanged: true,
		},
		{
			name:        "a file removed",
			mutate:      func(t *testing.T, root string) { removeAll(t, filepath.Join(root, "src", "lib.go")) },
			wantChanged: true,
		},
		{
			name: "a rename",
			mutate: func(t *testing.T, root string) {
				if err := os.Rename(filepath.Join(root, "main.go"), filepath.Join(root, "app.go")); err != nil {
					t.Fatalf("Rename() error = %v", err)
				}
			},
			wantChanged: true,
		},
		{
			name: "chmod +x",
			mutate: func(t *testing.T, root string) {
				if err := os.Chmod(filepath.Join(root, "main.go"), 0o755); err != nil {
					t.Fatalf("Chmod() error = %v", err)
				}
			},
			wantChanged: true,
		},
		{
			name:        "a new empty directory",
			mutate:      func(t *testing.T, root string) { mkdirAll(t, filepath.Join(root, "docs")) },
			wantChanged: true,
		},
		{
			name: "a symlink pointed somewhere else",
			mutate: func(t *testing.T, root string) {
				removeAll(t, filepath.Join(root, "lib-link"))
				symlink(t, "main.go", filepath.Join(root, "lib-link"))
			},
			wantChanged: true,
		},
		{
			// Touching a file with no edit reads as a change. That is the
			// safe direction to be wrong in for a fork, which asks rather
			// than assumes.
			name: "a touch",
			mutate: func(t *testing.T, root string) {
				setMtime(t, filepath.Join(root, "main.go"), surveyBase.Add(time.Hour))
			},
			wantChanged: true,
		},
		{
			name: "a write under .belay",
			mutate: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, ".belay", "runs", "r1", "state.json"), "{}\n", 0o600)
				writeFile(t, filepath.Join(root, ".belay", "runs", "r2", "journal.ndjson"), "{}\n", 0o600)
			},
		},
		{
			name: "a write under node_modules",
			mutate: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "node_modules", "dep", "index.js"), "changed\n", 0o644)
			},
		},
		{
			// Creating a skipped directory inside a surveyed one moves the
			// surveyed directory's mtime, which is why directory mtimes are
			// not in the digest.
			name: "a skipped directory appearing inside a surveyed one",
			mutate: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "src", "__pycache__", "lib.cpython-312.pyc"), "x", 0o644)
			},
		},
		{
			// The documented false negative: content rewritten with its size
			// kept and its mtime put back is invisible to a stat-only survey.
			name: "a same-size rewrite with the mtime restored",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "main.go")
				writeFile(t, p, "package mian\n", 0o644)
				setMtime(t, p, surveyBase)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := surveyTree(t)
			before := survey(t, root)
			tt.mutate(t, root)
			after := survey(t, root)

			if !strings.HasPrefix(before.Digest, "tree1:sha256:") || len(before.Digest) != len("tree1:sha256:")+64 {
				t.Fatalf("Digest = %q, want \"tree1:sha256:\" and 64 hex digits", before.Digest)
			}
			if changed := before.Digest != after.Digest; changed != tt.wantChanged {
				t.Errorf("digest changed = %v, want %v\n before %s\n after  %s", changed, tt.wantChanged, before.Digest, after.Digest)
			}
		})
	}
}

// TestSurveyTreeCountsWhatACopyWouldHold: Files and Bytes cover the regular
// files a copy would write — not the skipped directories, not a symlink, and
// never what a symlink points at.
func TestSurveyTreeCountsWhatACopyWouldHold(t *testing.T) {
	t.Parallel()

	root := surveyTree(t)
	// A link to a directory outside the tree: recorded, never walked.
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "big.bin"), strings.Repeat("x", 4096), 0o644)
	symlink(t, outside, filepath.Join(root, "out-link"))

	got := survey(t, root)
	wantBytes := int64(len("package main\n") + len("package src\n") + len("#!/bin/sh\n"))
	if got.Files != 3 || got.Bytes != wantBytes {
		t.Errorf("Survey = {Files %d, Bytes %d}, want {Files 3, Bytes %d}: skipped directories and symlink targets do not count",
			got.Files, got.Bytes, wantBytes)
	}
}

// TestSurveyTreeResolvesItsRootAsCreateDoes: the tree surveyed is the tree a
// copy of dir would be made from, so a root reached through a symlink is
// walked, not recorded as a single link.
func TestSurveyTreeResolvesItsRootAsCreateDoes(t *testing.T) {
	t.Parallel()

	root := surveyTree(t)
	link := filepath.Join(t.TempDir(), "checkout")
	symlink(t, root, link)

	if got, want := survey(t, link), survey(t, root); got != want {
		t.Errorf("survey through a symlinked root = %+v, want %+v", got, want)
	}
}

func TestSurveyTreeRefuses(t *testing.T) {
	t.Parallel()

	file := writeFile(t, filepath.Join(t.TempDir(), "f"), "x", 0o644)
	for _, dir := range []string{"", file, filepath.Join(t.TempDir(), "missing")} {
		var srcErr *SourceError
		if _, err := SurveyTree(context.Background(), dir, DefaultSkip()); !errors.As(err, &srcErr) {
			t.Errorf("SurveyTree(%q) error = %v, want a *SourceError", dir, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SurveyTree(ctx, surveyTree(t), DefaultSkip()); !errors.Is(err, context.Canceled) {
		t.Errorf("SurveyTree(cancelled) error = %v, want context.Canceled", err)
	}
}

func removeAll(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("RemoveAll(%q) error = %v", path, err)
	}
}
