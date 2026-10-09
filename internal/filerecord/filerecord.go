// Package filerecord records what every file in a workspace was, so that
// what a run changed can be read off the disk afterwards rather than taken
// from what the agent says it changed.
//
// It is the ground truth for a workspace git cannot answer for: a directory
// that is not a git repository, a copy belay made for a queued goal or a
// fanout candidate (which leaves .git out), and, recording only some paths,
// the files a person had already changed in a git checkout before a run
// began. internal/git answers for everything else, and the two give the same
// kind of answer: every path that differs, whoever reported what.
//
// # Content, not timestamps
//
// A file is recorded by its kind, its permission bits and the SHA-256 of its
// bytes (or, for a symbolic link, the text it points to), and a later check
// compares those. A check by size and modification time alone, which is what
// a fingerprint taken before every node can afford, cannot be ground truth
// against an agent that is misreporting on purpose: writing bytes of the
// same length and putting the modification time back with `touch -r` is two
// lines of shell, and the edit reads as no edit.
//
// What keeps a later check cheap is the inode number and the status-change
// time, which the record also keeps. The kernel moves a file's ctime
// whenever its contents, its mode or its timestamps change, and no process
// without root can set it, so a file whose inode and ctime are both as
// recorded has not been touched, and is not read again. Everything else is
// read and hashed, so a file rewritten with the bytes it already had — a
// formatter, a touch — is not reported as changed. A platform where the
// ctime cannot be read hashes every file every time, which is slower and
// exactly as correct.
//
// # What a record leaves out
//
// belay's own .belay directory at the top of the workspace, and any .git,
// file or directory, at any depth: neither is part of a change. Directories
// are not recorded, only what is in them, so an empty directory is not a
// change, as it is not one to git. Sockets, FIFOs and devices are left out,
// as a directory copy leaves them out.
//
// Nothing else is left out. A workspace without git has no ignore rules to
// consult, so a file a test run writes into the tree is part of the change,
// and a later check reports it. That is a real cost for a tree whose tests
// write into it, and the honest one: a list that skipped build directories
// by name would let an agent hide an edit under vendor/ or build/.
//
// # Reading the workspace
//
// Every read goes through an os.Root on the workspace, so a symbolic link is
// recorded and never followed, and nothing the agent made can point a check
// at a file outside the tree.
package filerecord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Scheme names this package's record format. A record with any other
// scheme is refused rather than compared, because two schemes can disagree
// about an untouched file.
const Scheme = "files1"

// Kinds of entry.
const (
	kindFile    = "file"
	kindSymlink = "symlink"
	kindAbsent  = "absent"
)

// Entry is what one path held when the record was taken.
type Entry struct {
	// Kind is "file", "symlink", or "absent" for a path a partial record
	// was asked about that did not exist.
	Kind string `json:"kind"`
	// Perm is the permission bits of a file.
	Perm uint32 `json:"perm,omitempty"`
	// Digest is the SHA-256 of a file's bytes, in hex, or for a symbolic
	// link the text it points to.
	Digest string `json:"digest,omitempty"`
	// Inode and Ctime, in nanoseconds, identify the inode as the record
	// found it; see the package documentation. Zero when the platform
	// cannot say.
	Inode uint64 `json:"inode,omitempty"`
	Ctime int64  `json:"ctime,omitempty"`
}

// Record is what a workspace's files were when it was taken.
type Record struct {
	// Scheme is the format: see Scheme.
	Scheme string `json:"scheme"`
	// Complete is true for a record of the whole workspace, so that a file
	// it does not hold is a new one. A partial record answers only for the
	// paths it holds.
	Complete bool `json:"complete"`
	// Entries maps a slash-separated path, relative to the workspace, to
	// what it held.
	Entries map[string]Entry `json:"entries"`
}

// Take records the workspace at dir: every file in it when only is nil, or
// exactly the paths in only, relative to dir, recording a path that does not
// exist as absent.
func Take(ctx context.Context, dir string, only []string) (Record, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Record{}, fmt.Errorf("filerecord: open the workspace %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	rec := Record{Scheme: Scheme, Complete: only == nil, Entries: map[string]Entry{}}
	paths := only
	if only == nil {
		if paths, err = walk(ctx, dir); err != nil {
			return Record{}, err
		}
	}
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return Record{}, fmt.Errorf("filerecord: %w", err)
		}
		rel = clean(rel)
		e, ok, err := read(root, rel)
		if err != nil {
			return Record{}, err
		}
		if !ok {
			if rec.Complete {
				continue // vanished between the walk and the read
			}
			e = Entry{Kind: kindAbsent}
		}
		rec.Entries[rel] = e
	}
	return rec, nil
}

// Paths returns the paths the record holds, sorted.
func (r Record) Paths() []string {
	return slices.Sorted(maps.Keys(r.Entries))
}

// Changed returns every path, relative to dir and sorted, whose kind, mode
// or contents differ from the record: for a complete record, every file
// added, removed or altered anywhere in the workspace; for a partial one,
// every recorded path that is no longer what it was.
func (r Record) Changed(ctx context.Context, dir string) ([]string, error) {
	if r.Scheme != Scheme {
		return nil, fmt.Errorf("filerecord: a record in scheme %q cannot be read by this belay, which reads %q", r.Scheme, Scheme)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("filerecord: open the workspace %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	candidates := r.Paths()
	if r.Complete {
		now, err := walk(ctx, dir)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, now...)
		slices.Sort(candidates)
		candidates = slices.Compact(candidates)
	}

	var changed []string
	for _, rel := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("filerecord: %w", err)
		}
		was, recorded := r.Entries[rel]
		if !recorded {
			was = Entry{Kind: kindAbsent}
		}
		same, err := unchanged(root, rel, was)
		if err != nil {
			return nil, err
		}
		if !same {
			changed = append(changed, rel)
		}
	}
	return changed, nil
}

// Save writes the record to path, replacing the file whole: a record cut
// short by a crash would read as every missing file deleted.
func (r Record) Save(path string) error {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("filerecord: encode: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("filerecord: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("filerecord: put %s in place: %w", path, err)
	}
	return nil
}

// Load reads a record Save wrote. A record that is missing or unreadable is
// an error, never an empty record: an empty complete record would report
// every file as new, and an empty partial one would report nothing.
func Load(path string) (Record, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a path inside belay's own run directory
	if err != nil {
		return Record{}, fmt.Errorf("filerecord: read %s: %w", path, err)
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, fmt.Errorf("filerecord: decode %s: %w", path, err)
	}
	if r.Scheme != Scheme {
		return Record{}, fmt.Errorf("filerecord: %s is in scheme %q, and this belay reads %q", path, r.Scheme, Scheme)
	}
	if r.Entries == nil {
		r.Entries = map[string]Entry{}
	}
	return r, nil
}

// walk lists every file and symbolic link under dir, relative to it, leaving
// out .belay at the top and .git at any depth. Symbolic links are listed and
// never followed.
func walk(ctx context.Context, dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil // removed while the walk was under way
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.Name() == ".git" || rel == ".belay" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() || d.Type()&fs.ModeSymlink != 0 {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("filerecord: list %s: %w", dir, err)
	}
	return out, nil
}

// read records rel, reporting false for a path that is not there or is not
// a file or a symbolic link.
func read(root *os.Root, rel string) (Entry, bool, error) {
	info, err := root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Entry{}, false, nil
	case err != nil:
		return Entry{}, false, fmt.Errorf("filerecord: inspect %s: %w", rel, err)
	}
	inode, ctime := identity(info)
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := root.Readlink(rel)
		if err != nil {
			return Entry{}, false, fmt.Errorf("filerecord: read the link %s: %w", rel, err)
		}
		return Entry{Kind: kindSymlink, Digest: target, Inode: inode, Ctime: ctime}, true, nil
	case info.Mode().IsRegular():
		digest, err := hashFile(root, rel)
		if err != nil {
			return Entry{}, false, err
		}
		return Entry{Kind: kindFile, Perm: uint32(info.Mode().Perm()), Digest: digest, Inode: inode, Ctime: ctime}, true, nil
	default:
		return Entry{}, false, nil
	}
}

// unchanged reports whether rel still holds what was recorded.
func unchanged(root *os.Root, rel string, was Entry) (bool, error) {
	info, err := root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return was.Kind == kindAbsent, nil
	case err != nil:
		return false, fmt.Errorf("filerecord: inspect %s: %w", rel, err)
	}
	// The kernel's word that nothing touched the inode since the record.
	if inode, ctime := identity(info); inode != 0 && inode == was.Inode && ctime == was.Ctime {
		return true, nil
	}
	now, ok, err := read(root, rel)
	if err != nil {
		return false, err
	}
	if !ok {
		now = Entry{Kind: kindAbsent}
	}
	return now.Kind == was.Kind && now.Perm == was.Perm && now.Digest == was.Digest, nil
}

// hashFile returns the SHA-256 of rel's bytes, in hex.
func hashFile(root *os.Root, rel string) (string, error) {
	f, err := root.Open(rel)
	if err != nil {
		return "", fmt.Errorf("filerecord: read %s: %w", rel, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("filerecord: read %s: %w", rel, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// clean normalises a path the way a record keys it.
func clean(rel string) string {
	return strings.TrimPrefix(path.Clean(filepath.ToSlash(rel)), "./")
}
