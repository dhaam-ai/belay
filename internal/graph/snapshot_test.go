package graph_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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
