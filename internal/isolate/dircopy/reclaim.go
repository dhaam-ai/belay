//go:build unix

package dircopy

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// Reclaim removes whatever is left under id, so that id can be created
// again.
//
// # Why this is not Destroy
//
// Destroy takes a belay.Workspace, and its contract is "at most once per
// successfully CREATED Workspace". Neither half fits here. A candidate
// abandoned by a crash has no Workspace: the process died inside Create,
// which by contract returns the zero Workspace on failure and therefore
// returned nothing to record. And the one thing left on disk is exactly
// what Create refuses to start over — prepareDest rejects a destination
// holding entries, because copying into a half-copied tree blends two
// sources and hands the mixture to an agent as a clean checkout of one of
// them. Without Reclaim that id is unusable forever and the directory
// leaks; the run that owned it is gone and will not be cleaning up.
//
// So Reclaim is keyed by id, which is all a later run has. It derives the
// path the same way Create does, from the candidate root and the id, and
// it goes through the same remove — and therefore the same containment
// check — that Destroy does. An id naming anything but a direct child of
// the candidate root is an *IDError wrapping ErrInvalidID; a path that is
// a symlink, or that resolves outside the root, is a *ContainmentError
// wrapping ErrRefused. Both mean nothing was deleted.
//
// # Where it sits
//
// Reclaim is deliberately NOT part of belay.Isolator. ADR-0005 warns
// against widening a contract to fit one implementation, and this is a
// recovery step only a caller that keeps candidate ids across process
// lifetimes has any use for. It is an optional interface the consumer
// declares and type-asserts for; an isolator without one simply cannot be
// reclaimed, and its caller reports the directory for a person to remove.
//
// # Convergence
//
// Reclaiming an id with nothing under it is not an error, and neither is
// reclaiming the same id twice. That is a requirement rather than a
// nicety, and the same one Destroy carries: the dispatcher re-runs a node
// whose start has no finish, so a reclaim runs again after a crash and
// the second run has to converge.
//
// Reclaim is silent on success, as Destroy is; the refusal path logs the
// warning that names the path it would not touch.
func (i *Isolator) Reclaim(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("dircopy: reclaim %q: %w", id, err)
	}
	if err := validateID(id); err != nil {
		return err
	}

	// The same reservation Create takes, for the same reason: a reclaim
	// must not delete a directory a concurrent Create for that id is in
	// the middle of filling.
	if err := i.claim(id); err != nil {
		return err
	}
	defer i.release(id)

	// Ephemeral true is the derivation's own claim about a path this
	// package builds, not a value read back from anywhere: Reclaim is
	// given an id and never a Workspace, so there is no field an
	// attacker-controlled file could have set. It is what lets remove's
	// containment check — written against the Workspace Destroy is
	// handed — apply unchanged.
	ws := belay.Workspace{ID: id, Dir: filepath.Join(i.root, id), Ephemeral: true}

	// remove is the single place this package deletes a tree, which is
	// what makes adding a caller safe: the containment check cannot be
	// bypassed by arriving here instead of through Destroy. It treats a
	// path that is already gone as nothing to do, which is the answer
	// Reclaim wants anyway.
	if err := i.remove("reclaim", ws); err != nil {
		return err
	}

	i.mu.Lock()
	delete(i.stats, id)
	i.mu.Unlock()
	return nil
}
