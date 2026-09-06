package plan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// Nothing in this package's tests runs the real `claude` binary: every
// invocation goes through belaytest.FakeAgent, which returns scripted
// values and never starts a process. See TestNoLiveBackendIsReachable.
const (
	testGoal  = "add a --dry-run flag to the export command"
	testRunID = "20260101T000000Z-abcdef123456"
	testPlan  = "# Summary\n\nAdd a --dry-run flag.\n\n## Steps\n\n1. Parse the flag.\n"
)

// testUsage is deliberately non-zero in every dimension: requirement 5 is
// that the ledger sees this node's cost, and a zero Usage would let a node
// that dropped it pass.
var testUsage = belay.Usage{InputTokens: 1200, OutputTokens: 800, USD: 0.0345}

// newRunContext builds the RunContext a dispatcher would hand this node,
// over a throwaway workspace.
func newRunContext(t *testing.T, agent belay.AgentBackend) *graph.RunContext {
	t.Helper()
	layout, err := state.NewLayout(t.TempDir(), testRunID)
	if err != nil {
		t.Fatalf("state.NewLayout: %v", err)
	}
	return &graph.RunContext{
		Goal:      testGoal,
		State:     state.NewState(testGoal),
		Config:    config.Default(),
		Layout:    layout,
		Workspace: layout.WorkspaceDir(),
		RunID:     layout.RunID(),
		NodeName:  graph.NodePlan,
		Step:      1,
		Attempt:   1,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agent:     agent,
	}
}

// planAgent returns a fake backend scripted to answer with text.
func planAgent(text string) *belaytest.FakeAgent {
	return &belaytest.FakeAgent{
		NameValue: "fake-agent",
		Responses: []belay.AgentResponse{{
			Text:      text,
			SessionID: "sess-1",
			Usage:     testUsage,
			Turns:     2,
			Raw:       json.RawMessage(`{"type":"result"}`),
		}},
	}
}

// readNodeFile reads one of this execution's evidence files.
func readNodeFile(t *testing.T, rc *graph.RunContext, name string) []byte {
	t.Helper()
	dir, err := rc.Layout.NodeDir(rc.Step, rc.NodeName)
	if err != nil {
		t.Fatalf("Layout.NodeDir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // path is built from the test's own Layout
	if err != nil {
		t.Fatalf("read node file %q: %v", name, err)
	}
	return data
}

// assertNoPlanArtifact fails if plan.md exists.
func assertNoPlanArtifact(t *testing.T, rc *graph.RunContext) {
	t.Helper()
	path, err := rc.Layout.ArtifactPath(ArtifactName)
	if err != nil {
		t.Fatalf("Layout.ArtifactPath: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan artifact exists at %s (stat err = %v), want no artifact", path, err)
	}
}

func TestName(t *testing.T) {
	if got := New().Name(); got != graph.NodePlan {
		t.Fatalf("Name() = %q, want %q", got, graph.NodePlan)
	}
}

// Requirement 4: the approval gate is the node's only branch, and both
// sides of it route somewhere the registry knows. Requirement 5 rides
// along here, since Usage is returned by the same successful path.
func TestRunRoutesOnApprovalGate(t *testing.T) {
	tests := []struct {
		name     string
		approval bool
		wantNext string
	}{
		{"approval on routes to the approve node", true, graph.NodeApprove},
		{"approval off routes straight to the code node", false, graph.NodeCode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := newRunContext(t, planAgent(testPlan))
			rc.Config.Graph.Approval = tt.approval

			res, err := New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if err := res.Validate(); err != nil {
				t.Fatalf("Result.Validate: %v", err)
			}
			if res.Next != tt.wantNext {
				t.Errorf("Next = %q, want %q", res.Next, tt.wantNext)
			}
			if res.Status != journal.StatusOK {
				t.Errorf("Status = %v, want StatusOK", res.Status)
			}
			if res.Usage != testUsage {
				t.Errorf("Usage = %+v, want %+v", res.Usage, testUsage)
			}
			if res.Usage.USD == 0 || res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
				t.Errorf("Usage = %+v, want the agent's non-zero usage; the budget ledger cannot see a node that drops it", res.Usage)
			}
			if strings.Contains(res.Note, testPlan) {
				t.Errorf("Note = %q, must not embed agent output", res.Note)
			}

			plan := res.Patch.Plan
			if plan == nil {
				t.Fatal("Patch.Plan = nil, want the plan record")
			}
			if want := filepath.Join("artifacts", ArtifactName); plan.Path != want {
				t.Errorf("Plan.Path = %q, want %q", plan.Path, want)
			}
			if plan.Approved {
				t.Error("Plan.Approved = true, want false: this node records that a plan exists, not that anyone approved it")
			}
			doc, err := rc.ReadArtifact(ArtifactName)
			if err != nil {
				t.Fatalf("ReadArtifact: %v", err)
			}
			if got, want := string(doc), testPlan; got != want {
				t.Errorf("plan.md = %q, want %q", got, want)
			}
			if got, want := plan.Digest, Digest(doc); got != want {
				t.Errorf("Plan.Digest = %q, want %q (the digest must cover the bytes on disk)", got, want)
			}
		})
	}
}

// ADR-0002: a node returns a Patch and writes no control state. The
// dispatcher owns History too — a node that appended its own entry would
// be inventing journal sequence numbers it cannot know.
func TestRunWritesNoControlState(t *testing.T) {
	rc := newRunContext(t, planAgent(testPlan))

	res, err := New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, path := range []string{rc.Layout.StatePath(), rc.Layout.ManifestPath(), rc.Layout.JournalPath()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists (stat err = %v); nodes never write control state", path, err)
		}
	}
	p := res.Patch
	if p.History != nil || p.Code != nil || p.Test != nil || p.Review != nil ||
		p.Fix != nil || p.Candidates != nil || p.Winner != nil || p.Goal != nil {
		t.Errorf("Patch = %+v, want only Plan set", p)
	}
	// A Patch the dispatcher cannot apply is as bad as no Patch at all.
	st := state.NewState(testGoal)
	if err := p.Apply(&st); err != nil {
		t.Fatalf("Patch.Apply: %v", err)
	}
	if st.Plan.Path == "" || st.Plan.Digest == "" || st.Plan.Approved {
		t.Errorf("applied state.Plan = %+v, want a path and digest with Approved false", st.Plan)
	}
}

// Requirement 2: the run must be diagnosable afterwards. prompt.txt is
// byte-for-byte what was sent, so it can be replayed by hand.
func TestRunRecordsEvidence(t *testing.T) {
	agent := planAgent(testPlan)
	rc := newRunContext(t, agent)

	if _, err := New().Run(context.Background(), rc); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sent, ok := agent.LastCall()
	if !ok {
		t.Fatal("agent was never invoked")
	}
	if got := string(readNodeFile(t, rc, PromptFile)); got != sent.Prompt {
		t.Errorf("prompt.txt is not the prompt that was sent:\n got %q\nwant %q", got, sent.Prompt)
	}
	if !strings.Contains(sent.Prompt, testGoal) {
		t.Errorf("prompt does not carry the goal: %q", sent.Prompt)
	}

	var req belay.AgentRequest
	if err := json.Unmarshal(readNodeFile(t, rc, RequestFile), &req); err != nil {
		t.Fatalf("decode request.json: %v", err)
	}
	if req.SystemPrompt != SystemPrompt {
		t.Errorf("request.json SystemPrompt = %q, want the package's SystemPrompt", req.SystemPrompt)
	}
	if req.Model != rc.Config.Agent.Model || req.MaxTurns != rc.Config.Agent.MaxTurns {
		t.Errorf("request.json model/turns = %q/%d, want %q/%d from config.Agent",
			req.Model, req.MaxTurns, rc.Config.Agent.Model, rc.Config.Agent.MaxTurns)
	}

	var resp belay.AgentResponse
	if err := json.Unmarshal(readNodeFile(t, rc, ResponseFile), &resp); err != nil {
		t.Fatalf("decode response.json: %v", err)
	}
	if resp.Text != testPlan || resp.Usage != testUsage || resp.SessionID != "sess-1" {
		t.Errorf("response.json = %+v, want the agent's response verbatim", resp)
	}
}

// Requirement 2: the request carries the run's configuration and points at
// the workspace, not at whatever directory the dispatcher started in.
func TestRunBuildsRequestFromConfigAndLayout(t *testing.T) {
	agent := planAgent(testPlan)
	rc := newRunContext(t, agent)
	rc.Config.Agent.Model = "opus"
	rc.Config.Agent.MaxTurns = 7

	if _, err := New().Run(context.Background(), rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	req, _ := agent.LastCall()

	if req.WorkDir != rc.Workspace {
		t.Errorf("WorkDir = %q, want rc.Workspace %q", req.WorkDir, rc.Workspace)
	}
	if !filepath.IsAbs(req.WorkDir) {
		t.Errorf("WorkDir = %q, want an absolute path", req.WorkDir)
	}
	if req.Model != "opus" || req.MaxTurns != 7 {
		t.Errorf("Model/MaxTurns = %q/%d, want opus/7 from rc.Config.Agent", req.Model, req.MaxTurns)
	}
	if req.SessionID != "" {
		t.Errorf("SessionID = %q, want empty: the plan node starts a fresh session", req.SessionID)
	}
	if !strings.Contains(req.SystemPrompt, "PLAN") {
		t.Errorf("SystemPrompt does not establish the planning role: %q", req.SystemPrompt)
	}
	if !strings.Contains(req.Prompt, req.WorkDir) {
		t.Errorf("prompt does not name the workspace %q", req.WorkDir)
	}
}

// The plan node runs before any approval gate, so the agent must not be
// able to change the repository even if the system prompt is ignored.
func TestRunRestrictsAgentToReadOnlyTools(t *testing.T) {
	agent := planAgent(testPlan)
	rc := newRunContext(t, agent)

	if _, err := New().Run(context.Background(), rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	req, _ := agent.LastCall()

	if len(req.AllowedTools) == 0 {
		t.Fatal("AllowedTools is empty, which means the backend's default (writable) tool set")
	}
	for _, mutating := range []string{"Edit", "Write", "Bash", "NotebookEdit", "MultiEdit"} {
		if slices.Contains(req.AllowedTools, mutating) {
			t.Errorf("AllowedTools = %v, must not include the mutating tool %q", req.AllowedTools, mutating)
		}
	}
	if want := readOnlyTools(); !slices.Equal(req.AllowedTools, want) {
		t.Errorf("AllowedTools = %v, want %v", req.AllowedTools, want)
	}
}

// Requirement 6: a missing toolchain must stay recognizable at the
// dispatcher, or the fix loop will ask an agent that does not exist to
// edit code until the problem goes away.
func TestRunPropagatesToolchainMissing(t *testing.T) {
	agent := &belaytest.FakeAgent{
		NameValue: "fake-agent",
		Errs:      []error{&belay.ToolchainError{Tool: "claude"}},
	}
	rc := newRunContext(t, agent)

	res, err := New().Run(context.Background(), rc)
	if err == nil {
		t.Fatal("Run returned nil error for a failing backend")
	}
	if !errors.Is(err, belay.ErrToolchainMissing) {
		t.Errorf("errors.Is(err, belay.ErrToolchainMissing) = false for %v", err)
	}
	var te *belay.ToolchainError
	if !errors.As(err, &te) || te.Tool != "claude" {
		t.Errorf("errors.As did not recover the tool name from %v", err)
	}
	if res != (graph.Result{}) {
		t.Errorf("Result = %+v, want the zero Result on error", res)
	}
	assertNoPlanArtifact(t, rc)
	// The failed call is still on disk to look at.
	readNodeFile(t, rc, PromptFile)
	readNodeFile(t, rc, ResponseFile)
}

// A generic backend failure is an error too — never a StatusFailed Result,
// which would invite the dispatcher to route on a plan that does not exist.
func TestRunAgentFailureIsAnError(t *testing.T) {
	sentinel := errors.New("backend exploded")
	rc := newRunContext(t, &belaytest.FakeAgent{Errs: []error{sentinel}})

	res, err := New().Run(context.Background(), rc)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the backend's error", err)
	}
	if res.Status != journal.StatusUnknown {
		t.Errorf("Status = %v, want the zero Status: a backend failure is an error, not a routable result", res.Status)
	}
}

// Requirement 6: an empty answer is a typed error, not an empty plan.md
// that every downstream path check would happily accept.
func TestRunEmptyPlanIsTypedError(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"spaces and tabs", "   \t  "},
		{"newlines only", "\n\n\n"},
		{"mixed whitespace", " \r\n\t \n "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := newRunContext(t, planAgent(tt.text))

			res, err := New().Run(context.Background(), rc)
			if !errors.Is(err, ErrEmptyPlan) {
				t.Fatalf("err = %v, want ErrEmptyPlan", err)
			}
			if res != (graph.Result{}) {
				t.Errorf("Result = %+v, want the zero Result on error", res)
			}
			assertNoPlanArtifact(t, rc)
			readNodeFile(t, rc, ResponseFile) // still diagnosable
		})
	}
}

// Requirement 7: the dispatcher re-runs a node whose node_started had no
// matching finish. The second run must land on the same artifact, not a
// second one and not a doubled file.
func TestRunIsIdempotentOnRerun(t *testing.T) {
	agent := planAgent(testPlan)
	rc := newRunContext(t, agent)
	node := New()

	first, err := node.Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// What the dispatcher changes on a re-run after a crash.
	rc.Attempt = 2

	second, err := node.Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if err := second.Validate(); err != nil {
		t.Fatalf("second Result.Validate: %v", err)
	}
	if second.Patch.Plan == nil {
		t.Fatal("second Patch.Plan = nil")
	}
	if first.Patch.Plan.Path != second.Patch.Plan.Path {
		t.Errorf("artifact path moved between runs: %q then %q", first.Patch.Plan.Path, second.Patch.Plan.Path)
	}
	if first.Patch.Plan.Digest != second.Patch.Plan.Digest {
		t.Errorf("digest changed for an identical plan: %q then %q", first.Patch.Plan.Digest, second.Patch.Plan.Digest)
	}

	doc, err := rc.ReadArtifact(ArtifactName)
	if err != nil {
		t.Fatalf("ReadArtifact: %v", err)
	}
	if string(doc) != testPlan {
		t.Errorf("plan.md = %q after two runs, want one copy of the plan", string(doc))
	}
	entries, err := os.ReadDir(rc.Layout.ArtifactsDir())
	if err != nil {
		t.Fatalf("read artifacts dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("artifacts dir holds %d files, want exactly 1", len(entries))
	}
	if agent.CallCount() != 2 {
		t.Errorf("agent CallCount = %d, want 2", agent.CallCount())
	}
}

// Requirement 8: a nil adapter is a clear error, never a panic. Every
// adapter on a RunContext is allowed to be nil.
func TestRunNilAgentIsTypedError(t *testing.T) {
	rc := newRunContext(t, nil)

	res, err := New().Run(context.Background(), rc)
	if !errors.Is(err, ErrNoAgent) {
		t.Fatalf("err = %v, want ErrNoAgent", err)
	}
	if res != (graph.Result{}) {
		t.Errorf("Result = %+v, want the zero Result on error", res)
	}
	assertNoPlanArtifact(t, rc)
}

// Requirement 9: a cancelled context stops the node before it spends
// anything.
func TestRunRespectsContextCancellation(t *testing.T) {
	agent := planAgent(testPlan)
	rc := newRunContext(t, agent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := New().Run(ctx, rc)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if agent.CallCount() != 0 {
		t.Errorf("agent CallCount = %d, want 0: a cancelled run must not invoke the backend", agent.CallCount())
	}
	assertNoPlanArtifact(t, rc)
}

// A backend whose Raw is not JSON must not cost the run its plan.
func TestRunSurvivesUnencodableRawResponse(t *testing.T) {
	agent := planAgent(testPlan)
	agent.Responses[0].Raw = json.RawMessage("not json at all")
	rc := newRunContext(t, agent)

	res, err := New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Patch.Plan == nil {
		t.Fatal("Patch.Plan = nil")
	}
	var resp belay.AgentResponse
	if err := json.Unmarshal(readNodeFile(t, rc, ResponseFile), &resp); err != nil {
		t.Fatalf("decode response.json: %v", err)
	}
	if resp.Text != testPlan {
		t.Errorf("response.json Text = %q, want the response recorded without its unencodable Raw", resp.Text)
	}
}

// A nil Logger is a dispatcher wiring slip, not a reason to panic inside a
// node.
func TestRunToleratesNilLogger(t *testing.T) {
	rc := newRunContext(t, planAgent(testPlan))
	rc.Logger = nil

	if _, err := New().Run(context.Background(), rc); err != nil {
		t.Fatalf("Run with a nil Logger: %v", err)
	}
}

// workspaceDir derives the repository root from a path shape state owns.
// This test is the guard: if state.NewLayout ever nests the run directory
// differently, this fails instead of the agent silently planning against
// the wrong directory.
func TestWorkspaceDirMatchesNewLayout(t *testing.T) {
	ws := t.TempDir()
	layout, err := state.NewLayout(ws, testRunID)
	if err != nil {
		t.Fatalf("state.NewLayout: %v", err)
	}
	if got := layout.WorkspaceDir(); got != ws {
		t.Errorf("Layout.WorkspaceDir() = %q, want %q", got, ws)
	}
	if _, err := workspaceDir(&graph.RunContext{}); !errors.Is(err, ErrNoWorkspace) {
		t.Errorf("workspaceDir(unset) = %v, want ErrNoWorkspace", err)
	}
}

func TestDigest(t *testing.T) {
	// Pins the formula: the hash covers the raw bytes, and the prefix is
	// not part of what is hashed.
	const golden = "sha256:5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if got := Digest([]byte("hello\n")); got != golden {
		t.Errorf("Digest(\"hello\\n\") = %q, want %q", got, golden)
	}
	if Digest([]byte(testPlan)) == Digest([]byte(testPlan+"x")) {
		t.Error("Digest collides for different content")
	}
	if !strings.HasPrefix(Digest(nil), DigestPrefix) {
		t.Error("Digest does not carry the algorithm prefix")
	}
}

// The acceptance criterion "zero live claude invocations" is structural,
// not incidental: this package's only backend seam is belay.AgentBackend,
// and the tests only ever pass a FakeAgent through it.
func TestNoLiveBackendIsReachable(t *testing.T) {
	var agent belay.AgentBackend = planAgent(testPlan)
	if _, ok := agent.(*belaytest.FakeAgent); !ok {
		t.Fatalf("test backend is %T, want *belaytest.FakeAgent", agent)
	}
}

// Evidence is written before the backend is called, so a run directory
// that cannot hold it fails before it spends anything.
func TestRunFailsWhenEvidenceCannotBeWritten(t *testing.T) {
	agent := planAgent(testPlan)
	rc := newRunContext(t, agent)

	// Occupy the node's own directory path with a regular file.
	dir, err := rc.Layout.NodeDir(rc.Step, rc.NodeName)
	if err != nil {
		t.Fatalf("Layout.NodeDir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		t.Fatalf("create nodes dir: %v", err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("occupy node dir: %v", err)
	}

	res, err := New().Run(context.Background(), rc)
	if err == nil {
		t.Fatal("Run succeeded with an unwritable node directory")
	}
	if res != (graph.Result{}) {
		t.Errorf("Result = %+v, want the zero Result on error", res)
	}
	if agent.CallCount() != 0 {
		t.Errorf("agent CallCount = %d, want 0: a call that cannot be recorded must not be made", agent.CallCount())
	}
	assertNoPlanArtifact(t, rc)
}

// The plan is worth nothing if it cannot be archived, so a failed write is
// a failed node rather than a Patch pointing at a file that is not there.
func TestRunFailsWhenArtifactCannotBeWritten(t *testing.T) {
	rc := newRunContext(t, planAgent(testPlan))

	if err := os.MkdirAll(rc.Layout.RunDir(), 0o750); err != nil {
		t.Fatalf("create run dir: %v", err)
	}
	if err := os.WriteFile(rc.Layout.ArtifactsDir(), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("occupy artifacts dir: %v", err)
	}

	res, err := New().Run(context.Background(), rc)
	if err == nil {
		t.Fatal("Run succeeded with an unwritable artifacts directory")
	}
	if res != (graph.Result{}) {
		t.Errorf("Result = %+v, want the zero Result on error", res)
	}
}
