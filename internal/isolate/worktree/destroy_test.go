//go:build unix

package worktree

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/git"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestDestroyReleasesEverythingCreateAllocated is the positive half: the
// directory, the registration git keeps in the common git directory, and the
// branch all go, and nothing else does.
func TestDestroyReleasesEverythingCreateAllocated(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("destroy")
	iso := f.newIsolator(dir)
	before := f.branches(dir)

	first := f.mustCreate(iso, dir, "cand-01")
	second := f.mustCreate(iso, dir, "cand-02")

	if err := iso.Destroy(t.Context(), first); err != nil {
		t.Fatalf("Destroy() error = %v", err)
	}
	if exists(t, first.Dir) {
		t.Errorf("%q is still there after Destroy", first.Dir)
	}
	if slices.Contains(f.worktreePaths(dir), first.Dir) {
		t.Errorf("git still lists %q as a worktree: %v", first.Dir, f.worktreePaths(dir))
	}
	branch, err := iso.Branch("cand-01")
	if err != nil {
		t.Fatalf("Branch() error = %v", err)
	}
	if slices.Contains(f.branches(dir), branch) {
		t.Errorf("branch %s survived Destroy: %v", branch, f.branches(dir))
	}

	// The sibling candidate and the user's own branches are untouched.
	if !exists(t, second.Dir) {
		t.Errorf("the sibling candidate %q was removed too", second.Dir)
	}
	for _, b := range before {
		if !slices.Contains(f.branches(dir), b) {
			t.Errorf("branch %s disappeared: %v", b, f.branches(dir))
		}
	}
	if !exists(t, iso.Root()) {
		t.Error("the candidate root was removed")
	}
}

// TestDestroyWorksOnAWorkspaceDecodedAfterRestart is the constraint that
// shapes this package. belay.Workspace is plain JSON in the run journal, so
// after a crash the only things Destroy has are the fields that survived that
// round trip and the values the Isolator was constructed with. Anything Create
// kept in memory — a map from id to branch, the admin entry git named after
// the directory's basename — is gone, and git numbers colliding admin entries,
// so they cannot be guessed either.
func TestDestroyWorksOnAWorkspaceDecodedAfterRestart(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("restart")
	root := filepath.Join(dir, ".belay", "candidates")

	first, err := New(f.open(dir), root, testNamespace, WithLogger(quiet()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ws := f.mustCreate(first, dir, "cand-01")

	encoded, err := json.Marshal(ws)
	if err != nil {
		t.Fatalf("Marshal(%+v) error = %v", ws, err)
	}
	var decoded belay.Workspace
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", encoded, err)
	}

	// A new process: a new Repo over the same directory, a new Execer, and
	// nothing at all from the Isolator that created the workspace.
	second, err := New(f.open(dir), root, testNamespace, WithLogger(quiet()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := second.Destroy(t.Context(), decoded); err != nil {
		t.Fatalf("Destroy(decoded) error = %v", err)
	}

	if exists(t, ws.Dir) {
		t.Errorf("%q is still there after a Destroy from a fresh Isolator", ws.Dir)
	}
	if slices.Contains(f.worktreePaths(dir), ws.Dir) {
		t.Errorf("git still lists %q as a worktree: %v", ws.Dir, f.worktreePaths(dir))
	}
	branch, err := second.Branch("cand-01")
	if err != nil {
		t.Fatalf("Branch() error = %v", err)
	}
	if slices.Contains(f.branches(dir), branch) {
		t.Errorf("branch %s survived: %v", branch, f.branches(dir))
	}
}

// TestDestroyRefusesHostileWorkspaces is the test this package exists for.
//
// Every row hands Destroy a Workspace naming something it must not touch, and
// asserts that the call is refused AND that the thing it would have removed is
// still there. The second assertion is the real one: an error return means
// nothing if the branch is already deleted.
//
// The rows past the path-handling ones are this package's own: a Workspace
// whose Dir does not match what its ID derives to, a worktree a person locked,
// and a worktree someone moved to another branch. Each is a way a decoded
// Workspace could steer a `branch -D` or an `rm -rf` somewhere it was never
// meant to go.
func TestDestroyRefusesHostileWorkspaces(t *testing.T) {
	tests := []struct {
		name string
		// build returns the hostile Workspace, the path that must survive,
		// and the sentinel the refusal must wrap.
		build func(t *testing.T, f *fixture, iso *Isolator, dir, victim, candidate string) (belay.Workspace, string, error)
	}{
		{
			name: "parent traversal out of the candidate root",
			build: func(t *testing.T, _ *fixture, iso *Isolator, _, victim, _ string) (belay.Workspace, string, error) {
				escape := filepath.Join(iso.Root(), "..", "..", "..", filepath.Base(victim))
				if escape != victim {
					t.Fatalf("the traversal lands on %q, not on the bystander %q, so the row proves nothing", escape, victim)
				}
				return belay.Workspace{ID: "cand-01", Dir: escape, Ephemeral: true}, victim, ErrRefused
			},
		},
		{
			name: "an absolute path outside the candidate root",
			build: func(_ *testing.T, _ *fixture, _ *Isolator, _, victim, _ string) (belay.Workspace, string, error) {
				return belay.Workspace{ID: "cand-01", Dir: victim, Ephemeral: true}, victim, ErrRefused
			},
		},
		{
			name: "a sibling directory whose name shares the root's prefix",
			build: func(_ *testing.T, f *fixture, _ *Isolator, _, _, _ string) (belay.Workspace, string, error) {
				sibling := f.dir("candidates-attacker")
				f.write(sibling, "keep.txt", "keep")
				return belay.Workspace{ID: "cand-01", Dir: sibling, Ephemeral: true}, sibling, ErrRefused
			},
		},
		{
			name: "an empty Dir",
			build: func(_ *testing.T, _ *fixture, _ *Isolator, _, _, candidate string) (belay.Workspace, string, error) {
				return belay.Workspace{ID: "cand-01", Dir: "", Ephemeral: true}, candidate, ErrRefused
			},
		},
		{
			name: "a relative Dir",
			build: func(_ *testing.T, _ *fixture, _ *Isolator, _, _, candidate string) (belay.Workspace, string, error) {
				return belay.Workspace{ID: "cand-01", Dir: ".belay/candidates/cand-01", Ephemeral: true}, candidate, ErrRefused
			},
		},
		{
			name: "a workspace this Isolator never produced, marked non-ephemeral",
			build: func(_ *testing.T, _ *fixture, _ *Isolator, _, _, candidate string) (belay.Workspace, string, error) {
				// belay.Workspace documents Ephemeral false as "this is the
				// user's own tree". That is the one directory this package
				// must never remove, even though the path is its own.
				return belay.Workspace{ID: "cand-01", Dir: candidate, Ephemeral: false}, candidate, ErrRefused
			},
		},
		{
			name: "an ID and a Dir that name different candidates",
			build: func(_ *testing.T, f *fixture, iso *Isolator, dir, _, _ string) (belay.Workspace, string, error) {
				other := f.mustCreate(iso, dir, "cand-02")
				// The branch belongs to cand-01, the directory to cand-02;
				// acting on either would release something the other owns.
				return belay.Workspace{ID: "cand-01", Dir: other.Dir, Ephemeral: true}, other.Dir, ErrRefused
			},
		},
		{
			name: "an ID that is not a candidate id at all",
			build: func(_ *testing.T, _ *fixture, _ *Isolator, _, _, candidate string) (belay.Workspace, string, error) {
				return belay.Workspace{ID: "../../main", Dir: candidate, Ephemeral: true}, candidate, ErrInvalidID
			},
		},
		{
			name: "a worktree a person locked",
			build: func(_ *testing.T, f *fixture, _ *Isolator, dir, _, candidate string) (belay.Workspace, string, error) {
				f.git(dir, "worktree", "lock", "--reason", "on a disk that is not mounted", candidate)
				return belay.Workspace{ID: "cand-01", Dir: candidate, Ephemeral: true}, candidate, git.ErrLocked
			},
		},
		{
			name: "a worktree someone moved to another branch",
			build: func(_ *testing.T, f *fixture, _ *Isolator, dir, _, candidate string) (belay.Workspace, string, error) {
				f.git(dir, "branch", "someone-elses-work")
				f.git(candidate, "checkout", "-q", "someone-elses-work")
				return belay.Workspace{ID: "cand-01", Dir: candidate, Ephemeral: true}, candidate, ErrRefused
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			dir := f.repo("hostile" + strconv.Itoa(i))
			iso := f.newIsolator(dir)

			// A real bystander tree outside the root and a real candidate
			// inside it, so every refusal has something concrete to protect.
			victim := f.dir("precious")
			victimFile := f.write(victim, "thesis.txt", "years of work")
			candidate := f.mustCreate(iso, dir, "cand-01").Dir

			ws, mustSurvive, want := tt.build(t, f, iso, dir, victim, candidate)
			// The baseline is taken after the row has set its trap, so what
			// is asserted is that the refused Destroy changed nothing, not
			// that the trap changed nothing.
			branches, worktrees := f.branches(dir), f.worktreePaths(dir)

			err := iso.Destroy(t.Context(), ws)
			if err == nil {
				t.Fatalf("Destroy(%+v) returned nil; it must refuse", ws)
			}
			if !errors.Is(err, want) {
				t.Errorf("Destroy error = %v, want it to wrap %v", err, want)
			}
			if !exists(t, mustSurvive) {
				t.Errorf("%q was removed by a refused Destroy", mustSurvive)
			}
			if !exists(t, victim) || !exists(t, victimFile) {
				t.Error("the bystander tree was touched")
			}
			if got := f.branches(dir); !slices.Equal(got, branches) {
				t.Errorf("branches after the refusal = %v, want %v", got, branches)
			}
			if got := f.worktreePaths(dir); !slices.Equal(got, worktrees) {
				t.Errorf("worktrees after the refusal = %v, want %v", got, worktrees)
			}
		})
	}
}

// TestDestroyKeepsABranchThatHoldsWork is the rule that makes this isolator
// safe to run on someone's repository: unique commits are work, not an
// allocation. Destroy releases the worktree either way, and deletes the branch
// only when some other ref already holds its tip.
//
// `git branch -d` is not the check. It judges merge status against the HEAD of
// the checkout it runs in, which is the user's own branch, not the run's — so
// a candidate merged into the run branch would still look unmerged and pile up.
func TestDestroyKeepsABranchThatHoldsWork(t *testing.T) {
	tests := []struct {
		name string
		// merge says whether the candidate's commit reaches the run branch.
		merge bool
		// wantBranch says whether the branch must still exist afterwards.
		wantBranch bool
	}{
		{name: "a commit no other ref holds is kept", wantBranch: true},
		{name: "a commit the run branch already holds is deleted", merge: true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			dir := f.repo("keepwork" + strconv.Itoa(i))
			logs := &capture{}
			iso := f.newIsolator(dir, WithLogger(logs.logger()))

			// The run's own branch and worktree, the way a real run has them.
			runTree := filepath.Join(f.root, "keepwork-run"+strconv.Itoa(i))
			f.git(dir, "branch", testNamespace)
			f.git(dir, "worktree", "add", "-q", runTree, testNamespace)

			ws := f.mustCreate(iso, runTree, "cand-01")
			f.write(ws.Dir, "work.txt", "the candidate's work\n")
			f.git(ws.Dir, "add", "-A")
			f.git(ws.Dir, "commit", "-qm", "the candidate's commit")
			tip := f.git(ws.Dir, "rev-parse", "HEAD")

			branch, err := iso.Branch("cand-01")
			if err != nil {
				t.Fatalf("Branch() error = %v", err)
			}
			if tt.merge {
				f.git(runTree, "merge", "-q", "--no-ff", "-m", "belay: promote the winner", branch)
			}

			if err := iso.Destroy(t.Context(), ws); err != nil {
				t.Fatalf("Destroy() error = %v", err)
			}
			if exists(t, ws.Dir) {
				t.Errorf("%q survived Destroy; the worktree goes either way", ws.Dir)
			}
			if got := slices.Contains(f.branches(dir), branch); got != tt.wantBranch {
				t.Errorf("branch %s exists = %v, want %v (branches %v)", branch, got, tt.wantBranch, f.branches(dir))
			}
			if !tt.wantBranch {
				return
			}
			if now := f.git(dir, "rev-parse", "refs/heads/"+branch); now != tip {
				t.Errorf("the kept branch moved to %s, want %s", now, tip)
			}
			// The commits are only recoverable if the run says where they are.
			warn := logs.find("branch -D " + branch)
			if warn == "" {
				t.Errorf("no warning names the command that would delete %s; logged: %v", branch, logs.messages())
			}
		})
	}
}

// TestDestroyConvergesOnRepeatedRuns covers the two shapes a re-run meets.
// promote destroys a winner and its losers, and a crash between that and the
// journal record means the dispatcher runs promote again, so a second Destroy
// is the normal case rather than an error. A directory someone deleted by hand
// leaves git's registration behind, and that has to go too — but only that
// one: `git worktree prune` would also drop the user's own stale entries, such
// as a worktree on a disk that is not mounted right now.
func TestDestroyConvergesOnRepeatedRuns(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("converge")
	iso := f.newIsolator(dir)

	// A stale entry of the user's own, which must survive everything below.
	usersOwn := filepath.Join(f.root, "the-users-own-worktree")
	f.git(dir, "branch", "the-users-own-branch")
	f.git(dir, "worktree", "add", "-q", usersOwn, "the-users-own-branch")
	if err := os.RemoveAll(usersOwn); err != nil {
		t.Fatalf("RemoveAll(%q) error = %v", usersOwn, err)
	}

	ws := f.mustCreate(iso, dir, "cand-01")
	if err := iso.Destroy(t.Context(), ws); err != nil {
		t.Fatalf("first Destroy error = %v", err)
	}
	if err := iso.Destroy(t.Context(), ws); err != nil {
		t.Errorf("second Destroy error = %v, want nil: promote's re-run cleans up again", err)
	}

	// A candidate whose directory was deleted by hand still has a
	// registration, and Destroy has to remove it.
	stale := f.mustCreate(iso, dir, "cand-02")
	if err := os.RemoveAll(stale.Dir); err != nil {
		t.Fatalf("RemoveAll(%q) error = %v", stale.Dir, err)
	}
	if err := iso.Destroy(t.Context(), stale); err != nil {
		t.Fatalf("Destroy on a stale registration error = %v", err)
	}
	if slices.Contains(f.worktreePaths(dir), stale.Dir) {
		t.Errorf("the stale registration for %q survived: %v", stale.Dir, f.worktreePaths(dir))
	}

	if !slices.Contains(f.worktreePaths(dir), usersOwn) {
		t.Errorf("the user's own stale worktree was pruned: %v", f.worktreePaths(dir))
	}
	if !slices.Contains(f.branches(dir), "the-users-own-branch") {
		t.Errorf("the user's own branch was deleted: %v", f.branches(dir))
	}
	if strings.Contains(strings.Join(f.branches(dir), " "), testNamespace+"-cand-") {
		t.Errorf("a candidate branch survived: %v", f.branches(dir))
	}
}
