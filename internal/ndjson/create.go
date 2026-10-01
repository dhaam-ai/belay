package ndjson

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrExists reports that CreateNew found something already at the path it
// was asked to create, and so created nothing.
var ErrExists = errors.New("ndjson: file already exists")

// CreateNew durably creates the file at path holding exactly lines, each on
// its own newline-terminated line, if and only if nothing is at path yet. If
// anything is there — a file, an empty file, a directory, a symlink even to
// nowhere — it returns an error wrapping ErrExists and changes nothing.
//
// # Why link(2)
//
// The file has to appear whole or not at all, and must never replace
// anything. The obvious ways each give up one of the two:
//
//   - Opening path with O_CREATE|O_EXCL refuses an existing file, but the file
//     exists from the moment it is opened. A crash while the lines are being
//     written leaves a torn file at path, and every later attempt is refused
//     over it.
//   - Writing a temporary file and renaming it into place makes the file
//     appear whole, but rename(2) replaces whatever is at path. Checking first
//     does not help: another process can create path between the check and
//     the rename, and the rename silently destroys what it wrote.
//
// link(2) has both properties. It gives a complete, fsynced temporary file a
// second name in one atomic step, and fails with EEXIST when that name is
// taken by anything at all, a symlink included, which it never follows. So
// CreateNew writes a temporary file in path's own directory, fsyncs it, links
// it to path, removes the temporary name, and fsyncs the directory, so that
// the new entry survives a power cut and not only a process exit. Of several
// callers creating one path at once, in one process or many, exactly one
// succeeds; that is the only guarantee across processes this package makes,
// and it comes from the kernel, not from a lock.
//
// # What it leaves behind
//
// Nothing, on any failure it can see: the temporary file is removed. A crash
// before the link leaves a stray "<name>.create-*" and no file at path, so
// trying again creates it. A crash between the link and the removal leaves
// the same stray name beside a file that is already whole. Nothing reads
// either.
//
// # What it does not do
//
// It does not create path's directory: a missing directory is the caller's
// mistake to see, not one to paper over by making whatever the path names.
// It does not check that a line holds no newline, any more than AppendLine
// does. And it needs a filesystem with hard links; on one without them (FAT,
// some network filesystems) it returns link(2)'s own error rather than fall
// back to a weaker way of writing.
func CreateNew(path string, lines [][]byte, perm fs.FileMode) error {
	tmpPath, err := writeTemp(path, lines, perm)
	if err != nil {
		return fmt.Errorf("ndjson: create %s: %w", path, err)
	}
	linkErr := os.Link(tmpPath, path)
	// The temporary name goes whether or not the link was made: after it the
	// file lives on under path, and without it there is nothing to keep. A
	// name that cannot be removed is a stray file beside a whole one, which
	// is not a failure to create path, so it is not reported as one.
	_ = os.Remove(tmpPath)
	switch {
	case errors.Is(linkErr, fs.ErrExist):
		return fmt.Errorf("%w: %s", ErrExists, path)
	case linkErr != nil:
		return fmt.Errorf("ndjson: create %s: %w", path, linkErr)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("ndjson: create %s: sync directory: %w", path, err)
	}
	return nil
}

// writeTemp writes lines, each newline-terminated, to a new temporary file
// beside path, sets its permission to exactly perm, fsyncs and closes it, and
// returns its name. On any failure it removes what it wrote.
//
// The permission is set explicitly rather than left to the process umask, so
// a file belay keeps private is private whatever the shell it ran from.
func writeTemp(path string, lines [][]byte, perm fs.FileMode) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".create-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if err := fill(tmp, lines, perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("close %s: %w", name, err)
	}
	return name, nil
}

// fill writes lines to f, sets its permission and fsyncs it.
func fill(f *os.File, lines [][]byte, perm fs.FileMode) error {
	w := bufio.NewWriter(f)
	for i, line := range lines {
		if _, err := w.Write(line); err != nil {
			return fmt.Errorf("write line %d: %w", i+1, err)
		}
		if err := w.WriteByte('\n'); err != nil {
			return fmt.Errorf("write line %d: %w", i+1, err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write %s: %w", f.Name(), err)
	}
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("set permissions on %s: %w", f.Name(), err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", f.Name(), err)
	}
	return nil
}

// syncDir fsyncs a directory, which is what makes a change to its entries —
// the link CreateNew adds — durable rather than merely visible to processes
// that have not crashed yet.
func syncDir(dir string) error {
	// #nosec G304 -- dir is the directory of the path the caller named; this
	// package has no policy opinion about where a caller's file may live.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
