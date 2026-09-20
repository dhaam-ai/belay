package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/nodes/plan"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

const regenPlanBody = "# Plan\n\n## Steps\n\n1. Write the test.\n2. Make it pass.\n"

// pausedAtTheGate seeds a run whose plan is written and waiting, the way a
// run that has just paused at the approve gate looks on disk.
func pausedAtTheGate(t *testing.T, body string) *state.Store {
	t.Helper()
	ws := goWorkspace(t)
	p, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	store := seedRunDir(t, p, "add tests")

	if err := state.ReplaceArtifact(store.Layout(), plan.ArtifactName, []byte(body)); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st.Plan.Path = plan.ArtifactName
	st.Plan.Digest = approve.Digest([]byte(body))
	if err := store.SaveState(st); err != nil {
		t.Fatalf("save state: %v", err)
	}
	return store
}

func loadPlanState(t *testing.T, store *state.Store) state.Plan {
	t.Helper()
	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return st.Plan
}

// TestRequestRegenerationRecordsTheRequestAndKeepsThePlanItReplaces covers
// what asking for a new plan writes while the run is paused: the request
// itself, the withdrawal of any approval, and a copy of the plan that is
// about to be overwritten -- because the person asking has read that plan
// and may want it back.
func TestRequestRegenerationRecordsTheRequestAndKeepsThePlanItReplaces(t *testing.T) {
	store := pausedAtTheGate(t, regenPlanBody)
	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st.Plan.Approved = true
	if err := store.SaveState(st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	if err := requestRegeneration(store, "fewer steps, and leave the parser alone"); err != nil {
		t.Fatalf("requestRegeneration: %v", err)
	}

	got := loadPlanState(t, store)
	want := state.Regeneration{Request: 1, Guidance: "fewer steps, and leave the parser alone"}
	if got.Regenerate != want {
		t.Errorf("Plan.Regenerate = %+v, want %+v", got.Regenerate, want)
	}
	if got.Approved {
		t.Error("the plan is still approved, so resuming would write code against the plan the person just rejected")
	}
	snapshot := filepath.Join(store.Layout().ArtifactsDir(), plan.ReplacedArtifact(1))
	data, err := os.ReadFile(snapshot) //nolint:gosec // a path inside this test's own t.TempDir()
	if err != nil {
		t.Fatalf("the plan being replaced was not kept: %v", err)
	}
	if string(data) != regenPlanBody {
		t.Errorf("snapshot holds %q, want the plan that was on disk", data)
	}
	// plan.md is untouched: the plan node replaces it, and only once it has
	// something to replace it with.
	live, err := os.ReadFile(filepath.Join(store.Layout().ArtifactsDir(), plan.ArtifactName)) //nolint:gosec // as above
	if err != nil {
		t.Fatalf("read plan.md: %v", err)
	}
	if string(live) != regenPlanBody {
		t.Errorf("plan.md was changed before a new plan existed: %q", live)
	}
}

// TestRequestRegenerationIsRetrySafe: the command may be run twice, or run
// again after a crash between writing the snapshot and saving the state.
// One outstanding request is one outstanding request, whatever the person
// last said it should say -- a second number would file a second snapshot
// of a plan that has not changed and leave the first request unanswerable.
func TestRequestRegenerationIsRetrySafe(t *testing.T) {
	store := pausedAtTheGate(t, regenPlanBody)

	if err := requestRegeneration(store, "fewer steps"); err != nil {
		t.Fatalf("first: %v", err)
	}
	first := loadPlanState(t, store)
	if err := requestRegeneration(store, "fewer steps"); err != nil {
		t.Fatalf("again with the same words: %v", err)
	}
	if got := loadPlanState(t, store); got != first {
		t.Errorf("asking twice changed the request:\n got %+v\nwant %+v", got, first)
	}

	if err := requestRegeneration(store, "actually, leave the parser alone"); err != nil {
		t.Fatalf("again with different words: %v", err)
	}
	got := loadPlanState(t, store)
	if got.Regenerate.Request != 1 {
		t.Errorf("Request = %d, want 1: nothing has answered the first request yet", got.Regenerate.Request)
	}
	if got.Regenerate.Guidance != "actually, leave the parser alone" {
		t.Errorf("Guidance = %q, want the words the person typed last", got.Regenerate.Guidance)
	}

	// Once a request has been served, asking again is a new request, with
	// its own snapshot of the plan that answered the last one.
	served := got
	served.Regenerate.Served = served.Regenerate.Request
	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st.SetRegeneration(served.Regenerate)
	if err := store.SaveState(st); err != nil {
		t.Fatalf("save state: %v", err)
	}
	if err := requestRegeneration(store, "shorter still"); err != nil {
		t.Fatalf("after the first was served: %v", err)
	}
	if n := loadPlanState(t, store).Regenerate.Request; n != 2 {
		t.Errorf("Request = %d, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(store.Layout().ArtifactsDir(), plan.ReplacedArtifact(2))); err != nil {
		t.Errorf("the second request kept no snapshot: %v", err)
	}
}

// A request for a new plan is a control field, so the run declares the
// capability while it holds one (ADR-0011): an older belay must refuse the
// run rather than drop the request and spend the budget on the plan the
// person asked to replace.
func TestRequestRegenerationDeclaresItsCapability(t *testing.T) {
	store := pausedAtTheGate(t, regenPlanBody)
	if err := requestRegeneration(store, "fewer steps"); err != nil {
		t.Fatalf("requestRegeneration: %v", err)
	}

	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !contains(st.Requires, state.CapabilityPlanRegenerate) {
		t.Errorf("Requires = %v, want it to name %q", st.Requires, state.CapabilityPlanRegenerate)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestApprovingCancelsAPendingRegeneration: `belay resume` means "the plan
// as it stands is approved". A request left over from a command that was
// interrupted before the plan node could answer it would turn that into a
// second agent call, spending money on a plan the person has just said
// they no longer want.
func TestApprovingCancelsAPendingRegeneration(t *testing.T) {
	store := pausedAtTheGate(t, regenPlanBody)
	if err := requestRegeneration(store, "fewer steps"); err != nil {
		t.Fatalf("requestRegeneration: %v", err)
	}

	done, err := approvePlan(store)
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !done.changed() {
		t.Error("approvePlan reported nothing to do although a request was outstanding")
	}
	if !done.withdrew {
		t.Error("approvePlan did not report that it withdrew the request, so nobody would be told")
	}

	got := loadPlanState(t, store)
	if !got.Approved {
		t.Error("the plan was not approved")
	}
	if got.Regenerate.Pending() {
		t.Errorf("the request is still outstanding: %+v", got.Regenerate)
	}
	if got.Regenerate.Request != 1 {
		t.Errorf("Request = %d, want the request to stay on the record as something that was asked", got.Regenerate.Request)
	}
	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if contains(st.Requires, state.CapabilityPlanRegenerate) {
		t.Errorf("Requires still names %q although nothing depends on it now: %v",
			state.CapabilityPlanRegenerate, st.Requires)
	}

	again, err := approvePlan(store)
	if err != nil {
		t.Fatalf("approvePlan (second): %v", err)
	}
	if again.changed() {
		t.Error("approving twice reported a second change")
	}
}

// An approved plan with a request nobody has served is still a change to
// record: the request has to go, or resuming regenerates instead of
// approving.
func TestApprovingCancelsARequestOnAnAlreadyApprovedPlan(t *testing.T) {
	store := pausedAtTheGate(t, regenPlanBody)
	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st.Plan.Approved = true
	st.SetRegeneration(state.Regeneration{Request: 1, Guidance: "fewer steps"})
	if err := store.SaveState(st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	done, err := approvePlan(store)
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !done.changed() {
		t.Error("approvePlan reported nothing to do although a request was outstanding")
	}
	if !done.withdrew {
		t.Error("approvePlan did not report that it withdrew the request, so nobody would be told")
	}
	if got := loadPlanState(t, store); got.Regenerate.Pending() {
		t.Errorf("the request survived the approval: %+v", got.Regenerate)
	}
}

// TestPlanRegenerateNeedsGuidanceAndARun covers the command's refusals,
// which a person meets far more often than the happy path: an empty
// guidance string, and a run that is not waiting at the gate.
func TestPlanRegenerateRefusesWhatItCannotDo(t *testing.T) {
	store := pausedAtTheGate(t, regenPlanBody)
	ws := store.Layout().WorkspaceDir()

	_, _, err := runCLI(t, "--workspace", ws, "plan", "--regenerate", "   ", store.Layout().RunID())
	if err == nil {
		t.Error("belay plan --regenerate with no guidance was accepted")
	} else if !strings.Contains(err.Error(), "what") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// regenAgent is a backend scripted to answer each plan request in turn.
func regenAgent(plans ...string) *belaytest.FakeAgent {
	responses := make([]belay.AgentResponse, len(plans))
	for i, body := range plans {
		responses[i] = belay.AgentResponse{Text: body, Usage: belay.Usage{USD: 0.01}, Turns: 1}
	}
	return &belaytest.FakeAgent{NameValue: "fake-agent", Responses: responses}
}

// regenPlanTest wires a run whose graph is the real plan node, the real
// approve gate, and a node standing in for everything after them.
func regenPlanTest(t *testing.T, agent belay.AgentBackend, reached *bool) (*runPlan, *state.Store) {
	t.Helper()
	p, err := resolve(resolveRequest{workspace: goWorkspace(t), logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	p.agent = agent
	registry := graph.NewRegistry()
	registry.MustRegister(plan.New(), approve.New(), endsTheRun{reached: reached})
	p.registry = registry
	return p, seedRunDir(t, p, "add tests")
}

// journalNodes lists the nodes a run finished, in order.
func journalNodes(t *testing.T, store *state.Store) []string {
	t.Helper()
	read, err := journal.ReadFile(store.Layout().JournalPath())
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var nodes []string
	for _, rec := range read.Records {
		if rec.Event == journal.EventNodeFinished {
			nodes = append(nodes, rec.Node)
		}
	}
	return nodes
}

// TestRegenerationIsAJournalledEdge drives the whole path a person takes:
// the run stops at the plan, they ask for a different one, and the plan
// node writes it. What is being pinned is that this happens inside the
// dispatcher -- as an edge from the gate back to the plan node, journalled
// and charged like any other step -- and not as a side effect of a CLI
// command calling an agent while the run sits paused.
func TestRegenerationIsAJournalledEdge(t *testing.T) {
	const (
		firstPlan  = "# Plan\n\n## Steps\n\n1. Rewrite the parser.\n2. Rewrite the tests.\n"
		secondPlan = "# Plan\n\n## Steps\n\n1. Add one test.\n"
		guidance   = "fewer steps, and leave the parser alone"
	)
	reachedCode := false
	agent := regenAgent(firstPlan, secondPlan)
	p, store := regenPlanTest(t, agent, &reachedCode)

	dispatcher, err := graph.NewDispatcher(p.options(store))
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	outcome, err := dispatcher.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Status != state.RunStatusPaused || outcome.Node != graph.NodeApprove {
		t.Fatalf("outcome = %+v, want a pause at the approval gate", outcome)
	}

	// What `belay plan --regenerate` records, and then what it dispatches.
	if err := requestRegeneration(store, guidance); err != nil {
		t.Fatalf("requestRegeneration: %v", err)
	}
	again, err := dispatcher.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if again.Status != state.RunStatusPaused || again.Node != graph.NodeApprove {
		t.Fatalf("outcome = %+v, want a pause at the gate with the new plan", again)
	}
	if reachedCode {
		t.Fatal("the run went past the gate; nobody has read the new plan")
	}

	// plan, approve (paused) — then, on the resume: the gate again, which
	// is the node the run was paused on and the node that routes back;
	// plan, writing the new one; and the gate once more, pausing on it.
	want := []string{graph.NodePlan, graph.NodeApprove, graph.NodeApprove, graph.NodePlan, graph.NodeApprove}
	if got := journalNodes(t, store); !slices.Equal(got, want) {
		t.Errorf("the journal records %v, want %v: the new plan is a step of the run, not a side effect", got, want)
	}
	calls := agent.Calls()
	if len(calls) != 2 {
		t.Fatalf("the agent was called %d time(s), want 2", len(calls))
	}
	if strings.Contains(calls[0].Prompt, guidance) {
		t.Error("the first plan was asked for with guidance nobody had given yet")
	}
	if !strings.Contains(calls[1].Prompt, guidance) {
		t.Errorf("the second request does not carry what the person asked for:\n%s", calls[1].Prompt)
	}

	artifacts := store.Layout().ArtifactsDir()
	if got := readFile(t, filepath.Join(artifacts, plan.ArtifactName)); got != secondPlan {
		t.Errorf("plan.md holds %q, want the new plan", got)
	}
	if got := readFile(t, filepath.Join(artifacts, plan.ReplacedArtifact(1))); got != firstPlan {
		t.Errorf("the replaced plan was not kept: %q", got)
	}

	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if st.Plan.Regenerate.Pending() {
		t.Errorf("the request is still outstanding after being answered: %+v", st.Plan.Regenerate)
	}
	if got, wanted := st.Plan.Regenerate, (state.Regeneration{Request: 1, Guidance: guidance, Served: 1}); got != wanted {
		t.Errorf("Plan.Regenerate = %+v, want %+v", got, wanted)
	}
	if contains(st.Requires, state.CapabilityPlanRegenerate) {
		t.Errorf("Requires still names %q once the request is answered: %v", state.CapabilityPlanRegenerate, st.Requires)
	}
	if st.Plan.Approved {
		t.Error("the new plan arrived approved; nobody has read it")
	}

	// And the ordinary path still ends the run: approve, resume, done.
	if _, err := approvePlan(store); err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	final, err := dispatcher.Resume(context.Background())
	if err != nil {
		t.Fatalf("final Resume: %v", err)
	}
	if !reachedCode || final.Status != state.RunStatusCompleted {
		t.Errorf("outcome = %+v (reached code: %v), want a completed run", final, reachedCode)
	}
}

// TestRegenerationResumesAfterACrashOnTheEdge kills the run in the one
// window the edge creates: the gate has already journalled its route back
// to the plan node, and the plan node has started and not finished. The
// dispatcher re-runs such a node, so the request must still be there to
// answer -- and be answered once, not twice.
func TestRegenerationResumesAfterACrashOnTheEdge(t *testing.T) {
	const (
		firstPlan  = "# Plan\n\n## Steps\n\n1. Rewrite the parser.\n"
		secondPlan = "# Plan\n\n## Steps\n\n1. Add one test.\n"
		guidance   = "fewer steps, and leave the parser alone"
	)
	reachedCode := false
	agent := regenAgent(firstPlan, secondPlan)
	p, store := regenPlanTest(t, agent, &reachedCode)

	dispatcher, err := graph.NewDispatcher(p.options(store))
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if _, err := dispatcher.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := requestRegeneration(store, guidance); err != nil {
		t.Fatalf("requestRegeneration: %v", err)
	}

	// Ctrl-C lands while the plan node is writing the new plan. The gate's
	// route to it is already durable; the plan node's finish is not.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupted := graph.NewRegistry()
	interrupted.MustRegister(pressesCtrlC{name: graph.NodePlan, cancel: cancel}, approve.New(), endsTheRun{reached: &reachedCode})
	opts := p.options(store)
	opts.Registry = interrupted
	opts.PauseOnCancel = true
	stopped, err := graph.NewDispatcher(opts)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if _, err := stopped.Resume(ctx); err != nil && !errors.Is(err, graph.ErrInterrupted) {
		t.Fatalf("Resume: %v", err)
	}

	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !st.Plan.Regenerate.Pending() {
		t.Fatalf("the request was lost when the run stopped: %+v", st.Plan.Regenerate)
	}

	// belay resume, in a new process, with the plan node that works.
	final, err := dispatcher.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume after the crash: %v", err)
	}
	if final.Status != state.RunStatusPaused || final.Node != graph.NodeApprove {
		t.Fatalf("outcome = %+v, want a pause at the gate with the new plan", final)
	}
	if got := readFile(t, filepath.Join(store.Layout().ArtifactsDir(), plan.ArtifactName)); got != secondPlan {
		t.Errorf("plan.md holds %q, want the new plan", got)
	}
	if got := agent.CallCount(); got != 2 {
		t.Errorf("the agent was called %d times, want 2: the re-run must answer the request once", got)
	}
	st, err = store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if st.Plan.Regenerate.Pending() {
		t.Errorf("the request survived being answered: %+v", st.Plan.Regenerate)
	}
}

// readFile returns a file's contents, failing the test if it cannot.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path inside this test's own t.TempDir()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
