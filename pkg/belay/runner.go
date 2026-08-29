package belay

import (
	"context"
	"encoding/json"
	"time"
)

// TestRunner adapts a language- or framework-specific test toolchain
// (go test, pytest, jest, ...) to the graph runner.
//
// A TestRunner is auto-detected: the dispatcher asks each registered
// TestRunner whether it recognizes a directory (Detect) and runs the match.
// pkg/belay defines only the contract — it ships no built-in runners;
// concrete runners live in adapter packages.
type TestRunner interface {
	// Name returns a short, stable, lowercase identifier, such as
	// "go-test" or "pytest". Name is used in TestReport provenance, logs,
	// and configuration that pins a specific runner instead of relying on
	// auto-detection.
	Name() string

	// Detect reports whether dir looks like a project this runner knows
	// how to test — for example the presence of a go.mod, a pytest.ini, or
	// a package.json with a "test" script. Detect must be side-effect
	// free: it must not execute the toolchain, must not modify dir, and
	// must not error — an inconclusive or unreadable directory is simply
	// false, never a panic.
	Detect(dir string) bool

	// Test runs the project's test suite rooted at dir and returns the
	// parsed result.
	//
	// If the underlying toolchain itself is not installed (no `go`, no
	// `pytest` on PATH, ...), Test returns a zero TestReport and an error
	// wrapping ErrToolchainMissing — never a TestReport claiming zero
	// tests ran, which a caller cannot distinguish from a genuinely empty
	// suite. A build or compile error that prevents any test from running
	// is not this case either: Test reports it as a TestReport with
	// exactly one synthesized TestFailure describing the build error (see
	// TestReport), so the "fix" node — which reads TestReport, not Go
	// errors — has something to act on.
	//
	// A test suite that runs to completion and fails is not a Go error at
	// all: Test returns (TestReport{Failed: n, ...}, nil) in that case,
	// with Failures describing what broke.
	Test(ctx context.Context, dir string) (TestReport, error)
}

// TestReport is the parsed result of one TestRunner.Test call.
//
// Total, Passed and Failed are independent counts, not required to sum:
// Total - Passed - Failed is the number of skipped or pending tests, which
// belay does not track individually. A runner that cannot distinguish
// passed from skipped should still report an accurate Failed and Total;
// nothing in belay computes Passed by subtracting Failed from Total, so an
// undercounted Passed is safe.
type TestReport struct {
	// Total is the number of tests the runner discovered and executed.
	// Total == 0 with a nil error means the suite ran cleanly but found no
	// tests to run — see OK, which treats that as not-passing.
	Total int `json:"total"`

	// Passed is the number of tests that succeeded.
	Passed int `json:"passed"`

	// Failed is the number of tests that failed. If the toolchain could
	// not even build/compile the target, Failed is 1 and Failures holds a
	// single synthesized entry describing the build error, even though no
	// individual test ran.
	Failed int `json:"failed"`

	// Failures describes each failing test. len(Failures) <= Failed:
	// equality is the common case, but a runner that learns a failure
	// count from a summary line without being able to parse every
	// individual failure may report fewer entries than Failed — never
	// more.
	Failures []TestFailure `json:"failures"`

	// Duration is the wall-clock time the test command took to run. Like
	// every time.Duration, it serializes as a plain integer count of
	// nanoseconds (encoding/json has no special case for the type) — not a
	// "5s"-style string.
	Duration time.Duration `json:"duration"`

	// Raw is the runner's unparsed output (for example a `go test -json`
	// stream or a pytest JSON report), preserved for debugging. The graph
	// itself never reads Raw.
	Raw json.RawMessage `json:"raw"`
}

// OK reports whether this report should be treated as a passing test run:
// zero failures and at least one test actually executed.
//
// A report with Total == 0 is deliberately not OK. A suite that discovered
// and ran no tests is far more often a misconfiguration — wrong directory,
// no matching test files, a filter that matched nothing — than a
// legitimately empty project, and treating it as green has silently broken
// enough CI pipelines that belay refuses to repeat the mistake. A
// TestRunner for a project that genuinely has zero tests by design should
// say so in its own documentation; the graph treats Total == 0 as
// "something needs a human", not "passed".
func (r TestReport) OK() bool {
	return r.Total > 0 && r.Failed == 0
}

// TestFailure describes one failing test.
type TestFailure struct {
	// Name is the test's identifier as the toolchain reports it, such as
	// "TestParseKind/empty" or "test_login.py::test_invalid_password". For
	// a synthesized build-error failure (see TestReport), Name is a
	// runner-chosen constant such as "build".
	Name string `json:"name"`

	// File is the path to the test's source file, relative to the
	// directory passed to TestRunner.Test. Empty if the runner cannot
	// determine it.
	File string `json:"file"`

	// Line is the 1-indexed line within File the failure was reported at.
	// Zero if unknown.
	Line int `json:"line"`

	// Message is the failure output — assertion message, stack trace,
	// panic text, or compiler error — as the toolchain produced it.
	Message string `json:"message"`
}
