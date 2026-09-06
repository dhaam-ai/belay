//go:build unix

// Package dircopy implements [belay.Isolator] by copying a source tree into
// a per-candidate directory, giving every best-of-N candidate its own
// writable working copy.
//
// This is the isolator ADR-0004 chose: plain directory copies rather than
// git worktrees, so fanout works on targets that are not git repositories
// and needs no external binary. The [belay.Isolator] interface exists so a
// worktree or container isolator can replace it later without touching the
// graph.
//
// # Threat model
//
// This package is small but dangerous, and it is written defensively for one
// reason: Create copies a tree the user designated and Destroy deletes a
// directory. A path-handling bug here does not produce a wrong answer, it
// deletes someone's work. Worse, the tree Create produces is then handed to
// an autonomous coding agent with write access, so anything reachable from
// inside the copy is reachable by that agent.
//
// Three properties follow, and each is enforced structurally rather than by
// convention:
//
//   - Destroy can only ever delete a direct child of the candidate root the
//     Isolator was constructed with. The path is validated lexically, then
//     resolved with [filepath.EvalSymlinks] and validated again, and both
//     checks compare whole path segments — "/a/bc" is not inside "/a/b". A
//     Workspace this Isolator did not produce, a Dir that escapes the root, a
//     Dir that is itself a symlink out of the root, and an empty Dir are all
//     refused with an error wrapping [ErrRefused]. Nothing is deleted on a
//     refusal.
//
//   - Create never follows a symlink out of the source tree. A repository
//     containing "link -> /" copies a symlink, never the filesystem. By
//     default a symlink whose target lexically escapes the new workspace is
//     not recreated at all (see [SymlinkInternal]), because inside the copy
//     it would be a ready-made write primitive pointing out of the sandbox
//     for the agent that runs there.
//
//   - Create leaves nothing behind when it fails. A copy interrupted by a
//     read error, a full disk or a cancelled context has its partial
//     destination removed, because a half-copied tree that a candidate then
//     builds and tests against is worse than no tree at all. That removal
//     goes through the same containment check as Destroy.
//
// # Cost
//
// ADR-0004 knowingly accepted O(N x repo size) disk and I/O for fanout. Two
// things here keep that cost bounded and, more importantly, visible. A skip
// list (see [DefaultSkip]) omits the directories that dominate a real
// checkout and that a candidate can regenerate — .git, node_modules, vendor
// and friends. And every Create records [Stats] — bytes copied, entries
// skipped, elapsed time — readable back with [Isolator.Stats] and logged at
// info level, so the price of a fanout run is a number someone can look at
// rather than an invisible drain.
//
// # Concurrency
//
// An *Isolator is safe for concurrent use: fanout creating N candidates at
// once is the normal case, not an edge case. Two concurrent Create calls
// with the same id are not: the second is refused while the first is in
// flight. Nothing here arbitrates between processes — belay assumes one
// dispatcher owns a run directory at a time, the same assumption
// internal/state makes.
package dircopy

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
	"time"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// Isolator implements belay.Isolator by copying a source tree into a
// candidate directory under a fixed root.
var _ belay.Isolator = (*Isolator)(nil)

// Sentinel errors returned by this package. Callers detect them with
// errors.Is; each has a companion error type carrying the details,
// recoverable with errors.As.
var (
	// ErrRefused reports that an operation was rejected because the path it
	// named is not a candidate directory this Isolator owns — it escaped the
	// candidate root, resolved outside it through a symlink, or belonged to a
	// Workspace this Isolator never created. It is the error every
	// containment failure wraps, and it always means nothing was deleted.
	//
	// Recover a *ContainmentError with errors.As for the offending path, the
	// root it violated, and why.
	ErrRefused = errors.New("dircopy: refused: path is not a candidate directory")

	// ErrNotEmpty reports that Create was asked to populate a destination
	// that already holds entries. Copying onto an occupied directory would
	// blend two trees and hand the result to an agent as if it were a clean
	// checkout, so it is refused rather than merged.
	//
	// Recover a *NotEmptyError with errors.As for the directory.
	ErrNotEmpty = errors.New("dircopy: destination directory is not empty")

	// ErrInsufficientSpace reports that the filesystem holding the candidate
	// root plainly cannot fit the copy, as measured before the copy starts.
	// Failing up front is the point: filling a disk damages every other
	// process on the machine, not just this run.
	//
	// Recover a *SpaceError with errors.As for the numbers behind the
	// refusal.
	ErrInsufficientSpace = errors.New("dircopy: insufficient free disk space")

	// ErrInvalidID reports a candidate id that cannot be used as a single
	// directory name under the candidate root — empty, containing a
	// separator or "..", absolute, or otherwise able to name something other
	// than a direct child of the root.
	//
	// Recover an *IDError with errors.As for the id and the reason.
	ErrInvalidID = errors.New("dircopy: invalid candidate id")

	// ErrInvalidSource reports that Create's src does not name a readable
	// directory, or names one that overlaps the destination in a way that
	// would copy a tree into itself.
	//
	// Recover a *SourceError with errors.As for the path and the reason.
	ErrInvalidSource = errors.New("dircopy: invalid source directory")
)

// ContainmentError reports that a path was refused because it is not a
// candidate directory under the Isolator's root. It wraps ErrRefused.
//
// Every ContainmentError means the operation stopped before touching the
// filesystem: no file was deleted, moved or truncated.
type ContainmentError struct {
	// Op is the operation that refused, "create" or "destroy".
	Op string
	// Path is the path as the caller supplied it.
	Path string
	// Resolved is Path after symlink resolution, or empty if the refusal
	// happened before resolution was attempted.
	Resolved string
	// Root is the candidate root Path had to be a direct child of.
	Root string
	// Reason explains which rule Path broke.
	Reason string
}

// Error implements error.
func (e *ContainmentError) Error() string {
	if e.Resolved != "" && e.Resolved != e.Path {
		return fmt.Sprintf("dircopy: %s refused for %q (resolves to %q): %s (candidate root %q)",
			e.Op, e.Path, e.Resolved, e.Reason, e.Root)
	}
	return fmt.Sprintf("dircopy: %s refused for %q: %s (candidate root %q)", e.Op, e.Path, e.Reason, e.Root)
}

// Unwrap reports ErrRefused.
func (e *ContainmentError) Unwrap() error { return ErrRefused }

// NotEmptyError reports the destination directory that already held entries.
// It wraps ErrNotEmpty.
type NotEmptyError struct {
	// Dir is the non-empty destination.
	Dir string
}

// Error implements error.
func (e *NotEmptyError) Error() string {
	return fmt.Sprintf("dircopy: destination %q is not empty", e.Dir)
}

// Unwrap reports ErrNotEmpty.
func (e *NotEmptyError) Unwrap() error { return ErrNotEmpty }

// SpaceError reports a copy refused by the pre-flight disk check, with every
// number that went into the decision so the message is actionable. It wraps
// ErrInsufficientSpace.
type SpaceError struct {
	// Dir is the destination whose filesystem was measured.
	Dir string
	// NeedBytes is the estimated size of the copy: the total size of every
	// regular file that would be copied, after the skip list is applied.
	NeedBytes int64
	// FreeBytes is the space available to this user on that filesystem when
	// the check ran.
	FreeBytes int64
	// ReserveBytes is the headroom the check insists on leaving free beyond
	// NeedBytes. See WithFreeSpaceReserve.
	ReserveBytes int64
}

// Error implements error.
func (e *SpaceError) Error() string {
	return fmt.Sprintf("dircopy: copy into %q needs %d bytes plus %d reserved but only %d are free",
		e.Dir, e.NeedBytes, e.ReserveBytes, e.FreeBytes)
}

// Unwrap reports ErrInsufficientSpace.
func (e *SpaceError) Unwrap() error { return ErrInsufficientSpace }

// IDError reports a rejected candidate id. It wraps ErrInvalidID.
type IDError struct {
	// ID is the id as supplied.
	ID string
	// Reason explains which rule it broke.
	Reason string
}

// Error implements error.
func (e *IDError) Error() string {
	return fmt.Sprintf("dircopy: invalid candidate id %q: %s", e.ID, e.Reason)
}

// Unwrap reports ErrInvalidID.
func (e *IDError) Unwrap() error { return ErrInvalidID }

// SourceError reports a rejected source directory. It wraps
// ErrInvalidSource.
type SourceError struct {
	// Path is the source as supplied.
	Path string
	// Reason explains why it cannot be copied.
	Reason string
}

// Error implements error.
func (e *SourceError) Error() string {
	return fmt.Sprintf("dircopy: cannot copy source %q: %s", e.Path, e.Reason)
}

// Unwrap reports ErrInvalidSource.
func (e *SourceError) Unwrap() error { return ErrInvalidSource }

// DefaultFreeSpaceReserve is the headroom Create leaves free beyond the
// estimated size of a copy, unless WithFreeSpaceReserve says otherwise.
//
// It is deliberately modest. The check exists to refuse a copy that plainly
// cannot fit, not to enforce a disk budget, and a reserve large enough to be
// a policy would start refusing copies that would have succeeded.
const DefaultFreeSpaceReserve int64 = 64 << 20 // 64 MiB

// maxIDLen bounds a candidate id at the traditional single-component limit,
// so a long id fails with this package's own error rather than a bare
// ENAMETOOLONG from the first syscall that happens to hit the limit.
const maxIDLen = 255

// defaultSkip lists the directory and file names Create omits by default.
//
// Every entry is either regenerable from what is copied (node_modules,
// vendor, .venv, __pycache__, target), a build output (dist, build), or
// belay's own state (.belay). .git is the interesting one: omitting it means
// a candidate cannot run git commands, which is the trade ADR-0004 already
// accepted when it chose copies over worktrees, and it is usually the
// largest single directory in a checkout.
var defaultSkip = []string{
	".git",
	"node_modules",
	"vendor",
	".venv",
	"__pycache__",
	"dist",
	"build",
	"target",
	".belay",
}

// DefaultSkip returns the names Create omits from a copy by default, as a
// fresh slice the caller may modify. See WithSkip to change the list.
func DefaultSkip() []string {
	out := make([]string, len(defaultSkip))
	copy(out, defaultSkip)
	return out
}

// SymlinkPolicy selects what Create does with a symlink it finds in the
// source tree.
//
// No policy ever follows a symlink: the tree is walked with lstat
// throughout, so a link to a directory is a link, never a doorway. The
// policy only decides whether the link itself is recreated in the copy.
type SymlinkPolicy int

const (
	// SymlinkInternal recreates a symlink only when its target stays inside
	// the new workspace, and silently omits it otherwise. It is the default,
	// and it is the safe choice: the workspace is handed to an autonomous
	// agent with write access, and a preserved "config -> /etc/nginx" or
	// "up -> ../../.." is a write primitive pointing out of the sandbox that
	// the agent did not have to work for.
	//
	// Containment is decided lexically, from the link text and the link's own
	// location in the copy, because the tree is still being built and the
	// target may not exist yet. Since every preserved link is individually
	// contained, no chain of them can leave the workspace either.
	SymlinkInternal SymlinkPolicy = iota

	// SymlinkPreserveAll recreates every symlink exactly as it appears in the
	// source, including targets outside the workspace.
	//
	// It reproduces the source tree most faithfully and is the right choice
	// when the copy is an archive rather than an agent's sandbox. Do not
	// combine it with an untrusted source tree and an agent that can write:
	// the links are the sandbox escape.
	SymlinkPreserveAll

	// SymlinkSkip recreates no symlinks at all, for callers that want a copy
	// containing only ordinary files and directories.
	SymlinkSkip
)

// String implements fmt.Stringer.
func (p SymlinkPolicy) String() string {
	switch p {
	case SymlinkInternal:
		return "internal"
	case SymlinkPreserveAll:
		return "preserve-all"
	case SymlinkSkip:
		return "skip"
	default:
		return fmt.Sprintf("SymlinkPolicy(%d)", int(p))
	}
}

// Isolator creates candidate workspaces by copying a source tree, and
// destroys them by removing the copy.
//
// Construct one with New; the zero value is not usable, because an Isolator
// with no candidate root has no containment boundary to enforce and Destroy
// would have nothing to check against.
//
// An *Isolator is safe for concurrent use by multiple goroutines.
type Isolator struct {
	// root is the candidate root: absolute, cleaned and fully resolved with
	// EvalSymlinks at construction. Every path this Isolator creates or
	// destroys must be a direct child of it. Resolving once, up front, is
	// what makes the containment check in ownedPath meaningful — comparing a
	// resolved path against an unresolved root would compare two different
	// namings of the filesystem.
	root string

	skip         map[string]struct{}
	policy       SymlinkPolicy
	reserveBytes int64
	log          *slog.Logger

	// freeSpace probes free space on the filesystem holding a path. It is a
	// field so tests can force the disk check to fail without filling a real
	// disk; production always uses freeSpaceOn.
	freeSpace func(path string) (int64, error)

	mu       sync.Mutex
	inflight map[string]struct{}
	stats    map[string]Stats
}

// Option configures an Isolator. See WithSkip, WithSymlinkPolicy,
// WithFreeSpaceReserve and WithLogger.
type Option func(*Isolator)

// WithSkip replaces the default skip list with names, matched against the
// base name of every entry at any depth. Passing no names disables skipping
// entirely and copies the tree in full — including .git, which is usually
// not what you want; see DefaultSkip for the reasoning behind the defaults.
func WithSkip(names ...string) Option {
	return func(i *Isolator) {
		set := make(map[string]struct{}, len(names))
		for _, n := range names {
			if n != "" {
				set[n] = struct{}{}
			}
		}
		i.skip = set
	}
}

// WithSymlinkPolicy selects what Create does with symlinks in the source
// tree. The default is SymlinkInternal; read that constant's documentation
// before choosing SymlinkPreserveAll.
func WithSymlinkPolicy(p SymlinkPolicy) Option {
	return func(i *Isolator) { i.policy = p }
}

// WithFreeSpaceReserve sets the headroom Create leaves free beyond the
// estimated size of a copy. A negative value is treated as zero, which keeps
// the "does it fit at all" check but demands no margin.
func WithFreeSpaceReserve(bytes int64) Option {
	return func(i *Isolator) {
		if bytes < 0 {
			bytes = 0
		}
		i.reserveBytes = bytes
	}
}

// WithLogger sets the logger used for the per-copy cost line and for the
// warning every containment refusal emits. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(i *Isolator) {
		if l != nil {
			i.log = l
		}
	}
}

// New returns an Isolator that creates candidate workspaces as direct
// children of root, creating root itself if it does not exist.
//
// root is made absolute and resolved with EvalSymlinks once, here, and that
// resolved path is the containment boundary for the Isolator's whole life.
// Callers that already have a run layout should pass
// state.Layout.CandidatesDir(); this package deliberately does not depend on
// internal/state, so the boundary is a parameter rather than a coupling.
func New(root string, opts ...Option) (*Isolator, error) {
	if strings.TrimSpace(root) == "" {
		return nil, &SourceError{Path: root, Reason: "candidate root must not be empty"}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("dircopy: resolve candidate root %q: %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("dircopy: create candidate root %q: %w", abs, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("dircopy: resolve candidate root %q: %w", abs, err)
	}

	i := &Isolator{
		root:         filepath.Clean(resolved),
		policy:       SymlinkInternal,
		reserveBytes: DefaultFreeSpaceReserve,
		log:          slog.Default(),
		freeSpace:    freeSpaceOn,
		inflight:     make(map[string]struct{}),
		stats:        make(map[string]Stats),
	}
	WithSkip(defaultSkip...)(i)
	for _, opt := range opts {
		if opt != nil {
			opt(i)
		}
	}
	return i, nil
}

// Root returns the candidate root every workspace this Isolator creates or
// destroys must be a direct child of, absolute and symlink-resolved.
func (i *Isolator) Root() string { return i.root }

// Stats returns what the most recent successful Create for id copied, and
// whether such a record exists. Destroy forgets the record for the workspace
// it removes.
func (i *Isolator) Stats(id string) (Stats, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	s, ok := i.stats[id]
	return s, ok
}

// Create copies src into a new directory named id under the candidate root
// and returns the resulting ephemeral workspace.
//
// The copy omits the skip list (see DefaultSkip), never follows a symlink
// out of src, preserves file permissions without setuid or setgid bits, and
// refuses to start if the destination already holds entries or if the copy
// plainly will not fit on the destination filesystem.
//
// On any failure — a bad id, an unreadable file midway, a cancelled ctx —
// Create removes whatever it had written and returns the zero Workspace, as
// belay.Isolator requires: a non-nil error means there is nothing to
// Destroy.
func (i *Isolator) Create(ctx context.Context, src, id string) (belay.Workspace, error) {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return belay.Workspace{}, fmt.Errorf("dircopy: create %q: %w", id, err)
	}
	if err := validateID(id); err != nil {
		return belay.Workspace{}, err
	}
	srcDir, err := i.resolveSource(src)
	if err != nil {
		return belay.Workspace{}, err
	}

	dst := filepath.Join(i.root, id)
	// Structural re-check of a path this function just built from a
	// validated id. It is redundant today and deliberately kept: it means a
	// future gap in validateID cannot, on its own, put a destination outside
	// the root.
	if _, err := i.ownedPath("create", belay.Workspace{Dir: dst, Ephemeral: true}, false); err != nil {
		return belay.Workspace{}, err
	}
	if err := checkOverlap(srcDir, dst); err != nil {
		return belay.Workspace{}, err
	}

	if err := i.claim(id); err != nil {
		return belay.Workspace{}, err
	}
	defer i.release(id)

	if err := prepareDest(dst); err != nil {
		return belay.Workspace{}, err
	}

	stats, err := i.copyInto(ctx, srcDir, dst, start)
	if err != nil {
		i.cleanupPartial(dst, err)
		return belay.Workspace{}, fmt.Errorf("dircopy: create %q: %w", id, err)
	}

	i.mu.Lock()
	i.stats[id] = stats
	i.mu.Unlock()

	i.log.Info("dircopy created candidate workspace",
		"id", id,
		"src", srcDir,
		"dir", dst,
		"files", stats.Files,
		"dirs", stats.Dirs,
		"symlinks", stats.Symlinks,
		"bytes_copied", stats.BytesCopied,
		"skipped", stats.Skipped,
		"skipped_symlinks", stats.SkippedSymlinks,
		"irregular", stats.Irregular,
		"duration", stats.Duration,
	)
	return belay.Workspace{ID: id, Dir: dst, Ephemeral: true}, nil
}

// copyInto runs the disk pre-check and then the copy itself.
func (i *Isolator) copyInto(ctx context.Context, srcDir, dst string, start time.Time) (Stats, error) {
	c := &copier{
		src:     srcDir,
		dst:     dst,
		skip:    i.skip,
		exclude: map[string]struct{}{dst: {}, i.root: {}},
		policy:  i.policy,
		log:     i.log,
	}

	need, err := c.estimate(ctx)
	if err != nil {
		return Stats{}, err
	}
	if err := i.checkSpace(dst, need); err != nil {
		return Stats{}, err
	}
	if err := c.run(ctx); err != nil {
		return Stats{}, err
	}
	c.stats.Duration = time.Since(start)
	return c.stats, nil
}

// checkSpace refuses a copy of need bytes that the destination filesystem
// plainly cannot hold.
//
// The heuristic is deliberately simple and stated here so its limits are
// visible: need is the summed size of every regular file that would be
// copied after the skip list is applied, and the copy is refused only when
// need plus the configured reserve exceeds the space currently available to
// this user. It ignores block-size rounding, sparse files, compression and
// deduplication, so it under-counts on filesystems that pack small files and
// over-counts on ones that compress; it is a guard against the obvious
// mistake of copying a 40 GiB tree onto 2 GiB of free space, not an
// accountant. When free space cannot be measured at all the copy proceeds:
// an unmeasurable filesystem is not evidence of a full one, and refusing
// there would break copies that work.
func (i *Isolator) checkSpace(dst string, need int64) error {
	if i.freeSpace == nil {
		return nil
	}
	free, err := i.freeSpace(dst)
	if err != nil {
		i.log.Debug("dircopy could not measure free space; continuing", "dir", dst, "error", err)
		return nil
	}
	if free-i.reserveBytes >= need {
		return nil
	}
	return &SpaceError{Dir: dst, NeedBytes: need, FreeBytes: free, ReserveBytes: i.reserveBytes}
}

// cleanupPartial removes a destination a failed Create had begun to fill, so
// no candidate is ever handed a half-copied tree. The removal goes through
// the same containment check as Destroy, because this path runs precisely
// when something has already gone wrong.
func (i *Isolator) cleanupPartial(dst string, cause error) {
	if rmErr := i.remove("create", belay.Workspace{Dir: dst, Ephemeral: true}); rmErr != nil {
		i.log.Error("dircopy could not remove a partial candidate workspace",
			"dir", dst, "cause", cause, "error", rmErr)
	}
}

// Destroy removes the candidate workspace ws, which must be a direct child
// of this Isolator's candidate root.
//
// Destroy refuses, without deleting anything, any Workspace whose Dir is
// empty, relative, outside the root, not a direct child of it, a symlink, or
// resolves outside the root; and any Workspace with Ephemeral false, which
// this Isolator never produces. Removing a workspace that is already gone is
// not an error, so a Destroy replayed from a run journal after a crash
// succeeds instead of failing on the cleanup it already did.
//
// Destroy does not follow symlinks inside the workspace: a link to somewhere
// outside is unlinked, and whatever it pointed at is untouched.
func (i *Isolator) Destroy(ctx context.Context, ws belay.Workspace) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("dircopy: destroy %q: %w", ws.ID, err)
	}
	if err := i.remove("destroy", ws); err != nil {
		return err
	}
	if ws.ID != "" {
		i.mu.Lock()
		delete(i.stats, ws.ID)
		i.mu.Unlock()
	}
	return nil
}

// remove is the single place in this package that deletes a tree. Every
// caller — Destroy and the partial-copy cleanup — goes through it, so the
// containment check cannot be bypassed by adding a caller.
func (i *Isolator) remove(op string, ws belay.Workspace) error {
	dir, err := i.ownedPath(op, ws, true)
	if err != nil {
		i.log.Warn("dircopy refused to delete a path outside its candidate root",
			"op", op, "dir", ws.Dir, "root", i.root, "error", err)
		return err
	}
	if dir == "" {
		return nil // already gone
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("dircopy: %s %q: %w", op, dir, err)
	}
	return nil
}

// ownedPath validates that ws names a candidate directory this Isolator may
// act on, and returns the path to act on.
//
// It returns ("", nil) when the path is legitimate but does not exist, which
// callers treat as "nothing to do". It returns a *ContainmentError wrapping
// ErrRefused for everything else, having touched nothing.
//
// The order matters. Lexical containment is checked first, against the
// path as given, so a path that escapes the root is refused whether or not
// it exists. Only then is the path resolved with EvalSymlinks and checked
// again, which is what catches a directory that sits inside the root but
// resolves outside it. Checking only the resolved path would let a
// non-existent escaping path through; checking only the lexical path would
// let a symlink through.
func (i *Isolator) ownedPath(op string, ws belay.Workspace, mustExist bool) (string, error) {
	refuse := func(reason, resolved string) error {
		return &ContainmentError{Op: op, Path: ws.Dir, Resolved: resolved, Root: i.root, Reason: reason}
	}

	switch {
	case ws.Dir == "":
		return "", refuse("workspace Dir is empty", "")
	case strings.ContainsRune(ws.Dir, 0):
		return "", refuse("workspace Dir contains a NUL byte", "")
	case !ws.Ephemeral:
		// Create always sets Ephemeral true, so a false one is a Workspace
		// from some other Isolator (or a hand-built struct). Refusing is not
		// pedantry: belay.Workspace documents Ephemeral false as "Dir is the
		// user's own tree, do not remove it", and that is the one directory
		// this package must never touch.
		return "", refuse("workspace is not ephemeral, so it was not created by this isolator", "")
	case !filepath.IsAbs(ws.Dir):
		return "", refuse("workspace Dir is not an absolute path", "")
	}

	clean := filepath.Clean(ws.Dir)
	if !isChildOf(i.root, clean) {
		return "", refuse("not a direct child of the candidate root", "")
	}

	info, err := os.Lstat(clean)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if mustExist {
			return "", nil // legitimate, already gone
		}
		return clean, nil
	case err != nil:
		return "", fmt.Errorf("dircopy: %s %q: %w", op, clean, err)
	case info.Mode()&fs.ModeSymlink != 0:
		return "", refuse("workspace Dir is a symlink", "")
	case !info.IsDir():
		return "", refuse("workspace Dir is not a directory", "")
	}

	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("dircopy: %s %q: resolve: %w", op, clean, err)
	}
	resolved = filepath.Clean(resolved)
	if !isChildOf(i.root, resolved) {
		return "", refuse("resolves outside the candidate root", resolved)
	}
	return resolved, nil
}

// claim reserves id for one in-flight Create, so two concurrent calls cannot
// both decide an empty destination is theirs and then copy into it at once.
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

// resolveSource validates src and returns it absolute and symlink-resolved.
func (i *Isolator) resolveSource(src string) (string, error) {
	if strings.TrimSpace(src) == "" {
		return "", &SourceError{Path: src, Reason: "source must not be empty"}
	}
	if strings.ContainsRune(src, 0) {
		return "", &SourceError{Path: src, Reason: "source contains a NUL byte"}
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", &SourceError{Path: src, Reason: "cannot be made absolute"}
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", &SourceError{Path: src, Reason: fmt.Sprintf("cannot be resolved: %v", err)}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", &SourceError{Path: src, Reason: fmt.Sprintf("cannot be read: %v", err)}
	}
	if !info.IsDir() {
		return "", &SourceError{Path: src, Reason: "is not a directory"}
	}
	return filepath.Clean(resolved), nil
}

// checkOverlap rejects the two source/destination arrangements that cannot
// work.
//
// dst inside src is not one of them: the common real configuration puts the
// candidate root under the repository being copied, at .belay/candidates,
// and that is handled by excluding the destination and the root from the
// walk rather than by refusing the copy. Without that exclusion this is
// where a copy recurses into its own output until the disk fills, which is
// why the walk's exclusion is structural and not merely the skip list
// happening to contain ".belay".
func checkOverlap(src, dst string) error {
	switch {
	case src == dst:
		return &SourceError{Path: src, Reason: "source and destination are the same directory"}
	case isAncestorOf(dst, src):
		return &SourceError{Path: src, Reason: "source is inside the destination directory"}
	default:
		return nil
	}
}

// validateID rejects any candidate id that could name something other than a
// direct child of the candidate root.
//
// internal/state applies the same rules to the same ids in
// Layout.CandidateDir. They are restated rather than imported because this
// package takes a root as a parameter and does not depend on internal/state,
// and because a containment rule that only holds when a second package is
// also correct is not a containment rule.
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
	default:
		return nil
	}
}

// prepareDest creates the destination directory, or accepts an existing
// empty one, and refuses everything else.
//
// Refusing a non-empty destination is a correctness rule, not tidiness:
// copying over an existing tree blends two sets of sources and then presents
// the mixture to an agent as a clean checkout of one of them.
func prepareDest(dst string) error {
	info, err := os.Lstat(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if mkErr := os.Mkdir(dst, 0o750); mkErr != nil {
			return fmt.Errorf("dircopy: create destination %q: %w", dst, mkErr)
		}
		return nil
	case err != nil:
		return fmt.Errorf("dircopy: inspect destination %q: %w", dst, err)
	case info.Mode()&fs.ModeSymlink != 0:
		return &ContainmentError{
			Op: "create", Path: dst, Root: filepath.Dir(dst),
			Reason: "destination is a symlink",
		}
	case !info.IsDir():
		return &NotEmptyError{Dir: dst}
	}

	// #nosec G304 -- dst is built by this package from its own resolved
	// candidate root and an id validateID has already accepted, and it is
	// opened only to count its entries.
	f, err := os.Open(dst)
	if err != nil {
		return fmt.Errorf("dircopy: inspect destination %q: %w", dst, err)
	}
	defer func() { _ = f.Close() }()

	names, err := f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("dircopy: inspect destination %q: %w", dst, err)
	}
	if len(names) > 0 {
		return &NotEmptyError{Dir: dst}
	}
	return nil
}

// isChildOf reports whether path is a direct child of root: inside it, and
// exactly one segment below.
//
// The direct-child rule is stronger than containment alone and it is what
// Destroy enforces, because every workspace this package creates is a direct
// child of the root. It shrinks the window a symlink swap between the check
// and the removal could exploit from any depth of the tree to the single
// component being removed.
//
// Both arguments must already be absolute and cleaned.
func isChildOf(root, path string) bool {
	if !isAncestorOf(root, path) {
		return false
	}
	return filepath.Dir(path) == root
}

// isAncestorOf reports whether path lies strictly inside root.
//
// The check is by whole path segments, which is the entire point: a plain
// strings.HasPrefix says "/a/bc" is inside "/a/b" and it is not — they are
// unrelated siblings, and treating one as inside the other is how a deletion
// escapes its boundary. Requiring the separator, and independently
// confirming with filepath.Rel that the relative path does not climb, gives
// two ways of reaching the same answer; both must agree.
//
// A path equal to root is not inside it. Nothing in this package should ever
// delete or populate the candidate root itself.
func isAncestorOf(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return false
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
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
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+sep) || filepath.IsAbs(rel) {
		return false
	}
	return true
}
