//go:build unix

package aireview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

const testRunID = "run-1"

// fixture is one assembled RunContext plus the paths a test needs to inspect
// what the node did with it.
type fixture struct {
	rc     *graph.RunContext
	runDir string
}

// newFixture builds a RunContext over a real temporary workspace, so
// WriteArtifact exercises the same state.Layout the dispatcher would hand
// the node.
func newFixture(t *testing.T, failOn config.Severity) *fixture {
	t.Helper()
	layout, err := state.NewLayout(t.TempDir(), testRunID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	cfg := config.Config{Version: config.Version}
	cfg.Review.Mode = config.ReviewModeAI
	cfg.Review.FailOn = failOn
	cfg.Review.AI.MCP = config.MCP{Mode: config.MCPModeServer, URL: "http://127.0.0.1:0/unused"}

	f := &fixture{runDir: layout.RunDir()}
	f.rc = &graph.RunContext{
		Goal:      "make the tests pass",
		Config:    cfg,
		Layout:    layout,
		Workspace: layout.WorkspaceDir(),
		RunID:     layout.RunID(),
		NodeName:  graph.NodeReview,
		Step:      6,
		Attempt:   1,
		Logger:    quietLogger(),
	}
	f.rc.State.Code.ChangedFiles = []string{"internal/store/pg.go"}
	return f
}

// scriptedReport builds a report whose Counts match its Issues, so a test
// never has to keep the two in sync by hand.
func scriptedReport(gate belay.GateStatus, sevs ...belay.Severity) belay.QualityReport {
	issues := make([]belay.Issue, 0, len(sevs))
	for i, sev := range sevs {
		issues = append(issues, belay.Issue{
			RuleID: fmt.Sprintf("go:S%d", i), Severity: sev, File: "main.go", Line: i + 1, Message: "m",
		})
	}
	return belay.QualityReport{
		Source: Source, Gate: gate, Counts: countIssues(issues), Issues: issues,
		Summary: "scripted", Raw: json.RawMessage(`{"scripted":true}`),
	}
}

func TestNameIsGraphNodeReview(t *testing.T) {
	if got := NewNode(nil).Name(); got != graph.NodeReview {
		t.Fatalf("Name() = %q, want %q", got, graph.NodeReview)
	}
}

// TestGateOutcomesRouteThreeWays is acceptance check 2 at the graph layer,
// and the reason this node exists in the shape it does: a passing gate ends
// the run, a failing gate is NOT an error but the ordinary branch to the fix
// loop, and a gate that reached no verdict is a third thing again that must
// never route to fix and burn a budgeted repair attempt.
func TestGateOutcomesRouteThreeWays(t *testing.T) {
	tests := []struct {
		name       string
		report     belay.QualityReport
		failOn     config.Severity
		wantNext   string
		wantStatus journal.Status
		wantGate   belay.GateStatus
		wantErr    error
		wantNote   string
	}{
		{
			name:     "pass ends the run",
			report:   scriptedReport(belay.GatePass, belay.SeverityMinor),
			failOn:   config.SeverityMajor,
			wantNext: graph.End, wantStatus: journal.StatusOK, wantGate: belay.GatePass,
			wantNote: "gate pass: no issues at or above fail_on=major",
		},
		{
			name:     "fail routes to the fix loop with a nil error",
			report:   scriptedReport(belay.GateFail, belay.SeverityBlocker, belay.SeverityMajor, belay.SeverityInfo),
			failOn:   config.SeverityMajor,
			wantNext: graph.NodeFix, wantStatus: journal.StatusOK, wantGate: belay.GateFail,
			wantNote: "gate fail: 1 blocker, 1 major at fail_on=major",
		},
		{
			name:     "no verdict routes nowhere at all",
			report:   scriptedReport(belay.GateError),
			failOn:   config.SeverityMajor,
			wantNext: "", wantStatus: journal.StatusFailed, wantGate: belay.GateError,
			wantErr:  ErrGateUnresolved,
			wantNote: "gate error: no verdict reached at fail_on=major",
		},
		{
			name:     "a reviewer that reports GateUnknown is treated as no verdict, not as a pass",
			report:   scriptedReport(belay.GateUnknown),
			failOn:   config.SeverityMajor,
			wantNext: "", wantStatus: journal.StatusFailed, wantGate: belay.GateError,
			wantErr:  ErrGateUnresolved,
			wantNote: "gate error: no verdict reached at fail_on=major",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.failOn)
			node := NewNode(&belaytest.FakeReviewer{Responses: []belay.QualityReport{tc.report}})

			got, err := node.Run(t.Context(), f.rc)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Run: unexpected error %v", err)
				}
			} else {
				errIs(t, err, tc.wantErr)
			}
			if got.Next != tc.wantNext {
				t.Errorf("Next = %q, want %q", got.Next, tc.wantNext)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %v, want %v", got.Status, tc.wantStatus)
			}
			if err := got.Validate(); tc.wantStatus == journal.StatusOK && err != nil {
				t.Errorf("Result.Validate() = %v, want nil", err)
			}
			if got.Patch.Review == nil {
				t.Fatal("Patch.Review is nil; every outcome must record what the reviewer said")
			}
			if got.Patch.Review.Gate != tc.wantGate {
				t.Errorf("Patch.Review.Gate = %s, want %s", got.Patch.Review.Gate, tc.wantGate)
			}
			if !strings.HasPrefix(got.Note, tc.wantNote) {
				t.Errorf("Note = %q, want it to start with %q", got.Note, tc.wantNote)
			}
			if got.Usage != (belay.Usage{}) {
				t.Errorf("Usage = %+v, want zero: belay.Reviewer reports no cost", got.Usage)
			}
		})
	}
}

// TestRunArchivesTheReportAndPointsStateAtIt covers requirement 6: the full
// report reaches disk and state.Review.ReportPath says where.
func TestRunArchivesTheReportAndPointsStateAtIt(t *testing.T) {
	f := newFixture(t, config.SeverityMajor)
	report := scriptedReport(belay.GateFail, belay.SeverityBlocker)
	node := NewNode(&belaytest.FakeReviewer{Responses: []belay.QualityReport{report}})

	got, err := node.Run(t.Context(), f.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	rel := got.Patch.Review.ReportPath
	if rel == "" {
		t.Fatal("state.Review.ReportPath is empty; nothing in state.json points at the full report")
	}
	if want := filepath.Join("artifacts", "review-6.json"); rel != want {
		t.Errorf("ReportPath = %q, want %q (the name internal/nodes/review uses for the same step)", rel, want)
	}

	// #nosec G304 -- the path came from the node under test, inside t.TempDir().
	data, err := os.ReadFile(filepath.Join(f.runDir, rel))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	var archived belay.QualityReport
	if err := json.Unmarshal(data, &archived); err != nil {
		t.Fatalf("artifact is not a belay.QualityReport: %v", err)
	}
	if diff := cmp.Diff(report.Issues, archived.Issues); diff != "" {
		t.Errorf("archived Issues mismatch (-want +got):\n%s", diff)
	}
	if len(archived.Raw) == 0 {
		t.Error("the archived report dropped Raw, which is where a human debugs from")
	}
	// The blackboard projection keeps the graph-relevant fields and drops Raw.
	if diff := cmp.Diff(report.Issues, got.Patch.Review.Issues); diff != "" {
		t.Errorf("Patch.Review.Issues mismatch (-want +got):\n%s", diff)
	}
	if got.Patch.Review.Source != Source {
		t.Errorf("Patch.Review.Source = %q, want %q", got.Patch.Review.Source, Source)
	}
}

// TestRunHandsTheReviewerAScopedRequest proves the node builds the request
// the Reviewer contract expects, and does not hand it the dispatcher's own
// slice to mutate.
func TestRunHandsTheReviewerAScopedRequest(t *testing.T) {
	f := newFixture(t, config.SeverityCritical)
	fake := &belaytest.FakeReviewer{Responses: []belay.QualityReport{scriptedReport(belay.GatePass)}}

	if _, err := NewNode(fake).Run(t.Context(), f.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("Review called %d times, want 1", len(calls))
	}
	req := calls[0]
	if req.WorkDir != f.rc.Workspace {
		t.Errorf("WorkDir = %q, want %q", req.WorkDir, f.rc.Workspace)
	}
	if req.ProjectKey != filepath.Base(f.rc.Workspace) {
		t.Errorf("ProjectKey = %q, want %q", req.ProjectKey, filepath.Base(f.rc.Workspace))
	}
	if req.FailOn != belay.SeverityCritical {
		t.Errorf("FailOn = %s, want critical", req.FailOn)
	}
	if diff := cmp.Diff([]string{"internal/store/pg.go"}, req.ChangedFiles); diff != "" {
		t.Errorf("ChangedFiles mismatch (-want +got):\n%s", diff)
	}
	req.ChangedFiles[0] = "clobbered"
	if f.rc.State.Code.ChangedFiles[0] == "clobbered" {
		t.Fatal("the reviewer was handed the blackboard's own backing array")
	}
}

// TestRunOverridesAnAdapterWhoseGateItsCountsContradict proves the node's
// threshold wins, exactly as internal/nodes/review's does — including for a
// belay.Reviewer this package did not write.
func TestRunOverridesAnAdapterWhoseGateItsCountsContradict(t *testing.T) {
	tests := []struct {
		name     string
		report   belay.QualityReport
		failOn   config.Severity
		wantGate belay.GateStatus
		wantNext string
	}{
		{
			name:   "a claimed pass with a blocker in it is a fail",
			report: scriptedReport(belay.GatePass, belay.SeverityBlocker),
			failOn: config.SeverityMajor, wantGate: belay.GateFail, wantNext: graph.NodeFix,
		},
		{
			name:   "a claimed fail with nothing at the threshold is a pass",
			report: scriptedReport(belay.GateFail, belay.SeverityInfo),
			failOn: config.SeverityMajor, wantGate: belay.GatePass, wantNext: graph.End,
		},
		{
			name:   "no verdict is never recomputed into one",
			report: scriptedReport(belay.GateError, belay.SeverityBlocker),
			failOn: config.SeverityMajor, wantGate: belay.GateError, wantNext: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.failOn)
			node := NewNode(&belaytest.FakeReviewer{Responses: []belay.QualityReport{tc.report}})
			got, _ := node.Run(t.Context(), f.rc)
			if got.Patch.Review.Gate != tc.wantGate {
				t.Errorf("Gate = %s, want %s", got.Patch.Review.Gate, tc.wantGate)
			}
			if got.Next != tc.wantNext {
				t.Errorf("Next = %q, want %q", got.Next, tc.wantNext)
			}
		})
	}
}

// TestRunNormalizesANilIssueSlice covers the last line of defense between an
// off-contract belay.Reviewer and "issues": null in state.json.
func TestRunNormalizesANilIssueSlice(t *testing.T) {
	f := newFixture(t, config.SeverityMajor)
	bad := belay.QualityReport{Source: "rogue", Gate: belay.GatePass, Summary: "s", Raw: json.RawMessage(`{}`)}
	got, err := NewNode(&belaytest.FakeReviewer{Responses: []belay.QualityReport{bad}}).Run(t.Context(), f.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Patch.Review.Issues == nil {
		t.Fatal("Patch.Review.Issues is nil; it would serialize as null")
	}
	// #nosec G304 -- the path came from the node under test, inside t.TempDir().
	data, err := os.ReadFile(filepath.Join(f.runDir, got.Patch.Review.ReportPath))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if strings.Contains(string(data), `"issues": null`) {
		t.Errorf("the archived report has a null issue list:\n%s", data)
	}
}

// TestRunErrorPaths covers everything that stops the node before a verdict.
// Every one of them must leave Next empty.
func TestRunErrorPaths(t *testing.T) {
	t.Run("nil run context", func(t *testing.T) {
		got, err := NewNode(nil).Run(t.Context(), nil)
		if err == nil {
			t.Fatal("Run(nil) returned no error")
		}
		if got.Next != "" || got.Status != journal.StatusFailed {
			t.Errorf("Result = %+v, want a failed result routing nowhere", got)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		f := newFixture(t, config.SeverityMajor)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		got, err := NewNode(&belaytest.FakeReviewer{}).Run(ctx, f.rc)
		errIs(t, err, context.Canceled)
		if got.Status != journal.StatusAborted || got.Next != "" {
			t.Errorf("Result = %+v, want an aborted result routing nowhere", got)
		}
	})

	t.Run("invalid review.fail_on", func(t *testing.T) {
		f := newFixture(t, config.Severity("catastrophic"))
		got, err := NewNode(&belaytest.FakeReviewer{}).Run(t.Context(), f.rc)
		if err == nil {
			t.Fatal("an unparsable review.fail_on was accepted")
		}
		if got.Next != "" || got.Status != journal.StatusFailed {
			t.Errorf("Result = %+v, want a failed result routing nowhere", got)
		}
	})

	t.Run("no workspace", func(t *testing.T) {
		f := newFixture(t, config.SeverityMajor)
		f.rc.Workspace = ""
		got, err := NewNode(&belaytest.FakeReviewer{}).Run(t.Context(), f.rc)
		if err == nil {
			t.Fatal("a run context with no workspace was accepted")
		}
		if got.Next != "" || got.Status != journal.StatusFailed {
			t.Errorf("Result = %+v, want a failed result routing nowhere", got)
		}
	})

	t.Run("a reviewer that could not run", func(t *testing.T) {
		f := newFixture(t, config.SeverityMajor)
		boom := &belay.ToolchainError{Tool: "docker"}
		fake := &belaytest.FakeReviewer{Errs: []error{boom}}
		got, err := NewNode(fake).Run(t.Context(), f.rc)
		errIs(t, err, belay.ErrToolchainMissing)
		if got.Next != "" || got.Status != journal.StatusFailed {
			t.Errorf("Result = %+v, want a failed result routing nowhere", got)
		}
	})

	t.Run("a reviewer cut short by cancellation", func(t *testing.T) {
		f := newFixture(t, config.SeverityMajor)
		fake := &belaytest.FakeReviewer{Errs: []error{fmt.Errorf("aireview: %w", context.DeadlineExceeded)}}
		got, err := NewNode(fake).Run(t.Context(), f.rc)
		errIs(t, err, context.DeadlineExceeded)
		if got.Status != journal.StatusAborted {
			t.Errorf("Status = %v, want aborted", got.Status)
		}
	})
}

// TestReviewerForResolutionOrder pins how a Node with no reviewer of its own
// finds one, which is what makes it a drop-in for the deterministic node.
func TestReviewerForResolutionOrder(t *testing.T) {
	own := &belaytest.FakeReviewer{}
	wired := &belaytest.FakeReviewer{}

	t.Run("its own reviewer wins", func(t *testing.T) {
		f := newFixture(t, config.SeverityMajor)
		f.rc.Reviewer = wired
		if got := NewNode(own).reviewerFor(f.rc); got != belay.Reviewer(own) {
			t.Errorf("reviewerFor() = %T, want the node's own reviewer", got)
		}
	})

	t.Run("the dispatcher's reviewer is next", func(t *testing.T) {
		f := newFixture(t, config.SeverityMajor)
		f.rc.Reviewer = wired
		if got := NewNode(nil).reviewerFor(f.rc); got != belay.Reviewer(wired) {
			t.Errorf("reviewerFor() = %T, want the RunContext's reviewer", got)
		}
	})

	t.Run("otherwise one is built from review.ai.mcp", func(t *testing.T) {
		f := newFixture(t, config.SeverityMajor)
		got, ok := NewNode(nil).reviewerFor(f.rc).(*Reviewer)
		if !ok {
			t.Fatalf("reviewerFor() = %T, want *aireview.Reviewer", got)
		}
		if diff := cmp.Diff(f.rc.Config.Review.AI.MCP, got.mcp); diff != "" {
			t.Errorf("the built Reviewer does not describe review.ai.mcp (-want +got):\n%s", diff)
		}
	})
}

// TestNodeAndReviewerComposeEndToEnd is the whole task in one test: a real
// mcp.Client over hand-authored frames, through the Reviewer, through the
// Node, into a state.Patch and an artifact on disk.
func TestNodeAndReviewerComposeEndToEnd(t *testing.T) {
	tr := newFakeTransport(t)
	tr.onTool("get_project_quality_gate_status", fixtureResponder("gate_fail.json"))
	tr.onTool("search_sonar_issues_in_projects", fixtureResponder("issues_mixed.json"))

	f := newFixture(t, config.SeverityMajor)
	f.rc.Reviewer = newReviewer(t, tr)

	got, err := NewNode(nil).Run(t.Context(), f.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Next != graph.NodeFix || got.Status != journal.StatusOK {
		t.Fatalf("Result = %+v, want a nil-error route to the fix node", got)
	}
	if got.Patch.Review.Counts != (belay.Counts{Blocker: 1, Critical: 1, Major: 1, Info: 2}) {
		t.Errorf("Counts = %+v", got.Patch.Review.Counts)
	}
	// #nosec G304 -- the path came from the node under test, inside t.TempDir().
	data, err := os.ReadFile(filepath.Join(f.runDir, got.Patch.Review.ReportPath))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	var archived belay.QualityReport
	if err := json.Unmarshal(data, &archived); err != nil {
		t.Fatalf("artifact is not a belay.QualityReport: %v", err)
	}
	assertReportRules(t, archived)
	if archived.Source != Source {
		t.Errorf("archived Source = %q, want %q", archived.Source, Source)
	}
}

// TestErrGateUnresolvedIsNotAFailedGate guards the distinction the whole
// package is built around against a future refactor collapsing it.
func TestErrGateUnresolvedIsNotAFailedGate(t *testing.T) {
	f := newFixture(t, config.SeverityMajor)
	fail := NewNode(&belaytest.FakeReviewer{
		Responses: []belay.QualityReport{scriptedReport(belay.GateFail, belay.SeverityBlocker)},
	})
	got, err := fail.Run(t.Context(), f.rc)
	if err != nil {
		t.Fatalf("a failed gate returned an error: %v", err)
	}
	if errors.Is(err, ErrGateUnresolved) {
		t.Fatal("a failed gate satisfied errors.Is(err, ErrGateUnresolved)")
	}
	if got.Next != graph.NodeFix {
		t.Fatalf("Next = %q, want %q", got.Next, graph.NodeFix)
	}
}
