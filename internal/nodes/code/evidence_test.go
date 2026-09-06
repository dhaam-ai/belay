package code_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/nodes/code"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// A backend whose Raw bytes are not JSON must not take the whole node down
// over a debugging artifact.
func TestRunToleratesUnparsableRawOutput(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{
		Text:      "done",
		SessionID: "s1",
		Raw:       json.RawMessage("not json at all"),
	})

	res, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Patch.Code.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", res.Patch.Code.SessionID)
	}

	var archived map[string]any
	if err := json.Unmarshal([]byte(f.nodeFile(t, "response.json")), &archived); err != nil {
		t.Fatalf("response.json is not valid JSON: %v", err)
	}
	if archived["raw"] != nil {
		t.Errorf("response.json kept unparsable raw output: %v", archived["raw"])
	}
	if archived["session_id"] != "s1" {
		t.Errorf("response.json lost the fields the graph reads: %v", archived)
	}
}

// A valid Raw is archived verbatim.
func TestRunArchivesValidRawOutput(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{
		Text: "done",
		Raw:  json.RawMessage(`{"total_cost_usd":0.02}`),
	})

	if _, err := code.New().Run(context.Background(), f.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.nodeFile(t, "response.json"); !strings.Contains(got, "total_cost_usd") {
		t.Errorf("response.json dropped a valid raw payload:\n%s", got)
	}
}

// Per the AgentBackend contract a failed Invoke's response is to be
// discarded, so the node must not archive one as if it were evidence.
func TestRunDoesNotArchiveAFailedResponse(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{}, func(f *fixture) {
		f.agent.Errs = []error{errors.New("backend exploded")}
	})

	if _, err := code.New().Run(context.Background(), f.rc); err == nil {
		t.Fatal("Run succeeded despite a backend error")
	}
	dir, err := f.rc.Layout.NodeDir(f.rc.Step, f.rc.NodeName)
	if err != nil {
		t.Fatalf("NodeDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "response.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("response.json exists after a failed invocation (stat err = %v)", err)
	}
	// The prompt is still evidence: it records what was asked before the
	// failure, which is exactly what a post-mortem needs.
	if _, err := os.Stat(filepath.Join(dir, "prompt.txt")); err != nil {
		t.Errorf("prompt.txt is missing after a failed invocation: %v", err)
	}
}

// A Step the Layout refuses must stop the node before it spends money,
// not after.
func TestRunRefusesAnUnusableStepBeforeInvoking(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "done"}, func(f *fixture) {
		f.rc.Step = -1
	})

	_, err := code.New().Run(context.Background(), f.rc)
	if !errors.Is(err, state.ErrInvalidPathSegment) {
		t.Fatalf("Run error = %v, want one wrapping state.ErrInvalidPathSegment", err)
	}
	if f.agent.CallCount() != 0 {
		t.Fatalf("the agent was invoked %d time(s) despite an unwritable node directory", f.agent.CallCount())
	}
}

// The dispatcher is expected to supply a logger, but a node must not
// panic on a RunContext that does not.
func TestRunToleratesANilLogger(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "done", SessionID: "s1"},
		func(f *fixture) { f.rc.Logger = nil })

	if _, err := code.New().Run(context.Background(), f.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// PlanError is exported API; its zero-Err form must still read well and
// still answer errors.Is.
func TestPlanErrorWithoutACause(t *testing.T) {
	err := error(&code.PlanError{Path: "artifacts/plan.md"})
	if !errors.Is(err, code.ErrPlanUnreadable) {
		t.Errorf("errors.Is(err, ErrPlanUnreadable) = false")
	}
	if !strings.Contains(err.Error(), "artifacts/plan.md") {
		t.Errorf("Error() = %q, want it to name the path", err)
	}
	if got := (&code.PlanError{}).Error(); !strings.Contains(got, "(unset)") {
		t.Errorf("an empty path reads as %q, want it marked unset", got)
	}
}
