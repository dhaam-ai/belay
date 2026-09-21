//go:build unix

package dircopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
)

// surveyScheme prefixes every Survey.Digest. See SurveyTree for why a digest
// names its scheme.
const surveyScheme = "tree1:sha256:"

// Survey is what SurveyTree found in a tree.
type Survey struct {
	// Digest fingerprints the tree: surveyScheme followed by 64 hex digits.
	Digest string
	// Files is the number of regular files the survey recorded: the files a
	// copy of the tree would write.
	Files int
	// Bytes is the total size of those files.
	Bytes int64
}

// SurveyTree fingerprints the tree at dir as a copy of it would hold it: it
// walks the tree Create would copy, omitting the base names in skip at any
// depth as WithSkip does. Pass DefaultSkip() for what Create omits by default.
//
// It exists so a fork can tell whether a workspace has changed since a node
// ran in it. The dispatcher records a digest in the blackboard snapshot it
// takes before every node execution (graph.Options.Fingerprint), and a fork
// surveys the tree it is about to re-run in and compares. It lives here
// because this package owns what a copy contains; a digest taken over a
// different set of files would report every skipped node_modules write, or
// belay's own journal under .belay, as a change to the person's code.
//
// # Stat, not content
//
// The survey reads metadata only — one lstat per entry, and a readlink per
// symlink — and never opens a file. It runs once per node execution, and
// hashing every byte of a checkout that often would cost as much as copying
// it. So a regular file is recorded by its path, permission bits, size and
// modification time, and the trade has one false negative: content rewritten
// with its size kept and its modification time put back (by a tool that
// preserves timestamps, or within a filesystem's timestamp resolution) reads
// as unchanged. The false positive — a touch with no edit reads as a change —
// is the safe direction to be wrong in, since a fork asks rather than
// assumes.
//
// A directory is recorded by its path and permission bits, without its
// modification time, which moves whenever an entry appears inside it —
// including an entry skip omits, such as src/__pycache__. A symlink is
// recorded by its path and the text it points to, and is never followed, so
// a link to a directory elsewhere adds nothing it points at. Sockets, FIFOs
// and devices are left out, as Create leaves them out.
//
// # The scheme tag
//
// Every digest begins "tree1:sha256:". Anything that changes what a survey
// records — the skip list, the fields, their encoding — changes the digest of
// an untouched tree, and a reader comparing two digests must be able to tell
// "recorded by a different belay" from "your files changed". A new scheme
// gets a new tag, and a caller compares tags before digests.
func SurveyTree(ctx context.Context, dir string, skip []string) (Survey, error) {
	if err := ctx.Err(); err != nil {
		return Survey{}, fmt.Errorf("dircopy: survey %q: %w", dir, err)
	}
	root, err := resolveTree(dir)
	if err != nil {
		return Survey{}, err
	}
	skipped := make(map[string]struct{}, len(skip))
	for _, name := range skip {
		skipped[name] = struct{}{}
	}

	var (
		survey Survey
		digest = sha256.New()
	)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An entry that vanished between being listed and being read is
			// a tree changing under the survey, not a failure of it: the
			// digest records what was there.
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if _, skip := skipped[d.Name()]; skip {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		return survey.record(digest, root, path, d)
	})
	if err != nil {
		return Survey{}, fmt.Errorf("dircopy: survey %q: %w", root, err)
	}
	survey.Digest = surveyScheme + hex.EncodeToString(digest.Sum(nil))
	return survey, nil
}

// record adds one entry to the digest, and a regular file to the counts.
//
// Each entry is six NUL-terminated fields — kind, slash-separated relative
// path, permission bits, size, modification time in nanoseconds, link text —
// with the ones that do not apply left empty. A path or a link text cannot
// contain NUL, so no two different trees encode alike. filepath.WalkDir
// visits entries in lexical order, so the same tree always encodes the same
// way.
func (s *Survey) record(digest hash.Hash, root, path string, d fs.DirEntry) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("relative path of %q: %w", path, err)
	}
	rel = filepath.ToSlash(rel)

	switch {
	case d.Type()&fs.ModeSymlink != 0:
		link, err := os.Readlink(path)
		if err != nil {
			return ignoreVanished(fmt.Errorf("read link %q: %w", path, err))
		}
		return writeEntry(digest, "l", rel, "", "", "", link)
	case d.IsDir(), d.Type().IsRegular():
		info, err := d.Info()
		if err != nil {
			return ignoreVanished(fmt.Errorf("stat %q: %w", path, err))
		}
		perm := fmt.Sprintf("%o", info.Mode().Perm())
		if d.IsDir() {
			return writeEntry(digest, "d", rel, perm, "", "", "")
		}
		s.Files++
		s.Bytes += info.Size()
		return writeEntry(digest, "f", rel, perm,
			fmt.Sprint(info.Size()), fmt.Sprint(info.ModTime().UnixNano()), "")
	default:
		// Sockets, FIFOs and devices: a copy omits them, so the survey does.
		return nil
	}
}

// writeEntry writes one entry's fields to the digest, each NUL-terminated.
func writeEntry(digest hash.Hash, fields ...string) error {
	for _, f := range fields {
		if _, err := digest.Write(append([]byte(f), 0)); err != nil {
			return err
		}
	}
	return nil
}

// ignoreVanished drops an error that only says the entry has gone since the
// walk listed it (see SurveyTree).
func ignoreVanished(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
