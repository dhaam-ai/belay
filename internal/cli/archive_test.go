package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/nodes/promote"
	"github.com/dhaam-ai/belay/internal/planarchive"
	"github.com/dhaam-ai/belay/internal/state"
)

// archivedBody is the plan the runs in this file write.
const archivedBody = "## Steps\n\n1. Write the test.\n2. Make it pass.\n"

// writesPlan stands in for the plan node the way the real one behaves: it
// writes the plan artifact and records its path and digest, then routes to
// next.
type writesPlan struct{ next string }

func (writesPlan) Name() string { return graph.NodePlan }

func (n writesPlan) Run(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
	path, err := rc.WriteArtifact("plan.md", []byte(archivedBody))
	if err != nil {
		return graph.Result{Status: journal.StatusFailed}, err
	}
	return graph.Result{
		Status: journal.StatusOK,
		Next:   n.next,
		Patch:  state.Patch{Plan: &state.Plan{Path: path, Digest: approve.Digest([]byte(archivedBody))}},
		Note:   "wrote the plan",
	}, nil
}

// failsTheRun stands in for a step whose failure ends the run.
type failsTheRun struct{}

func (failsTheRun) Name() string { return graph.NodeCode }

func (failsTheRun) Run(context.Context, *graph.RunContext) (graph.Result, error) {
	return graph.Result{Status: journal.StatusFailed, Note: "the tests never passed"}, nil
}

// finishInPlace drives an in-place run in ws through its plan to the end,
// with graph.approval off, the way `belay run` does, and returns what it
// printed, the run's store and driveRun's error.
func finishInPlace(t *testing.T, ws string, rest graph.Node) (stdout, stderr string, store *state.Store, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	u := &ui{out: &out, err: &errOut}
	plan, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	plan.config.Graph.Approval = false
	registry := graph.NewRegistry()
	registry.MustRegister(writesPlan{next: rest.Name()}, rest)
	plan.registry = registry
	store = seedRunDir(t, plan, "add tests")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err = driveRun(cmd, u, plan, store, false)
	return out.String(), errOut.String(), store, err
}

// archivedPlanIn returns where a run in ws files its plan.
func archivedPlanIn(ws string, store *state.Store) string {
	return filepath.Join(ws, filepath.FromSlash(planarchive.Rel("docs/plans", store.Layout().RunID(), "add tests")))
}

// TestAFinishedRunMovesItsPlanIntoTheWorkspace is the step afterCompletion
// gained: a run that edits its workspace in place, once it has completed,
// moves its plan out of the run directory into docs/plans, records where it
// went, and says so in one line. The exit status is the run's, whatever the
// move did.
func TestAFinishedRunMovesItsPlanIntoTheWorkspace(t *testing.T) {
	ws := goWorkspace(t)
	reached := false

	stdout, _, store, err := finishInPlace(t, ws, endsTheRun{reached: &reached})

	if err != nil || !reached {
		t.Fatalf("driveRun = %v (reached the end: %v), want a completed run", err, reached)
	}
	dest := archivedPlanIn(ws, store)
	got, readErr := os.ReadFile(dest) //nolint:gosec // under the test's own t.TempDir()
	if readErr != nil {
		t.Fatalf("the plan was not moved to %s: %v", dest, readErr)
	}
	if string(got) != archivedBody {
		t.Errorf("the moved plan holds %q, want %q", got, archivedBody)
	}
	if _, err := os.Stat(filepath.Join(store.Layout().ArtifactsDir(), "plan.md")); !os.IsNotExist(err) {
		t.Errorf("the run's copy of the plan is still there (stat error %v); it was to be moved, not copied", err)
	}
	man, err := store.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if want := planarchive.Rel("docs/plans", store.Layout().RunID(), "add tests"); man.ArchivedPlan.Path != want {
		t.Errorf("manifest archived_plan.path = %q, want %q", man.ArchivedPlan.Path, want)
	}
	if !strings.Contains(stdout, "plan moved to "+dest) {
		t.Errorf("the run does not say where its plan went; stdout:\n%s", stdout)
	}
}

// TestARunKeepsItsPlanWhenItShould: nothing moves, and nothing is said about
// it, for a run started with plan.archive off or a run that did not
// complete. A failed run's plan is the plan of code that does not exist.
func TestARunKeepsItsPlanWhenItShould(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		rest   func(reached *bool) graph.Node
	}{
		{
			name:   "plan.archive is off",
			config: "version: 1\nplan:\n  archive: false\n",
			rest:   func(reached *bool) graph.Node { return endsTheRun{reached: reached} },
		},
		{
			name: "the run failed",
			rest: func(*bool) graph.Node { return failsTheRun{} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := goWorkspace(t)
			if tc.config != "" {
				writeConfig(t, ws, tc.config)
			}
			reached := false

			stdout, stderr, store, _ := finishInPlace(t, ws, tc.rest(&reached))

			if _, err := os.Stat(filepath.Join(ws, "docs")); !os.IsNotExist(err) {
				t.Errorf("docs/ exists (stat error %v); nothing should have moved", err)
			}
			if _, err := os.Stat(filepath.Join(store.Layout().ArtifactsDir(), "plan.md")); err != nil {
				t.Errorf("the run's plan is gone from its run directory: %v", err)
			}
			if strings.Contains(stdout+stderr, "plan moved") || strings.Contains(stdout+stderr, "plan stays") {
				t.Errorf("belay talked about moving a plan it had no reason to move:\n%s%s", stdout, stderr)
			}
		})
	}
}

// TestAPlanThatCannotMoveStaysAndBelaySaysWhy: somebody's file where the plan
// would go is never replaced. The plan stays in the run directory, belay
// says where it is and why on stderr, and the run is still reported as the
// success it was.
func TestAPlanThatCannotMoveStaysAndBelaySaysWhy(t *testing.T) {
	ws := goWorkspace(t)
	theirs := filepath.Join(ws, "docs", "plans")
	if err := os.MkdirAll(theirs, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	reached := false
	var dest string
	// The run id is not known until the run exists, so the file is put in
	// the plan's way by the node that ends the run.
	rest := blocksThePlan{reached: &reached, dest: &dest, ws: ws}

	stdout, stderr, store, err := finishInPlace(t, ws, rest)

	if err != nil || !reached {
		t.Fatalf("driveRun = %v (reached the end: %v), want a completed run: a plan that stays must not fail it", err, reached)
	}
	if got, readErr := os.ReadFile(dest); readErr != nil || string(got) != "their notes\n" { //nolint:gosec // under the test's own t.TempDir()
		t.Errorf("the file in the plan's way now holds %q (error %v); it must never be replaced", got, readErr)
	}
	source := filepath.Join(store.Layout().ArtifactsDir(), "plan.md")
	if _, err := os.Stat(source); err != nil {
		t.Errorf("the run's plan is gone although it could not be moved: %v", err)
	}
	for _, want := range []string{"plan stays", source, dest, "belay plan --archive " + store.Layout().RunID()} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}
	if !strings.Contains(stdout, "belay finished") {
		t.Errorf("the run was not reported as finished; stdout:\n%s", stdout)
	}
}

// blocksThePlan ends the run, having first put a file of somebody's where
// the run's plan would be moved to.
type blocksThePlan struct {
	reached *bool
	dest    *string
	ws      string
}

func (blocksThePlan) Name() string { return graph.NodeCode }

func (n blocksThePlan) Run(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
	*n.reached = true
	*n.dest = filepath.Join(n.ws, filepath.FromSlash(planarchive.Rel("docs/plans", rc.RunID, rc.Goal)))
	if err := os.WriteFile(*n.dest, []byte("their notes\n"), 0o600); err != nil {
		return graph.Result{Status: journal.StatusFailed}, err
	}
	return graph.Result{Status: journal.StatusOK, Next: graph.End, Note: "done"}, nil
}

// TestABranchRunsPlanIsInItsCommitAndLeavesTheRunDirectory drives a branch
// run through the seams belay itself drives, with the real promote node and
// real git, and then asks git. The run's one commit holds the change and the
// plan; afterCompletion then records where the plan went and removes the
// run's copy. That it can remove the copy at all is the proof of the step's
// position: it confirms the plan against the worktree promote committed it
// from, which the step after it removes, so run later it would find no plan
// and keep the copy. The person's checkout is untouched throughout.
func TestABranchRunsPlanIsInItsCommitAndLeavesTheRunDirectory(t *testing.T) {
	dir := gitWorkspace(t)
	before := gitIn(t, dir, "rev-parse", "HEAD")
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}

	plan, err := resolve(resolveRequest{workspace: dir, logger: newLogger(u), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !plan.output.Branched() {
		t.Fatalf("resolve did not choose branch output: %s", plan.outputReason)
	}
	const (
		runID = "20260101T000000Z-c0ffee000000"
		goal  = "add an Add helper"
	)
	store, err := createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: runID, Goal: goal})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}
	var wroteIn string
	registry := graph.NewRegistry()
	registry.MustRegister(
		writesPlan{next: graph.NodeCode},
		writesAFile{name: graph.NodeCode, rel: "src/app.go",
			body: "package src\n\nfunc Add(a, b int) int { return a + b }\n",
			next: graph.NodePromote, where: &wroteIn},
		promote.New(),
	)
	plan.registry = registry
	plan.config.Graph.Approval = false

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	if err := driveRun(cmd, u, plan, store, false); err != nil {
		t.Fatalf("driveRun: %v\n%s", err, out.String())
	}

	branch := "belay/" + runID
	rel := planarchive.Rel("docs/plans", runID, goal)
	if got, want := gitIn(t, dir, "show", "--name-only", "--format=", branch), rel+"\nsrc/app.go"; got != want {
		t.Errorf("%s's commit changed %q, want %q", branch, got, want)
	}
	if got := gitIn(t, dir, "show", branch+":"+rel); got != strings.TrimSpace(archivedBody) {
		t.Errorf("the plan in the commit is not the run's plan:\n%s", got)
	}
	man, err := store.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if man.ArchivedPlan.Path != rel {
		t.Errorf("manifest archived_plan.path = %q, want %q", man.ArchivedPlan.Path, rel)
	}
	if _, err := os.Stat(filepath.Join(store.Layout().ArtifactsDir(), "plan.md")); !os.IsNotExist(err) {
		t.Errorf("the run's copy of the plan is still there (stat error %v)", err)
	}
	if _, err := os.Stat(man.Output.WorkTree); !os.IsNotExist(err) {
		t.Errorf("the finished run's worktree is still there (stat error %v)", err)
	}
	if !strings.Contains(out.String(), "plan committed on "+branch+", at "+rel) {
		t.Errorf("the run does not say where its plan went; output:\n%s", out.String())
	}
	if got := gitIn(t, dir, "rev-parse", "HEAD"); got != before {
		t.Errorf("the user's HEAD moved from %s to %s", before, got)
	}
	if _, err := os.Stat(filepath.Join(dir, "docs")); !os.IsNotExist(err) {
		t.Errorf("the plan was written into the user's checkout (stat error %v)", err)
	}
}

// TestDryRunSaysWhatItWouldWriteIntoTheRepository: a run writes into the
// person's repository at two moments — .belay/.gitignore when it starts,
// the plan when it finishes — and a dry run is where they find out before
// anything happens. Each line appears only when it is true: no plan line
// with plan.archive off, and no .gitignore line once there is one.
func TestDryRunSaysWhatItWouldWriteIntoTheRepository(t *testing.T) {
	for _, tc := range []struct {
		name          string
		config        string
		gitignore     bool
		wantPlan      bool
		wantGitignore bool
	}{
		{name: "the defaults", wantPlan: true, wantGitignore: true},
		{name: "plan.archive off", config: "version: 1\nplan:\n  archive: false\n", wantGitignore: true},
		{name: "a .gitignore already there", gitignore: true, wantPlan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := goWorkspace(t)
			if tc.config != "" {
				writeConfig(t, ws, tc.config)
			}
			if tc.gitignore {
				if err := os.MkdirAll(filepath.Join(ws, ".belay"), 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(ws, ".belay", ".gitignore"), []byte("*\n"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}

			out, _, err := runCLI(t, "--dry-run", "run", "--workspace", ws, "add tests")
			if err != nil {
				t.Fatalf("belay --dry-run run: %v\n%s", err, out)
			}

			plan := filepath.Join(ws, "docs", "plans", "<run-id>-add-tests.md")
			if got := strings.Contains(out, plan); got != tc.wantPlan {
				t.Errorf("the dry run mentions %s: %v, want %v\n%s", plan, got, tc.wantPlan, out)
			}
			gitignore := filepath.Join(ws, ".belay", ".gitignore")
			if got := strings.Contains(out, gitignore); got != tc.wantGitignore {
				t.Errorf("the dry run mentions %s: %v, want %v\n%s", gitignore, got, tc.wantGitignore, out)
			}
			if _, err := os.Stat(filepath.Join(ws, "docs")); !os.IsNotExist(err) {
				t.Errorf("docs/ exists after a dry run (stat error %v); a dry run creates nothing", err)
			}
		})
	}
}

// TestTheStartBannerSaysWhereThePlanWillGo: the lines a run prints before it
// spends anything name where its plan will end up, in the repository or in
// the run's commit, so a file appearing in docs/plans is never a surprise.
func TestTheStartBannerSaysWhereThePlanWillGo(t *testing.T) {
	ws := goWorkspace(t)
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const runID = "20260101T000000Z-c0ffee000000"
	layout, err := state.NewLayout(ws, runID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	announceStart(u, plan, layout, "add tests")

	want := "plan:   moved to " + filepath.Join(ws, "docs", "plans", runID+"-add-tests.md") + " when the run finishes"
	if !strings.Contains(out.String(), want) {
		t.Errorf("the start banner does not say %q:\n%s", want, out.String())
	}
}
