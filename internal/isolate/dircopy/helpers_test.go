//go:build unix

package dircopy

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// quietLogger keeps the package's info-level cost line out of test output
// while still exercising every logging call site.
func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newIsolator returns an Isolator rooted in a fresh candidates directory
// under t.TempDir(), with logging discarded.
func newIsolator(t *testing.T, opts ...Option) *Isolator {
	t.Helper()
	root := filepath.Join(t.TempDir(), "candidates")
	iso, err := New(root, append([]Option{WithLogger(quietLogger())}, opts...)...)
	if err != nil {
		t.Fatalf("New(%q) error = %v", root, err)
	}
	return iso
}

// mkdirAll creates dir and every missing parent.
func mkdirAll(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", dir, err)
	}
	return dir
}

// writeFile writes body to path, creating parents, and chmods to mode so the
// test gets the exact bits it asked for rather than the umask's opinion.
func writeFile(t *testing.T, path, body string, mode fs.FileMode) string {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	// #nosec G306 -- the mode is the fixture's whole point; several of these
	// tests exist to prove an executable file stays executable.
	if err := os.WriteFile(path, []byte(body), mode.Perm()); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod(%q, %v) error = %v", path, mode, err)
	}
	return path
}

// symlink creates a symlink at path pointing at target, creating parents.
func symlink(t *testing.T, target, path string) string {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Symlink(%q, %q) error = %v", target, path, err)
	}
	return path
}

// readFile returns path's contents.
func readFile(t *testing.T, path string) string {
	t.Helper()
	// #nosec G304 -- path is this test's own fixture under t.TempDir().
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return string(b)
}

// requireExists fails unless path exists. It is the assertion that matters
// most in this package's refusal tests: a refusal that still deleted
// something is not a refusal.
func requireExists(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s: %q should still exist, but Lstat failed: %v", why, path, err)
	}
}

// requireGone fails unless path is absent.
func requireGone(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s: %q should not exist, Lstat error = %v", why, path, err)
	}
}

// mode returns path's file mode, following nothing.
func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%q) error = %v", path, err)
	}
	return info.Mode()
}

// sampleTree writes a small, representative source tree and returns its
// root: a couple of ordinary files, a nested directory, an executable
// script, and two skip-listed directories with content inside them.
func sampleTree(t *testing.T) string {
	t.Helper()
	src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
	writeFile(t, filepath.Join(src, "go.mod"), "module example.com/x\n", 0o644)
	writeFile(t, filepath.Join(src, "main.go"), "package main\n", 0o644)
	writeFile(t, filepath.Join(src, "pkg", "lib.go"), "package pkg\n", 0o644)
	writeFile(t, filepath.Join(src, "build.sh"), "#!/bin/sh\necho hi\n", 0o755)
	writeFile(t, filepath.Join(src, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
	writeFile(t, filepath.Join(src, "node_modules", "left-pad", "index.js"), "module.exports = 1\n", 0o644)
	return src
}

// mustCreate runs Create and fails the test if it errors.
func mustCreate(t *testing.T, iso *Isolator, src, id string) string {
	t.Helper()
	ws, err := iso.Create(context.Background(), src, id)
	if err != nil {
		t.Fatalf("Create(%q, %q) error = %v", src, id, err)
	}
	if ws.ID != id {
		t.Errorf("Workspace.ID = %q, want %q", ws.ID, id)
	}
	if !ws.Ephemeral {
		t.Errorf("Workspace.Ephemeral = false, want true: a dircopy workspace is always removable")
	}
	return ws.Dir
}

// countEntries returns the number of entries in the tree at dir, without
// following symlinks — which is the point when the tree under test contains a
// link to "/".
func countEntries(t *testing.T, dir string) int {
	t.Helper()
	var n int
	err := filepath.WalkDir(dir, func(_ string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", dir, err)
	}
	return n
}
