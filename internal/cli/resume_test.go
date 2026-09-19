package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/state"
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
	// A directory with no manifest is kept, carrying the reason it cannot be
	// read, so that naming it says what is wrong with it rather than that it
	// does not exist. It has no readable update time, so it sorts last.
	const damaged = "20260905T150000Z-dddddddddddd"
	if err := os.MkdirAll(filepath.Join(ws, ".belay", "runs", damaged), 0o750); err != nil {
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
		damaged,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("scanRunsForResume() = %v, want newest first %v", got, want)
	}
	for _, r := range runs {
		if wantUnreadable := r.id == damaged; (r.unreadable != nil) != wantUnreadable {
			t.Errorf("run %s: unreadable = %v, want a reason: %v", r.id, r.unreadable, wantUnreadable)
		}
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

// seedNewerRun writes a run directory whose manifest.json this belay cannot
// read because a newer belay wrote it: it requires a capability this belay
// lacks, or, when capabilities is empty, it is at schema version 99.
func seedNewerRun(t *testing.T, workspace, runID string, updated time.Time, capabilities ...string) {
	t.Helper()
	layout, err := state.NewLayout(workspace, runID)
	if err != nil {
		t.Fatalf("state.NewLayout(%q): %v", runID, err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		t.Fatalf("create run dir: %v", err)
	}
	if len(capabilities) == 0 {
		raw := []byte(`{"schema_version": 99, "run_id": "` + runID + `", "status": "paused"}`)
		if err := os.WriteFile(layout.ManifestPath(), raw, 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		return
	}
	manifest := state.NewManifest(updated, runID, workspace, "a newer belay's goal", config.Default())
	manifest.Status = state.RunStatusPaused
	manifest.CurrentNode = graph.NodeApprove
	manifest.Requires = capabilities
	if err := state.SaveManifest(layout.ManifestPath(), manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

// TestResumeRefusesARunThatNeedsANewerBelay covers a run a newer belay
// wrote (ADR-0011). belay resume must refuse it by name with the error that
// says to upgrade, whether the person named it or left belay to choose.
// Skipping it is wrong both ways: named, belay said it had no such run; left
// to choose, it would continue an older run that happened to be readable,
// and spend on a run the person may not have meant.
func TestResumeRefusesARunThatNeedsANewerBelay(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	const (
		waiting = "20260905T120000Z-aaaaaaaaaaaa"
		newer   = "20260905T130000Z-bbbbbbbbbbbb"
	)

	tests := []struct {
		name         string
		capabilities []string
		named        bool
		want         []string
		wantAs       func(error) bool
	}{
		{
			name:         "named, requiring a capability this belay lacks",
			capabilities: []string{"output.branch"},
			named:        true,
			want:         []string{"cannot continue run " + newer, "needs a newer belay", `"output.branch"`, "upgrade belay"},
			wantAs:       isCapabilityError,
		},
		{
			name:   "named, at a schema version above this belay's",
			named:  true,
			want:   []string{"cannot continue run " + newer, "needs a newer belay", "schema version 99", "upgrade belay"},
			wantAs: isNewerVersionError,
		},
		{
			name:         "not named, requiring a capability this belay lacks",
			capabilities: []string{"output.branch"},
			want:         []string{newer, "needs a newer belay", `"output.branch"`, "upgrade belay", "belay resume <run-id>"},
			wantAs:       isCapabilityError,
		},
		{
			name:   "not named, at a schema version above this belay's",
			want:   []string{newer, "needs a newer belay", "schema version 99", "upgrade belay", "belay resume <run-id>"},
			wantAs: isNewerVersionError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := goWorkspace(t)
			// A readable run still waiting, which belay could continue:
			// refusing must not be because there is nothing else.
			seedManifest(t, ws, waiting, state.RunStatusPaused, graph.NodeApprove, "readable and waiting", base)
			seedNewerRun(t, ws, newer, base.Add(time.Hour), tt.capabilities...)

			args := []string{"--dry-run", "resume", "--workspace", ws}
			if tt.named {
				args = append(args, newer)
			}
			out, _, err := runCLI(t, args...)
			if err == nil {
				t.Fatalf("belay resume continued a run instead of refusing the one that needs a newer belay:\n%s", out)
			}
			var exit *ExitError
			if !errors.As(err, &exit) || exit.Code != graph.ExitFailed {
				t.Fatalf("error = %#v, want an ExitError with code %d", err, graph.ExitFailed)
			}
			for _, phrase := range tt.want {
				if !strings.Contains(exit.Message, phrase) {
					t.Errorf("message does not say %q; got %q", phrase, exit.Message)
				}
			}
			for _, phrase := range []string{"damaged", "no run called"} {
				if strings.Contains(exit.Message, phrase) {
					t.Errorf("message says %q of a run a newer belay wrote; got %q", phrase, exit.Message)
				}
			}
			if !tt.wantAs(err) {
				t.Errorf("error does not carry the state package's error for the run: %v", err)
			}
		})
	}
}

// emptyCassette writes the recording a replayed run reads, with nothing in
// it. A run wired this way cannot call a real agent: the first request the
// code node makes fails with replay.ErrNoInteraction instead. It is how a
// command-level test can let `belay resume` walk past a gate it should have
// stopped at without spending anything to prove it.
func emptyCassette(t *testing.T, workspace string) {
	t.Helper()
	dir := filepath.Join(workspace, ".belay")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	const body = `{"format_version":1,"provenance":{},"interactions":[]}`
	if err := os.WriteFile(filepath.Join(dir, defaultCassetteName), []byte(body), 0o600); err != nil {
		t.Fatalf("write cassette: %v", err)
	}
}

// isCapabilityError reports whether err wraps a *state.CapabilityError.
func isCapabilityError(err error) bool {
	var target *state.CapabilityError
	return errors.As(err, &target)
}

// isNewerVersionError reports whether err wraps a *state.ManifestVersionError
// for a version a newer belay wrote.
func isNewerVersionError(err error) bool {
	var target *state.ManifestVersionError
	return errors.As(err, &target) && target.Newer()
}

// TestResumeSaysADamagedRunIsDamaged: a run belay cannot read is not a run
// that does not exist. `belay resume <id>` skipped it and reported "belay has
// no run called <id>", which sends a person looking for a typo in an id they
// copied from `belay runs` — where the same run is listed, with its damage.
// It must say the run is there, what is wrong with it, and where to look.
func TestResumeSaysADamagedRunIsDamaged(t *testing.T) {
	const damaged = "20260905T130000Z-bbbbbbbbbbbb"

	tests := []struct {
		name     string
		manifest []byte
		want     []string
	}{
		{
			name:     "manifest.json is not valid JSON",
			manifest: []byte("{not json"),
			want:     []string{"cannot continue run " + damaged, "manifest.json is damaged"},
		},
		{
			name: "the run directory has no manifest.json",
			want: []string{"cannot continue run " + damaged, "no manifest.json"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := goWorkspace(t)
			base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			// A readable run still waiting: the refusal must be about the run
			// that was named, not about having nothing to continue.
			seedManifest(t, ws, "20260905T120000Z-aaaaaaaaaaaa", state.RunStatusPaused, graph.NodeApprove, "waiting", base)
			dir := filepath.Join(ws, ".belay", "runs", damaged)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if tt.manifest != nil {
				if err := os.WriteFile(filepath.Join(dir, "manifest.json"), tt.manifest, 0o600); err != nil {
					t.Fatalf("write manifest: %v", err)
				}
			}

			out, _, err := runCLI(t, "--dry-run", "resume", damaged, "--workspace", ws)
			if err == nil {
				t.Fatalf("belay resume continued a run it cannot read:\n%s", out)
			}
			var exit *ExitError
			if !errors.As(err, &exit) || exit.Code != graph.ExitFailed {
				t.Fatalf("error = %#v, want an ExitError with code %d", err, graph.ExitFailed)
			}
			for _, phrase := range append(tt.want, dir, "belay runs") {
				if !strings.Contains(exit.Message, phrase) {
					t.Errorf("message does not say %q; got %q", phrase, exit.Message)
				}
			}
			if strings.Contains(exit.Message, "no run called") {
				t.Errorf("message says the run does not exist; it does, and it is damaged: %q", exit.Message)
			}
		})
	}
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

// Continuing a run says which route it walks, as starting one does: a person
// resuming days later needs to see where the run is in it before it spends.
func TestAnnounceContinueShowsTheRoute(t *testing.T) {
	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const runID = "20260905T130000Z-bbbbbbbbbbbb"
	layout, err := state.NewLayout(ws, runID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	var out bytes.Buffer
	announceContinue(&ui{out: &out, err: &out}, plan, resumableRun{
		id: runID, goal: "add tests", status: state.RunStatusPaused, node: graph.NodeApprove,
	}, layout, false)

	const want = "route:  plan → approve → code → write → test → fix → review → done"
	if !strings.Contains(out.String(), want) {
		t.Errorf("continuing a run never says %q; got:\n%s", want, out.String())
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

// pressesCtrlC stands in for a person pressing Ctrl-C while this node runs.
// Cancelling the context the dispatcher was given reaches it exactly as the
// signal handler's cancellation does. The node then returns what a node that
// honours its context returns.
type pressesCtrlC struct {
	name   string
	cancel context.CancelFunc
}

func (n pressesCtrlC) Name() string { return n.name }

func (n pressesCtrlC) Run(ctx context.Context, _ *graph.RunContext) (graph.Result, error) {
	n.cancel()
	<-ctx.Done()
	return graph.Result{}, ctx.Err()
}

// TestResumeApprovesOnlyAPlanTheGateAskedAbout pins when resuming a run is
// the approval of its plan. It is when the approve gate paused the run to ask.
// It is not when Ctrl-C landed while the gate was running: the run is paused
// at approve then too, but nobody was shown the plan or told that resuming
// approves it, so resuming must run the gate again, and the gate asks.
func TestResumeApprovesOnlyAPlanTheGateAskedAbout(t *testing.T) {
	tests := []struct {
		name string
		gate func(cancel context.CancelFunc) graph.Node
		want bool
	}{
		{
			name: "the gate paused to ask",
			gate: func(context.CancelFunc) graph.Node { return approve.New() },
			want: true,
		},
		{
			name: "Ctrl-C landed while the gate ran",
			gate: func(cancel context.CancelFunc) graph.Node {
				return pressesCtrlC{name: graph.NodeApprove, cancel: cancel}
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := goWorkspace(t)
			plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reached := false
			registry := graph.NewRegistry()
			registry.MustRegister(
				planWriter{body: "# Plan\n\nwrite a test\n"},
				tt.gate(cancel),
				endsTheRun{reached: &reached},
			)
			plan.registry = registry
			store := seedRunDir(t, plan, "add tests")

			opts := plan.options(store)
			opts.PauseOnCancel = true
			dispatcher, err := graph.NewDispatcher(opts)
			if err != nil {
				t.Fatalf("graph.NewDispatcher: %v", err)
			}
			if _, err := dispatcher.Run(ctx); err != nil && !errors.Is(err, graph.ErrInterrupted) {
				t.Fatalf("Run: %v", err)
			}

			chosen, err := chooseRun(ws, store.Layout().RunID())
			if err != nil {
				t.Fatalf("chooseRun: %v", err)
			}
			// Both runs look the same to anything that reads only the
			// manifest, which is why awaitsApproval cannot.
			if chosen.status != state.RunStatusPaused || chosen.node != graph.NodeApprove {
				t.Fatalf("the run is %v at %q, want paused at %q", chosen.status, chosen.node, graph.NodeApprove)
			}

			got, err := awaitsApproval(chosen, store.Layout())
			if err != nil {
				t.Fatalf("awaitsApproval: %v", err)
			}
			if got != tt.want {
				t.Errorf("awaitsApproval() = %v, want %v", got, tt.want)
			}
		})
	}
}

// downgradeToVersion1 rewrites a run document as belay v0.1.x wrote it:
// schema_version 1, and no requires list, which is the shape those binaries
// read and the only shape they will resume.
func downgradeToVersion1(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a run document this test wrote itself, under t.TempDir()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	doc["schema_version"] = json.RawMessage(`1`)
	delete(doc, "requires")
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestResumeUpgradesTheManifestBeforeItRecordsAnApproval pins the order the
// two documents of a version-1 run are upgraded in when `belay resume`
// approves its plan.
//
// Recording the approval saves state.json, which saves it at version 2. A
// crash between that and the manifest's own upgrade leaves belay v0.1.x a
// version-1 manifest — which it accepts — over a version-2 state.json, and it
// resumes the run and writes into it (ADR-0011). So the manifest is saved
// first, before belay writes anything else to the run.
//
// The run directory is made read-only to prove the ordering: both writes then
// fail, and which one belay reports is which one it tried first.
func TestResumeUpgradesTheManifestBeforeItRecordsAnApproval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory is still writable, so there is no failure to order")
	}
	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	registry := graph.NewRegistry()
	registry.MustRegister(planWriter{body: "# Plan\n\nwrite a test\n"}, approve.New())
	plan.registry = registry
	store := seedRunDir(t, plan, "add tests")

	// The gate's own pause, so resuming is the approval.
	dispatcher, err := graph.NewDispatcher(plan.options(store))
	if err != nil {
		t.Fatalf("graph.NewDispatcher: %v", err)
	}
	if _, err := dispatcher.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	layout := store.Layout()
	downgradeToVersion1(t, layout.StatePath())
	downgradeToVersion1(t, layout.ManifestPath())
	// #nosec G302 -- read-only is the point: these are the modes that make
	// the run directory unwritable and then writable again for the cleanup,
	// on a directory inside t.TempDir().
	if err := os.Chmod(layout.RunDir(), 0o500); err != nil {
		t.Fatalf("chmod the run directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(layout.RunDir(), 0o750) }) // #nosec G302 -- as above, so t.TempDir can clean up.

	out, _, err := runCLI(t, "resume", layout.RunID(), "--workspace", ws)
	if err == nil {
		t.Fatalf("belay resume wrote to a read-only run directory:\n%s", out)
	}
	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("error = %#v, want an ExitError", err)
	}
	if !strings.Contains(exit.Message, "manifest.json") {
		t.Errorf("belay resume failed on %q; the manifest must be saved at the current version "+
			"before the approval is recorded", exit.Message)
	}
}

// finishedANode reports whether the run's journal already records a node
// finishing. A test uses it from the dispatcher's clock to cancel the run in
// the window between two nodes: the plan's completion is durable, so its
// result stands, and the next node has not been started.
func finishedANode(t *testing.T, layout state.Layout) bool {
	t.Helper()
	read, err := journal.ReadFile(layout.JournalPath())
	if err != nil {
		return false
	}
	for _, rec := range read.Records {
		if rec.Event == journal.EventNodeFinished {
			return true
		}
	}
	return false
}

// TestResumeDoesNotApproveAPlanTheGateNeverReached covers the two ways a run
// can be paused at the approve gate without the gate ever having asked
// anything, neither of which TestResumeApprovesOnlyAPlanTheGateAskedAbout can
// produce: a Ctrl-C that lands between the plan and the gate, which pauses the
// run before the gate starts, and a journal in which no node has finished at
// all. Resuming either must run the gate, not approve on the person's behalf.
func TestResumeDoesNotApproveAPlanTheGateNeverReached(t *testing.T) {
	t.Run("Ctrl-C landed between the plan and the gate", func(t *testing.T) {
		ws := goWorkspace(t)
		plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		registry := graph.NewRegistry()
		registry.MustRegister(planWriter{body: "# Plan\n\nwrite a test\n"}, approve.New())
		plan.registry = registry
		store := seedRunDir(t, plan, "add tests")

		opts := plan.options(store)
		opts.PauseOnCancel = true
		// Cancel at the first reading of the clock after the plan's
		// completion is in the journal. That is the window between two
		// nodes: the plan's result has been applied and recorded, and the
		// gate has not been started, so the dispatcher pauses the run at the
		// gate with no attempt opened.
		opts.Now = func() time.Time {
			if finishedANode(t, store.Layout()) {
				cancel()
			}
			return time.Now().UTC()
		}
		dispatcher, err := graph.NewDispatcher(opts)
		if err != nil {
			t.Fatalf("graph.NewDispatcher: %v", err)
		}
		if _, err := dispatcher.Run(ctx); err != nil && !errors.Is(err, graph.ErrInterrupted) {
			t.Fatalf("Run: %v", err)
		}

		chosen, err := chooseRun(ws, store.Layout().RunID())
		if err != nil {
			t.Fatalf("chooseRun: %v", err)
		}
		if chosen.status != state.RunStatusPaused || chosen.node != graph.NodeApprove {
			t.Fatalf("the run is %v at %q, want paused at %q before the gate ran",
				chosen.status, chosen.node, graph.NodeApprove)
		}
		// No attempt was opened at the gate, so the run's last node_finished
		// is the plan's.
		got, err := awaitsApproval(chosen, store.Layout())
		if err != nil {
			t.Fatalf("awaitsApproval: %v", err)
		}
		if got {
			t.Error("awaitsApproval() = true; the gate never ran, so nobody was shown the plan")
		}
	})

	t.Run("no node has finished at all", func(t *testing.T) {
		ws := goWorkspace(t)
		const runID = "20260905T120000Z-aaaaaaaaaaaa"
		base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
		seedManifest(t, ws, runID, state.RunStatusPaused, graph.NodeApprove, "add tests", base)
		layout, err := state.NewLayout(ws, runID)
		if err != nil {
			t.Fatalf("NewLayout: %v", err)
		}
		j, err := journal.Open(layout.JournalPath())
		if err != nil {
			t.Fatalf("journal.Open: %v", err)
		}
		for _, rec := range []journal.Record{
			{Event: journal.EventRunStarted}, {Event: journal.EventRunPaused},
		} {
			if _, err := j.Append(rec); err != nil {
				t.Fatalf("journal.Append(%v): %v", rec.Event, err)
			}
		}
		if err := j.Close(); err != nil {
			t.Fatalf("journal.Close: %v", err)
		}

		chosen, err := chooseRun(ws, runID)
		if err != nil {
			t.Fatalf("chooseRun: %v", err)
		}
		got, err := awaitsApproval(chosen, layout)
		if err != nil {
			t.Fatalf("awaitsApproval: %v", err)
		}
		if got {
			t.Error("awaitsApproval() = true for a run in which no node has finished; nothing has asked anything")
		}
	})
}

// TestResumeCommandDoesNotApproveAPlanCtrlCInterrupted is the same rule as
// TestResumeApprovesOnlyAPlanTheGateAskedAbout, through the command a person
// types. That test calls awaitsApproval; this one runs `belay resume`, so
// reverting the call site in continueRun to the manifest-only condition fails
// here rather than nowhere.
//
// The run is wired to replay an empty recording, so a resume that wrongly
// approved the plan would route to code and fail there on the first agent
// request instead of calling a real agent.
func TestResumeCommandDoesNotApproveAPlanCtrlCInterrupted(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nagent:\n  mode: \"replay\"\n")
	emptyCassette(t, ws)

	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := graph.NewRegistry()
	registry.MustRegister(
		planWriter{body: "# Plan\n\nwrite a test\n"},
		pressesCtrlC{name: graph.NodeApprove, cancel: cancel},
	)
	plan.registry = registry
	store := seedRunDir(t, plan, "add tests")

	opts := plan.options(store)
	opts.PauseOnCancel = true
	dispatcher, err := graph.NewDispatcher(opts)
	if err != nil {
		t.Fatalf("graph.NewDispatcher: %v", err)
	}
	if _, err := dispatcher.Run(ctx); err != nil && !errors.Is(err, graph.ErrInterrupted) {
		t.Fatalf("Run: %v", err)
	}

	out, _, err := runCLI(t, "resume", store.Layout().RunID(), "--workspace", ws)
	if err != nil {
		t.Fatalf("belay resume on a run Ctrl-C stopped at the gate: %v\n%s", err, out)
	}

	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if st.Plan.Approved {
		t.Error("belay resume approved a plan nobody was shown")
	}

	// The gate ran again and asked: its own pause is now the run's last
	// node_finished, so a second resume is the approval this one was not.
	chosen, err := chooseRun(ws, store.Layout().RunID())
	if err != nil {
		t.Fatalf("chooseRun: %v", err)
	}
	asked, err := awaitsApproval(chosen, store.Layout())
	if err != nil {
		t.Fatalf("awaitsApproval: %v", err)
	}
	if !asked {
		t.Errorf("the run is %v at %q; resuming should have run the gate again, which asks",
			chosen.status, chosen.node)
	}
	if !strings.Contains(out, "the plan is at") {
		t.Errorf("belay resume never told the person to read the plan; got:\n%s", out)
	}
}
