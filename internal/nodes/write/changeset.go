package write

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// changeKind classifies one verified member of a change set.
type changeKind int

const (
	// changePresent means the claimed path is a regular file on disk. It is
	// deliberately not called "modified": without a baseline this node cannot
	// know whether the content differs from anything, only that the file the
	// agent named is there and is a file.
	changePresent changeKind = iota + 1
	// changeAbsent means the claimed path is not on disk. That is how a
	// deletion presents, and it is recorded rather than refused — an agent
	// that deletes a file is doing its job.
	changeAbsent
)

// change is one verified member of a change set.
type change struct {
	// Rel is the path relative to the workspace root, cleaned and
	// slash-separated, as it is recorded in state and rendered in the patch.
	Rel string
	// Abs is the absolute path Rel denotes. It has passed safeJoin.
	Abs string
	// Kind is what was found at Abs.
	Kind changeKind
	// Mode is the file's permission bits, zero when Kind is changeAbsent.
	Mode fs.FileMode
	// Size is the file's size in bytes, zero when Kind is changeAbsent.
	Size int64
}

// verify turns the paths a change set claims into a sorted, deduplicated set
// of verified changes, refusing the whole set if any single path cannot be
// proven to name a regular file inside root.
//
// Refusing the whole set rather than dropping the offending entry is
// deliberate. A path that escapes the workspace is not noise to be filtered;
// it is evidence that the change set does not describe what happened, and a
// run that continued on a filtered version of it would be recording a
// half-truth as fact.
//
// Sorting and deduplicating is what makes the artifact this feeds
// byte-identical across re-runs: map iteration order and a duplicated path
// would otherwise both show up as a different diff for an unchanged tree.
//
// root must already have come from resolveRoot.
func verify(ctx context.Context, root *os.Root, rootPath string, claimed []string) ([]change, error) {
	seen := make(map[string]struct{}, len(claimed))
	out := make([]change, 0, len(claimed))

	for _, raw := range claimed {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("write: verify change set: %w", err)
		}
		abs, err := safeJoin(rootPath, raw)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(rootPath, abs)
		if err != nil {
			return nil, fmt.Errorf("write: relativize %q: %w", raw, err)
		}
		rel = filepath.ToSlash(rel)
		if _, dup := seen[rel]; dup {
			continue
		}
		seen[rel] = struct{}{}

		c, err := classify(root, rootPath, rel, abs, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, nil
}

// classify inspects abs and reports what kind of change it represents.
//
// It refuses anything that is neither a regular file nor absent. A directory,
// a device node, a socket or a FIFO in a change set is not a code change, and
// the node has no way to render one into a patch; treating such an entry as
// benign would mean recording a change set that does not describe files.
// Symlinks never reach here — safeJoin refuses them first, because following
// one is how a diff artifact ends up quoting a file outside the workspace.
func classify(root *os.Root, rootPath, rel, abs, claimed string) (change, error) {
	// Statted through the Root, not by absolute path: the answer is then
	// about a path resolved inside the workspace in the kernel, which is the
	// same path the later read will resolve, rather than about whatever an
	// absolute string happens to name by the time it is used.
	info, err := root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return change{Rel: rel, Abs: abs, Kind: changeAbsent}, nil
	case err != nil:
		// An os.Root refusal ("path escapes from parent") lands here. It is a
		// refusal, not an I/O failure: the kernel disagreed with safeJoin
		// about containment, and the safe reading of that disagreement is
		// that the path is not contained.
		if isEscape(err) {
			return change{}, refuseEscape(claimed, rootPath, "while inspecting it")
		}
		return change{}, fmt.Errorf("write: inspect %q: %w", rel, err)
	case info.IsDir():
		return change{}, &ContainmentError{Path: claimed, Root: rootPath, Reason: "is a directory, not a file"}
	case !info.Mode().IsRegular():
		return change{}, &ContainmentError{
			Path: claimed, Root: rootPath,
			Reason: fmt.Sprintf("is not a regular file (mode %s)", info.Mode().Type()),
		}
	}
	return change{
		Rel:  rel,
		Abs:  abs,
		Kind: changePresent,
		Mode: info.Mode().Perm(),
		Size: info.Size(),
	}, nil
}

// isEscape reports whether err is an os.Root containment refusal rather than
// an operating-system failure.
//
// os.Root signals an escape with an unexported error value that carries no
// sentinel identity, so errors.Is cannot match it, and its message ("path
// escapes from parent") is not covered by any compatibility promise — a
// future Go release may reword it, and a check built on the text would then
// silently start reporting escapes as disk errors.
//
// What the refusal does have is a shape no kernel failure shares: a
// *fs.PathError whose wrapped error is not a syscall.Errno. Every error the
// operating system itself produces for these calls — ENOENT, EACCES, EIO —
// is an Errno; os.Root's own refusal is the only one that is not. Matching on
// that distinction keeps a broken disk from being recorded as a security
// refusal, and vice versa.
func isEscape(err error) bool {
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		return false
	}
	var errno syscall.Errno
	return !errors.As(pe.Err, &errno)
}

// refuseEscape builds the refusal recorded when os.Root — the containment
// layer safeJoin cannot substitute for — declines to resolve a path inside
// the workspace.
func refuseEscape(claimed, rootPath, when string) error {
	return &ContainmentError{
		Path:   claimed,
		Root:   rootPath,
		Reason: "the kernel refused to resolve it inside the workspace root " + when,
	}
}

// paths returns the change set's workspace-relative paths, in order, for
// state.Code.ChangedFiles.
//
// The result is always non-nil, so an empty change set serializes as [] and
// not null — matching what internal/state guarantees for its own slice
// fields, and keeping "the agent changed nothing" distinguishable from "this
// field was never populated" for anything reading state.json later.
func paths(changes []change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Rel)
	}
	return out
}
