//go:build unix

package git

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestParseWorktrees pins the reading of `worktree list --porcelain -z`
// against every attribute git prints, including the two that only the -z
// form can carry safely: a path with a space and a path with a newline.
// Splitting the plain form on newlines would read the second as two
// worktrees, and lose the lock reason and the prunable flag besides.
func TestParseWorktrees(t *testing.T) {
	out := "worktree /repo\x00HEAD aaaa\x00branch refs/heads/main\x00\x00" +
		"worktree /repo/.belay/wt one\x00HEAD bbbb\x00branch refs/heads/belay/r1\x00\x00" +
		"worktree /repo/detached\x00HEAD cccc\x00detached\x00\x00" +
		"worktree /repo/locked\x00HEAD dddd\x00branch refs/heads/l1\x00locked on a usb stick\x00\x00" +
		"worktree /repo/locked-silently\x00HEAD eeee\x00branch refs/heads/l2\x00locked\x00\x00" +
		"worktree /repo/gone\x00HEAD ffff\x00branch refs/heads/g\x00prunable gitdir file points to non-existent location\x00\x00" +
		"worktree /repo/new\nline\x00HEAD 1111\x00detached\x00some-future-attribute value\x00\x00"

	got, err := parseWorktrees(out)
	if err != nil {
		t.Fatalf("parseWorktrees() error = %v", err)
	}
	want := []Worktree{
		{Path: "/repo", Head: "aaaa", Branch: "main"},
		{Path: "/repo/.belay/wt one", Head: "bbbb", Branch: "belay/r1"},
		{Path: "/repo/detached", Head: "cccc", Detached: true},
		{Path: "/repo/locked", Head: "dddd", Branch: "l1", Locked: true, LockReason: "on a usb stick"},
		{Path: "/repo/locked-silently", Head: "eeee", Branch: "l2", Locked: true},
		{Path: "/repo/gone", Head: "ffff", Branch: "g", Prunable: true},
		{Path: "/repo/new\nline", Head: "1111", Detached: true},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("parseWorktrees() mismatch (-want +got):\n%s", diff)
	}
}

// TestParseWorktreesRefusesWhatItCannotRead proves a listing that does not
// start an entry with its path is an error, not a worktree with no path. A
// caller deciding whether a directory is registered must not be told "no"
// because the answer was unreadable.
func TestParseWorktreesRefusesWhatItCannotRead(t *testing.T) {
	for _, out := range []string{
		"HEAD aaaa\x00branch refs/heads/main\x00\x00",
		"worktree /repo\x00HEAD aaaa\x00\x00branch refs/heads/orphan\x00\x00",
		"worktree \x00HEAD aaaa\x00\x00",
	} {
		if got, err := parseWorktrees(out); err == nil {
			t.Errorf("parseWorktrees(%q) = %+v, want an error", out, got)
		}
	}
	if got, err := parseWorktrees(""); err != nil || len(got) != 0 {
		t.Errorf("parseWorktrees(\"\") = %+v, %v; want nothing and no error", got, err)
	}
}

// TestWorktrees reads a real repository's list, so the parser is proven
// against the bytes the installed git really prints, not only against the
// ones this file wrote down.
func TestWorktrees(t *testing.T) {
	f := newFixture(t)
	top := f.repo("repo")
	head := f.git(top, "rev-parse", "HEAD")

	spaced := filepath.Join(f.root, "wt with space")
	f.git(top, "worktree", "add", "-q", "-b", "side", spaced)
	detached := filepath.Join(f.root, "detached")
	f.git(top, "worktree", "add", "-q", "--detach", detached)
	locked := filepath.Join(f.root, "locked")
	f.git(top, "worktree", "add", "-q", "-b", "held", locked)
	f.git(top, "worktree", "lock", "--reason", "on a usb stick", locked)
	gone := filepath.Join(f.root, "gone")
	f.git(top, "worktree", "add", "-q", "-b", "stale", gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("RemoveAll(%q) error = %v", gone, err)
	}

	got, err := f.open(top).Worktrees(t.Context())
	if err != nil {
		t.Fatalf("Worktrees() error = %v", err)
	}
	want := []Worktree{
		{Path: top, Head: head, Branch: "main"},
		{Path: detached, Head: head, Detached: true},
		{Path: gone, Head: head, Branch: "stale", Prunable: true},
		{Path: locked, Head: head, Branch: "held", Locked: true, LockReason: "on a usb stick"},
		{Path: spaced, Head: head, Branch: "side"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Worktrees() mismatch (-want +got):\n%s", diff)
	}
}

// TestAddWorktree checks out an existing branch into a new worktree, which
// is all it ever does: it never creates the branch (see
// TestAddWorktreeNeverCreatesABranch).
func TestAddWorktree(t *testing.T) {
	f := newFixture(t)
	top := f.repo("repo")
	r := f.open(top)
	f.git(top, "branch", "belay/run1")

	dir := filepath.Join(f.root, "wt")
	if err := r.AddWorktree(t.Context(), dir, "belay/run1"); err != nil {
		t.Fatalf("AddWorktree() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "b.txt")); err != nil {
		t.Errorf("the new worktree does not hold the branch's files: %v", err)
	}
	if got := f.git(dir, "symbolic-ref", "HEAD"); got != "refs/heads/belay/run1" {
		t.Errorf("the new worktree's HEAD is %q, want refs/heads/belay/run1", got)
	}
}

// TestAddWorktreeNeverCreatesABranch is why AddWorktree takes a branch that
// must already exist. `git worktree add <path> <name>` has two ways of
// creating a branch the caller never asked for: -b creates one even when
// the add then fails over an occupied path, and with no local branch of
// that name git quietly creates one from a remote-tracking branch of the
// same name. A branch that appears behind a caller's back is a branch
// nothing will ever clean up.
func TestAddWorktreeNeverCreatesABranch(t *testing.T) {
	f := newFixture(t)
	top := f.repo("repo")
	r := f.open(top)
	head := f.git(top, "rev-parse", "HEAD")
	f.git(top, "remote", "add", "origin", "https://example.invalid/repo.git")
	f.git(top, "update-ref", "refs/remotes/origin/feature", head)

	dir := filepath.Join(f.root, "wt")
	err := r.AddWorktree(t.Context(), dir, "feature")
	if !errors.Is(err, ErrNoBranch) {
		t.Fatalf("AddWorktree(<no local branch>) error = %v, want %v", err, ErrNoBranch)
	}
	if out := f.git(top, "for-each-ref", "refs/heads/feature"); out != "" {
		t.Errorf("AddWorktree created refs/heads/feature from origin/feature: %q", out)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("AddWorktree left %q behind (stat error %v)", dir, err)
	}
	assertWorktreeCount(t, f, top, 1)
}

// TestAddWorktreeRefusals covers the ways an add can fail. Each must leave
// the repository exactly as it was: no new registration, no new branch, and
// nothing the caller did not put at the destination.
func TestAddWorktreeRefusals(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name    string
		setup   func(top string) (dir, branch string)
		wantErr error
	}{
		{
			name: "a branch already checked out in another worktree",
			setup: func(top string) (string, string) {
				f.git(top, "worktree", "add", "-q", "-b", "taken", filepath.Join(filepath.Dir(top), filepath.Base(top)+"-first"))
				return filepath.Join(filepath.Dir(top), filepath.Base(top)+"-second"), "taken"
			},
		},
		{
			name: "a destination that already holds files",
			setup: func(top string) (string, string) {
				f.git(top, "branch", "free")
				dir := filepath.Join(filepath.Dir(top), filepath.Base(top)+"-occupied")
				f.write(dir, "someone-elses.txt", "keep me\n")
				return dir, "free"
			},
		},
		{
			name: "a relative destination",
			setup: func(top string) (string, string) {
				f.git(top, "branch", "free")
				return "relative/wt", "free"
			},
		},
		{
			name: "a branch name git would refuse",
			setup: func(top string) (string, string) {
				return filepath.Join(filepath.Dir(top), filepath.Base(top)+"-wt"), "belay/x.lock"
			},
			wantErr: ErrInvalidRef,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			top := f.repo("refusal" + strconv.Itoa(i))
			r := f.open(top)
			dir, branch := tt.setup(top)
			before := f.git(top, "worktree", "list", "--porcelain")
			refs := f.git(top, "for-each-ref")

			err := r.AddWorktree(t.Context(), dir, branch)
			if err == nil {
				t.Fatal("AddWorktree() error = nil, want an error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("AddWorktree() error = %v, want %v", err, tt.wantErr)
			}
			if after := f.git(top, "worktree", "list", "--porcelain"); after != before {
				t.Errorf("the worktree list changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if after := f.git(top, "for-each-ref"); after != refs {
				t.Errorf("the refs changed:\nbefore:\n%s\nafter:\n%s", refs, after)
			}
		})
	}

	t.Run("the occupant of a refused destination is untouched", func(t *testing.T) {
		top := f.repo("occupant")
		f.git(top, "branch", "free")
		dir := filepath.Join(f.root, "occupied")
		occupant := f.write(dir, "someone-elses.txt", "keep me\n")
		if err := f.open(top).AddWorktree(t.Context(), dir, "free"); err == nil {
			t.Fatal("AddWorktree() over an occupied directory error = nil, want an error")
		}
		body, err := os.ReadFile(occupant) //nolint:gosec // G304: occupant is a file this test wrote under t.TempDir().
		if err != nil || string(body) != "keep me\n" {
			t.Errorf("the occupant is now %q (read error %v)", body, err)
		}
	})
}

// TestRemoveWorktree covers removal, including the entries it must refuse
// to touch and the stale entry it must clean up without pruning anyone
// else's.
func TestRemoveWorktree(t *testing.T) {
	f := newFixture(t)

	t.Run("a worktree with uncommitted work is removed and its branch kept", func(t *testing.T) {
		top := f.repo("dirty")
		dir := filepath.Join(f.root, "dirty-wt")
		f.git(top, "worktree", "add", "-q", "-b", "kept", dir)
		f.write(dir, "scratch.txt", "not committed\n")
		f.write(dir, "a.txt", "modified\n")

		if err := f.open(top).RemoveWorktree(t.Context(), dir); err != nil {
			t.Fatalf("RemoveWorktree() error = %v", err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%q still exists (stat error %v)", dir, err)
		}
		assertWorktreeCount(t, f, top, 1)
		if out := f.git(top, "for-each-ref", "refs/heads/kept"); out == "" {
			t.Error("RemoveWorktree deleted the branch; removing a worktree must never delete a branch")
		}
	})

	t.Run("a second removal is not an error", func(t *testing.T) {
		top := f.repo("twice")
		dir := filepath.Join(f.root, "twice-wt")
		f.git(top, "worktree", "add", "-q", "-b", "t", dir)
		r := f.open(top)
		if err := r.RemoveWorktree(t.Context(), dir); err != nil {
			t.Fatalf("first RemoveWorktree() error = %v", err)
		}
		if err := r.RemoveWorktree(t.Context(), dir); err != nil {
			t.Errorf("second RemoveWorktree() error = %v, want nil: the worktree is gone, which is what was asked", err)
		}
	})

	t.Run("a directory that was never a worktree is not an error", func(t *testing.T) {
		top := f.repo("never")
		if err := f.open(top).RemoveWorktree(t.Context(), filepath.Join(f.root, "never-registered")); err != nil {
			t.Errorf("RemoveWorktree(<never registered>) error = %v, want nil", err)
		}
	})

	t.Run("a stale entry is removed and nobody else's is pruned", func(t *testing.T) {
		top := f.repo("stale")
		ours := filepath.Join(f.root, "stale-ours")
		theirs := filepath.Join(f.root, "stale-theirs")
		f.git(top, "worktree", "add", "-q", "-b", "ours", ours)
		f.git(top, "worktree", "add", "-q", "-b", "theirs", theirs)
		for _, dir := range []string{ours, theirs} {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatalf("RemoveAll(%q) error = %v", dir, err)
			}
		}

		r := f.open(top)
		if err := r.RemoveWorktree(t.Context(), ours); err != nil {
			t.Fatalf("RemoveWorktree(<stale>) error = %v", err)
		}
		list, err := r.Worktrees(t.Context())
		if err != nil {
			t.Fatalf("Worktrees() error = %v", err)
		}
		paths := make([]string, len(list))
		for i, w := range list {
			paths[i] = w.Path
		}
		if slices.Contains(paths, ours) {
			t.Errorf("the stale entry for %q is still registered", ours)
		}
		if !slices.Contains(paths, theirs) {
			t.Errorf("the user's own stale entry for %q was pruned; only a repo-wide `git worktree prune` does that, and belay never runs one", theirs)
		}
	})

	t.Run("a path through a symlink names the same worktree", func(t *testing.T) {
		top := f.repo("linked")
		dir := filepath.Join(f.root, "linked-wt")
		f.git(top, "worktree", "add", "-q", "-b", "l", dir)
		alias := filepath.Join(f.root, "alias")
		if err := os.Symlink(f.root, alias); err != nil {
			t.Skipf("cannot create a symlink here: %v", err)
		}
		if err := f.open(top).RemoveWorktree(t.Context(), filepath.Join(alias, "linked-wt")); err != nil {
			t.Fatalf("RemoveWorktree(<via symlink>) error = %v", err)
		}
		assertWorktreeCount(t, f, top, 1)
	})

	t.Run("a locked worktree is refused", func(t *testing.T) {
		top := f.repo("locked")
		dir := filepath.Join(f.root, "locked-wt")
		f.git(top, "worktree", "add", "-q", "-b", "held", dir)
		f.git(top, "worktree", "lock", "--reason", "on a usb stick", dir)

		err := f.open(top).RemoveWorktree(t.Context(), dir)
		if !errors.Is(err, ErrLocked) {
			t.Fatalf("RemoveWorktree(<locked>) error = %v, want %v", err, ErrLocked)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("the locked worktree's directory is gone: %v", err)
		}
		assertWorktreeCount(t, f, top, 2)
	})

	t.Run("the main working tree is refused", func(t *testing.T) {
		top := f.repo("main")
		if err := f.open(top).RemoveWorktree(t.Context(), top); err == nil {
			t.Fatal("RemoveWorktree(<main working tree>) error = nil, want an error")
		}
		if _, err := os.Stat(filepath.Join(top, "a.txt")); err != nil {
			t.Errorf("the main working tree was damaged: %v", err)
		}
	})

	t.Run("a relative path is refused", func(t *testing.T) {
		top := f.repo("relative")
		if err := f.open(top).RemoveWorktree(t.Context(), "relative/wt"); err == nil {
			t.Error("RemoveWorktree(<relative>) error = nil, want an error")
		}
	})
}

// assertWorktreeCount fails t unless the repository at top has exactly want
// worktrees registered, the main one included.
func assertWorktreeCount(t *testing.T, f *fixture, top string, want int) {
	t.Helper()
	list, err := f.open(top).Worktrees(t.Context())
	if err != nil {
		t.Fatalf("Worktrees() error = %v", err)
	}
	if len(list) != want {
		t.Errorf("%d worktrees are registered, want %d: %+v", len(list), want, list)
	}
}
