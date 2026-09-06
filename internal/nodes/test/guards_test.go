package test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// TestRunRefusesARelativeWorkspace closes the one derivation that passes the
// shape check but is still wrong: a Layout built over an empty workspace path
// resolves to ".", which would run the suite in whatever directory belay was
// started in rather than in the target repository.
func TestRunRefusesARelativeWorkspace(t *testing.T) {
	t.Parallel()

	// A relative workspace can no longer reach a node: state.NewLayout
	// refuses it outright, so the "." fallback is unreachable by
	// construction rather than caught downstream.
	for _, ws := range []string{"", "relative/repo"} {
		t.Run("NewLayout refuses "+ws, func(t *testing.T) {
			t.Parallel()
			if _, err := state.NewLayout(ws, "run-1"); err == nil {
				t.Fatalf("NewLayout(%q) succeeded; want refusal", ws)
			}
		})
	}
	t.Run("unset workspace on the context", func(t *testing.T) {
		t.Parallel()
		if _, err := workspaceDir(&graph.RunContext{}); !errors.Is(err, ErrNoWorkspace) {
			t.Fatalf("workspaceDir(unset) = %v, want ErrNoWorkspace", err)
		}
	})
}

// TestRunHonoursCancellationTheRunnerIgnored covers the re-check after the
// runner returns. FakeRunner does not consult ctx, so a runner that cancels
// mid-call and still reports success proves the node — not the runner — is what
// refuses to claim a verdict.
func TestRunHonoursCancellationTheRunnerIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	runner := &belaytest.FakeRunner{
		Func: func(_ context.Context, _ string) (belay.TestReport, error) {
			cancel() // the run is torn down while the suite is executing
			return belay.TestReport{Total: 9, Passed: 9}, nil
		},
	}
	rc, _ := newRC(t, runner)

	res, err := New().Run(ctx, rc)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if res.Next != "" {
		t.Errorf("Next = %q, want empty: a cancelled run reaches no verdict", res.Next)
	}
	if _, err := os.Stat(rc.Layout.ArtifactsDir()); !os.IsNotExist(err) {
		t.Error("a cancelled run wrote a report artifact")
	}
}

// TestRunReportsAnUnwritableArtifactDir: if the report cannot be archived the
// node errors rather than returning a verdict whose ReportPath points nowhere.
func TestRunReportsAnUnwritableArtifactDir(t *testing.T) {
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{{Total: 2, Passed: 2}}}
	rc, _ := newRC(t, runner)

	// Occupy artifacts/ with a regular file so MkdirAll cannot create it.
	if err := os.MkdirAll(rc.Layout.RunDir(), 0o750); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(rc.Layout.ArtifactsDir(), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	res, err := New().Run(context.Background(), rc)
	if err == nil {
		t.Fatal("Run() error = nil, want an archive error")
	}
	if res.Next == graph.NodeReview || res.Next == graph.NodeFix {
		t.Errorf("Next = %q, want no route when the report could not be archived", res.Next)
	}
}

// TestRunToleratesANilLogger: a RunContext assembled without a Logger must not
// panic the node.
func TestRunToleratesANilLogger(t *testing.T) {
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{{Total: 1, Passed: 1}}}
	rc, _ := newRC(t, runner)
	rc.Logger = nil

	res, err := New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Next != graph.NodeReview {
		t.Errorf("Next = %q, want %q", res.Next, graph.NodeReview)
	}
}

// TestReportNameIsStepScoped pins the artifact naming, which is what makes a
// re-run overwrite its own report instead of accumulating new ones.
func TestReportNameIsStepScoped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		step int
		want string
	}{
		{step: 0, want: "test-0000.json"},
		{step: 7, want: "test-0007.json"},
		{step: 42, want: "test-0042.json"},
		{step: 12345, want: "test-12345.json"},
	}
	for _, tt := range tests {
		if got := reportName(tt.step); got != tt.want {
			t.Errorf("reportName(%d) = %q, want %q", tt.step, got, tt.want)
		}
	}
}

// TestArtifactNameIsAcceptedByTheLayout guards against a name Layout would
// reject as unsafe.
func TestArtifactNameIsAcceptedByTheLayout(t *testing.T) {
	layout, err := state.NewLayout(t.TempDir(), "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	got, err := layout.ArtifactPath(reportName(7))
	if err != nil {
		t.Fatalf("ArtifactPath(%q) = %v", reportName(7), err)
	}
	if filepath.Base(got) != "test-0007.json" {
		t.Errorf("ArtifactPath base = %q", filepath.Base(got))
	}
}
