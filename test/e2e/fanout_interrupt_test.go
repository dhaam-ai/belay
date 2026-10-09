//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/isolate/dircopy"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Interruptions that land between a candidate's workspace being made and its
// first node starting. Its run has a journal by then, but one that names no
// node: run_started and run_paused after a Ctrl-C, run_started alone after a
// kill. Each must resume into the candidate's own first node and converge on
// what a run nothing interrupted settles.

// fanoutCancelAfterCreate is the dircopy isolator, except that making the
// workspace of target is followed by a Ctrl-C: the run's context is
// cancelled as Create returns, before the candidate's run has started a node.
type fanoutCancelAfterCreate struct {
	*dircopy.Isolator
	target string
	once   sync.Once
	cancel context.CancelFunc
}

// Create implements belay.Isolator.
func (c *fanoutCancelAfterCreate) Create(ctx context.Context, src, id string) (belay.Workspace, error) {
	ws, err := c.Isolator.Create(ctx, src, id)
	if err == nil && id == c.target {
		c.once.Do(c.cancel)
	}
	return ws, err
}

// fanoutCancelAfterAll cancels the run once want workspaces have been made: a
// Ctrl-C that lands as the last copy finishes, with the earlier candidates
// possibly already inside their first node.
type fanoutCancelAfterAll struct {
	*dircopy.Isolator
	mu     sync.Mutex
	made   int
	want   int
	cancel context.CancelFunc
}

// Create implements belay.Isolator.
func (b *fanoutCancelAfterAll) Create(ctx context.Context, src, id string) (belay.Workspace, error) {
	ws, err := b.Isolator.Create(ctx, src, id)
	if err == nil {
		b.mu.Lock()
		b.made++
		if b.made == b.want {
			b.cancel()
		}
		b.mu.Unlock()
	}
	return ws, err
}

// isolatedDispatcher is h.dispatcher with iso as the isolator and
// concurrency candidates at a time.
func (h *fanoutHarness) isolatedDispatcher(world *fanoutWorld, iso belay.Isolator, concurrency int) *graph.Dispatcher {
	h.t.Helper()
	parent, err := nodes.Default(h.cfg, nodes.WithFanout())
	if err != nil {
		h.t.Fatalf("nodes.Default() error = %v", err)
	}
	candidates, err := nodes.Candidate()
	if err != nil {
		h.t.Fatalf("nodes.Candidate() error = %v", err)
	}
	tools := fanoutScriptedTools()
	d, err := graph.NewDispatcher(graph.Options{
		Store: state.NewStore(h.layout), Registry: parent, Config: h.cfg,
		Logger:            fanoutQuietLogger(),
		Agent:             world.planner(),
		CandidateRegistry: candidates,
		CandidateAgent:    world.candidate,
		Concurrency:       concurrency,
		Runner:            tools.runner,
		Linter:            tools.linter,
		Isolator:          iso,
		PauseOnCancel:     true,
	})
	if err != nil {
		h.t.Fatalf("graph.NewDispatcher() error = %v", err)
	}
	return d
}

// fanoutDircopy is a plain directory-copy isolator over h's candidates.
func (h *fanoutHarness) fanoutDircopy() *dircopy.Isolator {
	h.t.Helper()
	iso, err := dircopy.New(h.layout.CandidatesDir(), dircopy.WithLogger(fanoutQuietLogger()))
	if err != nil {
		h.t.Fatalf("dircopy.New() error = %v", err)
	}
	return iso
}

// fanoutControl runs a fanout nothing interrupts, and returns what it settled.
func fanoutControl(t *testing.T) fanoutOutcome {
	t.Helper()
	control := newFanoutRun(t)
	out, err := control.dispatcher(newFanoutWorld(), fanoutScriptedTools(), nil).Run(context.Background())
	if err != nil || out.Status != state.RunStatusCompleted {
		t.Fatalf("the control run: status %v, error %v (note %q)", out.Status, err, out.Note)
	}
	return fanoutOutcomeOf(t, control)
}

// fanoutResumeConverges resumes h with nothing interrupting it, and checks it
// settles exactly what the control did, with no candidate run ended failed by
// anything but its own work.
func fanoutResumeConverges(t *testing.T, h *fanoutHarness, control fanoutOutcome) {
	t.Helper()
	out, err := h.isolatedDispatcher(newFanoutWorld(), h.fanoutDircopy(), 1).Resume(context.Background())
	if err != nil || out.Status != state.RunStatusCompleted {
		t.Fatalf("Resume(): status %v, error %v (note %q), want completed", out.Status, err, out.Note)
	}
	for _, id := range fanoutIDs {
		recs := fanoutRecords(t, h.candidateLayout(id).JournalPath())
		if err := journal.Validate(recs); err != nil {
			t.Errorf("%s: its journal does not validate after the resume: %v", id, err)
		}
		for _, r := range recs {
			if r.Event == journal.EventRunFailed && id != "cand-05" {
				t.Errorf("%s: its run ended failed (%q), but nothing about its work failed", id, r.Note)
			}
		}
	}
	if got := fanoutOutcomeOf(t, h); got.winner != control.winner || !slices.Equal(got.candidates, control.candidates) {
		t.Errorf("after the interruption the fanout settled\n  %v (winner %q)\nwant the control's\n  %v (winner %q)",
			got.candidates, got.winner, control.candidates, control.winner)
	}
}

// fanoutQuietLogger discards everything, as h.dispatcher's default does.
func fanoutQuietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestFanoutCtrlCRightAfterAWorkspaceIsMadeConverges: candidates are admitted
// one at a time, so cand-01 and cand-02 finish and the Ctrl-C lands as
// cand-03's workspace is made. cand-03's journal is left holding run_started
// and run_paused and nothing else.
func TestFanoutCtrlCRightAfterAWorkspaceIsMadeConverges(t *testing.T) {
	control := fanoutControl(t)

	h := newFanoutRun(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	iso := &fanoutCancelAfterCreate{Isolator: h.fanoutDircopy(), target: "cand-03", cancel: cancel}
	out, err := h.isolatedDispatcher(newFanoutWorld(), iso, 1).Run(ctx)
	if out.Status != state.RunStatusPaused {
		t.Fatalf("the interrupted dispatch: status %v, error %v, want paused", out.Status, err)
	}
	if walked := fanoutWalk(fanoutRecords(t, h.candidateLayout("cand-03").JournalPath())); len(walked) != 0 {
		t.Fatalf("cand-03 started %v before the Ctrl-C reached it; the scenario needs it to have started nothing", walked)
	}

	fanoutResumeConverges(t, h, control)
}

// TestFanoutCtrlCAsTheLastCopyIsMadeConverges: all five candidates are
// admitted together and the Ctrl-C lands as the fifth copy is made. Every
// candidate whose copy was made last has a journal that names no node.
func TestFanoutCtrlCAsTheLastCopyIsMadeConverges(t *testing.T) {
	control := fanoutControl(t)

	h := newFanoutRun(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	iso := &fanoutCancelAfterAll{Isolator: h.fanoutDircopy(), want: len(fanoutIDs), cancel: cancel}
	if out, err := h.isolatedDispatcher(newFanoutWorld(), iso, len(fanoutIDs)).Run(ctx); out.Status != state.RunStatusPaused {
		t.Fatalf("the interrupted dispatch: status %v, error %v, want paused", out.Status, err)
	}

	fanoutResumeConverges(t, h, control)
}

// TestFanoutKillRightAfterACandidateRunStartsConverges is the same window
// closed by a kill rather than a Ctrl-C: cand-03's journal holds run_started
// alone, which is what a SIGKILL between its first record and its first
// node leaves. The run is first brought to that moment with a Ctrl-C, and
// the record only a living process could have written after it is taken
// away.
func TestFanoutKillRightAfterACandidateRunStartsConverges(t *testing.T) {
	control := fanoutControl(t)

	h := newFanoutRun(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	iso := &fanoutCancelAfterCreate{Isolator: h.fanoutDircopy(), target: "cand-03", cancel: cancel}
	if out, err := h.isolatedDispatcher(newFanoutWorld(), iso, 1).Run(ctx); out.Status != state.RunStatusPaused {
		t.Fatalf("the interrupted dispatch: status %v, error %v, want paused", out.Status, err)
	}

	path := h.candidateLayout("cand-03").JournalPath()
	body, err := os.ReadFile(path) //nolint:gosec // a path this test's run made
	if err != nil {
		t.Fatalf("read cand-03's journal: %v", err)
	}
	first, _, found := bytes.Cut(body, []byte("\n"))
	if !found {
		t.Fatalf("cand-03's journal %q has no complete record", body)
	}
	if err := os.WriteFile(path, append(first, '\n'), 0o600); err != nil {
		t.Fatalf("rewrite cand-03's journal: %v", err)
	}
	if recs := fanoutRecords(t, path); len(recs) != 1 || recs[0].Event != journal.EventRunStarted {
		t.Fatalf("cand-03's journal holds %v, want run_started alone", recs)
	}

	fanoutResumeConverges(t, h, control)
}
