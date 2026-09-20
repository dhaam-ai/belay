//go:build unix

package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/internal/git"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestBranchNaming pins the name a candidate's branch gets, which is the one
// thing about this package another slice depends on: promote asks the
// isolator for it through an optional interface, so the name has to be a
// function of the id and nothing else.
func TestBranchNaming(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("naming")
	iso := f.newIsolator(dir)

	got, err := iso.Branch("cand-01-a1")
	if err != nil {
		t.Fatalf("Branch() error = %v", err)
	}
	if want := testNamespace + "-cand-01-a1"; got != want {
		t.Errorf("Branch() = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, testNamespace+"/") {
		t.Errorf("Branch() = %q nests under the run branch, which git cannot store beside it", got)
	}
	if _, err := iso.Branch("a b"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("Branch(%q) error = %v, want %v", "a b", err, ErrInvalidID)
	}

	root := filepath.Join(dir, ".belay", "other")
	for _, ns := range []string{"", "belay/run1/", "belay//run1", "belay/run1.lock", "belay/ru n1", "HEAD", "refs/heads/x"} {
		if _, err := New(f.open(dir), root, ns); err == nil {
			t.Errorf("New(namespace %q) = nil error, want a refusal", ns)
		}
	}
}

// TestBranchNamesCannotNestUnderTheRunBranch is why the separator is "-" and
// not "/". Refs are stored as paths, so refs/heads/belay/<run> and
// refs/heads/belay/<run>/<cand> would need one name to be both a file and a
// directory. This asserts git still refuses that, and that the name this
// package builds instead is one git accepts.
func TestBranchNamesCannotNestUnderTheRunBranch(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("dfconflict")
	f.git(dir, "branch", testNamespace)

	if _, err := f.tryGit(dir, "branch", testNamespace+"/cand-01"); err == nil {
		t.Fatalf("git created %s beside %s, so the naming this package uses is solving a problem that no longer exists",
			testNamespace+"/cand-01", testNamespace)
	}

	iso := f.newIsolator(dir)
	branch, err := iso.Branch("cand-01")
	if err != nil {
		t.Fatalf("Branch() error = %v", err)
	}
	if _, err := f.tryGit(dir, "branch", branch); err != nil {
		t.Fatalf("git refused the name this package builds, %q: %v", branch, err)
	}
}

// TestCreateMakesAWorktreeOnItsOwnBranch is the happy path, and the reason
// this isolator exists: a candidate gets the source's committed tree without
// a copy of it, on a branch of its own, and writing in it does not reach the
// source.
func TestCreateMakesAWorktreeOnItsOwnBranch(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("create")
	iso := f.newIsolator(dir)
	head := f.git(dir, "rev-parse", "HEAD")

	ws := f.mustCreate(iso, dir, "cand-01")

	if ws.ID != "cand-01" {
		t.Errorf("Workspace.ID = %q, want %q", ws.ID, "cand-01")
	}
	if !ws.Ephemeral {
		t.Error("Workspace.Ephemeral = false; Destroy really does remove this tree")
	}
	if want := filepath.Join(iso.Root(), "cand-01"); ws.Dir != want {
		t.Errorf("Workspace.Dir = %q, want %q", ws.Dir, want)
	}
	if body, err := readFile(filepath.Join(ws.Dir, "a.txt")); err != nil || string(body) != "a\n" {
		t.Errorf("the candidate does not hold the source's committed tree: %q, %v", body, err)
	}

	branch, err := iso.Branch("cand-01")
	if err != nil {
		t.Fatalf("Branch() error = %v", err)
	}
	if tip := f.git(dir, "rev-parse", "refs/heads/"+branch); tip != head {
		t.Errorf("branch %s is at %s, want the source's HEAD %s", branch, tip, head)
	}
	if got := f.git(ws.Dir, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Errorf("the candidate is on branch %q, want %q", got, branch)
	}
	if !slices.Contains(f.worktreePaths(dir), ws.Dir) {
		t.Errorf("git does not list %q as a worktree: %v", ws.Dir, f.worktreePaths(dir))
	}

	// Writing in the candidate is the whole point, and it must not reach the
	// source.
	f.write(ws.Dir, "a.txt", "the candidate's work\n")
	if body, err := readFile(filepath.Join(dir, "a.txt")); err != nil || string(body) != "a\n" {
		t.Errorf("the source changed when the candidate was written to: %q, %v", body, err)
	}
	// .belay is left out, exactly as internal/git's Status leaves it out:
	// belay writes that directory into every repository it runs in, and the
	// candidate root is inside it, so counting it would make every source
	// dirty the moment a run started.
	if out := f.git(dir, "status", "--porcelain", "--", ":/", ":(exclude,top).belay"); out != "" {
		t.Errorf("the source is dirty after a Create: %q", out)
	}
}

// TestCreateRejectsHostileIDs covers both rule sets an id has to pass: it
// names one directory under the candidate root, and it is one component of a
// ref. The second set is why the list is longer than a directory isolator's:
// "x.lock" and "@{0}" are perfectly good directory names and git refuses both
// as ref components, and finding that out from git happens after a branch has
// already been created.
func TestCreateRejectsHostileIDs(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("hostile-ids")
	iso := f.newIsolator(dir)
	before := f.branches(dir)

	ids := []string{
		"", " ", ".", "..", "a/b", `a\b`, "a..b", "/abs", "x.lock", "x.LOCK",
		"@{0}", "@", ".hidden", "trailing.", "a b", "a~1", "a^", "a:b", "a?", "a*",
		"a[b", "-dash", "nul\x00byte", strings.Repeat("x", 256),
	}
	for _, id := range ids {
		t.Run(strconv.Quote(id), func(t *testing.T) {
			ws, err := iso.Create(t.Context(), dir, id)
			if !errors.Is(err, ErrInvalidID) {
				t.Fatalf("Create(id %q) error = %v, want %v", id, err, ErrInvalidID)
			}
			var ie *IDError
			if !errors.As(err, &ie) {
				t.Errorf("Create(id %q) error is not an *IDError: %v", id, err)
			}
			if ws != (belay.Workspace{}) {
				t.Errorf("Create(id %q) = %+v, want the zero Workspace", id, ws)
			}
		})
	}

	if got := f.branches(dir); !slices.Equal(got, before) {
		t.Errorf("branches after the refusals = %v, want %v", got, before)
	}
	entries, err := os.ReadDir(iso.Root())
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", iso.Root(), err)
	}
	if len(entries) != 0 {
		t.Errorf("the candidate root holds %d entries after the refusals", len(entries))
	}
}

// TestCreateLeavesNothingBehindOnFailure is belay.Isolator's contract: a
// non-nil error means the zero Workspace and nothing for the caller to
// Destroy. The branch is the part that is easy to get wrong — `git worktree
// add -b` creates its branch even when the add then fails, which is why this
// package creates the branch itself and unwinds it by hand.
func TestCreateLeavesNothingBehindOnFailure(t *testing.T) {
	tests := []struct {
		name string
		// build prepares the failure and returns the isolator, the source,
		// the id and the sentinel the error must wrap.
		build func(t *testing.T, f *fixture, dir string) (iso *Isolator, src, id string, want error)
		// ctx, when set, replaces the test's context.
		ctx func(t *testing.T) context.Context
	}{
		{
			name: "the destination already holds files",
			build: func(_ *testing.T, f *fixture, dir string) (*Isolator, string, string, error) {
				iso := f.newIsolator(dir)
				f.write(filepath.Join(iso.Root(), "cand-01"), "squatter.txt", "mine\n")
				return iso, dir, "cand-01", ErrNotEmpty
			},
		},
		{
			name: "the branch already exists",
			build: func(t *testing.T, f *fixture, dir string) (*Isolator, string, string, error) {
				iso := f.newIsolator(dir)
				branch, err := iso.Branch("cand-01")
				if err != nil {
					t.Fatalf("Branch() error = %v", err)
				}
				f.git(dir, "branch", branch)
				return iso, dir, "cand-01", git.ErrBranchExists
			},
		},
		{
			name: "a worktree is already registered at the destination",
			build: func(t *testing.T, f *fixture, dir string) (*Isolator, string, string, error) {
				iso := f.newIsolator(dir)
				dest := filepath.Join(iso.Root(), "cand-01")
				f.git(dir, "branch", "squatter")
				f.git(dir, "worktree", "add", "-q", dest, "squatter")
				// Removed by hand, the way a crash or an rm -rf leaves it:
				// the registration is still there and git refuses to add
				// over it.
				if err := os.RemoveAll(dest); err != nil {
					t.Fatalf("RemoveAll(%q) error = %v", dest, err)
				}
				return iso, dir, "cand-01", ErrStaleWorktree
			},
		},
		{
			name: "the context is already cancelled",
			build: func(_ *testing.T, f *fixture, dir string) (*Isolator, string, string, error) {
				return f.newIsolator(dir), dir, "cand-01", context.Canceled
			},
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
		},
		{
			name: "git fails while adding the worktree",
			build: func(t *testing.T, f *fixture, dir string) (*Isolator, string, string, error) {
				root := filepath.Join(dir, ".belay", "candidates")
				repo := f.openWith(&breaker{next: f.run, on: []string{"worktree", "add"}}, dir)
				iso, err := New(repo, root, testNamespace, WithLogger(quiet()))
				if err != nil {
					t.Fatalf("New() error = %v", err)
				}
				return iso, dir, "cand-01", errBroken
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			dir := f.repo("unwind" + strconv.Itoa(i))
			iso, src, id, want := tt.build(t, f, dir)
			before := f.branches(dir)
			worktreesBefore := f.worktreePaths(dir)

			ctx := t.Context()
			if tt.ctx != nil {
				ctx = tt.ctx(t)
			}
			ws, err := iso.Create(ctx, src, id)
			if !errors.Is(err, want) {
				t.Fatalf("Create() error = %v, want it to wrap %v", err, want)
			}
			if ws != (belay.Workspace{}) {
				t.Errorf("Create() = %+v, want the zero Workspace", ws)
			}
			if got := f.branches(dir); !slices.Equal(got, before) {
				t.Errorf("branches after the failure = %v, want %v", got, before)
			}
			if got := f.worktreePaths(dir); !slices.Equal(got, worktreesBefore) {
				t.Errorf("worktrees after the failure = %v, want %v", got, worktreesBefore)
			}
		})
	}
}

// TestCreateRefusesADirtySource keeps the two isolators honest about the same
// question. A directory copy carries the source's uncommitted edits into the
// candidate; a worktree is a checkout of a commit and cannot. Branching from
// HEAD and saying nothing would mean candidates silently start from a
// different tree than the one the user is looking at.
func TestCreateRefusesADirtySource(t *testing.T) {
	tests := []struct {
		name string
		// dirty makes the source dirty and returns the paths the error must
		// name, or nil when the source is still considered clean.
		dirty func(f *fixture, dir string) []string
	}{
		{
			name: "a modified tracked file",
			dirty: func(f *fixture, dir string) []string {
				f.write(dir, "a.txt", "edited\n")
				return []string{"a.txt"}
			},
		},
		{
			name: "an untracked file",
			dirty: func(f *fixture, dir string) []string {
				f.write(dir, "new.txt", "new\n")
				return []string{"new.txt"}
			},
		},
		{
			name: "a deleted file",
			dirty: func(f *fixture, dir string) []string {
				if err := os.Remove(filepath.Join(dir, "sub", "b.txt")); err != nil {
					f.t.Fatalf("Remove error = %v", err)
				}
				return []string{"sub/b.txt"}
			},
		},
		{
			name: "belay's own directory does not count as dirty",
			dirty: func(f *fixture, dir string) []string {
				f.write(dir, ".belay/runs/r1/journal.ndjson", "{}\n")
				return nil
			},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			dir := f.repo("dirty" + strconv.Itoa(i))
			iso := f.newIsolator(dir)
			want := tt.dirty(f, dir)

			ws, err := iso.Create(t.Context(), dir, "cand-01")
			if want == nil {
				if err != nil {
					t.Fatalf("Create() error = %v, want a workspace", err)
				}
				if ws.Dir == "" {
					t.Error("Create() returned a workspace with no directory")
				}
				return
			}
			if !errors.Is(err, ErrDirtySource) {
				t.Fatalf("Create() error = %v, want %v", err, ErrDirtySource)
			}
			var de *DirtyError
			if !errors.As(err, &de) {
				t.Fatalf("Create() error is not a *DirtyError: %v", err)
			}
			if !slices.Equal(de.Paths, want) {
				t.Errorf("DirtyError.Paths = %v, want %v", de.Paths, want)
			}
			if ws != (belay.Workspace{}) {
				t.Errorf("Create() = %+v, want the zero Workspace", ws)
			}
		})
	}
}

// TestCreateRefusesSourcesItCannotBranchFrom separates "never" from "not
// yet". A source in another repository, or in none, can never be isolated
// this way and the caller should fall back to copying the directory. A
// repository with no commits has nothing to branch from today and one commit
// fixes it, so telling the user "never" there would be wrong.
func TestCreateRefusesSourcesItCannotBranchFrom(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("sources")
	iso := f.newIsolator(dir)

	tests := []struct {
		name string
		// source returns the src to isolate.
		source func(t *testing.T) string
		want   error
		// unsupported says whether the error must also satisfy
		// belay.ErrUnsupported, which is a caller's signal to use another
		// isolator instead of telling the user to fix something.
		unsupported bool
	}{
		{
			name:        "a directory that is in no repository",
			source:      func(*testing.T) string { return f.dir("not-a-repo") },
			want:        ErrForeignSource,
			unsupported: true,
		},
		{
			name:        "a working tree of another repository",
			source:      func(*testing.T) string { return f.repo("elsewhere") },
			want:        ErrForeignSource,
			unsupported: true,
		},
		{
			name:   "a repository with no commits",
			source: func(*testing.T) string { return f.emptyRepo("commitless") },
			want:   git.ErrNoCommits,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := tt.source(t)
			use := iso
			if errors.Is(tt.want, git.ErrNoCommits) {
				// A commitless repository is its own repository, so the
				// isolator has to be built over it for the row to be about
				// commits rather than about foreignness.
				use = f.newIsolator(src)
			}
			ws, err := use.Create(t.Context(), src, "cand-01")
			if !errors.Is(err, tt.want) {
				t.Fatalf("Create(%q) error = %v, want %v", src, err, tt.want)
			}
			if got := errors.Is(err, belay.ErrUnsupported); got != tt.unsupported {
				t.Errorf("errors.Is(err, belay.ErrUnsupported) = %v, want %v (error %v)", got, tt.unsupported, err)
			}
			if ws != (belay.Workspace{}) {
				t.Errorf("Create() = %+v, want the zero Workspace", ws)
			}
		})
	}
}

// TestCreateFollowsTheSourceItWasGiven covers the two ways src is not simply
// "the repository". A run works in its own linked worktree, so a candidate
// branches from that worktree's HEAD, not from the user's checkout. And a run
// started in a subdirectory hands its agent that subdirectory, so the
// workspace has to point at the same place inside the candidate.
func TestCreateFollowsTheSourceItWasGiven(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("sources-followed")

	// A linked worktree on its own branch, one commit ahead of main.
	linked := filepath.Join(f.root, "sources-followed-run")
	f.git(dir, "branch", testNamespace)
	f.git(dir, "worktree", "add", "-q", linked, testNamespace)
	f.write(linked, "a.txt", "the run's work\n")
	f.git(linked, "add", "-A")
	f.git(linked, "commit", "-qm", "the run's commit")
	runHead := f.git(linked, "rev-parse", "HEAD")
	if runHead == f.git(dir, "rev-parse", "HEAD") {
		t.Fatalf("the run worktree and the main checkout are on the same commit, so this proves nothing")
	}

	iso := f.newIsolator(dir)
	ws := f.mustCreate(iso, linked, "cand-01")

	branch, err := iso.Branch("cand-01")
	if err != nil {
		t.Fatalf("Branch() error = %v", err)
	}
	if tip := f.git(dir, "rev-parse", "refs/heads/"+branch); tip != runHead {
		t.Errorf("branch %s is at %s, want the source worktree's HEAD %s", branch, tip, runHead)
	}
	if body, _ := readFile(filepath.Join(ws.Dir, "a.txt")); string(body) != "the run's work\n" {
		t.Errorf("the candidate holds %q, want the source worktree's tree", body)
	}
	// The registration lives in the common git directory, so the main
	// checkout lists it too.
	if !slices.Contains(f.worktreePaths(dir), filepath.Join(iso.Root(), "cand-01")) {
		t.Errorf("the candidate is not registered in the common git directory: %v", f.worktreePaths(dir))
	}

	// A subdirectory source points the agent at the same subdirectory.
	sub := f.mustCreate(iso, filepath.Join(linked, "sub"), "cand-02")
	if want := filepath.Join(iso.Root(), "cand-02", "sub"); sub.Dir != want {
		t.Errorf("Workspace.Dir = %q, want %q", sub.Dir, want)
	}
	if !exists(t, filepath.Join(sub.Dir, "b.txt")) {
		t.Errorf("%q does not hold the subdirectory's file", sub.Dir)
	}
}

// TestCreateRefusesConcurrentDuplicateIDs closes the window between deciding
// a destination is free and filling it. Two Creates with one id would both
// pass every check and then race inside git, where the loser gets whichever
// lock error git happened to hit.
func TestCreateRefusesConcurrentDuplicateIDs(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("concurrent")
	gate := &gateOn{next: f.run, on: []string{"worktree", "add"}, entered: make(chan struct{}), release: make(chan struct{})}
	repo := f.openWith(gate, dir)
	iso, err := New(repo, filepath.Join(dir, ".belay", "candidates"), testNamespace, WithLogger(quiet()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var (
		wg    sync.WaitGroup
		first error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, first = iso.Create(t.Context(), dir, "cand-01")
	}()

	<-gate.entered
	_, second := iso.Create(t.Context(), dir, "cand-01")
	if !errors.Is(second, ErrInvalidID) {
		t.Errorf("the second Create error = %v, want %v", second, ErrInvalidID)
	}
	close(gate.release)
	wg.Wait()
	if first != nil {
		t.Errorf("the first Create error = %v, want it to succeed once the gate opened", first)
	}
}

// errBroken is what breaker fails a command with.
var errBroken = errors.New("the injected git failure")

// breaker is an Execer that fails one git command and runs every other for
// real, so a failure can be injected in the middle of a sequence that is
// otherwise genuine.
type breaker struct {
	next git.Execer
	on   []string
}

func (b *breaker) Run(ctx context.Context, c bexec.Command) (bexec.Result, error) {
	if isCommand(c.Args, b.on...) {
		return bexec.Result{ExitCode: -1}, errBroken
	}
	return b.next.Run(ctx, c)
}

// gateOn is an Execer that holds the first run of one git command until it is
// released, so a second call can be made while the first is inside git.
type gateOn struct {
	next    git.Execer
	on      []string
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gateOn) Run(ctx context.Context, c bexec.Command) (bexec.Result, error) {
	if isCommand(c.Args, g.on...) {
		g.once.Do(func() {
			close(g.entered)
			<-g.release
		})
	}
	return g.next.Run(ctx, c)
}

// isCommand reports whether args runs the git command named by words, such as
// "worktree add", ignoring the global options internal/git puts in front of
// every subcommand.
func isCommand(args []string, words ...string) bool {
	var plain []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			plain = append(plain, args[i])
		}
	}
	return len(plain) >= len(words) && slices.Equal(plain[:len(words)], words)
}
