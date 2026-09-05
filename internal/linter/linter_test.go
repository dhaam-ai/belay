//go:build unix

package linter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/internal/exec"
	"github.com/belay-dev/belay/pkg/belay"
)

// issuesAt builds one issue per given severity, so a gate test reads as the
// severity mix it is really about.
func issuesAt(severities ...belay.Severity) []belay.Issue {
	out := make([]belay.Issue, 0, len(severities))
	for i, s := range severities {
		out = append(out, belay.Issue{
			RuleID:   "rule" + string(rune('a'+i)),
			Severity: s,
			File:     "main.go",
			Line:     i + 1,
			Message:  "synthetic",
		})
	}
	return out
}

func TestNewReportGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		issues     []belay.Issue
		failOn     belay.Severity
		wantGate   belay.GateStatus
		wantCounts belay.Counts
	}{
		{
			name:     "no issues passes at every threshold",
			issues:   nil,
			failOn:   belay.SeverityInfo,
			wantGate: belay.GatePass,
		},
		{
			name:       "info issue passes a major gate",
			issues:     issuesAt(belay.SeverityInfo),
			failOn:     belay.SeverityMajor,
			wantGate:   belay.GatePass,
			wantCounts: belay.Counts{Info: 1},
		},
		{
			name:       "info issue fails an info gate",
			issues:     issuesAt(belay.SeverityInfo),
			failOn:     belay.SeverityInfo,
			wantGate:   belay.GateFail,
			wantCounts: belay.Counts{Info: 1},
		},
		{
			name:       "minor issue passes a major gate",
			issues:     issuesAt(belay.SeverityMinor, belay.SeverityMinor),
			failOn:     belay.SeverityMajor,
			wantGate:   belay.GatePass,
			wantCounts: belay.Counts{Minor: 2},
		},
		{
			name:       "major issue fails a major gate",
			issues:     issuesAt(belay.SeverityMajor),
			failOn:     belay.SeverityMajor,
			wantGate:   belay.GateFail,
			wantCounts: belay.Counts{Major: 1},
		},
		{
			name:       "major issue passes a critical gate",
			issues:     issuesAt(belay.SeverityMajor),
			failOn:     belay.SeverityCritical,
			wantGate:   belay.GatePass,
			wantCounts: belay.Counts{Major: 1},
		},
		{
			name:       "critical issue fails a critical gate",
			issues:     issuesAt(belay.SeverityCritical),
			failOn:     belay.SeverityCritical,
			wantGate:   belay.GateFail,
			wantCounts: belay.Counts{Critical: 1},
		},
		{
			name:       "critical issue passes a blocker gate",
			issues:     issuesAt(belay.SeverityCritical),
			failOn:     belay.SeverityBlocker,
			wantGate:   belay.GatePass,
			wantCounts: belay.Counts{Critical: 1},
		},
		{
			name:       "blocker issue fails a blocker gate",
			issues:     issuesAt(belay.SeverityBlocker),
			failOn:     belay.SeverityBlocker,
			wantGate:   belay.GateFail,
			wantCounts: belay.Counts{Blocker: 1},
		},
		{
			name: "mixed issues are counted per severity",
			issues: issuesAt(
				belay.SeverityBlocker,
				belay.SeverityCritical, belay.SeverityCritical,
				belay.SeverityMajor,
				belay.SeverityMinor,
				belay.SeverityInfo,
			),
			failOn:     belay.SeverityMajor,
			wantGate:   belay.GateFail,
			wantCounts: belay.Counts{Blocker: 1, Critical: 2, Major: 1, Minor: 1, Info: 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := newReport("tool", tc.failOn, tc.issues, json.RawMessage(`{}`))

			if got.Gate != tc.wantGate {
				t.Errorf("Gate = %v, want %v", got.Gate, tc.wantGate)
			}
			if diff := cmp.Diff(tc.wantCounts, got.Counts); diff != "" {
				t.Errorf("Counts mismatch (-want +got):\n%s", diff)
			}
			// The gate must be exactly the AtOrAbove verdict; this
			// package never reimplements the threshold test.
			wantFail := got.Counts.AtOrAbove(tc.failOn) > 0
			if wantFail != (got.Gate == belay.GateFail) {
				t.Errorf("Gate = %v but Counts.AtOrAbove(%v) = %d",
					got.Gate, tc.failOn, got.Counts.AtOrAbove(tc.failOn))
			}
		})
	}
}

func TestNewReportShape(t *testing.T) {
	t.Parallel()

	got := newReport("golangci-lint", belay.SeverityMajor, nil, json.RawMessage(`{"Issues":[]}`))

	if got.Source != "golangci-lint" {
		t.Errorf("Source = %q, want %q", got.Source, "golangci-lint")
	}
	if got.Issues == nil {
		t.Error("Issues is nil; the contract requires an empty, non-nil slice")
	}
	if string(got.Raw) != `{"Issues":[]}` {
		t.Errorf("Raw = %s, want the tool's own document", got.Raw)
	}
	// The whole report must round-trip through JSON: an adapter that put
	// non-JSON bytes in Raw would break every consumer that journals it.
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("marshal report: %v", err)
	}
}

func TestErrorReport(t *testing.T) {
	t.Parallel()

	got := errorReport("ruff", belay.SeverityMajor)

	if got.Gate != belay.GateError {
		t.Errorf("Gate = %v, want %v", got.Gate, belay.GateError)
	}
	if got.Counts.Total() != 0 {
		t.Errorf("Counts.Total() = %d, want 0", got.Counts.Total())
	}
	if got.Issues == nil {
		t.Error("Issues is nil; the contract requires an empty, non-nil slice")
	}
	// GateError must never look like a gate failure to the graph.
	if got.Counts.AtOrAbove(belay.SeverityInfo) != 0 {
		t.Error("an error report must carry no issues at any severity")
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("marshal report: %v", err)
	}
}

// okParser is the parser used by the base.lint tests: it accepts any JSON
// array and reports one major issue per element.
func okParser(stdout []byte) ([]belay.Issue, json.RawMessage, error) {
	var elems []struct {
		Message string `json:"message"`
	}
	raw, err := decodeFirstJSON(stdout, &elems)
	if err != nil {
		return nil, nil, err
	}
	issues := make([]belay.Issue, 0, len(elems))
	for _, e := range elems {
		issues = append(issues, belay.Issue{RuleID: "R", Severity: belay.SeverityMajor, Message: e.Message})
	}
	return issues, raw, nil
}

func testBase(runner CommandRunner) base {
	return newBase([]Option{WithRunner(runner), WithLogger(quietLogger())})
}

func TestBaseLintOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		runner     *stubRunner
		wantGate   belay.GateStatus
		wantErr    bool
		wantIs     []error
		wantIsNot  []error
		wantIssues int
	}{
		{
			name:     "clean run passes",
			runner:   okRunner(`[]`),
			wantGate: belay.GatePass,
		},
		{
			name:       "findings with a non-zero exit are not a Go error",
			runner:     findingsRunner(`[{"message":"a"},{"message":"b"}]`, 1),
			wantGate:   belay.GateFail,
			wantIssues: 2,
		},
		{
			name:      "missing binary is a belay toolchain error",
			runner:    missingRunner("faketool"),
			wantErr:   true,
			wantIs:    []error{belay.ErrToolchainMissing},
			wantIsNot: []error{ErrUnparsableOutput},
		},
		{
			name:     "malformed output is a gate error, not a gate failure",
			runner:   findingsRunner(`{"Issues": [`, 1),
			wantGate: belay.GateError,
			wantErr:  true,
			wantIs:   []error{ErrUnparsableOutput},
		},
		{
			name:     "empty output is a gate error",
			runner:   &stubRunner{exitCode: 3, stderr: "Error: unknown linters: 'nope'"},
			wantGate: belay.GateError,
			wantErr:  true,
			wantIs:   []error{ErrUnparsableOutput},
		},
		{
			name:     "truncated output is a gate error",
			runner:   &stubRunner{stdout: `[{"message":"a"}`, truncated: true},
			wantGate: belay.GateError,
			wantErr:  true,
			wantIs:   []error{ErrUnparsableOutput},
		},
		{
			name: "a timeout is a gate error and keeps its sentinel",
			runner: &stubRunner{
				exitCode: 143,
				err:      &exec.TimeoutError{Args: []string{"faketool"}, Timeout: time.Second},
			},
			wantGate:  belay.GateError,
			wantErr:   true,
			wantIs:    []error{exec.ErrTimeout},
			wantIsNot: []error{belay.ErrToolchainMissing, ErrUnparsableOutput},
		},
		{
			name:      "a cancelled context is a gate error",
			runner:    &stubRunner{exitCode: -1, err: context.Canceled},
			wantGate:  belay.GateError,
			wantErr:   true,
			wantIs:    []error{context.Canceled},
			wantIsNot: []error{belay.ErrToolchainMissing},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := testBase(tc.runner)

			got, err := b.lint(t.Context(), "faketool", "faketool",
				exec.Command{Path: "faketool"}, okParser)

			if tc.wantErr != (err != nil) {
				t.Fatalf("error = %v, wantErr = %v", err, tc.wantErr)
			}
			for _, target := range tc.wantIs {
				if !errors.Is(err, target) {
					t.Errorf("errors.Is(err, %v) = false, want true (err = %v)", target, err)
				}
			}
			for _, target := range tc.wantIsNot {
				if errors.Is(err, target) {
					t.Errorf("errors.Is(err, %v) = true, want false (err = %v)", target, err)
				}
			}
			if got.Gate != tc.wantGate {
				t.Errorf("Gate = %v, want %v", got.Gate, tc.wantGate)
			}
			if len(got.Issues) != tc.wantIssues {
				t.Errorf("len(Issues) = %d, want %d", len(got.Issues), tc.wantIssues)
			}
		})
	}
}

func TestBaseLintToolchainErrorCarriesToolAndZeroReport(t *testing.T) {
	t.Parallel()

	b := testBase(missingRunner("golangci-lint"))

	got, err := b.lint(t.Context(), "golangci-lint", "golangci-lint",
		exec.Command{Path: "golangci-lint"}, okParser)

	var te *belay.ToolchainError
	if !errors.As(err, &te) {
		t.Fatalf("errors.As(*belay.ToolchainError) = false, err = %v", err)
	}
	if te.Tool != "golangci-lint" {
		t.Errorf("Tool = %q, want %q", te.Tool, "golangci-lint")
	}
	// The Linter contract requires a zero report alongside the error.
	if diff := cmp.Diff(belay.QualityReport{}, got); diff != "" {
		t.Errorf("report is not zero (-want +got):\n%s", diff)
	}
}

func TestDecodeFirstJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stdout  string
		wantRaw string
		wantErr error
	}{
		{
			name:    "plain document",
			stdout:  `{"a":1}`,
			wantRaw: `{"a":1}`,
		},
		{
			name:    "leading and trailing whitespace",
			stdout:  "\n  {\"a\":1}\n\n",
			wantRaw: `{"a":1}`,
		},
		{
			// golangci-lint v2 prints a human summary after its JSON.
			name:    "trailing human summary is ignored",
			stdout:  "{\"a\":1}\n3 issues:\n* errcheck: 2\n",
			wantRaw: `{"a":1}`,
		},
		{
			name:    "empty output",
			stdout:  "",
			wantErr: errEmptyOutput,
		},
		{
			name:    "whitespace only",
			stdout:  "   \n\t\n",
			wantErr: errEmptyOutput,
		},
		{
			name:   "truncated document",
			stdout: `{"a":`,
		},
		{
			name:   "not json at all",
			stdout: "panic: runtime error\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var v any
			raw, err := decodeFirstJSON([]byte(tc.stdout), &v)

			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantRaw == "":
				if err == nil {
					t.Fatalf("err = nil, want a decode failure")
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if string(raw) != tc.wantRaw {
					t.Errorf("raw = %s, want %s", raw, tc.wantRaw)
				}
				if !json.Valid(raw) {
					t.Errorf("raw is not valid JSON: %s", raw)
				}
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		gate   belay.GateStatus
		counts belay.Counts
		want   string
	}{
		{
			name: "clean",
			gate: belay.GatePass,
			want: "eslint: no issues; gate pass at fail_on=major",
		},
		{
			name:   "one issue is singular",
			gate:   belay.GateFail,
			counts: belay.Counts{Major: 1},
			want:   "eslint: 1 issue (1 major); gate fail at fail_on=major",
		},
		{
			name:   "worst severity first",
			gate:   belay.GateFail,
			counts: belay.Counts{Blocker: 1, Major: 2, Info: 3},
			want:   "eslint: 6 issues (1 blocker, 2 major, 3 info); gate fail at fail_on=major",
		},
		{
			name: "error",
			gate: belay.GateError,
			want: "eslint: could not produce a report; gate error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := summarize("eslint", tc.gate, tc.counts, belay.SeverityMajor); got != tc.want {
				t.Errorf("summarize() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOptionDefaults(t *testing.T) {
	t.Parallel()

	b := newBase(nil)
	if b.failOn != DefaultFailOn {
		t.Errorf("failOn = %v, want %v", b.failOn, DefaultFailOn)
	}
	if b.timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", b.timeout, DefaultTimeout)
	}
	if b.runner == nil || b.logger == nil {
		t.Fatal("newBase must resolve a runner and a logger")
	}

	// Nil and non-positive arguments are ignored rather than accepted, so a
	// caller can pass optional configuration through unconditionally.
	nilSafe := newBase([]Option{WithRunner(nil), WithLogger(nil), WithTimeout(0), WithTimeout(-time.Second)})
	if nilSafe.runner == nil || nilSafe.logger == nil {
		t.Error("nil Option arguments must not clear the defaults")
	}
	if nilSafe.timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", nilSafe.timeout, DefaultTimeout)
	}

	set := newBase([]Option{WithFailOn(belay.SeverityBlocker), WithTimeout(time.Minute)})
	if set.failOn != belay.SeverityBlocker {
		t.Errorf("failOn = %v, want %v", set.failOn, belay.SeverityBlocker)
	}
	if set.timeout != time.Minute {
		t.Errorf("timeout = %v, want %v", set.timeout, time.Minute)
	}
}

func TestOutputError(t *testing.T) {
	t.Parallel()

	err := &OutputError{
		Tool:     "ruff",
		ExitCode: 2,
		Stderr:   "ruff failed\n  Cause: invalid pyproject.toml\n",
		Err:      errors.New("invalid character 'x'"),
	}

	if !errors.Is(err, ErrUnparsableOutput) {
		t.Error("errors.Is(err, ErrUnparsableOutput) = false, want true")
	}
	if errors.Is(err, belay.ErrToolchainMissing) {
		t.Error("an unparsable report must not look like a missing toolchain")
	}
	msg := err.Error()
	for _, want := range []string{"ruff", "exit 2", "invalid character 'x'", "Cause: invalid pyproject.toml"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to contain %q", msg, want)
		}
	}

	truncated := &OutputError{Tool: "eslint", ExitCode: 1, Truncated: true}
	if !strings.Contains(truncated.Error(), "truncated") {
		t.Errorf("Error() = %q, want it to mention truncation", truncated.Error())
	}
	if !errors.Is(truncated, ErrUnparsableOutput) {
		t.Error("errors.Is(err, ErrUnparsableOutput) = false, want true")
	}
}

func TestLocalTool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	executable := filepath.Join(binDir, "eslint")
	// #nosec G306 -- localTool tests the executable bit, so the fixture must
	// carry one. It lives in t.TempDir().
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	notExecutable := filepath.Join(dir, "plain")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name       string
		candidates []string
		want       string
	}{
		{name: "project-local binary wins", candidates: []string{"node_modules/.bin/eslint"}, want: executable},
		{name: "missing candidate falls back to PATH", candidates: []string{"node_modules/.bin/nope"}, want: "eslint"},
		{name: "non-executable file is not a tool", candidates: []string{"plain"}, want: "eslint"},
		{name: "directory is not a tool", candidates: []string{"node_modules"}, want: "eslint"},
		{name: "first existing candidate wins", candidates: []string{"nope", "node_modules/.bin/eslint"}, want: executable},
		{name: "no candidates falls back to PATH", want: "eslint"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := localTool(dir, "eslint", tc.candidates...); got != tc.want {
				t.Errorf("localTool() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNoRealLinterInvocation is the mechanical half of the guarantee that this
// suite never shells out to a real linter.
//
// The other half is structural: every adapter reaches a subprocess only
// through the CommandRunner seam, and every test in this package supplies a
// stubRunner. This test proves no test file quietly bypasses that by
// constructing the real runner or reaching for the standard library directly.
func TestNoRealLinterInvocation(t *testing.T) {
	t.Parallel()

	// Assembled from pieces so this file does not match its own scan.
	banned := []string{
		"os" + "/exec",
		"exec." + "New(",
		"exec.Runner" + "{",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		// #nosec G304 -- name comes from reading this package's own directory.
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		for _, needle := range banned {
			if strings.Contains(string(src), needle) {
				t.Errorf("%s contains %q: tests must never start a real process", name, needle)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no test files; the guard is not actually running")
	}
}

// TestAdapterNames pins the identifiers every report is attributed to.
//
// belay.Linter documents Name as "a short, stable, lowercase identifier" that
// "becomes QualityReport.Source", so a rename here is a breaking change for
// anything that reads a journal.
func TestAdapterNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		want   string
		linter belay.Linter
		stdout string
	}{
		{want: "golangci-lint", linter: NewGolangCI(WithRunner(okRunner(`{"Issues":[]}`)), WithLogger(quietLogger())), stdout: `{"Issues":[]}`},
		{want: "eslint", linter: NewESLint(WithRunner(okRunner(`[]`)), WithLogger(quietLogger()))},
		{want: "ruff", linter: NewRuff(WithRunner(okRunner(`[]`)), WithLogger(quietLogger()))},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.linter.Name(); got != tc.want {
				t.Errorf("Name() = %q, want %q", got, tc.want)
			}
			if strings.ToLower(tc.want) != tc.want {
				t.Errorf("Name() = %q, want a lowercase identifier", tc.want)
			}

			report, err := tc.linter.Lint(t.Context(), t.TempDir())
			if err != nil {
				t.Fatalf("Lint: %v", err)
			}
			if report.Source != tc.linter.Name() {
				t.Errorf("Source = %q, want Name() = %q", report.Source, tc.linter.Name())
			}
		})
	}
}

func TestCountIssuesOutOfRangeSeverity(t *testing.T) {
	t.Parallel()

	// No mapping table produces one, but a Severity outside the five
	// declared constants must still be counted rather than dropped: a lost
	// issue would make Counts.Total disagree with len(Issues).
	issues := []belay.Issue{
		{Severity: belay.Severity(42)},
		{Severity: belay.Severity(-3)},
		{Severity: belay.SeverityMajor},
	}
	got := countIssues(issues)

	if got.Total() != len(issues) {
		t.Errorf("Total() = %d, want %d: an unrecognized severity was dropped", got.Total(), len(issues))
	}
	if got.Info != 2 {
		t.Errorf("Info = %d, want 2: unrecognized severities count as info", got.Info)
	}
	if got.Major != 1 {
		t.Errorf("Major = %d, want 1", got.Major)
	}
}

func TestRelPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "an absolute path inside the directory", path: filepath.Join(dir, "src", "a.js"), want: filepath.Join("src", "a.js")},
		{name: "the directory itself", path: dir, want: "."},
		{name: "an already-relative path is untouched", path: "src/a.js", want: "src/a.js"},
		{name: "an empty path is untouched", path: "", want: ""},
		{
			// A linted file outside the working directory: an honest
			// absolute path beats a relative one full of "..".
			name: "an absolute path outside the directory stays absolute",
			path: "/somewhere/else/a.js",
			want: "/somewhere/else/a.js",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := relPath(dir, tc.path); got != tc.want {
				t.Errorf("relPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestRanToCompletion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "no error", err: nil, want: true},
		{
			// The whole point: a linter with findings exits non-zero,
			// and that is the tool working, not failing.
			name: "a non-zero exit",
			err:  &exec.ExitError{Args: []string{"ruff"}, Code: 1},
			want: true,
		},
		{name: "a wrapped non-zero exit", err: fmt.Errorf("wrapped: %w", &exec.ExitError{Code: 1}), want: true},
		{name: "a timeout", err: &exec.TimeoutError{Timeout: time.Second}, want: false},
		{name: "a cancelled context", err: context.Canceled, want: false},
		{name: "a missing binary", err: &exec.ToolchainError{Tool: "ruff"}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ranToCompletion(tc.err); got != tc.want {
				t.Errorf("ranToCompletion(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
