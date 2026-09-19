package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// ---------------------------------------------------------------------------
// Fakes. internal/nodes is being written in parallel and may not compile, so
// every Node here is defined locally.
// ---------------------------------------------------------------------------

// nodeStep scripts one execution of a fakeNode.
type nodeStep struct {
	res Result
	err error
	// sleep makes the node run long enough to blow a node timeout.
	sleep time.Duration
	// ignoreCtx makes the node sleep without watching for cancellation, so a
	// test can prove the dispatcher's deadline is authoritative over a node
	// that does not cooperate.
	ignoreCtx bool
}

// Result is a local alias so the scripts below read compactly.
type Result = graph.Result

// fakeNode replays a script of nodeSteps and records what it saw. When the
// script runs out, the last step repeats, which is what lets a self-looping
// node drive the MaxSteps test.
type fakeNode struct {
	name  string
	steps []nodeStep

	calls    int
	attempts []int
	seenStep []int
	seenGoal []string
	seenFix  []int
}

func (f *fakeNode) Name() string { return f.name }

func (f *fakeNode) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	f.calls++
	f.attempts = append(f.attempts, rc.Attempt)
	f.seenStep = append(f.seenStep, rc.Step)
	f.seenGoal = append(f.seenGoal, rc.Goal)
	f.seenFix = append(f.seenFix, rc.State.Fix.Attempts)

	idx := f.calls - 1
	if idx >= len(f.steps) {
		idx = len(f.steps) - 1
	}
	st := f.steps[idx]

	if st.sleep > 0 {
		if st.ignoreCtx {
			time.Sleep(st.sleep)
		} else {
			select {
			case <-time.After(st.sleep):
			case <-ctx.Done():
				return graph.Result{}, ctx.Err()
			}
		}
	}
	return st.res, st.err
}

// ok builds a fakeNode that always routes to next.
func ok(name, next string) *fakeNode {
	return &fakeNode{name: name, steps: []nodeStep{{
		res: Result{Next: next, Status: journal.StatusOK},
	}}}
}

// ---------------------------------------------------------------------------
// Run-directory harness.
// ---------------------------------------------------------------------------

const testRunID = "20260101T000000Z-0123456789ab"

type harness struct {
	store  *state.Store
	layout state.Layout
	cfg    config.Config
}

// newRun builds a run directory with a manifest and an initial state.json.
func newRun(t *testing.T, cfg config.Config, goal string) *harness {
	t.Helper()
	ws := t.TempDir()
	layout, err := state.NewLayout(ws, testRunID)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", layout.RunDir(), err)
	}
	store := state.NewStore(layout)
	man := state.NewManifest(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), testRunID, ws, goal, cfg)
	if err := store.SaveManifest(man); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}
	if err := store.SaveState(state.NewState(goal)); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
	return &harness{store: store, layout: layout, cfg: cfg}
}

// dispatcher wires nodes into a Dispatcher over this run directory.
func (h *harness) dispatcher(t *testing.T, nodes ...graph.Node) *graph.Dispatcher {
	t.Helper()
	reg := graph.NewRegistry()
	reg.MustRegister(nodes...)
	d, err := graph.NewDispatcher(graph.Options{
		Store: h.store, Registry: reg, Config: h.cfg,
		Now: func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	return d
}

// pausingDispatcher is dispatcher with Options.PauseOnCancel set to pause,
// which is how `belay run` and `belay resume` build theirs.
func (h *harness) pausingDispatcher(t *testing.T, pause bool, nodes ...graph.Node) *graph.Dispatcher {
	t.Helper()
	reg := graph.NewRegistry()
	reg.MustRegister(nodes...)
	d, err := graph.NewDispatcher(graph.Options{
		Store: h.store, Registry: reg, Config: h.cfg,
		Now:           func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) },
		PauseOnCancel: pause,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	return d
}

// seedJournal appends records directly, standing in for a previous process.
func (h *harness) seedJournal(t *testing.T, recs ...journal.Record) {
	t.Helper()
	j, err := journal.Open(h.layout.JournalPath())
	if err != nil {
		t.Fatalf("journal.Open() error = %v", err)
	}
	for _, r := range recs {
		if _, err := j.Append(r); err != nil {
			t.Fatalf("journal.Append(%v) error = %v", r.Event, err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatalf("journal.Close() error = %v", err)
	}
}

func (h *harness) records(t *testing.T) []journal.Record {
	t.Helper()
	res, err := journal.ReadFile(h.layout.JournalPath())
	if err != nil {
		t.Fatalf("journal.ReadFile() error = %v", err)
	}
	if res.Incomplete {
		t.Fatalf("journal has a torn tail after a clean dispatcher shutdown")
	}
	return res.Records
}

// timeline renders the journal compactly for assertion.
func (h *harness) timeline(t *testing.T) []string {
	t.Helper()
	recs := h.records(t)

	// Every terminal path must leave a structurally valid journal. This is
	// the invariant that catches a run-level record written while a
	// node_started is still open, which would make the run unresumable.
	if err := journal.Validate(recs); err != nil {
		t.Fatalf("journal.Validate() = %v, want nil; the dispatcher wrote an unresumable journal", err)
	}

	out := make([]string, 0, len(recs))
	for _, r := range recs {
		switch r.Event {
		case journal.EventNodeStarted:
			out = append(out, fmt.Sprintf("%s#%d started", r.Node, r.Attempt))
		case journal.EventNodeFinished:
			s := fmt.Sprintf("%s#%d finished %s", r.Node, r.Attempt, r.Status)
			if r.Next != "" {
				s += " -> " + r.Next
			}
			out = append(out, s)
		default:
			out = append(out, r.Event.String())
		}
	}
	return out
}

func (h *harness) manifest(t *testing.T) state.Manifest {
	t.Helper()
	m, err := h.store.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	return m
}

func (h *harness) state(t *testing.T) state.State {
	t.Helper()
	s, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	return s
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func closeTo(got, want float64) bool {
	d := got - want
	return d < 1e-9 && d > -1e-9
}

// budgetCfg returns a config with an explicit budget ceiling.
func budgetCfg(maxUSD float64, maxTokens int64) config.Config {
	cfg := config.Default()
	cfg.Budget.MaxUSD = maxUSD
	cfg.Budget.MaxTokens = maxTokens
	cfg.Budget.OnExceed = config.OnExceedAbort
	return cfg
}

// ---------------------------------------------------------------------------
// Straight line and loops.
// ---------------------------------------------------------------------------

func TestDispatcher_StraightLineRun(t *testing.T) {
	h := newRun(t, config.Default(), "ship it")
	plan := ok(graph.NodePlan, graph.NodeCode)
	code := ok(graph.NodeCode, graph.End)

	out, err := h.dispatcher(t, plan, code).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if out.Status != state.RunStatusCompleted || out.ExitCode != graph.ExitOK {
		t.Errorf("Outcome = {%v, exit %d}, want {completed, exit %d}", out.Status, out.ExitCode, graph.ExitOK)
	}
	if out.Steps != 2 {
		t.Errorf("Outcome.Steps = %d, want 2", out.Steps)
	}

	want := []string{
		"run_started",
		"plan#1 started", "plan#1 finished ok -> code",
		"code#1 started", "code#1 finished ok -> END",
		"run_completed",
	}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}

	man := h.manifest(t)
	if man.Status != state.RunStatusCompleted {
		t.Errorf("manifest.Status = %v, want completed", man.Status)
	}
	if man.Step != 2 {
		t.Errorf("manifest.Step = %d, want 2", man.Step)
	}

	// Every execution is recorded on the blackboard, keyed by a real,
	// non-zero journal Seq.
	hist := h.state(t).History
	if len(hist) != 2 {
		t.Fatalf("State.History has %d entries, want 2: %+v", len(hist), hist)
	}
	for _, e := range hist {
		if e.Seq == 0 {
			t.Errorf("history entry %+v has Seq 0, which cannot serve as an idempotency key", e)
		}
	}
	if plan.seenGoal[0] != "ship it" {
		t.Errorf("RunContext.Goal = %q, want %q", plan.seenGoal[0], "ship it")
	}
	if plan.seenStep[0] != 1 || code.seenStep[0] != 2 {
		t.Errorf("RunContext.Step = plan %d, code %d; want 1 and 2", plan.seenStep[0], code.seenStep[0])
	}
}

func TestDispatcher_FixLoop(t *testing.T) {
	h := newRun(t, config.Default(), "make tests pass")

	plan := ok(graph.NodePlan, graph.NodeTest)
	// test fails first, then passes.
	testNode := &fakeNode{name: graph.NodeTest, steps: []nodeStep{
		{res: Result{Next: graph.NodeFix, Status: journal.StatusOK, Note: "1 failing"}},
		{res: Result{Next: graph.NodeReview, Status: journal.StatusOK, Note: "green"}},
	}}
	fix := ok(graph.NodeFix, graph.NodeTest)
	review := ok(graph.NodeReview, graph.End)

	out, err := h.dispatcher(t, plan, testNode, fix, review).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed (note %q)", out.Status, out.Note)
	}

	want := []string{
		"run_started",
		"plan#1 started", "plan#1 finished ok -> test",
		"test#1 started", "test#1 finished ok -> fix",
		"fix#1 started", "fix#1 finished ok -> test",
		"test#1 started", "test#1 finished ok -> review",
		"review#1 started", "review#1 finished ok -> END",
		"run_completed",
	}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
	if testNode.calls != 2 {
		t.Errorf("test node ran %d times, want 2", testNode.calls)
	}
	// A loop iteration is a fresh visit, not a crash retry: both visits are
	// attempt 1. Attempt increments only for crash recovery.
	for i, a := range testNode.attempts {
		if a != 1 {
			t.Errorf("test visit %d ran at attempt %d, want 1 (a loop iteration is not a retry)", i+1, a)
		}
	}
}

// ---------------------------------------------------------------------------
// Crash / resume. The product's headline claim.
// ---------------------------------------------------------------------------

func TestDispatcher_CrashResume_ReRunsUnfinishedNodeAtAttempt2(t *testing.T) {
	h := newRun(t, config.Default(), "survive a crash")
	// A previous process died inside plan: node_started with no matching
	// node_finished.
	h.seedJournal(t,
		journal.Record{Event: journal.EventRunStarted},
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodePlan, Attempt: 1},
	)

	plan := ok(graph.NodePlan, graph.NodeCode)
	code := ok(graph.NodeCode, graph.End)

	out, err := h.dispatcher(t, plan, code).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed", out.Status)
	}

	// The node is re-run, not skipped.
	if plan.calls != 1 {
		t.Fatalf("plan ran %d times, want 1: an unfinished node must be re-run, never skipped", plan.calls)
	}
	if plan.attempts[0] != 2 {
		t.Errorf("plan re-ran at attempt %d, want 2", plan.attempts[0])
	}

	want := []string{
		"run_started",
		// The crashed attempt is closed as aborted before the retry opens,
		// naming itself as the resume point. Without that record the journal
		// would hold two open node_started entries and journal.Open would
		// refuse the file from then on.
		"plan#1 started", "plan#1 finished aborted -> plan",
		"plan#2 started", "plan#2 finished ok -> code",
		"code#1 started", "code#1 finished ok -> END",
		"run_completed",
	}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
}

// TestDispatcher_CrashResume_PatchIsNotDoubleApplied covers the crash window
// the dispatcher's write ordering deliberately makes reachable: the patch was
// applied, then the process died before node_finished was journalled.
//
// Re-running the node applies its patch a second time. Because every Patch
// field is an absolute set rather than a delta, the resulting State is
// identical to applying it once — Fix.Attempts stays 1 rather than becoming 2,
// which is exactly the corruption a delta-shaped patch would produce here.
func TestDispatcher_CrashResume_PatchIsNotDoubleApplied(t *testing.T) {
	h := newRun(t, config.Default(), "no double apply")

	// Reconstruct disk exactly as that crash leaves it: state.json already
	// carries the fix node's patch, the journal does not know it finished.
	pre := h.state(t)
	pre.Fix = state.Fix{Attempts: 1}
	if err := h.store.SaveState(pre); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
	h.seedJournal(t,
		journal.Record{Event: journal.EventRunStarted},
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodeFix, Attempt: 1},
	)

	// The re-run returns the very same absolute patch the crashed attempt did.
	fixState := state.Fix{Attempts: 1}
	fix := &fakeNode{name: graph.NodeFix, steps: []nodeStep{{
		res: Result{Next: graph.End, Status: journal.StatusOK, Patch: state.Patch{Fix: &fixState}},
	}}}

	if _, err := h.dispatcher(t, fix).Resume(context.Background()); err != nil {
		t.Fatalf("Resume() error = %v, want nil", err)
	}

	if fix.calls != 1 || fix.attempts[0] != 2 {
		t.Fatalf("fix ran %d times at attempts %v, want 1 call at attempt 2", fix.calls, fix.attempts)
	}
	// The node saw the state its crashed attempt had already written.
	if fix.seenFix[0] != 1 {
		t.Errorf("re-run observed Fix.Attempts = %d, want 1 (the crashed attempt's patch was durable)", fix.seenFix[0])
	}
	if got := h.state(t).Fix.Attempts; got != 1 {
		t.Errorf("Fix.Attempts = %d after re-applying the same patch, want 1: an absolute patch must be idempotent", got)
	}

	// History is keyed on Seq, so no entry is duplicated.
	seen := map[uint64]bool{}
	for _, e := range h.state(t).History {
		if seen[e.Seq] {
			t.Errorf("State.History contains a duplicate Seq %d", e.Seq)
		}
		seen[e.Seq] = true
	}
}

// TestDispatcher_ResumeSkipsANodeAlreadyMarkedFinished is the negative
// characterization behind the write ordering.
//
// Here the journal claims plan finished, but state.json carries none of its
// output — precisely the disk state that journalling node_finished BEFORE
// applying the patch would leave behind after a crash. ResolveStart reads the
// journal alone, so it resumes at the successor and the node's effects are
// gone for good, with nothing signalling the loss.
//
// The dispatcher applies the patch first specifically so this state is never
// produced. This test pins the consequence, so anyone tempted to swap the two
// writes can see what they would be buying.
func TestDispatcher_ResumeSkipsANodeAlreadyMarkedFinished(t *testing.T) {
	h := newRun(t, config.Default(), "characterize the rejected ordering")
	h.seedJournal(t,
		journal.Record{Event: journal.EventRunStarted},
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodePlan, Attempt: 1},
		journal.Record{
			Event: journal.EventNodeFinished, Node: graph.NodePlan, Attempt: 1,
			Status: journal.StatusOK, Next: graph.NodeCode,
		},
	)

	plan := ok(graph.NodePlan, graph.NodeCode)
	code := ok(graph.NodeCode, graph.End)

	if _, err := h.dispatcher(t, plan, code).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if plan.calls != 0 {
		t.Errorf("plan ran %d times, want 0: a journalled node_finished is trusted verbatim", plan.calls)
	}
	if code.calls != 1 {
		t.Errorf("code ran %d times, want 1", code.calls)
	}
	if got := h.state(t).Plan.Path; got != "" {
		t.Errorf("Plan.Path = %q, want empty: plan's patch was never applied and is unrecoverable", got)
	}
}

// ---------------------------------------------------------------------------
// Pause and resume.
// ---------------------------------------------------------------------------

func TestDispatcher_PausedRunRefusesRunAndAdvancesUnderResume(t *testing.T) {
	h := newRun(t, config.Default(), "await approval")

	plan := ok(graph.NodePlan, graph.NodeApprove)
	approve := &fakeNode{name: graph.NodeApprove, steps: []nodeStep{
		{res: Result{Status: journal.StatusPaused, Note: "plan needs sign-off"}},
		{res: Result{Next: graph.End, Status: journal.StatusOK}},
	}}
	d := h.dispatcher(t, plan, approve)

	// 1. The run pauses. That is a successful invocation: exit 0.
	out, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusPaused || out.ExitCode != graph.ExitOK {
		t.Errorf("Outcome = {%v, exit %d}, want {paused, exit %d}", out.Status, out.ExitCode, graph.ExitOK)
	}
	if h.manifest(t).Status != state.RunStatusPaused {
		t.Errorf("manifest.Status = %v, want paused", h.manifest(t).Status)
	}

	// 2. A plain Run refuses to step past the pause, and runs nothing.
	callsBefore := approve.calls
	out, err = d.Run(context.Background())
	if !errors.Is(err, graph.ErrRunPaused) {
		t.Fatalf("Run() on a paused run error = %v, want ErrRunPaused", err)
	}
	if out.ExitCode != graph.ExitPaused {
		t.Errorf("Outcome.ExitCode = %d, want %d", out.ExitCode, graph.ExitPaused)
	}
	if approve.calls != callsBefore {
		t.Errorf("approve ran %d more times; a refused Run must execute nothing", approve.calls-callsBefore)
	}

	// 3. Resume advances it.
	out, err = d.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted || out.ExitCode != graph.ExitOK {
		t.Errorf("Outcome = {%v, exit %d}, want {completed, exit 0}", out.Status, out.ExitCode)
	}

	want := []string{
		"run_started",
		"plan#1 started", "plan#1 finished ok -> approve",
		"approve#1 started", "approve#1 finished paused -> approve",
		"run_paused",
		"run_resumed",
		"approve#1 started", "approve#1 finished ok -> END",
		"run_completed",
	}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Budget.
// ---------------------------------------------------------------------------

func TestDispatcher_BudgetExceededAbortsBeforeTheNodeRuns(t *testing.T) {
	h := newRun(t, budgetCfg(5.00, 0), "spend nothing more")

	// The manifest already records spend past the ceiling, as a crashed run
	// would.
	man := h.manifest(t)
	man.Budget = state.Budget{LimitUSD: 5.00, SpentUSD: 7.50, TokensIn: 100, TokensOut: 50}
	if err := h.store.SaveManifest(man); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}

	plan := ok(graph.NodePlan, graph.End)
	out, err := h.dispatcher(t, plan).Run(context.Background())

	if !errors.Is(err, belay.ErrBudgetExceeded) {
		t.Fatalf("Run() error = %v, want one wrapping belay.ErrBudgetExceeded", err)
	}
	// The whole point: the ceiling is checked before the node is dispatched.
	if plan.calls != 0 {
		t.Errorf("plan ran %d times, want 0: the budget must abort before a node executes", plan.calls)
	}
	if out.Status != state.RunStatusAborted || out.ExitCode != graph.ExitAborted {
		t.Errorf("Outcome = {%v, exit %d}, want {aborted, exit %d}", out.Status, out.ExitCode, graph.ExitAborted)
	}
	// No node_started was ever written, so nothing is left open.
	want := []string{"run_started", "run_aborted"}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
	if h.manifest(t).Status != state.RunStatusAborted {
		t.Errorf("manifest.Status = %v, want aborted", h.manifest(t).Status)
	}
}

// TestDispatcher_ResumeContinuesTheLedgerRatherThanResettingIt is acceptance
// criterion 4: a resumed run must keep spending from where the crash left off.
// A fresh ledger would let every crash hand the run a full ceiling again.
func TestDispatcher_ResumeContinuesTheLedgerRatherThanResettingIt(t *testing.T) {
	h := newRun(t, budgetCfg(100.00, 0), "keep counting")

	man := h.manifest(t)
	man.Budget = state.Budget{LimitUSD: 100.00, SpentUSD: 3.00, TokensIn: 100, TokensOut: 50}
	if err := h.store.SaveManifest(man); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}

	plan := &fakeNode{name: graph.NodePlan, steps: []nodeStep{{
		res: Result{
			Next: graph.End, Status: journal.StatusOK,
			Usage: belay.Usage{InputTokens: 10, OutputTokens: 5, USD: 1.00},
		},
	}}}

	out, err := h.dispatcher(t, plan).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	// 3.00 restored + 1.00 spent. A reset ledger would report 1.00.
	if !closeTo(out.Usage.USD, 4.00) {
		t.Errorf("Outcome.Usage.USD = %v, want 4.00 (3.00 restored + 1.00 spent); "+
			"1.00 would mean the resume reset the ledger", out.Usage.USD)
	}
	if out.Usage.InputTokens != 110 || out.Usage.OutputTokens != 55 {
		t.Errorf("Outcome.Usage tokens = in %d / out %d, want 110 / 55",
			out.Usage.InputTokens, out.Usage.OutputTokens)
	}

	got := h.manifest(t).Budget
	if !closeTo(got.SpentUSD, 4.00) || got.TokensIn != 110 || got.TokensOut != 55 {
		t.Errorf("manifest.Budget = %+v, want SpentUSD 4.00, TokensIn 110, TokensOut 55", got)
	}
	if !closeTo(got.LimitUSD, 100.00) {
		t.Errorf("manifest.Budget.LimitUSD = %v, want 100.00", got.LimitUSD)
	}
}

// ---------------------------------------------------------------------------
// Guards: steps, timeout, node failure.
// ---------------------------------------------------------------------------

func TestDispatcher_MaxStepsExceeded(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.MaxSteps = 3
	h := newRun(t, cfg, "never converge")

	// A node that routes to itself forever: the fix loop that does not
	// terminate, which is exactly what MaxSteps exists to stop.
	loop := ok(graph.NodePlan, graph.NodePlan)

	out, err := h.dispatcher(t, loop).Run(context.Background())
	if !errors.Is(err, graph.ErrMaxSteps) {
		t.Fatalf("Run() error = %v, want one wrapping ErrMaxSteps", err)
	}
	if loop.calls != 3 {
		t.Errorf("node ran %d times, want exactly MaxSteps=3", loop.calls)
	}
	if out.Status != state.RunStatusAborted || out.ExitCode != graph.ExitAborted {
		t.Errorf("Outcome = {%v, exit %d}, want {aborted, exit %d}", out.Status, out.ExitCode, graph.ExitAborted)
	}
	if got := h.manifest(t).Step; got != 3 {
		t.Errorf("manifest.Step = %d, want 3", got)
	}
	if last := h.timeline(t)[len(h.timeline(t))-1]; last != "run_aborted" {
		t.Errorf("last journal record = %q, want run_aborted", last)
	}
}

func TestDispatcher_NodeTimeoutAbortsTheRun(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.NodeTimeout = config.NewDuration(20 * time.Millisecond)
	h := newRun(t, cfg, "too slow")

	// The node ignores its context entirely, proving the deadline is
	// authoritative rather than advisory.
	slow := &fakeNode{name: graph.NodePlan, steps: []nodeStep{{
		res:       Result{Next: graph.End, Status: journal.StatusOK},
		sleep:     150 * time.Millisecond,
		ignoreCtx: true,
	}}}

	out, err := h.dispatcher(t, slow).Run(context.Background())
	if !errors.Is(err, graph.ErrNodeTimeout) {
		t.Fatalf("Run() error = %v, want one wrapping ErrNodeTimeout", err)
	}
	if out.Status != state.RunStatusAborted || out.ExitCode != graph.ExitAborted {
		t.Errorf("Outcome = {%v, exit %d}, want {aborted, exit %d}", out.Status, out.ExitCode, graph.ExitAborted)
	}
	// The run ends rather than looping, and the node execution is closed as
	// aborted so the journal stays resumable.
	want := []string{"run_started", "plan#1 started", "plan#1 finished aborted", "run_aborted"}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
	if out.Note == "" {
		t.Error("Outcome.Note is empty, want a clear timeout explanation")
	}
}

func TestDispatcher_NodeErrorAndInvalidResult(t *testing.T) {
	tests := []struct {
		name       string
		step       nodeStep
		wantIs     []error
		notePrefix string
	}{
		{
			name:       "node returns an error",
			step:       nodeStep{err: errors.New("go toolchain not found")},
			wantIs:     []error{graph.ErrNodeFailed},
			notePrefix: "node error: ",
		},
		{
			name:       "node returns an unusable Result",
			step:       nodeStep{res: Result{Status: journal.StatusOK}}, // OK with no Next
			wantIs:     []error{graph.ErrNodeFailed, graph.ErrInvalidResult},
			notePrefix: "invalid result: ",
		},
		{
			name:       "node returns StatusFailed cleanly",
			step:       nodeStep{res: Result{Status: journal.StatusFailed, Note: "fix loop gave up"}},
			wantIs:     []error{graph.ErrNodeFailed},
			notePrefix: "fix loop gave up",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, config.Default(), "fail loudly")
			// A patch that must NOT be applied on the rejected paths.
			plan := &fakeNode{name: graph.NodePlan, steps: []nodeStep{tt.step}}

			out, err := h.dispatcher(t, plan).Run(context.Background())
			for _, target := range tt.wantIs {
				if !errors.Is(err, target) {
					t.Errorf("Run() error = %v, want one wrapping %v", err, target)
				}
			}
			if out.Status != state.RunStatusFailed || out.ExitCode != graph.ExitFailed {
				t.Errorf("Outcome = {%v, exit %d}, want {failed, exit %d}",
					out.Status, out.ExitCode, graph.ExitFailed)
			}

			recs := h.records(t)
			if err := journal.Validate(recs); err != nil {
				t.Fatalf("journal.Validate() = %v, want nil", err)
			}
			var finished journal.Record
			for _, r := range recs {
				if r.Event == journal.EventNodeFinished {
					finished = r
				}
			}
			if finished.Status != journal.StatusFailed {
				t.Errorf("node_finished status = %v, want failed", finished.Status)
			}
			// The journal distinguishes the three shapes by note prefix.
			if len(finished.Note) < len(tt.notePrefix) || finished.Note[:len(tt.notePrefix)] != tt.notePrefix {
				t.Errorf("node_finished note = %q, want prefix %q", finished.Note, tt.notePrefix)
			}
			if last := recs[len(recs)-1].Event; last != journal.EventRunFailed {
				t.Errorf("last record = %v, want run_failed", last)
			}
		})
	}
}

// TestDispatcher_RejectedResultsDoNotApplyTheirPatch pins that a Result the
// dispatcher refuses to trust never reaches state.json.
func TestDispatcher_RejectedResultsDoNotApplyTheirPatch(t *testing.T) {
	h := newRun(t, config.Default(), "distrust a broken node")
	poisoned := "REWRITTEN BY A BROKEN NODE"
	plan := &fakeNode{name: graph.NodePlan, steps: []nodeStep{{
		// Invalid: StatusOK with no Next. The Patch must be ignored with it.
		res: Result{Status: journal.StatusOK, Patch: state.Patch{Goal: &poisoned}},
	}}}

	if _, err := h.dispatcher(t, plan).Run(context.Background()); !errors.Is(err, graph.ErrInvalidResult) {
		t.Fatalf("Run() error = %v, want one wrapping ErrInvalidResult", err)
	}
	if got := h.state(t).Goal; got == poisoned {
		t.Error("the patch from an invalid Result was applied; an unusable Result must be discarded whole")
	}
	if got := len(h.state(t).History); got != 0 {
		t.Errorf("State.History has %d entries, want 0 for a rejected execution", got)
	}
}

func TestDispatcher_UnknownNextNodeFailsLoudly(t *testing.T) {
	h := newRun(t, config.Default(), "bad wiring")
	plan := ok(graph.NodePlan, "no-such-node")

	out, err := h.dispatcher(t, plan).Run(context.Background())
	if !errors.Is(err, graph.ErrUnknownNode) {
		t.Fatalf("Run() error = %v, want one wrapping ErrUnknownNode", err)
	}
	if out.ExitCode != graph.ExitFailed {
		t.Errorf("Outcome.ExitCode = %d, want %d", out.ExitCode, graph.ExitFailed)
	}
	if got := h.timeline(t); got[len(got)-1] != "run_failed" {
		t.Errorf("last journal record = %q, want run_failed", got[len(got)-1])
	}
}

func TestDispatcher_ContextCancellationAborts(t *testing.T) {
	h := newRun(t, config.Default(), "stop now")
	ctx, cancel := context.WithCancel(context.Background())
	blocked := &fakeNode{name: graph.NodePlan, steps: []nodeStep{{
		res: Result{Next: graph.End, Status: journal.StatusOK}, sleep: time.Minute,
	}}}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	out, err := h.dispatcher(t, blocked).Run(ctx)
	if !errors.Is(err, graph.ErrNodeAborted) {
		t.Fatalf("Run() error = %v, want one wrapping ErrNodeAborted", err)
	}
	if out.ExitCode != graph.ExitAborted {
		t.Errorf("Outcome.ExitCode = %d, want %d", out.ExitCode, graph.ExitAborted)
	}
	want := []string{"run_started", "plan#1 started", "plan#1 finished aborted", "run_aborted"}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Terminal states and construction.
// ---------------------------------------------------------------------------

func TestDispatcher_TerminalRunsDoNoWork(t *testing.T) {
	tests := []struct {
		name     string
		marker   journal.Event
		want     state.RunStatus
		exitCode int
		wantErr  error
	}{
		{"completed", journal.EventRunCompleted, state.RunStatusCompleted, graph.ExitOK, nil},
		{"failed", journal.EventRunFailed, state.RunStatusFailed, graph.ExitFailed, graph.ErrRunEnded},
		{"aborted", journal.EventRunAborted, state.RunStatusAborted, graph.ExitAborted, graph.ErrRunEnded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, config.Default(), "already over")
			h.seedJournal(t,
				journal.Record{Event: journal.EventRunStarted},
				journal.Record{Event: journal.EventNodeStarted, Node: graph.NodePlan, Attempt: 1},
				journal.Record{
					Event: journal.EventNodeFinished, Node: graph.NodePlan, Attempt: 1,
					Status: journal.StatusOK, Next: graph.End,
				},
				journal.Record{Event: tt.marker},
			)
			plan := ok(graph.NodePlan, graph.End)

			out, err := h.dispatcher(t, plan).Resume(context.Background())
			if tt.wantErr == nil && err != nil {
				t.Fatalf("Resume() error = %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Resume() error = %v, want one wrapping %v", err, tt.wantErr)
			}
			if out.Status != tt.want || out.ExitCode != tt.exitCode {
				t.Errorf("Outcome = {%v, exit %d}, want {%v, exit %d}",
					out.Status, out.ExitCode, tt.want, tt.exitCode)
			}
			if plan.calls != 0 {
				t.Errorf("plan ran %d times, want 0 on an already-terminal run", plan.calls)
			}
		})
	}
}

func TestNewDispatcher_Validation(t *testing.T) {
	full := graph.NewRegistry()
	full.MustRegister(ok(graph.NodePlan, graph.End))
	store := state.NewStore(state.Layout{})

	tests := []struct {
		name string
		opts graph.Options
		want error
	}{
		{"missing store", graph.Options{Registry: full}, graph.ErrInvalidOptions},
		{"missing registry", graph.Options{Store: store}, graph.ErrInvalidOptions},
		{"empty registry", graph.Options{Store: store, Registry: graph.NewRegistry()}, graph.ErrInvalidOptions},
		{"complete", graph.Options{Store: store, Registry: full}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := graph.NewDispatcher(tt.opts)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("NewDispatcher() error = %v, want nil", err)
				}
				if d == nil {
					t.Fatal("NewDispatcher() = nil, want a dispatcher")
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("NewDispatcher() error = %v, want one wrapping %v", err, tt.want)
			}
		})
	}
}

// TestDispatcher_ExitCodesAreDistinct guards the numbering the CLI maps onto
// os.Exit.
func TestDispatcher_ExitCodesAreDistinct(t *testing.T) {
	codes := map[string]int{
		"ExitOK":      graph.ExitOK,
		"ExitFailed":  graph.ExitFailed,
		"ExitAborted": graph.ExitAborted,
		"ExitPaused":  graph.ExitPaused,
	}
	want := map[string]int{"ExitOK": 0, "ExitFailed": 1, "ExitAborted": 3, "ExitPaused": 4}
	for name, got := range codes {
		if got != want[name] {
			t.Errorf("%s = %d, want %d", name, got, want[name])
		}
	}
}

// TestDispatcher_JournalSurvivesRepeatedCrashes is the regression test for the
// defect that made crash recovery work exactly once.
//
// Retrying a node without first closing the node_started its dead attempt left
// open produces two open executions in a row, which journal.Validate reports as
// corruption — and journal.Open refuses to append to a corrupt journal, so the
// run could never be resumed again. Here the run is interrupted twice, and the
// journal must stay both openable and valid throughout.
func TestDispatcher_JournalSurvivesRepeatedCrashes(t *testing.T) {
	h := newRun(t, config.Default(), "crash twice")

	h.seedJournal(t,
		journal.Record{Event: journal.EventRunStarted},
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodePlan, Attempt: 1},
	)
	// A second process also died inside plan. Recovering the first attempt
	// must leave a journal the next Open still accepts.
	plan1 := ok(graph.NodePlan, graph.NodeCode)
	code1 := ok(graph.NodeCode, graph.End)
	d := h.dispatcher(t, plan1, code1)
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatalf("first recovery Run() error = %v", err)
	}
	if err := journal.Validate(h.records(t)); err != nil {
		t.Fatalf("journal.Validate() after one recovery = %v, want nil", err)
	}

	// Simulate a third process dying inside a node appended onto the healed
	// journal, then recover again.
	h.seedJournal(t, journal.Record{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1})
	if err := journal.Validate(h.records(t)); err != nil {
		t.Fatalf("journal.Validate() with an open node = %v, want nil", err)
	}

	plan2 := ok(graph.NodePlan, graph.NodeCode)
	code2 := ok(graph.NodeCode, graph.End)
	if _, err := h.dispatcher(t, plan2, code2).Resume(context.Background()); err != nil {
		t.Fatalf("second recovery Resume() error = %v", err)
	}
	if err := journal.Validate(h.records(t)); err != nil {
		t.Fatalf("journal.Validate() after two recoveries = %v, want nil", err)
	}
	// Proof the file is still usable: Open refuses a corrupt journal.
	j, err := journal.Open(h.layout.JournalPath())
	if err != nil {
		t.Fatalf("journal.Open() after two recoveries = %v, want nil", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("journal.Close() error = %v", err)
	}
	if code2.calls != 1 || code2.attempts[0] != 2 {
		t.Errorf("code ran %d times at attempts %v, want 1 call at attempt 2", code2.calls, code2.attempts)
	}
}

// TestDispatcher_CrashBetweenNodeFinishedAndRunMarker covers the other crash
// window: the node ended the run, but the process died before the run-level
// record said so. ResolveStart alone would call such a journal merely
// runnable, which would walk straight through an approval gate or step a
// failed run onwards.
func TestDispatcher_CrashBetweenNodeFinishedAndRunMarker(t *testing.T) {
	tests := []struct {
		name       string
		status     journal.Status
		next       string
		wantMarker journal.Event
		wantStatus state.RunStatus
		wantExit   int
		wantErr    error
	}{
		{
			name: "paused gate is not walked through", status: journal.StatusPaused,
			next: graph.NodeApprove, wantMarker: journal.EventRunPaused,
			wantStatus: state.RunStatusPaused, wantExit: graph.ExitPaused, wantErr: graph.ErrRunPaused,
		},
		{
			name: "failed run is not stepped onwards", status: journal.StatusFailed,
			wantMarker: journal.EventRunFailed,
			wantStatus: state.RunStatusFailed, wantExit: graph.ExitFailed, wantErr: graph.ErrRunEnded,
		},
		{
			name: "aborted run stays aborted", status: journal.StatusAborted,
			wantMarker: journal.EventRunAborted,
			wantStatus: state.RunStatusAborted, wantExit: graph.ExitAborted, wantErr: graph.ErrRunEnded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, config.Default(), "died before the marker")
			h.seedJournal(t,
				journal.Record{Event: journal.EventRunStarted},
				journal.Record{Event: journal.EventNodeStarted, Node: graph.NodeApprove, Attempt: 1},
				journal.Record{
					Event: journal.EventNodeFinished, Node: graph.NodeApprove, Attempt: 1,
					Status: tt.status, Next: tt.next,
				},
			)
			approve := ok(graph.NodeApprove, graph.End)

			out, err := h.dispatcher(t, approve).Run(context.Background())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Run() error = %v, want one wrapping %v", err, tt.wantErr)
			}
			if out.Status != tt.wantStatus || out.ExitCode != tt.wantExit {
				t.Errorf("Outcome = {%v, exit %d}, want {%v, exit %d}",
					out.Status, out.ExitCode, tt.wantStatus, tt.wantExit)
			}
			if approve.calls != 0 {
				t.Errorf("approve ran %d times, want 0", approve.calls)
			}

			recs := h.records(t)
			if got := recs[len(recs)-1].Event; got != tt.wantMarker {
				t.Errorf("last record = %v, want the owed %v", got, tt.wantMarker)
			}
			if err := journal.Validate(recs); err != nil {
				t.Errorf("journal.Validate() = %v, want nil", err)
			}
			if got := h.manifest(t).Status; got != tt.wantStatus {
				t.Errorf("manifest.Status = %v, want %v; the manifest must agree with the healed journal", got, tt.wantStatus)
			}
		})
	}
}

// TestDispatcher_CrashAfterClosingAnInterruptedAttemptStillReRunsIt covers
// the window inside recovery itself: heal has closed the dead attempt, naming
// its own node as Next, and the process died before the retry's
// node_started. That record is an attempt closed so it can be run again, not
// an abort that ended the run, so nothing is owed: the next process re-runs
// the node at the attempt after the closed one and finishes the run.
func TestDispatcher_CrashAfterClosingAnInterruptedAttemptStillReRunsIt(t *testing.T) {
	h := newRun(t, config.Default(), "crash during recovery")
	h.seedJournal(t,
		journal.Record{Event: journal.EventRunStarted},
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodePlan, Attempt: 1},
		journal.Record{
			Event: journal.EventNodeFinished, Node: graph.NodePlan, Attempt: 1,
			Status: journal.StatusAborted, Next: graph.NodePlan,
		},
	)
	plan := ok(graph.NodePlan, graph.NodeCode)
	code := ok(graph.NodeCode, graph.End)

	out, err := h.dispatcher(t, plan, code).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil: the run was interrupted, not ended", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed", out.Status)
	}
	if plan.calls != 1 || plan.attempts[0] != 2 {
		t.Errorf("plan ran %d times at attempts %v, want 1 call at attempt 2", plan.calls, plan.attempts)
	}

	want := []string{
		"run_started",
		"plan#1 started", "plan#1 finished aborted -> plan",
		"plan#2 started", "plan#2 finished ok -> code",
		"code#1 started", "code#1 finished ok -> END",
		"run_completed",
	}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
}

// interruptingNode stands in for a person pressing Ctrl-C while it runs: it
// cancels the run's context, waits for the cancellation to reach it, and
// returns what a node that honours its context returns then. With finish
// set it ignores the cancellation instead and reports success, as a node
// that never looks at its context would.
type interruptingNode struct {
	name   string
	cancel context.CancelFunc
	finish bool
	calls  int
	// seenStep records the RunContext.Step of each execution, which is the
	// step number the run would have spent on it.
	seenStep []int
}

func (n *interruptingNode) Name() string { return n.name }

func (n *interruptingNode) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	n.calls++
	n.seenStep = append(n.seenStep, rc.Step)
	n.cancel()
	<-ctx.Done()
	if n.finish {
		return Result{
			Next: graph.End, Status: journal.StatusOK,
			Patch: state.Patch{Fix: &state.Fix{Attempts: 9}},
		}, nil
	}
	return Result{}, ctx.Err()
}

// TestDispatcher_CancellationEndsOrPausesTheRun is the Ctrl-C contract.
// By default a cancelled context ends the run as aborted, which Resume then
// refuses; that row is a guard for every caller of the dispatcher that never
// asked for anything else. With Options.PauseOnCancel the run is paused
// instead: the interrupted attempt is closed as one that will be run again,
// its Result is thrown away, and Resume re-runs the node at the next attempt
// and carries on to the end.
func TestDispatcher_CancellationEndsOrPausesTheRun(t *testing.T) {
	tests := []struct {
		name          string
		pause         bool
		finish        bool
		wantErr       []error
		wantStatus    state.RunStatus
		wantTimeline  []string
		wantResumeErr error
	}{
		{
			name:       "guard: by default the run ends as aborted",
			wantErr:    []error{graph.ErrNodeAborted, context.Canceled},
			wantStatus: state.RunStatusAborted,
			wantTimeline: []string{
				"run_started", "plan#1 started", "plan#1 finished ok -> code",
				"code#1 started", "code#1 finished aborted", "run_aborted",
			},
			wantResumeErr: graph.ErrRunEnded,
		},
		{
			name:       "PauseOnCancel pauses the run at the interrupted node",
			pause:      true,
			wantErr:    []error{graph.ErrInterrupted, context.Canceled},
			wantStatus: state.RunStatusPaused,
			wantTimeline: []string{
				"run_started", "plan#1 started", "plan#1 finished ok -> code",
				"code#1 started", "code#1 finished aborted -> code", "run_paused",
			},
		},
		{
			name:       "PauseOnCancel discards what a node finishing anyway returned",
			pause:      true,
			finish:     true,
			wantErr:    []error{graph.ErrInterrupted, context.Canceled},
			wantStatus: state.RunStatusPaused,
			wantTimeline: []string{
				"run_started", "plan#1 started", "plan#1 finished ok -> code",
				"code#1 started", "code#1 finished aborted -> code", "run_paused",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, config.Default(), "stop when asked")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			code := &interruptingNode{name: graph.NodeCode, cancel: cancel, finish: tt.finish}
			d := h.pausingDispatcher(t, tt.pause, ok(graph.NodePlan, graph.NodeCode), code)

			out, err := d.Run(ctx)
			for _, want := range tt.wantErr {
				if !errors.Is(err, want) {
					t.Errorf("Run() error = %v, want one wrapping %v", err, want)
				}
			}
			if out.Status != tt.wantStatus || out.ExitCode != graph.ExitAborted || out.Node != graph.NodeCode {
				t.Errorf("Outcome = {%v, exit %d, node %q}, want {%v, exit %d, node %q}",
					out.Status, out.ExitCode, out.Node, tt.wantStatus, graph.ExitAborted, graph.NodeCode)
			}
			if got := h.timeline(t); !equalStrings(got, tt.wantTimeline) {
				t.Errorf("timeline =\n  %v\nwant\n  %v", got, tt.wantTimeline)
			}
			man := h.manifest(t)
			if man.Status != tt.wantStatus || man.CurrentNode != graph.NodeCode {
				t.Errorf("manifest = {%v, %q}, want {%v, %q}", man.Status, man.CurrentNode, tt.wantStatus, graph.NodeCode)
			}
			if got := h.state(t).Fix.Attempts; got != 0 {
				t.Errorf("state.Fix.Attempts = %d, want 0: an interrupted node's patch must not land", got)
			}

			// A later process resumes, with a context nobody cancels.
			plan2 := ok(graph.NodePlan, graph.NodeCode)
			code2 := ok(graph.NodeCode, graph.End)
			out, err = h.pausingDispatcher(t, tt.pause, plan2, code2).Resume(context.Background())
			if tt.wantResumeErr != nil {
				if !errors.Is(err, tt.wantResumeErr) {
					t.Fatalf("Resume() error = %v, want one wrapping %v", err, tt.wantResumeErr)
				}
				if code2.calls != 0 {
					t.Errorf("code ran %d times resuming an ended run, want 0", code2.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resume() error = %v, want nil", err)
			}
			if out.Status != state.RunStatusCompleted {
				t.Fatalf("Resume() Outcome.Status = %v, want completed", out.Status)
			}
			if plan2.calls != 0 {
				t.Errorf("plan ran %d times on resume, want 0: it finished before the interruption", plan2.calls)
			}
			if code2.calls != 1 || code2.attempts[0] != 2 {
				t.Errorf("code ran %d times at attempts %v on resume, want 1 call at attempt 2", code2.calls, code2.attempts)
			}
			want := append(append([]string{}, tt.wantTimeline...),
				"run_resumed", "code#2 started", "code#2 finished ok -> END", "run_completed")
			if got := h.timeline(t); !equalStrings(got, want) {
				t.Errorf("timeline after resume =\n  %v\nwant\n  %v", got, want)
			}
		})
	}
}

// outlivesItsDeadline stands in for a node that runs past graph.node_timeout
// and is then caught by a Ctrl-C as well: it waits for its own deadline, and
// only once that has fired does it cancel the run's context and return. Both
// have happened by the time the dispatcher looks, in that order, with no
// sleep to make it so.
type outlivesItsDeadline struct {
	name   string
	cancel context.CancelFunc
}

func (n outlivesItsDeadline) Name() string { return n.name }

func (n outlivesItsDeadline) Run(ctx context.Context, _ *graph.RunContext) (graph.Result, error) {
	<-ctx.Done()
	n.cancel()
	return Result{Next: graph.End, Status: journal.StatusOK}, nil
}

// TestDispatcher_PauseOnCancelDoesNotExcuseATimeout: a node that outlives
// graph.node_timeout is stopped by a guard, not by the person running belay,
// and PauseOnCancel leaves that verdict alone — including when the person
// running belay also pressed Ctrl-C while the node overran. The timeout is
// checked first; a cancellation that arrives while a node outlives its
// deadline does not turn the dispatcher's verdict into a pause that resuming
// would only meet again.
func TestDispatcher_PauseOnCancelDoesNotExcuseATimeout(t *testing.T) {
	tests := []struct {
		name string
		node func(cancel context.CancelFunc) graph.Node
	}{
		{
			name: "the run's context left alone",
			node: func(context.CancelFunc) graph.Node {
				return &fakeNode{name: graph.NodePlan, steps: []nodeStep{{
					res:       Result{Next: graph.End, Status: journal.StatusOK},
					sleep:     150 * time.Millisecond,
					ignoreCtx: true,
				}}}
			},
		},
		{
			name: "the run's context cancelled after the deadline",
			node: func(cancel context.CancelFunc) graph.Node {
				return outlivesItsDeadline{name: graph.NodePlan, cancel: cancel}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Graph.NodeTimeout = config.NewDuration(20 * time.Millisecond)
			h := newRun(t, cfg, "too slow")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			out, err := h.pausingDispatcher(t, true, tt.node(cancel)).Run(ctx)
			if !errors.Is(err, graph.ErrNodeTimeout) {
				t.Fatalf("Run() error = %v, want one wrapping ErrNodeTimeout", err)
			}
			if out.Status != state.RunStatusAborted {
				t.Errorf("Outcome.Status = %v, want aborted", out.Status)
			}
			want := []string{"run_started", "plan#1 started", "plan#1 finished aborted", "run_aborted"}
			if got := h.timeline(t); !equalStrings(got, want) {
				t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
			}
		})
	}
}

// TestDispatcher_GuardsComeBeforeTheCancellationPause: a run the budget
// ceiling or graph.max_steps would end is ended, even when its context was
// cancelled before the next node. Pausing it instead would hand `belay
// resume` a run that can only meet the same guard again — and, for the budget,
// a person who pressed Ctrl-C would be told the run is waiting for them when
// it has in fact spent everything it may.
func TestDispatcher_GuardsComeBeforeTheCancellationPause(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Config
		seed    func(*state.Manifest)
		wantErr error
	}{
		{
			name:    "already over the budget ceiling",
			cfg:     budgetCfg(5.00, 0),
			seed:    func(m *state.Manifest) { m.Budget = state.Budget{LimitUSD: 5.00, SpentUSD: 7.50} },
			wantErr: belay.ErrBudgetExceeded,
		},
		{
			name: "already at graph.max_steps",
			cfg: func() config.Config {
				cfg := config.Default()
				cfg.Graph.MaxSteps = 3
				return cfg
			}(),
			seed:    func(m *state.Manifest) { m.Step = 3 },
			wantErr: graph.ErrMaxSteps,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, tt.cfg, "stop when asked")
			man := h.manifest(t)
			tt.seed(&man)
			if err := h.store.SaveManifest(man); err != nil {
				t.Fatalf("SaveManifest() error = %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			plan := ok(graph.NodePlan, graph.End)
			out, err := h.pausingDispatcher(t, true, plan).Run(ctx)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Run() error = %v, want one wrapping %v", err, tt.wantErr)
			}
			if errors.Is(err, graph.ErrInterrupted) {
				t.Errorf("Run() error = %v: the guard's verdict was turned into a pause", err)
			}
			if out.Status != state.RunStatusAborted || out.ExitCode != graph.ExitAborted {
				t.Errorf("Outcome = {%v, exit %d}, want {aborted, exit %d}", out.Status, out.ExitCode, graph.ExitAborted)
			}
			if plan.calls != 0 {
				t.Errorf("plan ran %d times, want 0", plan.calls)
			}
			want := []string{"run_started", "run_aborted"}
			if got := h.timeline(t); !equalStrings(got, want) {
				t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
			}
		})
	}
}

// TestDispatcher_PauseOnCancelStartsNoNodeOnceCancelled: once the caller
// has cancelled, a pausing dispatcher starts nothing more. The run pauses
// where it stands, with no attempt opened on a context that is already done
// and none thrown away, and Resume starts that node at its first attempt.
func TestDispatcher_PauseOnCancelStartsNoNodeOnceCancelled(t *testing.T) {
	h := newRun(t, config.Default(), "stopped before it began")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	plan := ok(graph.NodePlan, graph.End)
	out, err := h.pausingDispatcher(t, true, plan).Run(ctx)
	if !errors.Is(err, graph.ErrInterrupted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want one wrapping both ErrInterrupted and context.Canceled", err)
	}
	if out.Status != state.RunStatusPaused || out.ExitCode != graph.ExitAborted ||
		out.Node != graph.NodePlan || out.Steps != 0 {
		t.Errorf("Outcome = {%v, exit %d, node %q, %d steps}, want {paused, exit %d, node %q, 0 steps}",
			out.Status, out.ExitCode, out.Node, out.Steps, graph.ExitAborted, graph.NodePlan)
	}
	if plan.calls != 0 {
		t.Errorf("plan ran %d times after the run was cancelled, want 0", plan.calls)
	}
	if got, want := h.timeline(t), []string{"run_started", "run_paused"}; !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
	if m := h.manifest(t); m.Status != state.RunStatusPaused || m.CurrentNode != graph.NodePlan {
		t.Errorf("manifest = {%v, %q}, want {paused, %q}", m.Status, m.CurrentNode, graph.NodePlan)
	}

	plan2 := ok(graph.NodePlan, graph.End)
	if _, err := h.pausingDispatcher(t, true, plan2).Resume(context.Background()); err != nil {
		t.Fatalf("Resume() error = %v, want nil", err)
	}
	if plan2.calls != 1 || plan2.attempts[0] != 1 {
		t.Errorf("plan ran %d times at attempts %v on resume, want 1 call at attempt 1", plan2.calls, plan2.attempts)
	}
}

// downgradeToVersion1 rewrites a run document as belay v0.1.x wrote it:
// schema_version 1, and no requires list, which is the shape those binaries
// read and the only shape they will resume.
func downgradeToVersion1(t *testing.T, path string) {
	t.Helper()
	editRunDocument(t, path, func(doc map[string]json.RawMessage) {
		doc["schema_version"] = json.RawMessage(`1`)
		delete(doc, "requires")
	})
}

// editRunDocument applies edit to the JSON object at path, leaving every
// other key exactly as it was written.
func editRunDocument(t *testing.T, path string, edit func(map[string]json.RawMessage)) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a path the test's own harness built under t.TempDir()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	edit(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// schemaVersionOnDisk reads a run document's schema_version raw. A loader
// would migrate the document and report the version it produced, not the one
// a crash at this moment would leave behind.
func schemaVersionOnDisk(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a path the test's own harness built under t.TempDir()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return probe.SchemaVersion
}

// TestDispatcher_RefusesARunItMayNotReadBeforeWritingToIt is ADR-0011's
// promise, at the dispatcher: a run requiring a capability this belay does not
// have is refused before anything is written to it.
//
// A capability may be declared in state.json alone, and the dispatcher read
// the manifest, opened the journal, healed it and appended to it long before
// it ever loaded state.json. On a run a crash had left mid-node that wrote a
// node_finished into a run this belay was never allowed to read — and, once
// the loop reached a guard, run_aborted and an aborted manifest, ending for
// good a run whose only fault was having been written by a newer belay.
func TestDispatcher_RefusesARunItMayNotReadBeforeWritingToIt(t *testing.T) {
	tests := []struct {
		name    string
		seed    []journal.Record
		records int
	}{
		{name: "a fresh run: the journal is not even created"},
		{
			name: "a run a crash left inside a node: nothing is healed",
			seed: []journal.Record{
				{Event: journal.EventRunStarted},
				{Event: journal.EventNodeStarted, Node: graph.NodePlan, Attempt: 1},
			},
			records: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, config.Default(), "add tests")
			if len(tt.seed) > 0 {
				h.seedJournal(t, tt.seed...)
			}
			editRunDocument(t, h.layout.StatePath(), func(doc map[string]json.RawMessage) {
				doc["requires"] = json.RawMessage(`["output.branch"]`)
			})

			plan := ok(graph.NodePlan, graph.End)
			out, err := h.dispatcher(t, plan).Resume(context.Background())
			if !errors.Is(err, state.ErrUnknownCapability) {
				t.Fatalf("Resume() error = %v, want one wrapping state.ErrUnknownCapability", err)
			}
			if out.ExitCode != graph.ExitFailed {
				t.Errorf("exit code = %d, want %d", out.ExitCode, graph.ExitFailed)
			}
			if plan.calls != 0 {
				t.Errorf("plan ran %d times in a run belay may not read, want 0", plan.calls)
			}

			if tt.records == 0 {
				if _, err := os.Stat(h.layout.JournalPath()); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("the journal exists (%v); belay wrote to a run it refused", err)
				}
			} else if got := len(h.records(t)); got != tt.records {
				t.Errorf("the journal has %d records, want the %d it had; belay wrote to a run it refused:\n  %v",
					got, tt.records, h.timeline(t))
			}
			if m := h.manifest(t); m.Status != state.RunStatusRunning {
				t.Errorf("manifest status = %v, want it left as %v", m.Status, state.RunStatusRunning)
			}
		})
	}
}

// TestDispatcher_SavesTheManifestAtTheCurrentVersionBeforeItWritesAnything is
// the other half of ADR-0011's promise. A version-1 run resumed here has both
// its documents rewritten at version 2 as they are saved, and whichever goes
// first decides what belay v0.1.x makes of a crash in between: a version-1
// manifest over a version-2 state.json is a run it accepts, resumes, and
// writes into. So the manifest goes first, before the journal is even opened.
//
// The journal is made unopenable to prove the ordering: the dispatch fails at
// the first write it attempts, and the manifest is already at version 2.
func TestDispatcher_SavesTheManifestAtTheCurrentVersionBeforeItWritesAnything(t *testing.T) {
	h := newRun(t, config.Default(), "add tests")
	downgradeToVersion1(t, h.layout.StatePath())
	downgradeToVersion1(t, h.layout.ManifestPath())
	// A directory where the journal goes: journal.Open cannot write a byte,
	// which stands in for a crash at belay's first write to this run.
	if err := os.MkdirAll(h.layout.JournalPath(), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	plan := ok(graph.NodePlan, graph.End)
	if _, err := h.dispatcher(t, plan).Resume(context.Background()); err == nil {
		t.Fatal("Resume() = nil with an unopenable journal")
	}
	if plan.calls != 0 {
		t.Errorf("plan ran %d times, want 0", plan.calls)
	}
	if got := schemaVersionOnDisk(t, h.layout.ManifestPath()); got != state.ManifestSchemaVersion {
		t.Errorf("manifest.json is at schema version %d before belay's first write to the run, want %d",
			got, state.ManifestSchemaVersion)
	}
}

// TestDispatcher_AnInterruptedAttemptDoesNotConsumeAStep pins what an
// interruption costs the run: nothing. Options.PauseOnCancel promises the
// attempt number, the discarded Result and the re-run "a kill at the same
// moment would have produced", and a kill costs no step — the crashed process
// never got to write the manifest.
//
// A step is not bookkeeping. graph.max_steps is counted in them, so a person
// who presses Ctrl-C twice has fewer steps left to finish the run with than
// one who was killed twice; and a node's evidence directory is named after
// its step, so the re-run of an interrupted node would write beside the
// attempt it replaces rather than over it.
func TestDispatcher_AnInterruptedAttemptDoesNotConsumeAStep(t *testing.T) {
	t.Run("Ctrl-C inside the node", func(t *testing.T) {
		h := newRun(t, config.Default(), "stop when asked")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		code := &interruptingNode{name: graph.NodeCode, cancel: cancel}
		_, err := h.pausingDispatcher(t, true, ok(graph.NodePlan, graph.NodeCode), code).Run(ctx)
		if !errors.Is(err, graph.ErrInterrupted) {
			t.Fatalf("Run() error = %v, want one wrapping ErrInterrupted", err)
		}
		if got := code.seenStep; len(got) != 1 || got[0] != 2 {
			t.Fatalf("the interrupted attempt ran at steps %v, want [2]", got)
		}
		if got := h.manifest(t).Step; got != 1 {
			t.Errorf("manifest.Step = %d after an interrupted attempt, want 1: only plan finished", got)
		}

		code2 := ok(graph.NodeCode, graph.End)
		if _, err := h.pausingDispatcher(t, true, ok(graph.NodePlan, graph.NodeCode), code2).
			Resume(context.Background()); err != nil {
			t.Fatalf("Resume() error = %v, want nil", err)
		}
		if got := code2.seenStep; len(got) != 1 || got[0] != 2 {
			t.Errorf("the re-run ran at steps %v, want [2]: the same step the interrupted attempt had", got)
		}
	})

	// The same run as a kill leaves it: plan finished and counted, code
	// opened and never closed. This is the behaviour the row above must
	// match, and it is a guard, not a change.
	t.Run("a kill at the same moment", func(t *testing.T) {
		h := newRun(t, config.Default(), "survive a kill")
		h.seedJournal(t,
			journal.Record{Event: journal.EventRunStarted},
			journal.Record{Event: journal.EventNodeStarted, Node: graph.NodePlan, Attempt: 1},
			journal.Record{
				Event: journal.EventNodeFinished, Node: graph.NodePlan, Attempt: 1,
				Status: journal.StatusOK, Next: graph.NodeCode, Usage: &journal.Usage{},
			},
			journal.Record{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1},
		)
		man := h.manifest(t)
		man.Step = 1
		man.CurrentNode = graph.NodeCode
		if err := h.store.SaveManifest(man); err != nil {
			t.Fatalf("SaveManifest: %v", err)
		}

		code := ok(graph.NodeCode, graph.End)
		if _, err := h.dispatcher(t, ok(graph.NodePlan, graph.NodeCode), code).
			Resume(context.Background()); err != nil {
			t.Fatalf("Resume() error = %v, want nil", err)
		}
		if got := code.seenStep; len(got) != 1 || got[0] != 2 {
			t.Errorf("the re-run after a kill ran at steps %v, want [2]", got)
		}
	})
}
