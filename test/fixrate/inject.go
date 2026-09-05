//go:build fixrate

package fixrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// injectTimeout bounds one `go run ./inject.go` call. Injection builds the
// copy, runs `go build`, `go test -race -json` and (opportunistically)
// golangci-lint against it — see fixtures/seeded-bug/README.md — which is
// the same work inject_test.go budgets roughly 3 minutes for across its
// whole suite; one defect set is far smaller than that, but the ceiling
// still has to cover a cold module cache on a fresh temp directory.
const injectTimeout = 3 * time.Minute

// Selection chooses which catalogued defects one Run measures. Exactly one
// of Bugs or Random must be set — this mirrors fixtures/seeded-
// bug/inject.go's own --bugs/--random mutual exclusion deliberately, since
// this package's whole job is to hand those same semantics to that same
// CLI.
type Selection struct {
	// Bugs is an explicit, comma-free list of defect IDs, e.g.
	// []string{"B01", "B05"}. Mutually exclusive with Random.
	Bugs []string

	// Random is a count of defects to select by seeded random permutation
	// of the full catalogue, sorted by ID first — see inject.go's
	// randomDefects. Mutually exclusive with Bugs; requires Seed.
	Random int

	// Seed seeds the permutation Random draws from. Required whenever
	// Random > 0: there is no silent default, so that a --random
	// selection is always reproducible from the seed alone (requirement:
	// same seed, same defect set, same result).
	Seed int64
}

// ErrInvalidSelection reports a Selection that is not exactly one of "an
// explicit bug list" or "a seeded random count".
var ErrInvalidSelection = errors.New("fixrate: invalid defect selection")

// validate reports whether s names exactly one selection mode.
func (s Selection) validate() error {
	switch {
	case len(s.Bugs) > 0 && s.Random > 0:
		return fmt.Errorf("%w: Bugs and Random are mutually exclusive", ErrInvalidSelection)
	case len(s.Bugs) == 0 && s.Random == 0:
		return fmt.Errorf("%w: specify either Bugs or Random (with Seed)", ErrInvalidSelection)
	case s.Random < 0:
		return fmt.Errorf("%w: Random must be >= 0, got %d", ErrInvalidSelection, s.Random)
	default:
		return nil
	}
}

// args renders s as fixtures/seeded-bug/inject.go's own flag spellings.
func (s Selection) args() []string {
	if s.Random > 0 {
		return []string{"--random=" + strconv.Itoa(s.Random), "--seed=" + strconv.FormatInt(s.Seed, 10)}
	}
	return []string{"--bugs=" + strings.Join(s.Bugs, ",")}
}

// label renders a short, stable, filesystem-safe description of s, used to
// name temp directories and to label a Result.
func (s Selection) label() string {
	if s.Random > 0 {
		return fmt.Sprintf("random-%d-seed-%d", s.Random, s.Seed)
	}
	ids := make([]string, len(s.Bugs))
	copy(ids, s.Bugs)
	return "bugs-" + strings.ToLower(strings.Join(ids, "-"))
}

// InjectError reports that `go run ./inject.go` did not produce a copy
// this harness can measure against — either the process itself failed
// with no injected.json to show for it, or injected.json says the copy
// did not behave exactly as its defects are catalogued to. Both are
// pre-conditions this harness refuses to paper over: a fix-rate number
// computed against a fixture that is not behaving as documented would not
// mean what it claims to.
type InjectError struct {
	// Selection is the request that failed.
	Selection Selection
	// Output is inject.go's combined stdout+stderr, for diagnosis.
	Output string
	// Manifest is the injected.json that was written, if any could be
	// read. A process failure before injected.json is written (a bad
	// --bugs ID, an unreadable --src) leaves this nil.
	Manifest *InjectedManifest
	// Err is the underlying cause.
	Err error
}

// Error implements error.
func (e *InjectError) Error() string {
	if e.Manifest != nil {
		return fmt.Sprintf("fixrate: injecting %s did not behave as catalogued (missing=%v unexpected=%v lint_unmatched=%v): %v",
			e.Selection.label(), e.Manifest.Verification.MissingFailingTests,
			e.Manifest.Verification.UnexpectedFailingTests, e.Manifest.Verification.LintUnmatchedDefects, e.Err)
	}
	return fmt.Sprintf("fixrate: injecting %s: %v\n--- inject.go output ---\n%s", e.Selection.label(), e.Err, e.Output)
}

// Unwrap reports the underlying cause.
func (e *InjectError) Unwrap() error { return e.Err }

// injectDefects shells out to `go run ./inject.go` in fixtureDir to
// materialize sel into a fresh copy at outDir, and returns the parsed
// injected.json.
//
// outDir is cleared and recreated by inject.go itself (checkSafeDestination
// / os.RemoveAll — see fixtures/seeded-bug/inject.go); this function does
// not pre-create it.
//
// A non-nil error is always *InjectError: the copy either could not be
// produced at all, or was produced but does not behave exactly as its
// selected defects are catalogued to (a bad combination the caller chose,
// or — extremely unlikely, since every defect is individually verified —
// a catalogue bug). Either way, injectDefects refuses to hand back a
// manifest this harness could be misled by.
func injectDefects(ctx context.Context, fixtureDir, outDir string, sel Selection) (*InjectedManifest, error) {
	if err := sel.validate(); err != nil {
		return nil, &InjectError{Selection: sel, Err: err}
	}

	ctx, cancel := context.WithTimeout(ctx, injectTimeout)
	defer cancel()

	args := append([]string{"run", "./inject.go"}, sel.args()...)
	args = append(args, "--out="+outDir)

	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // fixed subcommand ("go run ./inject.go"); the only variable parts are outDir (this harness's own temp dir) and sel's fields, which are constrained to defect IDs / integers by Selection.args
	cmd.Dir = fixtureDir
	cmd.Env = injectEnv()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	manifest, readErr := readInjectedManifest(outDir)
	if manifest == nil {
		err := runErr
		if err == nil {
			err = readErr
		}
		return nil, &InjectError{Selection: sel, Output: out.String(), Err: fmt.Errorf("no injected.json produced: %w", err)}
	}
	if !manifest.Verification.OK() {
		return manifest, &InjectError{Selection: sel, Output: out.String(), Manifest: manifest,
			Err: errors.New("injected.json verification did not pass")}
	}
	return manifest, nil
}

// injectEnv is the subprocess environment for `go run ./inject.go`: the
// parent's own environment (PATH, HOME, GOPATH/GOCACHE/etc. — inject.go
// needs a real Go toolchain and module cache) verbatim. There is nothing
// here to redact: this is belay's own build tooling talking to its own
// fixture module, not a target repository's untrusted environment.
func injectEnv() []string { return os.Environ() }

// readInjectedManifest reads and parses <outDir>/injected.json, which
// inject.go writes on every run, success or failure (see its README).
func readInjectedManifest(outDir string) (*InjectedManifest, error) {
	path := filepath.Join(outDir, "injected.json")
	data, err := os.ReadFile(path) //nolint:gosec // outDir is this harness's own temp directory, never external input
	if err != nil {
		return nil, err
	}
	var m InjectedManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &m, nil
}
