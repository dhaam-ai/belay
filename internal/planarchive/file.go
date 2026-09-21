package planarchive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/state"
)

// ErrChanged reports a plan artifact whose bytes no longer fingerprint to
// the digest the run recorded. Somebody edited it after the run last read
// it, so it is not the plan the run's code was written from, and archiving
// it would file a record of something that did not happen.
var ErrChanged = errors.New("planarchive: the plan changed after the run last read it")

// Outcome is what File did with a run's plan.
type Outcome int

// What File can do with a run's plan.
const (
	// Filed means the plan is in the repository and the run's own copy is
	// gone: this call finished the move, whether it did all of it or only
	// what a crashed call had left.
	Filed Outcome = iota + 1
	// AlreadyFiled means an earlier call had finished the move, and there
	// was nothing left to do.
	AlreadyFiled
	// NotAsked means the run's manifest pins no directory: plan.archive was
	// off when the run started, or the run is older than archival. Nothing
	// was touched, and there is nothing to say.
	NotAsked
	// Kept means the plan stays in the run's artifacts, and Result.Reason
	// says why.
	Kept
)

// Result is what File did with a run's plan, and where the plan is.
type Result struct {
	// Outcome is what File did.
	Outcome Outcome
	// Rel is where the plan goes, or went: relative to the tree the run
	// worked in, slash-separated, as the manifest records it. It is empty
	// only for NotAsked.
	Rel string
	// Path is Rel as an absolute path in that tree. For a run on a branch
	// it is inside the run's worktree, which the CLI removes once the run
	// has finished, so it is only worth printing for a run in place.
	Path string
	// Source is the run's own copy, under its artifacts: where the plan
	// still is when File keeps it.
	Source string
	// Reason is, for Kept, why the plan stayed, in words a person can act
	// on.
	Reason string
}

// Approved returns the plan the run's code was written from: the plan
// artifact the blackboard names, provided it still fingerprints to the
// digest the run recorded for it (ErrChanged otherwise).
//
// The approve gate re-fingerprints the plan every time it passes, so the
// digest is of the plan as the person approved it, edits and all; with
// graph.approval off it is the plan node's, which is what the code node
// was handed instead. Either way it is the one set of bytes that describes
// the code, and it is the only set this package will archive. The artifact
// is found by approve.PlanArtifact, the gate's own rule, and fingerprinted
// by approve.Digest, so this can never disagree with the gate about which
// file is the plan or whether it changed.
//
// It is exported for the promote node, which writes a branch run's plan
// into the run's worktree before its commit and must write exactly what
// File will later look for.
func Approved(l state.Layout, p state.Plan) ([]byte, error) {
	_, abs, err := approve.PlanArtifact(l, p.Path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs) //nolint:gosec // abs is confined to the run's artifacts by approve.PlanArtifact
	if err != nil {
		return nil, fmt.Errorf("planarchive: read the plan: %w", err)
	}
	if p.Digest == "" || approve.Digest(data) != p.Digest {
		return nil, fmt.Errorf("%w: %s", ErrChanged, abs)
	}
	return data, nil
}

// fileStage is one durable step of File, in the order File takes them. The
// tests stop File after each one, the way a kill -9 would, to prove that
// every stopping point converges when File runs again.
type fileStage int

const (
	// stagePlaced: the plan is in the repository, and the manifest does not
	// know yet.
	stagePlaced fileStage = iota + 1
	// stageRecorded: the manifest records where the plan went, and the
	// run's copy is still there.
	stageRecorded
	// stageRemoved: the move is finished. File always runs to here.
	stageRemoved
)

// String names a stage for a test's output.
func (s fileStage) String() string {
	switch s {
	case stagePlaced:
		return "placed"
	case stageRecorded:
		return "recorded"
	default:
		return "removed"
	}
}

// File moves the plan of the completed run in store out of the run's
// artifacts and into the repository, at the directory the run's manifest
// pinned when the run was created, and records in the manifest where it
// went. store must be the Store the run's dispatcher used: for a run on a
// branch its Layout points at the run's worktree (state.Layout.WithWorkTree),
// which is where the promote node put the plan.
//
// # A move that never loses the plan
//
// The person asked for the plan to be moved, not copied, so the run's copy
// is removed. Removing it is the last of three durable steps, and each step
// is taken only once the one before it is on the disk:
//
//  1. Place the plan in the repository. Place writes it whole, links it into
//     place without replacing anything, and fsyncs every directory on the
//     way, so the copy survives a power cut before anything else happens.
//  2. Record its path in the manifest, which is written atomically.
//  3. Remove the run's copy, and fsync the directory it was in.
//
// At every point between two steps the plan exists, whole, in at least one
// place, and File run again finishes from wherever the last call stopped.
// Stopped after step 1, it finds the plan already in place, byte for byte
// the same, and adopts it rather than filing a second copy beside it (Place
// converges). Stopped after step 2, the pointer says the move is all but
// done, and all that is left is step 3 — which is taken only while the
// archived copy is still exactly the plan, because the run's copy would
// otherwise be the only intact one. Recording before removing is the whole
// point of the order: the other way round, a crash between the two would
// leave a plan in the repository that nothing points to.
//
// # What moves
//
// Only a completed run's plan: a failed, stopped or unfinished run's plan is
// the plan of code that does not exist, and it stays with the run. Only a
// run that pinned a directory: one started with plan.archive off, or before
// belay archived plans, was never told its plan would move. And only the
// plan the run's code was written from (see Approved): a plan somebody
// edited after the run last read it is not that plan.
//
// # A branch run
//
// A run on a branch commits once, in its promote node, and the promote node
// writes the plan into the worktree before that commit, so the plan is part
// of what the branch delivers. File, which runs after the run has
// completed, therefore never writes into a worktree: a file written there
// after the commit would be in no commit, and the CLI removes the worktree
// of a completed run, taking the file with it — moving the plan there would
// lose it. File only adopts the copy the commit already has, and keeps the
// run's copy if it finds none.
//
// # Results and errors
//
// Every reason the plan stays is a Result, Kept with a Reason, because each
// is something File understood: somebody's file where the plan would go, a
// symbolic link out of the workspace, a plan edited after the run, a run
// that did not complete. An error means File could not find out, or could
// not finish a step it had started, such as a manifest it cannot read or a
// disk that refused a write; the plan is still in at least one place.
func File(store *state.Store) (Result, error) {
	return fileUpTo(store, stageRemoved)
}

// fileUpTo is File, stopping after stop. Production code always runs it to
// stageRemoved; the tests stop it earlier to simulate a crash.
func fileUpTo(store *state.Store, stop fileStage) (Result, error) {
	man, err := store.LoadManifest()
	if err != nil {
		return Result{}, fmt.Errorf("planarchive: read the run's manifest: %w", err)
	}
	if man.ArchivedPlan.Dir == "" {
		return Result{Outcome: NotAsked}, nil
	}
	st, err := store.LoadState()
	if err != nil {
		return Result{}, fmt.Errorf("planarchive: read the run's state: %w", err)
	}

	layout := store.Layout()
	root := layout.WorkspaceDir()
	rel := man.ArchivedPlan.Path
	if rel == "" {
		rel = Rel(man.ArchivedPlan.Dir, man.RunID, man.Goal)
	}
	res := Result{Rel: rel, Path: filepath.Join(root, filepath.FromSlash(rel))}
	if _, abs, err := approve.PlanArtifact(layout, st.Plan.Path); err == nil {
		res.Source = abs
	}

	if man.Status != state.RunStatusCompleted {
		return keep(res, "the run has not finished successfully: it is %s, and only a completed run's plan moves", man.Status), nil
	}
	if man.ArchivedPlan.Path != "" {
		return finish(res)
	}

	data, err := Approved(layout, st.Plan)
	switch {
	case errors.Is(err, ErrChanged):
		return keep(res, "%s changed after the run last read it, so it is not the plan the run's code was written from", res.Source), nil
	case errors.Is(err, fs.ErrNotExist):
		return keep(res, "the run directory has no plan at %s", res.Source), nil
	case err != nil:
		return keep(res, "belay cannot use the run's plan: %v", err), nil
	}

	if man.Output.Branched() {
		reason, err := missingFromCommit(root, rel, data)
		if err != nil {
			return res, err
		}
		if reason != "" {
			return keep(res, "%s", reason), nil
		}
	} else if _, err := Place(root, rel, data); err != nil {
		switch {
		case errors.Is(err, ErrOccupied):
			return keep(res, "there is already a different file at %s, and belay never replaces one", res.Path), nil
		case errors.Is(err, ErrOutside):
			return keep(res, "the way to %s leads outside the workspace, and belay never writes there", res.Path), nil
		default:
			return res, err
		}
	}
	if stop == stagePlaced {
		return res, nil
	}

	man.ArchivedPlan.Path = rel
	if err := store.SaveManifest(man); err != nil {
		return res, fmt.Errorf("planarchive: record where the plan went: %w", err)
	}
	if stop == stageRecorded {
		return res, nil
	}
	if err := remove(res.Source); err != nil {
		return res, err
	}
	res.Outcome = Filed
	return res, nil
}

// missingFromCommit returns why a branch run's worktree does not hold the
// plan at rel, where its promote node puts it before committing, or "" when
// it does. It only reads.
func missingFromCommit(root, rel string, data []byte) (string, error) {
	_, dest, err := resolve(root, rel)
	if err != nil {
		return fmt.Sprintf("belay cannot look for the plan in the run's commit at %s: %v", rel, err), nil
	}
	held, err := holds(dest, data)
	switch {
	case errors.Is(err, ErrOccupied):
		return fmt.Sprintf("the run's commit has a different file at %s, so the plan was not put in it", rel), nil
	case err != nil:
		return "", err
	case !held:
		return fmt.Sprintf("the run's commit does not have the plan at %s", rel), nil
	}
	return "", nil
}

// finish is File for a run whose manifest already records where its plan
// went: the move is done, or all that is left of it is removing the run's
// copy, which is taken only while the archived copy is still the plan.
func finish(res Result) (Result, error) {
	if res.Source == "" {
		res.Outcome = AlreadyFiled
		return res, nil
	}
	data, err := os.ReadFile(res.Source)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		res.Outcome = AlreadyFiled
		return res, nil
	case err != nil:
		return res, fmt.Errorf("planarchive: read the run's copy of the plan: %w", err)
	}
	if held, err := holds(res.Path, data); err != nil || !held {
		return keep(res, "the manifest says the plan went to %s, but that file is not the plan any more, so the run's copy stays", res.Path), nil
	}
	if err := remove(res.Source); err != nil {
		return res, err
	}
	res.Outcome = Filed
	return res, nil
}

// keep is a Kept result saying why.
func keep(res Result, format string, a ...any) Result {
	res.Outcome = Kept
	res.Reason = fmt.Sprintf(format, a...)
	return res
}

// remove removes the run's copy of the plan and fsyncs the directory it was
// in, so the removal is as durable as the copy that replaced it.
func remove(source string) error {
	if err := os.Remove(source); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("planarchive: remove the run's copy of the plan: %w", err)
	}
	if err := syncDir(filepath.Dir(source)); err != nil {
		return fmt.Errorf("planarchive: sync %s: %w", filepath.Dir(source), err)
	}
	return nil
}
