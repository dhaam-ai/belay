//go:build unix

package linter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/pkg/belay"
)

// TestRuffSeverityTable pins every row, and fails if ruffSeverities gains one
// this test does not name.
func TestRuffSeverityTable(t *testing.T) {
	t.Parallel()

	want := map[string]belay.Severity{
		// Blocker: the file was never analysed.
		"E9": belay.SeverityBlocker,

		// Critical: near-certain runtime failure, or opted-into security.
		"F82": belay.SeverityCritical,
		"S":   belay.SeverityCritical,

		// Major: real defects.
		"F":     belay.SeverityMajor,
		"B":     belay.SeverityMajor,
		"ASYNC": belay.SeverityMajor,
		"PLE":   belay.SeverityMajor,
		"W6":    belay.SeverityMajor,
		"T10":   belay.SeverityMajor,

		// Minor: code health, including four carve-outs from the
		// families above.
		"F401": belay.SeverityMinor,
		"F841": belay.SeverityMinor,
		"S101": belay.SeverityMinor,
		"E":    belay.SeverityMinor,
		"W":    belay.SeverityMinor,
		"C4":   belay.SeverityMinor,
		"C90":  belay.SeverityMinor,
		"SIM":  belay.SeverityMinor,
		"N":    belay.SeverityMinor,
		"UP":   belay.SeverityMinor,
		"RUF":  belay.SeverityMinor,
		"PLR":  belay.SeverityMinor,
		"PLW":  belay.SeverityMinor,
		"PLC":  belay.SeverityMinor,
		"PT":   belay.SeverityMinor,

		// Info: mechanically fixable.
		"I":   belay.SeverityInfo,
		"D":   belay.SeverityInfo,
		"Q":   belay.SeverityInfo,
		"COM": belay.SeverityInfo,
	}

	seen := make(map[string]bool, len(ruffSeverities))
	for _, rule := range ruffSeverities {
		if seen[rule.Prefix] {
			t.Errorf("ruffSeverities has a duplicate row for %q", rule.Prefix)
		}
		seen[rule.Prefix] = true

		wantSeverity, ok := want[rule.Prefix]
		if !ok {
			t.Errorf("ruffSeverities has an untested row %q: name its severity in this test", rule.Prefix)
			continue
		}
		if rule.Severity != wantSeverity {
			t.Errorf("ruffSeverities[%q] = %v, want %v", rule.Prefix, rule.Severity, wantSeverity)
		}
		if strings.TrimSpace(rule.Why) == "" {
			t.Errorf("ruffSeverities[%q] has no rationale; every row must justify itself", rule.Prefix)
		}
	}
	for prefix := range want {
		if !seen[prefix] {
			t.Errorf("ruffSeverities is missing %q", prefix)
		}
	}
}

// TestRuffCodeMatching is the test that matters for the mapping: it pins the
// longest-prefix rule, the letters-must-match-exactly rule, and every carve-out.
func TestRuffCodeMatching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code string
		want belay.Severity
	}{
		// Longest prefix wins over the shorter family row.
		{name: "E999 is a syntax error, not a style nit", code: "E999", want: belay.SeverityBlocker},
		{name: "E902 is an I/O error", code: "E902", want: belay.SeverityBlocker},
		{name: "E501 is line length", code: "E501", want: belay.SeverityMinor},
		{name: "E731 is a style rule", code: "E731", want: belay.SeverityMinor},
		{name: "F821 is an undefined name", code: "F821", want: belay.SeverityCritical},
		{name: "F823 is an undefined local", code: "F823", want: belay.SeverityCritical},
		{name: "F811 stays a pyflakes defect", code: "F811", want: belay.SeverityMajor},
		{name: "F401 is a demoted unused import", code: "F401", want: belay.SeverityMinor},
		{name: "F841 is a demoted unused variable", code: "F841", want: belay.SeverityMinor},
		{name: "S105 is a security finding", code: "S105", want: belay.SeverityCritical},
		{name: "S608 is a security finding", code: "S608", want: belay.SeverityCritical},
		{name: "S101 is the assert carve-out", code: "S101", want: belay.SeverityMinor},
		{name: "W605 is an invalid escape sequence", code: "W605", want: belay.SeverityMajor},
		{name: "W291 is trailing whitespace", code: "W291", want: belay.SeverityMinor},
		{name: "B006 is a mutable default argument", code: "B006", want: belay.SeverityMajor},
		{name: "T100 is a stray debugger", code: "T100", want: belay.SeverityMajor},
		{name: "PLE0101 is a pylint error", code: "PLE0101", want: belay.SeverityMajor},
		{name: "PLR0913 is a pylint refactor hint", code: "PLR0913", want: belay.SeverityMinor},
		{name: "I001 is import ordering", code: "I001", want: belay.SeverityInfo},
		{name: "D100 is a missing docstring", code: "D100", want: belay.SeverityInfo},

		// Letters must match exactly, or a one-letter row would swallow
		// every rule set that starts with the same letter.
		{name: "ISC001 is not an isort rule", code: "ISC001", want: defaultRuffSeverity.Severity},
		{name: "INP001 is not an isort rule", code: "INP001", want: defaultRuffSeverity.Severity},
		{name: "BLE001 is not a bugbear rule", code: "BLE001", want: defaultRuffSeverity.Severity},
		{name: "SLF001 is not a bandit rule", code: "SLF001", want: defaultRuffSeverity.Severity},
		{name: "TID252 is not a debugger rule", code: "TID252", want: defaultRuffSeverity.Severity},
		{name: "EM101 is not a pycodestyle rule", code: "EM101", want: defaultRuffSeverity.Severity},

		// Unknown rule sets take the safe default rather than the
		// hardcoded "error" severity that would inflate them to major.
		{name: "an entirely unknown code", code: "ZZZ999", want: defaultRuffSeverity.Severity},

		// Normalization.
		{name: "lowercase codes still match", code: "f821", want: belay.SeverityCritical},
		{name: "surrounding space still matches", code: "  S101  ", want: belay.SeverityMinor},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Every one of these arrives with ruff's hardcoded
			// "error" severity string, which must change nothing.
			got, why := ruffSeverity(tc.code, "error")
			if got != tc.want {
				t.Errorf("ruffSeverity(%q) = %v, want %v", tc.code, got, tc.want)
			}
			if strings.TrimSpace(why) == "" {
				t.Error("resolution returned no rationale")
			}
		})
	}
}

func TestRuffSeverityStringFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		code     string
		severity string
		want     belay.Severity
	}{
		// Preview output can omit the code; there the severity is real.
		{name: "no code, fatal", severity: "fatal", want: belay.SeverityBlocker},
		{name: "no code, error", severity: "error", want: belay.SeverityMajor},
		{name: "no code, warning", severity: "warning", want: belay.SeverityMinor},
		{name: "no code, info", severity: "info", want: belay.SeverityInfo},
		{name: "no code, unrecognized severity", severity: "meltdown", want: defaultRuffSeverity.Severity},
		{name: "no code, no severity", want: defaultRuffSeverity.Severity},
		{name: "no code, mixed case severity", severity: " ERROR ", want: belay.SeverityMajor},

		// The point of the whole design: with a code present the
		// severity string is ignored, because stable ruff hardcodes it
		// to "error" and every finding would otherwise become major.
		{
			name:     "an unknown code ignores the hardcoded error severity",
			code:     "ZZZ999",
			severity: "error",
			want:     defaultRuffSeverity.Severity,
		},
		{
			name:     "a known code ignores a conflicting severity",
			code:     "I001",
			severity: "fatal",
			want:     belay.SeverityInfo,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, _ := ruffSeverity(tc.code, tc.severity); got != tc.want {
				t.Errorf("ruffSeverity(%q, %q) = %v, want %v", tc.code, tc.severity, got, tc.want)
			}
		})
	}
}

func TestSplitRuffCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code        string
		wantLetters string
		wantDigits  string
	}{
		{code: "F401", wantLetters: "F", wantDigits: "401"},
		{code: "PLR0913", wantLetters: "PLR", wantDigits: "0913"},
		{code: "E9", wantLetters: "E", wantDigits: "9"},
		{code: "RUF", wantLetters: "RUF", wantDigits: ""},
		{code: "", wantLetters: "", wantDigits: ""},
	}

	for _, tc := range tests {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			letters, digits := splitRuffCode(tc.code)
			if letters != tc.wantLetters || digits != tc.wantDigits {
				t.Errorf("splitRuffCode(%q) = (%q, %q), want (%q, %q)",
					tc.code, letters, digits, tc.wantLetters, tc.wantDigits)
			}
		})
	}
}

func TestRuffParse(t *testing.T) {
	t.Parallel()

	issues, raw, err := parseRuff("/repo", []byte(readFixture(t, "ruff-issues.json")))
	if err != nil {
		t.Fatalf("parseRuff: %v", err)
	}

	want := []belay.Issue{
		{RuleID: "E999", Severity: belay.SeverityBlocker, File: "app/broken.py", Line: 3, Message: "SyntaxError: Expected an expression"},
		{RuleID: "F821", Severity: belay.SeverityCritical, File: "app/service.py", Line: 14, Message: "Undefined name `reqeusts`"},
		{RuleID: "S105", Severity: belay.SeverityCritical, File: "app/config.py", Line: 21, Message: `Possible hardcoded password assigned to: "token"`},
		{RuleID: "B006", Severity: belay.SeverityMajor, File: "app/service.py", Line: 8, Message: "Do not use mutable data structures for argument defaults"},
		{RuleID: "S101", Severity: belay.SeverityMinor, File: "tests/test_service.py", Line: 5, Message: "Use of `assert` detected"},
		{RuleID: "F401", Severity: belay.SeverityMinor, File: "app/service.py", Line: 1, Message: "`os` imported but unused"},
		{RuleID: "E501", Severity: belay.SeverityMinor, File: "app/service.py", Line: 31, Message: "Line too long (119 > 88)"},
		{RuleID: "I001", Severity: belay.SeverityInfo, File: "app/service.py", Line: 1, Message: "Import block is un-sorted or un-formatted"},
		{RuleID: "ZZZ999", Severity: belay.SeverityMinor, File: "app/service.py", Line: 40, Message: "a rule set belay has never heard of"},
	}

	if diff := cmp.Diff(want, issues); diff != "" {
		t.Errorf("issues mismatch (-want +got):\n%s", diff)
	}
	if !json.Valid(raw) {
		t.Errorf("raw is not valid JSON: %s", raw)
	}
}

func TestRuffParseOptionalFields(t *testing.T) {
	t.Parallel()

	// A null location and a null code, both of which ruff emits: the
	// former for a whole-file finding, the latter in preview mode.
	stdout := `[{"cell":null,"code":null,"name":"unsorted-imports","severity":"warning",
	  "filename":"/repo/app/x.py","fix":null,"location":null,"end_location":null,
	  "message":"whole-file finding","noqa_row":null,"url":null}]`

	issues, _, err := parseRuff("/repo", []byte(stdout))
	if err != nil {
		t.Fatalf("parseRuff: %v", err)
	}
	want := []belay.Issue{{
		RuleID:   "unsorted-imports",
		Severity: belay.SeverityMinor,
		File:     "app/x.py",
		Line:     0,
		Message:  "whole-file finding",
	}}
	if diff := cmp.Diff(want, issues); diff != "" {
		t.Errorf("issues mismatch (-want +got):\n%s", diff)
	}

	// Neither a code nor a name: the RuleID still has to be something.
	bare := `[{"code":null,"name":"","severity":"error","filename":"x.py","message":"bare","location":null}]`
	issues, _, err = parseRuff("/repo", []byte(bare))
	if err != nil {
		t.Fatalf("parseRuff: %v", err)
	}
	if len(issues) != 1 || issues[0].RuleID != ruffFallbackRuleID {
		t.Errorf("RuleID = %+v, want %q", issues, ruffFallbackRuleID)
	}
}

func TestRuffLintFindingsAreNotAnError(t *testing.T) {
	t.Parallel()

	// ruff documents exit 1 as "violations were found".
	linter := NewRuff(
		WithRunner(findingsRunner(readFixture(t, "ruff-issues.json"), 1)),
		WithLogger(quietLogger()),
	)

	got, err := linter.Lint(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Lint returned an error for a run that found issues: %v", err)
	}
	if got.Gate != belay.GateFail {
		t.Errorf("Gate = %v, want %v", got.Gate, belay.GateFail)
	}
	if got.Source != "ruff" {
		t.Errorf("Source = %q, want %q", got.Source, "ruff")
	}
	wantCounts := belay.Counts{Blocker: 1, Critical: 2, Major: 1, Minor: 4, Info: 1}
	if diff := cmp.Diff(wantCounts, got.Counts); diff != "" {
		t.Errorf("Counts mismatch (-want +got):\n%s", diff)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("report does not round-trip through JSON: %v", err)
	}
}

func TestRuffLintClean(t *testing.T) {
	t.Parallel()

	linter := NewRuff(WithRunner(okRunner(readFixture(t, "ruff-clean.json"))), WithLogger(quietLogger()))

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
	if got.Summary != "ruff: no issues; gate pass at fail_on=major" {
		t.Errorf("Summary = %q", got.Summary)
	}
}

func TestRuffLintMissingToolchain(t *testing.T) {
	t.Parallel()

	linter := NewRuff(WithRunner(missingRunner("ruff")), WithLogger(quietLogger()))

	got, err := linter.Lint(t.Context(), t.TempDir())

	if !errors.Is(err, belay.ErrToolchainMissing) {
		t.Fatalf("errors.Is(err, belay.ErrToolchainMissing) = false, err = %v", err)
	}
	var te *belay.ToolchainError
	if !errors.As(err, &te) {
		t.Fatalf("errors.As(*belay.ToolchainError) = false, err = %v", err)
	}
	if te.Tool != "ruff" {
		t.Errorf("Tool = %q, want %q", te.Tool, "ruff")
	}
	if diff := cmp.Diff(belay.QualityReport{}, got); diff != "" {
		t.Errorf("report is not zero (-want +got):\n%s", diff)
	}
}

func TestRuffLintCannotRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		runner *stubRunner
	}{
		{name: "malformed json", runner: findingsRunner(readFixture(t, "malformed.json"), 1)},
		{
			// ruff documents exit 2 as terminating abnormally due to
			// invalid configuration, CLI options, or an internal error.
			name: "an invalid configuration",
			runner: &stubRunner{
				exitCode: 2,
				stderr:   "ruff failed\n  Cause: Failed to parse pyproject.toml",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			linter := NewRuff(WithRunner(tc.runner), WithLogger(quietLogger()))

			got, err := linter.Lint(t.Context(), t.TempDir())

			if !errors.Is(err, ErrUnparsableOutput) {
				t.Fatalf("errors.Is(err, ErrUnparsableOutput) = false, err = %v", err)
			}
			if got.Gate != belay.GateError {
				t.Errorf("Gate = %v, want %v", got.Gate, belay.GateError)
			}
			if errors.Is(err, belay.ErrToolchainMissing) {
				t.Error("a linter that ran must not look like a missing toolchain")
			}
		})
	}
}

func TestRuffLintGateThresholds(t *testing.T) {
	t.Parallel()

	// One S105 hardcoded-password finding, which the table places at
	// critical.
	stdout := `[{"code":"S105","filename":"/repo/app/config.py","location":{"row":21,"column":12},` +
		`"message":"Possible hardcoded password assigned to: \"token\"","severity":"error"}]`

	tests := []struct {
		name   string
		failOn belay.Severity
		want   belay.GateStatus
	}{
		{name: "major gate fails on a critical finding", failOn: belay.SeverityMajor, want: belay.GateFail},
		{name: "critical gate fails on a critical finding", failOn: belay.SeverityCritical, want: belay.GateFail},
		{name: "blocker gate passes a critical finding", failOn: belay.SeverityBlocker, want: belay.GatePass},
		{name: "info gate fails on a critical finding", failOn: belay.SeverityInfo, want: belay.GateFail},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			linter := NewRuff(
				WithRunner(findingsRunner(stdout, 1)),
				WithLogger(quietLogger()),
				WithFailOn(tc.failOn),
			)
			got, err := linter.Lint(t.Context(), t.TempDir())
			if err != nil {
				t.Fatalf("Lint: %v", err)
			}
			if got.Gate != tc.want {
				t.Errorf("Gate = %v, want %v", got.Gate, tc.want)
			}
		})
	}
}

func TestRuffCommand(t *testing.T) {
	t.Parallel()

	t.Run("prefers the project's virtualenv", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		local := filepath.Join(dir, ".venv", "bin", "ruff")
		if err := os.MkdirAll(filepath.Dir(local), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// #nosec G306 -- localTool tests the executable bit, so the
		// fixture must carry one. It lives in t.TempDir().
		if err := os.WriteFile(local, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatalf("write: %v", err)
		}

		runner := okRunner("[]")
		linter := NewRuff(WithRunner(runner), WithLogger(quietLogger()))
		if _, err := linter.Lint(t.Context(), dir); err != nil {
			t.Fatalf("Lint: %v", err)
		}
		if cmd := runner.lastCall(t); cmd.Path != local {
			t.Errorf("Path = %q, want the virtualenv binary %q", cmd.Path, local)
		}
	})

	t.Run("falls back to PATH", func(t *testing.T) {
		t.Parallel()
		runner := okRunner("[]")
		linter := NewRuff(WithRunner(runner), WithLogger(quietLogger()))
		dir := t.TempDir()
		if _, err := linter.Lint(t.Context(), dir); err != nil {
			t.Fatalf("Lint: %v", err)
		}

		cmd := runner.lastCall(t)
		if cmd.Path != "ruff" {
			t.Errorf("Path = %q, want %q", cmd.Path, "ruff")
		}
		wantArgs := []string{"check", "--output-format", "json", "."}
		if diff := cmp.Diff(wantArgs, cmd.Args); diff != "" {
			t.Errorf("Args mismatch (-want +got):\n%s", diff)
		}
		if cmd.Dir != dir {
			t.Errorf("Dir = %q, want %q", cmd.Dir, dir)
		}
		if cmd.MaxOutput != DefaultMaxOutput {
			t.Errorf("MaxOutput = %d, want %d", cmd.MaxOutput, DefaultMaxOutput)
		}
	})
}

func TestRuffDetect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{
			name:  "ruff configured in pyproject.toml",
			files: map[string]string{"pyproject.toml": "[tool.ruff]\nline-length = 88\n"},
			want:  true,
		},
		{
			name: "a standalone ruff.toml",
			files: map[string]string{
				"pyproject.toml": "[project]\nname = \"app\"\n",
				"ruff.toml":      "line-length = 88\n",
			},
			want: true,
		},
		{
			name:  "a python project with no ruff",
			files: map[string]string{"pyproject.toml": "[project]\nname = \"app\"\n"},
			want:  false,
		},
		{
			name:  "a go module is not a python project",
			files: map[string]string{"go.mod": "module example.com/app\n\ngo 1.26\n"},
			want:  false,
		},
		{
			name:  "an empty directory",
			files: map[string]string{},
			want:  false,
		},
	}

	linter := NewRuff(WithRunner(okRunner("")), WithLogger(quietLogger()))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := linter.Detect(writeFiles(t, tc.files)); got != tc.want {
				t.Errorf("Detect() = %v, want %v", got, tc.want)
			}
		})
	}
}
