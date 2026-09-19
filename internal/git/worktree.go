//go:build unix

package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Worktree sentinels. Match them with errors.Is.
var (
	// ErrNoBranch reports a branch that does not exist where one must.
	ErrNoBranch = errors.New("git: no such branch")

	// ErrLocked reports a worktree someone locked with `git worktree lock`.
	// A lock is a person saying "leave this alone", often because the
	// worktree lives on a disk that is not always mounted, and belay
	// honours it rather than reaching for the double --force that overrides
	// it.
	ErrLocked = errors.New("git: the worktree is locked")
)

// Worktree is one entry of `git worktree list`.
type Worktree struct {
	// Path is the worktree's absolute, symlink-resolved directory.
	Path string
	// Head is the commit checked out there. It is empty for a bare main
	// worktree.
	Head string
	// Branch is the branch checked out there, without "refs/heads/", or ""
	// when HEAD is detached or the entry is bare.
	Branch string
	// Bare reports a bare main repository, which has no files.
	Bare bool
	// Detached reports a worktree whose HEAD is on no branch.
	Detached bool
	// Locked reports a worktree someone locked, which belay never removes.
	Locked bool
	// LockReason is the reason given to `git worktree lock --reason`, if any.
	LockReason string
	// Prunable reports an entry whose directory no longer exists, which
	// `git worktree prune` would remove.
	Prunable bool
}

// Worktrees lists every worktree of the repository, the main one first, in
// the order git lists them.
func (r *Repo) Worktrees(ctx context.Context) ([]Worktree, error) {
	res, err := r.run(ctx, r.top, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(res.Stdout)
}

// parseWorktrees reads `worktree list --porcelain -z`: each entry is a run of
// NUL-terminated "key value" lines, starting with "worktree <path>" and
// ended by an empty line. The -z form is the only safe one: without it git
// quotes a path with a newline in it, and the plain form's line splitting
// reads it as two entries.
//
// Attributes this package does not know are skipped, so a git that adds
// one does not break the listing. A line outside an entry, or an entry with
// no path, is an error: a caller asking "is this directory registered?" must
// never be told "no" because the answer was unreadable.
func parseWorktrees(out string) ([]Worktree, error) {
	var (
		list []Worktree
		cur  *Worktree
	)
	for line := range strings.SplitSeq(out, "\x00") {
		if line == "" {
			if cur != nil {
				list = append(list, *cur)
				cur = nil
			}
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		if key == "worktree" {
			if cur != nil {
				return nil, fmt.Errorf("git: worktree entry for %q is not terminated", cur.Path)
			}
			if value == "" {
				return nil, fmt.Errorf("git: worktree entry with no path")
			}
			cur = &Worktree{Path: value}
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("git: worktree attribute %q outside an entry", line)
		}
		switch key {
		case "HEAD":
			cur.Head = value
		case "branch":
			cur.Branch = strings.TrimPrefix(value, "refs/heads/")
		case "bare":
			cur.Bare = true
		case "detached":
			cur.Detached = true
		case "locked":
			cur.Locked, cur.LockReason = true, value
		case "prunable":
			cur.Prunable = true
		}
	}
	if cur != nil {
		return nil, fmt.Errorf("git: worktree entry for %q is not terminated", cur.Path)
	}
	return list, nil
}

// AddWorktree checks the existing branch out into a new worktree at dir,
// which must be absolute and must not already hold files.
//
// It never creates a branch, and that is the whole design. `git worktree
// add -b` creates its branch even when the add then fails over an occupied
// directory, and `git worktree add <path> <name>` with no local branch of
// that name quietly creates one from a remote-tracking branch of the same
// name. Either leaves a branch nobody asked for and nothing will clean up.
// So the caller creates the branch with CreateBranch, which is
// compare-and-swap, and AddWorktree refuses with ErrNoBranch when the
// branch is not there.
//
// A failed add leaves nothing behind: no registration, no branch, and the
// destination's existing contents untouched. dir is absolute and branch is
// validated, so neither can be read as an option.
func (r *Repo) AddWorktree(ctx context.Context, dir, branch string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("git: worktree directory %q is not absolute", dir)
	}
	if err := validateBranch(branch); err != nil {
		return err
	}
	if _, ok, err := r.BranchTip(ctx, branch); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%w: %s", ErrNoBranch, branch)
	}
	_, err := r.run(ctx, r.top, "worktree", "add", dir, branch)
	return err
}

// RemoveWorktree removes the worktree registered at dir, which must be
// absolute: its directory, uncommitted changes and all, and its
// registration. It never deletes the branch that was checked out there.
//
// It converges when it is run again. A directory that is not registered —
// removed already, or never a worktree — is not an error, because the
// caller asked for it to be gone and it is. That matters on resume: a
// cleanup interrupted after git removed the worktree must not fail when it
// runs a second time.
//
// A stale entry, whose directory someone deleted by hand, is removed too,
// and it is removed alone. belay never runs `git worktree prune`, which
// would also drop the user's own stale entries, such as worktrees on a disk
// that is not mounted right now; `git worktree remove` on the one path
// removes exactly that registration.
//
// Two entries are refused rather than removed. The main working tree is the
// user's checkout, never a worktree belay made. A locked worktree returns
// ErrLocked: the lock is someone saying "leave this alone", and git would
// need a double --force to override it, which belay does not pass.
//
// dir is compared with the registered paths after symlinks are resolved,
// because git records worktree paths resolved: on macOS a worktree added
// under /var/folders is registered under /private/var/folders.
func (r *Repo) RemoveWorktree(ctx context.Context, dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("git: worktree directory %q is not absolute", dir)
	}
	list, err := r.Worktrees(ctx)
	if err != nil {
		return err
	}
	target := resolvePath(dir)
	for i, w := range list {
		if w.Path != target {
			continue
		}
		switch {
		case i == 0:
			// git lists the main worktree first, always.
			return fmt.Errorf("git: %s is the main working tree, which belay never removes", dir)
		case w.Locked && w.LockReason != "":
			return fmt.Errorf("%w: %s (%s); run `git worktree unlock %s` if it may go", ErrLocked, dir, w.LockReason, w.Path)
		case w.Locked:
			return fmt.Errorf("%w: %s; run `git worktree unlock %s` if it may go", ErrLocked, dir, w.Path)
		}
		// --force because a candidate's worktree holds whatever the agent
		// left in it, and discarding that is the point of removing it.
		_, err := r.run(ctx, r.top, "worktree", "remove", "--force", w.Path)
		return err
	}
	return nil
}

// resolvePath returns path with symlinks resolved, as git records worktree
// paths. A path that no longer exists, such as a stale worktree's, is
// resolved through its parent, which usually still does; when neither
// resolves, path is returned cleaned.
func resolvePath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(parent, filepath.Base(path))
	}
	return path
}
