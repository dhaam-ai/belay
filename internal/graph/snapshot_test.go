package graph_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
)

// funcNode is a node whose behaviour is a closure, for the tests below that
// need a node to look at the run directory, or at what it was given, while it
// runs.
type funcNode struct {
	name string
	run  func(ctx context.Context, rc *graph.RunContext) (graph.Result, error)
}

func (f *funcNode) Name() string { return f.name }

func (f *funcNode) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	return f.run(ctx, rc)
}

// seenBy records what each execution of a node was given.
type seenBy struct {
	states []state.State
	steps  []int
}

// recording returns a node that records what it was given and returns res.
func recording(name string, seen *seenBy, res graph.Result) *funcNode {
	return &funcNode{name: name, run: func(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
		seen.states = append(seen.states, rc.State)
		seen.steps = append(seen.steps, rc.Step)
		return res, nil
	}}
}

// snapshotAt loads the snapshot the dispatcher took for (seq, node).
func (h *harness) snapshotAt(t *testing.T, seq uint64, node string) state.Snapshot {
	t.Helper()
	path, err := h.layout.SnapshotPath(seq, node)
	if err != nil {
		t.Fatalf("SnapshotPath(%d, %q) error = %v", seq, node, err)
	}
	snap, err := state.LoadSnapshot(path)
	if err != nil {
		t.Fatalf("no snapshot for %s at seq %d: %v", node, seq, err)
	}
	return snap
}

// visits groups the run's journal with journal.Visits, the grouping a fork
// will choose its snapshot by.
func (h *harness) visits(t *testing.T) []journal.Visit {
	t.Helper()
	vs, err := journal.Visits(h.records(t))
	if err != nil {
		t.Fatalf("journal.Visits() error = %v", err)
	}
	return vs
}

// TestDispatcher_SnapshotsTheBlackboardBeforeEachVisit pins what a fork will
// read: for every visit journal.Visits finds, a snapshot keyed on the visit's
// StartSeq holding exactly the blackboard the node was given, taken before
// the node's own patch landed.
func TestDispatcher_SnapshotsTheBlackboardBeforeEachVisit(t *testing.T) {
	h := newRun(t, config.Default(), "snapshot every visit")

	var planSaw, codeSaw, testSaw seenBy
	plan := recording(graph.NodePlan, &planSaw, graph.Result{
		Next: graph.NodeCode, Status: journal.StatusOK,
		Patch: state.Patch{Plan: &state.Plan{Path: "plan.md"}},
	})
	code := recording(graph.NodeCode, &codeSaw, graph.Result{
		Next: graph.NodeTest, Status: journal.StatusOK,
		Patch: state.Patch{Code: &state.Code{LastDiff: "diff-0002.patch"}},
	})
	test := recording(graph.NodeTest, &testSaw, graph.Result{Next: graph.End, Status: journal.StatusOK})

	if _, err := h.dispatcher(t, plan, code, test).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	saw := map[string]*seenBy{graph.NodePlan: &planSaw, graph.NodeCode: &codeSaw, graph.NodeTest: &testSaw}
	visits := h.visits(t)
	if len(visits) != 3 {
		t.Fatalf("journal.Visits() found %d visits, want 3", len(visits))
	}
	for _, v := range visits {
		snap := h.snapshotAt(t, v.StartSeq, v.Node)
		seen := saw[v.Node]
		if diff := cmp.Diff(seen.states[0], snap.State); diff != "" {
			t.Errorf("snapshot of %s differs from the blackboard the node was given (-given +snapshot):\n%s", v.Node, diff)
		}
		if snap.Step != seen.steps[0] || snap.Seq != v.StartSeq || snap.Node != v.Node || snap.RunID != testRunID {
			t.Errorf("snapshot of %s = {step %d, seq %d, node %q, run %q}, want {step %d, seq %d, node %q, run %q}",
				v.Node, snap.Step, snap.Seq, snap.Node, snap.RunID, seen.steps[0], v.StartSeq, v.Node, testRunID)
		}
	}

	// Taken before the node ran: code's snapshot has plan's patch and not
	// its own.
	codeSnap := h.snapshotAt(t, visits[1].StartSeq, graph.NodeCode)
	if codeSnap.State.Plan.Path != "plan.md" || codeSnap.State.Code.LastDiff != "" {
		t.Errorf("code's snapshot has Plan.Path %q and Code.LastDiff %q, want \"plan.md\" and \"\": "+
			"it must be the blackboard before code ran", codeSnap.State.Plan.Path, codeSnap.State.Code.LastDiff)
	}
}

// TestDispatcher_SnapshotsAGateEachTimeItIsTakenUpAgain: the approve gate's
// visit spans its pause, and the person acts in between — `belay resume`
// records the approval in state.json. The execution after the resume gets a
// snapshot of its own, holding the approval, and the visit's first snapshot
// is left exactly as it was taken.
func TestDispatcher_SnapshotsAGateEachTimeItIsTakenUpAgain(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = true
	h := newRun(t, cfg, "snapshot the gate twice")

	var gateSaw seenBy
	gate := &funcNode{name: graph.NodeApprove, run: func(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
		gateSaw.states = append(gateSaw.states, rc.State)
		if rc.State.Plan.Approved {
			return graph.Result{Next: graph.End, Status: journal.StatusOK}, nil
		}
		return graph.Result{Status: journal.StatusPaused, Note: "waiting for a person"}, nil
	}}
	plan := ok(graph.NodePlan, graph.NodeApprove)

	if _, err := h.dispatcher(t, plan, gate).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// What `belay resume` does before it dispatches: record the approval.
	st := h.state(t)
	st.Plan.Approved = true
	if err := h.store.SaveState(st); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
	if _, err := h.dispatcher(t, plan, gate).Resume(context.Background()); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	var gateStarts []uint64
	for _, r := range h.records(t) {
		if r.Event == journal.EventNodeStarted && r.Node == graph.NodeApprove {
			gateStarts = append(gateStarts, r.Seq)
		}
	}
	if len(gateStarts) != 2 {
		t.Fatalf("approve started %d times, want 2 (the pause and the resume)", len(gateStarts))
	}
	first, again := h.snapshotAt(t, gateStarts[0], graph.NodeApprove), h.snapshotAt(t, gateStarts[1], graph.NodeApprove)
	if first.State.Plan.Approved {
		t.Error("the gate visit's first snapshot holds an approval nobody had given when it was taken")
	}
	if !again.State.Plan.Approved {
		t.Error("the snapshot taken when the gate was taken up again does not hold the approval recorded before it")
	}
	if diff := cmp.Diff(gateSaw.states[1], again.State); diff != "" {
		t.Errorf("second snapshot differs from what the gate was given (-given +snapshot):\n%s", diff)
	}
}

// TestDispatcher_SnapshotRecordsTheWorkspaceDigest: Options.Fingerprint is
// how a fork will tell whether the tree has changed since the visit it
// re-runs. A fingerprint that fails is logged and recorded as unknown; it
// never stops a run, since the run does not depend on it.
func TestDispatcher_SnapshotRecordsTheWorkspaceDigest(t *testing.T) {
	tests := []struct {
		name        string
		fingerprint func(ctx context.Context, dir string) (string, error)
		wantDigest  string
		wantWarn    bool
	}{
		{
			name:        "the digest is recorded",
			fingerprint: func(context.Context, string) (string, error) { return "tree1:sha256:ab", nil },
			wantDigest:  "tree1:sha256:ab",
		},
		{
			name: "a failed fingerprint is recorded as unknown, and said",
			fingerprint: func(context.Context, string) (string, error) {
				return "tree1:sha256:partial", errors.New("walk interrupted")
			},
			wantWarn: true,
		},
		{name: "no fingerprint, no digest"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRun(t, config.Default(), "fingerprint the workspace")

			var dirs []string
			var fingerprint func(context.Context, string) (string, error)
			if tt.fingerprint != nil {
				fingerprint = func(ctx context.Context, dir string) (string, error) {
					dirs = append(dirs, dir)
					return tt.fingerprint(ctx, dir)
				}
			}
			var logs bytes.Buffer
			reg := graph.NewRegistry()
			reg.MustRegister(ok(graph.NodePlan, graph.End))
			d, err := graph.NewDispatcher(graph.Options{
				Store: h.store, Registry: reg, Config: h.cfg,
				Logger:      slog.New(slog.NewTextHandler(&logs, nil)),
				Fingerprint: fingerprint,
			})
			if err != nil {
				t.Fatalf("NewDispatcher() error = %v", err)
			}

			out, err := d.Run(context.Background())
			if err != nil || out.Status != state.RunStatusCompleted {
				t.Fatalf("Run() = %v, %v; want a completed run whatever the fingerprint did", out.Status, err)
			}
			snap := h.snapshotAt(t, h.visits(t)[0].StartSeq, graph.NodePlan)
			if snap.WorkspaceDigest != tt.wantDigest {
				t.Errorf("WorkspaceDigest = %q, want %q", snap.WorkspaceDigest, tt.wantDigest)
			}
			if tt.fingerprint != nil && (len(dirs) != 1 || dirs[0] != h.layout.WorkspaceDir()) {
				t.Errorf("Fingerprint was called with %q, want once with the workspace %q", dirs, h.layout.WorkspaceDir())
			}
			if warned := strings.Contains(logs.String(), "level=WARN") &&
				strings.Contains(logs.String(), "walk interrupted"); warned != tt.wantWarn {
				t.Errorf("warned about the fingerprint = %v, want %v; log:\n%s", warned, tt.wantWarn, logs.String())
			}
		})
	}
}

// TestDispatcher_ASnapshotItCannotSaveStopsTheNodeBeforeItRuns: without its
// snapshot, a crash later in the execution could not be recovered correctly
// (see state.Snapshot), so the node does not run. Its attempt is closed as
// one to run again and charged nothing, since nothing ran, and the run picks
// up once the snapshot can be written.
func TestDispatcher_ASnapshotItCannotSaveStopsTheNodeBeforeItRuns(t *testing.T) {
	h := newRun(t, config.Default(), "no snapshot, no node")

	// A file where the snapshots directory belongs: no snapshot can be saved.
	blocker := h.layout.SnapshotsDir()
	writeFile(t, blocker, []byte("not a directory"))

	plan := ok(graph.NodePlan, graph.End)
	out, err := h.dispatcher(t, plan).Run(context.Background())
	if err == nil {
		t.Fatal("Run() = nil error with no way to save a snapshot")
	}
	if out.ExitCode != graph.ExitFailed {
		t.Errorf("ExitCode = %d, want ExitFailed", out.ExitCode)
	}
	if plan.calls != 0 {
		t.Fatalf("plan ran %d times, want 0: it must not run without its snapshot", plan.calls)
	}
	want := []string{"run_started", "plan#1 started", "plan#1 finished aborted -> plan"}
	if got := h.timeline(t); !equalStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
	if u := h.lastNodeFinished(t).Usage; u == nil || u.USD != 0 || u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("closing record's usage = %+v, want an accounted zero: the node never ran", u)
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	out, err = h.dispatcher(t, plan).Resume(context.Background())
	if err != nil || out.Status != state.RunStatusCompleted {
		t.Fatalf("Resume() = %v, %v; want the run to complete once snapshots can be saved", out.Status, err)
	}
	if plan.calls != 1 || plan.attempts[0] != 2 {
		t.Errorf("plan ran %d times at attempts %v, want once, at attempt 2", plan.calls, plan.attempts)
	}
	if out.Usage.USD != 0 {
		t.Errorf("run spent $%.2f, want $0: an attempt that never ran costs nothing", out.Usage.USD)
	}
}

// readFile reads a run file a test captured or is about to restore.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	// #nosec G304 -- path is inside the test's own run directory.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

// countingFix is the fix node's counting, reduced to what the double
// increment needs: it reads State.Fix.Attempts, sets it one higher (an
// absolute set, as internal/nodes/fix does), and records what it read.
func countingFix(next string, seen *[]int, during func(call int)) *funcNode {
	return &funcNode{name: graph.NodeFix, run: func(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
		*seen = append(*seen, rc.State.Fix.Attempts)
		if during != nil {
			during(len(*seen))
		}
		return graph.Result{
			Next: next, Status: journal.StatusOK,
			Patch: state.Patch{Fix: &state.Fix{Attempts: rc.State.Fix.Attempts + 1}},
		}, nil
	}}
}

// TestDispatcher_CrashReRunDoesNotCountTheCrashedAttemptTwice reproduces the
// double increment, through the real dispatcher.
//
// The dispatcher applies a node's patch and only then journals that the node
// finished (see Dispatcher). A process killed between the two leaves
// state.json holding the patch and the journal ending in the node's open
// node_started, and the dispatcher re-runs the node at attempt 2. That re-run
// used to read state.json — which already holds its own crashed attempt's
// output — so a node whose patch is computed from what it read counts the
// crashed attempt as well as its own: fix's "attempt 1 of 3" became "attempt 2
// of 3", and a run could give up a repair early.
//
// The crash is reconstructed from what the first dispatch really wrote: the
// journal and the manifest exactly as they stood when fix began (its
// node_started is the journal's last record), and state.json exactly as fix's
// own patch left it, captured by the next node before it changed anything.
// A torn half-line stands for the node_finished the kill cut off.
func TestDispatcher_CrashReRunDoesNotCountTheCrashedAttemptTwice(t *testing.T) {
	h := newRun(t, config.Default(), "count each repair once")

	var (
		journalAtFix, manifestAtFix, stateAfterFix []byte
		seen                                       []int
	)
	plan := ok(graph.NodePlan, graph.NodeFix)
	fix := countingFix(graph.NodeTest, &seen, func(call int) {
		if call == 1 {
			journalAtFix = readFile(t, h.layout.JournalPath())
			manifestAtFix = readFile(t, h.layout.ManifestPath())
		}
	})
	testCalls := 0
	test := &funcNode{name: graph.NodeTest, run: func(context.Context, *graph.RunContext) (graph.Result, error) {
		testCalls++
		if testCalls == 1 {
			stateAfterFix = readFile(t, h.layout.StatePath())
			return graph.Result{Status: journal.StatusPaused, Note: "stop here"}, nil
		}
		return graph.Result{Next: graph.End, Status: journal.StatusOK}, nil
	}}

	if _, err := h.dispatcher(t, plan, fix, test).Run(context.Background()); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}

	// The kill: fix's patch is durable, its node_finished is not.
	writeFile(t, h.layout.JournalPath(), append(journalAtFix, []byte(`{"seq":`)...))
	writeFile(t, h.layout.ManifestPath(), manifestAtFix)
	writeFile(t, h.layout.StatePath(), stateAfterFix)
	forgetSnapshotsAfter(t, h, lastSeq(t, journalAtFix))
	if got := h.state(t).Fix.Attempts; got != 1 {
		t.Fatalf("reconstructed crash: state.json Fix.Attempts = %d, want 1 (fix's patch landed)", got)
	}

	out, err := h.dispatcher(t, plan, fix, test).Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed", out.Status)
	}

	if len(seen) != 2 {
		t.Fatalf("fix ran %d times, want 2 (the crashed attempt and its re-run)", len(seen))
	}
	if seen[1] != 0 {
		t.Errorf("fix's re-run read Fix.Attempts = %d, want 0: it must see the blackboard as it stood "+
			"before the attempt it repeats, not that attempt's own patch", seen[1])
	}
	if got := h.state(t).Fix.Attempts; got != 1 {
		t.Errorf("Fix.Attempts = %d after recovering, want 1: one repair was made, and it was counted twice", got)
	}
}

// seedCrashedFix builds the disk a run leaves when fix's attempts were cut
// short: the given records after run_started, the snapshot fix's first
// attempt took (Fix.Attempts 0), and a state.json in which a crashed
// attempt's patch landed (Fix.Attempts 1).
func seedCrashedFix(t *testing.T, h *harness, recs ...journal.Record) {
	t.Helper()
	before := h.state(t)
	if _, err := h.store.SaveSnapshotOnce(state.Snapshot{
		RunID: testRunID, Node: graph.NodeFix, Seq: 2, Step: 1, State: before,
	}); err != nil {
		t.Fatalf("SaveSnapshotOnce() error = %v", err)
	}
	after := before
	after.Fix = state.Fix{Attempts: 1}
	if err := h.store.SaveState(after); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
	h.seedJournal(t, append([]journal.Record{{Event: journal.EventRunStarted}}, recs...)...)
}

// TestDispatcher_EveryReRunReadsTheSnapshotOfTheAttemptItRepeats: after two
// crashes in a row the third attempt still reads the snapshot the first one
// took. The second attempt took none — a re-run never does — so keying the
// read on the attempt immediately before would find nothing and fall back to
// a state.json that already holds a crashed attempt's patch.
func TestDispatcher_EveryReRunReadsTheSnapshotOfTheAttemptItRepeats(t *testing.T) {
	h := newRun(t, config.Default(), "crash twice")
	seedCrashedFix(t, h,
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodeFix, Attempt: 1}, // seq 2
		journal.Record{
			Event: journal.EventNodeFinished, Node: graph.NodeFix, Attempt: 1,
			Status: journal.StatusAborted, Next: graph.NodeFix, Note: "interrupted",
		},
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodeFix, Attempt: 2}, // left open
	)

	var seen []int
	fix := countingFix(graph.End, &seen, nil)
	if _, err := h.dispatcher(t, fix).Resume(context.Background()); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	if len(seen) != 1 || seen[0] != 0 {
		t.Fatalf("fix's third attempt read Fix.Attempts %v, want [0]: the blackboard from before its first attempt", seen)
	}
	if got := h.state(t).Fix.Attempts; got != 1 {
		t.Errorf("Fix.Attempts = %d, want 1", got)
	}
}

// TestDispatcher_AReRunTakesTheSnapshotItsFirstAttemptDidNotLiveToTake: an
// attempt can die between its node_started and its snapshot. Then nothing of
// it ran — the snapshot comes before the node — so state.json is still the
// blackboard from before it, and the re-run saves that as the snapshot, keyed
// where the first attempt's would have been.
func TestDispatcher_AReRunTakesTheSnapshotItsFirstAttemptDidNotLiveToTake(t *testing.T) {
	h := newRun(t, config.Default(), "snapshot at recovery")
	before := h.state(t)
	before.Plan = state.Plan{Path: "plan.md"}
	if err := h.store.SaveState(before); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
	h.seedJournal(t,
		journal.Record{Event: journal.EventRunStarted},
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1}, // seq 2, no snapshot
	)

	var saw seenBy
	code := recording(graph.NodeCode, &saw, graph.Result{
		Next: graph.End, Status: journal.StatusOK,
		Patch: state.Patch{Code: &state.Code{LastDiff: "diff-0001.patch"}},
	})
	if _, err := h.dispatcher(t, code).Resume(context.Background()); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	snap := h.snapshotAt(t, 2, graph.NodeCode)
	if diff := cmp.Diff(before, snap.State); diff != "" {
		t.Errorf("snapshot taken at recovery differs from the blackboard before the execution (-want +got):\n%s", diff)
	}
	if len(saw.states) != 1 || saw.states[0].Plan.Path != "plan.md" || saw.states[0].Code.LastDiff != "" {
		t.Errorf("the re-run was given %+v, want the blackboard from before the execution", saw.states)
	}
}

// TestDispatcher_ACrashReRunOfAGateTakenUpAgainKeepsWhatThePersonDid: the
// person approved the plan while the run waited at the gate, and the gate's
// execution after that was killed. Its re-run must be given the blackboard
// that execution was given, with the approval in it — not the one from the
// start of the gate's visit, taken before anybody had approved anything,
// which would ask the person again for an approval they had already given.
func TestDispatcher_ACrashReRunOfAGateTakenUpAgainKeepsWhatThePersonDid(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = true
	h := newRun(t, cfg, "keep the approval")

	var (
		journalAtGate, manifestAtGate, stateAfterGate []byte
		gateSaw                                       seenBy
	)
	gate := &funcNode{name: graph.NodeApprove, run: func(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
		gateSaw.states = append(gateSaw.states, rc.State)
		if len(gateSaw.states) == 2 {
			journalAtGate = readFile(t, h.layout.JournalPath())
			manifestAtGate = readFile(t, h.layout.ManifestPath())
		}
		if rc.State.Plan.Approved {
			return graph.Result{Next: graph.NodeCode, Status: journal.StatusOK}, nil
		}
		return graph.Result{Status: journal.StatusPaused, Note: "waiting for a person"}, nil
	}}
	codeCalls := 0
	code := &funcNode{name: graph.NodeCode, run: func(context.Context, *graph.RunContext) (graph.Result, error) {
		codeCalls++
		if codeCalls == 1 {
			stateAfterGate = readFile(t, h.layout.StatePath())
			return graph.Result{Status: journal.StatusPaused, Note: "stop here"}, nil
		}
		return graph.Result{Next: graph.End, Status: journal.StatusOK}, nil
	}}
	plan := ok(graph.NodePlan, graph.NodeApprove)

	if _, err := h.dispatcher(t, plan, gate, code).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	st := h.state(t) // what `belay resume` records before it dispatches
	st.Plan.Approved = true
	if err := h.store.SaveState(st); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
	if _, err := h.dispatcher(t, plan, gate, code).Resume(context.Background()); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	// The kill: the approving execution of the gate never finished.
	writeFile(t, h.layout.JournalPath(), journalAtGate)
	writeFile(t, h.layout.ManifestPath(), manifestAtGate)
	writeFile(t, h.layout.StatePath(), stateAfterGate)
	forgetSnapshotsAfter(t, h, lastSeq(t, journalAtGate))

	out, err := h.dispatcher(t, plan, gate, code).Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume() after the crash error = %v", err)
	}
	if len(gateSaw.states) != 3 || !gateSaw.states[2].Plan.Approved {
		t.Fatalf("the gate's re-run was not given the approval (%d executions); it would ask again", len(gateSaw.states))
	}
	if out.Status != state.RunStatusCompleted {
		t.Errorf("Outcome.Status = %v, want completed", out.Status)
	}
}

// lastSeq returns the Seq of the last complete record in a captured journal.
func lastSeq(t *testing.T, data []byte) uint64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "captured.ndjson")
	writeFile(t, path, data)
	res, err := journal.ReadFile(path)
	if err != nil || len(res.Records) == 0 {
		t.Fatalf("captured journal is unreadable or empty: %v", err)
	}
	return res.Records[len(res.Records)-1].Seq
}

// forgetSnapshotsAfter removes every snapshot the first dispatch took for an
// execution after seq, which a process killed at seq would never have taken.
// Leaving one would let a later execution find a snapshot keyed on a Seq the
// truncated journal gives out again, which no real crash can produce: a
// snapshot is written only after its node_started is durable, and a durable
// record is never discarded.
func forgetSnapshotsAfter(t *testing.T, h *harness, seq uint64) {
	t.Helper()
	entries, err := os.ReadDir(h.layout.SnapshotsDir())
	if err != nil {
		t.Fatalf("ReadDir(snapshots) error = %v", err)
	}
	for _, e := range entries {
		prefix, _, found := strings.Cut(e.Name(), "-")
		n, err := strconv.ParseUint(prefix, 10, 64)
		if !found || err != nil {
			continue
		}
		if n > seq {
			if err := os.Remove(filepath.Join(h.layout.SnapshotsDir(), e.Name())); err != nil {
				t.Fatalf("Remove(%s) error = %v", e.Name(), err)
			}
		}
	}
}
