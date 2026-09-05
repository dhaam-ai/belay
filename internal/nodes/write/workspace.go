package write

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/belay-dev/belay/internal/state"
)

// Sentinel errors this package returns. Match them with errors.Is; the two
// that carry structured detail also have a companion type recoverable with
// errors.As — see ContainmentError and WorkspaceError.
var (
	// ErrRefused reports a path this node declined to touch because it
	// could not be proven to lie inside the workspace root, or because it
	// is not the kind of filesystem object a code change may consist of.
	//
	// It is deliberately distinct from an I/O failure: a refusal means the
	// change set itself is untrustworthy — every path in it ultimately
	// comes from model output — and the correct response is to stop the
	// run, not to retry it.
	ErrRefused = errors.New("write: refused a path outside the workspace")

	// ErrWorkspace reports that the workspace root itself could not be
	// established: it does not exist, it is not a directory, or it could
	// not be derived from the run Layout.
	ErrWorkspace = errors.New("write: workspace root is unusable")
)

// ContainmentError explains why one claimed path was refused. It wraps
// ErrRefused.
type ContainmentError struct {
	// Path is the offending path exactly as the change set claimed it,
	// before any cleaning.
	Path string
	// Resolved is the symlink-resolved absolute path, when the refusal
	// happened late enough to have one. Empty otherwise.
	Resolved string
	// Root is the workspace root the path had to stay inside.
	Root string
	// Reason is a short, human-readable explanation.
	Reason string
}

// Error implements error.
func (e *ContainmentError) Error() string {
	if e.Resolved != "" {
		return fmt.Sprintf("write: refused %q (resolves to %q, outside %q): %s",
			e.Path, e.Resolved, e.Root, e.Reason)
	}
	return fmt.Sprintf("write: refused %q (outside %q): %s", e.Path, e.Root, e.Reason)
}

// Unwrap reports ErrRefused.
func (e *ContainmentError) Unwrap() error { return ErrRefused }

// WorkspaceError explains why a workspace root was rejected. It wraps
// ErrWorkspace.
type WorkspaceError struct {
	// Dir is the directory that was rejected, as given.
	Dir string
	// Reason is a short, human-readable explanation.
	Reason string
}

// Error implements error.
func (e *WorkspaceError) Error() string {
	return fmt.Sprintf("write: workspace root %q: %s", e.Dir, e.Reason)
}

// Unwrap reports ErrWorkspace.
func (e *WorkspaceError) Unwrap() error { return ErrWorkspace }

// belayDirName is belay's own directory inside a workspace. Nothing under it
// is ever part of an agent's change set: it holds the run's control state,
// and a change set naming it is either a confused agent or an attempt to
// rewrite the record of what happened.
const belayDirName = ".belay"

// resolveRoot returns dir as an absolute, cleaned, symlink-resolved path
// after confirming it is an existing directory.
//
// Resolving symlinks here, once, is what lets every later containment check
// compare two already-resolved absolute paths rather than a resolved path
// against an unresolved one — a comparison that silently succeeds for the
// wrong reason when the workspace itself sits behind a link, as it does on
// macOS where /tmp is a symlink to /private/tmp.
func resolveRoot(dir string) (string, error) {
	switch {
	case strings.TrimSpace(dir) == "":
		return "", &WorkspaceError{Dir: dir, Reason: "must not be empty"}
	case strings.ContainsRune(dir, 0):
		return "", &WorkspaceError{Dir: dir, Reason: "must not contain a NUL byte"}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", &WorkspaceError{Dir: dir, Reason: "cannot be made absolute"}
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", &WorkspaceError{Dir: dir, Reason: fmt.Sprintf("cannot be resolved: %v", err)}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", &WorkspaceError{Dir: dir, Reason: fmt.Sprintf("cannot be read: %v", err)}
	}
	if !info.IsDir() {
		return "", &WorkspaceError{Dir: dir, Reason: "is not a directory"}
	}
	return filepath.Clean(resolved), nil
}

// openRoot resolves dir and opens it as an os.Root.
//
// The Root is the second of this package's two independent containment
// mechanisms, and it is the one that closes the gap the first cannot. safeJoin
// decides, by inspecting paths, that a claim is contained — but between that
// decision and the read that follows it, the workspace belongs to an agent
// process that may still be running, and a file checked as regular can become
// a symlink out of the tree before it is opened. An os.Root resolves every
// path against the directory itself, in the kernel, refusing any traversal
// that escapes it ("path escapes from parent") with no window in between.
//
// Both mechanisms are kept. The lexical gate rejects a hostile claim before
// any syscall is made, produces the typed refusal the run needs to report, and
// enforces rules the kernel knows nothing about, such as .belay being
// off-limits. The Root makes the guarantee hold even if the gate is wrong.
func openRoot(dir string) (*os.Root, string, error) {
	resolved, err := resolveRoot(dir)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, "", &WorkspaceError{Dir: dir, Reason: fmt.Sprintf("cannot be opened: %v", err)}
	}
	return root, resolved, nil
}

// rootFromLayout derives the workspace root from a run Layout.
//
// state.NewLayout builds a run directory as <workspace>/.belay/runs/<run id>,
// so the workspace is three levels up. The derivation is verified rather than
// assumed: the two intermediate components must actually be named "runs" and
// ".belay", so a Layout built some other way fails loudly here instead of

// safeJoin turns one workspace-relative path claimed by the change set into
// the absolute path it denotes inside root, refusing anything that cannot be
// proven to stay there.
//
// The order of the checks is the point. Lexical rules run first, against the
// path as claimed, so an escape is refused whether or not the target exists —
// a check that only ran against something on disk would let a path that
// escapes to a not-yet-created file through. Only then is the deepest
// existing ancestor resolved with EvalSymlinks and checked again, which is
// what catches a path that is lexically innocent but sits under a directory
// symlinked out of the tree. Finally the leaf itself is refused if it is a
// symlink: a link is not a content change, and following one is how a diff
// artifact ends up quoting a file the run was never allowed to read.
//
// root must already have come from resolveRoot.
func safeJoin(root, rel string) (string, error) {
	refuse := func(reason, resolved string) error {
		return &ContainmentError{Path: rel, Resolved: resolved, Root: root, Reason: reason}
	}
	switch {
	case rel == "":
		return "", refuse("must not be empty", "")
	case strings.TrimSpace(rel) == "":
		return "", refuse("must not be only whitespace", "")
	case strings.ContainsRune(rel, 0):
		return "", refuse("must not contain a NUL byte", "")
	case filepath.IsAbs(rel):
		return "", refuse("must not be an absolute path", "")
	case strings.Contains(rel, `\`):
		// Backslash is a path separator on one of Go's target platforms and
		// a legal filename byte on the others, so a path containing one
		// means different things depending on where it is interpreted.
		// Refusing is cheaper than being right about which.
		return "", refuse("must not contain a backslash", "")
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", refuse(`must not contain a ".." segment`, "")
		}
	}

	abs := filepath.Clean(filepath.Join(root, rel))
	if !isWithin(root, abs) {
		return "", refuse("does not lie inside the workspace root", "")
	}
	if inside, relErr := filepath.Rel(root, abs); relErr == nil && isBelayPath(inside) {
		return "", refuse("names belay's own run directory", "")
	}

	// Resolve the deepest existing ancestor. The leaf itself may legitimately
	// not exist — that is how a deletion presents — so resolution starts at
	// its parent, and a parent that does not exist yet cannot be a symlink
	// out of the tree either.
	if parent, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		parent = filepath.Clean(parent)
		if parent != root && !isWithin(root, parent) {
			return "", refuse("its parent directory resolves outside the workspace root", parent)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("write: resolve parent of %q: %w", rel, err)
	}

	info, err := os.Lstat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return abs, nil
	case err != nil:
		return "", fmt.Errorf("write: inspect %q: %w", rel, err)
	case info.Mode()&fs.ModeSymlink != 0:
		return "", refuse("is a symlink", "")
	}
	return abs, nil
}

// isBelayPath reports whether a workspace-relative path names belay's own
// run directory or anything under it.
func isBelayPath(rel string) bool {
	rel = filepath.Clean(rel)
	return rel == belayDirName || strings.HasPrefix(rel, belayDirName+string(filepath.Separator))
}

// isWithin reports whether path lies strictly inside root.
//
// The comparison is by whole path segments, which is the entire point: a
// plain strings.HasPrefix says "/a/bc" is inside "/a/b", and it is not — they
// are unrelated siblings, and treating one as inside the other is how a write
// escapes the tree it was bounded to. Requiring the separator and
// independently confirming with filepath.Rel that the relative path does not
// climb gives two ways of reaching the same answer; both must agree.
//
// A path equal to root is not inside it: the workspace root itself is never a
// member of a change set.
//
// Both arguments must already be absolute and cleaned.
func isWithin(root, path string) bool {
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
	return rel != ".." && !strings.HasPrefix(rel, ".."+sep)
}
