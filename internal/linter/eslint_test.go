//go:build unix

package linter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestESLintSeverityTable pins the whole mapping. ESLint's vocabulary is three
// integers plus a flag, so "exhaustive" here means literally every value the
// tool can emit, plus the out-of-range case.
func TestESLintSeverityTable(t *testing.T) {
	t.Parallel()

	want := map[int]belay.Severity{
		2: belay.SeverityMajor,
		1: belay.SeverityMinor,
		0: belay.SeverityInfo,
	}

	for severity, wantSeverity := range want {
		got, ok := eslintSeverities[severity]
		if !ok {
			t.Errorf("eslintSeverities is missing %d", severity)
			continue
		}
		if got.Severity != wantSeverity {
			t.Errorf("eslintSeverities[%d] = %v, want %v", severity, got.Severity, wantSeverity)
		}
		if strings.TrimSpace(got.Why) == "" {
			t.Errorf("eslintSeverities[%d] has no rationale", severity)
		}
	}
	for severity := range eslintSeverities {
		if _, ok := want[severity]; !ok {
			t.Errorf("eslintSeverities has an untested row %d", severity)
		}
	}

	if eslintFatalSeverity.Severity != belay.SeverityBlocker {
		t.Errorf("fatal maps to %v, want %v", eslintFatalSeverity.Severity, belay.SeverityBlocker)
	}
	if defaultESLintSeverity.Severity != belay.SeverityInfo {
		t.Errorf("default maps to %v, want %v", defaultESLintSeverity.Severity, belay.SeverityInfo)
	}
	for name, m := range map[string]mapping{"fatal": eslintFatalSeverity, "default": defaultESLintSeverity} {
		if strings.TrimSpace(m.Why) == "" {
			t.Errorf("the %s row has no rationale", name)
		}
	}

	// A parse error is the only severity this adapter assigns on its own
	// authority; everything else defers to the repository's config.
	for severity, m := range eslintSeverities {
		if m.Severity > belay.SeverityMajor {
			t.Errorf("eslintSeverities[%d] = %v: no rule verdict may outrank a parse error", severity, m.Severity)
		}
	}
}

func TestESLintSeverityResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		severity int
		fatal    bool
		want     belay.Severity
	}{
		{name: "error", severity: 2, want: belay.SeverityMajor},
		{name: "warning", severity: 1, want: belay.SeverityMinor},
		{name: "off", severity: 0, want: belay.SeverityInfo},
		{
			// A fatal message carries severity 2 like any other error;
			// the flag, not the integer, is what makes it a blocker.
			name:     "a fatal parse error outranks its own severity integer",
			severity: 2,
			fatal:    true,
			want:     belay.SeverityBlocker,
		},
		{name: "fatal with a warning severity is still a blocker", severity: 1, fatal: true, want: belay.SeverityBlocker},
		{name: "an undocumented severity takes the safe default", severity: 7, want: belay.SeverityInfo},
		{name: "a negative severity takes the safe default", severity: -1, want: belay.SeverityInfo},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, why := eslintSeverity(tc.severity, tc.fatal)
			if got != tc.want {
				t.Errorf("eslintSeverity(%d, %v) = %v, want %v", tc.severity, tc.fatal, got, tc.want)
			}
			if strings.TrimSpace(why) == "" {
				t.Error("resolution returned no rationale")
			}
		})
	}
}

func TestESLintParse(t *testing.T) {
	t.Parallel()

	// The fixture reports absolute paths, as ESLint does; "/repo" is the
	// directory they are relative to.
	issues, raw, err := parseESLint("/repo", []byte(readFixture(t, "eslint-issues.json")))
	if err != nil {
		t.Fatalf("parseESLint: %v", err)
	}

	want := []belay.Issue{
		{
			RuleID:   "no-undef",
			Severity: belay.SeverityMajor,
			File:     "src/index.js",
			Line:     12,
			Message:  "'fetchUser' is not defined.",
		},
		{
			RuleID:   "no-unused-vars",
			Severity: belay.SeverityMinor,
			File:     "src/index.js",
			Line:     18,
			Message:  "'tmp' is assigned a value but never used.",
		},
		{
			RuleID:   eslintCoreRuleID,
			Severity: belay.SeverityMinor,
			File:     "src/index.js",
			Line:     30,
			Message:  "Unused eslint-disable directive (no problems were reported from 'no-console').",
		},
		{
			RuleID:   eslintFatalRuleID,
			Severity: belay.SeverityBlocker,
			File:     "src/broken.js",
			Line:     7,
			Message:  "Parsing error: Unexpected token }",
		},
	}

	if diff := cmp.Diff(want, issues); diff != "" {
		t.Errorf("issues mismatch (-want +got):\n%s", diff)
	}
	if !json.Valid(raw) {
		t.Errorf("raw is not valid JSON: %s", raw)
	}

	// suppressedMessages were silenced by an eslint-disable comment in the
	// repository. Counting them would overrule a decision the project made
	// explicitly.
	for _, issue := range issues {
		if issue.RuleID == "no-console" {
			t.Error("a suppressed message was counted as an issue")
		}
	}
}

func TestESLintLintFindingsAreNotAnError(t *testing.T) {
	t.Parallel()

	// ESLint documents exit 1 as "linting was successful and there is at
	// least one linting error".
	runner := findingsRunner(readFixture(t, "eslint-issues.json"), 1)
	linter := NewESLint(WithRunner(runner), WithLogger(quietLogger()))

	got, err := linter.Lint(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Lint returned an error for a run that found issues: %v", err)
	}
	if got.Gate != belay.GateFail {
		t.Errorf("Gate = %v, want %v", got.Gate, belay.GateFail)
	}
	if got.Source != "eslint" {
		t.Errorf("Source = %q, want %q", got.Source, "eslint")
	}
	wantCounts := belay.Counts{Blocker: 1, Major: 1, Minor: 2}
	if diff := cmp.Diff(wantCounts, got.Counts); diff != "" {
		t.Errorf("Counts mismatch (-want +got):\n%s", diff)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("report does not round-trip through JSON: %v", err)
	}
}

func TestESLintLintClean(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
	}{
		{name: "files linted, nothing found", stdout: readFixture(t, "eslint-clean.json")},
		{name: "an empty result array", stdout: "[]\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			linter := NewESLint(WithRunner(okRunner(tc.stdout)), WithLogger(quietLogger()))

			got, err := linter.Lint(t.Context(), t.TempDir())
			if err != nil {
				t.Fatalf("Lint: %v", err)
			}
			if got.Gate != belay.GatePass {
				t.Errorf("Gate = %v, want %v", got.Gate, belay.GatePass)
			}
			if got.Issues == nil || len(got.Issues) != 0 {
				t.Errorf("Issues = %v, want an empty non-nil slice", got.Issues)
			}
		})
	}
}

func TestESLintLintMissingToolchain(t *testing.T) {
	t.Parallel()

	linter := NewESLint(WithRunner(missingRunner("eslint")), WithLogger(quietLogger()))

	got, err := linter.Lint(t.Context(), t.TempDir())

	if !errors.Is(err, belay.ErrToolchainMissing) {
		t.Fatalf("errors.Is(err, belay.ErrToolchainMissing) = false, err = %v", err)
	}
	var te *belay.ToolchainError
	if !errors.As(err, &te) {
		t.Fatalf("errors.As(*belay.ToolchainError) = false, err = %v", err)
	}
	if te.Tool != "eslint" {
		t.Errorf("Tool = %q, want %q", te.Tool, "eslint")
	}
	if diff := cmp.Diff(belay.QualityReport{}, got); diff != "" {
		t.Errorf("report is not zero (-want +got):\n%s", diff)
	}
}

func TestESLintLintCannotRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		runner *stubRunner
	}{
		{
			name:   "malformed json",
			runner: findingsRunner(readFixture(t, "malformed.json"), 1),
		},
		{
			// ESLint documents exit 2 as "linting was unsuccessful due
			// to a configuration problem or an internal error", and
			// prints nothing to stdout.
			name: "a configuration problem",
			runner: &stubRunner{
				exitCode: 2,
				stderr:   "Oops! Something went wrong!\nESLint couldn't find an eslint.config.js file.",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			linter := NewESLint(WithRunner(tc.runner), WithLogger(quietLogger()))

			got, err := linter.Lint(t.Context(), t.TempDir())

			if !errors.Is(err, ErrUnparsableOutput) {
				t.Fatalf("errors.Is(err, ErrUnparsableOutput) = false, err = %v", err)
			}
			if got.Gate != belay.GateError {
				t.Errorf("Gate = %v, want %v", got.Gate, belay.GateError)
			}
			if got.Gate == belay.GateFail {
				t.Error("a linter that could not run must never route the graph to fix")
			}
			var oe *OutputError
			if errors.As(err, &oe) && oe.Tool != "eslint" {
				t.Errorf("Tool = %q, want %q", oe.Tool, "eslint")
			}
		})
	}
}

func TestESLintCommand(t *testing.T) {
	t.Parallel()

	t.Run("prefers the project's own eslint", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		local := filepath.Join(dir, "node_modules", ".bin", "eslint")
		if err := os.MkdirAll(filepath.Dir(local), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// #nosec G306 -- localTool tests the executable bit, so the
		// fixture must carry one. It lives in t.TempDir().
		if err := os.WriteFile(local, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatalf("write: %v", err)
		}

		runner := okRunner("[]")
		linter := NewESLint(WithRunner(runner), WithLogger(quietLogger()))
		if _, err := linter.Lint(t.Context(), dir); err != nil {
			t.Fatalf("Lint: %v", err)
		}

		cmd := runner.lastCall(t)
		if cmd.Path != local {
			t.Errorf("Path = %q, want the project-local binary %q", cmd.Path, local)
		}
		if !filepath.IsAbs(cmd.Path) {
			t.Error("a project-local path must be absolute: a relative program path resolves against belay's own working directory, not the child's")
		}
	})

	t.Run("falls back to PATH", func(t *testing.T) {
		t.Parallel()
		runner := okRunner("[]")
		linter := NewESLint(WithRunner(runner), WithLogger(quietLogger()))
		dir := t.TempDir()
		if _, err := linter.Lint(t.Context(), dir); err != nil {
			t.Fatalf("Lint: %v", err)
		}

		cmd := runner.lastCall(t)
		if cmd.Path != "eslint" {
			t.Errorf("Path = %q, want %q", cmd.Path, "eslint")
		}
		wantArgs := []string{"--format", "json", "."}
		if diff := cmp.Diff(wantArgs, cmd.Args); diff != "" {
			t.Errorf("Args mismatch (-want +got):\n%s", diff)
		}
		if cmd.Dir != dir {
			t.Errorf("Dir = %q, want %q", cmd.Dir, dir)
		}
		if cmd.MaxOutput != DefaultMaxOutput {
			t.Errorf("MaxOutput = %d, want %d", cmd.MaxOutput, DefaultMaxOutput)
		}
		// npx would download and execute code from the network as a
		// side effect of running a quality gate.
		if cmd.Path == "npx" || slices.Contains(cmd.Args, "npx") {
			t.Error("eslint must never be invoked through npx")
		}
		// Suppressing the unmatched-pattern error would report a
		// passing gate for a run that linted no files at all.
		if slices.Contains(cmd.Args, "--no-error-on-unmatched-pattern") {
			t.Error("a run that matches no files must surface as a gate error, not a pass")
		}
	})
}

func TestESLintDetect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{
			name:  "eslint as a devDependency",
			files: map[string]string{"package.json": `{"name":"web","devDependencies":{"eslint":"^9.0.0"}}`},
			want:  true,
		},
		{
			name: "a flat config file",
			files: map[string]string{
				"package.json":      `{"name":"web"}`,
				"eslint.config.mjs": "export default [];\n",
			},
			want: true,
		},
		{
			name:  "a node package with no eslint",
			files: map[string]string{"package.json": `{"name":"web"}`},
			want:  false,
		},
		{
			name:  "a go module is not a node package",
			files: map[string]string{"go.mod": "module example.com/app\n\ngo 1.26\n"},
			want:  false,
		},
		{
			name:  "an empty directory",
			files: map[string]string{},
			want:  false,
		},
	}

	linter := NewESLint(WithRunner(okRunner("")), WithLogger(quietLogger()))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := linter.Detect(writeFiles(t, tc.files)); got != tc.want {
				t.Errorf("Detect() = %v, want %v", got, tc.want)
			}
		})
	}
}
