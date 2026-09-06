package code_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/code"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

func TestName(t *testing.T) {
	if got := code.New().Name(); got != graph.NodeCode {
		t.Fatalf("Name() = %q, want %q", got, graph.NodeCode)
	}
}

// The happy path: the node routes to write, reports OK, and archives both
// halves of the exchange.
func TestRunRoutesToWriteAndArchivesTheExchange(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{
		Text:      "Added Divide.",
		SessionID: "sess-1",
		Turns:     4,
	})

	res, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("Result.Validate: %v", err)
	}
	if res.Next != graph.NodeWrite {
		t.Fatalf("Next = %q, want %q", res.Next, graph.NodeWrite)
	}
	if res.Status != journal.StatusOK {
		t.Fatalf("Status = %v, want %v", res.Status, journal.StatusOK)
	}
	if res.Patch.Code == nil {
		t.Fatal("Patch.Code is nil; the code node must report its own output")
	}
	if got := res.Patch.Code.SessionID; got != "sess-1" {
		t.Fatalf("Patch.Code.SessionID = %q, want %q", got, "sess-1")
	}

	prompt := f.nodeFile(t, "prompt.txt")
	if !strings.Contains(prompt, testGoal) {
		t.Errorf("prompt.txt does not carry the goal:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Add Divide to calc.go") {
		t.Errorf("prompt.txt does not carry the plan:\n%s", prompt)
	}

	var archived belay.AgentResponse
	if err := json.Unmarshal([]byte(f.nodeFile(t, "response.json")), &archived); err != nil {
		t.Fatalf("response.json is not a belay.AgentResponse: %v", err)
	}
	if archived.SessionID != "sess-1" || archived.Turns != 4 {
		t.Fatalf("response.json = %+v, want the backend's response verbatim", archived)
	}
}

// The prompt must reach the backend intact rather than only reaching disk.
func TestRunSendsThePlanAndGoalToTheBackend(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "done"})

	if _, err := code.New().Run(context.Background(), f.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := f.agent.Calls()
	if len(calls) != 1 {
		t.Fatalf("CallCount = %d, want exactly 1", len(calls))
	}
	req := calls[0]
	if !strings.Contains(req.Prompt, testGoal) || !strings.Contains(req.Prompt, "Add Divide to calc.go") {
		t.Errorf("request Prompt is missing the goal or the plan:\n%s", req.Prompt)
	}
	if req.SystemPrompt == "" {
		t.Error("request SystemPrompt is empty; the edit-in-place instruction is unstated")
	}
	if req.WorkDir != f.ws {
		t.Errorf("WorkDir = %q, want the workspace root %q", req.WorkDir, f.ws)
	}
	if req.MaxTurns != f.rc.Config.Agent.MaxTurns {
		t.Errorf("MaxTurns = %d, want config value %d", req.MaxTurns, f.rc.Config.Agent.MaxTurns)
	}
	if req.Model != f.rc.Config.Agent.Model {
		t.Errorf("Model = %q, want config value %q", req.Model, f.rc.Config.Agent.Model)
	}
	assertTools(t, req.AllowedTools)
}

// The capability contract, asserted against what the backend actually
// received rather than against the constructor that built it.
//
// Edit and Write are what make this node able to do its job at all: nothing
// downstream applies a patch, so an agent without them leaves the workspace
// untouched and the run loops test -> fix -> test until the give-up budget is
// gone. Bash is withheld just as deliberately — the test node owns running the
// suite, and a coding agent with a shell can also commit, which is what makes
// a run abandonable.
func assertTools(t *testing.T, tools []string) {
	t.Helper()

	granted := make(map[string]bool, len(tools))
	for _, tool := range tools {
		granted[tool] = true
	}
	for _, want := range []string{"Read", "Grep", "Glob", "Edit", "Write"} {
		if !granted[want] {
			t.Errorf("AllowedTools is missing %q; the agent cannot implement the plan without it: %v",
				want, tools)
		}
	}
	if granted["Bash"] {
		t.Errorf("AllowedTools grants \"Bash\"; the test node runs the suite and the agent must not "+
			"run arbitrary commands or commit: %v", tools)
	}
	if len(tools) == 0 {
		t.Error("AllowedTools is empty, which selects the backend's default tool set, not this node's")
	}
}

// Session reuse, both directions: a first call must send nothing, and a
// continuation must send exactly what the blackboard holds.
func TestRunSessionReuse(t *testing.T) {
	tests := []struct {
		name        string
		priorSess   string
		respSess    string
		wantSent    string
		wantPatched string
	}{
		{
			name:        "first call sends no session and records the new one",
			priorSess:   "",
			respSess:    "sess-new",
			wantSent:    "",
			wantPatched: "sess-new",
		},
		{
			name:        "continuation sends the stored session",
			priorSess:   "sess-1",
			respSess:    "sess-1",
			wantSent:    "sess-1",
			wantPatched: "sess-1",
		},
		{
			name:        "a backend that renumbers the session wins",
			priorSess:   "sess-1",
			respSess:    "sess-2",
			wantSent:    "sess-1",
			wantPatched: "sess-2",
		},
		{
			name:        "a backend with no session concept does not erase the stored one",
			priorSess:   "sess-1",
			respSess:    "",
			wantSent:    "sess-1",
			wantPatched: "sess-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t,
				belay.AgentResponse{Text: "done", SessionID: tt.respSess},
				func(f *fixture) { f.rc.State.Code.SessionID = tt.priorSess },
			)

			res, err := code.New().Run(context.Background(), f.rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			calls := f.agent.Calls()
			if len(calls) != 1 {
				t.Fatalf("CallCount = %d, want 1", len(calls))
			}
			if got := calls[0].SessionID; got != tt.wantSent {
				t.Errorf("AgentRequest.SessionID = %q, want %q", got, tt.wantSent)
			}
			if got := res.Patch.Code.SessionID; got != tt.wantPatched {
				t.Errorf("Patch.Code.SessionID = %q, want %q", got, tt.wantPatched)
			}
		})
	}
}

// Usage must survive into the Result or the node's spend never reaches the
// budget ledger.
func TestRunReturnsAgentUsage(t *testing.T) {
	want := belay.Usage{InputTokens: 1200, OutputTokens: 340, USD: 0.0271, Estimated: true}
	f := newFixture(t, belay.AgentResponse{Text: "done", Usage: want})

	res, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Usage != want {
		t.Fatalf("Result.Usage = %+v, want %+v", res.Usage, want)
	}
}

// An agent failure is an error, never a StatusFailed Result, and every
// sentinel the caller branches on must survive the node's wrapping.
func TestRunAgentFailureIsAnError(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"toolchain missing sentinel", belay.ErrToolchainMissing},
		{"toolchain missing typed", &belay.ToolchainError{Tool: "claude"}},
		{"budget exceeded", &belay.BudgetError{SpentUSD: 6, LimitUSD: 5}},
		{"unsupported", belay.ErrUnsupported},
	}

	for _, tt := range sentinels {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, belay.AgentResponse{},
				func(f *fixture) { f.agent.Errs = []error{tt.err} },
			)

			res, err := code.New().Run(context.Background(), f.rc)
			if err == nil {
				t.Fatalf("Run succeeded; want an error. Result = %+v", res)
			}
			if !errors.Is(err, code.ErrAgent) {
				t.Errorf("errors.Is(err, ErrAgent) = false: %v", err)
			}
			if !errors.Is(err, tt.err) {
				t.Errorf("the backend's own error did not survive wrapping: %v", err)
			}
			if res.Status != journal.StatusUnknown {
				t.Errorf("Status = %v; an agent failure must not be reported as a Result", res.Status)
			}
		})
	}
}

// The specific errors.Is the fix loop depends on, pinned on its own so a
// regression names itself clearly.
func TestRunPreservesErrToolchainMissing(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{}, func(f *fixture) {
		f.agent.Errs = []error{&belay.ToolchainError{Tool: "claude"}}
	})

	_, err := code.New().Run(context.Background(), f.rc)
	if !errors.Is(err, belay.ErrToolchainMissing) {
		t.Fatalf("errors.Is(err, belay.ErrToolchainMissing) = false; got %v", err)
	}
	var te *belay.ToolchainError
	if !errors.As(err, &te) || te.Tool != "claude" {
		t.Fatalf("errors.As(*belay.ToolchainError) did not recover the tool name; got %v", err)
	}
}

func TestRunGuards(t *testing.T) {
	tests := []struct {
		name    string
		mut     func(*fixture)
		wantErr error
	}{
		{
			name:    "nil agent",
			mut:     func(f *fixture) { f.rc.Agent = nil },
			wantErr: code.ErrNoAgent,
		},
		{
			name: "approval required but plan unapproved",
			mut: func(f *fixture) {
				f.rc.Config.Graph.Approval = true
				f.rc.State.Plan.Approved = false
			},
			wantErr: code.ErrPlanNotApproved,
		},
		{
			name: "plan path unset",
			mut: func(f *fixture) {
				f.rc.State.Plan.Path = ""
			},
			wantErr: code.ErrPlanUnreadable,
		},
		{
			name: "plan artifact missing",
			mut: func(f *fixture) {
				f.rc.State.Plan.Path = "artifacts/nope.md"
			},
			wantErr: code.ErrPlanUnreadable,
		},
		{
			name: "plan artifact empty",
			mut: func(f *fixture) {
				f.writePlan(t, "   \n\t\n")
			},
			wantErr: code.ErrPlanUnreadable,
		},
		{
			name: "plan path escapes the artifacts directory",
			mut: func(f *fixture) {
				f.rc.State.Plan.Path = "../../etc/passwd"
			},
			wantErr: code.ErrPlanUnreadable,
		},
		{
			name: "plan path is absolute",
			mut: func(f *fixture) {
				f.rc.State.Plan.Path = "/etc/passwd"
			},
			wantErr: code.ErrPlanUnreadable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, belay.AgentResponse{Text: "done"}, tt.mut)

			_, err := code.New().Run(context.Background(), f.rc)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Run error = %v, want one wrapping %v", err, tt.wantErr)
			}
			if f.agent.CallCount() != 0 {
				t.Fatalf("the agent was invoked %d time(s); a guard must fire before any paid call",
					f.agent.CallCount())
			}
		})
	}
}

// Approval off is a supported configuration, not an implicit rejection.
func TestRunAllowsUnapprovedPlanWhenApprovalIsOff(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "done"}, func(f *fixture) {
		f.rc.Config.Graph.Approval = false
		f.rc.State.Plan.Approved = false
	})

	if _, err := code.New().Run(context.Background(), f.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.agent.CallCount() != 1 {
		t.Fatalf("CallCount = %d, want 1", f.agent.CallCount())
	}
}

// A missing plan must name the path it looked for, and expose it
// programmatically.
func TestPlanErrorNamesThePath(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{}, func(f *fixture) {
		f.rc.State.Plan.Path = "artifacts/missing-plan.md"
	})

	_, err := code.New().Run(context.Background(), f.rc)
	if err == nil {
		t.Fatal("Run succeeded with no plan artifact")
	}
	if !strings.Contains(err.Error(), "artifacts/missing-plan.md") {
		t.Errorf("error does not name the plan path: %v", err)
	}
	var pe *code.PlanError
	if !errors.As(err, &pe) {
		t.Fatalf("errors.As(*code.PlanError) = false; got %v", err)
	}
	if pe.Path != "artifacts/missing-plan.md" {
		t.Errorf("PlanError.Path = %q, want %q", pe.Path, "artifacts/missing-plan.md")
	}
}

func TestRunRejectsNilRunContext(t *testing.T) {
	_, err := code.New().Run(context.Background(), nil)
	if !errors.Is(err, code.ErrNoRunContext) {
		t.Fatalf("Run(nil) error = %v, want ErrNoRunContext", err)
	}
}

func TestRunRespectsContextCancellation(t *testing.T) {
	t.Run("canceled before the call", func(t *testing.T) {
		f := newFixture(t, belay.AgentResponse{Text: "done"})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := code.New().Run(ctx, f.rc)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
		if f.agent.CallCount() != 0 {
			t.Fatalf("the agent was invoked despite a canceled context")
		}
	})

	t.Run("context is handed to the backend", func(t *testing.T) {
		f := newFixture(t, belay.AgentResponse{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.agent.Func = func(ctx context.Context, _ belay.AgentRequest) (belay.AgentResponse, error) {
			cancel()
			return belay.AgentResponse{}, ctx.Err()
		}

		_, err := code.New().Run(ctx, f.rc)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want the backend's context.Canceled to surface", err)
		}
	})
}

// A zero Layout cannot name a workspace; the node must refuse it rather
// than pointing the agent at whatever directory happens to be the process's
// current one.
//
// The sentinel is deliberately not pinned. state.Layout now refuses a zero
// value inside ArtifactPath, so the plan read fails first and the run never
// reaches this package's own ErrWorkspace check — which stays in place as
// the guard for any Layout that is constructible but wrongly shaped. What
// this test protects is the property that survives either route: an
// unusable Layout costs no paid invocation.
func TestRunRejectsALayoutItCannotInvert(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "done"})

	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "artifacts"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "artifacts", "plan.md"), []byte(testPlan), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	t.Chdir(cwd)

	rc := *f.rc
	rc.Layout = state.Layout{}

	_, err := code.New().Run(context.Background(), &rc)
	if err == nil {
		t.Fatal("Run succeeded on a Layout that names no run directory")
	}
	if f.agent.CallCount() != 0 {
		t.Fatal("the agent was invoked without a resolvable workspace directory")
	}
}
