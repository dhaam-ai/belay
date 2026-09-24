//go:build unix

package dircopy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dhaam-ai/belay/pkg/belay"
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

// TestReclaimRemovesDebrisACreateLeftBehind is the behaviour the whole
// method exists for: Create refuses a destination that already holds
// entries, so a candidate whose Create died part-way can never be
// recreated, and the run that owned it is gone. Reclaim takes it back.
func TestReclaimRemovesDebrisACreateLeftBehind(t *testing.T) {
	iso := newIsolator(t)
	src := sampleTree(t)

	// What a process killed mid-copy leaves: the destination directory
	// with some of the tree in it and no record anywhere that it is
	// half-done.
	debris := filepath.Join(iso.Root(), "cand-01")
	writeFile(t, filepath.Join(debris, "main.go"), "package main\n", 0o644)
	writeFile(t, filepath.Join(debris, "pkg", "lib.go"), "package pkg\n", 0o644)

	if _, err := iso.Create(context.Background(), src, "cand-01"); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("Create over debris error = %v, want ErrNotEmpty: the premise of this test does not hold", err)
	}

	if err := iso.Reclaim(context.Background(), "cand-01"); err != nil {
		t.Fatalf("Reclaim(%q) error = %v", "cand-01", err)
	}
	requireGone(t, debris, "Reclaim must remove the abandoned workspace")

	// And the point of removing it: the id is usable again.
	dir := mustCreate(t, iso, src, "cand-01")
	if _, err := os.Lstat(filepath.Join(dir, "go.mod")); err != nil {
		t.Errorf("the recreated workspace is not a full copy: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "pkg", "lib.go")); err != nil {
		t.Errorf("the recreated workspace is missing pkg/lib.go: %v", err)
	}
}

// TestReclaimConvergesOnAWorkspaceThatIsAlreadyGone covers the two ways a
// caller reaches Reclaim with nothing to do. Both must succeed: the
// dispatcher re-runs a node whose start has no finish, so a reclaim runs
// again after a crash and the second run has to converge, and Destroy has
// the same rule for the same reason.
func TestReclaimConvergesOnAWorkspaceThatIsAlreadyGone(t *testing.T) {
	iso := newIsolator(t)
	src := sampleTree(t)

	t.Run("an id nothing was ever created for", func(t *testing.T) {
		if err := iso.Reclaim(context.Background(), "cand-99"); err != nil {
			t.Errorf("Reclaim of an unused id error = %v, want nil", err)
		}
	})

	t.Run("an id Destroy already released", func(t *testing.T) {
		dir := mustCreate(t, iso, src, "cand-02")
		if err := iso.Destroy(context.Background(), belay.Workspace{ID: "cand-02", Dir: dir, Ephemeral: true}); err != nil {
			t.Fatalf("Destroy() error = %v", err)
		}
		if err := iso.Reclaim(context.Background(), "cand-02"); err != nil {
			t.Errorf("Reclaim after Destroy error = %v, want nil", err)
		}
	})

	t.Run("twice in a row", func(t *testing.T) {
		dir := mustCreate(t, iso, src, "cand-03")
		for i := range 2 {
			if err := iso.Reclaim(context.Background(), "cand-03"); err != nil {
				t.Fatalf("Reclaim call %d error = %v, want nil", i+1, err)
			}
			requireGone(t, dir, "Reclaim must leave the workspace removed")
		}
	})
}

// TestReclaimForgetsTheStatsOfWhatItRemoved proves Reclaim leaves the
// Isolator with no memory of a workspace that is no longer there, the
// same as Destroy. A stale Stats entry would report a copy that does not
// exist as this candidate's cost.
func TestReclaimForgetsTheStatsOfWhatItRemoved(t *testing.T) {
	iso := newIsolator(t)
	mustCreate(t, iso, sampleTree(t), "cand-01")

	if _, ok := iso.Stats("cand-01"); !ok {
		t.Fatal("Create recorded no Stats, so this test proves nothing")
	}
	if err := iso.Reclaim(context.Background(), "cand-01"); err != nil {
		t.Fatalf("Reclaim() error = %v", err)
	}
	if _, ok := iso.Stats("cand-01"); ok {
		t.Error("Stats still reports the reclaimed workspace")
	}
}

// TestReclaimRefusesHostileIDs is the containment proof. Reclaim takes an
// id and deletes a tree, so an id that could name anything but a direct
// child of the candidate root must be refused with nothing touched.
func TestReclaimRefusesHostileIDs(t *testing.T) {
	iso := newIsolator(t)

	// A bystander directory one level above the root, which every
	// traversal below aims at.
	victim := mkdirAll(t, filepath.Join(filepath.Dir(iso.Root()), "escaped"))
	writeFile(t, filepath.Join(victim, "keep.txt"), "someone's work", 0o644)

	for _, id := range []string{"../escaped", "..", ".", "/escaped", "a/b", `a\b`, "", "   ", "cand..1", "cand\x001"} {
		t.Run(id, func(t *testing.T) {
			err := iso.Reclaim(context.Background(), id)
			if !errors.Is(err, ErrInvalidID) {
				t.Fatalf("Reclaim(%q) error = %v, want ErrInvalidID", id, err)
			}
			var ide *IDError
			if !errors.As(err, &ide) {
				t.Errorf("error is not an *IDError: %v", err)
			}
			requireExists(t, victim, "a refused Reclaim must not delete anything")
			requireExists(t, filepath.Join(victim, "keep.txt"), "a refused Reclaim must not delete anything")
		})
	}
}

// TestReclaimRefusesACandidatePathThatIsASymlink covers the case a
// lexical id check cannot see: a valid id whose path in the root is a
// symlink to somewhere else. Following it would delete the target.
func TestReclaimRefusesACandidatePathThatIsASymlink(t *testing.T) {
	iso := newIsolator(t)

	victim := mkdirAll(t, filepath.Join(filepath.Dir(iso.Root()), "outside"))
	writeFile(t, filepath.Join(victim, "keep.txt"), "someone's work", 0o644)
	link := symlink(t, victim, filepath.Join(iso.Root(), "cand-01"))

	err := iso.Reclaim(context.Background(), "cand-01")
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Reclaim() error = %v, want ErrRefused", err)
	}
	var ce *ContainmentError
	if !errors.As(err, &ce) {
		t.Errorf("error is not a *ContainmentError: %v", err)
	}
	requireExists(t, victim, "the symlink target must survive")
	requireExists(t, filepath.Join(victim, "keep.txt"), "the symlink target's contents must survive")
	requireExists(t, link, "the symlink itself is left for a person to look at, not silently unlinked")
}

// TestReclaimRespectsACanceledContext keeps Reclaim to the same rule as
// Create and Destroy: a cancelled context stops it before it removes
// anything.
func TestReclaimRespectsACanceledContext(t *testing.T) {
	iso := newIsolator(t)
	dir := mustCreate(t, iso, sampleTree(t), "cand-01")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := iso.Reclaim(ctx, "cand-01"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reclaim() error = %v, want context.Canceled", err)
	}
	requireExists(t, dir, "a cancelled Reclaim must not have removed anything")
}
