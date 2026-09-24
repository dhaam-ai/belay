//go:build unix

package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/dhaam-ai/belay/internal/git"
)

// candidateReclaimer is the optional interface internal/graph's candidate
// runner looks for on a run's isolator, restated here so that a change to
// either signature stops compiling.
//
// It is declared by the consumer, not by pkg/belay: Reclaim takes an id
// and not a Workspace, because after a Create that died part-way there is
// no Workspace to pass. See Isolator.Reclaim.
type candidateReclaimer interface {
	Reclaim(ctx context.Context, id string) error
}

var _ candidateReclaimer = (*Isolator)(nil)

// TestReclaimMakesAnAbandonedIDCreatableAgain is what the method exists
// for, over every shape a Create that died part-way leaves behind. Each
// row asserts the same two things: Create refuses the id first, and after
// Reclaim it succeeds.
func TestReclaimMakesAnAbandonedIDCreatableAgain(t *testing.T) {
	tests := []struct {
		name string
		// leave puts the debris there and returns the sentinel Create
		// must refuse it with.
		leave func(t *testing.T, f *fixture, iso *Isolator, repo, dir, branch string) error
	}{
		{
			name: "a directory holding a part-copied tree and no registration",
			leave: func(t *testing.T, f *fixture, _ *Isolator, _, dir, _ string) error {
				t.Helper()
				f.write(dir, "a.txt", "half a checkout\n")
				return ErrNotEmpty
			},
		},
		{
			name: "a branch created before the worktree add failed",
			leave: func(t *testing.T, f *fixture, _ *Isolator, repo, _, branch string) error {
				t.Helper()
				f.git(repo, "branch", branch)
				return git.ErrBranchExists
			},
		},
		{
			name: "a whole worktree the run that owned it never released",
			leave: func(t *testing.T, f *fixture, iso *Isolator, repo, _, _ string) error {
				t.Helper()
				f.mustCreate(iso, repo, "cand-01")
				return ErrStaleWorktree
			},
		},
		{
			name: "a registration whose directory someone deleted by hand",
			leave: func(t *testing.T, f *fixture, iso *Isolator, repo, dir, _ string) error {
				t.Helper()
				f.mustCreate(iso, repo, "cand-01")
				if err := os.RemoveAll(dir); err != nil {
					t.Fatalf("RemoveAll(%q) error = %v", dir, err)
				}
				return ErrStaleWorktree
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			repo := f.repo("abandoned" + strconv.Itoa(i))
			iso := f.newIsolator(repo)
			branch, err := iso.Branch("cand-01")
			if err != nil {
				t.Fatalf("Branch() error = %v", err)
			}
			dir := filepath.Join(iso.Root(), "cand-01")

			wantRefusal := tt.leave(t, f, iso, repo, dir, branch)
			if _, err := iso.Create(t.Context(), repo, "cand-01"); !errors.Is(err, wantRefusal) {
				t.Fatalf("Create over the debris error = %v, want %v: the premise of this row does not hold", err, wantRefusal)
			}

			if err := iso.Reclaim(t.Context(), "cand-01"); err != nil {
				t.Fatalf("Reclaim() error = %v", err)
			}
			if exists(t, dir) {
				t.Errorf("%q survived Reclaim", dir)
			}
			if slices.Contains(f.worktreePaths(repo), dir) {
				t.Errorf("a registration for %q survived Reclaim: %v", dir, f.worktreePaths(repo))
			}
			if slices.Contains(f.branches(repo), branch) {
				t.Errorf("branch %s survived Reclaim, so the id is still not creatable: %v", branch, f.branches(repo))
			}

			// The whole point: the id works again.
			ws := f.mustCreate(iso, repo, "cand-01")
			if !exists(t, filepath.Join(ws.Dir, "a.txt")) {
				t.Errorf("the recreated workspace is not a checkout of the source: %q has no a.txt", ws.Dir)
			}
		})
	}
}

// TestReclaimKeepsABranchThatHoldsWork holds Reclaim to the same rule
// Destroy follows. A candidate that committed for itself and then lost
// its run has commits no other ref holds, and those commits are work:
// this package will not be the thing that throws them away. The branch
// stays and the warning names the command that would delete it.
func TestReclaimKeepsABranchThatHoldsWork(t *testing.T) {
	f := newFixture(t)
	repo := f.repo("keepwork")
	logs := &capture{}
	iso := f.newIsolator(repo, WithLogger(logs.logger()))

	ws := f.mustCreate(iso, repo, "cand-01")
	f.write(ws.Dir, "work.txt", "the candidate's work\n")
	f.git(ws.Dir, "add", "-A")
	f.git(ws.Dir, "commit", "-qm", "the candidate's commit")
	tip := f.git(ws.Dir, "rev-parse", "HEAD")

	branch, err := iso.Branch("cand-01")
	if err != nil {
		t.Fatalf("Branch() error = %v", err)
	}

	if err := iso.Reclaim(t.Context(), "cand-01"); err != nil {
		t.Fatalf("Reclaim() error = %v", err)
	}
	if exists(t, ws.Dir) {
		t.Errorf("%q survived Reclaim; the worktree goes either way", ws.Dir)
	}
	if !slices.Contains(f.branches(repo), branch) {
		t.Fatalf("branch %s was deleted although it holds commits nothing else does: %v", branch, f.branches(repo))
	}
	if now := f.git(repo, "rev-parse", "refs/heads/"+branch); now != tip {
		t.Errorf("the kept branch moved to %s, want %s", now, tip)
	}
	if warn := logs.find("branch -D " + branch); warn == "" {
		t.Errorf("no warning names the command that would delete %s; logged: %v", branch, logs.messages())
	}
}

// TestReclaimConvergesOnAnIDWithNothingUnderIt covers the two ways a
// caller reaches Reclaim with nothing to do. Both succeed: the dispatcher
// re-runs a node whose start has no finish, so a reclaim runs again after
// a crash and the second run has to converge.
func TestReclaimConvergesOnAnIDWithNothingUnderIt(t *testing.T) {
	f := newFixture(t)
	repo := f.repo("converge")
	iso := f.newIsolator(repo)

	if err := iso.Reclaim(t.Context(), "cand-99"); err != nil {
		t.Errorf("Reclaim of an id nothing was ever created for error = %v, want nil", err)
	}

	ws := f.mustCreate(iso, repo, "cand-01")
	for i := range 2 {
		if err := iso.Reclaim(t.Context(), "cand-01"); err != nil {
			t.Fatalf("Reclaim call %d error = %v, want nil", i+1, err)
		}
		if exists(t, ws.Dir) {
			t.Errorf("after Reclaim call %d, %q still exists", i+1, ws.Dir)
		}
	}

	// And after Destroy, which is the other order the two can arrive in.
	again := f.mustCreate(iso, repo, "cand-02")
	if err := iso.Destroy(t.Context(), again); err != nil {
		t.Fatalf("Destroy() error = %v", err)
	}
	if err := iso.Reclaim(t.Context(), "cand-02"); err != nil {
		t.Errorf("Reclaim after Destroy error = %v, want nil", err)
	}
}

// TestReclaimLeavesTheUsersOwnStaleEntriesAlone is the reason Reclaim
// removes one registration by path rather than running `git worktree
// prune`: a worktree on a disk that is not mounted right now looks
// exactly like debris, and it is not belay's.
func TestReclaimLeavesTheUsersOwnStaleEntriesAlone(t *testing.T) {
	f := newFixture(t)
	repo := f.repo("bystander")
	iso := f.newIsolator(repo)

	usersOwn := filepath.Join(f.root, "the-users-own-worktree")
	f.git(repo, "branch", "the-users-own-branch")
	f.git(repo, "worktree", "add", "-q", usersOwn, "the-users-own-branch")
	if err := os.RemoveAll(usersOwn); err != nil {
		t.Fatalf("RemoveAll(%q) error = %v", usersOwn, err)
	}

	f.mustCreate(iso, repo, "cand-01")
	if err := iso.Reclaim(t.Context(), "cand-01"); err != nil {
		t.Fatalf("Reclaim() error = %v", err)
	}

	if !slices.Contains(f.worktreePaths(repo), usersOwn) {
		t.Errorf("the user's own stale registration for %q was dropped: %v", usersOwn, f.worktreePaths(repo))
	}
	if !slices.Contains(f.branches(repo), "the-users-own-branch") {
		t.Errorf("the user's own branch was deleted: %v", f.branches(repo))
	}
}

// TestReclaimRefusesWhatItDoesNotOwn is the containment proof. Reclaim
// takes an id and then deletes a directory and possibly a branch, so
// everything it will not act on must be refused with nothing touched.
func TestReclaimRefusesWhatItDoesNotOwn(t *testing.T) {
	tests := []struct {
		name string
		// setUp returns the id to reclaim and the sentinel the refusal
		// must wrap.
		setUp func(t *testing.T, f *fixture, iso *Isolator, repo string) (string, error)
		// survivor, when set, must still be there afterwards.
		survivor func(f *fixture, iso *Isolator) string
	}{
		{
			name: "an id that climbs out of the candidate root",
			setUp: func(_ *testing.T, _ *fixture, _ *Isolator, _ string) (string, error) {
				return "../escaped", ErrInvalidID
			},
		},
		{
			name: "an id that is a path",
			setUp: func(_ *testing.T, _ *fixture, _ *Isolator, _ string) (string, error) {
				return "a/b", ErrInvalidID
			},
		},
		{
			name: "an empty id",
			setUp: func(_ *testing.T, _ *fixture, _ *Isolator, _ string) (string, error) {
				return "", ErrInvalidID
			},
		},
		{
			name: "an id git would refuse as a ref component",
			setUp: func(_ *testing.T, _ *fixture, _ *Isolator, _ string) (string, error) {
				return "cand 01..x", ErrInvalidID
			},
		},
		{
			name: "a worktree someone checked another branch out in",
			setUp: func(_ *testing.T, f *fixture, iso *Isolator, repo string) (string, error) {
				ws := f.mustCreate(iso, repo, "cand-01")
				f.git(repo, "branch", "someone-elses-branch")
				f.git(ws.Dir, "checkout", "-q", "someone-elses-branch")
				return "cand-01", ErrRefused
			},
			survivor: func(_ *fixture, iso *Isolator) string { return filepath.Join(iso.Root(), "cand-01") },
		},
		{
			name: "a worktree a person locked",
			setUp: func(_ *testing.T, f *fixture, iso *Isolator, repo string) (string, error) {
				ws := f.mustCreate(iso, repo, "cand-01")
				f.git(repo, "worktree", "lock", "--reason", "on a disk that is not mounted", ws.Dir)
				return "cand-01", git.ErrLocked
			},
			survivor: func(_ *fixture, iso *Isolator) string { return filepath.Join(iso.Root(), "cand-01") },
		},
		{
			name: "a candidate path that is a symlink out of the root",
			setUp: func(t *testing.T, f *fixture, iso *Isolator, _ string) (string, error) {
				t.Helper()
				victim := f.dir("outside")
				f.write(victim, "keep.txt", "someone's work\n")
				if err := os.MkdirAll(iso.Root(), 0o750); err != nil {
					t.Fatalf("MkdirAll(%q) error = %v", iso.Root(), err)
				}
				if err := os.Symlink(victim, filepath.Join(iso.Root(), "cand-01")); err != nil {
					t.Fatalf("Symlink error = %v", err)
				}
				return "cand-01", ErrRefused
			},
			survivor: func(f *fixture, _ *Isolator) string { return filepath.Join(f.root, "outside", "keep.txt") },
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			repo := f.repo("refuse" + strconv.Itoa(i))
			iso := f.newIsolator(repo)

			// A bystander one level above the candidate root, which no
			// refusal may take with it.
			bystander := f.dir("bystander" + strconv.Itoa(i))
			f.write(bystander, "keep.txt", "someone's work\n")

			id, want := tt.setUp(t, f, iso, repo)
			err := iso.Reclaim(t.Context(), id)
			if !errors.Is(err, want) {
				t.Fatalf("Reclaim(%q) error = %v, want %v", id, err, want)
			}
			if !exists(t, filepath.Join(bystander, "keep.txt")) {
				t.Errorf("a refused Reclaim deleted the bystander %q", bystander)
			}
			if tt.survivor == nil {
				return
			}
			if path := filepath.Clean(tt.survivor(f, iso)); !exists(t, path) {
				t.Errorf("a refused Reclaim deleted %q", path)
			}
		})
	}
}

// TestReclaimRespectsACanceledContext keeps Reclaim to the same rule as
// Create and Destroy: a cancelled context stops it before it removes
// anything.
func TestReclaimRespectsACanceledContext(t *testing.T) {
	f := newFixture(t)
	repo := f.repo("cancelled")
	iso := f.newIsolator(repo)
	ws := f.mustCreate(iso, repo, "cand-01")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := iso.Reclaim(ctx, "cand-01"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reclaim() error = %v, want context.Canceled", err)
	}
	if !exists(t, ws.Dir) {
		t.Errorf("a cancelled Reclaim removed %q", ws.Dir)
	}
}
