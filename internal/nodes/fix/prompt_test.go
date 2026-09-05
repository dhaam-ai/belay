package fix

import (
	"errors"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
)

func failingTestState() state.Test {
	return state.Test{
		Total: 14, Passed: 12, Failed: 2,
		ReportPath: "artifacts/test.json",
		Failures: []belay.TestFailure{
			{Name: "TestLogin/empty_password", File: "internal/auth/login_test.go", Line: 42,
				Message: "login_test.go:42: status = 500, want 400"},
			{Name: "TestLogin/locked_account", File: "internal/auth/login_test.go", Line: 61,
				Message: "panic: runtime error: index out of range [3] with length 2"},
		},
	}
}

func failedGateReview() state.Review {
	return state.Review{
		Source: "golangci-lint",
		Gate:   belay.GateFail,
		Counts: belay.Counts{Blocker: 1, Major: 2},
		Issues: []belay.Issue{
			{RuleID: "revive:exported", Severity: belay.SeverityMajor, File: "internal/auth/login.go", Line: 8,
				Message: "exported function Login should have comment"},
			{RuleID: "gosec:G401", Severity: belay.SeverityBlocker, File: "internal/crypto/hash.go", Line: 17,
				Message: "Use of weak cryptographic primitive", Effort: "5min"},
			{RuleID: "staticcheck:SA4006", Severity: belay.SeverityMajor, File: "internal/auth/limiter.go", Line: 3,
				Message: "value never read"},
		},
		Summary: "3 issues (1 blocker, 2 major)",
	}
}

// TestDetectCause pins the two entry paths and the precedence between them.
// Getting this wrong does not fail loudly: it silently sends the agent to
// repair the wrong thing and burns an attempt doing it.
func TestDetectCause(t *testing.T) {
	tests := []struct {
		name  string
		st    state.State
		want  cause
		label string
	}{
		{
			name: "green run has no cause",
			st: state.State{
				Test:   state.Test{Total: 14, Passed: 14, ReportPath: "artifacts/test.json"},
				Review: state.Review{Gate: belay.GatePass},
			},
			want: causeNone, label: "none",
		},
		{
			name: "nothing has run yet",
			st:   state.State{},
			want: causeNone, label: "none",
		},
		{
			name: "failing tests",
			st:   state.State{Test: failingTestState()},
			want: causeTests, label: "failing_tests",
		},
		{
			name: "failed review gate",
			st: state.State{
				Test:   state.Test{Total: 14, Passed: 14, ReportPath: "artifacts/test.json"},
				Review: failedGateReview(),
			},
			want: causeReview, label: "failed_review_gate",
		},
		{
			name: "both failing: tests win",
			st:   state.State{Test: failingTestState(), Review: failedGateReview()},
			want: causeTests, label: "failing_tests",
		},
		{
			name: "gate error is not the fix loop's job",
			st: state.State{
				Test:   state.Test{Total: 14, Passed: 14, ReportPath: "artifacts/test.json"},
				Review: state.Review{Gate: belay.GateError, Source: "sonar"},
			},
			want: causeNone, label: "none",
		},
		{
			name: "suite discovered no tests",
			st:   state.State{Test: state.Test{ReportPath: "artifacts/test.json"}},
			want: causeTests, label: "failing_tests",
		},
		{
			name: "failures reported without counts",
			st:   state.State{Test: state.Test{Failures: []belay.TestFailure{{Name: "TestX"}}}},
			want: causeTests, label: "failing_tests",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectCause(tt.st); got != tt.want {
				t.Fatalf("detectCause() = %v, want %v", got, tt.want)
			}
			if got := tt.want.String(); got != tt.label {
				t.Fatalf("cause.String() = %q, want %q", got, tt.label)
			}
		})
	}
}

// TestBuildPromptFailingTests asserts the prompt names the concrete
// failures. A prompt that says "the tests failed, please fix" wastes the
// attempt: the agent has to rediscover what this node already knows.
func TestBuildPromptFailingTests(t *testing.T) {
	got := buildPrompt(promptInput{
		Goal:         "add rate limiting to the login handler",
		Cause:        causeTests,
		Attempt:      2,
		GiveUp:       3,
		Test:         failingTestState(),
		ChangedFiles: []string{"internal/auth/login.go", "internal/auth/limiter.go"},
	})

	for _, want := range []string{
		"add rate limiting to the login handler",
		"2 of 14 tests failed (12 passed)",
		"artifacts/test.json",
		"TestLogin/empty_password",
		"internal/auth/login_test.go:42",
		"status = 500, want 400",
		"TestLogin/locked_account",
		"internal/auth/login_test.go:61",
		"index out of range",
		"internal/auth/limiter.go",
		"Do not delete, skip, rename, or weaken a test",
		"This is fix attempt 2 of 3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, got)
		}
	}
	if strings.Contains(got, "quality gate") {
		t.Errorf("test-caused prompt should not talk about the quality gate:\n%s", got)
	}
}

// TestBuildPromptFailedGate asserts the other entry path renders concrete
// issues, most severe first.
func TestBuildPromptFailedGate(t *testing.T) {
	got := buildPrompt(promptInput{
		Goal:    "add rate limiting to the login handler",
		Cause:   causeReview,
		Attempt: 1,
		GiveUp:  3,
		Review:  failedGateReview(),
	})

	for _, want := range []string{
		"golangci-lint failed the quality gate with 3 issues: 1 blocker, 2 major",
		"Reviewer summary: 3 issues (1 blocker, 2 major)",
		"[blocker] gosec:G401",
		"internal/crypto/hash.go:17",
		"Use of weak cryptographic primitive",
		"estimated effort: 5min",
		"[major] revive:exported",
		"Do not silence a finding",
		"This is fix attempt 1 of 3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, got)
		}
	}

	blocker := strings.Index(got, "gosec:G401")
	major := strings.Index(got, "revive:exported")
	if blocker < 0 || major < 0 || blocker > major {
		t.Errorf("blocker must be listed before major issues; got blocker at %d, major at %d\n%s", blocker, major, got)
	}
}

// TestBuildPromptDeterministic guards cassette replay: the same input must
// render the same bytes, including issue ordering, which arrives in no
// guaranteed order from a reviewer.
func TestBuildPromptDeterministic(t *testing.T) {
	r := failedGateReview()
	shuffled := state.Review{
		Source: r.Source, Gate: r.Gate, Counts: r.Counts, Summary: r.Summary,
		Issues: []belay.Issue{r.Issues[2], r.Issues[0], r.Issues[1]},
	}
	in := promptInput{Goal: "g", Cause: causeReview, Attempt: 1, GiveUp: 3, Review: r}
	other := in
	other.Review = shuffled

	if a, b := buildPrompt(in), buildPrompt(in); a != b {
		t.Error("buildPrompt is not deterministic for identical input")
	}
	if a, b := buildPrompt(in), buildPrompt(other); a != b {
		t.Errorf("issue order changed the prompt:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
	if len(shuffled.Issues) != 3 || shuffled.Issues[0].RuleID != "staticcheck:SA4006" {
		t.Error("buildPrompt must not sort the caller's own slice in place")
	}
}

// TestBuildPromptCaps proves the prompt stays bounded when a shared helper
// breaks and the suite reports hundreds of failures.
func TestBuildPromptCaps(t *testing.T) {
	failures := make([]belay.TestFailure, 0, 25)
	for i := range 25 {
		failures = append(failures, belay.TestFailure{Name: "TestBig", Line: i + 1, File: "big_test.go",
			Message: strings.Repeat("x", 4000)})
	}
	got := buildPrompt(promptInput{
		Cause: causeTests, Attempt: 1, GiveUp: 3,
		Test:         state.Test{Total: 30, Failed: 25, Failures: failures},
		ChangedFiles: makeFiles(30),
	})

	if n := strings.Count(got, "big_test.go:"); n != maxPromptFailures {
		t.Errorf("listed %d failures, want the cap of %d", n, maxPromptFailures)
	}
	if !strings.Contains(got, "...and 15 more failing tests") {
		t.Errorf("prompt does not say how much it left out:\n%s", got)
	}
	if !strings.Contains(got, "(message truncated)") {
		t.Error("a 4000-character failure message should be truncated")
	}
	if !strings.Contains(got, "...and 5 more") {
		t.Errorf("changed-file list is not capped:\n%s", got)
	}
	if strings.Contains(got, strings.Repeat("x", maxMessageChars+1)) {
		t.Error("a message survived past the truncation cap")
	}
}

// TestBuildPromptDegradedInputs covers the failure records a runner or a
// reviewer can legitimately hand over half-populated. None of them may
// produce a prompt with nothing concrete in it.
func TestBuildPromptDegradedInputs(t *testing.T) {
	tests := []struct {
		name string
		in   promptInput
		want []string
	}{
		{
			name: "suite discovered no tests",
			in: promptInput{Cause: causeTests, Attempt: 1, GiveUp: 2,
				Test: state.Test{ReportPath: "artifacts/test.json"}},
			want: []string{"discovered no tests at all", "artifacts/test.json"},
		},
		{
			name: "failures counted but not attributed",
			in: promptInput{Cause: causeTests, Attempt: 1, GiveUp: 2,
				Test: state.Test{Total: 9, Passed: 6, Failed: 3}},
			want: []string{"3 of 9 tests failed", "could not attribute them to individual tests"},
		},
		{
			name: "failure with neither file nor line",
			in: promptInput{Cause: causeTests, Attempt: 1, GiveUp: 2,
				Test: state.Test{Total: 1, Failed: 1, Failures: []belay.TestFailure{{Message: "boom"}}}},
			want: []string{"(unnamed test)", "boom"},
		},
		{
			name: "gate failed with no issues listed",
			in: promptInput{Cause: causeReview, Attempt: 1, GiveUp: 2,
				Review: state.Review{Gate: belay.GateFail, Summary: "the diff drops error handling"}},
			want: []string{"failed the quality gate", "the diff drops error handling", "without listing individual issues"},
		},
		{
			name: "issues present but counts empty",
			in: promptInput{Cause: causeReview, Attempt: 1, GiveUp: 2,
				Review: state.Review{Gate: belay.GateFail, Issues: []belay.Issue{{RuleID: "x", Message: "m"}}}},
			want: []string{"with 1 issues", "[info] x"},
		},
		{
			name: "no goal recorded",
			in: promptInput{Cause: causeTests, Attempt: 1, GiveUp: 2,
				Test: state.Test{Total: 1, Failed: 1, Failures: []belay.TestFailure{{Name: "TestA"}}}},
			want: []string{"TestA", "What to do"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPrompt(tt.in)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, got)
				}
			}
		})
	}
}

func TestLocationAndIndent(t *testing.T) {
	tests := []struct {
		file string
		line int
		want string
	}{
		{"a.go", 7, "a.go:7"},
		{"a.go", 0, "a.go"},
		{"", 7, "line 7"},
		{"", 0, ""},
		{"  ", 0, ""},
	}
	for _, tt := range tests {
		if got := location(tt.file, tt.line); got != tt.want {
			t.Errorf("location(%q, %d) = %q, want %q", tt.file, tt.line, got, tt.want)
		}
	}

	if got, want := indent("a\n\nb", "  "), "  a\n\n  b"; got != want {
		t.Errorf("indent() = %q, want %q", got, want)
	}
}

// TestTruncateCutsOnRuneBoundary guards against handing an agent invalid
// UTF-8, which some backends reject outright.
func TestTruncateCutsOnRuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 20) // two bytes per rune
	got := truncate(s, 9)
	if !strings.HasPrefix(got, strings.Repeat("é", 4)) {
		t.Errorf("truncate() = %q, want it to keep whole runes", got)
	}
	if !strings.Contains(got, "(message truncated)") {
		t.Errorf("truncate() = %q, want a truncation marker", got)
	}
	if got := truncate("short", 100); got != "short" {
		t.Errorf("truncate() = %q, want it to leave short input alone", got)
	}
}

func makeFiles(n int) []string {
	out := make([]string, 0, n)
	for range n {
		out = append(out, "pkg/file.go")
	}
	return out
}

// update rewrites the golden prompts instead of asserting against them:
// go test ./internal/nodes/fix/... -update
var update = flag.Bool("update", false, "rewrite the golden prompt files")

// TestBuildPromptGolden pins both entry paths' prompts byte for byte.
//
// The prompt is this node's product, and a product deserves review: a
// golden file makes a change to the wording show up as a reviewable diff
// rather than as a quietly worse instruction the tests still pass on.
func TestBuildPromptGolden(t *testing.T) {
	tests := []struct {
		name   string
		golden string
		in     promptInput
	}{
		{
			name:   "failing tests",
			golden: "testdata/prompt_failing_tests.golden",
			in: promptInput{
				Goal:         "add rate limiting to the login handler",
				Cause:        causeTests,
				Attempt:      2,
				GiveUp:       3,
				Test:         failingTestState(),
				ChangedFiles: []string{"internal/auth/login.go", "internal/auth/limiter.go"},
			},
		},
		{
			name:   "failed review gate",
			golden: "testdata/prompt_failed_gate.golden",
			in: promptInput{
				Goal:         "add rate limiting to the login handler",
				Cause:        causeReview,
				Attempt:      1,
				GiveUp:       3,
				Review:       failedGateReview(),
				ChangedFiles: []string{"internal/auth/login.go"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPrompt(tt.in)
			if *update {
				if err := os.MkdirAll("testdata", 0o750); err != nil {
					t.Fatalf("create testdata: %v", err)
				}
				if err := os.WriteFile(tt.golden, []byte(got), 0o600); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(tt.golden) //nolint:gosec // fixed testdata path
			if err != nil {
				t.Fatalf("read golden (regenerate with -update): %v", err)
			}
			if diff := cmp.Diff(string(want), got); diff != "" {
				t.Errorf("prompt differs from %s (-want +got):\n%s", tt.golden, diff)
			}
		})
	}
}

// TestCauseNoteAndExhaustedNote covers the strings that end up in the
// journal. They carry counts only: a Result.Note is journalled verbatim,
// and captured tool output is exactly where a leaked secret would be.
func TestCauseNoteAndExhaustedNote(t *testing.T) {
	tests := []struct {
		name string
		c    cause
		st   state.State
		want string
	}{
		{"failing tests", causeTests, state.State{Test: failingTestState()}, "2 of 14 tests failing"},
		{"no tests discovered", causeTests, state.State{Test: state.Test{ReportPath: "artifacts/test.json"}},
			"the test suite discovered no tests"},
		{"failed gate", causeReview, state.State{Review: failedGateReview()}, "quality gate failed with 3 issues"},
		{"no cause", causeNone, state.State{}, "nothing recorded as failing"},
		{"out of range", cause(99), state.State{}, "cause(99)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := causeNote(tt.c, tt.st); got != tt.want {
				t.Fatalf("causeNote() = %q, want %q", got, tt.want)
			}
		})
	}

	note := exhaustedNote(3, 3, causeTests, state.State{Test: failingTestState()})
	for _, want := range []string{giveUpNote, "3 of 3 fix attempts used", "2 of 14 tests failing"} {
		if !strings.Contains(note, want) {
			t.Errorf("exhaustedNote() = %q, want it to contain %q", note, want)
		}
	}
	if got := exhaustedNote(0, 0, causeNone, state.State{}); !strings.Contains(got, "no fix attempt is permitted") {
		t.Errorf("exhaustedNote() = %q, want it to explain a non-positive cap", got)
	}
	for _, msg := range []string{"login_test.go:42", "status = 500"} {
		if strings.Contains(note, msg) {
			t.Errorf("exhaustedNote() leaked captured output %q: %s", msg, note)
		}
	}
}

// TestWorkDirRejectsAShallowLayout: a layout with no workspace above it
// must fail loudly rather than send the agent to "." — the belay process's
// own working directory, which is very unlikely to be the repository.
func TestWorkDirRejectsAShallowLayout(t *testing.T) {
	shallow, err := state.NewLayout("", "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if _, err := workDir(shallow); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("workDir() error = %v, want ErrNoWorkspace", err)
	}
	if _, err := workDir(state.Layout{}); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("workDir(zero) error = %v, want ErrNoWorkspace", err)
	}
}

// TestCompareIssuesIsATotalOrder: ties on severity fall through to file,
// line and rule, so no two distinct issues ever compare equal and the
// rendered order cannot drift between runs.
func TestCompareIssuesIsATotalOrder(t *testing.T) {
	base := belay.Issue{RuleID: "b", Severity: belay.SeverityMajor, File: "b.go", Line: 5}
	tests := []struct {
		name string
		x, y belay.Issue
		want int
	}{
		{"higher severity first", belay.Issue{Severity: belay.SeverityBlocker}, belay.Issue{Severity: belay.SeverityInfo}, -1},
		{"lower severity last", belay.Issue{Severity: belay.SeverityInfo}, belay.Issue{Severity: belay.SeverityBlocker}, 1},
		{"then by file", base, belay.Issue{RuleID: "b", Severity: belay.SeverityMajor, File: "c.go", Line: 5}, -1},
		{"then by line", base, belay.Issue{RuleID: "b", Severity: belay.SeverityMajor, File: "b.go", Line: 9}, -1},
		{"then by rule", base, belay.Issue{RuleID: "c", Severity: belay.SeverityMajor, File: "b.go", Line: 5}, -1},
		{"identical", base, base, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compareIssues(tt.x, tt.y); got != tt.want {
				t.Fatalf("compareIssues() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestReviewIssueCap keeps a reviewer with hundreds of findings from
// blowing the prompt budget on every remaining attempt.
func TestReviewIssueCap(t *testing.T) {
	issues := make([]belay.Issue, 0, maxPromptIssues+4)
	for i := range maxPromptIssues + 4 {
		issues = append(issues, belay.Issue{RuleID: "r", Severity: belay.SeverityMajor, File: "a.go", Line: i + 1,
			Message: "problem"})
	}
	got := buildPrompt(promptInput{Cause: causeReview, Attempt: 1, GiveUp: 3,
		Review: state.Review{Gate: belay.GateFail, Source: "linter", Issues: issues,
			Counts: belay.Counts{Major: len(issues)}}})

	if n := strings.Count(got, "[major] r"); n != maxPromptIssues {
		t.Errorf("listed %d issues, want the cap of %d", n, maxPromptIssues)
	}
	if !strings.Contains(got, "...and 4 more issues") {
		t.Errorf("prompt does not say how many issues it left out:\n%s", got)
	}
}
