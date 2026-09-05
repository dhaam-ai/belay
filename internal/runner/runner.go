//go:build unix

// Package runner adapts concrete test toolchains — go test, jest, vitest,
// node:test, pytest, or an operator-supplied command — to the
// [belay.TestRunner] contract the graph's test and fix nodes consume.
//
// # The one distinction that matters
//
// Everything in this package exists to keep three outcomes apart, because the
// graph branches on them and the remedies are unrelated:
//
//   - The suite ran and something failed. That is not a Go error. Test returns
//     (TestReport{Failed: n, ...}, nil) and the fix node edits code.
//   - The toolchain is not installed. Test returns an error satisfying
//     errors.Is(err, [belay.ErrToolchainMissing]) — a human installs software.
//   - The tests could not be run for some other reason (bad working directory,
//     a timeout, a tool that exited non-zero with nothing parseable). Test
//     returns a [*RunError], and the run stops.
//
// A build or compile failure is a fourth case that lives inside the first: the
// suite "ran" in the sense that the toolchain reported something actionable,
// but no assertion was evaluated. It surfaces as a TestReport carrying a
// synthesized failure named [BuildFailureName], which [IsBuildFailure]
// recognizes, so the fix node knows it is repairing a compile error rather
// than an assertion.
//
// # Error translation is a seam, not an accident
//
// [internal/exec] has its own ErrToolchainMissing sentinel, distinct from
// [belay.ErrToolchainMissing]. Returning an exec error unchanged from a
// TestRunner would make errors.Is(err, belay.ErrToolchainMissing) report
// false and silently route a missing toolchain into the fix loop, where an
// agent would be asked to edit code to make `go` exist. Every runner in this
// package therefore translates at its own boundary, via [ToolchainError], and
// every runner has a test proving it.
//
// # Subprocesses
//
// No runner calls os/exec. Every process starts through the [Execer] seam,
// which production code satisfies with *exec.Runner (no shell, deny-by-default
// environment, redacted capture) and tests satisfy with a fake that replays a
// captured fixture. That is what makes this package testable without any
// toolchain installed.
//
// # Environment
//
// exec passes HOME, LANG, PATH, TERM and TMPDIR to every child and nothing
// else. Each runner adds a short, fixed allowlist of the variables its
// toolchain needs to find its caches and proxies. Variables that inject flags
// into the tool being parsed — GOFLAGS, NODE_OPTIONS, PYTEST_ADDOPTS — are
// deliberately excluded: they can change the output format underneath the
// parser, turning a green suite into an unparseable one. The lists are not
// configurable in this version.
//
// # Platform
//
// Unix only, inherited from internal/exec, which needs Setpgid to kill a
// child's whole process group.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/detect"
	bexec "github.com/belay-dev/belay/internal/exec"
	"github.com/belay-dev/belay/pkg/belay"
)

// Execer runs one external command and returns its captured result.
//
// It is the seam every runner in this package uses instead of os/exec.
// *exec.Runner satisfies it in production; tests substitute a fake that
// returns fixture bytes without starting a process.
type Execer interface {
	// Run executes c. It returns a Result even when it returns an error, so
	// a caller can inspect the exit code and captured output either way.
	Run(ctx context.Context, c bexec.Command) (bexec.Result, error)
}

// The production Execer is the landed exec.Runner; this assertion is what
// keeps the seam honest if either signature moves.
var _ Execer = (*bexec.Runner)(nil)

// Compile-time proof that every runner in this package satisfies the contract
// the graph consumes.
var (
	_ belay.TestRunner = (*Go)(nil)
	_ belay.TestRunner = (*Node)(nil)
	_ belay.TestRunner = (*Pytest)(nil)
	_ belay.TestRunner = (*Custom)(nil)
)

// BuildFailureName is the [belay.TestFailure.Name] a runner uses for a
// synthesized compile or build failure — a failure that stopped any test from
// running, rather than a test that ran and failed.
//
// Match it with [IsBuildFailure] rather than comparing strings at call sites.
const BuildFailureName = "build"

// IsBuildFailure reports whether r describes a build or compile failure rather
// than tests that ran and failed.
//
// The fix node needs this: repairing a compile error and repairing a failed
// assertion are different jobs, and a report whose Failures are all assertion
// messages is useless input for the first. A report can contain both — one
// broken package in a module whose other packages tested fine — in which case
// IsBuildFailure is true and Failures holds the compiler output alongside the
// real test failures.
//
// Only the Go runner synthesizes build failures. A pytest collection error or
// a Node module-load error surfaces as an ordinary failure naming the file
// that would not import; see each runner's documentation.
func IsBuildFailure(r belay.TestReport) bool {
	for _, f := range r.Failures {
		if f.Name == BuildFailureName {
			return true
		}
	}
	return false
}

// RunError reports that a test command could not be run to completion, so no
// TestReport could be produced.
//
// It is deliberately not a test failure and deliberately not a missing
// toolchain: errors.Is(err, [belay.ErrToolchainMissing]) is false for a
// RunError. It means the graph cannot learn anything about the code from this
// node — a working directory that does not exist, a deadline that elapsed, a
// tool that exited non-zero having printed nothing a parser could use.
type RunError struct {
	// Runner is the [belay.TestRunner.Name] of the adapter that failed.
	Runner string
	// Args is the redacted argv that was run, program first. It may be nil
	// if the failure happened before the command was assembled.
	Args []string
	// ExitCode is the process exit status, or -1 if it never ran.
	ExitCode int
	// Stderr is the redacted tail of the command's standard error.
	Stderr string
	// Err is the underlying cause, typically from internal/exec.
	Err error
}

// Error implements error.
func (e *RunError) Error() string {
	msg := fmt.Sprintf("belay/runner: %s: could not run tests", e.Runner)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + lastLines(s, 3)
	}
	return msg
}

// Unwrap returns the underlying cause.
func (e *RunError) Unwrap() error { return e.Err }

// ErrNoProject reports that a runner was pointed at a directory that does not
// hold the kind of project it knows how to test.
//
// It is a [RunError] cause rather than a toolchain problem: the toolchain may
// be perfectly healthy and simply aimed at the wrong directory.
var ErrNoProject = errors.New("belay/runner: no project of the expected kind at this directory")

// ErrNoCommand reports that a runner had nothing to run — a custom runner
// configured with an empty command, or a Node package with neither a test
// script nor a recognizable framework.
var ErrNoCommand = errors.New("belay/runner: no test command to run")

// base is the state every runner shares: where to start processes and where to
// log. It is embedded, not inherited — the runners have nothing else in common.
type base struct {
	exec Execer
	log  *slog.Logger
}

// newBase resolves the nil defaults once, so no runner has to.
func newBase(x Execer, logger *slog.Logger) base {
	if logger == nil {
		logger = slog.Default()
	}
	if x == nil {
		x = bexec.New(logger)
	}
	return base{exec: x, log: logger}
}

// run executes c and classifies the outcome into the three cases the graph
// cares about.
//
// It returns the Result unconditionally, plus a non-nil error only for the two
// fatal cases: a missing toolchain (translated to *belay.ToolchainError) and
// anything else that stopped the command from producing usable output. A
// non-zero exit status is not an error here — a failing test suite is exactly
// that, and every runner in this package parses the output regardless.
func (b base) run(ctx context.Context, runnerName, tool string, c bexec.Command) (bexec.Result, error) {
	res, err := b.exec.Run(ctx, c)
	b.log.LogAttrs(ctx, slog.LevelDebug, "belay/runner: test command finished",
		slog.String("runner", runnerName),
		slog.String("dir", c.Dir),
		slog.Int("exit_code", res.ExitCode),
		slog.Bool("timed_out", res.TimedOut))
	if err == nil {
		return res, nil
	}
	if te := toolchainError(tool, err); te != nil {
		return res, te
	}
	var exit *bexec.ExitError
	if errors.As(err, &exit) {
		// The tool ran and disagreed. That is a report, not an error.
		return res, nil
	}
	return res, &RunError{
		Runner:   runnerName,
		Args:     res.Args,
		ExitCode: res.ExitCode,
		Stderr:   res.Stderr,
		Err:      err,
	}
}

// toolchainError translates an internal/exec toolchain failure into the
// pkg/belay one, or returns nil when err is not that failure.
//
// This is the seam the package doc warns about. exec.ErrToolchainMissing and
// belay.ErrToolchainMissing are different sentinels; the graph tests the
// second. Translating here — rather than at each call site — means a runner
// cannot forget.
//
// tool is the name belay should tell a human to install ("go", "pytest"),
// which is not always exec's redacted program name: a Node run through pnpm
// fails on "pnpm", not on "node".
func toolchainError(tool string, err error) error {
	var te *bexec.ToolchainError
	if !errors.As(err, &te) {
		return nil
	}
	// te.Err is the underlying lookup failure (exec.ErrNotFound,
	// fs.ErrPermission, ...), which is exactly what belay.ToolchainError
	// documents its Err field to carry.
	return &belay.ToolchainError{Tool: tool, Err: te.Err}
}

// projectAt returns the detected project of the given kind rooted exactly at
// dir.
//
// Detection is deliberately depth-0: a runner that will execute its toolchain
// in dir must not claim a directory whose marker files live three levels down,
// where `go test ./...` or `pnpm test` would fail. This keeps Detect cheap
// (one os.ReadDir plus a bounded read per marker) and side-effect free, as
// [belay.TestRunner.Detect] requires.
func projectAt(dir string, kind detect.Kind) (detect.Project, bool) {
	projects, err := detect.Detect(dir, detect.WithMaxDepth(0))
	if err != nil {
		return detect.Project{}, false
	}
	for _, p := range projects {
		if p.Kind == kind {
			return p, true
		}
	}
	return detect.Project{}, false
}

// rawString wraps arbitrary tool output as a JSON string for
// [belay.TestReport.Raw].
//
// Raw must be valid JSON on its own, because a TestReport is marshalled into
// the run journal. A `go test -json` stream is a sequence of JSON objects, not
// one document, so storing it verbatim would make the enclosing report fail to
// marshal. Every runner therefore stores its raw stdout as a JSON string, even
// the ones whose output already is a JSON document, so the rule has no
// exceptions to remember.
func rawString(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return json.RawMessage(b)
}

// sortFailures orders failures by name, stably.
//
// Reports must be deterministic: `go test ./...` runs packages in parallel and
// interleaves their events, so encounter order is not reproducible, and a
// journal that differs between two identical runs is a debugging trap. Sorting
// by name in every runner keeps one rule instead of four.
func sortFailures(failures []belay.TestFailure) {
	slices.SortStableFunc(failures, func(a, b belay.TestFailure) int {
		return strings.Compare(a.Name, b.Name)
	})
}

// relTo reports path relative to dir when it can, and unchanged otherwise.
//
// Toolchains report absolute paths (jest, vitest, node:test) or paths relative
// to their own working directory (go, pytest). belay.TestFailure.File is
// documented as relative to the directory passed to Test, so absolute paths are
// converted and anything that would escape dir is left alone rather than
// rendered as a pile of "..".
func relTo(dir, path string) string {
	if path == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

// lastLines returns the final n lines of s joined by "; ", for one-line error
// messages.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// Select returns the runner named by cfg, or the one that recognizes dir when
// cfg.Runner is [config.TestRunnerAuto].
//
// Auto-detection asks each language runner's Detect in a fixed order — Go,
// Node, Python — and returns the first match. The order is stable rather than
// clever: a repository that is both a Go module and a Node package at its root
// gets the Go runner every time, and an operator who wants the other one pins
// test.runner in configuration. That is a better failure mode than a heuristic
// whose answer changes when a file is added.
//
// Select returns an error wrapping [belay.ErrUnsupported] for a runner name it
// does not implement, [ErrNoCommand] for "custom" with an empty custom_cmd,
// and [ErrNoProject] when auto-detection recognizes nothing at dir.
func Select(cfg config.Test, dir string, x Execer, logger *slog.Logger) (belay.TestRunner, error) {
	switch cfg.Runner {
	case config.TestRunnerGo:
		return NewGo(x, logger), nil
	case config.TestRunnerNode:
		return NewNode(x, logger), nil
	case config.TestRunnerPython:
		return NewPytest(x, logger), nil
	case config.TestRunnerCustom:
		if strings.TrimSpace(cfg.CustomCmd) == "" {
			return nil, fmt.Errorf("belay/runner: test.runner is %q: %w", cfg.Runner, ErrNoCommand)
		}
		return NewCustom(cfg.CustomCmd, x, logger), nil
	case config.TestRunnerAuto:
		for _, r := range []belay.TestRunner{NewGo(x, logger), NewNode(x, logger), NewPytest(x, logger)} {
			if r.Detect(dir) {
				return r, nil
			}
		}
		return nil, fmt.Errorf("belay/runner: nothing to test at %s: %w", dir, ErrNoProject)
	default:
		return nil, fmt.Errorf("belay/runner: test.runner %q: %w", cfg.Runner, belay.ErrUnsupported)
	}
}

// exitStatusReport builds the coarsest report a runner can offer: pass or fail
// from the exit status alone, with no per-test detail.
//
// It is what the custom runner always produces, and what the Node runner falls
// back to for a framework with no machine-readable output. Total is 1 rather
// than 0 in both the passing and failing case because [belay.TestReport.OK]
// requires Total > 0: a report claiming zero tests would flag a green command
// as needing a human. The counts describe the command, not the tests inside
// it, and the failure message says so.
func exitStatusReport(label, failureName string, res bexec.Result) belay.TestReport {
	report := belay.TestReport{Total: 1, Duration: res.Duration}
	if res.ExitCode == 0 {
		report.Passed = 1
		return report
	}
	report.Failed = 1
	report.Failures = []belay.TestFailure{{
		Name:    failureName,
		Message: exitStatusMessage(label, res),
	}}
	return report
}

// exitStatusMessage renders everything known about a coarse failure: what ran,
// how it exited, and the tail of what it printed.
func exitStatusMessage(label string, res bexec.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s exited with status %d", label, res.ExitCode)
	if res.TimedOut {
		b.WriteString(" (timed out)")
	}
	b.WriteString("; no per-test detail is available from this runner")
	for _, stream := range []struct{ name, text string }{
		{"stdout", res.Stdout},
		{"stderr", res.Stderr},
	} {
		if s := strings.TrimSpace(stream.text); s != "" {
			fmt.Fprintf(&b, "\n%s: %s", stream.name, lastLines(s, 20))
		}
	}
	return b.String()
}
