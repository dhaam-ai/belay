package code_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
	"github.com/belay-dev/belay/pkg/belay/belaytest"
)

const (
	testRunID   = "2026-08-31T00-00-00Z-code"
	testPlanRel = "artifacts/plan.md"
	testPlan    = "# Plan\n\n1. Add Divide to calc.go\n"
	testGoal    = "add a Divide function"
)

// quiet returns a logger that discards, so a table test does not spray the
// node's Info lines across the test output.
func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fixture is one ready-to-run code node execution: a workspace, a run
// layout with a plan artifact already written, and a scriptable agent.
type fixture struct {
	rc    *graph.RunContext
	agent *belaytest.FakeAgent
	ws    string
}

// newFixture builds the happy path — approval off, plan present and
// approved, agent returning resp — then applies mut so a test can bend one
// thing at a time.
func newFixture(t *testing.T, resp belay.AgentResponse, mut ...func(*fixture)) fixture {
	t.Helper()

	ws := t.TempDir()
	layout, err := state.NewLayout(ws, testRunID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	agent := &belaytest.FakeAgent{Responses: []belay.AgentResponse{resp}}

	st := state.NewState(testGoal)
	st.Plan = state.Plan{Path: testPlanRel, Approved: true}

	f := fixture{
		ws:    ws,
		agent: agent,
		rc: &graph.RunContext{
			Goal:     testGoal,
			State:    st,
			Config:   config.Default(),
			Layout:   layout,
			NodeName: graph.NodeCode,
			Step:     3,
			Attempt:  1,
			Logger:   quiet(),
			Agent:    agent,
		},
	}
	f.writePlan(t, testPlan)
	for _, m := range mut {
		m(&f)
	}
	return f
}

// writePlan puts content at artifacts/plan.md through the same API the
// plan node uses.
func (f *fixture) writePlan(t *testing.T, content string) {
	t.Helper()
	if _, err := f.rc.WriteArtifact("plan.md", []byte(content)); err != nil {
		t.Fatalf("WriteArtifact(plan.md): %v", err)
	}
}

// nodeFile reads one of the node's own evidence files for this execution.
func (f *fixture) nodeFile(t *testing.T, name string) string {
	t.Helper()
	dir, err := f.rc.Layout.NodeDir(f.rc.Step, f.rc.NodeName)
	if err != nil {
		t.Fatalf("NodeDir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // path is a t.TempDir run directory
	if err != nil {
		t.Fatalf("read node file %q: %v", name, err)
	}
	return string(data)
}
