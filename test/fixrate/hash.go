//go:build fixrate

package fixrate

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// snapshot is a content fingerprint of a directory tree at one point in
// time, used by the cheat detector (evaluate.go) to answer "did this file
// change between injection and the end of belay's run" without keeping
// full file copies around.
type snapshot struct {
	// testFiles maps every "*_test.go" path (relative to the tree root,
	// slash-separated) to its sha256 hex digest. Scanning every test file
	// rather than only ones a specific defect names is deliberate — see
	// evaluate.go's doc comment on why the cheat check is tree-wide.
	testFiles map[string]string
	// sourceFiles maps the specific non-test files the caller asked to be
	// tracked (ordinarily each selected defect's own target file) to
	// their sha256 hex digest.
	sourceFiles map[string]string
}

// hashFile returns the lowercase-hex sha256 digest of path's contents.
func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is always constructed by this package from a workspace it created, never external input
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// snapshotWorkspace hashes every "*_test.go" file under root, plus every
// path in sourceFiles (relative to root, slash-separated — ordinarily the
// InjectedDefect.File values of the defects under test).
func snapshotWorkspace(root string, sourceFiles []string) (snapshot, error) {
	snap := snapshot{
		testFiles:   make(map[string]string),
		sourceFiles: make(map[string]string, len(sourceFiles)),
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// The only place a belay run writes anything of its own under
			// the workspace is .belay/; its contents (journal, artifacts,
			// node evidence) are run bookkeeping, never target-repository
			// test files, and walking into it would make every run
			// "change" its own snapshot trivially.
			if d.Name() == ".belay" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum, err := hashFile(path)
		if err != nil {
			return err
		}
		snap.testFiles[filepath.ToSlash(rel)] = sum
		return nil
	})
	if err != nil {
		return snapshot{}, err
	}

	for _, rel := range sourceFiles {
		sum, err := hashFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return snapshot{}, err
		}
		snap.sourceFiles[rel] = sum
	}

	return snap, nil
}

// anyTestFileChanged reports whether any "*_test.go" file present in
// either snapshot has a different digest (or existed in only one of the
// two, i.e. was added or removed) between before and after.
func anyTestFileChanged(before, after snapshot) bool {
	for path, sum := range before.testFiles {
		if after.testFiles[path] != sum {
			return true
		}
	}
	for path := range after.testFiles {
		if _, ok := before.testFiles[path]; !ok {
			return true
		}
	}
	return false
}

// sourceChanged reports whether the tracked source file rel has a
// different digest between before and after. rel must have been included
// in both snapshotWorkspace calls' sourceFiles argument; a rel that is
// absent from either snapshot is treated as changed (fail safe: an
// untracked file cannot be vouched for as unchanged).
func sourceChanged(before, after snapshot, rel string) bool {
	b, ok := before.sourceFiles[rel]
	if !ok {
		return true
	}
	a, ok := after.sourceFiles[rel]
	if !ok {
		return true
	}
	return a != b
}
