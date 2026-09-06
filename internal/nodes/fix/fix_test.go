package fix_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/nodes/fix"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
	"github.com/belay-dev/belay/pkg/belay/belaytest"
	"github.com/google/go-cmp/cmp"
)

const fixStep = 7

// harness is one fully wired fix-node execution: a real run layout under a
// temp workspace, a scriptable agent, and the RunContext the dispatcher
// would have built.
type harness struct {
	rc    *graph.RunContext
	agent *belaytest.FakeAgent
	ws    string
}

func newHarness(t *testing.T, st state.State) *harness {
	t.Helper()
	ws := t.TempDir()
	layout, err := state.NewLayout(ws, "20260101T000000Z-abcdef123456")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	agent := &belaytest.FakeAgent{
		NameValue: "fake-agent",
		Responses: []belay.AgentResponse{{
			Text:      "fixed the off-by-one in the limiter",
			SessionID: "sess-42",
			Usage:     belay.Usage{InputTokens: 1200, OutputTokens: 340, USD: 0.0731},
			Turns:     4,
		}},
	}
	return &harness{
		ws:    ws,
		agent: agent,
		rc: &graph.RunContext{
			Goal:      "add rate limiting to the login handler",
			State:     st,
			Config:    config.Default(),
			Layout:    layout,
			Workspace: layout.WorkspaceDir(),
			RunID:     layout.RunID(),
			NodeName:  graph.NodeFix,
			Step:      fixStep,
			Attempt:   1,
			Logger:    slog.New(slog.DiscardHandler),
			Agent:     agent,
		},
	}
}

func (h *harness) nodeDir(t *testing.T) string {
	t.Helper()
	dir, err := h.rc.Layout.NodeDir(fixStep, graph.NodeFix)
	if err != nil {
		t.Fatalf("NodeDir: %v", err)
	}
	return dir
}

func failingTests() state.Test {
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

func greenTests() state.Test {
	return state.Test{Total: 14, Passed: 14, ReportPath: "artifacts/test.json"}
}

func failedGate() state.Review {
	return state.Review{
		Source: "golangci-lint",
		Gate:   belay.GateFail,
		Counts: belay.Counts{Blocker: 1, Major: 2},
		Issues: []belay.Issue{
			{RuleID: "gosec:G401", Severity: belay.SeverityBlocker, File: "internal/crypto/hash.go", Line: 17,
				Message: "Use of weak cryptographic primitive", Effort: "5min"},
			{RuleID: "revive:exported", Severity: belay.SeverityMajor, File: "internal/auth/login.go", Line: 8,
				Message: "exported function Login should have comment"},
			{RuleID: "staticcheck:SA4006", Severity: belay.SeverityMajor, File: "internal/auth/limiter.go", Line: 3,
				Message: "value never read"},
		},
		Summary: "3 issues (1 blocker, 2 major)",
	}
}

func failingState(attempts int) state.State {
	st := state.NewState("add rate limiting to the login handler")
	st.Code = state.Code{
		SessionID:    "sess-code-1",
		ChangedFiles: []string{"internal/auth/login.go", "internal/auth/limiter.go"},
	}
	st.Test = failingTests()
	st.Fix = state.Fix{Attempts: attempts}
	return st
}

func TestNameIsTheCanonicalFixNode(t *testing.T) {
	if got, want := fix.New().Name(), graph.NodeFix; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
}

// TestGiveUpBoundary is the acceptance test for the cap this node owns.
//
// It walks every attempt count from zero through the cap for several cap
// values, because both off-by-one directions are silent: one too many and
// the fix loop never terminates, one too few and a run gives up with budget
// it was configured to spend.
func TestGiveUpBoundary(t *testing.T) {
	for _, giveUp := range []int{1, 2, 3} {
		for attempts := 0; attempts <= giveUp; attempts++ {
			name := fmt.Sprintf("give_up=%d/attempts=%d", giveUp, attempts)
			t.Run(name, func(t *testing.T) {
				h := newHarness(t, failingState(attempts))
				h.rc.Config.Graph.GiveUp = giveUp

				res, err := fix.New().Run(context.Background(), h.rc)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if err := res.Validate(); err != nil {
					t.Fatalf("Result.Validate: %v", err)
				}

				exhausted := attempts >= giveUp
				if exhausted {
					if res.Status != journal.StatusFailed {
						t.Errorf("Status = %v, want StatusFailed at the cap", res.Status)
					}
					if !strings.Contains(res.Note, "give_up_exhausted") {
						t.Errorf("Note = %q, want it to name give_up_exhausted", res.Note)
					}
					if !strings.Contains(res.Note, fmt.Sprintf("%d of %d fix attempts", attempts, giveUp)) {
						t.Errorf("Note = %q, want it to carry the attempt count", res.Note)
					}
					if res.Patch.Fix == nil || res.Patch.Fix.Attempts != attempts || !res.Patch.Fix.GiveUp {
						t.Errorf("Patch.Fix = %+v, want Attempts=%d and GiveUp=true", res.Patch.Fix, attempts)
					}
					if n := h.agent.CallCount(); n != 0 {
						t.Errorf("agent called %d times at the cap; the cap must be enforced before spending", n)
					}
					if _, err := os.Stat(filepath.Join(h.nodeDir(t), "prompt.txt")); !os.IsNotExist(err) {
						t.Error("a give-up must not write a prompt: nothing was asked of the agent")
					}
					return
				}

				if res.Status != journal.StatusOK {
					t.Errorf("Status = %v, want StatusOK below the cap", res.Status)
				}
				if res.Next != graph.NodeTest {
					t.Errorf("Next = %q, want %q", res.Next, graph.NodeTest)
				}
				if res.Patch.Fix == nil || res.Patch.Fix.Attempts != attempts+1 {
					t.Errorf("Patch.Fix = %+v, want Attempts=%d", res.Patch.Fix, attempts+1)
				}
				if res.Patch.Fix != nil && res.Patch.Fix.GiveUp {
					t.Error("Patch.Fix.GiveUp must stay false while attempts remain")
				}
				if n := h.agent.CallCount(); n != 1 {
					t.Errorf("agent called %d times, want exactly 1", n)
				}
			})
		}
	}
}

// TestGiveUpNonPositiveCapPermitsNothing pins the defensive direction of
// the cap: a misconfigured non-positive give_up must stop the loop, never
// let it run unbounded.
func TestGiveUpNonPositiveCapPermitsNothing(t *testing.T) {
	h := newHarness(t, failingState(0))
	h.rc.Config.Graph.GiveUp = 0

	res, err := fix.New().Run(context.Background(), h.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != journal.StatusFailed {
		t.Errorf("Status = %v, want StatusFailed", res.Status)
	}
	for _, want := range []string{"give_up_exhausted", "no fix attempt is permitted"} {
		if !strings.Contains(res.Note, want) {
			t.Errorf("Note = %q, want it to contain %q", res.Note, want)
		}
	}
	if n := h.agent.CallCount(); n != 0 {
		t.Errorf("agent called %d times, want 0", n)
	}
}

// TestPatchIsAbsoluteAndIdempotent is the ADR-0002 acceptance test:
// applying the returned Patch a second time — which is what a crash between
// applying and recording looks like on resume — must leave Attempts
// exactly where the first application put it.
func TestPatchIsAbsoluteAndIdempotent(t *testing.T) {
	tests := []struct {
		name     string
		attempts int
		giveUp   int
		want     state.Fix
	}{
		{name: "below the cap", attempts: 1, giveUp: 3, want: state.Fix{Attempts: 2}},
		{name: "at the cap", attempts: 3, giveUp: 3, want: state.Fix{Attempts: 3, GiveUp: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, failingState(tt.attempts))
			h.rc.Config.Graph.GiveUp = tt.giveUp

			res, err := fix.New().Run(context.Background(), h.rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			st := failingState(tt.attempts)
			if err := res.Patch.Apply(&st); err != nil {
				t.Fatalf("first Apply: %v", err)
			}
			once := st.Fix
			if diff := cmp.Diff(tt.want, once); diff != "" {
				t.Errorf("Fix after one Apply (-want +got):\n%s", diff)
			}

			if err := res.Patch.Apply(&st); err != nil {
				t.Fatalf("second Apply: %v", err)
			}
			if diff := cmp.Diff(once, st.Fix); diff != "" {
				t.Errorf("applying the patch twice changed Fix (-first +second):\n%s", diff)
			}
		})
	}
}

// TestEntryPaths covers both ways into this node, and the precedence
// between them when both records look failing.
func TestEntryPaths(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*state.State)
		wantPrompt  []string
		wantAbsent  []string
		wantNoteHas string
	}{
		{
			name:        "from a failing test run",
			mutate:      func(st *state.State) { st.Test = failingTests() },
			wantPrompt:  []string{"## Failing tests", "TestLogin/empty_password", "internal/auth/login_test.go:42", "status = 500, want 400"},
			wantAbsent:  []string{"## Failed quality gate"},
			wantNoteHas: "2 of 14 tests failing",
		},
		{
			name: "from a failed review gate",
			mutate: func(st *state.State) {
				st.Test = greenTests()
				st.Review = failedGate()
			},
			wantPrompt:  []string{"## Failed quality gate", "gosec:G401", "internal/crypto/hash.go:17", "Use of weak cryptographic primitive"},
			wantAbsent:  []string{"## Failing tests"},
			wantNoteHas: "quality gate failed with 3 issues",
		},
		{
			name: "both failing: the test failure wins",
			mutate: func(st *state.State) {
				st.Test = failingTests()
				st.Review = failedGate()
			},
			wantPrompt:  []string{"## Failing tests", "TestLogin/locked_account"},
			wantAbsent:  []string{"## Failed quality gate", "gosec:G401"},
			wantNoteHas: "2 of 14 tests failing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := state.NewState("add rate limiting to the login handler")
			st.Code = state.Code{SessionID: "sess-code-1"}
			tt.mutate(&st)
			h := newHarness(t, st)

			res, err := fix.New().Run(context.Background(), h.rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Next != graph.NodeTest {
				t.Errorf("Next = %q, want %q", res.Next, graph.NodeTest)
			}
			if !strings.Contains(res.Note, tt.wantNoteHas) {
				t.Errorf("Note = %q, want it to contain %q", res.Note, tt.wantNoteHas)
			}

			calls := h.agent.Calls()
			if len(calls) != 1 {
				t.Fatalf("agent called %d times, want 1", len(calls))
			}
			for _, want := range tt.wantPrompt {
				if !strings.Contains(calls[0].Prompt, want) {
					t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, calls[0].Prompt)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(calls[0].Prompt, absent) {
					t.Errorf("prompt should not contain %q\n--- prompt ---\n%s", absent, calls[0].Prompt)
				}
			}
		})
	}
}

// TestAgentRequestShape asserts what the node actually sends, including the
// session it resumes. A fix that starts a cold session is a rewrite, not a
// repair, and it costs a full context reload every attempt.
func TestAgentRequestShape(t *testing.T) {
	h := newHarness(t, failingState(1))
	h.rc.Config.Agent.Model = "sonnet"
	h.rc.Config.Agent.MaxTurns = 17

	if _, err := fix.New().Run(context.Background(), h.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := h.agent.Calls()
	if len(calls) != 1 {
		t.Fatalf("agent called %d times, want 1", len(calls))
	}
	got := calls[0]
	if got.SessionID != "sess-code-1" {
		t.Errorf("SessionID = %q, want the code node's session %q", got.SessionID, "sess-code-1")
	}
	if got.Model != "sonnet" {
		t.Errorf("Model = %q, want the configured model", got.Model)
	}
	if got.MaxTurns != 17 {
		t.Errorf("MaxTurns = %d, want the configured 17", got.MaxTurns)
	}
	if got.WorkDir != h.ws {
		t.Errorf("WorkDir = %q, want the workspace root %q", got.WorkDir, h.ws)
	}
	if strings.TrimSpace(got.SystemPrompt) == "" {
		t.Error("SystemPrompt is empty")
	}
	if strings.TrimSpace(got.Prompt) == "" {
		t.Fatal("Prompt is empty")
	}
}

// TestSessionIDEmptyStaysEmpty: a backend without session support must not
// receive a fabricated session ID, because AgentRequest.SessionID is
// documented to fail loudly on a backend that cannot resume.
func TestSessionIDEmptyStaysEmpty(t *testing.T) {
	st := failingState(0)
	st.Code.SessionID = ""
	h := newHarness(t, st)

	if _, err := fix.New().Run(context.Background(), h.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call, ok := h.agent.LastCall()
	if !ok {
		t.Fatal("agent was never called")
	}
	if call.SessionID != "" {
		t.Errorf("SessionID = %q, want empty when no session was recorded", call.SessionID)
	}
}

// TestUsageIsReturned guards the budget guard: usage this node drops is
// spend nothing downstream can see.
func TestUsageIsReturned(t *testing.T) {
	h := newHarness(t, failingState(0))
	want := belay.Usage{InputTokens: 1200, OutputTokens: 340, USD: 0.0731}

	res, err := fix.New().Run(context.Background(), h.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if diff := cmp.Diff(want, res.Usage); diff != "" {
		t.Errorf("Usage (-want +got):\n%s", diff)
	}
}

// TestNodeFilesWritten checks the evidence trail a human reads when a fix
// loop went wrong.
func TestNodeFilesWritten(t *testing.T) {
	h := newHarness(t, failingState(1))

	if _, err := fix.New().Run(context.Background(), h.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dir := h.nodeDir(t)

	prompt, err := os.ReadFile(filepath.Join(dir, "prompt.txt")) //nolint:gosec // path built from the test's own Layout
	if err != nil {
		t.Fatalf("read prompt.txt: %v", err)
	}
	call, _ := h.agent.LastCall()
	if string(prompt) != call.Prompt {
		t.Error("prompt.txt does not match the prompt actually sent")
	}
	if !strings.Contains(string(prompt), "TestLogin/empty_password") {
		t.Error("prompt.txt does not name the failing test")
	}

	raw, err := os.ReadFile(filepath.Join(dir, "response.json")) //nolint:gosec // path built from the test's own Layout
	if err != nil {
		t.Fatalf("read response.json: %v", err)
	}
	var got belay.AgentResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode response.json: %v", err)
	}
	if got.Text != "fixed the off-by-one in the limiter" || got.SessionID != "sess-42" {
		t.Errorf("response.json = %+v, want the agent's response verbatim", got)
	}
	if got.Usage.USD != 0.0731 {
		t.Errorf("response.json lost the usage: %+v", got.Usage)
	}
}

// TestAgentErrorsPropagate covers the error contract: a backend failure is
// a Go error, and the sentinels a caller branches on must survive the
// wrapping this node adds.
func TestAgentErrorsPropagate(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"toolchain missing", fmt.Errorf("claude cli: %w", belay.ErrToolchainMissing), belay.ErrToolchainMissing},
		{"typed toolchain error", &belay.ToolchainError{Tool: "claude"}, belay.ErrToolchainMissing},
		{"budget exceeded", &belay.BudgetError{SpentUSD: 6, LimitUSD: 5}, belay.ErrBudgetExceeded},
		{"session unsupported", fmt.Errorf("resume: %w", belay.ErrUnsupported), belay.ErrUnsupported},
		{"plain failure", errors.New("backend exploded"), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, failingState(0))
			h.agent.Errs = []error{tt.err}

			res, err := fix.New().Run(context.Background(), h.rc)
			if err == nil {
				t.Fatal("Run returned nil error for a failing agent")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("errors.Is(err, %v) = false; err = %v", tt.want, err)
			}
			if !errors.Is(err, tt.err) {
				t.Errorf("the backend's own error did not survive wrapping: %v", err)
			}
			if diff := cmp.Diff(graph.Result{}, res); diff != "" {
				t.Errorf("Result on error (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPreconditionErrors covers every way this node refuses to run, none of
// which may panic.
func TestPreconditionErrors(t *testing.T) {
	t.Run("nil run context", func(t *testing.T) {
		res, err := fix.New().Run(context.Background(), nil)
		if !errors.Is(err, fix.ErrNilRunContext) {
			t.Fatalf("err = %v, want ErrNilRunContext", err)
		}
		if diff := cmp.Diff(graph.Result{}, res); diff != "" {
			t.Errorf("Result (-want +got):\n%s", diff)
		}
	})

	t.Run("nil agent", func(t *testing.T) {
		h := newHarness(t, failingState(0))
		h.rc.Agent = nil
		if _, err := fix.New().Run(context.Background(), h.rc); !errors.Is(err, fix.ErrNoAgent) {
			t.Fatalf("err = %v, want ErrNoAgent", err)
		}
	})

	t.Run("nothing is failing", func(t *testing.T) {
		st := state.NewState("goal")
		st.Test = greenTests()
		st.Review = state.Review{Gate: belay.GatePass}
		h := newHarness(t, st)
		if _, err := fix.New().Run(context.Background(), h.rc); !errors.Is(err, fix.ErrNoFailureContext) {
			t.Fatalf("err = %v, want ErrNoFailureContext", err)
		}
		if n := h.agent.CallCount(); n != 0 {
			t.Errorf("agent called %d times with nothing to fix", n)
		}
	})

	t.Run("gate error is not a repairable cause", func(t *testing.T) {
		st := state.NewState("goal")
		st.Test = greenTests()
		st.Review = state.Review{Gate: belay.GateError, Source: "sonar", Summary: "scanner crashed"}
		h := newHarness(t, st)
		if _, err := fix.New().Run(context.Background(), h.rc); !errors.Is(err, fix.ErrNoFailureContext) {
			t.Fatalf("err = %v, want ErrNoFailureContext", err)
		}
	})

	t.Run("unset workspace", func(t *testing.T) {
		h := newHarness(t, failingState(0))
		h.rc.Workspace = ""
		if _, err := fix.New().Run(context.Background(), h.rc); !errors.Is(err, fix.ErrNoWorkspace) {
			t.Fatalf("err = %v, want ErrNoWorkspace", err)
		}
		if n := h.agent.CallCount(); n != 0 {
			t.Errorf("agent called %d times without a workspace", n)
		}
	})

	t.Run("nil logger does not panic", func(t *testing.T) {
		h := newHarness(t, failingState(0))
		h.rc.Logger = nil
		if _, err := fix.New().Run(context.Background(), h.rc); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
}

// TestContextCancellation: a cancelled run must not start a new agent call.
func TestContextCancellation(t *testing.T) {
	h := newHarness(t, failingState(0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fix.New().Run(ctx, h.rc)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := h.agent.CallCount(); n != 0 {
		t.Errorf("agent called %d times after cancellation", n)
	}
}

// TestContextPassedToAgent proves cancellation reaches the backend rather
// than only being checked once on entry.
func TestContextPassedToAgent(t *testing.T) {
	h := newHarness(t, failingState(0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.agent.Func = func(ctx context.Context, _ belay.AgentRequest) (belay.AgentResponse, error) {
		cancel()
		return belay.AgentResponse{}, ctx.Err()
	}
	if _, err := fix.New().Run(ctx, h.rc); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled from the backend", err)
	}
}

// TestWorkDirTracksLayout pins the derivation of the workspace root against
// state.NewLayout itself, so a change to the run-directory shape fails here
// instead of pointing an agent at the wrong repository.
func TestWorkDirTracksLayout(t *testing.T) {
	ws := t.TempDir()
	layout, err := state.NewLayout(ws, "run-xyz")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if got, want := layout.WorkspaceDir(), ws; got != want {
		t.Fatalf("Layout.WorkspaceDir() = %q, want %q", got, want)
	}

	h := newHarness(t, failingState(0))
	h.rc.Layout = layout
	h.rc.Workspace = layout.WorkspaceDir()
	if _, err := fix.New().Run(context.Background(), h.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call, _ := h.agent.LastCall()
	if call.WorkDir != ws {
		t.Errorf("WorkDir = %q, want %q", call.WorkDir, ws)
	}
}

// TestGoalFallsBackToBlackboard: a resumed run rebuilding a RunContext may
// not carry the goal, and a prompt without one is measurably worse.
func TestGoalFallsBackToBlackboard(t *testing.T) {
	h := newHarness(t, failingState(0))
	h.rc.Goal = ""

	if _, err := fix.New().Run(context.Background(), h.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call, _ := h.agent.LastCall()
	if !strings.Contains(call.Prompt, "add rate limiting to the login handler") {
		t.Errorf("prompt lost the goal:\n%s", call.Prompt)
	}
}

// TestRerunIsPure re-runs the node against the same snapshot, which is what
// the dispatcher does after a crash inside a node. The second run must ask
// for the same thing and produce the same Patch — the node must not carry
// state between executions.
func TestRerunIsPure(t *testing.T) {
	h := newHarness(t, failingState(1))
	node := fix.New()

	first, err := node.Run(context.Background(), h.rc)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	second, err := node.Run(context.Background(), h.rc)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if diff := cmp.Diff(first, second); diff != "" {
		t.Errorf("re-running the node changed its Result (-first +second):\n%s", diff)
	}
	calls := h.agent.Calls()
	if len(calls) != 2 {
		t.Fatalf("agent called %d times, want 2", len(calls))
	}
	if calls[0].Prompt != calls[1].Prompt {
		t.Error("re-running the node changed the prompt")
	}
}

// TestMalformedRawStillArchivesTheResponse: AgentResponse.Raw is opaque
// backend output and can be invalid JSON. A repair the run already paid for
// and already has on disk must not be thrown away over an evidence file.
func TestMalformedRawStillArchivesTheResponse(t *testing.T) {
	h := newHarness(t, failingState(0))
	h.agent.Responses = []belay.AgentResponse{{
		Text:      "repaired",
		SessionID: "sess-42",
		Usage:     belay.Usage{USD: 0.02},
		Raw:       json.RawMessage("this is not json"),
	}}

	res, err := fix.New().Run(context.Background(), h.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != journal.StatusOK || res.Next != graph.NodeTest {
		t.Errorf("Result = %+v, want a normal OK->test result", res)
	}
	if res.Usage.USD != 0.02 {
		t.Errorf("Usage = %+v, want the spend to still be reported", res.Usage)
	}

	raw, err := os.ReadFile(filepath.Join(h.nodeDir(t), "response.json")) //nolint:gosec // path built from the test's own Layout
	if err != nil {
		t.Fatalf("read response.json: %v", err)
	}
	var got belay.AgentResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("response.json is not valid JSON: %v\n%s", err, raw)
	}
	if got.Text != "repaired" {
		t.Errorf("response.json lost the text: %+v", got)
	}
	if !strings.Contains(string(raw), "not valid JSON and was dropped") {
		t.Errorf("response.json does not say that Raw was dropped:\n%s", raw)
	}
}

// TestUnwritableNodeDirFailsBeforeSpending: if the evidence trail cannot be
// written, the node fails before it calls the agent rather than spending
// money on an attempt nobody will be able to diagnose.
func TestUnwritableNodeDirFailsBeforeSpending(t *testing.T) {
	h := newHarness(t, failingState(0))
	nodesDir := h.rc.Layout.NodesDir()
	if err := os.MkdirAll(filepath.Dir(nodesDir), 0o750); err != nil {
		t.Fatalf("create run dir: %v", err)
	}
	// A regular file where the nodes/ directory belongs: MkdirAll fails.
	if err := os.WriteFile(nodesDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("create blocking file: %v", err)
	}

	_, err := fix.New().Run(context.Background(), h.rc)
	if err == nil {
		t.Fatal("Run returned nil error with an unwritable node directory")
	}
	if !strings.Contains(err.Error(), "prompt.txt") {
		t.Errorf("err = %v, want it to name the file it could not write", err)
	}
	if n := h.agent.CallCount(); n != 0 {
		t.Errorf("agent called %d times; the prompt is written before the call, not after", n)
	}
}

// The fix node must ask for edit tools explicitly.
//
// An empty AllowedTools means "the backend's default", and a headless agent's
// default is to ask permission before editing -- permission nobody can grant,
// because no one is at the other end. The agent then describes the repair
// instead of performing it, and the loop burns its whole give_up budget
// without changing a line. The first live run did exactly that: three
// attempts, a dollar spent, the same three findings at the end.
//
// Every unit test in this package passes with AllowedTools empty, because the
// fake agent has no permission model. Only this assertion stands between that
// bug and another live run.
func TestFixRequestsEditTools(t *testing.T) {
	t.Parallel()

	h := newHarness(t, failingState(0))
	if _, err := fix.New().Run(context.Background(), h.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call, ok := h.agent.LastCall()
	if !ok {
		t.Fatal("the agent was never called")
	}
	if len(call.AllowedTools) == 0 {
		t.Fatal("AllowedTools is empty: the backend default requires interactive " +
			"approval, so the fix loop cannot edit anything and give_up expires silently")
	}
	for _, want := range []string{"Edit", "Write"} {
		if !slices.Contains(call.AllowedTools, want) {
			t.Errorf("AllowedTools = %v, missing %q: a repair that cannot write is not a repair",
				call.AllowedTools, want)
		}
	}
	// A repair needs to read and search, but not to run commands: the test
	// node runs the suite, and a shell is where an agent reaches the network.
	if slices.Contains(call.AllowedTools, "Bash") {
		t.Errorf("AllowedTools = %v: Bash is deliberately withheld from a repair", call.AllowedTools)
	}
}
