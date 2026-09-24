//go:build unix

package worktree

import (
	"context"
	"fmt"
	"log/slog"
)

// Reclaim releases whatever is left under id — a worktree, git's
// registration of one, a directory, the branch — so that id can be
// created again.
//
// # Why this is not Destroy
//
// Destroy takes a belay.Workspace, and its contract is "at most once per
// successfully CREATED Workspace". Neither half fits a candidate a crash
// abandoned. The process died inside Create, which by contract returns
// the zero Workspace on failure, so there is no Workspace anywhere to
// pass. And Create refuses to start over whatever is there: a directory
// holding entries is ErrNotEmpty, a registration at that path is
// ErrStaleWorktree, and the branch, which `git worktree add -b` creates
// even when the add then fails, is git.ErrBranchExists. Without Reclaim
// that id is unusable forever and its workspace leaks, because the run
// that owned it is gone and will not be cleaning up.
//
// So Reclaim is keyed by id, which is all a later run has.
//
// # Everything is derived, nothing is trusted
//
// The branch is the namespace and the id, and the directory is the root
// and the id, exactly as Create derives them — the derivation Destroy
// also checks against, and the reason belay.Workspace.Dir is never
// trusted. Reclaim is handed no path at all, which makes that property
// structural here rather than enforced: there is no caller-supplied
// directory in this function to get wrong.
//
// What is on disk is still checked before anything is removed. A worktree
// registered at the derived path but checked out on another branch is
// somebody's now, and removing it would take their work: that is a
// *ContainmentError wrapping ErrRefused, with nothing touched. So is a
// candidate path that is a symlink or that resolves outside the candidate
// root. A worktree a person locked is git.ErrLocked, because the lock is
// a person saying "leave this alone".
//
// The registration is removed by path, one entry, never with `git
// worktree prune` — a worktree of the user's own on a disk that is not
// mounted right now looks exactly like debris, and it is not belay's.
//
// The branch follows the same retention rule as Destroy, through the same
// code: a branch whose tip no other ref holds is KEPT and the warning
// names the command that would delete it (see releaseBranch). Reclaim
// then leaves that id uncreatable, which is the right outcome — a
// candidate that committed and lost its run has work in that branch, and
// silently deleting it to free a name is the one thing worse than leaking
// a directory.
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
// reclaiming the same id twice, or one Destroy already released. That is
// a requirement rather than a nicety, and the same one Destroy carries:
// the dispatcher re-runs a node whose start has no finish, so a reclaim
// runs again after a crash and the second run has to converge.
func (i *Isolator) Reclaim(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("worktree: reclaim %q: %w", id, err)
	}
	branch, err := i.Branch(id) // validates id
	if err != nil {
		return err
	}
	dir, err := i.workspaceDir("reclaim", id, "")
	if err != nil {
		return err
	}

	// The same reservation Create takes, for the same reason: a reclaim
	// must not remove a worktree a concurrent Create for that id is in
	// the middle of making.
	if err := i.claim(id); err != nil {
		return err
	}
	defer i.release(id)

	list, err := i.repo.Worktrees(ctx)
	if err != nil {
		return fmt.Errorf("worktree: reclaim %q: %w", id, err)
	}
	for _, w := range list {
		if w.Path != dir {
			continue
		}
		// Whatever is registered here must still be this candidate's. A
		// torn Create leaves either no registration or one on the branch
		// it derived; anything else is someone who took the path over
		// since, and it is theirs.
		if w.Branch != branch {
			return &ContainmentError{
				Op: "reclaim", Path: dir, Derived: dir, Root: i.root,
				Reason: fmt.Sprintf("the worktree there is on %s, not on %s", describeBranch(w), branch),
			}
		}
	}

	// RemoveWorktree removes exactly this registration, a stale one whose
	// directory is already gone included, and is not an error when there
	// is none.
	if err := i.repo.RemoveWorktree(ctx, dir); err != nil {
		return fmt.Errorf("worktree: reclaim %q: %w", id, err)
	}
	if err := i.removeDir("reclaim", dir); err != nil {
		return err
	}
	kept, err := i.releaseBranch(ctx, "reclaim", id, branch)
	if err != nil {
		return err
	}
	if !kept {
		i.log.Info("worktree: reclaimed an abandoned candidate workspace",
			slog.String("id", id), slog.String("dir", dir), slog.String("branch", branch))
	}
	return nil
}
