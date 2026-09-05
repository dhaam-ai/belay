package review_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/nodes/review"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
	"github.com/belay-dev/belay/pkg/belay/belaytest"
)

const testRunID = "run-1"

// fixture is one assembled RunContext plus the paths a test needs to inspect
// what the node did with it.
type fixture struct {
	rc        *graph.RunContext
	workspace string
	runDir    string
}

// newFixture builds a RunContext over a real temporary workspace, so
// WriteArtifact exercises the same Layout the dispatcher would hand the node.
func newFixture(t *testing.T, mode config.ReviewMode, failOn config.Severity) *fixture {
	t.Helper()
	ws := t.TempDir()
	layout, err := state.NewLayout(ws, testRunID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	cfg := config.Config{Version: config.Version}
	cfg.Review.Mode = mode
	cfg.Review.FailOn = failOn
	return &fixture{
		workspace: ws,
		runDir:    layout.RunDir(),
		rc: &graph.RunContext{
			Goal:     "make the tests pass",
			Config:   cfg,
			Layout:   layout,
			NodeName: graph.NodeReview,
			Step:     6,
			Attempt:  1,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
}

// report builds a QualityReport with a matching Counts histogram, so a test
// never has to keep issues and counts in sync by hand.
func report(source string, gate belay.GateStatus, sevs ...belay.Severity) belay.QualityReport {
	q := belay.QualityReport{
		Source:  source,
		Gate:    gate,
		Issues:  []belay.Issue{},
		Summary: source + " summary",
		Raw:     json.RawMessage(`{"tool":"fake"}`),
	}
	for i, sev := range sevs {
		q.Issues = append(q.Issues, belay.Issue{
			RuleID:   fmt.Sprintf("rule-%d", i),
			Severity: sev,
			File:     "main.go",
			Line:     i + 1,
			Message:  "something is wrong",
		})
		switch sev {
		case belay.SeverityBlocker:
			q.Counts.Blocker++
		case belay.SeverityCritical:
			q.Counts.Critical++
		case belay.SeverityMajor:
			q.Counts.Major++
		case belay.SeverityMinor:
			q.Counts.Minor++
		case belay.SeverityInfo:
			q.Counts.Info++
		}
	}
	return q
}

func TestNameIsGraphNodeReview(t *testing.T) {
	if got := review.New().Name(); got != graph.NodeReview {
		t.Fatalf("Name() = %q, want %q", got, graph.NodeReview)
	}
	var _ graph.Node = review.New()
}

// TestGateOutcomesRouteThreeWays is the core of this node: a passing gate
// ends the run, a failing gate is NOT an error but the ordinary branch to the
// fix loop, and a gate that reached no verdict is a third thing again that
// must not be mistaken for either neighbour.
func TestGateOutcomesRouteThreeWays(t *testing.T) {
	tests := []struct {
		name       string
		reported   belay.GateStatus
		severities []belay.Severity
		wantNext   string
		wantStatus journal.Status
		wantErr    error
		wantGate   belay.GateStatus
		wantNote   string
	}{
		{
			name:       "pass routes to End with no error",
			reported:   belay.GatePass,
			severities: []belay.Severity{belay.SeverityMinor, belay.SeverityInfo},
			wantNext:   graph.End,
			wantStatus: journal.StatusOK,
			wantGate:   belay.GatePass,
			wantNote:   "gate pass: no issues at or above fail_on=major",
		},
		{
			name:       "fail routes to fix with no error",
			reported:   belay.GateFail,
			severities: []belay.Severity{belay.SeverityBlocker, belay.SeverityMajor, belay.SeverityMajor, belay.SeverityMajor},
			wantNext:   graph.NodeFix,
			wantStatus: journal.StatusOK,
			wantGate:   belay.GateFail,
			wantNote:   "gate fail: 1 blocker, 3 major at fail_on=major",
		},
		{
			name:       "error routes nowhere and returns ErrGateUnresolved",
			reported:   belay.GateError,
			severities: nil,
			wantNext:   "",
			wantStatus: journal.StatusFailed,
			wantErr:    review.ErrGateUnresolved,
			wantGate:   belay.GateError,
			wantNote:   "gate error: no verdict reached at fail_on=major",
		},
		{
			name:       "error with issues still routes nowhere",
			reported:   belay.GateError,
			severities: []belay.Severity{belay.SeverityBlocker},
			wantNext:   "",
			wantStatus: journal.StatusFailed,
			wantErr:    review.ErrGateUnresolved,
			wantGate:   belay.GateError,
			wantNote:   "gate error: no verdict reached at fail_on=major",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
			f.rc.Linter = &belaytest.FakeLinter{
				NameValue: "golangci-lint",
				Responses: []belay.QualityReport{report("golangci-lint", tt.reported, tt.severities...)},
			}

			res, err := review.New().Run(t.Context(), f.rc)

			if tt.wantErr == nil && err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Run() error = %v, want errors.Is %v", err, tt.wantErr)
			}
			if res.Next != tt.wantNext {
				t.Errorf("Next = %q, want %q", res.Next, tt.wantNext)
			}
			if res.Status != tt.wantStatus {
				t.Errorf("Status = %v, want %v", res.Status, tt.wantStatus)
			}
			if err := res.Validate(); err != nil {
				t.Errorf("Result.Validate() = %v, want nil", err)
			}
			if res.Patch.Review == nil {
				t.Fatal("Patch.Review is nil; the verdict must reach the blackboard on every path that got a report")
			}
			if got := res.Patch.Review.Gate; got != tt.wantGate {
				t.Errorf("Patch.Review.Gate = %v, want %v", got, tt.wantGate)
			}
			if !strings.HasPrefix(res.Note, tt.wantNote) {
				t.Errorf("Note = %q, want prefix %q", res.Note, tt.wantNote)
			}
			if res.Usage != (belay.Usage{}) {
				t.Errorf("Usage = %+v, want zero on the lint path", res.Usage)
			}
		})
	}
}

// TestFailingGateIsNotAGoError states the invariant in isolation, because it
// is the one a future refactor is most likely to break: a failed gate is
// exactly as much an error as a failing test, which is to say none at all.
func TestFailingGateIsNotAGoError(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
	f.rc.Linter = &belaytest.FakeLinter{
		NameValue: "golangci-lint",
		Responses: []belay.QualityReport{report("golangci-lint", belay.GateFail, belay.SeverityBlocker)},
	}

	res, err := review.New().Run(t.Context(), f.rc)
	if err != nil {
		t.Fatalf("a failed gate must not be a Go error, got %v", err)
	}
	if errors.Is(err, belay.ErrGateFailed) {
		t.Fatal("the node must not wrap belay.ErrGateFailed; that conversion belongs at a CLI boundary")
	}
	if res.Next != graph.NodeFix || res.Status != journal.StatusOK {
		t.Fatalf("got (Next=%q, Status=%v), want (%q, %v)", res.Next, res.Status, graph.NodeFix, journal.StatusOK)
	}
}

func TestModeDispatch(t *testing.T) {
	tests := []struct {
		name        string
		mode        config.ReviewMode
		wantLinter  int
		wantReviewr int
	}{
		{"lint uses the Linter", config.ReviewModeLint, 1, 0},
		{"sonar uses the Reviewer", config.ReviewModeSonar, 0, 1},
		{"ai uses the Reviewer", config.ReviewModeAI, 0, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.mode, config.SeverityMajor)
			linter := &belaytest.FakeLinter{
				NameValue: "golangci-lint",
				Responses: []belay.QualityReport{report("golangci-lint", belay.GatePass)},
			}
			reviewer := &belaytest.FakeReviewer{
				Responses: []belay.QualityReport{report("sonar", belay.GatePass)},
			}
			// Both adapters are present: dispatch must pick on mode alone,
			// not on which adapter happens to be wired.
			f.rc.Linter, f.rc.Reviewer = linter, reviewer

			if _, err := review.New().Run(t.Context(), f.rc); err != nil {
				t.Fatalf("Run() = %v, want nil", err)
			}
			if got := linter.CallCount(); got != tt.wantLinter {
				t.Errorf("Linter calls = %d, want %d", got, tt.wantLinter)
			}
			if got := reviewer.CallCount(); got != tt.wantReviewr {
				t.Errorf("Reviewer calls = %d, want %d", got, tt.wantReviewr)
			}
		})
	}
}

func TestUnknownModeIsTypedError(t *testing.T) {
	for _, mode := range []config.ReviewMode{"", "sonarqube", "LINT", "manual"} {
		t.Run(fmt.Sprintf("mode=%q", mode), func(t *testing.T) {
			f := newFixture(t, mode, config.SeverityMajor)
			f.rc.Linter = &belaytest.FakeLinter{}
			f.rc.Reviewer = &belaytest.FakeReviewer{}

			res, err := review.New().Run(t.Context(), f.rc)
			if !errors.Is(err, review.ErrUnknownMode) {
				t.Fatalf("Run() error = %v, want errors.Is review.ErrUnknownMode", err)
			}
			if res.Next != "" {
				t.Errorf("Next = %q, want empty: an unknown mode must not route anywhere", res.Next)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", string(mode))) {
				t.Errorf("error %q does not name the offending mode", err)
			}
		})
	}
}

// TestNilAdapterIsTypedErrorNotPanic covers the failure that would otherwise
// be a nil-interface dereference inside the dispatcher's goroutine.
func TestNilAdapterIsTypedErrorNotPanic(t *testing.T) {
	tests := []struct {
		name        string
		mode        config.ReviewMode
		wantAdapter string
	}{
		{"lint without a Linter", config.ReviewModeLint, "belay.Linter"},
		{"sonar without a Reviewer", config.ReviewModeSonar, "belay.Reviewer"},
		{"ai without a Reviewer", config.ReviewModeAI, "belay.Reviewer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.mode, config.SeverityMajor)
			// Every adapter left nil on purpose.

			res, err := review.New().Run(t.Context(), f.rc)
			if !errors.Is(err, review.ErrAdapterMissing) {
				t.Fatalf("Run() error = %v, want errors.Is review.ErrAdapterMissing", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, string(tt.mode)) {
				t.Errorf("error %q does not name the mode %q", msg, tt.mode)
			}
			if !strings.Contains(msg, tt.wantAdapter) {
				t.Errorf("error %q does not name the missing adapter %q", msg, tt.wantAdapter)
			}
			if res.Next != "" {
				t.Errorf("Next = %q, want empty", res.Next)
			}
			if res.Status != journal.StatusFailed {
				t.Errorf("Status = %v, want %v", res.Status, journal.StatusFailed)
			}
		})
	}
}

// TestToolchainMissingSurvivesWrappingAndDoesNotRouteToFix guards the exact
// failure the sentinel exists for: sending an agent to edit source because
// golangci-lint is not installed.
func TestToolchainMissingSurvivesWrappingAndDoesNotRouteToFix(t *testing.T) {
	tests := []struct {
		name  string
		mode  config.ReviewMode
		cause error
	}{
		{
			name:  "linter reports a missing binary",
			mode:  config.ReviewModeLint,
			cause: &belay.ToolchainError{Tool: "golangci-lint"},
		},
		{
			name:  "linter wraps the sentinel directly",
			mode:  config.ReviewModeLint,
			cause: fmt.Errorf("%w: golangci-lint", belay.ErrToolchainMissing),
		},
		{
			name:  "reviewer reports a missing scanner",
			mode:  config.ReviewModeSonar,
			cause: &belay.ToolchainError{Tool: "sonar-scanner"},
		},
		{
			name:  "ai reviewer reports a missing binary",
			mode:  config.ReviewModeAI,
			cause: &belay.ToolchainError{Tool: "docker"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.mode, config.SeverityMajor)
			f.rc.Linter = &belaytest.FakeLinter{NameValue: "golangci-lint", Errs: []error{tt.cause}}
			f.rc.Reviewer = &belaytest.FakeReviewer{Errs: []error{tt.cause}}

			res, err := review.New().Run(t.Context(), f.rc)

			if !errors.Is(err, belay.ErrToolchainMissing) {
				t.Fatalf("errors.Is(err, belay.ErrToolchainMissing) = false for %v; wrapping lost the sentinel", err)
			}
			if res.Next == graph.NodeFix {
				t.Fatal("a missing toolchain routed to the fix node; no agent edit can install software")
			}
			if res.Next != "" {
				t.Errorf("Next = %q, want empty", res.Next)
			}
			if res.Status != journal.StatusFailed {
				t.Errorf("Status = %v, want %v", res.Status, journal.StatusFailed)
			}
			// The concrete type must survive too, so a caller can say which
			// tool to install.
			var te *belay.ToolchainError
			if errors.As(tt.cause, new(*belay.ToolchainError)) && !errors.As(err, &te) {
				t.Error("errors.As lost *belay.ToolchainError")
			}
		})
	}
}

// TestAdapterErrorDoesNotRouteToFix generalizes the guard above: no error,
// whatever its cause, may spend a fix attempt.
func TestAdapterErrorDoesNotRouteToFix(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
	f.rc.Linter = &belaytest.FakeLinter{
		NameValue: "golangci-lint",
		Errs:      []error{errors.New("linter output could not be parsed")},
	}

	res, err := review.New().Run(t.Context(), f.rc)
	if err == nil {
		t.Fatal("Run() = nil error, want the adapter failure")
	}
	if res.Next != "" {
		t.Errorf("Next = %q, want empty", res.Next)
	}
	if res.Patch.Review != nil {
		t.Error("Patch.Review is set, but the adapter never produced a report to record")
	}
}

// TestFailOnThresholdHonoured walks every severity against every threshold.
// The node must agree with Counts.AtOrAbove exactly, never approximate it.
func TestFailOnThresholdHonoured(t *testing.T) {
	severities := []struct {
		cfg config.Severity
		sev belay.Severity
	}{
		{config.SeverityInfo, belay.SeverityInfo},
		{config.SeverityMinor, belay.SeverityMinor},
		{config.SeverityMajor, belay.SeverityMajor},
		{config.SeverityCritical, belay.SeverityCritical},
		{config.SeverityBlocker, belay.SeverityBlocker},
	}

	for _, issue := range severities {
		for _, failOn := range severities {
			name := fmt.Sprintf("issue=%s/fail_on=%s", issue.cfg, failOn.cfg)
			t.Run(name, func(t *testing.T) {
				counts := report("golangci-lint", belay.GateUnknown, issue.sev).Counts
				wantFail := counts.AtOrAbove(failOn.sev) > 0
				if want := issue.sev >= failOn.sev; want != wantFail {
					t.Fatalf("test premise broken: AtOrAbove disagrees with the ordering")
				}

				f := newFixture(t, config.ReviewModeLint, failOn.cfg)
				// The adapter reports GatePass regardless, so the assertion
				// is about the node's threshold and nothing else.
				f.rc.Linter = &belaytest.FakeLinter{
					NameValue: "golangci-lint",
					Responses: []belay.QualityReport{report("golangci-lint", belay.GatePass, issue.sev)},
				}

				res, err := review.New().Run(t.Context(), f.rc)
				if err != nil {
					t.Fatalf("Run() = %v, want nil", err)
				}
				wantNext, wantGate := graph.End, belay.GatePass
				if wantFail {
					wantNext, wantGate = graph.NodeFix, belay.GateFail
				}
				if res.Next != wantNext {
					t.Errorf("Next = %q, want %q", res.Next, wantNext)
				}
				if got := res.Patch.Review.Gate; got != wantGate {
					t.Errorf("Gate = %v, want %v", got, wantGate)
				}
			})
		}
	}
}

// TestNodeThresholdOverridesAdapterGate documents the reconciliation rule.
// belay.Linter.Lint takes no threshold, so a Linter's Gate reflects whatever
// the adapter was built with; this node's review.fail_on wins — except for
// GateError, which is the absence of a verdict rather than a verdict, and
// survives.
func TestNodeThresholdOverridesAdapterGate(t *testing.T) {
	tests := []struct {
		name       string
		reported   belay.GateStatus
		severities []belay.Severity
		failOn     config.Severity
		wantGate   belay.GateStatus
		wantNext   string
	}{
		{
			name:       "adapter says pass, node's threshold says fail",
			reported:   belay.GatePass,
			severities: []belay.Severity{belay.SeverityMajor},
			failOn:     config.SeverityMajor,
			wantGate:   belay.GateFail,
			wantNext:   graph.NodeFix,
		},
		{
			name:       "adapter says fail, node's looser threshold says pass",
			reported:   belay.GateFail,
			severities: []belay.Severity{belay.SeverityMinor},
			failOn:     config.SeverityBlocker,
			wantGate:   belay.GatePass,
			wantNext:   graph.End,
		},
		{
			name:       "adapter GateError survives a clean histogram",
			reported:   belay.GateError,
			severities: nil,
			failOn:     config.SeverityMajor,
			wantGate:   belay.GateError,
			wantNext:   "",
		},
		{
			name:       "adapter GateUnknown is treated as no verdict, never a pass",
			reported:   belay.GateUnknown,
			severities: nil,
			failOn:     config.SeverityMajor,
			wantGate:   belay.GateError,
			wantNext:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, config.ReviewModeLint, tt.failOn)
			f.rc.Linter = &belaytest.FakeLinter{
				NameValue: "golangci-lint",
				Responses: []belay.QualityReport{report("golangci-lint", tt.reported, tt.severities...)},
			}

			res, _ := review.New().Run(t.Context(), f.rc)
			if got := res.Patch.Review.Gate; got != tt.wantGate {
				t.Errorf("Gate = %v, want %v", got, tt.wantGate)
			}
			if res.Next != tt.wantNext {
				t.Errorf("Next = %q, want %q", res.Next, tt.wantNext)
			}
		})
	}
}

// TestReviewerReceivesFullRequest checks every field of the ReviewRequest the
// node builds, including that FailOn came through belay.ParseSeverity rather
// than a cast of the config string.
func TestReviewerReceivesFullRequest(t *testing.T) {
	f := newFixture(t, config.ReviewModeSonar, config.SeverityCritical)
	changed := []string{"internal/a.go", "internal/b.go"}
	f.rc.State = state.NewState("goal")
	f.rc.State.Code.ChangedFiles = changed
	reviewer := &belaytest.FakeReviewer{
		Responses: []belay.QualityReport{report("sonar", belay.GatePass)},
	}
	f.rc.Reviewer = reviewer

	if _, err := review.New().Run(t.Context(), f.rc); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	calls := reviewer.Calls()
	if len(calls) != 1 {
		t.Fatalf("Review called %d times, want 1", len(calls))
	}
	want := belay.ReviewRequest{
		WorkDir:      f.workspace,
		ChangedFiles: changed,
		ProjectKey:   filepath.Base(f.workspace),
		FailOn:       belay.SeverityCritical,
	}
	if diff := cmp.Diff(want, calls[0]); diff != "" {
		t.Errorf("ReviewRequest mismatch (-want +got):\n%s", diff)
	}
}

// TestReviewRequestChangedFilesAreCloned proves the request cannot be used to
// reach back into the blackboard snapshot the node was handed.
func TestReviewRequestChangedFilesAreCloned(t *testing.T) {
	f := newFixture(t, config.ReviewModeAI, config.SeverityMajor)
	f.rc.State.Code.ChangedFiles = []string{"a.go", "b.go"}
	f.rc.Reviewer = &belaytest.FakeReviewer{
		Func: func(_ context.Context, req belay.ReviewRequest) (belay.QualityReport, error) {
			req.ChangedFiles[0] = "clobbered.go"
			return report("ai", belay.GatePass), nil
		},
	}

	if _, err := review.New().Run(t.Context(), f.rc); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if got := f.rc.State.Code.ChangedFiles[0]; got != "a.go" {
		t.Fatalf("adapter mutated the caller's slice: ChangedFiles[0] = %q", got)
	}
}

// TestArtifactWrittenAndRecorded covers requirement 6: the full report,
// including Raw, lands on disk and the blackboard points at it.
func TestArtifactWrittenAndRecorded(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
	f.rc.Step = 6
	src := report("golangci-lint", belay.GateFail, belay.SeverityBlocker, belay.SeverityMajor)
	f.rc.Linter = &belaytest.FakeLinter{NameValue: "golangci-lint", Responses: []belay.QualityReport{src}}

	res, err := review.New().Run(t.Context(), f.rc)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	const wantRel = "artifacts/review-6.json"
	abs := filepath.Join(f.runDir, filepath.FromSlash(wantRel))
	data, readErr := os.ReadFile(abs) //nolint:gosec // path is built from the test's own TempDir
	if readErr != nil {
		t.Fatalf("review artifact not written at %s: %v", abs, readErr)
	}

	var got belay.QualityReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("artifact is not valid QualityReport JSON: %v", err)
	}
	if got.Gate != belay.GateFail {
		t.Errorf("artifact Gate = %v, want %v", got.Gate, belay.GateFail)
	}
	if diff := cmp.Diff(src.Issues, got.Issues); diff != "" {
		t.Errorf("artifact lost issues (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(src.Counts, got.Counts); diff != "" {
		t.Errorf("artifact lost counts (-want +got):\n%s", diff)
	}
	// MarshalIndent re-indents an embedded RawMessage, so compare the
	// compacted bytes: what must survive is Raw's content, not its spacing.
	var gotRaw, wantRaw bytes.Buffer
	if err := json.Compact(&gotRaw, got.Raw); err != nil {
		t.Fatalf("artifact Raw is not valid JSON: %v", err)
	}
	if err := json.Compact(&wantRaw, src.Raw); err != nil {
		t.Fatalf("fixture Raw is not valid JSON: %v", err)
	}
	if gotRaw.String() != wantRaw.String() {
		t.Errorf("artifact Raw = %s, want %s", gotRaw.String(), wantRaw.String())
	}

	// The path reaches the patch through its own field, and the Note.
	if res.Patch.Review.ReportPath != wantRel {
		t.Errorf("Patch.Review.ReportPath = %q, want %q", res.Patch.Review.ReportPath, wantRel)
	}
	// The adapter's own prose must survive untouched: the path has a field
	// now, so nothing appends to Summary behind the adapter's back.
	if res.Patch.Review.Summary != src.Summary {
		t.Errorf("Patch.Review.Summary = %q, want the adapter's verbatim %q", res.Patch.Review.Summary, src.Summary)
	}
	if !strings.Contains(res.Note, wantRel) {
		t.Errorf("Note = %q, want it to carry %q", res.Note, wantRel)
	}
}

// TestArtifactNameTracksStep keeps two review executions in one run from
// overwriting each other's evidence.
func TestArtifactNameTracksStep(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
	f.rc.Linter = &belaytest.FakeLinter{
		NameValue: "golangci-lint",
		Responses: []belay.QualityReport{report("golangci-lint", belay.GatePass)},
	}
	for _, step := range []int{3, 11} {
		f.rc.Step = step
		if _, err := review.New().Run(t.Context(), f.rc); err != nil {
			t.Fatalf("Run() at step %d = %v", step, err)
		}
	}
	for _, name := range []string{"review-3.json", "review-11.json"} {
		if _, err := os.Stat(filepath.Join(f.runDir, "artifacts", name)); err != nil {
			t.Errorf("missing artifact %s: %v", name, err)
		}
	}
}

// TestPatchProjectsReportWithoutHandConstruction asserts the Review block is
// exactly state.NewReview's projection of the report the node acted on.
func TestPatchProjectsReportWithoutHandConstruction(t *testing.T) {
	f := newFixture(t, config.ReviewModeSonar, config.SeverityMajor)
	src := report("sonar", belay.GateFail, belay.SeverityCritical, belay.SeverityInfo)
	f.rc.Reviewer = &belaytest.FakeReviewer{Responses: []belay.QualityReport{src}}

	res, err := review.New().Run(t.Context(), f.rc)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	resolved := src
	resolved.Gate = belay.GateFail
	want := state.NewReview(resolved, res.Patch.Review.ReportPath)
	if diff := cmp.Diff(want, *res.Patch.Review); diff != "" {
		t.Errorf("Patch.Review mismatch (-want +got):\n%s", diff)
	}

	// Applying the patch twice must land where applying it once does.
	st := state.NewState("goal")
	for range 2 {
		if err := res.Patch.Apply(&st); err != nil {
			t.Fatalf("Patch.Apply: %v", err)
		}
	}
	if diff := cmp.Diff(want, st.Review); diff != "" {
		t.Errorf("patch is not idempotent (-want +got):\n%s", diff)
	}
}

// TestNodeWritesNoControlState enforces ADR 0002 from the node's side: the
// only thing it may leave on disk is an artifact.
func TestNodeWritesNoControlState(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
	f.rc.Linter = &belaytest.FakeLinter{
		NameValue: "golangci-lint",
		Responses: []belay.QualityReport{report("golangci-lint", belay.GateFail, belay.SeverityBlocker)},
	}

	if _, err := review.New().Run(t.Context(), f.rc); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	for _, forbidden := range []string{"state.json", "manifest.json", "journal.ndjson"} {
		if _, err := os.Stat(filepath.Join(f.runDir, forbidden)); !os.IsNotExist(err) {
			t.Errorf("node wrote %s; nodes never own control state (ADR 0002)", forbidden)
		}
	}
}

func TestContextCancellation(t *testing.T) {
	t.Run("cancelled before the adapter runs", func(t *testing.T) {
		f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
		linter := &belaytest.FakeLinter{
			NameValue: "golangci-lint",
			Responses: []belay.QualityReport{report("golangci-lint", belay.GatePass)},
		}
		f.rc.Linter = linter
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		res, err := review.New().Run(ctx, f.rc)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
		if linter.CallCount() != 0 {
			t.Error("the adapter ran despite a cancelled context")
		}
		if res.Status != journal.StatusAborted {
			t.Errorf("Status = %v, want %v", res.Status, journal.StatusAborted)
		}
		if res.Next != "" {
			t.Errorf("Next = %q, want empty", res.Next)
		}
	})

	t.Run("cancelled inside the adapter", func(t *testing.T) {
		f := newFixture(t, config.ReviewModeAI, config.SeverityMajor)
		f.rc.Reviewer = &belaytest.FakeReviewer{
			Func: func(ctx context.Context, _ belay.ReviewRequest) (belay.QualityReport, error) {
				return belay.QualityReport{Gate: belay.GateError, Issues: []belay.Issue{}}, ctx.Err()
			},
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		res, err := review.New().Run(ctx, f.rc)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled to survive wrapping", err)
		}
		if res.Status != journal.StatusAborted {
			t.Errorf("Status = %v, want %v", res.Status, journal.StatusAborted)
		}
	})
}

func TestInvalidFailOnIsTypedError(t *testing.T) {
	for _, bad := range []config.Severity{"", "fatal", "warn", "err"} {
		t.Run(fmt.Sprintf("fail_on=%q", bad), func(t *testing.T) {
			f := newFixture(t, config.ReviewModeLint, bad)
			f.rc.Linter = &belaytest.FakeLinter{}

			res, err := review.New().Run(t.Context(), f.rc)
			if !errors.Is(err, review.ErrInvalidThreshold) {
				t.Fatalf("Run() error = %v, want errors.Is review.ErrInvalidThreshold", err)
			}
			if !errors.Is(err, belay.ErrUnknownSeverity) {
				t.Errorf("Run() error = %v, want the parse cause to survive", err)
			}
			if res.Next != "" {
				t.Errorf("Next = %q, want empty", res.Next)
			}
		})
	}
}

// belay.ParseSeverity trims and lowercases, so "  Major  " is a valid
// threshold. It must reach the Reviewer as belay.SeverityMajor, which a cast
// of the config string would never produce.
func TestFailOnIsParsedNotCast(t *testing.T) {
	f := newFixture(t, config.ReviewModeSonar, "  Major  ")
	reviewer := &belaytest.FakeReviewer{Responses: []belay.QualityReport{report("sonar", belay.GatePass)}}
	f.rc.Reviewer = reviewer

	if _, err := review.New().Run(t.Context(), f.rc); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if got := reviewer.Calls()[0].FailOn; got != belay.SeverityMajor {
		t.Fatalf("FailOn = %v, want %v", got, belay.SeverityMajor)
	}
}

func TestMissingRunDirIsTypedError(t *testing.T) {
	rc := &graph.RunContext{
		Config: config.Config{Review: config.Review{Mode: config.ReviewModeLint, FailOn: config.SeverityMajor}},
		Linter: &belaytest.FakeLinter{},
	}
	if _, err := review.New().Run(t.Context(), rc); !errors.Is(err, review.ErrNoWorkspace) {
		t.Fatalf("Run() error = %v, want errors.Is review.ErrNoWorkspace", err)
	}
}

func TestNilRunContextDoesNotPanic(t *testing.T) {
	res, err := review.New().Run(t.Context(), nil)
	if err == nil {
		t.Fatal("Run(nil) = nil error, want a failure")
	}
	if res.Next != "" {
		t.Errorf("Next = %q, want empty", res.Next)
	}
}

func TestNilLoggerDoesNotPanic(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
	f.rc.Logger = nil
	f.rc.Linter = &belaytest.FakeLinter{
		NameValue: "golangci-lint",
		Responses: []belay.QualityReport{report("golangci-lint", belay.GatePass)},
	}
	if _, err := review.New().Run(t.Context(), f.rc); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

// TestUnmarshalableRawStillProducesAVerdict: Raw is opaque adapter bytes the
// graph never reads, so a malformed one must not discard a verdict belay
// already reached.
func TestUnmarshalableRawStillProducesAVerdict(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
	broken := report("golangci-lint", belay.GatePass)
	broken.Raw = json.RawMessage("{not json")
	f.rc.Linter = &belaytest.FakeLinter{NameValue: "golangci-lint", Responses: []belay.QualityReport{broken}}

	res, err := review.New().Run(t.Context(), f.rc)
	if err != nil {
		t.Fatalf("Run() = %v, want nil: a malformed Raw must not sink a verdict", err)
	}
	if res.Next != graph.End {
		t.Errorf("Next = %q, want %q", res.Next, graph.End)
	}
}

// TestNoteIsComposedFromCountsNotAdapterProse: the Note must not leak an
// adapter's unredacted summary text.
func TestNoteExcludesAdapterSummary(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
	leaky := report("golangci-lint", belay.GateFail, belay.SeverityBlocker)
	leaky.Summary = "token=sk-ant-SECRET leaked from the tool"
	f.rc.Linter = &belaytest.FakeLinter{NameValue: "golangci-lint", Responses: []belay.QualityReport{leaky}}

	res, err := review.New().Run(t.Context(), f.rc)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if strings.Contains(res.Note, "SECRET") {
		t.Fatalf("Note carried adapter prose: %q", res.Note)
	}
	if want := "gate fail: 1 blocker at fail_on=major"; !strings.HasPrefix(res.Note, want) {
		t.Errorf("Note = %q, want prefix %q", res.Note, want)
	}
}

func TestNoteBreakdownOmitsIssuesBelowThreshold(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityCritical)
	f.rc.Linter = &belaytest.FakeLinter{
		NameValue: "golangci-lint",
		Responses: []belay.QualityReport{report("golangci-lint", belay.GateFail,
			belay.SeverityBlocker, belay.SeverityCritical, belay.SeverityCritical,
			belay.SeverityMajor, belay.SeverityMinor, belay.SeverityInfo)},
	}

	res, err := review.New().Run(t.Context(), f.rc)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	const want = "gate fail: 1 blocker, 2 critical at fail_on=critical"
	if !strings.HasPrefix(res.Note, want) {
		t.Errorf("Note = %q, want prefix %q", res.Note, want)
	}
}

func TestReviewIsSafeToReRun(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
	f.rc.Linter = &belaytest.FakeLinter{
		NameValue: "golangci-lint",
		Responses: []belay.QualityReport{report("golangci-lint", belay.GateFail, belay.SeverityMajor)},
	}

	var first graph.Result
	for attempt := 1; attempt <= 3; attempt++ {
		f.rc.Attempt = attempt
		res, err := review.New().Run(t.Context(), f.rc)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if attempt == 1 {
			first = res
			continue
		}
		if diff := cmp.Diff(first.Patch.Review, res.Patch.Review); diff != "" {
			t.Errorf("attempt %d produced a different patch (-first +got):\n%s", attempt, diff)
		}
		if res.Next != first.Next || res.Note != first.Note {
			t.Errorf("attempt %d diverged: (%q, %q) vs (%q, %q)", attempt, res.Next, res.Note, first.Next, first.Note)
		}
	}
}

// TestUsageIsZeroOnEveryPath documents requirement 9. Neither belay.Linter
// nor belay.Reviewer has a cost channel: a Reviewer's only route for one
// would be QualityReport.Raw, which ADR 0006 forbids the graph from reading.
// An "ai" reviewer therefore accounts for its own spend below this seam, and
// the node reports a truthful zero rather than an invented number.
func TestUsageIsZeroOnEveryPath(t *testing.T) {
	for _, mode := range []config.ReviewMode{config.ReviewModeLint, config.ReviewModeSonar, config.ReviewModeAI} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode, config.SeverityMajor)
			f.rc.Linter = &belaytest.FakeLinter{
				NameValue: "golangci-lint",
				Responses: []belay.QualityReport{report("golangci-lint", belay.GateFail, belay.SeverityBlocker)},
			}
			f.rc.Reviewer = &belaytest.FakeReviewer{
				Responses: []belay.QualityReport{report("sonar", belay.GateFail, belay.SeverityBlocker)},
			}

			res, err := review.New().Run(t.Context(), f.rc)
			if err != nil {
				t.Fatalf("Run() = %v, want nil", err)
			}
			if res.Usage != (belay.Usage{}) {
				t.Errorf("Usage = %+v, want the zero Usage", res.Usage)
			}
		})
	}
}

// TestNilIssuesAreNormalized: a non-conforming adapter must not be able to
// put "issues": null into the artifact or the blackboard.
func TestNilIssuesAreNormalized(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
	sloppy := belay.QualityReport{Source: "sloppy", Gate: belay.GatePass, Issues: nil}
	f.rc.Linter = &belaytest.FakeLinter{NameValue: "sloppy", Responses: []belay.QualityReport{sloppy}}

	res, err := review.New().Run(t.Context(), f.rc)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if res.Patch.Review.Issues == nil {
		t.Error("Patch.Review.Issues is nil; it must serialize as [] not null")
	}
	data, err := os.ReadFile(filepath.Join(f.runDir, "artifacts", "review-6.json")) //nolint:gosec // test TempDir
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !strings.Contains(string(data), `"issues": []`) {
		t.Errorf("artifact does not carry an empty issues array:\n%s", data)
	}
}

// TestUnwritableArtifactFailsClosed: if the evidence for a gate decision
// cannot be recorded, the node fails rather than routing on a verdict nobody
// will be able to audit. Fail-closed is the right default here — the usual
// cause is a broken run directory, not a code problem an agent could fix.
func TestUnwritableArtifactFailsClosed(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
	f.rc.Linter = &belaytest.FakeLinter{
		NameValue: "golangci-lint",
		Responses: []belay.QualityReport{report("golangci-lint", belay.GatePass)},
	}
	// A plain file where artifacts/ must be a directory.
	if err := os.MkdirAll(f.runDir, 0o750); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.runDir, "artifacts"), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	res, err := review.New().Run(t.Context(), f.rc)
	if err == nil {
		t.Fatal("Run() = nil error, want the artifact write failure")
	}
	if !strings.Contains(err.Error(), "review-6.json") {
		t.Errorf("error %q does not name the artifact it failed to write", err)
	}
	if res.Next != "" {
		t.Errorf("Next = %q, want empty", res.Next)
	}
	if res.Status != journal.StatusFailed {
		t.Errorf("Status = %v, want %v", res.Status, journal.StatusFailed)
	}
}

func TestDeadlineExceededIsAborted(t *testing.T) {
	f := newFixture(t, config.ReviewModeLint, config.SeverityMajor)
	f.rc.Linter = &belaytest.FakeLinter{
		NameValue: "golangci-lint",
		Errs:      []error{context.DeadlineExceeded},
	}
	res, err := review.New().Run(t.Context(), f.rc)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want context.DeadlineExceeded to survive wrapping", err)
	}
	if res.Status != journal.StatusAborted {
		t.Errorf("Status = %v, want %v", res.Status, journal.StatusAborted)
	}
}
