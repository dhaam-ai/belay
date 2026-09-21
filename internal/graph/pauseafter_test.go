package graph_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
)

// pausingAfter wires nodes into a Dispatcher with Options.PauseAfter set.
func (h *harness) pausingAfter(t *testing.T, n int, nodes ...graph.Node) *graph.Dispatcher {
	t.Helper()
	reg := graph.NewRegistry()
	reg.MustRegister(nodes...)
	d, err := graph.NewDispatcher(graph.Options{Store: h.store, Registry: reg, Config: h.cfg, PauseAfter: n})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	return d
}

// TestDispatcher_PauseAfterStopsBeforeTheNextNode is how a fork runs exactly
// one node: the run pauses once its first node has finished, before the next
// one starts, and is an ordinary paused run from then on.
func TestDispatcher_PauseAfterStopsBeforeTheNextNode(t *testing.T) {
	h := newRun(t, config.Default(), "run one node")
	a, b := ok(graph.NodePlan, graph.NodeCode), ok(graph.NodeCode, graph.End)

	out, err := h.pausingAfter(t, 1, a, b).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil: a pause that was asked for is not a failure", err)
	}
	want := graph.Outcome{Status: state.RunStatusPaused, ExitCode: graph.ExitOK, Node: graph.NodeCode, Steps: 1}
	if out.Status != want.Status || out.ExitCode != want.ExitCode || out.Node != want.Node || out.Steps != want.Steps {
		t.Errorf("Outcome = {%v, exit %d, node %q, steps %d}, want {%v, exit %d, node %q, steps %d}",
			out.Status, out.ExitCode, out.Node, out.Steps, want.Status, want.ExitCode, want.Node, want.Steps)
	}
	if b.calls != 0 {
		t.Errorf("code ran %d times, want 0", b.calls)
	}
	wantTimeline := []string{"run_started", "plan#1 started", "plan#1 finished ok -> code", "run_paused"}
	if got := h.timeline(t); !equalStrings(got, wantTimeline) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, wantTimeline)
	}
	if m := h.manifest(t); m.Status != state.RunStatusPaused || m.CurrentNode != graph.NodeCode {
		t.Errorf("manifest = {%v, current %q}, want {paused, current %q}", m.Status, m.CurrentNode, graph.NodeCode)
	}

	if _, err := h.dispatcher(t, a, b).Run(context.Background()); !errors.Is(err, graph.ErrRunPaused) {
		t.Errorf("a later Run() error = %v, want ErrRunPaused", err)
	}
	out, err = h.dispatcher(t, a, b).Resume(context.Background())
	if err != nil || out.Status != state.RunStatusCompleted {
		t.Fatalf("Resume() = %v, %v; want the run to complete", out.Status, err)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("plan ran %d times and code %d, want once each", a.calls, b.calls)
	}
}

// TestDispatcher_PauseAfterCountsThisCallsExecutions: the limit is how many
// nodes this call runs, not the run's step count. A fork continues its source
// run's step numbering, so a limit compared with man.Step would pause a fork
// taken at step 10 before it ran anything.
func TestDispatcher_PauseAfterCountsThisCallsExecutions(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.MaxSteps = 100
	h := newRun(t, cfg, "count this call")
	man := h.manifest(t)
	man.Step = 10
	if err := h.store.SaveManifest(man); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}

	a, b, c := ok(graph.NodePlan, graph.NodeCode), ok(graph.NodeCode, graph.NodeTest), ok(graph.NodeTest, graph.End)
	out, err := h.pausingAfter(t, 2, a, b, c).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if a.calls != 1 || b.calls != 1 || c.calls != 0 {
		t.Errorf("ran plan %d, code %d, test %d times, want 1, 1, 0", a.calls, b.calls, c.calls)
	}
	if out.Steps != 2 || out.Node != graph.NodeTest || out.Status != state.RunStatusPaused {
		t.Errorf("Outcome = {%v, node %q, steps %d}, want {paused, node %q, steps 2}", out.Status, out.Node, out.Steps, graph.NodeTest)
	}
	if got := h.manifest(t).Step; got != 12 {
		t.Errorf("manifest Step = %d, want 12: the run's own count carries on from 10", got)
	}
}

// TestDispatcher_PauseAfterNeverMasksAnOutcomeTheNodeChose: the limit only
// ever stops a run that would have gone on to another node. A node that
// fails, ends the run, pauses or aborts ends the dispatch exactly as it does
// without the limit.
func TestDispatcher_PauseAfterNeverMasksAnOutcomeTheNodeChose(t *testing.T) {
	tests := []struct {
		name string
		res  graph.Result
	}{
		{name: "failed", res: graph.Result{Status: journal.StatusFailed, Note: "no"}},
		{name: "reached the end", res: graph.Result{Next: graph.End, Status: journal.StatusOK}},
		{name: "paused", res: graph.Result{Status: journal.StatusPaused, Note: "waiting"}},
		{name: "aborted", res: graph.Result{Status: journal.StatusAborted, Note: "stop"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := func() *fakeNode { return &fakeNode{name: graph.NodePlan, steps: []nodeStep{{res: tt.res}}} }

			plain := newRun(t, config.Default(), "without the limit")
			wantOut, wantErr := plain.dispatcher(t, node()).Run(context.Background())

			limited := newRun(t, config.Default(), "with the limit")
			gotOut, gotErr := limited.pausingAfter(t, 1, node()).Run(context.Background())

			if gotOut != wantOut {
				t.Errorf("Outcome with PauseAfter 1 = %+v, want %+v", gotOut, wantOut)
			}
			if (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) {
				t.Errorf("error with PauseAfter 1 = %v, want %v", gotErr, wantErr)
			}
			if got, want := limited.timeline(t), plain.timeline(t); !equalStrings(got, want) {
				t.Errorf("timeline with PauseAfter 1 =\n  %v\nwant\n  %v", got, want)
			}
		})
	}
}

func TestNewDispatcherRefusesANegativePauseAfter(t *testing.T) {
	h := newRun(t, config.Default(), "refuse")
	reg := graph.NewRegistry()
	reg.MustRegister(ok(graph.NodePlan, graph.End))
	_, err := graph.NewDispatcher(graph.Options{Store: h.store, Registry: reg, Config: h.cfg, PauseAfter: -1})
	if !errors.Is(err, graph.ErrInvalidOptions) {
		t.Errorf("NewDispatcher(PauseAfter: -1) error = %v, want ErrInvalidOptions", err)
	}
}
