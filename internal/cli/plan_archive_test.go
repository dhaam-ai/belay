package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/nodes/promote"
	"github.com/dhaam-ai/belay/internal/planarchive"
	"github.com/dhaam-ai/belay/internal/state"
)

// TestPlanShowsAMovedPlanFromWhereItNowIs: once a finished run's plan has
// moved into the repository, `belay plan <run-id>` shows it from there, says
// where that is, and says the plan is a record now rather than offering to
// resume a run that is over.
func TestPlanShowsAMovedPlanFromWhereItNowIs(t *testing.T) {
	ws := goWorkspace(t)
	reached := false
	if _, _, _, err := finishInPlace(t, ws, endsTheRun{reached: &reached}); err != nil {
		t.Fatalf("finish the run: %v", err)
	}
	runID := onlyRunID(t, ws)

	stdout, _, err := runCLI(t, "--workspace", ws, "plan", runID)
	if err != nil {
		t.Fatalf("belay plan on a finished run whose plan moved: %v", err)
	}
	for _, want := range []string{
		"1. Write the test.",
		filepath.Join(ws, "docs", "plans", runID+"-add-tests.md"),
		"record",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("belay plan does not show %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "belay resume") {
		t.Errorf("belay plan offers to resume a run that is over:\n%s", stdout)
	}
}

// TestPlanShowsABranchRunsPlanFromItsCommit: a finished branch run's plan is
// in the run's commit, and the run's worktree, the one other place it was, is
// gone. Until the person takes the branch, that commit is the only copy, so
// `belay plan` reads it from there.
func TestPlanShowsABranchRunsPlanFromItsCommit(t *testing.T) {
	dir, runID := finishOnABranch(t)

	stdout, _, err := runCLI(t, "--workspace", dir, "plan", runID)
	if err != nil {
		t.Fatalf("belay plan on a finished branch run: %v", err)
	}
	for _, want := range []string{"1. Write the test.", "belay/" + runID, planarchive.Rel("docs/plans", runID, "add an Add helper")} {
		if !strings.Contains(stdout, want) {
			t.Errorf("belay plan does not show %q:\n%s", want, stdout)
		}
	}
}

// TestTimelineIsTheSameBeforeAndAfterThePlanMoves: `belay timeline` reads the
// journal, the manifest and state.json, all of which stay in the run
// directory, and moving the plan changes none of what it shows. The move is
// made here by the retry command, on a run whose plan the run itself could
// not move, so the same run can be looked at on both sides of it.
func TestTimelineIsTheSameBeforeAndAfterThePlanMoves(t *testing.T) {
	ws := goWorkspace(t)
	if err := os.MkdirAll(filepath.Join(ws, "docs", "plans"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	reached := false
	var blocker string
	if _, _, _, err := finishInPlace(t, ws, blocksThePlan{reached: &reached, dest: &blocker, ws: ws}); err != nil {
		t.Fatalf("finish the run: %v", err)
	}
	runID := onlyRunID(t, ws)
	before, _, err := runCLI(t, "--workspace", ws, "timeline", runID)
	if err != nil {
		t.Fatalf("belay timeline before the move: %v", err)
	}
	beforeJSON, _, err := runCLI(t, "--workspace", ws, "timeline", "--json", runID)
	if err != nil {
		t.Fatalf("belay timeline --json before the move: %v", err)
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatalf("clear the way: %v", err)
	}
	if _, _, err := runCLI(t, "--workspace", ws, "plan", "--archive", runID); err != nil {
		t.Fatalf("belay plan --archive: %v", err)
	}
	if _, err := os.Stat(blocker); err != nil {
		t.Fatalf("the plan did not move, so this proves nothing about the timeline: %v", err)
	}

	after, _, err := runCLI(t, "--workspace", ws, "timeline", runID)
	if err != nil {
		t.Fatalf("belay timeline after the move: %v", err)
	}
	afterJSON, _, err := runCLI(t, "--workspace", ws, "timeline", "--json", runID)
	if err != nil {
		t.Fatalf("belay timeline --json after the move: %v", err)
	}
	if after != before {
		t.Errorf("belay timeline changed when the plan moved:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if afterJSON != beforeJSON {
		t.Errorf("belay timeline --json changed when the plan moved:\nbefore:\n%s\nafter:\n%s", beforeJSON, afterJSON)
	}
}

// TestPlanArchiveFinishesAMoveTheRunCouldNotMake is the retry: a plan the run
// could not move — somebody's file was in the way, or belay was killed part
// way through the move — is moved by `belay plan --archive <run-id>` once the
// way is clear. Asking again is harmless and says so, and a run that has not
// finished is refused with the reason, because its plan describes code that
// does not exist yet.
func TestPlanArchiveFinishesAMoveTheRunCouldNotMake(t *testing.T) {
	ws := goWorkspace(t)
	if err := os.MkdirAll(filepath.Join(ws, "docs", "plans"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	reached := false
	var blocker string
	if _, _, _, err := finishInPlace(t, ws, blocksThePlan{reached: &reached, dest: &blocker, ws: ws}); err != nil {
		t.Fatalf("finish the run: %v", err)
	}
	runID := onlyRunID(t, ws)
	if err := os.Remove(blocker); err != nil {
		t.Fatalf("clear the way: %v", err)
	}

	stdout, _, err := runCLI(t, "--workspace", ws, "plan", "--archive", runID)
	if err != nil {
		t.Fatalf("belay plan --archive: %v", err)
	}
	if !strings.Contains(stdout, "plan moved to "+blocker) {
		t.Errorf("belay plan --archive does not say where the plan went:\n%s", stdout)
	}
	if got, err := os.ReadFile(blocker); err != nil || string(got) != archivedBody { //nolint:gosec // under the test's own t.TempDir()
		t.Errorf("the moved plan holds %q (error %v), want the run's plan", got, err)
	}

	again, _, err := runCLI(t, "--workspace", ws, "plan", "--archive", runID)
	if err != nil {
		t.Fatalf("belay plan --archive a second time: %v", err)
	}
	if !strings.Contains(again, "already") {
		t.Errorf("a second belay plan --archive does not say the plan had already moved:\n%s", again)
	}

	paused := "20260101T000000Z-aaaaaaaaaaaa"
	seedManifest(t, ws, paused, state.RunStatusPaused, graph.NodeApprove, "add tests", time.Now().UTC())
	if _, _, err := runCLI(t, "--workspace", ws, "plan", "--archive", paused); err == nil || !strings.Contains(err.Error(), "not finished") {
		t.Errorf("belay plan --archive on a paused run = %v, want a refusal saying it has not finished", err)
	}
}

// onlyRunID returns the id of the one run in ws.
func onlyRunID(t *testing.T, ws string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(ws, ".belay", "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("want exactly one run in %s, found %d (%v)", ws, len(entries), err)
	}
	return entries[0].Name()
}

// finishOnABranch drives a branch run in a fresh git repository to its end,
// through the seams belay itself drives, and returns the repository and the
// run's id.
func finishOnABranch(t *testing.T) (dir, runID string) {
	t.Helper()
	dir = gitWorkspace(t)
	runID = "20260101T000000Z-c0ffee000000"
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	plan, err := resolve(resolveRequest{workspace: dir, logger: newLogger(u), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	store, err := createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: runID, Goal: "add an Add helper"})
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
	return dir, runID
}
