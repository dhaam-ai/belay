package test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// newRC returns a RunContext rooted at a real temporary workspace, so
// WriteArtifact writes somewhere real, plus that workspace's path.
func newRC(t *testing.T, runner belay.TestRunner) (*graph.RunContext, string) {
	t.Helper()
	ws := t.TempDir()
	layout, err := state.NewLayout(ws, "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	return &graph.RunContext{
		Goal:      "make the suite green",
		Layout:    layout,
		Workspace: layout.WorkspaceDir(),
		RunID:     layout.RunID(),
		NodeName:  graph.NodeTest,
		Step:      7,
		Attempt:   1,
		Runner:    runner,
		Logger:    slog.New(slog.DiscardHandler),
	}, ws
}

func TestNameIsTheCanonicalNodeName(t *testing.T) {
	if got := New().Name(); got != graph.NodeTest {
		t.Errorf("Name() = %q, want %q", got, graph.NodeTest)
	}
}

// TestRunRoutesOnTheReport is the heart of the node: every outcome a suite can
// actually reach is a Result with a nil error, and the only question is which
// way it branches.
func TestRunRoutesOnTheReport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		report   belay.TestReport
		wantNext string
		wantNote string
	}{
		{
			name:     "all pass routes to review",
			report:   belay.TestReport{Total: 12, Passed: 12},
			wantNext: graph.NodeReview,
			wantNote: "fake-runner: 12 tests, 0 failed",
		},
		{
			name: "failures route to fix",
			report: belay.TestReport{
				Total: 12, Passed: 9, Failed: 3,
				Failures: []belay.TestFailure{{Name: "TestA", File: "a_test.go", Line: 4, Message: "boom"}},
			},
			wantNext: graph.NodeFix,
			wantNote: "fake-runner: 12 tests, 3 failed",
		},
		{
			name:     "a single failure routes to fix",
			report:   belay.TestReport{Total: 1, Failed: 1},
			wantNext: graph.NodeFix,
			wantNote: "fake-runner: 1 tests, 1 failed",
		},
		{
			name:     "a build error routes to fix",
			report:   belay.TestReport{Total: 1, Failed: 1, Failures: []belay.TestFailure{{Name: "build", Message: "undefined: x"}}},
			wantNext: graph.NodeFix,
			wantNote: "fake-runner: 1 tests, 1 failed",
		},
		{
			name:     "skipped tests still pass when nothing failed",
			report:   belay.TestReport{Total: 10, Passed: 7},
			wantNext: graph.NodeReview,
			wantNote: "fake-runner: 10 tests, 0 failed",
		},
		{
			// Total == 0 is not green. See the package doc.
			name:     "a zero-test run routes to fix, not review",
			report:   belay.TestReport{},
			wantNext: graph.NodeFix,
			wantNote: "fake-runner: no tests discovered (0 total), not treated as passing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := &belaytest.FakeRunner{Responses: []belay.TestReport{tt.report}}
			rc, _ := newRC(t, runner)

			res, err := New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run() error = %v, want nil (a failing suite is not a Go error)", err)
			}
			if res.Next != tt.wantNext {
				t.Errorf("Next = %q, want %q", res.Next, tt.wantNext)
			}
			if res.Status != journal.StatusOK {
				t.Errorf("Status = %v, want %v", res.Status, journal.StatusOK)
			}
			if res.Note != tt.wantNote {
				t.Errorf("Note = %q, want %q", res.Note, tt.wantNote)
			}
			if (res.Usage != belay.Usage{}) {
				t.Errorf("Usage = %+v, want zero (this node calls no agent)", res.Usage)
			}
			if err := res.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}

			if res.Patch.Test == nil {
				t.Fatal("Patch.Test is nil, want the test block")
			}
			want := state.NewTest(tt.report, res.Patch.Test.ReportPath)
			if diff := cmp.Diff(want, *res.Patch.Test); diff != "" {
				t.Errorf("Patch.Test mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRunPassesTheWorkspaceDirToTheRunner pins the directory the suite runs
// in: the target repository's root, not the run directory underneath it.
func TestRunPassesTheWorkspaceDirToTheRunner(t *testing.T) {
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{{Total: 1, Passed: 1}}}
	rc, ws := newRC(t, runner)

	if _, err := New().Run(context.Background(), rc); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	calls := runner.TestCalls()
	if len(calls) != 1 {
		t.Fatalf("Test called %d times, want 1", len(calls))
	}
	if calls[0] != ws {
		t.Errorf("Test(dir) = %q, want the workspace root %q", calls[0], ws)
	}
}

func TestRunReturnsATypedErrorWhenTheRunnerIsNil(t *testing.T) {
	rc, _ := newRC(t, nil)
	rc.Runner = nil

	res, err := New().Run(context.Background(), rc) // must not panic
	if !errors.Is(err, ErrNoRunner) {
		t.Fatalf("Run() error = %v, want ErrNoRunner", err)
	}
	if res.Next != "" {
		t.Errorf("Next = %q, want empty", res.Next)
	}
}

func TestRunReturnsATypedErrorForANilRunContext(t *testing.T) {
	res, err := New().Run(context.Background(), nil) // must not panic
	if !errors.Is(err, ErrNilRunContext) {
		t.Fatalf("Run() error = %v, want ErrNilRunContext", err)
	}
	if res.Next != "" {
		t.Errorf("Next = %q, want empty", res.Next)
	}
}

func TestRunRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	// FakeRunner ignores ctx entirely and reports success, so both cases
	// prove the node itself enforces cancellation rather than relying on the
	// runner to notice.
	tests := []struct {
		name    string
		reports []belay.TestReport
	}{
		{name: "passing suite", reports: []belay.TestReport{{Total: 3, Passed: 3}}},
		{name: "failing suite", reports: []belay.TestReport{{Total: 3, Failed: 3}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := &belaytest.FakeRunner{Responses: tt.reports}
			rc, _ := newRC(t, runner)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			res, err := New().Run(ctx, rc)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run() error = %v, want context.Canceled", err)
			}
			if res.Next == graph.NodeFix {
				t.Error("a cancelled run must not route to fix")
			}
		})
	}
}

// TestRunArchivesTheFullReport checks that the artifact holds what the
// blackboard projection drops — Duration and Raw — and that the path recorded
// in the patch is relative to the run directory.
func TestRunArchivesTheFullReport(t *testing.T) {
	report := belay.TestReport{
		Total: 5, Passed: 4, Failed: 1,
		Failures: []belay.TestFailure{{Name: "TestX", File: "x_test.go", Line: 12, Message: "want 1 got 2"}},
		Duration: 1500 * time.Millisecond,
		Raw:      json.RawMessage(`"raw go test -json stream"`),
	}
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{report}}
	rc, _ := newRC(t, runner)

	res, err := New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	rel := res.Patch.Test.ReportPath
	if rel == "" {
		t.Fatal("ReportPath is empty")
	}
	if filepath.IsAbs(rel) {
		t.Errorf("ReportPath = %q, want a run-relative path", rel)
	}
	if want := filepath.Join("artifacts", "test-0007.json"); rel != want {
		t.Errorf("ReportPath = %q, want %q", rel, want)
	}

	//nolint:gosec // path is the run dir joined with the node's own returned relative path
	data, err := os.ReadFile(filepath.Join(rc.Layout.RunDir(), rel))
	if err != nil {
		t.Fatalf("read archived report: %v", err)
	}
	var got belay.TestReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("archived report is not valid JSON: %v", err)
	}
	if diff := cmp.Diff(report, got); diff != "" {
		t.Errorf("archived report mismatch (-want +got):\n%s", diff)
	}
}
