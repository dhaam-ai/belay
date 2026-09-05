//go:build fixrate

package fixrate

import (
	"os"
	"path/filepath"
	"sync"
)

// golangciLintCacheEnv is the environment variable golangci-lint reads for
// its own result cache directory (see internal/linter/golangci.go's
// golangciEnv, which allowlists it through to the child process).
const golangciLintCacheEnv = "GOLANGCI_LINT_CACHE"

// golangciCacheMu serializes the lifetime of every Run call's
// GOLANGCI_LINT_CACHE override.
//
// # Why this exists
//
// fixtures/seeded-bug/inject.go writes the SAME hardcoded module path
// (belay.dev/fixtures/seededbug/injected) into every injected copy, no
// matter how many temporary directories Run creates across however many
// calls. golangci-lint's own result cache is keyed in a way that was
// observed, empirically, to leak a stale absolute file path from an
// EARLIER Run's already-deleted temp directory into a LATER Run's lint
// output — surfaced as a spurious, unresolvable finding that the review
// gate (internal/nodes/review, driven by the identical unmodified belay
// this package measures) then fails on forever, since neither this
// harness's agent nor a real one could ever address an issue attributed
// to a file that no longer exists. The dispatcher's own graph.max_steps
// guard is what eventually stops such a run — correctly, as designed —
// but "aborted: max steps exceeded" is a false negative for a defect set
// this package's own agent genuinely repaired, which the determinism
// check (same seed, same defect set, same result) surfaced directly.
//
// The fix is to never let two Run calls share a golangci-lint cache:
// each call points GOLANGCI_LINT_CACHE at a fresh directory under its own
// temp work directory before belay's review node (or this package's own
// post-run lint check) ever runs golangci-lint, and restores the
// previous value before returning. Because that is a process-wide
// environment variable — this package has no way to scope a child
// process's environment more narrowly without editing internal/linter,
// which is out of T42's scope — golangciCacheMu serializes the override's
// entire lifetime across concurrent Run calls in one process, rather than
// letting two calls' cache directories race on the same variable. Run is
// already a multi-second-to-multi-minute operation; serializing this
// slice of it is a correctness trade this package makes deliberately,
// not an oversight.
var golangciCacheMu sync.Mutex

// withIsolatedGolangciCache points GOLANGCI_LINT_CACHE at dir/.golangci-
// cache for the duration of fn, restoring the previous value (or
// unsetting the variable, if it was unset before) once fn returns. See
// golangciCacheMu's doc comment for why this exists and why it is
// process-wide.
func withIsolatedGolangciCache(dir string, fn func() error) error {
	golangciCacheMu.Lock()
	defer golangciCacheMu.Unlock()

	cacheDir := filepath.Join(dir, ".golangci-cache")
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return err
	}

	prev, hadPrev := os.LookupEnv(golangciLintCacheEnv)
	if err := os.Setenv(golangciLintCacheEnv, cacheDir); err != nil {
		return err
	}
	defer func() {
		if hadPrev {
			_ = os.Setenv(golangciLintCacheEnv, prev)
		} else {
			_ = os.Unsetenv(golangciLintCacheEnv)
		}
	}()

	return fn()
}
