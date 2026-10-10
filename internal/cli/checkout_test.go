//go:build unix

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/planview"
	"github.com/dhaam-ai/belay/internal/state"
)

// The promise branch output makes -- belay works in the run's own worktree
// and never writes to the checkout a person is sitting in -- is kept in one
// place, openRun, and pinned there by TestOpenRunWorksInTheRunsWorktree. That
// test proves the helper redirects; it cannot prove that a command uses what
// the helper returned. A command that called openRun and then built a store
// of its own would pass it, and every other test, while dispatching a branch
// run's agent into the person's checkout. So each command that continues a
// run is driven here as a person drives it, against a real repository, with
// a coding agent that writes where it is started.

// checkoutRunID is the branch run every test here continues.
const checkoutRunID = "20260101T000000Z-c4ec0c000000"

// branchRunAtItsPlan makes, in a real git repository, a branch run stopped
// at its approve gate with the gate having asked, as `belay run` leaves one:
// its own branch and worktree, a plan, and a journal ending in the gate's
// pause.
func branchRunAtItsPlan(t *testing.T) (dir string, store *state.Store) {
	t.Helper()
	dir = gitWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !plan.output.Branched() {
		t.Fatalf("resolve did not choose branch output, so nothing here is about a worktree: %s", plan.outputReason)
	}
	store, err = createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: checkoutRunID, Goal: "add an Add helper"})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}
	registry := graph.NewRegistry()
	registry.MustRegister(planWriter{body: "# Plan\n\n1. Add an Add helper.\n"}, approve.New())
	plan.registry = registry
	view, err := dispatchOnce(t.Context(), plan, store, false)
	if err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}
	if view.outcome.Status != state.RunStatusPaused || view.outcome.Node != graph.NodeApprove {
		t.Fatalf("the run stopped %v at %q (error %v), want paused at the approve gate",
			view.outcome.Status, view.outcome.Node, view.runErr)
	}
	return dir, store
}

// agentThatSaysWhereItRan puts a `claude` first on PATH that records the
// directory it was started in, leaves a file of its own there, and fails, as
// an agent that wrote something and then fell over would. It returns the
// file the directories are recorded in, one per call.
//
// It is a real process, started the way belay starts its agent, because the
// question is where belay starts it: the backend runs the agent in the
// workspace the dispatched node was given (RunContext.Workspace), so the
// directory it finds itself in is that workspace.
func agentThatSaysWhereItRan(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "where-the-agent-ran")
	script := "#!/bin/sh\npwd -P >> '" + record + "'\n: > agent-was-here\nexit 1\n"
	//nolint:gosec // G306: an executable the test itself runs; it must be executable.
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake agent: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

// checkoutSums hashes every file of a person's checkout, by path, with its
// modification time, leaving out .git, which git keeps for itself, and
// .belay, which is belay's and holds the run's own worktree.
func checkoutSums(t *testing.T, dir string) map[string]string {
	t.Helper()
	sums := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && (d.Name() == ".git" || d.Name() == ".belay") && filepath.Dir(path) == dir {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // the test's own repository
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		sums[rel] = info.ModTime().String() + " " + hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
	return sums
}

// assertTheAgentWorkedInTheWorktree reads where the agent was started and
// checks that every call was in the run's worktree and that the person's
// checkout, dir, is exactly as it was. store is the run's own, whose
// workspace is the worktree, so the checkout is named separately.
func assertTheAgentWorkedInTheWorktree(t *testing.T, dir string, store *state.Store, record string,
	before map[string]string) {
	t.Helper()
	manifest, err := store.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	worktree := evalPath(t, manifest.Output.WorkTree)

	data, err := os.ReadFile(record) //nolint:gosec // the test's own record
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the agent was never started, so this test proves nothing about where it would have been")
	}
	if err != nil {
		t.Fatalf("read %s: %v", record, err)
	}
	for _, where := range strings.Fields(string(data)) {
		if where != worktree {
			t.Errorf("the agent ran in %s, want the run's worktree %s", where, worktree)
		}
	}
	if diff := cmp.Diff(before, checkoutSums(t, dir)); diff != "" {
		t.Errorf("the person's checkout changed (-before +after):\n%s", diff)
	}
}

// sayOnFailure prints what a command said, and how it ended, if the test
// fails. The command is expected to end in an error -- the agent here fails
// on purpose -- so the error is evidence for a reader, not a verdict.
func sayOnFailure(t *testing.T, command string, err error, said string) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s ended with error %v, having said:\n%s", command, err, said)
		}
	})
}

// TestBelayResumeWorksInTheRunsWorktree: `belay resume` on a branch run
// approves its plan and dispatches the code node, whose agent must start in
// the run's worktree and leave the checkout alone.
func TestBelayResumeWorksInTheRunsWorktree(t *testing.T) {
	dir, store := branchRunAtItsPlan(t)
	record := agentThatSaysWhereItRan(t)
	before := checkoutSums(t, dir)

	out, errOut, err := runCLI(t, "resume", checkoutRunID, "--workspace", dir, "--no-input")
	sayOnFailure(t, "belay resume", err, out+errOut)

	assertTheAgentWorkedInTheWorktree(t, dir, store, record, before)
}

// TestBelayPlanRegenerateWorksInTheRunsWorktree: `belay plan --regenerate`
// dispatches the plan node again, whose agent must start in the run's
// worktree and leave the checkout alone. It is the command that once built a
// store of its own and did exactly the opposite.
func TestBelayPlanRegenerateWorksInTheRunsWorktree(t *testing.T) {
	dir, store := branchRunAtItsPlan(t)
	record := agentThatSaysWhereItRan(t)
	before := checkoutSums(t, dir)

	out, errOut, err := runCLI(t, "plan", "--regenerate", "fewer steps", checkoutRunID, "--workspace", dir)
	sayOnFailure(t, "belay plan --regenerate", err, out+errOut)

	assertTheAgentWorkedInTheWorktree(t, dir, store, record, before)
}

// TestApprovingInThePlanViewerWorksInTheRunsWorktree: approving in the viewer
// `belay plan` opens dispatches the code node, whose agent must start in the
// run's worktree and leave the checkout alone.
func TestApprovingInThePlanViewerWorksInTheRunsWorktree(t *testing.T) {
	dir, store := branchRunAtItsPlan(t)
	record := agentThatSaysWhereItRan(t)
	before := checkoutSums(t, dir)
	// belay plan reads the root's flags for the configuration and the
	// recording to resolve the run with; a value an earlier test left there
	// would resolve a different run.
	resetFlags(t, rootCmd)

	reviewer := &scriptedReviewer{answers: []planview.Decision{{Action: planview.ActionApprove}}}
	var out bytes.Buffer
	u := reviewUI(&out, reviewer)
	cmd := cmdWithContext()
	cmd.Flags().String("workspace", dir, "")
	err := showPlan(cmd, u, checkoutRunID)
	sayOnFailure(t, "belay plan, approved in the viewer", err, out.String())
	if len(reviewer.opens) != 1 {
		t.Fatalf("the viewer was opened %d time(s), want once", len(reviewer.opens))
	}

	assertTheAgentWorkedInTheWorktree(t, dir, store, record, before)
}
