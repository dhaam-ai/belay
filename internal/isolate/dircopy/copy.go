//go:build unix

package dircopy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// copyBufferSize is the chunk size the file copier reads and writes in.
//
// It is also the granularity at which a copy notices a cancelled context, so
// it trades throughput against how promptly a cancelled run stops touching
// the disk. 256 KiB is large enough that the syscall overhead is noise on a
// tree of ordinary source files and small enough that even a multi-gigabyte
// file aborts promptly.
const copyBufferSize = 256 << 10

// Stats records what one Create actually did, so the cost ADR-0004 accepted
// for fanout is a number someone can read rather than an invisible drain.
// Read it back with Isolator.Stats.
type Stats struct {
	// Dirs is the number of directories created in the copy, including the
	// workspace root itself.
	Dirs int
	// Files is the number of regular files copied.
	Files int
	// Symlinks is the number of symlinks recreated in the copy. None of them
	// was followed.
	Symlinks int
	// BytesCopied is the total number of bytes of regular-file content
	// written. It excludes directory and symlink overhead, so it is a floor
	// on the disk the workspace occupies, not an exact figure.
	BytesCopied int64
	// Skipped is the number of entries the skip list omitted. A skipped
	// directory counts once and its contents are never walked — which is the
	// whole point, since not walking node_modules is most of the saving — so
	// this is a count of skip decisions, not of files that would have been
	// copied.
	Skipped int
	// SkippedSymlinks is the number of symlinks not recreated: under the
	// default SymlinkInternal policy, those whose target escapes the
	// workspace. A non-zero value here on an unexpected tree is worth
	// looking at, since it means the source relies on links this workspace
	// does not have.
	SkippedSymlinks int
	// Irregular is the number of entries that were neither a directory, a
	// regular file nor a symlink — sockets, FIFOs, devices — which are
	// skipped because they cannot be meaningfully copied.
	Irregular int
	// Duration is how long the Create that produced this Stats took,
	// including the pre-flight size estimate.
	Duration time.Duration
}

// copier carries the state of one tree copy.
type copier struct {
	// src and dst are absolute, cleaned, symlink-resolved directories.
	src, dst string
	// skip holds base names omitted at any depth.
	skip map[string]struct{}
	// exclude holds absolute paths never descended into, whatever the skip
	// list says. It always contains the destination and the candidate root,
	// which is what stops a copy whose destination lives inside its own
	// source from recursing into its own output until the disk fills.
	exclude map[string]struct{}
	policy  SymlinkPolicy
	log     *slog.Logger
	stats   Stats
}

// estimate sums the size of every regular file the copy would write, using
// the same skip and exclusion rules as the copy itself so the two cannot
// disagree.
//
// This costs a second walk of the tree, stat-only. That is cheap next to
// reading and writing every byte, and it buys the disk pre-check a real
// number instead of a guess.
func (c *copier) estimate(ctx context.Context) (int64, error) {
	var total int64
	err := filepath.WalkDir(c.src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if path == c.src {
			return nil
		}
		if skip, isDir := c.omits(path, d); skip {
			if isDir {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// A file that vanished between the walk listing it and this stat
			// is not a reason to refuse the copy; the copy itself will make
			// its own decision about it.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if size := info.Size(); size > 0 {
			total += size
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("dircopy: measure source %q: %w", c.src, err)
	}
	return total, nil
}

// run copies the tree. Symlinks are never followed: filepath.WalkDir uses
// lstat, so a link to a directory is reported as a link and is never
// descended into.
func (c *copier) run(ctx context.Context) error {
	buf := make([]byte, copyBufferSize)
	c.stats.Dirs++ // the workspace root, created by prepareDest

	return filepath.WalkDir(c.src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if path == c.src {
			return nil
		}
		if skip, isDir := c.omits(path, d); skip {
			c.stats.Skipped++
			if isDir {
				return fs.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(c.src, path)
		if err != nil {
			return fmt.Errorf("relative path of %q: %w", path, err)
		}
		target := filepath.Join(c.dst, rel)

		switch {
		case d.IsDir():
			return c.copyDir(path, target, d)
		case d.Type()&fs.ModeSymlink != 0:
			return c.copySymlink(path, target)
		case d.Type().IsRegular():
			return c.copyFile(ctx, path, target, d, buf)
		default:
			// Sockets, FIFOs, block and character devices. Skipping them is
			// not only about fidelity: opening a FIFO for reading blocks
			// until someone opens the write end, so a copier that treated
			// one as an ordinary file would hang the whole run on it.
			c.stats.Irregular++
			c.log.Debug("dircopy skipped an irregular file", "path", path, "mode", d.Type().String())
			return nil
		}
	})
}

// omits reports whether path is excluded from the copy, and whether it is a
// directory (so the caller can prune the walk rather than merely skip an
// entry).
func (c *copier) omits(path string, d fs.DirEntry) (skip, isDir bool) {
	if _, excluded := c.exclude[filepath.Clean(path)]; excluded {
		return true, d.IsDir()
	}
	if _, listed := c.skip[d.Name()]; listed {
		return true, d.IsDir()
	}
	return false, d.IsDir()
}

// copyDir recreates a directory.
//
// The mode is the source's, but always with owner rwx. Preserving a mode
// like 0555 exactly would produce a workspace this process cannot finish
// populating and, worse, one Destroy could not remove — trading a directory
// bit nothing reads for a candidate tree that leaks and cannot be cleaned
// up. Group and other bits are preserved, so the copy is never more open to
// anyone else than the source was.
func (c *copier) copyDir(src, target string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return fmt.Errorf("stat %q: %w", src, err)
	}
	if err := os.Mkdir(target, info.Mode().Perm()|0o700); err != nil {
		return fmt.Errorf("create directory %q: %w", target, err)
	}
	c.stats.Dirs++
	return nil
}

// copyFile copies one regular file's content and permissions.
func (c *copier) copyFile(ctx context.Context, src, target string, d fs.DirEntry, buf []byte) error {
	info, err := d.Info()
	if err != nil {
		return fmt.Errorf("stat %q: %w", src, err)
	}

	// O_NOFOLLOW closes the gap between the walk deciding this entry is a
	// regular file and this open: if it has become a symlink in between, the
	// open fails rather than following it out of the tree.
	//
	// #nosec G304 -- src is an entry the walk of the caller-designated source
	// tree produced, opened read-only and without following symlinks; a
	// copier that may not open variable paths cannot copy anything.
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open %q: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	// Mode().Perm() keeps only the 0777 bits, which is exactly the intended
	// policy: setuid, setgid and the sticky bit are separate bits in
	// fs.FileMode and are therefore dropped here, not carried into a tree an
	// autonomous agent is about to run commands in. O_EXCL means this never
	// writes through a symlink or an existing file.
	//
	// #nosec G304 -- target is built from the validated destination and the
	// walk's own relative path, and O_EXCL refuses anything already there.
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create %q: %w", target, err)
	}

	written, copyErr := copyContents(ctx, out, in, buf)
	c.stats.BytesCopied += written
	if copyErr != nil {
		_ = out.Close()
		return fmt.Errorf("copy %q: %w", src, copyErr)
	}
	// Chmod on the open descriptor rather than the path: it cannot be
	// redirected by a swap after the write, and it defeats the umask, so an
	// executable file in the source is still executable in the copy.
	if err := out.Chmod(info.Mode().Perm()); err != nil {
		_ = out.Close()
		return fmt.Errorf("set mode on %q: %w", target, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %q: %w", target, err)
	}
	// Preserving mtime keeps build caches and test runners that compare
	// timestamps from treating every file in every candidate as new work.
	if err := os.Chtimes(target, time.Time{}, info.ModTime()); err != nil {
		return fmt.Errorf("set times on %q: %w", target, err)
	}

	c.stats.Files++
	return nil
}

// copySymlink applies the SymlinkPolicy to one symlink. It reads the link
// text; it never resolves or follows it.
func (c *copier) copySymlink(src, target string) error {
	link, err := os.Readlink(src)
	if err != nil {
		return fmt.Errorf("read link %q: %w", src, err)
	}

	switch c.policy {
	case SymlinkSkip:
		c.stats.SkippedSymlinks++
		return nil
	case SymlinkInternal:
		if c.escapes(target, link) {
			c.stats.SkippedSymlinks++
			c.log.Debug("dircopy omitted a symlink pointing outside the workspace",
				"path", src, "target", link)
			return nil
		}
	case SymlinkPreserveAll:
		// Recreated verbatim below, escaping or not.
	}

	if err := os.Symlink(link, target); err != nil {
		return fmt.Errorf("create link %q: %w", target, err)
	}
	c.stats.Symlinks++
	return nil
}

// escapes reports whether a symlink placed at target, pointing at link,
// would refer to something outside the workspace.
//
// The decision is lexical, from the link text and the link's own location in
// the copy, because the tree is still being built and the target need not
// exist yet. An absolute target always escapes. A relative one is resolved
// against the link's directory and must land inside the workspace, or on the
// workspace root itself.
//
// Because every preserved link is individually contained this way, no chain
// of them can leave the workspace either: a link that would have provided
// the escaping hop was itself omitted when it was reached.
func (c *copier) escapes(target, link string) bool {
	if filepath.IsAbs(link) {
		return true
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(target), link))
	return resolved != c.dst && !isAncestorOf(c.dst, resolved)
}

// copyContents streams from in to out, checking ctx every chunk so a
// cancelled run stops mid-file instead of at the next file boundary.
func copyContents(ctx context.Context, out io.Writer, in io.Reader, buf []byte) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			written, writeErr := out.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// freeSpaceOn reports the bytes available to this user on the filesystem
// holding path.
//
// It reports the unprivileged figure (Bavail), not the total free figure,
// because the reserved blocks a filesystem keeps for root are exactly the
// space this copy will not be allowed to use. The result is clamped to
// MaxInt64 so an implausible or corrupt statfs cannot wrap the comparison it
// feeds into and turn a full disk into an empty one.
func freeSpaceOn(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", path, err)
	}

	// Bsize is int64 on Linux and uint32 on Darwin; widening to int64 first
	// is lossless on both and lets the guard below prove the conversion to
	// uint64 is safe.
	blockSize := int64(st.Bsize)
	if blockSize <= 0 {
		return 0, fmt.Errorf("statfs %q: implausible block size %d", path, blockSize)
	}
	bsize, bavail := uint64(blockSize), uint64(st.Bavail)
	if bavail > math.MaxUint64/bsize {
		return math.MaxInt64, nil
	}
	if free := bavail * bsize; free <= uint64(math.MaxInt64) {
		return int64(free), nil
	}
	return math.MaxInt64, nil
}
