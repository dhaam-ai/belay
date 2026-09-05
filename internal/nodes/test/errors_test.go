package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
	"github.com/belay-dev/belay/pkg/belay/belaytest"
)

// TestRunTreatsAMissingToolchainAsAnErrorNotAFixRoute is the test this node
// exists for.
//
// A missing `go` or `pytest` is a machine that needs software installed, not
// code that needs an agent to edit it. Routing it to the fix loop would spend
// every give_up attempt asking an agent to repair source that was never
// broken, then fail the run for the wrong reason.
func TestRunTreatsAMissingToolchainAsAnErrorNotAFixRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		wantTool string
	}{
		{
			name: "bare sentinel",
			err:  belay.ErrToolchainMissing,
		},
		{
			name: "wrapped sentinel",
			err:  fmt.Errorf("%w: pytest", belay.ErrToolchainMissing),
		},
		{
			name:     "structured ToolchainError",
			err:      &belay.ToolchainError{Tool: "go"},
			wantTool: "go",
		},
		{
			name:     "structured ToolchainError with a cause",
			err:      &belay.ToolchainError{Tool: "pytest", Err: os.ErrNotExist},
			wantTool: "pytest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := &belaytest.FakeRunner{
				// A report alongside the error: even if a runner hands back
				// something that looks routable, a toolchain error wins.
				Responses: []belay.TestReport{{Total: 0}},
				Errs:      []error{tt.err},
			}
			rc, _ := newRC(t, runner)

			res, err := New().Run(context.Background(), rc)

			if err == nil {
				t.Fatal("Run() error = nil, want a toolchain error")
			}
			if !errors.Is(err, belay.ErrToolchainMissing) {
				t.Errorf("errors.Is(err, belay.ErrToolchainMissing) = false, want true (err = %v)", err)
			}

			// The whole point: this must not become a fix-loop iteration.
			if res.Next == graph.NodeFix {
				t.Error("a missing toolchain routed to the fix node; it must not")
			}
			if res.Next != "" {
				t.Errorf("Next = %q, want empty: an error means no verdict to route on", res.Next)
			}
			if res.Status == journal.StatusOK {
				t.Error("Status = StatusOK on an error return")
			}
			if res.Patch.Test != nil {
				t.Error("Patch.Test was set for a run that never produced a report")
			}

			if tt.wantTool != "" {
				var te *belay.ToolchainError
				if !errors.As(err, &te) {
					t.Fatalf("errors.As(*belay.ToolchainError) = false, want true")
				}
				if te.Tool != tt.wantTool {
					t.Errorf("ToolchainError.Tool = %q, want %q", te.Tool, tt.wantTool)
				}
			}
		})
	}
}

// TestRunPropagatesANonToolchainRunnerError covers the other "could not run"
// errors: they are also errors, not routes, but they are not toolchain errors.
func TestRunPropagatesANonToolchainRunnerError(t *testing.T) {
	sentinel := errors.New("workspace vanished")
	runner := &belaytest.FakeRunner{Errs: []error{sentinel}}
	rc, _ := newRC(t, runner)

	res, err := New().Run(context.Background(), rc)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v, want it to wrap the runner's error", err)
	}
	if errors.Is(err, belay.ErrToolchainMissing) {
		t.Error("a generic runner error must not masquerade as a missing toolchain")
	}
	if res.Next != "" {
		t.Errorf("Next = %q, want empty", res.Next)
	}
}

// TestRunRefusesAnUnderivableWorkspace pins the refusal rather than a fallback:
// an unset Workspace must never resolve to the process's working directory,
// because that would run a stranger's test suite -- and hand an agent write
// access -- in whatever directory belay started in.
func TestRunRefusesAnUnderivableWorkspace(t *testing.T) {
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{{Total: 1, Passed: 1}}}
	rc, _ := newRC(t, runner)
	rc.Workspace = "" // the dispatcher always sets this; prove the node checks

	res, err := New().Run(context.Background(), rc)
	if !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("Run() error = %v, want ErrNoWorkspace", err)
	}
	if res.Next != "" {
		t.Errorf("Next = %q, want empty", res.Next)
	}
	if n := runner.CallCount(); n != 0 {
		t.Errorf("runner was called %d times; it must not run before the directory is known", n)
	}
}

// TestUnsetWorkspaceIsRefused: the dispatcher supplies RunContext.Workspace.
// If it is ever empty the node must refuse, never fall back to a relative path
// -- that would run the target's suite inside belay's own directory.
func TestUnsetWorkspaceIsRefused(t *testing.T) {
	t.Parallel()

	rc, _ := newRC(t, &belaytest.FakeRunner{
		Responses: []belay.TestReport{{Total: 1, Passed: 1}},
	})
	rc.Workspace = ""

	if _, err := New().Run(context.Background(), rc); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("Run() error = %v, want ErrNoWorkspace", err)
	}
}

// state.NewLayout is the upstream guard: a relative or empty workspace can
// never enter a Layout in the first place.
func TestNewLayoutRefusesARelativeWorkspace(t *testing.T) {
	t.Parallel()

	for _, ws := range []string{"", ".", "relative/path"} {
		if _, err := state.NewLayout(ws, "run-1"); err == nil {
			t.Errorf("NewLayout(%q) succeeded; want refusal", ws)
		}
	}
}

// TestRunRejectsAReportItCannotArchive covers a runner that violates the
// TestReport contract by putting a bare `go test -json` stream in Raw, which is
// a sequence of JSON objects and therefore not valid JSON on its own. That is an
// adapter bug, so it stays on the error path rather than becoming a fix route.
func TestRunRejectsAReportItCannotArchive(t *testing.T) {
	runner := &belaytest.FakeRunner{
		Responses: []belay.TestReport{{
			Total: 2, Passed: 2,
			Raw: json.RawMessage(`{"Action":"run"}` + "\n" + `{"Action":"pass"}`),
		}},
	}
	rc, _ := newRC(t, runner)

	res, err := New().Run(context.Background(), rc)
	if err == nil {
		t.Fatal("Run() error = nil, want an encode error")
	}
	if res.Next == graph.NodeFix || res.Next == graph.NodeReview {
		t.Errorf("Next = %q, want no route: an adapter bug is not a verdict", res.Next)
	}
}
