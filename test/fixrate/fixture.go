//go:build fixrate

package fixrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ErrFixtureNotFound reports that a candidate fixture directory does not
// look like fixtures/seeded-bug (see looksLikeFixtureDir).
var ErrFixtureNotFound = errors.New("fixrate: fixtures/seeded-bug not found")

// looksLikeFixtureDir reports whether dir contains the files this package
// depends on being present: inject.go (the CLI this package shells out
// to), bugs/catalogue.go (the defect catalogue it applies) and src/ (the
// known-good tree it repairs from — see agent.go). Checking all three
// rather than just one guards against a half-checked-out or renamed
// fixture failing confusingly deep inside a subprocess call instead of
// here, up front, with a clear message.
func looksLikeFixtureDir(dir string) bool {
	for _, rel := range []string{"inject.go", filepath.Join("bugs", "catalogue.go"), "src"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			return false
		}
	}
	return true
}

// DefaultFixtureDir locates fixtures/seeded-bug starting from this source
// file's own location and walking up to the repository root.
//
// It uses runtime.Caller rather than the process's working directory
// because `go test` always runs with the package directory as its working
// directory anyway — this only matters for a caller importing the package
// from somewhere else (a future CLI command) that has not set RunOptions.
// FixtureDir explicitly.
func DefaultFixtureDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("fixrate: cannot resolve this package's own source location")
	}
	// This file lives at <repo>/test/fixrate/fixture.go.
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	dir := filepath.Join(repoRoot, "fixtures", "seeded-bug")
	if !looksLikeFixtureDir(dir) {
		return "", fmt.Errorf("%w: %s does not look like fixtures/seeded-bug "+
			"(missing inject.go, bugs/catalogue.go, or src/)", ErrFixtureNotFound, dir)
	}
	return dir, nil
}

// resolveFixtureDir returns opts.FixtureDir if set (validated), otherwise
// DefaultFixtureDir's result.
func resolveFixtureDir(configured string) (string, error) {
	if configured == "" {
		return DefaultFixtureDir()
	}
	abs, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("fixrate: resolving fixture dir %q: %w", configured, err)
	}
	if !looksLikeFixtureDir(abs) {
		return "", fmt.Errorf("%w: %s does not look like fixtures/seeded-bug "+
			"(missing inject.go, bugs/catalogue.go, or src/)", ErrFixtureNotFound, abs)
	}
	return abs, nil
}
