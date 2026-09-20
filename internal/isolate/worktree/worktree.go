//go:build unix

// Package worktree implements [belay.Isolator] with git worktrees: every
// candidate gets a working tree of its own, checked out from the source's
// current commit onto a branch of its own, instead of a copy of the source's
// files.
//
// It is the isolator ADR-0004 decided against and left room for. What changed
// is that a candidate's work now has somewhere to go: a branch can be merged,
// and a directory copy can only be read out by hand. The cost is that this
// only works inside a git repository, which is why internal/isolate/dircopy
// stays and remains the fallback for everything else — a target that is not a
// repository, one with no commits yet, one with submodules, or a git older
// than internal/git's floor.
//
// # Workspace is plain data, and that decides the design
//
// [belay.Workspace] is JSON in the run journal, with no behaviour and no
// handle, and Destroy has to work on one decoded after the process that
// created it is gone. So this package keeps nothing that matters in memory.
// The branch is namespace + "-" + [belay.Workspace.ID] and the worktree is
// root + "/" + ID, where the namespace (the run's branch) and the root are
// fixed when the Isolator is built and come back from the run's manifest on a
// resume. Everything Destroy acts on is derived from those three values.
//
// Workspace.Dir is never one of them. It is compared against the derivation
// and then ignored, because a Workspace read back from a file an agent could
// write is not evidence about anything: a Dir naming the user's checkout, with
// an ID naming a real candidate, would otherwise turn one `git worktree
// remove` into the loss of someone's working tree. The same reasoning is why
// [belay.Workspace] does not grow a Branch field: a decoded branch name would
// have to be checked against the derivation anyway, so it would be a field
// that can only ever agree.
//
// # The separator is "-" and not "/"
//
// The run's own branch is belay/<run-id>, so belay/<run-id>/<candidate> cannot
// exist: refs are stored as paths, and git refuses to make one name both a
// file and a directory ("cannot lock ref ... exists", verified on git 2.54 in
// TestBranchNamesCannotNestUnderTheRunBranch). A "-" cannot collide that way,
// and it also cannot join two valid components into something git rejects,
// since every rule about dots, ".lock" and "@{" is about characters that would
// have to be adjacent.
//
// # What it refuses to do
//
// These are decisions, not gaps:
//
//   - It does not carry the source's uncommitted changes into a candidate. A
//     worktree is a checkout of a commit and cannot hold them, and branching
//     from HEAD while saying nothing would mean candidates silently started
//     from a different tree than the one the user is looking at. A dirty
//     source is [ErrDirtySource], naming the files.
//   - It does not delete a branch whose tip no other ref holds. Unique commits
//     are work, not an allocation. Destroy releases the worktree, keeps the
//     branch, and logs the exact `git branch -D` that would remove it.
//   - It does not run `git worktree prune`, which would also drop the user's
//     own stale entries — a worktree on a disk that is not mounted right now.
//     It removes exactly its own registration.
//   - It does not force a locked worktree. A lock is a person saying "leave
//     this alone"; Destroy returns [git.ErrLocked] and stops.
//   - It does not take over a destination that already holds files or that git
//     has a worktree registered at, even a stale one. Either is someone else's,
//     and a candidate handed a tree that is partly another's is worse than no
//     candidate at all.
//   - It does not create the run's own worktree or branch. Destroy promises to
//     release everything Create allocated, and the run branch is the
//     deliverable.
//
// # Concurrency
//
// An *Isolator is safe for concurrent use: fanout creating N candidates at
// once is the normal case. Two concurrent Creates with the same id are not —
// the second is refused while the first is in flight, rather than both racing
// into git and the loser getting whichever lock error git happened to hit.
// Nothing here arbitrates between processes; belay assumes one dispatcher owns
// a run at a time, the same assumption internal/state makes.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dhaam-ai/belay/internal/git"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Sentinel errors returned by this package. Match them with errors.Is; the
// ones with a companion type carry their details, recoverable with errors.As.
//
// Which of belay's own sentinels each one wraps is the point of it. A caller
// choosing between this isolator and a directory copy needs to tell "this
// source can never be isolated this way" (belay.ErrUnsupported) from "this one
// can, once someone does something".
var (
	// ErrRefused reports an operation rejected because what it named is not a
	// workspace this Isolator owns: a Workspace whose Dir does not match what
	// its ID derives to, one that is not ephemeral, or a worktree that has
	// been moved to another branch since. It always means nothing was
	// removed and no branch was deleted.
	//
	// Recover a *ContainmentError with errors.As for the path, the
	// derivation it disagreed with and the rule it broke.
	ErrRefused = errors.New("worktree: refused: not a workspace this isolator owns")

	// ErrInvalidID reports a candidate id this Isolator cannot use: one that
	// could name something other than a direct child of the candidate root,
	// or that git would refuse as one component of a ref.
	//
	// Recover an *IDError with errors.As for the id and the reason.
	ErrInvalidID = errors.New("worktree: invalid candidate id")

	// ErrNotEmpty reports a destination that already holds entries. Checking
	// a worktree out over them would blend two trees and hand the result to
	// an agent as if it were a clean checkout.
	ErrNotEmpty = errors.New("worktree: destination directory is not empty")

	// ErrDirtySource reports a source with uncommitted changes, which a
	// worktree cannot carry.
	//
	// Recover a *DirtyError with errors.As for the paths.
	ErrDirtySource = errors.New("worktree: the source has uncommitted changes")

	// ErrForeignSource reports a source that is not inside any working tree
	// of this Isolator's repository — another repository, or no repository
	// at all. It wraps belay.ErrUnsupported: no amount of waiting makes a
	// directory part of a repository it is not in, and the caller's answer is
	// to copy the directory instead.
	ErrForeignSource = fmt.Errorf("%w: worktree: the source is not inside this repository", belay.ErrUnsupported)

	// ErrStaleWorktree reports that git already has a worktree registered at
	// the destination, whether or not its directory still exists. It
	// deliberately does NOT wrap belay.ErrUnsupported: one `git worktree
	// remove` fixes it, and the error names that command.
	ErrStaleWorktree = errors.New("worktree: a worktree is already registered at the destination")
)

// ContainmentError reports a path or a Workspace that was refused because it
// is not one this Isolator owns. It wraps ErrRefused.
//
// Every ContainmentError means the operation stopped before touching
// anything: no directory was removed and no branch was deleted.
type ContainmentError struct {
	// Op is the operation that refused, "create" or "destroy".
	Op string
	// Path is the path as the caller supplied it, if any.
	Path string
	// Derived is the path this Isolator derived from the workspace's ID,
	// which is the only one it would have acted on.
	Derived string
	// Root is the candidate root every workspace is a direct child of.
	Root string
	// Reason explains which rule was broken.
	Reason string
}

// Error implements error.
func (e *ContainmentError) Error() string {
	if e.Derived != "" {
		return fmt.Sprintf("worktree: %s refused for %q: %s (this isolator would only act on %q, under %q)",
			e.Op, e.Path, e.Reason, e.Derived, e.Root)
	}
	return fmt.Sprintf("worktree: %s refused for %q: %s (candidate root %q)", e.Op, e.Path, e.Reason, e.Root)
}

// Unwrap reports ErrRefused.
func (e *ContainmentError) Unwrap() error { return ErrRefused }

// IDError reports a rejected candidate id. It wraps ErrInvalidID.
type IDError struct {
	// ID is the id as supplied.
	ID string
	// Reason explains which rule it broke.
	Reason string
}

// Error implements error.
func (e *IDError) Error() string {
	return fmt.Sprintf("worktree: invalid candidate id %q: %s", e.ID, e.Reason)
}

// Unwrap reports ErrInvalidID.
func (e *IDError) Unwrap() error { return ErrInvalidID }

// DirtyError reports the source that had uncommitted changes and the paths
// that made it dirty. It wraps ErrDirtySource.
type DirtyError struct {
	// Dir is the source as supplied.
	Dir string
	// Paths are the changed files, relative to the top of the source's
	// working tree, in the order git listed them.
	Paths []string
}

// Error implements error.
func (e *DirtyError) Error() string {
	if len(e.Paths) == 0 {
		return fmt.Sprintf("worktree: %q has uncommitted changes", e.Dir)
	}
	return fmt.Sprintf("worktree: %q has %d uncommitted change(s), starting with %s; commit or stash them, "+
		"or use the dircopy isolator, which carries them", e.Dir, len(e.Paths), e.Paths[0])
}

// Unwrap reports ErrDirtySource.
func (e *DirtyError) Unwrap() error { return ErrDirtySource }

// maxIDLen bounds a candidate id at the traditional single-component limit,
// so a long id fails with this package's own error rather than an
// ENAMETOOLONG from whichever syscall happens to hit the limit first. git's
// own ref rules stop at the same number, for the same reason: the files ref
// backend stores a ref as a file named after the component.
const maxIDLen = 255

// branchSeparator joins the namespace and the id into a branch name. See the
// package doc for why it is not "/".
const branchSeparator = "-"

// Isolator creates candidate workspaces as git worktrees on branches under a
// fixed namespace, and destroys them by removing the worktree and, when it
// holds nothing, the branch.
//
// Construct one with New; the zero value is not usable, because an Isolator
// with no root and no namespace has nothing to derive a workspace from and
// Destroy would have nothing to check against.
//
// An *Isolator is safe for concurrent use by multiple goroutines.
type Isolator struct {
	// repo is the repository every worktree belongs to. Everything this
	// package does to git goes through it, so the hardening internal/git
	// applies to every command applies here too.
	repo *git.Repo

	// root is the candidate root: absolute, cleaned and resolved with
	// EvalSymlinks at construction. Every workspace is a direct child of it.
	// Resolving once, up front, is what makes the containment check
	// meaningful — git records worktree paths resolved, and comparing a
	// resolved path against an unresolved root compares two different
	// namings of the same filesystem.
	root string

	// namespace is the run's own branch, which every candidate branch hangs
	// off with branchSeparator. It is validated at construction, so
	// Branch cannot fail for a reason that is not the id's.
	namespace string

	log *slog.Logger

	mu       sync.Mutex
	inflight map[string]struct{}
}

// Isolator implements belay.Isolator.
var _ belay.Isolator = (*Isolator)(nil)

// Option configures an Isolator. See WithLogger.
type Option func(*Isolator)

// WithLogger sets the logger used for the line every Create and Destroy
// writes, and for the warning that names the branch Destroy kept. The default
// is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(i *Isolator) {
		if l != nil {
			i.log = l
		}
	}
}

// New returns an Isolator that creates worktrees as direct children of root,
// on branches under namespace, in repo.
//
// namespace is the run's own branch, belay/<run-id>. It is validated here
// rather than when a branch is built, so a run whose branch name git would
// refuse fails before a candidate is created rather than halfway through one.
//
// root is made absolute, created if it does not exist, and resolved with
// EvalSymlinks once; that resolved path is the containment boundary for the
// Isolator's whole life. Callers with a run layout pass
// state.Layout.CandidatesDir(); this package takes it as a parameter rather
// than depending on internal/state, so the boundary cannot change under it.
func New(repo *git.Repo, root, namespace string, opts ...Option) (*Isolator, error) {
	if repo == nil {
		return nil, errors.New("worktree: no repository")
	}
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("worktree: the candidate root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("worktree: resolve candidate root %q: %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("worktree: create candidate root %q: %w", abs, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("worktree: resolve candidate root %q: %w", abs, err)
	}

	i := &Isolator{
		repo:      repo,
		root:      filepath.Clean(resolved),
		namespace: namespace,
		log:       slog.Default(),
		inflight:  make(map[string]struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(i)
		}
	}
	return i, nil
}

// Root returns the candidate root every workspace this Isolator creates or
// destroys is a direct child of, absolute and symlink-resolved.
func (i *Isolator) Root() string { return i.root }

// Branch returns the name of the branch the workspace created with id is
// checked out on, which is namespace + "-" + id.
//
// It is a pure function of the id and the namespace, with nothing read from
// disk, which is what lets the promote node ask for a candidate's branch after
// a restart — and what lets Destroy rebuild the name from a Workspace that
// came back from the journal.
func (i *Isolator) Branch(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return i.namespace + branchSeparator + id, nil
}

// Create checks a new branch, made at the source's current commit, out into a
// worktree named id under the candidate root, and returns that workspace.
//
// The source must be inside this Isolator's repository (ErrForeignSource),
// must have a commit to branch from (git.ErrNoCommits), and must have nothing
// uncommitted (ErrDirtySource) — a worktree is a checkout of a commit and
// cannot carry uncommitted work, and branching from HEAD without saying so
// would give the candidate a different tree than the one the user sees.
//
// The returned Dir is the same place inside the candidate that src is inside
// its own working tree, so a run started in a subdirectory hands its agent
// that subdirectory and not the repository root.
//
// On any failure Create leaves nothing behind, as belay.Isolator requires: the
// branch it made is deleted, the directory it made is removed, and the
// returned Workspace is the zero value, so there is nothing for the caller to
// Destroy. The branch is the part that needs saying: `git worktree add -b`
// creates its branch even when the add then fails over an occupied directory,
// which is why the branch is created here, by name, and unwound by hand.
func (i *Isolator) Create(ctx context.Context, src, id string) (belay.Workspace, error) {
	if err := ctx.Err(); err != nil {
		return belay.Workspace{}, fmt.Errorf("worktree: create %q: %w", id, err)
	}
	branch, err := i.Branch(id) // validates id
	if err != nil {
		return belay.Workspace{}, err
	}
	dir, err := i.workspaceDir("create", id, "")
	if err != nil {
		return belay.Workspace{}, err
	}

	if err := i.claim(id); err != nil {
		return belay.Workspace{}, err
	}
	defer i.release(id)

	list, err := i.repo.Worktrees(ctx)
	if err != nil {
		return belay.Workspace{}, fmt.Errorf("worktree: create %q: %w", id, err)
	}
	base, prefix, err := source(list, src)
	if err != nil {
		return belay.Workspace{}, err
	}
	dirty, err := i.repo.Status(ctx, src)
	if err != nil {
		return belay.Workspace{}, fmt.Errorf("worktree: create %q: %w", id, err)
	}
	if len(dirty) > 0 {
		return belay.Workspace{}, &DirtyError{Dir: src, Paths: dirty}
	}
	if err := checkDestination(list, dir); err != nil {
		return belay.Workspace{}, err
	}

	// CreateBranch is create-only and AddWorktree refuses a branch that does
	// not exist, so the two steps cannot between them move a branch someone
	// else owns or invent one from a remote-tracking name.
	if err := i.repo.CreateBranch(ctx, branch, base); err != nil {
		return belay.Workspace{}, fmt.Errorf("worktree: create %q: %w", id, err)
	}
	if err := i.repo.AddWorktree(ctx, dir, branch); err != nil {
		return belay.Workspace{}, errors.Join(
			fmt.Errorf("worktree: create %q: %w", id, err),
			i.unwind(ctx, dir, branch, base),
		)
	}

	ws := belay.Workspace{ID: id, Dir: filepath.Join(dir, prefix), Ephemeral: true}
	i.log.Info("worktree: created a candidate workspace",
		slog.String("id", id), slog.String("dir", ws.Dir),
		slog.String("branch", branch), slog.String("base", base), slog.String("src", src))
	return ws, nil
}

// unwind takes back what a failed Create had already allocated: the
// registration and directory `worktree add` may have left part-built, and the
// branch created just before it.
//
// DeleteBranch is compare-and-swap against the commit the branch was created
// at, so a branch something else moved in the meantime is left alone rather
// than deleted with whatever it now holds.
func (i *Isolator) unwind(ctx context.Context, dir, branch, base string) error {
	errs := []error{i.repo.RemoveWorktree(ctx, dir), i.removeDir("create", dir)}
	if _, ok, err := i.repo.BranchTip(ctx, branch); err != nil {
		errs = append(errs, err)
	} else if ok {
		errs = append(errs, i.repo.DeleteBranch(ctx, branch, base))
	}
	if err := errors.Join(errs...); err != nil {
		i.log.Error("worktree: could not take back everything a failed Create allocated",
			slog.String("dir", dir), slog.String("branch", branch), slog.Any("error", err))
		return fmt.Errorf("worktree: unwind %q: %w", dir, err)
	}
	return nil
}

// Destroy removes the worktree ws was created as, and the branch it was on
// when no other ref holds that branch's tip.
//
// Everything it acts on is derived from ws.ID and the root and namespace the
// Isolator was built with, so a Workspace decoded from a run journal after a
// restart works exactly as the one Create returned. ws.Dir is compared against
// the derivation and otherwise unused: it arrives from a file, and a Dir
// naming the user's own checkout must never be able to steer a removal.
//
// A Workspace whose Dir does not match, one that is not ephemeral, and a
// worktree that has been moved to another branch since are refused with
// ErrRefused, having removed nothing. A worktree someone locked is
// git.ErrLocked, for the same reason: the lock is a person saying "leave this
// alone".
//
// Destroying a workspace that is already gone is not an error. That is a
// requirement rather than a nicety: the dispatcher re-runs a node whose start
// has no finish, so the node that destroys candidates runs its cleanup again
// after a crash, and the second run has to converge.
//
// A branch whose tip no other ref contains is KEPT, and the warning says the
// command that would delete it. Those commits are work — a candidate that
// committed for itself and then lost — and this package will not be the thing
// that throws them away. The worktree is released either way.
func (i *Isolator) Destroy(ctx context.Context, ws belay.Workspace) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("worktree: destroy %q: %w", ws.ID, err)
	}
	branch, err := i.Branch(ws.ID) // validates ws.ID
	if err != nil {
		return err
	}
	dir, err := i.workspaceDir("destroy", ws.ID, ws.Dir)
	if err != nil {
		return err
	}
	if err := i.matches(ws, dir); err != nil {
		return err
	}

	list, err := i.repo.Worktrees(ctx)
	if err != nil {
		return fmt.Errorf("worktree: destroy %q: %w", ws.ID, err)
	}
	for _, w := range list {
		if w.Path != dir {
			continue
		}
		// The registration must still be the one Create made. A worktree
		// someone checked another branch out in is theirs now, and removing
		// it would take their work with it.
		if w.Branch != branch {
			return &ContainmentError{
				Op: "destroy", Path: ws.Dir, Derived: dir, Root: i.root,
				Reason: fmt.Sprintf("the worktree there is on %s, not on %s", describeBranch(w), branch),
			}
		}
	}
	// RemoveWorktree removes exactly this registration, a stale one included,
	// and is not an error when there is none. It never runs `worktree prune`,
	// which would also drop the user's own stale entries.
	if err := i.repo.RemoveWorktree(ctx, dir); err != nil {
		return fmt.Errorf("worktree: destroy %q: %w", ws.ID, err)
	}
	if err := i.removeDir("destroy", dir); err != nil {
		return err
	}

	tip, ok, err := i.repo.BranchTip(ctx, branch)
	if err != nil {
		return fmt.Errorf("worktree: destroy %q: %w", ws.ID, err)
	}
	if !ok {
		i.log.Info("worktree: destroyed a candidate workspace",
			slog.String("id", ws.ID), slog.String("dir", dir), slog.String("branch", branch))
		return nil
	}
	held, err := i.repo.ContainedElsewhere(ctx, branch)
	if err != nil {
		return fmt.Errorf("worktree: destroy %q: %w", ws.ID, err)
	}
	if !held {
		// `git branch -d` is not the check that belongs here: it judges merge
		// status against the HEAD of the checkout it runs in, which is the
		// user's branch and not the run's, so a candidate merged into the run
		// branch would look unmerged and pile up. ContainedElsewhere asks
		// whether any ref at all holds the tip.
		i.log.Warn("worktree: keeping a candidate branch whose commits no other ref holds",
			slog.String("id", ws.ID), slog.String("branch", branch), slog.String("tip", tip),
			slog.String("delete_with", "git branch -D "+branch))
		return nil
	}
	if err := i.repo.DeleteBranch(ctx, branch, tip); err != nil {
		return fmt.Errorf("worktree: destroy %q: %w", ws.ID, err)
	}
	i.log.Info("worktree: destroyed a candidate workspace",
		slog.String("id", ws.ID), slog.String("dir", dir), slog.String("branch", branch))
	return nil
}

// describeBranch names what a worktree has checked out, for an error a person
// reads. A detached worktree has no branch name to print, and "" would read
// as a missing value rather than as the state it is.
func describeBranch(w git.Worktree) string {
	if w.Branch == "" {
		return "no branch (a detached HEAD at " + w.Head + ")"
	}
	return w.Branch
}

// workspaceDir returns the directory the workspace with id occupies.
//
// The path is built from the resolved root and a validated id, so it is
// already a direct child of the root; checking again is deliberate and cheap.
// It means a future gap in validateID cannot, on its own, put a removal
// outside the root.
func (i *Isolator) workspaceDir(op, id, path string) (string, error) {
	dir := filepath.Clean(filepath.Join(i.root, id))
	if !isChildOf(i.root, dir) {
		return "", &ContainmentError{
			Op: op, Path: path, Derived: dir, Root: i.root,
			Reason: "the path derived from the id is not a direct child of the candidate root",
		}
	}
	return dir, nil
}

// matches reports whether ws is a Workspace this Isolator produced for dir,
// which is what ws.Dir is read for and the only thing it is trusted with.
//
// ws.Dir may be dir itself or a directory inside it: Create returns the same
// place inside the candidate that the source was inside its own working tree,
// so a run started in a subdirectory has a Dir one level down. The comparison
// is lexical, on the cleaned path, because nothing is done with ws.Dir
// afterwards — resolving it would invite exactly the symlink race the
// derivation exists to avoid.
func (i *Isolator) matches(ws belay.Workspace, dir string) error {
	refuse := func(reason string) error {
		return &ContainmentError{Op: "destroy", Path: ws.Dir, Derived: dir, Root: i.root, Reason: reason}
	}
	switch {
	case ws.Dir == "":
		return refuse("the workspace has no directory")
	case strings.ContainsRune(ws.Dir, 0):
		return refuse("the workspace directory holds a NUL byte")
	case !ws.Ephemeral:
		// Create always sets Ephemeral true, so a false one is a Workspace
		// from another isolator or a hand-built struct. belay.Workspace
		// documents Ephemeral false as "Dir is the user's own tree, do not
		// remove it", and that is the one directory this package must never
		// touch.
		return refuse("the workspace is not ephemeral, so it was not created by this isolator")
	case !filepath.IsAbs(ws.Dir):
		return refuse("the workspace directory is not an absolute path")
	}
	if clean := filepath.Clean(ws.Dir); clean != dir && !isAncestorOf(dir, clean) {
		return refuse("the workspace directory is not the one its id derives to")
	}
	return nil
}

// removeDir removes the derived directory, having proven it is still an
// ordinary directory where the derivation said it would be.
//
// `git worktree remove` has usually done this already; what is left for this
// is a directory whose registration was gone — removed by an earlier attempt
// that crashed before it finished, or never made because `worktree add` failed
// part way.
func (i *Isolator) removeDir(op, dir string) error {
	refuse := func(reason string) error {
		return &ContainmentError{Op: op, Path: dir, Derived: dir, Root: i.root, Reason: reason}
	}
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil // already gone, which is what was asked for
	case err != nil:
		return fmt.Errorf("worktree: %s %q: %w", op, dir, err)
	case info.Mode()&fs.ModeSymlink != 0:
		return refuse("the candidate directory is a symlink")
	case !info.IsDir():
		return refuse("the candidate directory is not a directory")
	}
	// Resolved and checked again: a directory that sits inside the root but
	// resolves outside it is the case the lexical check alone cannot see.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("worktree: %s %q: resolve: %w", op, dir, err)
	}
	if resolved = filepath.Clean(resolved); !isChildOf(i.root, resolved) {
		return refuse("the candidate directory resolves outside the candidate root")
	}
	if err := os.RemoveAll(resolved); err != nil {
		return fmt.Errorf("worktree: %s %q: %w", op, resolved, err)
	}
	return nil
}

// claim reserves id for one in-flight Create, so two concurrent calls cannot
// both decide a destination is free and then race into git for it.
func (i *Isolator) claim(id string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, busy := i.inflight[id]; busy {
		return &IDError{ID: id, Reason: "a Create for this id is already in flight"}
	}
	i.inflight[id] = struct{}{}
	return nil
}

// release ends the reservation claim took.
func (i *Isolator) release(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.inflight, id)
}

// source finds the working tree of this repository that holds src, and
// returns the commit to branch from and src's path within that tree.
//
// Membership is decided from the list git itself keeps, compared against the
// resolved source path, rather than by asking git about the source directory.
// That matters: a worktree's .git is an ordinary file an agent with write
// access can rewrite, so an answer obtained by running git inside src is an
// answer src chose. The registration list is written by git in the common git
// directory, which is outside every workspace.
//
// The longest matching path wins, because candidate worktrees live under
// .belay inside the repository and are therefore nested inside the main
// working tree; the shortest match would say every candidate belonged to the
// user's checkout and branch all of them from the wrong commit.
func source(list []git.Worktree, src string) (base, prefix string, err error) {
	if strings.TrimSpace(src) == "" {
		return "", "", fmt.Errorf("%w: the source must not be empty", ErrForeignSource)
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", "", fmt.Errorf("%w: %q cannot be made absolute: %w", ErrForeignSource, src, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", fmt.Errorf("%w: %q cannot be resolved: %w", ErrForeignSource, src, err)
	}
	resolved = filepath.Clean(resolved)
	if info, statErr := os.Stat(resolved); statErr != nil {
		return "", "", fmt.Errorf("%w: %q cannot be read: %w", ErrForeignSource, src, statErr)
	} else if !info.IsDir() {
		return "", "", fmt.Errorf("%w: %q is not a directory", ErrForeignSource, src)
	}

	var best git.Worktree
	for _, w := range list {
		if w.Bare || (w.Path != resolved && !isAncestorOf(w.Path, resolved)) {
			continue
		}
		if len(w.Path) > len(best.Path) {
			best = w
		}
	}
	switch {
	case best.Path == "":
		return "", "", fmt.Errorf("%w: %q", ErrForeignSource, src)
	case unborn(best.Head):
		// An unborn HEAD is listed with the all-zero object ID. git would
		// happily make an orphan worktree from it, and an orphan cannot be
		// merged into anything, so there would be no way to promote whatever
		// the candidate did.
		return "", "", fmt.Errorf("%w: %q has nothing to branch from", git.ErrNoCommits, src)
	}
	rel, err := filepath.Rel(best.Path, resolved)
	if err != nil {
		return "", "", fmt.Errorf("%w: %q: %w", ErrForeignSource, src, err)
	}
	if rel == "." {
		rel = ""
	}
	return best.Head, rel, nil
}

// unborn reports whether a worktree's HEAD is the all-zero object ID git
// prints for a branch with no commits yet.
func unborn(head string) bool {
	return head == "" || strings.Trim(head, "0") == ""
}

// checkDestination refuses a destination that is not this Isolator's to fill.
//
// A registration is checked first because it is the one git would report as
// "a missing but already registered worktree", which is the same words for a
// live worktree and for one whose directory someone deleted; either way the
// path belongs to a worktree that is not this one, and the answer is the
// command that clears it.
func checkDestination(list []git.Worktree, dir string) error {
	for _, w := range list {
		if w.Path == dir {
			return fmt.Errorf("%w: %s; clear it with `git worktree remove --force %s`", ErrStaleWorktree, dir, dir)
		}
	}

	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil // git creates it
	case err != nil:
		return fmt.Errorf("worktree: inspect destination %q: %w", dir, err)
	case info.Mode()&fs.ModeSymlink != 0:
		return &ContainmentError{Op: "create", Path: dir, Derived: dir, Root: filepath.Dir(dir), Reason: "the destination is a symlink"}
	case !info.IsDir():
		return fmt.Errorf("%w: %q", ErrNotEmpty, dir)
	}

	// #nosec G304 -- dir was built by this package from its own resolved
	// candidate root and an id validateID has already accepted, and it is
	// opened only to count its entries.
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("worktree: inspect destination %q: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("worktree: inspect destination %q: %w", dir, err)
	}
	if len(names) > 0 {
		return fmt.Errorf("%w: %q holds %s", ErrNotEmpty, dir, names[0])
	}
	return nil
}

// validateNamespace refuses a run branch this Isolator cannot hang candidates
// off.
//
// It is the whole-branch-name rule: every slash-separated component has to
// pass git's own, which also rules out an empty component and so a leading,
// trailing or doubled slash. "HEAD" and a name already starting with "refs/"
// are refused on top of that, because git refuses the first as a branch name
// and would turn the second into refs/heads/refs/heads/x.
func validateNamespace(namespace string) error {
	switch {
	case namespace == "HEAD":
		return fmt.Errorf("%w: %q is not a branch name", git.ErrInvalidRef, namespace)
	case strings.HasPrefix(namespace, "refs/"):
		return fmt.Errorf("%w: %q is a ref, and the namespace is a branch name without refs/heads/", git.ErrInvalidRef, namespace)
	}
	for component := range strings.SplitSeq(namespace, "/") {
		if err := git.ValidateRefComponent(component); err != nil {
			return fmt.Errorf("worktree: namespace %q: %w", namespace, err)
		}
	}
	return nil
}

// validateID rejects a candidate id that could name something other than a
// direct child of the candidate root, or that git would refuse as one
// component of a ref.
//
// Both rule sets are applied, and neither would do on its own. The directory
// rules are restated here rather than imported from internal/state, because a
// containment rule that only holds while a second package is also correct is
// not a containment rule. And git's rules matter because the id becomes part
// of a branch name: "x.lock" and "@{0}" are perfectly good directory names,
// and finding out git refuses them by watching a branch creation fail happens
// after a worktree has already been reserved.
func validateID(id string) error {
	switch {
	case id == "":
		return &IDError{ID: id, Reason: "must not be empty"}
	case len(id) > maxIDLen:
		return &IDError{ID: id, Reason: fmt.Sprintf("must be at most %d bytes", maxIDLen)}
	case id == "." || id == "..":
		return &IDError{ID: id, Reason: "must not be a directory reference"}
	case strings.ContainsAny(id, `/\`):
		return &IDError{ID: id, Reason: "must not contain a path separator"}
	case strings.Contains(id, ".."):
		return &IDError{ID: id, Reason: `must not contain ".."`}
	case filepath.IsAbs(id):
		return &IDError{ID: id, Reason: "must not be an absolute path"}
	case strings.ContainsRune(id, 0):
		return &IDError{ID: id, Reason: "must not contain a NUL byte"}
	case strings.TrimSpace(id) == "":
		return &IDError{ID: id, Reason: "must not be only whitespace"}
	}
	if err := git.ValidateRefComponent(id); err != nil {
		return &IDError{ID: id, Reason: strings.TrimPrefix(err.Error(), "git: invalid ref component: ")}
	}
	return nil
}

// isChildOf reports whether path is a direct child of root: inside it, and
// exactly one segment below.
//
// The direct-child rule is stronger than containment alone and it is what
// Destroy enforces, because every workspace this package creates is a direct
// child of the root. It shrinks the window a symlink swapped between the check
// and the removal could exploit from any depth of the tree to the single
// component being removed.
//
// Both arguments must already be absolute and cleaned.
func isChildOf(root, path string) bool {
	return isAncestorOf(root, path) && filepath.Dir(path) == root
}

// isAncestorOf reports whether path lies strictly inside root.
//
// The check is by whole path segments, which is the entire point: a plain
// strings.HasPrefix says "/a/bc" is inside "/a/b" and it is not — they are
// unrelated siblings, and treating one as inside the other is how a deletion
// escapes its boundary. Requiring the separator, and independently confirming
// with filepath.Rel that the relative path does not climb, gives two ways of
// reaching the same answer; both must agree.
//
// A path equal to root is not inside it.
func isAncestorOf(root, path string) bool {
	if root == "" || path == "" || !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return false
	}
	root, path = filepath.Clean(root), filepath.Clean(path)
	if root == path {
		return false
	}
	sep := string(filepath.Separator)
	prefix := root
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+sep) || filepath.IsAbs(rel) {
		return false
	}
	return true
}
