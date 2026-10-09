//go:build fixrate

package fixrate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/nodes/code"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// TestControlAllowance_FromShippedDefaults pins the derivation against
// belay's own shipped configuration, so that changing a default which
// widens what belay's graph may spend on an agent, without widening what
// the control arm gets, fails here instead of silently producing a
// comparison the control arm was starved in.
func TestControlAllowance_FromShippedDefaults(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	// Restate the inputs so a reader of a failure can see which default
	// moved without opening internal/config.
	if cfg.Agent.MaxTurns != 30 || cfg.Graph.GiveUp != 3 || cfg.Graph.NodeTimeout.Duration() != 15*time.Minute {
		t.Fatalf("shipped defaults moved: agent.max_turns=%d graph.give_up=%d graph.node_timeout=%s "+
			"(this test's expectations below are derived from 30/3/15m)",
			cfg.Agent.MaxTurns, cfg.Graph.GiveUp, cfg.Graph.NodeTimeout.Duration())
	}

	got := controlAllowance(cfg)
	// plan (1) + code (1) + fix (give_up) = 5 agent-invoking node
	// executions, each capped at agent.max_turns turns and
	// graph.node_timeout wall clock.
	if want := 5; got.AgentCalls != want {
		t.Errorf("controlAllowance().AgentCalls = %d, want %d (plan + code + give_up fixes)", got.AgentCalls, want)
	}
	if want := 150; got.MaxTurns != want {
		t.Errorf("controlAllowance().MaxTurns = %d, want %d (agent.max_turns * 5)", got.MaxTurns, want)
	}
	if want := 75 * time.Minute; got.Timeout != want {
		t.Errorf("controlAllowance().Timeout = %s, want %s (graph.node_timeout * 5)", got.Timeout, want)
	}
}

func TestControlAllowance_ScalesWithConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		maxTurns       int
		giveUp         int
		nodeTimeout    time.Duration
		wantAgentCalls int
		wantMaxTurns   int
		wantTimeout    time.Duration
	}{
		{"one fix attempt", 10, 1, time.Minute, 3, 30, 3 * time.Minute},
		{"ten fix attempts", 2, 10, 2 * time.Second, 12, 24, 24 * time.Second},
		// A zero node timeout is how belay spells "no per-node ceiling"
		// (see the dispatcher's timeout > 0 guard), and the control arm
		// must inherit the absence rather than multiply it into one.
		{"no node timeout", 7, 2, 0, 4, 28, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.Agent.MaxTurns = tt.maxTurns
			cfg.Graph.GiveUp = tt.giveUp
			cfg.Graph.NodeTimeout = config.NewDuration(tt.nodeTimeout)

			got := controlAllowance(cfg)
			if got.AgentCalls != tt.wantAgentCalls {
				t.Errorf("AgentCalls = %d, want %d", got.AgentCalls, tt.wantAgentCalls)
			}
			if got.MaxTurns != tt.wantMaxTurns {
				t.Errorf("MaxTurns = %d, want %d", got.MaxTurns, tt.wantMaxTurns)
			}
			if got.Timeout != tt.wantTimeout {
				t.Errorf("Timeout = %s, want %s", got.Timeout, tt.wantTimeout)
			}
		})
	}
}

// TestRunControl_MakesExactlyOneCall is the control arm's defining
// property: one raw AgentBackend invocation per task, with no graph, no
// fix loop and no review gate around it.
func TestRunControl_MakesExactlyOneCall(t *testing.T) {
	t.Parallel()

	agent := &belaytest.FakeAgent{
		NameValue: "fixrate-control-fake",
		Responses: []belay.AgentResponse{{
			Text:  "edited the file",
			Turns: 4,
			Usage: belay.Usage{InputTokens: 100, OutputTokens: 20, USD: 0.25},
		}},
	}
	workspace := t.TempDir()
	cfg := belayConfig()

	got, err := runControl(t.Context(), workspace, belayGoal, agent, cfg)
	if err != nil {
		t.Fatalf("runControl() error = %v", err)
	}

	if agent.CallCount() != 1 {
		t.Fatalf("runControl() made %d agent call(s), want exactly 1 — the control arm has no fix loop", agent.CallCount())
	}
	req := agent.Calls()[0]

	allow := controlAllowance(cfg)
	switch {
	case req.WorkDir != workspace:
		t.Errorf("WorkDir = %q, want the task workspace %q", req.WorkDir, workspace)
	case req.MaxTurns != allow.MaxTurns:
		t.Errorf("MaxTurns = %d, want the whole graph's allowance %d", req.MaxTurns, allow.MaxTurns)
	case req.Model != cfg.Agent.ModelFor(graph.NodeCode):
		t.Errorf("Model = %q, want the code node's pinned model %q", req.Model, cfg.Agent.ModelFor(graph.NodeCode))
	case req.SessionID != "":
		t.Errorf("SessionID = %q, want empty — one call is not a continued conversation", req.SessionID)
	}
	if !strings.Contains(req.Prompt, belayGoal) {
		t.Errorf("Prompt = %q, want it to carry the belay arm's goal text verbatim (%q)", req.Prompt, belayGoal)
	}

	if got.Usage.USD != 0.25 {
		t.Errorf("Usage.USD = %v, want the backend's reported 0.25", got.Usage.USD)
	}
	if got.Turns != 4 {
		t.Errorf("Turns = %d, want the backend's reported 4", got.Turns)
	}
	if got.Model != cfg.Agent.ModelFor(graph.NodeCode) {
		t.Errorf("Model = %q, want %q", got.Model, cfg.Agent.ModelFor(graph.NodeCode))
	}
	if got.Allowance != allow {
		t.Errorf("Allowance = %+v, want %+v", got.Allowance, allow)
	}
	if got.RunErr != "" {
		t.Errorf("RunErr = %q, want empty", got.RunErr)
	}
}

// TestRunControl_CountsSpendOnAFailedCall is the cost-accounting property
// that makes USD per resolved task trustworthy: whatever the backend
// reports having spent, it has spent, even on the call that returned an
// error. A control arm whose failures were free would flatter its own
// cost per fix.
func TestRunControl_CountsSpendOnAFailedCall(t *testing.T) {
	t.Parallel()

	agent := &belaytest.FakeAgent{
		Responses: []belay.AgentResponse{{Text: "partial work", Turns: 9, Usage: belay.Usage{USD: 1.5, Estimated: true}}},
		Errs:      []error{errors.New("backend hit its own ceiling")},
	}
	got, err := runControl(t.Context(), t.TempDir(), belayGoal, agent, belayConfig())
	if err != nil {
		t.Fatalf("runControl() error = %v; a failed agent call is an outcome to record, not a harness failure", err)
	}
	if got.Usage.USD != 1.5 {
		t.Errorf("Usage.USD = %v, want the 1.5 the backend reported alongside its error", got.Usage.USD)
	}
	if !got.Usage.Estimated {
		t.Error("Usage.Estimated = false, want the backend's own true carried through")
	}
	if got.RunErr == "" {
		t.Error("RunErr = \"\", want the backend's error message recorded")
	}
}

func TestRunControl_Validation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		workspace string
		goal      string
		agent     belay.AgentBackend
		wantErr   error
	}{
		{"no agent", "/tmp", belayGoal, nil, ErrNoAgent},
		{"no goal", "/tmp", "", &belaytest.FakeAgent{}, ErrNoGoal},
		{"no workspace", "", belayGoal, &belaytest.FakeAgent{}, ErrNoWorkspace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := runControl(t.Context(), tt.workspace, tt.goal, tt.agent, belayConfig())
			if err == nil {
				t.Fatal("runControl() = nil error, want one")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("runControl() error = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

// TestControlTools_MatchesTheCodeNode is the drift guard behind the claim
// "the same tools belay's code node uses". internal/nodes/code's tool
// allow-list is unexported, so this package cannot import it and must
// replicate the spellings — which means nothing would notice the code node
// gaining or losing a tool. So rather than compare against a copy, this
// drives the REAL code node once and reads the AllowedTools it actually
// sent off a recording backend.
//
// The node is expected to fail after the invocation (its reply carries no
// changed-files block), which is irrelevant here: the AgentRequest is
// recorded by then, and the request is the whole subject.
func TestControlTools_MatchesTheCodeNode(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	layout, err := state.NewLayout(workspace, "control-tools-guard")
	if err != nil {
		t.Fatalf("state.NewLayout() error = %v", err)
	}
	if err := os.MkdirAll(layout.ArtifactsDir(), 0o750); err != nil {
		t.Fatalf("creating the run directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layout.ArtifactsDir(), "plan.md"), []byte("Fix the defect."), 0o600); err != nil {
		t.Fatalf("writing the plan: %v", err)
	}

	cfg := belayConfig()
	agent := &belaytest.FakeAgent{Responses: []belay.AgentResponse{{Text: "done"}}}
	rc := &graph.RunContext{
		Goal:      belayGoal,
		State:     state.State{Plan: state.Plan{Path: filepath.Join("artifacts", "plan.md"), Approved: true}},
		Config:    cfg,
		Layout:    layout,
		Workspace: workspace,
		RunID:     "control-tools-guard",
		NodeName:  graph.NodeCode,
		Step:      1,
		Attempt:   1,
		Agent:     agent,
	}
	// Whatever the node concluded is irrelevant to this guard, but it is
	// logged rather than discarded so a surprise is visible under -v.
	if _, err := code.New().Run(t.Context(), rc); err != nil {
		t.Logf("code node returned %v after the invocation (expected to be irrelevant here)", err)
	}

	if agent.CallCount() != 1 {
		t.Fatalf("the code node made %d agent call(s), want 1; this guard cannot read its tool set", agent.CallCount())
	}
	sent := agent.Calls()[0]
	if !slices.Equal(controlTools(), sent.AllowedTools) {
		t.Errorf("controlTools() = %v but belay's code node sends %v; the control arm must offer the same tools",
			controlTools(), sent.AllowedTools)
	}
	if sent.MaxTurns != cfg.Agent.MaxTurns {
		t.Errorf("the code node sent MaxTurns = %d, want agent.max_turns = %d; controlAllowance's derivation assumes this",
			sent.MaxTurns, cfg.Agent.MaxTurns)
	}
	if sent.Model != cfg.Agent.ModelFor(graph.NodeCode) {
		t.Errorf("the code node sent Model = %q, want %q", sent.Model, cfg.Agent.ModelFor(graph.NodeCode))
	}
}

// TestArm_Provenance_IsUnambiguous is the arm-label counterpart of
// TestMode_Provenance_IsUnambiguous: the text is a contract, not prose.
// A reader who sees only this line must be able to tell whether belay's
// graph was involved, and must not be able to mistake the control arm for
// a different backend or a different model — which is the single most
// likely misreading of a published delta.
//
// Guard, not RED: Arm.provenance was written with the arm constants in the
// same step, so this pins its wording rather than driving it out.
func TestArm_Provenance_IsUnambiguous(t *testing.T) {
	t.Parallel()

	belayText := ArmBelay.provenance()
	controlText := ArmControl.provenance()

	for _, want := range []string{"belay", "fix loop"} {
		if !strings.Contains(strings.ToLower(belayText), want) {
			t.Errorf("ArmBelay.provenance() = %q, want it to mention %q", belayText, want)
		}
	}
	for _, want := range []string{"same backend", "no graph", "no fix loop", "same model"} {
		if !strings.Contains(strings.ToLower(controlText), want) {
			t.Errorf("ArmControl.provenance() = %q, want it to mention %q", controlText, want)
		}
	}
	// The control arm is not a different backend, and a reader must not
	// be able to take it for one.
	if !strings.Contains(controlText, "not a different backend") {
		t.Errorf("ArmControl.provenance() = %q, want it to rule out being read as a different backend", controlText)
	}
	if strings.Contains(strings.ToLower(belayText), "no graph") {
		t.Errorf("ArmBelay.provenance() = %q, must not describe itself as having no graph", belayText)
	}
	if got := Arm("bogus").provenance(); !strings.Contains(got, "cannot be vouched for") {
		t.Errorf(`Arm("bogus").provenance() = %q, want it to refuse to vouch for the number`, got)
	}
}

// TestAllowance_RoundTripsThroughJSON: a corpus keeps a control cell's
// allowance on disk and reads it back on resume, so what MarshalJSON writes
// -- a human-readable duration -- must read back as the same Allowance.
func TestAllowance_RoundTripsThroughJSON(t *testing.T) {
	t.Parallel()

	for _, want := range []Allowance{
		controlAllowance(config.Default()),
		{MaxTurns: 30, Timeout: 0, AgentCalls: 1},
	} {
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal(%+v): %v", want, err)
		}
		var got Allowance
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", data, err)
		}
		if got != want {
			t.Errorf("round trip of %+v through %s = %+v", want, data, got)
		}
	}
	var bad Allowance
	if err := json.Unmarshal([]byte(`{"max_turns":1,"timeout":"soon","agent_calls":1}`), &bad); err == nil {
		t.Errorf("Unmarshal of an unreadable timeout = %+v, nil error; want an error", bad)
	}
}
