//go:build unix

// Package linter adapts the off-the-shelf linters belay runs as its default
// quality gate to the belay.Linter interface.
//
// review.mode: lint (ADR 0008) is the zero-setup path: with no SonarQube, no
// API token and no configuration at all, belay still gates a run on the
// linters the target repository already has. Three adapters cover the three
// ecosystems internal/detect recognizes — [GolangCI], [ESLint] and [Ruff] —
// and each answers one question for the graph: is this change good enough to
// leave the fix loop?
//
// # The one job: a severity vocabulary
//
// Every tool has its own idea of how bad a finding is, and none of them is
// belay.Severity. golangci-lint reports a linter name and an often-empty
// severity string; ESLint reports the integers 1 and 2; ruff reports a rule
// code and, in stable output, the constant "error". Translating those into
// Info < Minor < Major < Critical < Blocker is this package's real work,
// because review.fail_on turns the result into a hard stop for the whole run.
//
// Each adapter therefore carries an explicit, documented mapping table
// (golangciSeverities, eslintSeverities, ruffSeverities) with a rationale
// recorded against every row. The tables are data, not control flow, so a
// reviewer can read one, disagree with a row, and change it without reading a
// parser.
//
// # Three outcomes, and only one of them is a Go error
//
//   - The tool ran and found nothing that crosses the threshold: GatePass, nil
//     error.
//   - The tool ran and found something: GateFail, nil error. Linters exit
//     non-zero when they have findings; that is the tool doing its job, not a
//     failure, and the graph routes it to the fix node exactly like a failing
//     test.
//   - The tool could not produce a verdict — it is not installed, it timed
//     out, it crashed, or it emitted output that is not the JSON it promised:
//     an error. A missing binary yields a *belay.ToolchainError; everything
//     else yields a GateError report alongside the error, because the remedy
//     for "install golangci-lint" is a human and the remedy for "your code has
//     problems" is the agent, and the graph must not confuse the two.
//
// # Subprocesses
//
// Every process this package starts goes through internal/exec — never
// os/exec — for its deny-by-default environment, output caps, redaction and
// process-group timeouts. [CommandRunner] is that seam, and it is injectable:
// the tests in this package drive the parsers with hand-authored JSON
// fixtures and never invoke a real linter.
package linter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/belay-dev/belay/internal/detect"
	"github.com/belay-dev/belay/internal/exec"
	"github.com/belay-dev/belay/pkg/belay"
)

// Defaults applied when an Option leaves a setting unset.
const (
	// DefaultTimeout bounds one lint run. It is deliberately shorter than
	// exec.DefaultTimeout: a linter that has not finished in five minutes
	// is stuck, and every minute past that is billed to a run that cannot
	// make progress until it returns.
	DefaultTimeout = 5 * time.Minute

	// DefaultMaxOutput caps captured stdout, in bytes. Eight times
	// exec.DefaultMaxOutput, because a JSON report for a large repository
	// is legitimately megabytes long and a truncated report is not a
	// report at all — it parses as an error, not as "no issues".
	DefaultMaxOutput int64 = 8 << 20

	// DefaultFailOn is the severity at which a report's gate fails. It
	// matches the review.fail_on default in internal/config, so an adapter
	// constructed with no options gates exactly like the shipped
	// configuration.
	DefaultFailOn = belay.SeverityMajor
)

// ErrUnparsableOutput reports that a linter ran but belay could not turn what
// it printed into a QualityReport.
//
// It is deliberately distinct from belay.ErrToolchainMissing: the tool exists
// and ran, so installing something will not help. It is equally distinct from
// a failing gate — nothing was measured at all, so routing the run to the fix
// node would hand an agent a diagnosis belay does not have.
var ErrUnparsableOutput = errors.New("belay/linter: linter output could not be parsed")

// errEmptyOutput is the parse failure for a tool that printed nothing at all,
// which is what every one of these tools does when it rejects its own
// configuration.
var errEmptyOutput = errors.New("linter produced no output")

// OutputError reports that a linter ran to completion but its output could not
// be decoded. It wraps ErrUnparsableOutput, so
//
//	errors.Is(err, linter.ErrUnparsableOutput)
//
// holds, and it carries the diagnostics needed to work out why: the tool's
// exit code and the tail of its standard error, which is where all three of
// these tools report a configuration they could not load.
type OutputError struct {
	// Tool is the executable that produced the output, such as "eslint".
	Tool string
	// ExitCode is the tool's exit status, or -1 if it never ran.
	ExitCode int
	// Truncated reports that output hit DefaultMaxOutput and was cut, which
	// makes a valid JSON document unparsable through no fault of the tool.
	Truncated bool
	// Stderr is the redacted tail of the tool's standard error.
	Stderr string
	// Err is the underlying decode failure, if any.
	Err error
}

// Error implements error.
func (e *OutputError) Error() string {
	var b strings.Builder
	b.WriteString("belay/linter: ")
	b.WriteString(e.Tool)
	b.WriteString(" produced unusable output (exit ")
	b.WriteString(strconv.Itoa(e.ExitCode))
	b.WriteString(")")
	if e.Truncated {
		b.WriteString(": output was truncated at the capture limit")
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	if tail := lastLine(e.Stderr); tail != "" {
		b.WriteString(": ")
		b.WriteString(tail)
	}
	return b.String()
}

// Unwrap reports ErrUnparsableOutput and, when present, the decode failure.
func (e *OutputError) Unwrap() []error {
	if e.Err != nil {
		return []error{ErrUnparsableOutput, e.Err}
	}
	return []error{ErrUnparsableOutput}
}

// CommandRunner starts subprocesses. It is the seam every adapter in this
// package runs its tool through, satisfied in production by *exec.Runner and
// in tests by a stub that returns a captured fixture without starting a
// process.
type CommandRunner interface {
	// Run executes c and returns its redacted result. It follows
	// exec.Runner.Run: a Result is returned even on failure, and a non-zero
	// exit yields an *exec.ExitError rather than nil.
	Run(ctx context.Context, c exec.Command) (exec.Result, error)
}

var _ CommandRunner = (*exec.Runner)(nil)

// Option customizes an adapter at construction. Options are shared by every
// adapter in this package.
type Option func(*base)

// WithRunner replaces the subprocess runner. A nil runner is ignored, so a
// caller can pass one through unconditionally.
func WithRunner(r CommandRunner) Option {
	return func(b *base) {
		if r != nil {
			b.runner = r
		}
	}
}

// WithLogger sets the logger. A nil logger is ignored and slog.Default is used.
func WithLogger(l *slog.Logger) Option {
	return func(b *base) {
		if l != nil {
			b.logger = l
		}
	}
}

// WithFailOn sets the severity at or above which a report's gate fails,
// defaulting to DefaultFailOn.
//
// belay.Linter.Lint takes only a directory, so unlike a belay.Reviewer an
// adapter has no per-call threshold; this is where the caller supplies the one
// it would otherwise have put in a ReviewRequest. Callers holding a
// config.Severity (a string type) convert with belay.ParseSeverity first.
//
// belay.SeverityInfo is a legitimate, deliberately strict value meaning "fail
// on anything at all", not "unset".
func WithFailOn(s belay.Severity) Option {
	return func(b *base) { b.failOn = s }
}

// WithTimeout bounds one lint run, defaulting to DefaultTimeout. A
// non-positive duration is ignored.
func WithTimeout(d time.Duration) Option {
	return func(b *base) {
		if d > 0 {
			b.timeout = d
		}
	}
}

// base is the machinery every adapter shares: the runner seam, the gate
// threshold, and the run-parse-report pipeline in lint. An adapter contributes
// only the two things that genuinely differ between tools — the command line
// and the parser.
type base struct {
	runner  CommandRunner
	logger  *slog.Logger
	failOn  belay.Severity
	timeout time.Duration
}

// newBase resolves opts against the package defaults.
func newBase(opts []Option) base {
	b := base{failOn: DefaultFailOn, timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(&b)
	}
	if b.logger == nil {
		b.logger = slog.Default()
	}
	if b.runner == nil {
		b.runner = exec.New(b.logger)
	}
	return b
}

// parser turns a tool's captured stdout into issues plus the exact JSON
// document those issues came from, which becomes QualityReport.Raw.
type parser func(stdout []byte) ([]belay.Issue, json.RawMessage, error)

// lint is the whole run-parse-report pipeline, and the single place the
// three-outcome contract in the package doc is implemented.
//
// source names the adapter (and becomes QualityReport.Source), tool names the
// executable for error messages, cmd is the command to run, and parse decodes
// what it printed.
func (b base) lint(ctx context.Context, source, tool string, cmd exec.Command, parse parser) (belay.QualityReport, error) {
	res, runErr := b.runner.Run(ctx, cmd)

	if runErr != nil && !ranToCompletion(runErr) {
		// TRAP: internal/exec and pkg/belay declare separate
		// ErrToolchainMissing sentinels. Handing exec's straight back
		// would make errors.Is(err, belay.ErrToolchainMissing) false at
		// the graph, which decides "a human installs software" versus
		// "the agent edits code" on exactly that test. Translate here.
		if errors.Is(runErr, exec.ErrToolchainMissing) {
			return belay.QualityReport{}, &belay.ToolchainError{Tool: tool, Err: runErr}
		}
		// A timeout, a cancelled context or an unusable working
		// directory: the tool never reached a verdict, so the report is
		// GateError and the error is passed through with its sentinels
		// intact (exec.ErrTimeout, context.Canceled).
		return errorReport(source, b.failOn), fmt.Errorf("belay/linter: %s: %w", tool, runErr)
	}

	if res.Truncated {
		return errorReport(source, b.failOn), &OutputError{
			Tool: tool, ExitCode: res.ExitCode, Truncated: true, Stderr: res.Stderr,
		}
	}

	issues, raw, err := parse([]byte(res.Stdout))
	if err != nil {
		return errorReport(source, b.failOn), &OutputError{
			Tool: tool, ExitCode: res.ExitCode, Stderr: res.Stderr, Err: err,
		}
	}

	report := newReport(source, b.failOn, issues, raw)
	b.logger.LogAttrs(ctx, slog.LevelDebug, "belay/linter: lint finished",
		slog.String("source", source),
		slog.String("gate", report.Gate.String()),
		slog.Int("issues", report.Counts.Total()),
		slog.Int("at_or_above_fail_on", report.Counts.AtOrAbove(b.failOn)),
		slog.String("fail_on", b.failOn.String()),
		slog.Int("exit_code", res.ExitCode))
	return report, nil
}

// ranToCompletion reports whether err still means the tool ran and printed a
// report. A non-zero exit is the normal outcome for a linter with findings, so
// *exec.ExitError is success as far as this package is concerned.
func ranToCompletion(err error) bool {
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// mapping is one row of a tool's severity table: the belay.Severity a tool's
// own verdict translates to, and the reason that translation is defensible.
//
// Why is not decoration. review.fail_on turns Critical and Blocker into a hard
// stop for an entire run, so every promotion to one has to survive being read
// by someone who disagrees with it.
type mapping struct {
	// Severity is the belay.Severity this row maps to.
	Severity belay.Severity
	// Why records the rationale, in one sentence.
	Why string
}

// newReport assembles a QualityReport from parsed issues.
//
// Gate is computed only through belay.Counts.AtOrAbove, which is the same
// comparison a Reviewer makes against ReviewRequest.FailOn — this package
// never reimplements the threshold test.
func newReport(source string, failOn belay.Severity, issues []belay.Issue, raw json.RawMessage) belay.QualityReport {
	if issues == nil {
		issues = []belay.Issue{}
	}
	counts := countIssues(issues)
	gate := belay.GatePass
	if counts.AtOrAbove(failOn) > 0 {
		gate = belay.GateFail
	}
	return belay.QualityReport{
		Source:  source,
		Gate:    gate,
		Counts:  counts,
		Issues:  issues,
		Summary: summarize(source, gate, counts, failOn),
		Raw:     raw,
	}
}

// errorReport is the report for a tool that never reached a verdict.
//
// It carries GateError, not GateFail: nothing was measured, so the graph must
// escalate rather than send an agent to fix findings that do not exist.
func errorReport(source string, failOn belay.Severity) belay.QualityReport {
	return belay.QualityReport{
		Source:  source,
		Gate:    belay.GateError,
		Issues:  []belay.Issue{},
		Summary: summarize(source, belay.GateError, belay.Counts{}, failOn),
	}
}

// countIssues builds the severity histogram.
//
// A Severity outside the five declared constants counts as Info; the mapping
// tables in this package never produce one.
func countIssues(issues []belay.Issue) belay.Counts {
	var c belay.Counts
	for _, issue := range issues {
		switch issue.Severity {
		case belay.SeverityBlocker:
			c.Blocker++
		case belay.SeverityCritical:
			c.Critical++
		case belay.SeverityMajor:
			c.Major++
		case belay.SeverityMinor:
			c.Minor++
		case belay.SeverityInfo:
			c.Info++
		default:
			c.Info++
		}
	}
	return c
}

// summarize renders the one-line human synopsis carried in
// QualityReport.Summary.
func summarize(source string, gate belay.GateStatus, c belay.Counts, failOn belay.Severity) string {
	if gate == belay.GateError {
		return source + ": could not produce a report; gate error"
	}
	head := source + ": no issues"
	if total := c.Total(); total > 0 {
		head = fmt.Sprintf("%s: %d issue%s (%s)", source, total, plural(total), breakdown(c))
	}
	return fmt.Sprintf("%s; gate %s at fail_on=%s", head, gate, failOn)
}

// breakdown renders the non-zero severity counts, worst first.
func breakdown(c belay.Counts) string {
	parts := make([]string, 0, 5)
	for _, part := range []struct {
		n    int
		name belay.Severity
	}{
		{c.Blocker, belay.SeverityBlocker},
		{c.Critical, belay.SeverityCritical},
		{c.Major, belay.SeverityMajor},
		{c.Minor, belay.SeverityMinor},
		{c.Info, belay.SeverityInfo},
	} {
		if part.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", part.n, part.name))
		}
	}
	return strings.Join(parts, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// decodeFirstJSON decodes the first JSON value in stdout into v and returns
// exactly the bytes that value occupied.
//
// Decoding the first value rather than the whole buffer is not defensive
// programming, it is required: golangci-lint v2.12.2 writes its JSON document
// and then a human-readable issue summary to the same stream, so
// json.Unmarshal over all of stdout fails with "invalid character 'N' after
// top-level value" on every run that finds something. Returning the consumed
// slice also keeps QualityReport.Raw valid JSON, which a trailing summary
// would break.
func decodeFirstJSON(stdout []byte, v any) (json.RawMessage, error) {
	if len(bytes.TrimSpace(stdout)) == 0 {
		return nil, errEmptyOutput
	}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	if err := dec.Decode(v); err != nil {
		return nil, err
	}
	consumed := bytes.TrimSpace(stdout[:dec.InputOffset()])
	return json.RawMessage(slices.Clone(consumed)), nil
}

// lastLine returns the final non-empty line of s, which is where these tools
// put the reason they refused to run.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// relPath renders p relative to dir.
//
// belay.Issue.File is documented as relative to the directory the check ran
// against, but ESLint and ruff both report absolute paths. A path that is
// already relative, or that points outside dir, is returned unchanged: a
// wrong relative path is worse than an honest absolute one.
func relPath(dir, p string) string {
	if p == "" || !filepath.IsAbs(p) {
		return p
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return rel
}

// projects reports every project internal/detect finds under dir.
//
// belay.Linter.Detect must not error, so a directory that cannot be walked
// yields no projects and, therefore, no claim on it.
func projects(dir string) []detect.Project {
	found, err := detect.Detect(dir)
	if err != nil {
		return nil
	}
	return found
}

// localTool resolves a project-local executable, falling back to name for a
// PATH lookup.
//
// Node and Python projects normally install their linters inside the project
// (node_modules/.bin, .venv/bin) rather than globally, and belay must find
// those without reaching for npx or pipx — a package runner would download and
// execute code from the network as a side effect of linting, which is neither
// free nor safe. The returned path is absolute because os/exec resolves a
// relative program path against belay's working directory, not the child's.
func localTool(dir, name string, candidates ...string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return name
	}
	for _, candidate := range candidates {
		path := filepath.Join(abs, filepath.FromSlash(candidate))
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return path
	}
	return name
}
