package planarchive

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

var (
	// ErrOccupied reports a destination that already holds something other
	// than the file Place was asked to put there: different bytes, a
	// directory, a symbolic link. Place never replaces it.
	ErrOccupied = errors.New("planarchive: something else is already there")

	// ErrOutside reports a destination that is not inside the tree it was
	// meant to be in: an absolute path, a "..", or a symbolic link that
	// leads out.
	ErrOutside = errors.New("planarchive: the destination is outside the workspace")
)

// filePerm is the mode Place creates a file with, before the umask takes
// its share. belay's run files are 0600 because they hold an agent's raw
// prompts; an archived plan is the one file belay writes that is meant to be
// committed and read by other people and other tools, so it is made the way
// git makes the files of a checkout.
const filePerm fs.FileMode = 0o644

// dirPerm is the mode Place creates a missing directory with, for the same
// reason: docs/ is the repository's, not belay's.
const dirPerm fs.FileMode = 0o755

// tempPath is the file Place writes before linking it into place, beside
// the destination. It is named after the destination rather than chosen at
// random so that a re-run can recognise, and remove, the one a crashed
// attempt left behind; and it starts with a dot so the tools that publish a
// docs directory skip it in the moment it exists.
func tempPath(dest string) string {
	return filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".belay-tmp")
}

// Place makes root/rel hold data, and reports whether this call put it
// there. It is the one way this package writes into a repository.
//
// It never replaces anything. A file already holding exactly data is taken
// as the work of an earlier attempt that did not finish, and Place reports
// false and nil, which is what lets every caller of it converge when it is
// re-run after a crash. Anything else already there is ErrOccupied and is
// left exactly as it was: somebody put it there, and a plan is not worth
// their file.
//
// It never leaves the tree. rel is slash-separated and relative, and a rel
// that is absolute or holds a ".." segment is ErrOutside, whatever the
// configuration that produced it already checked. Every directory on the
// way that already exists is resolved through its symbolic links, and one
// that leads out of root is ErrOutside too, before anything is created,
// because only the disk knows where a link goes. A root reached through a
// link is fine; it is still the root.
//
// It is atomic and durable. The bytes are written to tempPath and fsynced,
// then hard-linked to the destination, which fails rather than replace
// anything that appeared there in the meantime, and then every directory
// from the destination's up to root is fsynced, so the file and each
// directory created for it survive a power cut before a caller removes the
// only other copy. A partial file is never visible at the destination, and
// the temporary file a crash leaves behind is removed by the next call. On a
// file system without hard links Place fails rather than fall back to a
// write that could leave a torn file where the plan should be.
func Place(root, rel string, data []byte) (bool, error) {
	top, dest, err := resolve(root, rel)
	if err != nil {
		return false, err
	}
	tmp := tempPath(dest)
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("planarchive: remove %s, left by an attempt that did not finish: %w", tmp, err)
	}
	switch held, err := holds(dest, data); {
	case err != nil:
		return false, err
	case held:
		return false, nil
	}

	dir := filepath.Dir(dest)
	// #nosec G301 -- a directory of the repository, such as docs/plans, made
	// the way the rest of it is; see dirPerm.
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return false, fmt.Errorf("planarchive: create %s: %w", dir, err)
	}
	if err := writeTemp(tmp, data); err != nil {
		return false, err
	}
	if err := os.Link(tmp, dest); err != nil {
		err = fmt.Errorf("planarchive: put %s in place: %w", dest, err)
		if rmErr := os.Remove(tmp); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("planarchive: remove %s: %w", tmp, rmErr))
		}
		if errors.Is(err, fs.ErrExist) {
			// Something arrived at dest after holds looked. It is judged
			// the way holds judges anything already there.
			held, heldErr := holds(dest, data)
			if held {
				return false, nil
			}
			return false, errors.Join(err, heldErr)
		}
		return false, err
	}
	if err := os.Remove(tmp); err != nil {
		return false, fmt.Errorf("planarchive: remove %s: %w", tmp, err)
	}
	if err := syncUp(dir, top); err != nil {
		return false, err
	}
	return true, nil
}

// resolve returns root with its symbolic links resolved, and root/rel
// beneath it, or ErrOutside when rel could lead anywhere else. See Place.
func resolve(root, rel string) (top, dest string, err error) {
	if rel == "" || path.IsAbs(rel) || slices.Contains(strings.Split(rel, "/"), "..") {
		return "", "", fmt.Errorf("%w: %q must be a relative path with no \"..\" in it", ErrOutside, rel)
	}
	top, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("planarchive: resolve %s: %w", root, err)
	}
	dest = filepath.Join(top, filepath.FromSlash(rel))

	// The deepest directory on the way that exists already is the one whose
	// links decide where everything created beneath it lands.
	dir := filepath.Dir(dest)
	for {
		_, err := os.Lstat(dir)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", "", fmt.Errorf("planarchive: look at %s: %w", dir, err)
		}
		dir = filepath.Dir(dir)
	}
	lands, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", fmt.Errorf("planarchive: resolve %s: %w", dir, err)
	}
	if lands != top && !strings.HasPrefix(lands, top+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%w: %s leads to %s, outside %s", ErrOutside, dir, lands, root)
	}
	return top, dest, nil
}

// holds reports whether dest is a regular file holding exactly data. Nothing
// there is false and nil; anything else there is ErrOccupied.
func holds(dest string, data []byte) (bool, error) {
	info, err := os.Lstat(dest)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("planarchive: look at %s: %w", dest, err)
	case !info.Mode().IsRegular():
		return false, fmt.Errorf("%w: %s is a %s, not a file", ErrOccupied, dest, kind(info.Mode()))
	case info.Size() != int64(len(data)):
		return false, fmt.Errorf("%w: %s holds something else", ErrOccupied, dest)
	}
	have, err := os.ReadFile(dest) //nolint:gosec // dest is under the resolved root; see resolve
	if err != nil {
		return false, fmt.Errorf("planarchive: read %s: %w", dest, err)
	}
	if !bytes.Equal(have, data) {
		return false, fmt.Errorf("%w: %s holds something else", ErrOccupied, dest)
	}
	return true, nil
}

// kind names what a non-regular file is, for an error a person reads.
func kind(mode fs.FileMode) string {
	switch {
	case mode.IsDir():
		return "directory"
	case mode&fs.ModeSymlink != 0:
		return "symbolic link"
	default:
		return "special file"
	}
}

// writeTemp writes data to tmp and fsyncs it, removing it again if any step
// fails. tmp must not exist: it is created exclusively, so two writers can
// never interleave in one file.
func writeTemp(tmp string, data []byte) error {
	// #nosec G302 G304 -- tmp is beside a destination resolve kept inside
	// the workspace, and the mode is an ordinary document's; see filePerm.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return fmt.Errorf("planarchive: create %s: %w", tmp, err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		return nil
	}
	err = fmt.Errorf("planarchive: write %s: %w", tmp, err)
	if rmErr := os.Remove(tmp); rmErr != nil {
		err = errors.Join(err, fmt.Errorf("planarchive: remove %s: %w", tmp, rmErr))
	}
	return err
}

// syncUp fsyncs dir and every directory above it up to and including top,
// so that each entry on the way to a new file is on the disk, including the
// entries for directories Place has just created.
func syncUp(dir, top string) error {
	for {
		if err := syncDir(dir); err != nil {
			return fmt.Errorf("planarchive: sync %s: %w", dir, err)
		}
		if dir == top || filepath.Dir(dir) == dir {
			return nil
		}
		dir = filepath.Dir(dir)
	}
}

// syncDir fsyncs one directory.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir is under the resolved root; see resolve
	if err != nil {
		return err
	}
	err = d.Sync()
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}
	return err
}
