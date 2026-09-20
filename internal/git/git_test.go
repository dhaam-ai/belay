//go:build unix

package git

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/internal/vcs"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestOpen pins what Open learns about a directory, and which refusals mean
// "never" and which mean "not like this".
func TestOpen(t *testing.T) {
	f := newFixture(t)

	plain := f.dir("plain")
	top := f.repo("repo")
	sub := filepath.Join(top, "sub")
	bare := f.bareRepo("bare.git")
	linked := filepath.Join(f.root, "linked")
	f.git(top, "worktree", "add", "-q", linked, "-b", "side")

	tests := []struct {
		name       string
		dir        string
		wantTop    string
		wantCommon string
		wantPrefix string
		wantErr    error
		// unsupported says whether the error must also satisfy
		// belay.ErrUnsupported, which is belay's word for "never".
		unsupported bool
	}{
		{
			name:       "the top of a repository",
			dir:        top,
			wantTop:    top,
			wantCommon: filepath.Join(top, ".git"),
		},
		{
			name:       "a subdirectory names itself in the prefix",
			dir:        sub,
			wantTop:    top,
			wantCommon: filepath.Join(top, ".git"),
			wantPrefix: "sub/",
		},
		{
			name:       "a linked worktree keeps its own top and the shared common dir",
			dir:        linked,
			wantTop:    linked,
			wantCommon: filepath.Join(top, ".git"),
		},
		{
			name:        "a plain directory is not a repository",
			dir:         plain,
			wantErr:     ErrNotARepository,
			unsupported: true,
		},
		{
			name:        "a bare repository has no working tree to edit",
			dir:         bare,
			wantErr:     ErrBare,
			unsupported: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Open(t.Context(), f.run, tt.dir)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Open(%q) error = %v, want %v", tt.dir, err, tt.wantErr)
				}
				if got := errors.Is(err, belay.ErrUnsupported); got != tt.unsupported {
					t.Errorf("errors.Is(err, belay.ErrUnsupported) = %t, want %t", got, tt.unsupported)
				}
				if r != nil {
					t.Errorf("Open(%q) returned a Repo alongside its error", tt.dir)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open(%q) error = %v", tt.dir, err)
			}
			if got := r.Top(); got != tt.wantTop {
				t.Errorf("Top() = %q, want %q", got, tt.wantTop)
			}
			if got := r.CommonDir(); got != tt.wantCommon {
				t.Errorf("CommonDir() = %q, want %q: `rev-parse --git-common-dir` is relative without --path-format=absolute", got, tt.wantCommon)
			}
			if got := r.Prefix(); got != tt.wantPrefix {
				t.Errorf("Prefix() = %q, want %q", got, tt.wantPrefix)
			}
			if !r.Version().AtLeast(MinVersion) {
				t.Errorf("Version() = %s, which Open should have refused", r.Version())
			}
		})
	}
}

// TestOpenRefusesAGitItCannotUse pins the two toolchain refusals. A caller
// turns both into "fall back to editing in place", never into "this
// repository cannot be used", so neither may satisfy belay.ErrUnsupported.
func TestOpenRefusesAGitItCannotUse(t *testing.T) {
	tests := []struct {
		name    string
		version string
		wantErr error
	}{
		{name: "one minor too old", version: "git version 2.37.1\n", wantErr: ErrTooOld},
		{name: "much too old", version: "git version 1.9.5\n", wantErr: ErrTooOld},
		{name: "not git at all", version: "hg version 6.7\n", wantErr: ErrUnreadableVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{reply: func(bexec.Command) (bexec.Result, error) {
				return bexec.Result{Stdout: tt.version}, nil
			}}
			r, err := Open(t.Context(), rec, t.TempDir())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Open() error = %v, want %v", err, tt.wantErr)
			}
			if !errors.Is(err, belay.ErrToolchainMissing) {
				t.Errorf("errors.Is(err, belay.ErrToolchainMissing) = false for %v", err)
			}
			if errors.Is(err, belay.ErrUnsupported) {
				t.Errorf("errors.Is(err, belay.ErrUnsupported) = true for %v; upgrading git fixes it", err)
			}
			if r != nil {
				t.Error("Open returned a Repo alongside its error")
			}
			if got := len(rec.commands()); got != 1 {
				t.Errorf("Open ran %d commands with a git it cannot use; want 1, the version probe itself", got)
			}
		})
	}
}

// TestOpenReportsAMissingGitAsAToolchainProblem proves a missing binary is not
// mistaken for "not a repository". Both are non-zero outcomes of the first
// command, and only one of them is about the directory.
func TestOpenReportsAMissingGitAsAToolchainProblem(t *testing.T) {
	rec := &recorder{reply: func(c bexec.Command) (bexec.Result, error) {
		return bexec.Result{ExitCode: -1}, &bexec.ToolchainError{Tool: c.Path, Err: os.ErrNotExist, Detail: "not found"}
	}}
	_, err := Open(t.Context(), rec, t.TempDir())
	if !errors.Is(err, belay.ErrToolchainMissing) {
		t.Errorf("errors.Is(err, belay.ErrToolchainMissing) = false for %v", err)
	}
	if errors.Is(err, ErrNotARepository) {
		t.Errorf("errors.Is(err, ErrNotARepository) = true for %v; the directory was never looked at", err)
	}
}

// TestAMissingDirectoryIsNotAMissingGit pins the other half. internal/exec
// reports a failed chdir as a missing toolchain, because both surface as
// ENOENT, so a worktree directory someone deleted would read as "git is not
// installed" unless this package looks first.
func TestAMissingDirectoryIsNotAMissingGit(t *testing.T) {
	f := newFixture(t)
	gone := filepath.Join(f.root, "deleted")

	_, err := Open(t.Context(), f.run, gone)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(<missing dir>) error = %v, want one wrapping fs.ErrNotExist", err)
	}
	if errors.Is(err, belay.ErrToolchainMissing) || errors.Is(err, bexec.ErrToolchainMissing) {
		t.Errorf("Open(<missing dir>) error = %v, which claims git is missing; git is fine, the directory is not", err)
	}

	r := f.open(f.repo("present"))
	_, err = r.Status(t.Context(), gone)
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, bexec.ErrToolchainMissing) {
		t.Errorf("Status(<missing dir>) error = %v, want one wrapping fs.ErrNotExist and not a missing toolchain", err)
	}
}

// TestHead covers the three states HEAD can be in. A commitless repository
// is deliberately not ErrUnsupported: one commit fixes it, and telling a
// user "never" for something they can fix in one command is worse than
// telling them nothing.
func TestHead(t *testing.T) {
	f := newFixture(t)

	t.Run("on a branch", func(t *testing.T) {
		dir := f.repo("onbranch")
		head, err := f.open(dir).Head(t.Context())
		if err != nil {
			t.Fatalf("Head() error = %v", err)
		}
		want := Head{Commit: f.git(dir, "rev-parse", "HEAD"), Branch: "main"}
		if head != want {
			t.Errorf("Head() = %+v, want %+v", head, want)
		}
	})

	t.Run("detached", func(t *testing.T) {
		dir := f.repo("detached")
		r := f.open(dir)
		f.git(dir, "checkout", "-q", "--detach", "HEAD")

		head, err := r.Head(t.Context())
		if err != nil {
			t.Fatalf("Head() error = %v", err)
		}
		want := Head{Commit: f.git(dir, "rev-parse", "HEAD"), Branch: ""}
		if head != want {
			t.Errorf("Head() = %+v, want %+v: a detached HEAD is on no branch", head, want)
		}
	})

	t.Run("no commits yet", func(t *testing.T) {
		_, err := f.open(f.emptyRepo("nocommits")).Head(t.Context())
		if !errors.Is(err, ErrNoCommits) {
			t.Fatalf("Head() error = %v, want %v", err, ErrNoCommits)
		}
		if errors.Is(err, belay.ErrUnsupported) {
			t.Error("errors.Is(err, belay.ErrUnsupported) = true; one commit fixes this, so it is not 'never'")
		}
	})
}

// TestHasSubmodules pins the probe output.mode: auto uses to fall back. A
// repository with submodules cannot be reproduced by checking HEAD out into
// a fresh worktree, because nothing initialises the submodules there.
func TestHasSubmodules(t *testing.T) {
	f := newFixture(t)

	if f.open(f.repo("plain")).HasSubmodules() {
		t.Error("HasSubmodules() = true for a repository with no .gitmodules")
	}

	dir := f.repo("withsub")
	f.write(dir, ".gitmodules", "[submodule \"vendor/lib\"]\n\tpath = vendor/lib\n\turl = ../lib.git\n")
	f.git(dir, "add", ".gitmodules")
	f.git(dir, "commit", "-qm", "add a submodule")
	if !f.open(dir).HasSubmodules() {
		t.Error("HasSubmodules() = false for a repository whose index holds .gitmodules")
	}
	if !f.open(filepath.Join(dir, "sub")).HasSubmodules() {
		t.Error("HasSubmodules() = false when opened from a subdirectory; .gitmodules lives at the top")
	}
}

// TestStatus pins what counts as dirty, including the one directory belay
// writes into every repository it runs in.
func TestStatus(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  []string
	}{
		{
			name:  "a clean tree reports nothing",
			setup: func(*testing.T, string) {},
		},
		{
			name: "belay's own directory is not the user's change",
			setup: func(_ *testing.T, dir string) {
				f.write(dir, ".belay/runs/r1/journal.ndjson", "{}\n")
			},
		},
		{
			name: "a modified tracked file",
			setup: func(_ *testing.T, dir string) {
				f.write(dir, "a.txt", "changed\n")
			},
			want: []string{"a.txt"},
		},
		{
			name: "a deleted tracked file",
			setup: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
					t.Fatalf("Remove error = %v", err)
				}
			},
			want: []string{"a.txt"},
		},
		{
			name: "an untracked file",
			setup: func(_ *testing.T, dir string) {
				f.write(dir, "new.txt", "new\n")
			},
			want: []string{"new.txt"},
		},
		{
			name: "an untracked directory is reported file by file",
			setup: func(_ *testing.T, dir string) {
				f.write(dir, "pkg/one.go", "package pkg\n")
				f.write(dir, "pkg/two.go", "package pkg\n")
			},
			want: []string{"pkg/one.go", "pkg/two.go"},
		},
		{
			name: "a staged rename is two paths, not one record with two fields",
			setup: func(_ *testing.T, dir string) {
				f.git(dir, "mv", "a.txt", "renamed.txt")
			},
			want: []string{"a.txt", "renamed.txt"},
		},
		{
			name: "a name with a space and a newline survives",
			setup: func(_ *testing.T, dir string) {
				f.write(dir, "odd name\nhere.txt", "odd\n")
			},
			want: []string{"odd name\nhere.txt"},
		},
		{
			name: "everything at once, and .belay still does not count",
			setup: func(_ *testing.T, dir string) {
				f.write(dir, "a.txt", "changed\n")
				f.write(dir, "new.txt", "new\n")
				f.write(dir, ".belay/x", "x\n")
			},
			want: []string{"a.txt", "new.txt"},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := f.repo("status" + strings.Repeat("x", i))
			r := f.open(dir)
			tt.setup(t, dir)

			got, err := r.Status(t.Context(), dir)
			if err != nil {
				t.Fatalf("Status(%q) error = %v", dir, err)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("Status() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStatusAnswersForTheWholeWorkingTree proves Status answers about the
// whole working tree whatever directory inside it is handed. A workspace may
// be a subdirectory of its worktree, and a dirty file outside that
// subdirectory is still a dirty working tree.
func TestStatusAnswersForTheWholeWorkingTree(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("fromtop")
	r := f.open(dir)
	f.write(dir, "a.txt", "changed at the top\n")
	f.write(dir, "sub/b.txt", "changed in a subdirectory\n")

	got, err := r.Status(t.Context(), filepath.Join(dir, "sub"))
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	slices.Sort(got)
	if want := []string{"a.txt", "sub/b.txt"}; !slices.Equal(got, want) {
		t.Errorf("Status(<subdirectory>) = %q, want %q: the answer is about the working tree, with paths from its top", got, want)
	}
}

// TestStatusDoesNotWriteTheIndex is why --no-optional-locks is on every
// command. `belay run --dry-run` promises to create nothing, and a plain
// `git status` rewrites .git/index as a side effect of refreshing the stat
// data it caches.
func TestStatusDoesNotWriteTheIndex(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("noindexwrite")
	r := f.open(dir)

	// A tracked file whose mtime no longer matches the cached stat data is
	// exactly what makes git want to refresh the index. The index is also
	// backdated, so git's racy-timestamp rule cannot be why it is written.
	index := filepath.Join(dir, ".git", "index")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(index, past, past); err != nil {
		t.Fatalf("Chtimes(%q) error = %v", index, err)
	}
	if err := os.Chtimes(filepath.Join(dir, "a.txt"), time.Now(), time.Now()); err != nil {
		t.Fatalf("Chtimes(a.txt) error = %v", err)
	}
	before, err := os.Stat(index)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", index, err)
	}

	if _, err := r.Status(t.Context(), dir); err != nil {
		t.Fatalf("Status() error = %v", err)
	}

	after, err := os.Stat(index)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", index, err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf(".git/index was rewritten (mtime %v -> %v): Status must not write, so something dropped --no-optional-locks",
			before.ModTime(), after.ModTime())
	}
}

// TestRefusesTruncatedOutput proves a capped capture is an error rather than
// a short answer. A repository big enough to overflow the capture would
// otherwise report a clean-looking subset of its dirty files, and a caller
// would branch from a tree it believes is clean.
func TestRefusesTruncatedOutput(t *testing.T) {
	rec := &recorder{reply: func(c bexec.Command) (bexec.Result, error) {
		if subcommand(c) == "version" {
			return bexec.Result{Stdout: "git version 2.54.0\n"}, nil
		}
		return bexec.Result{Stdout: "/a\ntrue\n", Truncated: true}, nil
	}}
	if _, err := Open(t.Context(), rec, t.TempDir()); !errors.Is(err, ErrTruncated) {
		t.Fatalf("Open() error = %v, want %v", err, ErrTruncated)
	}
}

// hardeningPrefix is the argv every git this package starts must begin with.
var hardeningPrefix = []string{
	"--no-optional-locks",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "commit.gpgsign=false",
	"-c", "tag.gpgsign=false",
	"-c", "gc.auto=0",
}

// hardenedCall is one method that starts git, set up to succeed.
type hardenedCall struct {
	name string
	call func(r *Repo) error
}

// hardenedCalls names every method that starts git, in an order in which
// each can succeed, so the hardening test cannot miss one: a method that is
// not listed here has nothing proving its argv carries the flags. Each call
// must succeed, or its argv proves nothing.
func hardenedCalls(t *testing.T, f *fixture, dir string) []hardenedCall {
	t.Helper()
	wt := filepath.Join(f.root, "hardened-wt")
	return []hardenedCall{
		{name: "Head", call: func(r *Repo) error { _, err := r.Head(t.Context()); return err }},
		{name: "Status", call: func(r *Repo) error { _, err := r.Status(t.Context(), dir); return err }},
		{name: "Worktrees", call: func(r *Repo) error { _, err := r.Worktrees(t.Context()); return err }},
		{name: "AddWorktree", call: func(r *Repo) error {
			f.git(dir, "branch", "hardened-wt")
			return r.AddWorktree(t.Context(), wt, "hardened-wt")
		}},
		{name: "RemoveWorktree", call: func(r *Repo) error { return r.RemoveWorktree(t.Context(), wt) }},
		{name: "CreateBranch", call: func(r *Repo) error {
			return r.CreateBranch(t.Context(), "hardened-branch", f.git(dir, "rev-parse", "HEAD"))
		}},
		{name: "BranchTip", call: func(r *Repo) error { _, _, err := r.BranchTip(t.Context(), "hardened-branch"); return err }},
		{name: "ContainedElsewhere", call: func(r *Repo) error {
			_, err := r.ContainedElsewhere(t.Context(), "hardened-branch")
			return err
		}},
		{name: "DeleteBranch", call: func(r *Repo) error {
			return r.DeleteBranch(t.Context(), "hardened-branch", f.git(dir, "rev-parse", "refs/heads/hardened-branch"))
		}},
		{name: "Commit", call: func(r *Repo) error {
			f.write(dir, "a.txt", "changed for the hardening test\n")
			_, err := r.Commit(t.Context(), dir, vcs.Message{Subject: "belay: hardening"})
			return err
		}},
		{name: "Changed", call: func(r *Repo) error {
			_, err := r.Changed(t.Context(), "HEAD~1", "HEAD")
			return err
		}},
		{name: "Integrate", call: func(r *Repo) error {
			f.git(dir, "branch", "hardened-merge")
			commitOn(f, dir, "hardened-merge", "work to integrate\n")
			_, err := r.Integrate(t.Context(), dir, "hardened-merge", vcs.Message{Subject: "belay: hardening merge"})
			return err
		}},
	}
}

// TestEveryCommandIsHardened is the argv test. Every git this package starts
// must carry the six global options and the two environment variables the
// package doc lists, because each closes a hole that is only closed when it
// is on every command.
//
// The recorder forwards every command to a real git, so the argv checked are
// the ones real calls built against a real repository.
func TestEveryCommandIsHardened(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("hardened")
	rec := &recorder{next: f.run}

	r, err := Open(t.Context(), rec, dir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	calls := append([]hardenedCall{{name: "Open"}}, hardenedCalls(t, f, dir)...)

	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			if tc.call != nil {
				rec.reset()
				if err := tc.call(r); err != nil {
					t.Fatalf("%s error = %v; the call must succeed for its argv to mean anything", tc.name, err)
				}
			}
			cmds := rec.commands()
			if len(cmds) == 0 {
				t.Fatalf("%s started no git", tc.name)
			}
			for _, c := range cmds {
				assertHardened(t, c)
			}
		})
	}
}

// assertHardened fails t unless c is a git command carrying the full
// hardening prefix and environment.
func assertHardened(t *testing.T, c bexec.Command) {
	t.Helper()
	if c.Path != "git" {
		t.Errorf("runs %q, not git", c.Path)
	}
	if len(c.Args) < len(hardeningPrefix) || !slices.Equal(c.Args[:len(hardeningPrefix)], hardeningPrefix) {
		t.Errorf("git %s: argv %q does not start with the hardening prefix %q", subcommand(c), c.Args, hardeningPrefix)
	}
	if got := c.ExtraEnv["GIT_TERMINAL_PROMPT"]; got != "0" {
		t.Errorf("git %s: GIT_TERMINAL_PROMPT=%q, want %q", subcommand(c), got, "0")
	}
	if got := c.ExtraEnv["LC_ALL"]; got != "C" {
		t.Errorf("git %s: LC_ALL=%q, want %q", subcommand(c), got, "C")
	}
}
