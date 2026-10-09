package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/procgroup"
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

// lastNodeFinished returns the run's final node_finished record, which is
// where a node execution's Usage is journalled.
func (h *harness) lastNodeFinished(t *testing.T) journal.Record {
	t.Helper()
	recs := h.records(t)
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Event == journal.EventNodeFinished {
			return recs[i]
		}
	}
	t.Fatal("journal holds no node_finished record")
	return journal.Record{}
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
// Entry: where a run with no history starts.
// ---------------------------------------------------------------------------

// entryDispatcher wires nodes into a Dispatcher that starts a run with an
// empty journal at entry rather than at journal.FirstNode.
func (h *harness) entryDispatcher(t *testing.T, entry string, nodes ...graph.Node) *graph.Dispatcher {
	t.Helper()
	reg := graph.NewRegistry()
	reg.MustRegister(nodes...)
	d, err := graph.NewDispatcher(graph.Options{
		Store: h.store, Registry: reg, Config: h.cfg, Entry: entry,
		Now: func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	return d
}

// TestDispatcher_StartsAnEmptyJournalAtEntry covers the run a fanout
// candidate performs: a graph of code, write, test, fix and review, with no
// plan node and no approve gate, because the parent has already planned and
// been approved. journal.FirstNode is "plan", so without Entry such a run
// ends on its first step with an unknown node.
func TestDispatcher_StartsAnEmptyJournalAtEntry(t *testing.T) {
	h := newRun(t, config.Default(), "start where the caller says")
	code := ok(graph.NodeCode, graph.NodeTest)
	testNode := ok(graph.NodeTest, graph.End)

	out, err := h.entryDispatcher(t, graph.NodeCode, code, testNode).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted || out.ExitCode != graph.ExitOK {
		t.Fatalf("Outcome = {%v, exit %d}, want {completed, exit %d} (note %q)",
			out.Status, out.ExitCode, graph.ExitOK, out.Note)
	}
	want := []string{
		"run_started",
		"code#1 started", "code#1 finished ok -> test",
		"test#1 started", "test#1 finished ok -> END",
		"run_completed",
	}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
	if code.attempts[0] != 1 {
		t.Errorf("the entry node ran at attempt %d, want 1", code.attempts[0])
	}
}

// TestDispatcher_EntryNeverOverridesAJournalWithHistory is the other half:
// where a run with history resumes is journal.ResolveStart's decision alone.
// Applying Entry unconditionally would re-run the entry node on every resume
// of every candidate, paying for work the journal says is already done.
func TestDispatcher_EntryNeverOverridesAJournalWithHistory(t *testing.T) {
	h := newRun(t, config.Default(), "resume where the journal says")
	h.seedJournal(t,
		journal.Record{Event: journal.EventRunStarted},
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1},
		journal.Record{
			Event: journal.EventNodeFinished, Node: graph.NodeCode, Attempt: 1,
			Status: journal.StatusOK, Next: graph.NodeTest,
		},
	)

	code := ok(graph.NodeCode, graph.NodeTest)
	testNode := ok(graph.NodeTest, graph.End)

	out, err := h.entryDispatcher(t, graph.NodeCode, code, testNode).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed (note %q)", out.Status, out.Note)
	}
	if code.calls != 0 {
		t.Errorf("the entry node ran %d times, want 0: the journal already records it finished", code.calls)
	}
	if testNode.calls != 1 {
		t.Errorf("test ran %d times, want 1", testNode.calls)
	}
}

// TestDispatcher_EntryHoldsUntilANodeHasStarted covers the window between a
// candidate run's first record and its first node. A Ctrl-C there leaves
// run_started and run_paused, and a kill leaves run_started alone: history,
// but none that names a node. journal.ResolveStart can only answer
// journal.FirstNode for such a journal, a node a candidate's registry does
// not have, so a resume that let the history outvote Entry ended the
// candidate failed, for good, over an interruption that cost nothing.
func TestDispatcher_EntryHoldsUntilANodeHasStarted(t *testing.T) {
	tests := []struct {
		name    string
		history []journal.Record
	}{
		{
			name: "a Ctrl-C before the first node started",
			history: []journal.Record{
				{Event: journal.EventRunStarted},
				{Event: journal.EventRunPaused, Note: "interrupted at \"code\"; use `belay resume` to continue"},
			},
		},
		{
			name:    "a kill before the first node started",
			history: []journal.Record{{Event: journal.EventRunStarted}},
		},
		{
			name: "a Ctrl-C before the first node started, resumed and interrupted again",
			history: []journal.Record{
				{Event: journal.EventRunStarted},
				{Event: journal.EventRunPaused, Note: "interrupted"},
				{Event: journal.EventRunResumed},
				{Event: journal.EventRunPaused, Note: "interrupted"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, config.Default(), "start where the caller says, however often it is interrupted")
			h.seedJournal(t, tt.history...)
			code := ok(graph.NodeCode, graph.NodeTest)
			testNode := ok(graph.NodeTest, graph.End)

			out, err := h.entryDispatcher(t, graph.NodeCode, code, testNode).Resume(context.Background())
			if err != nil {
				t.Fatalf("Resume() error = %v, want nil (note %q)", err, out.Note)
			}
			if out.Status != state.RunStatusCompleted {
				t.Fatalf("Outcome.Status = %v, want completed (note %q)", out.Status, out.Note)
			}
			if code.calls != 1 || code.attempts[0] != 1 {
				t.Errorf("the entry node ran %d times at attempts %v, want once at attempt 1", code.calls, code.attempts)
			}
			if got := h.timeline(t); !slices.Contains(got, "code#1 started") {
				t.Errorf("timeline = %v, want the entry node started at attempt 1", got)
			}
		})
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

// TestDispatcher_HealChargesAnAttemptItNeverSawFinish is ADR-0007's rule for
// the one spend belay cannot know. A process killed inside a node never
// reported what that attempt cost, and the backend may have spent any amount
// before the kill. heal used to close the attempt as free, so a run killed
// during a paid step and resumed paid for that step twice while the ledger
// counted it once.
//
// The attempt is now charged the most any single step of this run has
// recorded, marked estimated, so the total never claims to be exact. With
// nothing recorded yet there is no evidence to charge from, and the attempt
// is charged nothing rather than a number belay made up.
func TestDispatcher_HealChargesAnAttemptItNeverSawFinish(t *testing.T) {
	planSpend := belay.Usage{InputTokens: 1000, OutputTokens: 200, USD: 3.00}
	codeSpend := belay.Usage{InputTokens: 10, OutputTokens: 5, USD: 1.00}
	tests := []struct {
		name       string
		seed       []journal.Record
		wantCharge belay.Usage
		wantTotal  belay.Usage
	}{
		{
			name: "killed inside a step after the run had spent",
			seed: []journal.Record{
				{Event: journal.EventRunStarted},
				{Event: journal.EventNodeStarted, Node: graph.NodePlan, Attempt: 1},
				{
					Event: journal.EventNodeFinished, Node: graph.NodePlan, Attempt: 1,
					Status: journal.StatusOK, Next: graph.NodeCode,
					Usage: &journal.Usage{InputTokens: 1000, OutputTokens: 200, USD: 3.00},
				},
				{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1},
			},
			wantCharge: belay.Usage{InputTokens: 1000, OutputTokens: 200, USD: 3.00, Estimated: true},
			wantTotal: belay.Usage{
				InputTokens:  planSpend.InputTokens*2 + codeSpend.InputTokens,
				OutputTokens: planSpend.OutputTokens*2 + codeSpend.OutputTokens,
				USD:          planSpend.USD*2 + codeSpend.USD, Estimated: true,
			},
		},
		{
			name: "killed inside the first step, before anything was spent",
			seed: []journal.Record{
				{Event: journal.EventRunStarted},
				{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1},
			},
			wantCharge: belay.Usage{},
			wantTotal:  codeSpend,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, budgetCfg(100.00, 0), "survive a kill -9")
			h.seedJournal(t, tt.seed...)

			code := &fakeNode{name: graph.NodeCode, steps: []nodeStep{{
				res: Result{Next: graph.End, Status: journal.StatusOK, Usage: codeSpend},
			}}}
			out, err := h.dispatcher(t, ok(graph.NodePlan, graph.NodeCode), code).Run(context.Background())
			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}

			var healed journal.Record
			for _, r := range h.records(t) {
				if r.Interrupted() {
					healed = r
				}
			}
			if healed.Usage == nil {
				t.Fatalf("no interrupted attempt with a usage block in the journal: %v", h.timeline(t))
			}
			got := belay.Usage{
				InputTokens: healed.Usage.InputTokens, OutputTokens: healed.Usage.OutputTokens,
				USD: healed.Usage.USD, Estimated: healed.Usage.Estimated,
			}
			if got != tt.wantCharge {
				t.Errorf("healed attempt charged %+v, want %+v", got, tt.wantCharge)
			}
			if !closeTo(out.Usage.USD, tt.wantTotal.USD) || out.Usage.InputTokens != tt.wantTotal.InputTokens ||
				out.Usage.OutputTokens != tt.wantTotal.OutputTokens || out.Usage.Estimated != tt.wantTotal.Estimated {
				t.Errorf("Outcome.Usage = %+v, want %+v", out.Usage, tt.wantTotal)
			}
		})
	}
}

// TestDispatcher_GivesANodeItsEvidenceDirectoryToRecordProcessesIn: every
// process a node starts goes through internal/exec, which records it in the
// directory on its context so that a belay killed with SIGKILL can find the
// process again. The dispatcher is what puts it there, and it must be this
// execution's own evidence directory.
func TestDispatcher_GivesANodeItsEvidenceDirectoryToRecordProcessesIn(t *testing.T) {
	h := newRun(t, config.Default(), "record what it starts")
	var seen string
	node := &contextReadingNode{name: graph.NodePlan, read: func(ctx context.Context) {
		seen = procgroup.DirFrom(ctx)
	}}

	if _, err := h.dispatcher(t, node).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	want, err := h.layout.NodeDir(1, graph.NodePlan)
	if err != nil {
		t.Fatalf("NodeDir: %v", err)
	}
	if seen != want {
		t.Errorf("the node's context carried %q, want its evidence directory %q", seen, want)
	}
}

// contextReadingNode hands its context to read before finishing, so a test
// can assert what the dispatcher put on it.
type contextReadingNode struct {
	name string
	read func(context.Context)
}

func (n *contextReadingNode) Name() string { return n.name }

func (n *contextReadingNode) Run(ctx context.Context, _ *graph.RunContext) (graph.Result, error) {
	n.read(ctx)
	return Result{Next: graph.End, Status: journal.StatusOK}, nil
}

// TestDispatcher_HealKillsProcessGroupsAKilledRunLeftRunning is the whole
// point of recording a process group: a belay killed with SIGKILL leaves the
// agent and the test suite running, in groups the terminal's Ctrl-C never
// reaches, and nothing stopped them. Resuming the run kills them.
//
// The run below is in the state a kill -9 leaves: a node_started nothing
// closed, and a live process group recorded in that execution's evidence
// directory.
func TestDispatcher_HealKillsProcessGroupsAKilledRunLeftRunning(t *testing.T) {
	h := newRun(t, config.Default(), "clean up after a kill -9")
	h.seedJournal(t,
		journal.Record{Event: journal.EventRunStarted},
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodeTest, Attempt: 1},
	)

	// The process the dead belay had started, recorded exactly as
	// internal/exec records one.
	sleeper := exec.Command("sleep", "60")
	sleeper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sleeper.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		if err := sleeper.Wait(); err != nil {
			t.Logf("sleeper exited: %v", err) // expected: killed
		}
		close(exited)
	}()
	t.Cleanup(func() {
		if err := syscall.Kill(-sleeper.Process.Pid, syscall.SIGKILL); err != nil &&
			!errors.Is(err, syscall.ESRCH) {
			t.Errorf("clean up process group: %v", err)
		}
		<-exited
	})

	nodeDir, err := h.layout.NodeDir(1, graph.NodeTest)
	if err != nil {
		t.Fatalf("NodeDir: %v", err)
	}
	tracked, err := procgroup.Track(nodeDir, []string{"go", "test", "./..."})
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := tracked.Started(sleeper.Process.Pid); err != nil {
		t.Fatalf("Started: %v", err)
	}

	out, err := h.dispatcher(t, ok(graph.NodeTest, graph.End)).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed", out.Status)
	}

	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("process %d is still running: resuming the run did not kill the group the kill -9 left behind",
			sleeper.Process.Pid)
	}

	// The record is spent, so a later resume does not act on a pid that by
	// then may belong to somebody else.
	left, err := filepath.Glob(filepath.Join(nodeDir, "pgroup-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("records left behind: %v", left)
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
	// The node saw the state its crashed attempt had already written. That
	// is only because this disk holds no snapshot, as a crash recorded by a
	// belay that took none would leave it: a run this belay records hands the
	// re-run the snapshot its first attempt took instead (see
	// TestDispatcher_CrashReRunDoesNotCountTheCrashedAttemptTwice).
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

func TestDispatcher_BudgetExceededStopsTheRunBeforeTheNodeRuns(t *testing.T) {
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
		t.Errorf("plan ran %d times, want 0: the budget must stop the run before a node executes", plan.calls)
	}
	// Paused, not ended: raising the ceiling and resuming continues this
	// run. The exit code is unchanged.
	if out.Status != state.RunStatusPaused || out.ExitCode != graph.ExitAborted {
		t.Errorf("Outcome = {%v, exit %d}, want {paused, exit %d}", out.Status, out.ExitCode, graph.ExitAborted)
	}
	// No node_started was ever written, so nothing is left open.
	want := []string{"run_started", "run_paused"}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
	if h.manifest(t).Status != state.RunStatusPaused {
		t.Errorf("manifest.Status = %v, want paused", h.manifest(t).Status)
	}
	// The note names the settings to raise, for anyone reading the journal.
	if !strings.Contains(out.Note, "budget.max_usd") {
		t.Errorf("Outcome.Note = %q, want it to name the setting to raise", out.Note)
	}
}

// unpricedNode is a fakeNode that calls no agent, so the run's budget
// ceiling has nothing to say about it.
type unpricedNode struct{ *fakeNode }

func (unpricedNode) SpendsNothing() {}

// TestDispatcher_JoinAndPromoteRunAfterTheCeilingIsCrossed is the fanout
// scenario: three candidates split a $3.00 ceiling, two of them cross their
// envelopes, and the parent ledger settles above max_usd with a winner
// already selected and already paid for. The nodes that deliver it — join,
// which compares numbers other nodes produced, and promote, which runs git —
// cannot spend anything, so stopping them at the ceiling saves nothing and
// throws the work away.
func TestDispatcher_JoinAndPromoteRunAfterTheCeilingIsCrossed(t *testing.T) {
	tests := []struct {
		name       string
		unpriced   bool
		wantStatus state.RunStatus
		wantCalls  int
		wantErr    error
		wantSteps  []string
	}{
		{
			name:       "a run holding a paid-for winner delivers it",
			unpriced:   true,
			wantStatus: state.RunStatusCompleted,
			wantCalls:  1,
			wantSteps: []string{
				"run_started",
				"join#1 started", "join#1 finished ok -> promote",
				"promote#1 started", "promote#1 finished ok -> END",
				"run_completed",
			},
		},
		{
			name:       "a node that could spend is still stopped at the ceiling",
			unpriced:   false,
			wantStatus: state.RunStatusPaused,
			wantCalls:  0,
			wantErr:    belay.ErrBudgetExceeded,
			wantSteps:  []string{"run_started", "run_paused"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, budgetCfg(3.00, 0), "deliver the winner")
			man := h.manifest(t)
			man.Budget = state.Budget{LimitUSD: 3.00, SpentUSD: 3.10, TokensIn: 100, TokensOut: 50}
			if err := h.store.SaveManifest(man); err != nil {
				t.Fatalf("SaveManifest() error = %v", err)
			}

			join := ok(graph.NodeJoin, graph.NodePromote)
			promote := ok(graph.NodePromote, graph.End)
			nodes := []graph.Node{join, promote}
			if tt.unpriced {
				nodes = []graph.Node{unpricedNode{fakeNode: join}, unpricedNode{fakeNode: promote}}
			}

			out, err := h.entryDispatcher(t, graph.NodeJoin, nodes...).Run(context.Background())

			if tt.wantErr == nil && err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Run() error = %v, want one wrapping %v", err, tt.wantErr)
			}
			if out.Status != tt.wantStatus {
				t.Errorf("Outcome.Status = %v, want %v (note %q)", out.Status, tt.wantStatus, out.Note)
			}
			if join.calls != tt.wantCalls || promote.calls != tt.wantCalls {
				t.Errorf("join ran %d times and promote %d, want %d each",
					join.calls, promote.calls, tt.wantCalls)
			}
			if got := h.timeline(t); !equalStrings(got, tt.wantSteps) {
				t.Errorf("timeline =\n  %v\nwant\n  %v", got, tt.wantSteps)
			}
			// Whatever the ceiling did, the run's spend is reported as it
			// stands: an unpriced node adds nothing to it and hides nothing.
			if !closeTo(out.Usage.USD, 3.10) {
				t.Errorf("Outcome.Usage.USD = %v, want 3.10", out.Usage.USD)
			}
		})
	}
}

// TestDispatcher_ANodeRefusingOnTheBudgetCeilingPausesTheRun covers the one
// place a node refuses because of money rather than because of the work: the
// fanout node's pre-check, which refuses to buy N candidates it can see the
// ceiling will not cover. Ending the run for that would be the mistake the
// dispatcher's own guards stopped making — the money already spent and the
// work already done would be out of reach, and the only way on a new run
// from nothing, spending it all again.
func TestDispatcher_ANodeRefusingOnTheBudgetCeilingPausesTheRun(t *testing.T) {
	h := newRun(t, budgetCfg(1.00, 0), "refuse what you cannot pay for")

	refused := &belay.BudgetError{SpentUSD: 0.40, LimitUSD: 1.00}
	fanoutNode := &fakeNode{name: graph.NodeFanout, steps: []nodeStep{
		{
			// It had already spent something before it refused, and
			// money a backend reported is gone whatever the verdict.
			res: Result{Usage: belay.Usage{InputTokens: 80, OutputTokens: 20, USD: 0.40}},
			err: fmt.Errorf("fanout: 3 candidates need $3.00: %w", refused),
		},
		{res: Result{Next: graph.End, Status: journal.StatusOK}},
	}}

	out, err := h.entryDispatcher(t, graph.NodeFanout, fanoutNode).Run(context.Background())

	if !errors.Is(err, belay.ErrBudgetExceeded) {
		t.Fatalf("Run() error = %v, want one wrapping belay.ErrBudgetExceeded", err)
	}
	// Paused rather than failed, and stopped by a limit rather than by the
	// graph, so the exit code is the one every other limit uses.
	if out.Status != state.RunStatusPaused || out.ExitCode != graph.ExitAborted {
		t.Errorf("Outcome = {%v, exit %d}, want {paused, exit %d}", out.Status, out.ExitCode, graph.ExitAborted)
	}
	if !strings.Contains(out.Note, "budget.max_usd") {
		t.Errorf("Outcome.Note = %q, want it to name the setting to raise", out.Note)
	}
	// The attempt is closed as one to run again — it names its own node —
	// so resuming re-runs it rather than resolving to no node at all.
	want := []string{"run_started", "fanout#1 started", "fanout#1 finished aborted -> fanout", "run_paused"}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
	if got := h.lastNodeFinished(t); got.Usage == nil || !closeTo(got.Usage.USD, 0.40) {
		t.Errorf("the journalled usage is %+v, want the $0.40 the node reported spending", got.Usage)
	}

	// Raising the ceiling and resuming continues this run, keeping what it
	// has already spent: limits are re-read on the resume.
	h.cfg = budgetCfg(10.00, 0)
	out, err = h.entryDispatcher(t, graph.NodeFanout, fanoutNode).Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status after resume = %v, want completed (note %q)", out.Status, out.Note)
	}
	if fanoutNode.attempts[1] != 2 {
		t.Errorf("the re-run ran at attempt %d, want 2", fanoutNode.attempts[1])
	}
	if !closeTo(out.Usage.USD, 0.40) {
		t.Errorf("Outcome.Usage.USD after resume = %v, want the 0.40 the refused attempt spent", out.Usage.USD)
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

// TestDispatcher_ResumeRebuildsTheLedgerFromTheJournal closes the stale
// window ADR-0007's ledger used to leave open. The dispatcher appends a
// node's node_finished, Usage included, and only then saves the manifest's
// budget block. A crash between the two left a manifest that had never heard
// of the node's spend, and a resume that restored the ledger from that
// manifest let the run spend it again. The journal is the durable record, so
// the ledger is rebuilt from it.
func TestDispatcher_ResumeRebuildsTheLedgerFromTheJournal(t *testing.T) {
	journalled := belay.Usage{InputTokens: 1000, OutputTokens: 200, USD: 3.00}
	tests := []struct {
		name     string
		maxUSD   float64
		wantCode int
		wantUSD  float64
		wantErr  error
	}{
		{
			name:     "a resume counts the spend the journal recorded",
			maxUSD:   100.00,
			wantCode: 1,
			wantUSD:  3.00 + 1.00,
		},
		{
			name:     "a resume meets the ceiling the journal says the run reached",
			maxUSD:   2.00,
			wantCode: 0,
			wantUSD:  3.00,
			wantErr:  belay.ErrBudgetExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, budgetCfg(tt.maxUSD, 0), "count what the journal says")
			// The manifest's budget block is still zero: the previous
			// process died after journalling plan's spend and before
			// saving the manifest.
			h.seedJournal(t,
				journal.Record{Event: journal.EventRunStarted},
				journal.Record{Event: journal.EventNodeStarted, Node: graph.NodePlan, Attempt: 1},
				journal.Record{
					Event: journal.EventNodeFinished, Node: graph.NodePlan, Attempt: 1,
					Status: journal.StatusOK, Next: graph.NodeCode,
					Usage: &journal.Usage{
						InputTokens: journalled.InputTokens, OutputTokens: journalled.OutputTokens,
						USD: journalled.USD,
					},
				},
			)

			code := &fakeNode{name: graph.NodeCode, steps: []nodeStep{{
				res: Result{
					Next: graph.End, Status: journal.StatusOK,
					Usage: belay.Usage{InputTokens: 10, OutputTokens: 5, USD: 1.00},
				},
			}}}
			out, err := h.dispatcher(t, ok(graph.NodePlan, graph.NodeCode), code).Resume(context.Background())

			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Resume() error = %v, want one wrapping %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && err != nil {
				t.Fatalf("Resume() error = %v, want nil", err)
			}
			if code.calls != tt.wantCode {
				t.Errorf("code ran %d times, want %d", code.calls, tt.wantCode)
			}
			if !closeTo(out.Usage.USD, tt.wantUSD) {
				t.Errorf("Outcome.Usage.USD = %v, want %v: the spend the journal recorded before the crash was lost",
					out.Usage.USD, tt.wantUSD)
			}
			if got := h.manifest(t).Budget; !closeTo(got.SpentUSD, tt.wantUSD) {
				t.Errorf("manifest.Budget.SpentUSD = %v, want %v", got.SpentUSD, tt.wantUSD)
			}
		})
	}
}

// TestDispatcher_RecordsWhatARejectedNodeSpent is the first half of the
// budget-integrity contract ADR-0007 makes: money a backend reports having
// spent is spent, whatever the dispatcher then decides about the node's
// verdict.
//
// Every rejected path used to journal a zero Usage and never call
// Ledger.Record, so an agent that burned $2.50 and then timed out, was
// cancelled, or reported an error cost the run nothing on paper. The run
// could then be resumed and spend its whole ceiling over again.
func TestDispatcher_RecordsWhatARejectedNodeSpent(t *testing.T) {
	spent := belay.Usage{InputTokens: 1000, OutputTokens: 500, USD: 2.50}
	timeoutCfg := func() config.Config {
		cfg := config.Default()
		cfg.Graph.NodeTimeout = config.NewDuration(20 * time.Millisecond)
		return cfg
	}

	tests := []struct {
		name  string
		cfg   config.Config
		pause bool
		node  func(cancel context.CancelFunc) graph.Node
	}{
		{
			name: "the node returned an error after the backend had spent",
			cfg:  config.Default(),
			node: func(context.CancelFunc) graph.Node {
				return &fakeNode{name: graph.NodePlan, steps: []nodeStep{{
					res: Result{Usage: spent},
					err: errors.New("the agent CLI reported an error"),
				}}}
			},
		},
		{
			name: "the node returned an unusable Result after the backend had spent",
			cfg:  config.Default(),
			node: func(context.CancelFunc) graph.Node {
				return &fakeNode{name: graph.NodePlan, steps: []nodeStep{{
					res: Result{Usage: spent},
				}}}
			},
		},
		{
			name: "the node outlived graph.node_timeout after the backend had spent",
			cfg:  timeoutCfg(),
			node: func(context.CancelFunc) graph.Node {
				return &fakeNode{name: graph.NodePlan, steps: []nodeStep{{
					res:       Result{Next: graph.End, Status: journal.StatusOK, Usage: spent},
					sleep:     150 * time.Millisecond,
					ignoreCtx: true,
				}}}
			},
		},
		{
			name: "the run was cancelled after the backend had spent",
			cfg:  config.Default(),
			node: func(cancel context.CancelFunc) graph.Node {
				return &interruptingNode{name: graph.NodePlan, cancel: cancel, usage: spent}
			},
		},
		{
			name:  "the run was paused by a cancellation after the backend had spent",
			cfg:   config.Default(),
			pause: true,
			node: func(cancel context.CancelFunc) graph.Node {
				return &interruptingNode{name: graph.NodePlan, cancel: cancel, usage: spent}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, tt.cfg, "spend, then fall over")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			out, _ := h.pausingDispatcher(t, tt.pause, tt.node(cancel)).Run(ctx)

			if !closeTo(out.Usage.USD, spent.USD) {
				t.Errorf("Outcome.Usage.USD = %v, want %v: the spend the node reported vanished",
					out.Usage.USD, spent.USD)
			}
			if out.Usage.InputTokens != spent.InputTokens || out.Usage.OutputTokens != spent.OutputTokens {
				t.Errorf("Outcome.Usage tokens = in %d / out %d, want in %d / out %d",
					out.Usage.InputTokens, out.Usage.OutputTokens, spent.InputTokens, spent.OutputTokens)
			}
			if got := h.manifest(t).Budget; !closeTo(got.SpentUSD, spent.USD) {
				t.Errorf("manifest.Budget.SpentUSD = %v, want %v: a resume would restore the wrong total",
					got.SpentUSD, spent.USD)
			}
			// The journal is the durable source of truth the ledger is
			// rebuilt from, so the record must carry the figure too.
			rec := h.lastNodeFinished(t)
			if rec.Usage == nil || !closeTo(rec.Usage.USD, spent.USD) {
				t.Errorf("node_finished usage = %+v, want USD %v", rec.Usage, spent.USD)
			}
		})
	}
}

// TestDispatcher_ChargesAnAttemptCutShortThatReportedNothing is the other
// half of the budget-integrity contract: a node a Ctrl-C or graph.node_timeout
// stops part-way usually has nothing to report, because the agent CLI that
// was killed never printed its bill. Recording the zero it reported let an
// interrupted paid call cost nothing, so interrupting and resuming spent past
// the ceiling with the ledger none the wiser, where a kill at the same moment
// was charged an estimate by heal. Both now charge the same estimate: the
// costliest step the run has paid for, marked estimated.
func TestDispatcher_ChargesAnAttemptCutShortThatReportedNothing(t *testing.T) {
	planSpend := belay.Usage{InputTokens: 1000, OutputTokens: 200, USD: 0.40}
	timeoutCfg := func() config.Config {
		cfg := config.Default()
		cfg.Graph.NodeTimeout = config.NewDuration(20 * time.Millisecond)
		return cfg
	}
	tests := []struct {
		name  string
		cfg   config.Config
		pause bool
		node  func(cancel context.CancelFunc) graph.Node
	}{
		{
			name: "the node outlived graph.node_timeout and reported nothing",
			cfg:  timeoutCfg(),
			node: func(context.CancelFunc) graph.Node {
				return &fakeNode{name: graph.NodeCode, steps: []nodeStep{{sleep: time.Minute}}}
			},
		},
		{
			name:  "a Ctrl-C paused the run and the node reported nothing",
			cfg:   config.Default(),
			pause: true,
			node: func(cancel context.CancelFunc) graph.Node {
				return &interruptingNode{name: graph.NodeCode, cancel: cancel}
			},
		},
		{
			name: "a cancellation ended the run and the node reported nothing",
			cfg:  config.Default(),
			node: func(cancel context.CancelFunc) graph.Node {
				return &interruptingNode{name: graph.NodeCode, cancel: cancel}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, tt.cfg, "pay for what a killed call spent")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			plan := &fakeNode{name: graph.NodePlan, steps: []nodeStep{{
				res: Result{Next: graph.NodeCode, Status: journal.StatusOK, Usage: planSpend},
			}}}

			out, _ := h.pausingDispatcher(t, tt.pause, plan, tt.node(cancel)).Run(ctx)

			rec := h.lastNodeFinished(t)
			want := journal.Usage{
				InputTokens: planSpend.InputTokens, OutputTokens: planSpend.OutputTokens,
				USD: planSpend.USD, Estimated: true,
			}
			if rec.Node != graph.NodeCode || rec.Usage == nil || *rec.Usage != want {
				t.Errorf("the cut-short attempt of %q was journalled with usage %+v, want %+v", rec.Node, rec.Usage, want)
			}
			if !closeTo(out.Usage.USD, 2*planSpend.USD) || !out.Usage.Estimated {
				t.Errorf("Outcome.Usage = %+v, want USD %v marked estimated", out.Usage, 2*planSpend.USD)
			}
			if !strings.Contains(rec.Note, "estimated") {
				t.Errorf("the attempt's note %q does not say its charge is an estimate", rec.Note)
			}
		})
	}
}

// TestDispatcher_InterruptingAndResumingCannotSpendPastTheCeiling is the
// scenario the charge exists for: a paid step that is cut short every time,
// by a person pressing Ctrl-C and resuming, or by graph.node_timeout and a
// person resuming without raising it. Each cut is charged what the run's
// costliest step cost, so the budget guard stops the loop once the charges
// reach the ceiling, instead of it running for ever on a ledger that only
// ever saw the plan.
func TestDispatcher_InterruptingAndResumingCannotSpendPastTheCeiling(t *testing.T) {
	planSpend := belay.Usage{InputTokens: 1000, OutputTokens: 200, USD: 0.40}
	tests := []struct {
		name    string
		timeout bool
	}{
		{name: "a step a Ctrl-C keeps interrupting"},
		{name: "a step that keeps timing out", timeout: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := budgetCfg(1.00, 0)
			if tt.timeout {
				cfg.Graph.NodeTimeout = config.NewDuration(20 * time.Millisecond)
			}
			h := newRun(t, cfg, "spend no more than the ceiling, however often interrupted")
			plan := &fakeNode{name: graph.NodePlan, steps: []nodeStep{{
				res: Result{Next: graph.NodeCode, Status: journal.StatusOK, Usage: planSpend},
			}}}

			codeRuns := 0
			var lastErr error
			for i := range 10 {
				ctx, cancel := context.WithCancel(context.Background())
				var code graph.Node = &interruptingNode{name: graph.NodeCode, cancel: cancel}
				if tt.timeout {
					code = &fakeNode{name: graph.NodeCode, steps: []nodeStep{{sleep: time.Minute}}}
				}
				d := h.pausingDispatcher(t, true, plan, code)
				if i == 0 {
					_, lastErr = d.Run(ctx)
				} else {
					_, lastErr = d.Resume(ctx)
				}
				cancel()
				switch n := code.(type) {
				case *interruptingNode:
					codeRuns += n.calls
				case *fakeNode:
					codeRuns += n.calls
				}
				if errors.Is(lastErr, belay.ErrBudgetExceeded) {
					break
				}
			}

			if !errors.Is(lastErr, belay.ErrBudgetExceeded) {
				t.Fatalf("after 10 dispatches the last error is %v, want the budget ceiling to have stopped the run", lastErr)
			}
			// $0.40 for the plan, then $0.40 for each cut: the guard lets a
			// second cut start at $0.80 and stops the third at $1.20.
			if codeRuns != 2 {
				t.Errorf("the cut-short step started %d times, want 2 before the ceiling stopped it", codeRuns)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Guards: steps, timeout, node failure.
// ---------------------------------------------------------------------------

// TestDispatcher_GuardsPauseSoARaisedLimitContinuesTheRun is the decision
// that the dispatcher's own limits — the budget ceiling, graph.max_steps and
// graph.node_timeout — stop a run without ending it.
//
// They used to end the run for good, so the work and the money already spent
// were unreachable: the only way on was a new run, from nothing. A limit is a
// statement about how much the person is willing to spend, and they can
// change their mind; the run pauses, and `belay resume` re-checks it against
// the configuration as it is then, because limits are read live.
func TestDispatcher_GuardsPauseSoARaisedLimitContinuesTheRun(t *testing.T) {
	spent := belay.Usage{InputTokens: 100, OutputTokens: 50, USD: 2.00}
	tests := []struct {
		name string
		// cfg is the configuration whose limit stops the run.
		cfg config.Config
		// seed puts the run in the state that limit refuses.
		seed func(*state.Manifest)
		// raise is the same configuration with the limit lifted.
		raise func(config.Config) config.Config
		// nodes for the first dispatch and, fresh, for the resume.
		nodes   func() []graph.Node
		wantErr error
		// wantNode is where the run pauses.
		wantNode string
		// wantTimeline is the journal after the pause.
		wantTimeline []string
	}{
		{
			name: "the budget ceiling",
			cfg:  budgetCfg(5.00, 0),
			seed: func(m *state.Manifest) {
				m.Budget = state.Budget{LimitUSD: 5.00, SpentUSD: 7.50, TokensIn: 100, TokensOut: 50}
			},
			raise: func(c config.Config) config.Config { c.Budget.MaxUSD = 100.00; return c },
			nodes: func() []graph.Node {
				return []graph.Node{&fakeNode{name: graph.NodePlan, steps: []nodeStep{{
					res: Result{Next: graph.End, Status: journal.StatusOK, Usage: spent},
				}}}}
			},
			wantErr:      belay.ErrBudgetExceeded,
			wantNode:     graph.NodePlan,
			wantTimeline: []string{"run_started", "run_paused"},
		},
		{
			name: "graph.max_steps",
			cfg: func() config.Config {
				c := budgetCfg(100.00, 0)
				c.Graph.MaxSteps = 1
				return c
			}(),
			seed:  func(*state.Manifest) {},
			raise: func(c config.Config) config.Config { c.Graph.MaxSteps = 5; return c },
			nodes: func() []graph.Node {
				return []graph.Node{
					&fakeNode{name: graph.NodePlan, steps: []nodeStep{{
						res: Result{Next: graph.NodeCode, Status: journal.StatusOK, Usage: spent},
					}}},
					ok(graph.NodeCode, graph.End),
				}
			},
			wantErr:  graph.ErrMaxSteps,
			wantNode: graph.NodeCode,
			wantTimeline: []string{
				"run_started", "plan#1 started", "plan#1 finished ok -> code", "run_paused",
			},
		},
		{
			name: "graph.node_timeout",
			cfg: func() config.Config {
				c := budgetCfg(100.00, 0)
				c.Graph.NodeTimeout = config.NewDuration(20 * time.Millisecond)
				return c
			}(),
			seed: func(*state.Manifest) {},
			raise: func(c config.Config) config.Config {
				c.Graph.NodeTimeout = config.NewDuration(30 * time.Second)
				return c
			},
			nodes: func() []graph.Node {
				return []graph.Node{&fakeNode{name: graph.NodePlan, steps: []nodeStep{
					{
						res:       Result{Next: graph.End, Status: journal.StatusOK, Usage: spent},
						sleep:     150 * time.Millisecond,
						ignoreCtx: true,
					},
					{res: Result{Next: graph.End, Status: journal.StatusOK, Usage: spent}},
				}}}
			},
			wantErr:  graph.ErrNodeTimeout,
			wantNode: graph.NodePlan,
			// The attempt is closed as one to run again, naming its own
			// node, which is what makes the resume re-run it.
			wantTimeline: []string{
				"run_started", "plan#1 started", "plan#1 finished aborted -> plan", "run_paused",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, tt.cfg, "stop, but do not end")
			man := h.manifest(t)
			tt.seed(&man)
			if err := h.store.SaveManifest(man); err != nil {
				t.Fatalf("SaveManifest() error = %v", err)
			}

			out, err := h.dispatcher(t, tt.nodes()...).Run(context.Background())

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Run() error = %v, want one wrapping %v", err, tt.wantErr)
			}
			if out.Status != state.RunStatusPaused {
				t.Errorf("Outcome.Status = %v, want paused: a limit stops a run, it does not end it", out.Status)
			}
			// The exit code does not change: this invocation still stopped
			// short of where the graph would have taken it.
			if out.ExitCode != graph.ExitAborted {
				t.Errorf("Outcome.ExitCode = %d, want %d", out.ExitCode, graph.ExitAborted)
			}
			if out.Node != tt.wantNode {
				t.Errorf("Outcome.Node = %q, want %q", out.Node, tt.wantNode)
			}
			if got := h.timeline(t); !equalStrings(got, tt.wantTimeline) {
				t.Fatalf("timeline =\n  %v\nwant\n  %v", got, tt.wantTimeline)
			}
			if got := h.manifest(t).Status; got != state.RunStatusPaused {
				t.Errorf("manifest.Status = %v, want paused, so `belay runs` offers the run", got)
			}
			paused := h.manifest(t).Budget.SpentUSD

			// Raising the limit and resuming continues the same run, with
			// everything it had already spent still counted.
			h.cfg = tt.raise(tt.cfg)
			out2, err2 := h.dispatcher(t, tt.nodes()...).Resume(context.Background())
			if err2 != nil {
				t.Fatalf("Resume() after raising the limit error = %v, want nil", err2)
			}
			if out2.Status != state.RunStatusCompleted {
				t.Errorf("Outcome.Status after the resume = %v, want completed", out2.Status)
			}
			if out2.Usage.USD < paused {
				t.Errorf("Outcome.Usage.USD after the resume = %v, less than the %v spent before the pause: "+
					"resuming lost the run's spend", out2.Usage.USD, paused)
			}
		})
	}
}

// TestDispatcher_APausedGuardIsRefusedByRunAndMetAgainIfTheLimitStands:
// resuming re-checks the limit against the configuration as it is now. Left
// alone, the run pauses again, having done nothing and spent nothing; a plain
// `belay run` refuses it, as it refuses every paused run.
func TestDispatcher_APausedGuardIsRefusedByRunAndMetAgainIfTheLimitStands(t *testing.T) {
	h := newRun(t, budgetCfg(5.00, 0), "meet the ceiling again")
	man := h.manifest(t)
	man.Budget = state.Budget{LimitUSD: 5.00, SpentUSD: 7.50}
	if err := h.store.SaveManifest(man); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}

	plan := ok(graph.NodePlan, graph.End)
	if _, err := h.dispatcher(t, plan).Run(context.Background()); !errors.Is(err, belay.ErrBudgetExceeded) {
		t.Fatalf("Run() error = %v, want one wrapping belay.ErrBudgetExceeded", err)
	}

	// A plain Run refuses a paused run, whoever paused it.
	out, err := h.dispatcher(t, plan).Run(context.Background())
	if !errors.Is(err, graph.ErrRunPaused) || out.ExitCode != graph.ExitPaused {
		t.Errorf("Run() on the paused run = {%v, exit %d}, want ErrRunPaused and exit %d",
			err, out.ExitCode, graph.ExitPaused)
	}

	// Resumed with the ceiling unchanged, it meets it again.
	out2, err2 := h.dispatcher(t, plan).Resume(context.Background())
	if !errors.Is(err2, belay.ErrBudgetExceeded) {
		t.Fatalf("Resume() error = %v, want one wrapping belay.ErrBudgetExceeded", err2)
	}
	if out2.Status != state.RunStatusPaused {
		t.Errorf("Outcome.Status = %v, want paused", out2.Status)
	}
	if plan.calls != 0 {
		t.Errorf("plan ran %d times, want 0: nothing may run while the run is over its ceiling", plan.calls)
	}
}

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
	if out.Status != state.RunStatusPaused || out.ExitCode != graph.ExitAborted {
		t.Errorf("Outcome = {%v, exit %d}, want {paused, exit %d}", out.Status, out.ExitCode, graph.ExitAborted)
	}
	if got := h.manifest(t).Step; got != 3 {
		t.Errorf("manifest.Step = %d, want 3", got)
	}
	// Paused, so raising graph.max_steps and resuming lets the run go on
	// from the step it reached.
	if last := h.timeline(t)[len(h.timeline(t))-1]; last != "run_paused" {
		t.Errorf("last journal record = %q, want run_paused", last)
	}
	if !strings.Contains(out.Note, "graph.max_steps") {
		t.Errorf("Outcome.Note = %q, want it to name the setting to raise", out.Note)
	}
}

func TestDispatcher_NodeTimeoutPausesTheRun(t *testing.T) {
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
	if out.Status != state.RunStatusPaused || out.ExitCode != graph.ExitAborted {
		t.Errorf("Outcome = {%v, exit %d}, want {paused, exit %d}", out.Status, out.ExitCode, graph.ExitAborted)
	}
	// The run stops rather than looping, and the attempt is closed as one to
	// run again, naming its own node: raising graph.node_timeout and
	// resuming re-runs the step that overran.
	want := []string{"run_started", "plan#1 started", "plan#1 finished aborted -> plan", "run_paused"}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
	if !strings.Contains(out.Note, "graph.node_timeout") {
		t.Errorf("Outcome.Note = %q, want a clear timeout explanation naming the setting", out.Note)
	}
}

// supervisingNode is a fakeNode that supervises candidate runs, so
// graph.node_timeout does not apply to it.
type supervisingNode struct{ *fakeNode }

func (supervisingNode) SupervisesCandidates() {}

// TestDispatcher_NodeTimeoutDoesNotApplyToASupervisor pins the exemption a
// fanout node needs. graph.node_timeout caps one node's own work; a node
// that supervises candidate runs is waiting on runs whose every node is
// already capped by that same setting, so applying it a second time at the
// parent would kill N candidates in the middle of paid work the moment the
// fanout as a whole outlived a single node's limit.
func TestDispatcher_NodeTimeoutDoesNotApplyToASupervisor(t *testing.T) {
	tests := []struct {
		name       string
		supervises bool
		wantErr    error
		wantStatus state.RunStatus
		wantSteps  []string
	}{
		{
			name:       "a node that supervises candidate runs outlives the cap",
			supervises: true,
			wantStatus: state.RunStatusCompleted,
			wantSteps: []string{
				"run_started", "fanout#1 started", "fanout#1 finished ok -> END", "run_completed",
			},
		},
		{
			name:       "a plain node in the same position is still capped",
			supervises: false,
			wantErr:    graph.ErrNodeTimeout,
			wantStatus: state.RunStatusPaused,
			wantSteps: []string{
				"run_started", "fanout#1 started", "fanout#1 finished aborted -> fanout", "run_paused",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Graph.NodeTimeout = config.NewDuration(50 * time.Millisecond)
			h := newRun(t, cfg, "wait for the candidates")

			// It ignores its context, as a node waiting on candidate runs
			// that each watch their own does: the cap must be absent,
			// not merely unobserved.
			slow := &fakeNode{name: graph.NodeFanout, steps: []nodeStep{{
				res:       Result{Next: graph.End, Status: journal.StatusOK},
				sleep:     200 * time.Millisecond,
				ignoreCtx: true,
			}}}
			node := graph.Node(slow)
			if tt.supervises {
				node = supervisingNode{fakeNode: slow}
			}

			out, err := h.entryDispatcher(t, graph.NodeFanout, node).Run(context.Background())

			if tt.wantErr == nil && err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Run() error = %v, want one wrapping %v", err, tt.wantErr)
			}
			if out.Status != tt.wantStatus {
				t.Errorf("Outcome.Status = %v, want %v (note %q)", out.Status, tt.wantStatus, out.Note)
			}
			if got := h.timeline(t); !equalStrings(got, tt.wantSteps) {
				t.Errorf("timeline =\n  %v\nwant\n  %v", got, tt.wantSteps)
			}
		})
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
	// usage is what the backend behind this node reported spending before
	// the cancellation reached it. A node cut short still knows what it has
	// already paid for.
	usage belay.Usage
	calls int
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
			Patch: state.Patch{Fix: &state.Fix{Attempts: 9}}, Usage: n.usage,
		}, nil
	}
	return Result{Usage: n.usage}, ctx.Err()
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
// checked first, and it is the timeout the run records: both stop the run in
// the same state now, and the reason is what tells a person to raise
// graph.node_timeout rather than simply to resume.
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
			// Both stop the run the same way now, so the verdict is told
			// from the interruption by the reason recorded, which is what
			// says which setting to raise.
			if errors.Is(err, graph.ErrInterrupted) {
				t.Errorf("Run() error = %v: the timeout was reported as a Ctrl-C", err)
			}
			if !strings.Contains(out.Note, "graph.node_timeout") {
				t.Errorf("Outcome.Note = %q, want the timeout named, not the cancellation", out.Note)
			}
			if out.Status != state.RunStatusPaused {
				t.Errorf("Outcome.Status = %v, want paused", out.Status)
			}
			want := []string{"run_started", "plan#1 started", "plan#1 finished aborted -> plan", "run_paused"}
			if got := h.timeline(t); !equalStrings(got, want) {
				t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
			}
		})
	}
}

// TestDispatcher_GuardsComeBeforeTheCancellationPause: a run the budget
// ceiling or graph.max_steps stops is stopped by that limit, even when its
// context was cancelled before the next node. Both pause the run, so what is
// at stake is the reason: a person told only that belay stopped when they
// asked would resume, and meet the same limit again, never learning which
// setting to raise.
func TestDispatcher_GuardsComeBeforeTheCancellationPause(t *testing.T) {
	tests := []struct {
		name        string
		cfg         config.Config
		seed        func(*state.Manifest)
		wantErr     error
		wantSetting string
	}{
		{
			name:        "already over the budget ceiling",
			cfg:         budgetCfg(5.00, 0),
			seed:        func(m *state.Manifest) { m.Budget = state.Budget{LimitUSD: 5.00, SpentUSD: 7.50} },
			wantErr:     belay.ErrBudgetExceeded,
			wantSetting: "budget.max_usd",
		},
		{
			name: "already at graph.max_steps",
			cfg: func() config.Config {
				cfg := config.Default()
				cfg.Graph.MaxSteps = 3
				return cfg
			}(),
			seed:        func(m *state.Manifest) { m.Step = 3 },
			wantErr:     graph.ErrMaxSteps,
			wantSetting: "graph.max_steps",
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
				t.Errorf("Run() error = %v: the limit's verdict was reported as a Ctrl-C", err)
			}
			if out.Status != state.RunStatusPaused || out.ExitCode != graph.ExitAborted {
				t.Errorf("Outcome = {%v, exit %d}, want {paused, exit %d}", out.Status, out.ExitCode, graph.ExitAborted)
			}
			if plan.calls != 0 {
				t.Errorf("plan ran %d times, want 0", plan.calls)
			}
			if !strings.Contains(out.Note, tt.wantSetting) {
				t.Errorf("Outcome.Note = %q, want it to name %s", out.Note, tt.wantSetting)
			}
			want := []string{"run_started", "run_paused"}
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
				// A name no belay has: "output.branch" stood in for one
				// until this binary gained it.
				doc["requires"] = json.RawMessage(`["from.the.future"]`)
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
