//go:build unix

package linter

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestGolangCISeverityTable pins every row of the mapping table.
//
// The second half of the test is the important half: it fails if
// golangciSeverities gains a row this test does not name, so a new mapping
// cannot land without someone writing down what it should be.
func TestGolangCISeverityTable(t *testing.T) {
	t.Parallel()

	want := map[string]belay.Severity{
		// Blocker: the code does not build.
		"typecheck": belay.SeverityBlocker,

		// Critical: security.
		"gosec": belay.SeverityCritical,

		// Major: correctness defects.
		"govet":         belay.SeverityMajor,
		"staticcheck":   belay.SeverityMajor,
		"errcheck":      belay.SeverityMajor,
		"errorlint":     belay.SeverityMajor,
		"bodyclose":     belay.SeverityMajor,
		"sqlclosecheck": belay.SeverityMajor,
		"rowserrcheck":  belay.SeverityMajor,
		"nilerr":        belay.SeverityMajor,
		"nilnesserr":    belay.SeverityMajor,
		"noctx":         belay.SeverityMajor,
		"contextcheck":  belay.SeverityMajor,
		"makezero":      belay.SeverityMajor,
		"durationcheck": belay.SeverityMajor,
		"exhaustive":    belay.SeverityMajor,

		// Minor: code health.
		"unused":        belay.SeverityMinor,
		"ineffassign":   belay.SeverityMinor,
		"revive":        belay.SeverityMinor,
		"gocritic":      belay.SeverityMinor,
		"unconvert":     belay.SeverityMinor,
		"unparam":       belay.SeverityMinor,
		"prealloc":      belay.SeverityMinor,
		"gocyclo":       belay.SeverityMinor,
		"cyclop":        belay.SeverityMinor,
		"gocognit":      belay.SeverityMinor,
		"funlen":        belay.SeverityMinor,
		"maintidx":      belay.SeverityMinor,
		"dupl":          belay.SeverityMinor,
		"goconst":       belay.SeverityMinor,
		"nakedret":      belay.SeverityMinor,
		"predeclared":   belay.SeverityMinor,
		"usestdlibvars": belay.SeverityMinor,

		// Info: mechanically fixable.
		"gofmt":      belay.SeverityInfo,
		"gofumpt":    belay.SeverityInfo,
		"goimports":  belay.SeverityInfo,
		"gci":        belay.SeverityInfo,
		"golines":    belay.SeverityInfo,
		"lll":        belay.SeverityInfo,
		"whitespace": belay.SeverityInfo,
		"wsl":        belay.SeverityInfo,
		"wsl_v5":     belay.SeverityInfo,
		"nlreturn":   belay.SeverityInfo,
		"godot":      belay.SeverityInfo,
		"misspell":   belay.SeverityInfo,
		"dupword":    belay.SeverityInfo,
		"godoclint":  belay.SeverityInfo,
	}

	for name, wantSeverity := range want {
		got, ok := golangciSeverities[name]
		if !ok {
			t.Errorf("golangciSeverities is missing %q", name)
			continue
		}
		if got.Severity != wantSeverity {
			t.Errorf("golangciSeverities[%q] = %v, want %v", name, got.Severity, wantSeverity)
		}
		if strings.TrimSpace(got.Why) == "" {
			t.Errorf("golangciSeverities[%q] has no rationale; every row must justify itself", name)
		}
	}

	for name := range golangciSeverities {
		if _, ok := want[name]; !ok {
			t.Errorf("golangciSeverities has an untested row %q: name its severity in this test", name)
		}
	}

	// Promotions past Major stop a run outright, so they are pinned by
	// name rather than only by row.
	if got := slices.Sorted(promotedGolangCI(belay.SeverityCritical)); !slices.Equal(got, []string{"gosec", "typecheck"}) {
		t.Errorf("linters at or above critical = %v, want [gosec typecheck]", got)
	}
}

// promotedGolangCI yields the linters mapped at or above threshold.
func promotedGolangCI(threshold belay.Severity) func(func(string) bool) {
	return func(yield func(string) bool) {
		for name, m := range golangciSeverities {
			if m.Severity >= threshold && !yield(name) {
				return
			}
		}
	}
}

func TestGolangCISeverityResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		fromLinter string
		severity   string
		want       belay.Severity
	}{
		{
			name:       "a known linter wins over its own severity string",
			fromLinter: "gosec",
			severity:   "warning",
			want:       belay.SeverityCritical,
		},
		{
			name:       "a known linter wins over a repository-wide severity default",
			fromLinter: "misspell",
			severity:   "error",
			want:       belay.SeverityInfo,
		},
		{
			name:       "an unknown linter falls back to its severity string",
			fromLinter: "futurelint",
			severity:   "error",
			want:       belay.SeverityMajor,
		},
		{
			name:       "an unknown linter with a warning string is minor",
			fromLinter: "futurelint",
			severity:   "warning",
			want:       belay.SeverityMinor,
		},
		{
			name:       "an unknown linter with an info string is info",
			fromLinter: "futurelint",
			severity:   "info",
			want:       belay.SeverityInfo,
		},
		{
			name:       "an unknown linter with no severity takes the safe default",
			fromLinter: "futurelint",
			severity:   "",
			want:       defaultGolangCISeverity.Severity,
		},
		{
			name:       "an unknown linter with unrecognized severity text takes the safe default",
			fromLinter: "futurelint",
			severity:   "catastrophe",
			want:       defaultGolangCISeverity.Severity,
		},
		{
			name:       "matching ignores case and surrounding space",
			fromLinter: "  GoSec  ",
			severity:   "",
			want:       belay.SeverityCritical,
		},
		{
			name:       "severity strings match case-insensitively too",
			fromLinter: "futurelint",
			severity:   " ERROR ",
			want:       belay.SeverityMajor,
		},
		{
			name:       "a finding with no linter name at all still resolves",
			fromLinter: "",
			severity:   "",
			want:       defaultGolangCISeverity.Severity,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, why := golangciSeverity(tc.fromLinter, tc.severity)
			if got != tc.want {
				t.Errorf("golangciSeverity(%q, %q) = %v, want %v", tc.fromLinter, tc.severity, got, tc.want)
			}
			if strings.TrimSpace(why) == "" {
				t.Error("resolution returned no rationale")
			}
		})
	}
}

func TestGolangCIParse(t *testing.T) {
	t.Parallel()

	issues, raw, err := parseGolangCI(t.TempDir(), []byte(readFixture(t, "golangci-issues.json")))
	if err != nil {
		t.Fatalf("parseGolangCI: %v", err)
	}

	want := []belay.Issue{
		{
			RuleID:   "typecheck",
			Severity: belay.SeverityBlocker,
			File:     "handler.go",
			Line:     1,
			Message:  ": # example.com/app\n./handler.go:14:6: declared and not used: total",
		},
		{
			RuleID:   "gosec",
			Severity: belay.SeverityCritical,
			File:     "crypto.go",
			Line:     22,
			Message:  "G404: Use of weak random number generator (math/rand instead of crypto/rand)",
		},
		{
			RuleID:   "errcheck",
			Severity: belay.SeverityMajor,
			File:     "handler.go",
			Line:     31,
			Message:  "Error return value of `w.Write` is not checked",
		},
		{
			RuleID:   "govet",
			Severity: belay.SeverityMajor,
			File:     "handler.go",
			Line:     44,
			Message:  "printf: non-constant format string in call to fmt.Errorf",
		},
		{
			RuleID:   "revive",
			Severity: belay.SeverityMinor,
			File:     "handler.go",
			Line:     1,
			Message:  "package-comments: should have a package comment",
		},
		{
			RuleID:   "misspell",
			Severity: belay.SeverityInfo,
			File:     "doc.go",
			Line:     7,
			//nolint:misspell // the misspelled word is the fixture data: this is a
			// linter finding *about* a misspelling, quoted verbatim.
			Message: "`recieve` is a misspelling of `receive`",
		},
		{
			RuleID:   "futurelint",
			Severity: belay.SeverityMajor,
			File:     "future.go",
			Line:     3,
			Message:  "a linter belay has never heard of, carrying a severity string",
		},
		{
			RuleID:   "otherlint",
			Severity: belay.SeverityMinor,
			File:     "future.go",
			Line:     4,
			Message:  "a linter belay has never heard of, carrying no severity at all",
		},
	}

	if diff := cmp.Diff(want, issues); diff != "" {
		t.Errorf("issues mismatch (-want +got):\n%s", diff)
	}
	if !json.Valid(raw) {
		t.Errorf("raw is not valid JSON: %s", raw)
	}
}

func TestGolangCILintFindingsAreNotAnError(t *testing.T) {
	t.Parallel()

	fixture := readFixture(t, "golangci-issues.json")
	// golangci-lint exits 1 when it has findings. That is the tool working.
	runner := findingsRunner(fixture, 1)
	linter := NewGolangCI(WithRunner(runner), WithLogger(quietLogger()))

	got, err := linter.Lint(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Lint returned an error for a run that found issues: %v", err)
	}
	if got.Gate != belay.GateFail {
		t.Errorf("Gate = %v, want %v", got.Gate, belay.GateFail)
	}
	if got.Source != "golangci-lint" {
		t.Errorf("Source = %q, want %q", got.Source, "golangci-lint")
	}
	wantCounts := belay.Counts{Blocker: 1, Critical: 1, Major: 3, Minor: 2, Info: 1}
	if diff := cmp.Diff(wantCounts, got.Counts); diff != "" {
		t.Errorf("Counts mismatch (-want +got):\n%s", diff)
	}
	if string(got.Raw) != strings.TrimSpace(fixture) {
		t.Error("Raw does not carry the tool's own JSON document verbatim")
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("report does not round-trip through JSON: %v", err)
	}
}

func TestGolangCILintClean(t *testing.T) {
	t.Parallel()

	linter := NewGolangCI(WithRunner(okRunner(readFixture(t, "golangci-clean.json"))), WithLogger(quietLogger()))

	got, err := linter.Lint(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if got.Gate != belay.GatePass {
		t.Errorf("Gate = %v, want %v", got.Gate, belay.GatePass)
	}
	if len(got.Issues) != 0 || got.Issues == nil {
		t.Errorf("Issues = %v, want an empty non-nil slice", got.Issues)
	}
	if got.Summary != "golangci-lint: no issues; gate pass at fail_on=major" {
		t.Errorf("Summary = %q", got.Summary)
	}
}

// TestGolangCILintTrailingSummary covers the shape golangci-lint v2.12.2
// actually writes to stdout: the JSON document, and then a human-readable
// issue tally on the same stream. Decoding all of stdout as one document fails
// on every run that finds something.
func TestGolangCILintTrailingSummary(t *testing.T) {
	t.Parallel()

	stdout := readFixture(t, "golangci-stdout-with-summary.txt")
	linter := NewGolangCI(WithRunner(findingsRunner(stdout, 1)), WithLogger(quietLogger()))

	got, err := linter.Lint(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if got.Gate != belay.GateFail {
		t.Errorf("Gate = %v, want %v", got.Gate, belay.GateFail)
	}
	if len(got.Issues) != 1 {
		t.Fatalf("len(Issues) = %d, want 1", len(got.Issues))
	}
	if !json.Valid(got.Raw) {
		t.Errorf("Raw is not valid JSON; the trailing summary leaked into it: %s", got.Raw)
	}
	if strings.Contains(string(got.Raw), "issues:") {
		t.Errorf("Raw contains the human summary: %s", got.Raw)
	}
}

func TestGolangCILintMissingToolchain(t *testing.T) {
	t.Parallel()

	linter := NewGolangCI(WithRunner(missingRunner("golangci-lint")), WithLogger(quietLogger()))

	got, err := linter.Lint(t.Context(), t.TempDir())

	// The trap: exec has its own ErrToolchainMissing. If the adapter passes
	// it straight through, this assertion fails and the graph would send an
	// agent to edit code because golangci-lint is not installed.
	if !errors.Is(err, belay.ErrToolchainMissing) {
		t.Fatalf("errors.Is(err, belay.ErrToolchainMissing) = false, err = %v", err)
	}
	var te *belay.ToolchainError
	if !errors.As(err, &te) {
		t.Fatalf("errors.As(*belay.ToolchainError) = false, err = %v", err)
	}
	if te.Tool != "golangci-lint" {
		t.Errorf("Tool = %q, want %q", te.Tool, "golangci-lint")
	}
	if diff := cmp.Diff(belay.QualityReport{}, got); diff != "" {
		t.Errorf("report is not zero (-want +got):\n%s", diff)
	}
}

func TestGolangCILintMalformedOutput(t *testing.T) {
	t.Parallel()

	linter := NewGolangCI(
		WithRunner(findingsRunner(readFixture(t, "malformed.json"), 1)),
		WithLogger(quietLogger()),
	)

	got, err := linter.Lint(t.Context(), t.TempDir())

	if !errors.Is(err, ErrUnparsableOutput) {
		t.Fatalf("errors.Is(err, ErrUnparsableOutput) = false, err = %v", err)
	}
	var oe *OutputError
	if !errors.As(err, &oe) {
		t.Fatalf("errors.As(*OutputError) = false, err = %v", err)
	}
	if oe.Tool != "golangci-lint" {
		t.Errorf("Tool = %q, want %q", oe.Tool, "golangci-lint")
	}
	// Unparsable output must not look like a failing gate: the graph would
	// route to fix with nothing to fix.
	if got.Gate != belay.GateError {
		t.Errorf("Gate = %v, want %v", got.Gate, belay.GateError)
	}
	if errors.Is(err, belay.ErrToolchainMissing) {
		t.Error("unparsable output must not look like a missing toolchain")
	}
}

func TestGolangCILintGateThresholds(t *testing.T) {
	t.Parallel()

	// One misspell finding, which the table places at info.
	stdout := `{"Issues":[{"FromLinter":"misspell","Text":"` +
		//nolint:misspell // the misspelled word is the fixture data.
		"`recieve` is a misspelling of `receive`" +
		`","Severity":"","Pos":{"Filename":"doc.go","Line":7,"Column":4}}],"Report":{}}`

	tests := []struct {
		name   string
		failOn belay.Severity
		want   belay.GateStatus
	}{
		{name: "info gate fails on an info finding", failOn: belay.SeverityInfo, want: belay.GateFail},
		{name: "minor gate passes an info finding", failOn: belay.SeverityMinor, want: belay.GatePass},
		{name: "major gate passes an info finding", failOn: belay.SeverityMajor, want: belay.GatePass},
		{name: "blocker gate passes an info finding", failOn: belay.SeverityBlocker, want: belay.GatePass},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			linter := NewGolangCI(
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

// TestGolangCICommand pins the command line, and with it two decisions: the
// v1-versus-v2 flag spelling (v2.12.2 has no --out-format flag, so passing the
// v1 spelling would fail every invocation rather than degrade), and scoping
// findings to the lines that differ from HEAD. test/lintgate checks what that
// scoping does with the real tool.
func TestGolangCICommand(t *testing.T) {
	t.Parallel()

	runner := okRunner(readFixture(t, "golangci-clean.json"))
	dir := t.TempDir()
	linter := NewGolangCI(WithRunner(runner), WithLogger(quietLogger()), WithTimeout(90*time.Second))

	if _, err := linter.Lint(t.Context(), dir); err != nil {
		t.Fatalf("Lint: %v", err)
	}

	cmd := runner.lastCall(t)
	if cmd.Path != "golangci-lint" {
		t.Errorf("Path = %q, want %q", cmd.Path, "golangci-lint")
	}
	wantArgs := []string{"run", "--output.json.path=stdout", "--new-from-rev=HEAD", "./..."}
	if diff := cmp.Diff(wantArgs, cmd.Args); diff != "" {
		t.Errorf("Args mismatch (-want +got):\n%s", diff)
	}
	if cmd.Dir != dir {
		t.Errorf("Dir = %q, want %q", cmd.Dir, dir)
	}
	if cmd.Timeout != 90*time.Second {
		t.Errorf("Timeout = %v, want %v", cmd.Timeout, 90*time.Second)
	}
	if cmd.MaxOutput != DefaultMaxOutput {
		t.Errorf("MaxOutput = %d, want %d", cmd.MaxOutput, DefaultMaxOutput)
	}
	for _, name := range []string{"GOCACHE", "GOMODCACHE", "GOFLAGS", "GOROOT"} {
		if !slices.Contains(cmd.EnvAllow, name) {
			t.Errorf("EnvAllow is missing %q", name)
		}
	}
	if slices.Contains(cmd.Args, "--out-format") {
		t.Error("the v1 --out-format flag was removed in golangci-lint v2")
	}
}

func TestGolangCIDetect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{
			name:  "a go module at the root",
			files: map[string]string{"go.mod": "module example.com/app\n\ngo 1.26\n"},
			want:  true,
		},
		{
			name:  "a go module below the root",
			files: map[string]string{"services/api/go.mod": "module example.com/api\n\ngo 1.26\n"},
			want:  true,
		},
		{
			name:  "a node package is not a go module",
			files: map[string]string{"package.json": `{"name":"web"}`},
			want:  false,
		},
		{
			name:  "an empty directory",
			files: map[string]string{},
			want:  false,
		},
	}

	linter := NewGolangCI(WithRunner(okRunner("")), WithLogger(quietLogger()))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := linter.Detect(writeFiles(t, tc.files)); got != tc.want {
				t.Errorf("Detect() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("a directory that does not exist", func(t *testing.T) {
		t.Parallel()
		if linter.Detect(filepath.Join(t.TempDir(), "absent")) {
			t.Error("Detect() = true for a missing directory; it must not error or claim it")
		}
	})

	t.Run("detect starts no process", func(t *testing.T) {
		t.Parallel()
		runner := okRunner("")
		l := NewGolangCI(WithRunner(runner), WithLogger(quietLogger()))
		l.Detect(writeFiles(t, map[string]string{"go.mod": "module example.com/app\n"}))
		if runner.callCount() != 0 {
			t.Errorf("Detect ran %d commands, want 0", runner.callCount())
		}
	})
}
