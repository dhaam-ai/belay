package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// filePerm is the permission every file this package writes is created
// with: readable and writable by its owner only. A run directory can hold
// an agent's raw prompts and responses, so it defaults to private rather
// than the more common 0644.
const filePerm fs.FileMode = 0o600

// dirPerm is the permission every directory this package creates is
// created with.
const dirPerm fs.FileMode = 0o750

// ErrInvalidPathSegment reports a caller-supplied path component — a run
// ID, artifact name, node name, or candidate ID — that could place a file
// outside the run directory it is meant to be confined to.
var ErrInvalidPathSegment = errors.New("state: invalid path segment")

// PathSegmentError explains why one path segment was rejected. It wraps
// ErrInvalidPathSegment, so errors.Is(err, ErrInvalidPathSegment) detects
// it generically and errors.As recovers Kind and Segment for a precise
// message.
type PathSegmentError struct {
	// Kind names what the segment was meant to be, such as "run id",
	// "artifact name", "node name" or "candidate id".
	Kind string
	// Segment is the offending value as given.
	Segment string
	// Reason is a short, human-readable explanation of what disqualified
	// Segment.
	Reason string
}

// Error implements error.
func (e *PathSegmentError) Error() string {
	return fmt.Sprintf("state: invalid %s %q: %s", e.Kind, e.Segment, e.Reason)
}

// Unwrap reports ErrInvalidPathSegment.
func (e *PathSegmentError) Unwrap() error { return ErrInvalidPathSegment }

// validateSegment reports whether segment is safe to use as exactly one
// path component under a run directory.
//
// It is deliberately conservative and never tries to repair its input:
// anything that could plausibly let a caller escape the run directory —
// path separators, "..", an absolute path, an embedded NUL — is rejected
// outright. Sanitizing instead of rejecting would let two different,
// unsafe inputs collapse onto the same "cleaned" directory, silently
// merging two runs or two candidates.
func validateSegment(kind, segment string) error {
	switch {
	case segment == "":
		return &PathSegmentError{Kind: kind, Segment: segment, Reason: "must not be empty"}
	case strings.ContainsAny(segment, "/\\"):
		return &PathSegmentError{Kind: kind, Segment: segment, Reason: "must not contain a path separator"}
	case strings.Contains(segment, ".."):
		return &PathSegmentError{Kind: kind, Segment: segment, Reason: `must not contain ".."`}
	case filepath.IsAbs(segment):
		return &PathSegmentError{Kind: kind, Segment: segment, Reason: "must not be an absolute path"}
	case strings.ContainsRune(segment, 0):
		return &PathSegmentError{Kind: kind, Segment: segment, Reason: "must not contain a NUL byte"}
	default:
		return nil
	}
}

// joinSafe validates segment with validateSegment and joins it onto base.
//
// After joining, it independently re-derives the relative path from base to
// the result and confirms it does not climb out of base. This second check
// is defense in depth: it means a future gap in validateSegment's blocklist
// cannot, by itself, let a path escape base.
func joinSafe(kind, base, segment string) (string, error) {
	if err := validateSegment(kind, segment); err != nil {
		return "", err
	}
	joined := filepath.Join(base, segment)
	rel, err := filepath.Rel(base, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &PathSegmentError{Kind: kind, Segment: segment, Reason: "escapes its parent directory"}
	}
	return joined, nil
}

// atomicWriteStage identifies one step of atomicWriteFileUpTo's write
// sequence, in the order they execute. atomic_test.go uses it to prove
// that stopping at any prefix of the sequence — simulating a crash at
// exactly that point — never leaves the target path holding a truncated or
// otherwise torn file: it holds either its complete old content or its
// complete new content.
type atomicWriteStage int

const (
	stageTempCreated atomicWriteStage = iota + 1
	stageDataWritten
	stageTempSynced
	stageTempClosed
	stageRenamed
	stageDirSynced
)

// atomicWriteFile durably and atomically replaces the file at path with
// data.
//
// It writes to a temporary file in the same directory as path, fsyncs that
// file, closes it, renames it over path, and finally fsyncs the directory.
// Every one of those steps matters:
//
//   - Writing to a temporary file first, rather than to path directly,
//     means a reader can never observe a half-written path: right up until
//     the rename, path still holds whatever it held before this call.
//   - fsyncing the temporary file before renaming means the rename can
//     never expose content that has not actually reached disk.
//   - rename(2) is atomic on both of belay's target platforms (macOS,
//     Linux) as long as both paths are on the same filesystem, which they
//     always are here: the temporary file is created in path's own
//     directory.
//   - fsyncing the directory after the rename is what makes the rename
//     itself durable. Without it, a crash could leave the directory entry
//     still pointing at the old inode even though the rename appeared to
//     succeed — the one step in this sequence that is easy to skip and
//     easy to regret.
//
// atomicWriteFile creates path's directory (and any missing parents) if it
// does not already exist.
func atomicWriteFile(path string, data []byte, perm fs.FileMode) error {
	return atomicWriteFileUpTo(path, data, perm, stageDirSynced)
}

// atomicWriteFileUpTo performs atomicWriteFile's sequence but returns
// immediately after completing stage stop, without executing any later
// stage. Production code always calls it (via atomicWriteFile) with
// stop == stageDirSynced, running the full sequence; atomic_test.go calls
// it directly with earlier stages to simulate a crash at each point.
func atomicWriteFileUpTo(path string, data []byte, perm fs.FileMode, stop atomicWriteStage) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("state: create directory %s: %w", dir, err)
	}

	// #nosec G304 -- dir is derived from path, which every caller in this
	// package builds through Layout; Layout refuses to construct a path
	// outside the run directory (see validateSegment and joinSafe).
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("state: create temp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	closed := false
	renamed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()
	if stop == stageTempCreated {
		return nil
	}

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("state: write %s: %w", tmpPath, err)
	}
	if stop == stageDataWritten {
		return nil
	}

	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("state: sync %s: %w", tmpPath, err)
	}
	if stop == stageTempSynced {
		return nil
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close %s: %w", tmpPath, err)
	}
	closed = true
	if stop == stageTempClosed {
		return nil
	}

	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("state: set permissions on %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("state: install %s: %w", path, err)
	}
	renamed = true
	if stop == stageRenamed {
		return nil
	}

	if err := syncDir(dir); err != nil {
		return fmt.Errorf("state: sync directory %s: %w", dir, err)
	}
	return nil
}

// syncDir fsyncs a directory so that changes to its entries — such as the
// rename in atomicWriteFileUpTo — are durable, not merely visible to
// processes that have not crashed yet.
func syncDir(dir string) error {
	// #nosec G304 -- dir is always derived from a path this package built
	// through Layout.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// readFile reads the file at path, wrapping any error with path for
// context.
func readFile(path string) ([]byte, error) {
	// #nosec G304 -- path is always constructed by this package's own
	// Layout, which refuses to build a path outside the run directory.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("state: read %s: %w", path, err)
	}
	return data, nil
}

// writeJSON marshals v as indented JSON and atomically writes it to path
// with permission perm.
func writeJSON(path string, v any, perm fs.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode %s: %w", path, err)
	}
	data = append(data, '\n')
	return atomicWriteFile(path, data, perm)
}
