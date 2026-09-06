//go:build unix

package dircopy

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestCreateCopiesTheTree is the baseline: structure and content arrive
// intact.
func TestCreateCopiesTheTree(t *testing.T) {
	iso := newIsolator(t)
	dir := mustCreate(t, iso, sampleTree(t), "cand")

	want := map[string]string{
		"go.mod":     "module example.com/x\n",
		"main.go":    "package main\n",
		"pkg/lib.go": "package pkg\n",
		"build.sh":   "#!/bin/sh\necho hi\n",
	}
	for rel, body := range want {
		if got := readFile(t, filepath.Join(dir, rel)); got != body {
			t.Errorf("%s = %q, want %q", rel, got, body)
		}
	}
	if !mode(t, filepath.Join(dir, "pkg")).IsDir() {
		t.Error("pkg is not a directory in the copy")
	}
}

// TestCreateHonoursTheSkipList proves the cost control ADR-0004 depends on
// actually fires, and that the saving is reported rather than silent.
func TestCreateHonoursTheSkipList(t *testing.T) {
	t.Run("defaults omit the expensive directories", func(t *testing.T) {
		iso := newIsolator(t)
		dir := mustCreate(t, iso, sampleTree(t), "cand")

		for _, skipped := range []string{".git", "node_modules"} {
			requireGone(t, filepath.Join(dir, skipped), "a skip-listed directory")
		}
		requireExists(t, filepath.Join(dir, "go.mod"), "an ordinary file")

		stats, ok := iso.Stats("cand")
		if !ok {
			t.Fatal("Stats has no record for a successful Create")
		}
		if stats.Skipped != 2 {
			t.Errorf("Stats.Skipped = %d, want 2 (.git and node_modules)", stats.Skipped)
		}
		if stats.Files != 4 {
			t.Errorf("Stats.Files = %d, want 4", stats.Files)
		}
		if stats.BytesCopied <= 0 {
			t.Errorf("Stats.BytesCopied = %d, want a positive byte count", stats.BytesCopied)
		}
		// The skipped content must not be inside the reported byte count.
		if stats.BytesCopied > 200 {
			t.Errorf("Stats.BytesCopied = %d, larger than the tree that was actually copied", stats.BytesCopied)
		}
		if stats.Duration <= 0 {
			t.Error("Stats.Duration was not recorded")
		}
		if stats.Dirs != 2 {
			t.Errorf("Stats.Dirs = %d, want 2 (the workspace root and pkg/)", stats.Dirs)
		}
	})

	t.Run("skip list is configurable", func(t *testing.T) {
		iso := newIsolator(t, WithSkip("node_modules"))
		dir := mustCreate(t, iso, sampleTree(t), "cand")

		requireExists(t, filepath.Join(dir, ".git", "HEAD"), ".git is no longer skipped")
		requireGone(t, filepath.Join(dir, "node_modules"), "node_modules is still skipped")
	})

	t.Run("an empty skip list copies everything", func(t *testing.T) {
		iso := newIsolator(t, WithSkip())
		dir := mustCreate(t, iso, sampleTree(t), "cand")

		requireExists(t, filepath.Join(dir, ".git", "HEAD"), "nothing is skipped")
		requireExists(t, filepath.Join(dir, "node_modules", "left-pad", "index.js"), "nothing is skipped")

		stats, _ := iso.Stats("cand")
		if stats.Skipped != 0 {
			t.Errorf("Stats.Skipped = %d, want 0", stats.Skipped)
		}
	})

	t.Run("skip list matches at any depth", func(t *testing.T) {
		src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
		writeFile(t, filepath.Join(src, "a", "b", "node_modules", "x.js"), "x", 0o600)
		writeFile(t, filepath.Join(src, "a", "b", "keep.go"), "keep", 0o600)

		iso := newIsolator(t)
		dir := mustCreate(t, iso, src, "cand")

		requireGone(t, filepath.Join(dir, "a", "b", "node_modules"), "a nested skip-listed directory")
		requireExists(t, filepath.Join(dir, "a", "b", "keep.go"), "its sibling")
	})
}

// TestCreateSymlinkPolicies covers requirement three from both directions: an
// internal relative link and an escaping absolute one, under each policy.
//
// The "link -> /" case is the one that matters most. Under every policy the
// copy must stay the size of the little source tree, because a copier that
// followed that link would walk the whole filesystem.
func TestCreateSymlinkPolicies(t *testing.T) {
	buildSource := func(t *testing.T) (src, outsideFile string) {
		t.Helper()
		outside := mkdirAll(t, filepath.Join(t.TempDir(), "outside"))
		outsideFile = writeFile(t, filepath.Join(outside, "secret.txt"), "not yours", 0o600)

		src = mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
		writeFile(t, filepath.Join(src, "inner", "target.txt"), "inner content", 0o600)
		// Internal, relative, stays inside the workspace.
		symlink(t, filepath.Join("..", "inner", "target.txt"), filepath.Join(src, "links", "internal.txt"))
		// Internal, relative, pointing at the workspace root.
		symlink(t, "..", filepath.Join(src, "links", "up.txt"))
		// Escaping: absolute to the filesystem root, absolute to a file, and
		// relative but climbing out of the tree.
		symlink(t, "/", filepath.Join(src, "links", "root"))
		symlink(t, outsideFile, filepath.Join(src, "links", "secret.txt"))
		symlink(t, filepath.Join("..", "..", "..", "etc"), filepath.Join(src, "links", "climb"))
		return src, outsideFile
	}

	tests := []struct {
		name             string
		policy           SymlinkPolicy
		wantSymlinks     int
		wantSkippedLinks int
		wantPresent      []string
		wantAbsent       []string
	}{
		{
			name:             "internal (default) keeps contained links and drops escaping ones",
			policy:           SymlinkInternal,
			wantSymlinks:     2,
			wantSkippedLinks: 3,
			wantPresent:      []string{"links/internal.txt", "links/up.txt"},
			wantAbsent:       []string{"links/root", "links/secret.txt", "links/climb"},
		},
		{
			name:             "preserve-all keeps every link, still without following any",
			policy:           SymlinkPreserveAll,
			wantSymlinks:     5,
			wantSkippedLinks: 0,
			wantPresent:      []string{"links/internal.txt", "links/up.txt", "links/root", "links/secret.txt", "links/climb"},
		},
		{
			name:             "skip drops every link",
			policy:           SymlinkSkip,
			wantSymlinks:     0,
			wantSkippedLinks: 5,
			wantAbsent:       []string{"links/internal.txt", "links/root", "links/secret.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, outsideFile := buildSource(t)
			iso := newIsolator(t, WithSymlinkPolicy(tt.policy))
			dir := mustCreate(t, iso, src, "cand")

			stats, _ := iso.Stats("cand")
			if stats.Symlinks != tt.wantSymlinks {
				t.Errorf("Stats.Symlinks = %d, want %d", stats.Symlinks, tt.wantSymlinks)
			}
			if stats.SkippedSymlinks != tt.wantSkippedLinks {
				t.Errorf("Stats.SkippedSymlinks = %d, want %d", stats.SkippedSymlinks, tt.wantSkippedLinks)
			}

			for _, rel := range tt.wantPresent {
				path := filepath.Join(dir, rel)
				requireExists(t, path, "a link the policy should keep")
				if mode(t, path)&fs.ModeSymlink == 0 {
					t.Errorf("%s is not a symlink in the copy: a link must never be dereferenced into content", rel)
				}
			}
			for _, rel := range tt.wantAbsent {
				requireGone(t, filepath.Join(dir, rel), "a link the policy should drop")
			}

			// The heart of it: "link -> /" copied a link or nothing at all,
			// never the filesystem behind it.
			if stats.Files != 1 {
				t.Errorf("Stats.Files = %d, want 1: only inner/target.txt is a real file", stats.Files)
			}
			if stats.BytesCopied > 1<<20 {
				t.Errorf("Stats.BytesCopied = %d: a symlink was followed", stats.BytesCopied)
			}
			requireNoEscapedFiles(t, dir)
			// A copier that followed "root -> /" would have walked the whole
			// filesystem into this workspace. WalkDir does not follow links,
			// so this counts what is really there.
			if got := countEntries(t, dir); got > 12 {
				t.Errorf("workspace holds %d entries: a symlink was followed", got)
			}

			// Nothing the links pointed at was read, written or moved.
			requireExists(t, outsideFile, "the file an escaping link pointed at")
			if got := readFile(t, outsideFile); got != "not yours" {
				t.Errorf("outside file content = %q, want it untouched", got)
			}
		})
	}
}

// TestCreatePreservesModes covers requirement five: an executable script
// stays executable, permissions are never widened, and setuid and setgid are
// left behind.
func TestCreatePreservesModes(t *testing.T) {
	src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
	writeFile(t, filepath.Join(src, "script.sh"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(src, "private.key"), "secret", 0o600)
	writeFile(t, filepath.Join(src, "group.txt"), "g", 0o640)
	writeFile(t, filepath.Join(src, "world.txt"), "w", 0o644)
	setuid := writeFile(t, filepath.Join(src, "suid-tool"), "#!/bin/sh\n", 0o755)
	setgid := writeFile(t, filepath.Join(src, "sgid-tool"), "#!/bin/sh\n", 0o755)

	// fs.ModeSetuid, not the octal 0o4000: fs.FileMode is not the POSIX mode
	// word, and os.Chmod silently drops a bare 0o4755 down to 0o755 — which
	// would leave this test passing against a fixture that was never setuid
	// in the first place.
	if err := os.Chmod(setuid, 0o755|fs.ModeSetuid); err != nil {
		t.Fatalf("cannot create the setuid fixture: %v", err)
	}
	if mode(t, setuid)&fs.ModeSetuid == 0 {
		t.Fatal("the setuid fixture is not setuid; this test would prove nothing")
	}
	if err := os.Chmod(setgid, 0o755|fs.ModeSetgid); err != nil {
		t.Fatalf("cannot create the setgid fixture: %v", err)
	}
	if mode(t, setgid)&fs.ModeSetgid == 0 {
		t.Fatal("the setgid fixture is not setgid; this test would prove nothing")
	}

	iso := newIsolator(t)
	dir := mustCreate(t, iso, src, "cand")

	tests := []struct {
		name string
		rel  string
		want fs.FileMode
	}{
		{"executable script stays executable", "script.sh", 0o755},
		{"private file stays private", "private.key", 0o600},
		{"group-readable file is unchanged", "group.txt", 0o640},
		{"world-readable file is unchanged", "world.txt", 0o644},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mode(t, filepath.Join(dir, tt.rel))
			if got.Perm() != tt.want {
				t.Errorf("%s mode = %v, want %v", tt.rel, got.Perm(), tt.want)
			}
			if got.Perm()&^tt.want != 0 {
				t.Errorf("%s mode = %v, which is wider than the source's %v", tt.rel, got.Perm(), tt.want)
			}
		})
	}

	t.Run("setuid is not copied", func(t *testing.T) {
		got := mode(t, filepath.Join(dir, "suid-tool"))
		if got&fs.ModeSetuid != 0 {
			t.Errorf("suid-tool mode = %v, still setuid: a copy must not carry privilege into an agent's sandbox", got)
		}
		if got.Perm() != 0o755 {
			t.Errorf("suid-tool permission bits = %v, want 0755", got.Perm())
		}
	})

	t.Run("setgid is not copied", func(t *testing.T) {
		if got := mode(t, filepath.Join(dir, "sgid-tool")); got&fs.ModeSetgid != 0 {
			t.Errorf("sgid-tool mode = %v, still setgid", got)
		}
	})

	t.Run("directories stay traversable so the workspace can be cleaned up", func(t *testing.T) {
		readOnly := mkdirAll(t, filepath.Join(src, "locked"))
		writeFile(t, filepath.Join(readOnly, "inner.txt"), "inner", 0o600)
		// #nosec G302 -- 0555 is the fixture: this subtest exists to prove a
		// non-writable source directory still yields a removable copy.
		if err := os.Chmod(readOnly, 0o555); err != nil {
			t.Skipf("cannot create a read-only directory fixture: %v", err)
		}
		// #nosec G302 -- restoring owner write on a directory so t.TempDir's
		// own cleanup can remove it.
		t.Cleanup(func() { _ = os.Chmod(readOnly, 0o750) })

		iso := newIsolator(t)
		dir := mustCreate(t, iso, src, "cand2")

		copied := filepath.Join(dir, "locked")
		if got := mode(t, copied).Perm(); got&0o700 != 0o700 {
			t.Errorf("copied directory mode = %v, want owner rwx so Destroy can remove it", got)
		}
		if got := mode(t, copied).Perm() &^ 0o700; got != 0o055 {
			t.Errorf("copied directory group/other bits = %v, want the source's 0055", got)
		}
		if err := iso.Destroy(context.Background(), mustWorkspace("cand2", dir)); err != nil {
			t.Errorf("Destroy of a workspace containing a read-only directory: %v", err)
		}
	})
}

// TestCreateSkipsIrregularFiles proves a FIFO does not hang the copy. A
// copier that opened one as an ordinary file would block until something
// opened the write end, which is never, and take the whole run with it.
func TestCreateSkipsIrregularFiles(t *testing.T) {
	src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
	writeFile(t, filepath.Join(src, "ordinary.txt"), "fine", 0o600)
	fifo := filepath.Join(src, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create a FIFO on this filesystem: %v", err)
	}

	iso := newIsolator(t)
	dir := mustCreate(t, iso, src, "cand")

	requireExists(t, filepath.Join(dir, "ordinary.txt"), "the ordinary file")
	requireGone(t, filepath.Join(dir, "pipe"), "the FIFO")

	stats, _ := iso.Stats("cand")
	if stats.Irregular != 1 {
		t.Errorf("Stats.Irregular = %d, want 1", stats.Irregular)
	}
}

// TestCreateCleansUpAfterAMidCopyFailure covers requirement seven with a real
// failure rather than a test seam: a file the copier cannot read, sorted so
// the walk reaches it after it has already written something.
func TestCreateCleansUpAfterAMidCopyFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable, so there is no failure to inject")
	}

	src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
	writeFile(t, filepath.Join(src, "a-first.txt"), "copied before the failure", 0o600)
	unreadable := writeFile(t, filepath.Join(src, "b-unreadable.txt"), "cannot be read", 0o000)
	writeFile(t, filepath.Join(src, "c-last.txt"), "never reached", 0o600)
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })

	iso := newIsolator(t)
	_, err := iso.Create(context.Background(), src, "cand")
	if err == nil {
		t.Fatal("Create succeeded despite an unreadable source file")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("Create error = %v, want it to wrap a permission error", err)
	}
	if !strings.Contains(err.Error(), "cand") {
		t.Errorf("Create error %q does not name the candidate", err)
	}

	dest := filepath.Join(iso.Root(), "cand")
	requireGone(t, dest, "a failed Create must leave no partial tree")
	requireGone(t, filepath.Join(dest, "a-first.txt"), "the file copied before the failure")
	requireExists(t, iso.Root(), "the candidate root itself")

	if _, ok := iso.Stats("cand"); ok {
		t.Error("Stats recorded a Create that failed")
	}

	entries, err := os.ReadDir(iso.Root())
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", iso.Root(), err)
	}
	if len(entries) != 0 {
		t.Errorf("candidate root still holds %d entries after a failed Create, want 0", len(entries))
	}
}

// TestCreateCleansUpAfterAnUnreadableDirectory covers the other failure the
// filesystem can hand back mid-walk.
func TestCreateCleansUpAfterAnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not apply")
	}

	src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
	writeFile(t, filepath.Join(src, "a.txt"), "fine", 0o600)
	locked := mkdirAll(t, filepath.Join(src, "b-locked"))
	writeFile(t, filepath.Join(locked, "inner.txt"), "inner", 0o600)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("cannot create an unreadable directory fixture: %v", err)
	}
	// #nosec G302 -- restoring owner access on a directory this test made
	// unreadable, so t.TempDir's own cleanup can remove it.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })

	iso := newIsolator(t)
	if _, err := iso.Create(context.Background(), src, "cand"); err == nil {
		t.Fatal("Create succeeded despite an unreadable source directory")
	}
	requireGone(t, filepath.Join(iso.Root(), "cand"), "a failed Create must leave no partial tree")
}

// trippingContext returns Canceled once trip reports the copy has reached the
// point the test cares about. It lets a cancellation test be exact — cancel
// after real files have been written — with no seam in the production code.
type trippingContext struct {
	context.Context
	mu    sync.Mutex
	trip  func() bool
	fired bool
}

func (c *trippingContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fired || c.trip() {
		c.fired = true
		return context.Canceled
	}
	return nil
}

func (c *trippingContext) tripped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired
}

// TestCreateHonoursContextCancellation covers requirement eight: a cancelled
// copy stops, and cleans up after itself rather than leaving a partial tree.
func TestCreateHonoursContextCancellation(t *testing.T) {
	t.Run("cancelled midway through the copy", func(t *testing.T) {
		src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
		for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
			writeFile(t, filepath.Join(src, name+".txt"), strings.Repeat(name, 4096), 0o600)
		}

		iso := newIsolator(t)
		dest := filepath.Join(iso.Root(), "cand")

		// Trip as soon as two files exist in the destination: by then the
		// copy is demonstrably underway, so this tests cancellation of a
		// running copy rather than of one that never started.
		ctx := &trippingContext{Context: context.Background(), trip: func() bool {
			entries, err := os.ReadDir(dest)
			return err == nil && len(entries) >= 2
		}}

		_, err := iso.Create(ctx, src, "cand")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Create error = %v, want context.Canceled", err)
		}
		if !ctx.tripped() {
			t.Fatal("the cancellation never fired, so this did not test a running copy")
		}
		requireGone(t, dest, "a cancelled Create must clean up after itself")
		if _, ok := iso.Stats("cand"); ok {
			t.Error("Stats recorded a cancelled Create")
		}
	})

	t.Run("cancelled before it starts", func(t *testing.T) {
		iso := newIsolator(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := iso.Create(ctx, sampleTree(t), "cand")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Create error = %v, want context.Canceled", err)
		}
		requireGone(t, filepath.Join(iso.Root(), "cand"), "nothing should have been created")
	})

	t.Run("cancelled inside a single large file", func(t *testing.T) {
		src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
		writeFile(t, filepath.Join(src, "big.bin"), strings.Repeat("x", 4*copyBufferSize), 0o600)

		iso := newIsolator(t)
		dest := filepath.Join(iso.Root(), "cand")
		ctx := &trippingContext{Context: context.Background(), trip: func() bool {
			info, err := os.Stat(filepath.Join(dest, "big.bin"))
			return err == nil && info.Size() >= copyBufferSize
		}}

		_, err := iso.Create(ctx, src, "cand")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Create error = %v, want context.Canceled", err)
		}
		if !ctx.tripped() {
			t.Fatal("cancellation never fired inside the large file")
		}
		requireGone(t, dest, "a cancelled Create must clean up after itself")
	})
}

// TestCreateRefusesACopyThatWillNotFit covers requirement nine. The free
// space probe is injected rather than filling a real disk.
func TestCreateRefusesACopyThatWillNotFit(t *testing.T) {
	src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
	writeFile(t, filepath.Join(src, "a.txt"), strings.Repeat("a", 1000), 0o600)
	writeFile(t, filepath.Join(src, "b.txt"), strings.Repeat("b", 1000), 0o600)
	writeFile(t, filepath.Join(src, "node_modules", "huge.bin"), strings.Repeat("n", 100_000), 0o600)

	t.Run("refused when free space is below the estimate", func(t *testing.T) {
		iso := newIsolator(t, WithFreeSpaceReserve(0))
		iso.freeSpace = func(string) (int64, error) { return 1500, nil }

		_, err := iso.Create(context.Background(), src, "cand")
		if !errors.Is(err, ErrInsufficientSpace) {
			t.Fatalf("Create error = %v, want ErrInsufficientSpace", err)
		}
		var se *SpaceError
		if !errors.As(err, &se) {
			t.Fatalf("error is not a *SpaceError: %v", err)
		}
		// The estimate must respect the skip list, or it would refuse copies
		// that would comfortably have fitted.
		if se.NeedBytes != 2000 {
			t.Errorf("SpaceError.NeedBytes = %d, want 2000 (node_modules is skipped)", se.NeedBytes)
		}
		if se.FreeBytes != 1500 {
			t.Errorf("SpaceError.FreeBytes = %d, want 1500", se.FreeBytes)
		}
		requireGone(t, filepath.Join(iso.Root(), "cand"), "a refused Create leaves nothing behind")
	})

	t.Run("the reserve is respected", func(t *testing.T) {
		iso := newIsolator(t, WithFreeSpaceReserve(10_000))
		iso.freeSpace = func(string) (int64, error) { return 5000, nil }

		_, err := iso.Create(context.Background(), src, "cand")
		if !errors.Is(err, ErrInsufficientSpace) {
			t.Fatalf("Create error = %v, want ErrInsufficientSpace: 5000 free is enough for the copy but not for the reserve", err)
		}
	})

	t.Run("allowed when the copy fits", func(t *testing.T) {
		iso := newIsolator(t, WithFreeSpaceReserve(0))
		iso.freeSpace = func(string) (int64, error) { return 1 << 30, nil }
		mustCreate(t, iso, src, "cand")
	})

	t.Run("an unmeasurable filesystem does not block the copy", func(t *testing.T) {
		iso := newIsolator(t)
		iso.freeSpace = func(string) (int64, error) { return 0, errors.New("statfs unavailable") }
		mustCreate(t, iso, src, "cand")
	})

	t.Run("a negative reserve is clamped to zero", func(t *testing.T) {
		iso := newIsolator(t, WithFreeSpaceReserve(-1))
		if iso.reserveBytes != 0 {
			t.Errorf("reserveBytes = %d, want 0", iso.reserveBytes)
		}
	})
}

// TestFreeSpaceOnRealFilesystem checks the production probe answers
// plausibly for the directory the test is running in.
func TestFreeSpaceOnRealFilesystem(t *testing.T) {
	free, err := freeSpaceOn(t.TempDir())
	if err != nil {
		t.Fatalf("freeSpaceOn: %v", err)
	}
	if free <= 0 {
		t.Errorf("freeSpaceOn = %d, want a positive byte count", free)
	}
}

// TestFreeSpaceOnMissingPath checks the probe reports rather than guesses.
func TestFreeSpaceOnMissingPath(t *testing.T) {
	if _, err := freeSpaceOn(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("freeSpaceOn on a missing path returned nil error")
	}
}

// TestCreateDoesNotRecurseIntoItsOwnDestination is the disk-filling bug this
// package would otherwise have. The candidate root lives inside the source
// tree — the normal configuration, with the run directory under the repo —
// and the skip list is emptied so nothing but the structural exclusion stops
// the copy walking into its own output.
func TestCreateDoesNotRecurseIntoItsOwnDestination(t *testing.T) {
	src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
	writeFile(t, filepath.Join(src, "main.go"), "package main\n", 0o600)
	writeFile(t, filepath.Join(src, "nested", "lib.go"), "package nested\n", 0o600)

	root := filepath.Join(src, ".belay", "candidates")
	iso, err := New(root, WithLogger(quietLogger()), WithSkip()) // no skip list at all
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	dir := mustCreate(t, iso, src, "cand")

	requireExists(t, filepath.Join(dir, "main.go"), "the source content")
	requireExists(t, filepath.Join(dir, "nested", "lib.go"), "the nested source content")
	requireGone(t, filepath.Join(dir, ".belay", "candidates", "cand"), "the copy must not contain itself")

	stats, _ := iso.Stats("cand")
	if stats.Files != 2 {
		t.Errorf("Stats.Files = %d, want 2: the copy recursed into its own output", stats.Files)
	}

	// A second candidate must not pick up the first one either.
	second := mustCreate(t, iso, src, "cand-2")
	requireGone(t, filepath.Join(second, ".belay", "candidates", "cand"), "the first candidate")
	if stats2, _ := iso.Stats("cand-2"); stats2.Files != 2 {
		t.Errorf("second Stats.Files = %d, want 2", stats2.Files)
	}
}

// TestCreateRefusesOverlappingSourceAndDestination covers the arrangements
// that cannot work at all.
func TestCreateRefusesOverlappingSourceAndDestination(t *testing.T) {
	t.Run("source inside the destination", func(t *testing.T) {
		iso := newIsolator(t)
		src := mkdirAll(t, filepath.Join(iso.Root(), "cand", "inner"))
		writeFile(t, filepath.Join(src, "a.txt"), "a", 0o600)

		if _, err := iso.Create(context.Background(), src, "cand"); !errors.Is(err, ErrInvalidSource) {
			t.Fatalf("Create error = %v, want ErrInvalidSource", err)
		}
	})

	t.Run("source is the destination", func(t *testing.T) {
		iso := newIsolator(t)
		src := mkdirAll(t, filepath.Join(iso.Root(), "cand"))

		if _, err := iso.Create(context.Background(), src, "cand"); err == nil {
			t.Fatal("Create copying a directory onto itself succeeded")
		}
	})
}

// TestEscapesIsLexical pins the symlink containment rule directly, including
// the prefix-confusion case in link form.
func TestEscapesIsLexical(t *testing.T) {
	c := &copier{dst: "/ws"}
	tests := []struct {
		name   string
		target string // where the link will live in the copy
		link   string // its text
		want   bool
	}{
		{"relative sibling", "/ws/a/link", "b.txt", false},
		{"relative into a subdirectory", "/ws/link", "sub/b.txt", false},
		{"relative up but still inside", "/ws/a/b/link", "../c.txt", false},
		{"relative to the workspace root itself", "/ws/a/link", "..", false},
		{"relative climbing out", "/ws/a/link", "../../etc", true},
		{"relative climbing far out", "/ws/link", "../../../../etc/passwd", true},
		{"absolute to the filesystem root", "/ws/link", "/", true},
		{"absolute elsewhere", "/ws/link", "/etc/passwd", true},
		{"absolute into a prefix-sharing sibling", "/ws/link", "/wsx/secrets", true},
		{"relative into a prefix-sharing sibling", "/ws/link", "../wsx/secrets", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.escapes(tt.target, tt.link); got != tt.want {
				t.Errorf("escapes(%q, %q) = %v, want %v", tt.target, tt.link, got, tt.want)
			}
		})
	}
}

// TestCreateHandlesAnEmptySource is the degenerate case.
func TestCreateHandlesAnEmptySource(t *testing.T) {
	iso := newIsolator(t)
	src := mkdirAll(t, filepath.Join(t.TempDir(), "empty"))

	dir := mustCreate(t, iso, src, "cand")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("copy of an empty tree has %d entries, want 0", len(entries))
	}

	stats, _ := iso.Stats("cand")
	if stats.Files != 0 || stats.Dirs != 1 {
		t.Errorf("Stats = %+v, want 0 files and 1 dir (the workspace root)", stats)
	}
}

// TestCreatePreservesModTimes checks the build-cache-friendly behaviour.
func TestCreatePreservesModTimes(t *testing.T) {
	src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
	file := writeFile(t, filepath.Join(src, "a.txt"), "a", 0o600)

	want, err := os.Stat(file)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	iso := newIsolator(t)
	dir := mustCreate(t, iso, src, "cand")

	got, err := os.Stat(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !got.ModTime().Equal(want.ModTime()) {
		t.Errorf("copied mtime = %v, want %v", got.ModTime(), want.ModTime())
	}
}

// mustWorkspace is a small constructor for the tests that destroy what they
// created.
func mustWorkspace(id, dir string) belay.Workspace {
	return belay.Workspace{ID: id, Dir: dir, Ephemeral: true}
}

// TestCopyFileRefusesToWriteThroughAPlantedSymlink covers the O_EXCL on the
// destination file.
//
// Create checks the destination is empty before it starts, so nothing can
// pre-exist there in a well-behaved run. The check and the writes are not
// atomic, though: anything that can write to the candidate root can plant a
// symlink in the gap, and a plain O_CREATE would then follow it and write the
// source file's content over whatever it names. Driving the copier directly
// puts the symlink there deterministically instead of racing for it.
func TestCopyFileRefusesToWriteThroughAPlantedSymlink(t *testing.T) {
	victim := writeFile(t, filepath.Join(t.TempDir(), "victim.txt"), "original content", 0o600)

	src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
	writeFile(t, filepath.Join(src, "a.txt"), "attacker-chosen payload", 0o600)

	dst := mkdirAll(t, filepath.Join(t.TempDir(), "ws"))
	symlink(t, victim, filepath.Join(dst, "a.txt"))

	c := &copier{src: src, dst: dst, log: quietLogger()}
	err := c.run(context.Background())
	if err == nil {
		t.Fatal("the copy wrote through a planted symlink instead of refusing")
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("error = %v, want it to wrap fs.ErrExist", err)
	}
	if got := readFile(t, victim); got != "original content" {
		t.Errorf("victim content = %q, want it untouched: the copy wrote through the symlink", got)
	}
}

// TestCopyDirRefusesToReuseAPlantedSymlink is the same argument for a
// directory the walk is about to create.
func TestCopyDirRefusesToReuseAPlantedSymlink(t *testing.T) {
	victim := mkdirAll(t, filepath.Join(t.TempDir(), "victim"))
	writeFile(t, filepath.Join(victim, "keep.txt"), "keep", 0o600)

	src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
	writeFile(t, filepath.Join(src, "sub", "a.txt"), "payload", 0o600)

	dst := mkdirAll(t, filepath.Join(t.TempDir(), "ws"))
	symlink(t, victim, filepath.Join(dst, "sub"))

	c := &copier{src: src, dst: dst, log: quietLogger()}
	if err := c.run(context.Background()); err == nil {
		t.Fatal("the copy descended into a planted directory symlink instead of refusing")
	}
	requireGone(t, filepath.Join(victim, "a.txt"), "nothing may be written into the linked-to directory")
}
