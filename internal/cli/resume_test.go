package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/nodes/approve"
	"github.com/belay-dev/belay/internal/state"
)

// seedManifest writes a run directory holding only a manifest, which is all
// choosing between runs needs to read.
func seedManifest(t *testing.T, workspace, runID string, status state.RunStatus, node, goal string, updated time.Time) {
	t.Helper()
	layout, err := state.NewLayout(workspace, runID)
	if err != nil {
		t.Fatalf("state.NewLayout(%q): %v", runID, err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		t.Fatalf("create run dir: %v", err)
	}
	manifest := state.NewManifest(updated, runID, workspace, goal, config.Default())
	manifest.Status = status
	manifest.CurrentNode = node
	manifest.UpdatedAt = updated
	if err := state.SaveManifest(layout.ManifestPath(), manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

func TestScanRunsForResumeOrdersNewestFirst(t *testing.T) {
	ws := t.TempDir()
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	seedManifest(t, ws, "20260905T120000Z-aaaaaaaaaaaa", state.RunStatusCompleted, graph.End, "old and done", base)
	seedManifest(t, ws, "20260905T130000Z-bbbbbbbbbbbb", state.RunStatusPaused, graph.NodeApprove, "waiting", base.Add(time.Hour))
	seedManifest(t, ws, "20260905T140000Z-cccccccccccc", state.RunStatusRunning, graph.NodeTest, "crashed", base.Add(2*time.Hour))
	// A directory with no manifest is skipped rather than reported: a run
	// belay cannot read is a run it must not resume.
	if err := os.MkdirAll(filepath.Join(ws, ".belay", "runs", "20260905T150000Z-dddddddddddd"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	runs, err := scanRunsForResume(ws)
	if err != nil {
		t.Fatalf("scanRunsForResume: %v", err)
	}

	got := make([]string, 0, len(runs))
	for _, r := range runs {
		got = append(got, r.id)
	}
	want := []string{
		"20260905T140000Z-cccccccccccc",
		"20260905T130000Z-bbbbbbbbbbbb",
		"20260905T120000Z-aaaaaaaaaaaa",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("scanRunsForResume() = %v, want newest first %v", got, want)
	}
}

func TestScanRunsForResumeWithoutARunsDirectory(t *testing.T) {
	runs, err := scanRunsForResume(t.TempDir())
	if err != nil {
		t.Fatalf("a workspace belay has never run in is not an error: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("got %d runs, want none", len(runs))
	}
}

func TestChooseRun(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	t.Run("picks the most recent waiting run and names it", func(t *testing.T) {
		ws := t.TempDir()
		seedManifest(t, ws, "20260905T120000Z-aaaaaaaaaaaa", state.RunStatusPaused, graph.NodeApprove, "older", base)
		seedManifest(t, ws, "20260905T130000Z-bbbbbbbbbbbb", state.RunStatusPaused, graph.NodeApprove, "newer", base.Add(time.Hour))
		// Newest of all, but finished: it must not be chosen.
		seedManifest(t, ws, "20260905T140000Z-cccccccccccc", state.RunStatusCompleted, graph.End, "done", base.Add(2*time.Hour))

		chosen, err := chooseRun(ws, "")
		if err != nil {
			t.Fatalf("chooseRun: %v", err)
		}
		if chosen.id != "20260905T130000Z-bbbbbbbbbbbb" {
			t.Errorf("chose %s, want the most recent run that was still waiting", chosen.id)
		}

		said := chosen.describe(true)
		for _, phrase := range []string{
			"20260905T130000Z-bbbbbbbbbbbb",
			"approve (waiting for you to read the plan and approve it)",
			"the most recent run that was still waiting",
		} {
			if !strings.Contains(said, phrase) {
				t.Errorf("the choice does not say %q; got %q", phrase, said)
			}
		}
	})

	t.Run("a crashed run is resumable", func(t *testing.T) {
		ws := t.TempDir()
		seedManifest(t, ws, "20260905T120000Z-aaaaaaaaaaaa", state.RunStatusRunning, graph.NodeTest, "crashed", base)

		chosen, err := chooseRun(ws, "")
		if err != nil {
			t.Fatalf("chooseRun: %v", err)
		}
		if !strings.Contains(chosen.describe(true), "interrupted part-way through test (running your tests)") {
			t.Errorf("a crashed run should be explained, got %q", chosen.describe(true))
		}
	})

	t.Run("nothing to resume says so plainly", func(t *testing.T) {
		ws := t.TempDir()
		_, err := chooseRun(ws, "")
		if err == nil {
			t.Fatal("an empty workspace has nothing to continue")
		}
		for _, phrase := range []string{"has not run in", ws, `belay run "what you want built"`} {
			if !strings.Contains(err.Error(), phrase) {
				t.Errorf("message does not say %q; got %q", phrase, err)
			}
		}
	})

	t.Run("every run already ended says so plainly", func(t *testing.T) {
		ws := t.TempDir()
		seedManifest(t, ws, "20260905T120000Z-aaaaaaaaaaaa", state.RunStatusCompleted, graph.End, "done", base)

		_, err := chooseRun(ws, "")
		if err == nil {
			t.Fatal("a workspace of finished runs has nothing to continue")
		}
		for _, phrase := range []string{"every one of them has already ended", "belay runs"} {
			if !strings.Contains(err.Error(), phrase) {
				t.Errorf("message does not say %q; got %q", phrase, err)
			}
		}
	})

	t.Run("a finished run named outright is refused in plain language", func(t *testing.T) {
		ws := t.TempDir()
		seedManifest(t, ws, "20260905T120000Z-aaaaaaaaaaaa", state.RunStatusCompleted, graph.End, "done", base)

		_, err := chooseRun(ws, "20260905T120000Z-aaaaaaaaaaaa")
		if err == nil {
			t.Fatal("a completed run cannot be continued")
		}
		for _, phrase := range []string{"cannot continue run", "already finished", "Start a new run"} {
			if !strings.Contains(err.Error(), phrase) {
				t.Errorf("message does not say %q; got %q", phrase, err)
			}
		}
		if strings.Contains(err.Error(), "RunStatusCompleted") {
			t.Errorf("message leaks a Go identifier: %q", err)
		}
	})

	t.Run("an unknown id lists what there is", func(t *testing.T) {
		ws := t.TempDir()
		seedManifest(t, ws, "20260905T120000Z-aaaaaaaaaaaa", state.RunStatusPaused, graph.NodeApprove, "waiting", base)

		_, err := chooseRun(ws, "nosuchrun")
		if err == nil {
			t.Fatal("an unknown run id must be reported")
		}
		for _, phrase := range []string{"no run called nosuchrun", "It does have:", "20260905T120000Z-aaaaaaaaaaaa"} {
			if !strings.Contains(err.Error(), phrase) {
				t.Errorf("message does not say %q; got %q", phrase, err)
			}
		}
	})
}

// TestResumeWithNoArgumentSaysWhichRunItChose is acceptance criterion 5,
// through the command a person types.
func TestResumeWithNoArgumentSaysWhichRunItChose(t *testing.T) {
	ws := goWorkspace(t)
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	seedManifest(t, ws, "20260905T120000Z-aaaaaaaaaaaa", state.RunStatusPaused, graph.NodeApprove, "older", base)
	seedManifest(t, ws, "20260905T130000Z-bbbbbbbbbbbb", state.RunStatusPaused, graph.NodeApprove, "add tests", base.Add(time.Hour))

	// --dry-run stops before it would drive the dispatcher, which is what
	// keeps this test from calling an agent.
	out, _, err := runCLI(t, "--dry-run", "resume", "--workspace", ws)
	if err != nil {
		t.Fatalf("belay --dry-run resume: %v\n%s", err, out)
	}
	assertPlainText(t, "resume", out)

	for _, phrase := range []string{
		"which run it would continue",
		"20260905T130000Z-bbbbbbbbbbbb",
		"the most recent run that was still waiting",
		"add tests",
	} {
		if !strings.Contains(out, phrase) {
			t.Errorf("resume never says %q; got:\n%s", phrase, out)
		}
	}
	if strings.Contains(out, "20260905T120000Z-aaaaaaaaaaaa") {
		t.Errorf("resume picked or mentioned the older run:\n%s", out)
	}
}

func TestResumeWithNothingToContinue(t *testing.T) {
	ws := goWorkspace(t)

	_, _, err := runCLI(t, "resume", "--workspace", ws)
	if err == nil {
		t.Fatal("belay resume must refuse when there is nothing to continue")
	}
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != graph.ExitFailed {
		t.Fatalf("error = %#v, want an ExitError with code %d", err, graph.ExitFailed)
	}
	for _, phrase := range []string{"has not run in", ws, `belay run "what you want built"`} {
		if !strings.Contains(exit.Message, phrase) {
			t.Errorf("message does not say %q; got %q", phrase, exit.Message)
		}
	}
}

// planWriter stands in for the plan node, which would call the agent. It
// writes the artifact the approve gate reads and routes to that gate.
type planWriter struct{ body string }

func (planWriter) Name() string { return graph.NodePlan }

func (n planWriter) Run(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
	path, err := rc.WriteArtifact("plan.md", []byte(n.body))
	if err != nil {
		return graph.Result{Status: journal.StatusFailed}, err
	}
	plan := rc.State.Plan
	plan.Path = path
	return graph.Result{
		Status: journal.StatusOK,
		Next:   graph.NodeApprove,
		Patch:  state.Patch{Plan: &plan},
		Note:   "wrote the plan",
	}, nil
}

// endsTheRun stands in for the code node and everything after it.
type endsTheRun struct{ reached *bool }

func (endsTheRun) Name() string { return graph.NodeCode }

func (n endsTheRun) Run(context.Context, *graph.RunContext) (graph.Result, error) {
	*n.reached = true
	return graph.Result{Status: journal.StatusOK, Next: graph.End, Note: "done"}, nil
}

// TestResumingApprovesThePlanAndPicksUpTheEditedFile drives the real approve
// gate through the exact sequence `belay run` and `belay resume` perform: a
// run that pauses at the gate, a human editing the plan, and a resume that
// approves it and moves on.
func TestResumingApprovesThePlanAndPicksUpTheEditedFile(t *testing.T) {
	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	reachedCode := false
	registry := graph.NewRegistry()
	registry.MustRegister(
		planWriter{body: "# Plan\n\nwrite a test\n"},
		approve.New(),
		endsTheRun{reached: &reachedCode},
	)
	plan.registry = registry

	store := seedRunDir(t, plan, "add tests")
	dispatcher, err := graph.NewDispatcher(plan.options(store))
	if err != nil {
		t.Fatalf("graph.NewDispatcher: %v", err)
	}

	// First invocation: plan, then pause at the gate.
	outcome, err := dispatcher.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Status != state.RunStatusPaused || outcome.Node != graph.NodeApprove {
		t.Fatalf("outcome = %+v, want a pause at the approval gate", outcome)
	}
	if outcome.ExitCode != graph.ExitOK {
		t.Errorf("a pause is a successful invocation; exit code = %d, want %d", outcome.ExitCode, graph.ExitOK)
	}
	if reachedCode {
		t.Fatal("nothing after the gate may run before a person approves")
	}

	// A plain `belay run` must refuse to step past the pause.
	paused, err := dispatcher.Run(context.Background())
	if !errors.Is(err, graph.ErrRunPaused) {
		t.Fatalf("Run on a paused run = %v, want ErrRunPaused", err)
	}
	if paused.ExitCode != graph.ExitPaused {
		t.Errorf("exit code = %d, want %d", paused.ExitCode, graph.ExitPaused)
	}

	// The human edits the plan during the pause, which is the whole point of
	// the gate.
	planPath := filepath.Join(store.Layout().ArtifactsDir(), "plan.md")
	if err := os.WriteFile(planPath, []byte("# Plan\n\nwrite two tests, not one\n"), 0o600); err != nil {
		t.Fatalf("edit the plan: %v", err)
	}

	// What `belay resume` does before dispatching.
	approved, err := approvePlan(store)
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !approved {
		t.Fatal("approvePlan should have had something to record")
	}

	final, err := dispatcher.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !reachedCode {
		t.Error("resuming past the gate must let the run continue")
	}
	if final.Status != state.RunStatusCompleted || final.ExitCode != graph.ExitOK {
		t.Errorf("outcome = %+v, want a completed run", final)
	}

	// The gate re-read the file, so the digest on the blackboard describes
	// what the agent will actually be given.
	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !st.Plan.Approved {
		t.Error("state should record the approval")
	}
	edited, err := os.ReadFile(planPath) //nolint:gosec // a path this test wrote itself, inside t.TempDir()
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if st.Plan.Digest != approve.Digest(edited) {
		t.Errorf("plan digest = %q, want the fingerprint of the edited file", st.Plan.Digest)
	}
}

func TestApprovePlanIsIdempotent(t *testing.T) {
	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	store := seedRunDir(t, plan, "add tests")

	first, err := approvePlan(store)
	if err != nil || !first {
		t.Fatalf("approvePlan() = (%v, %v), want (true, nil)", first, err)
	}
	second, err := approvePlan(store)
	if err != nil {
		t.Fatalf("approvePlan (second): %v", err)
	}
	if second {
		t.Error("approving an already-approved plan should report that nothing changed")
	}
}
