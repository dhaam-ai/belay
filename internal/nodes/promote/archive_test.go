package promote_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/planarchive"
	"github.com/dhaam-ai/belay/internal/state"
)

// approvedPlan is the plan the runs in this file approved.
var approvedPlan = []byte("## Steps\n\n1. Add the flag.\n2. Test it.\n")

// planRel is where a run of goal files its plan, relative to its workspace.
var planRel = planarchive.Rel("docs/plans", runID, goal)

// withPlan gives the run what a finished branch run has when it reaches
// promote: its approved plan in the run's artifacts, fingerprinted on the
// blackboard, and the archive directory its manifest pinned. It also makes
// the fake repository commit the files promote writes on disk, as git's
// `add --all` would.
func (f *fixture) withPlan(t *testing.T) *fixture {
	t.Helper()
	layout, err := state.NewLayout(filepath.Join(f.root, "repo"), runID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := state.ReplaceArtifact(layout, "plan.md", approvedPlan); err != nil {
		t.Fatalf("write the plan artifact: %v", err)
	}
	if f.rc.Layout, err = layout.WithWorkTree(f.rc.Workspace); err != nil {
		t.Fatalf("WithWorkTree: %v", err)
	}
	f.rc.State.Plan = state.Plan{Path: "artifacts/plan.md", Approved: true, Digest: approve.Digest(approvedPlan)}
	f.rc.ArchiveDir = "docs/plans"
	f.repo.disk = true
	return f
}

// changedFiles lists what the run's branch changes since its base.
func (f *fixture) changedFiles(t *testing.T) []string {
	t.Helper()
	files, err := f.repo.Changed(context.Background(), base, f.repo.tipOf(runBranch))
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	return files
}

// TestPromoteCommitsTheApprovedPlanWithTheChange is what archival means for
// a run on a branch: the plan is part of what the run delivers. promote
// writes it into the run's workspace, at the path the manifest pinned,
// before its one commit, so that commit holds the change and the plan
// together, and the plan is byte for byte the one the run approved. A run
// started in a subdirectory files it in that subdirectory, which is where a
// person's own checkout will have it once they take the branch.
func TestPromoteCommitsTheApprovedPlanWithTheChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
	}{
		{name: "a run at the top of its repository"},
		{name: "a run started in a subdirectory", prefix: "sub/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRun(t, t.TempDir(), tc.prefix+"cmd/runs.go")
			f.rc.Output.Prefix = tc.prefix
			f.rc.Workspace = filepath.Join(f.worktree, filepath.FromSlash(tc.prefix))
			// Recorded relative to the run's directory, as the write step
			// records it.
			f.rc.State.Code.ChangedFiles = []string{"cmd/runs.go"}
			// A worktree is a checkout, so the directory the run started in
			// is there in it.
			if err := os.MkdirAll(f.rc.Workspace, 0o750); err != nil {
				t.Fatalf("create the run's workspace: %v", err)
			}
			f.withPlan(t)

			res := f.mustRun(t)

			want := []string{tc.prefix + "cmd/runs.go", tc.prefix + planRel}
			slices.Sort(want)
			if diff := cmp.Diff(want, f.changedFiles(t)); diff != "" {
				t.Errorf("the run's branch changes (-want +got):\n%s", diff)
			}
			if got := res.Patch.Promotion; got == nil || got.Files != 2 {
				t.Errorf("Promotion = %+v, want the change and the plan, 2 files", got)
			}
			if n := f.repo.commitsWithSubject(runBranch, goal); n != 1 {
				t.Errorf("commits on the run's branch = %d, want exactly 1", n)
			}
			got, err := os.ReadFile(filepath.Join(f.rc.Workspace, filepath.FromSlash(planRel))) //nolint:gosec // under the test's own t.TempDir()
			if err != nil {
				t.Fatalf("read the committed plan: %v", err)
			}
			if string(got) != string(approvedPlan) {
				t.Errorf("the committed plan holds %q, want the approved plan %q", got, approvedPlan)
			}
		})
	}
}

// TestPromoteMergesTheWinnerWithTheApprovedPlan: with a fanout winner, what
// the run delivers is the winner's work, brought in by one merge commit. The
// plan goes into the winner's tree before its work is committed there, so
// the merge brings plan and code in together. The run's own worktree is
// never written to: the merge refuses a tree with uncommitted changes, and
// a plan left there would pause the run.
func TestPromoteMergesTheWinnerWithTheApprovedPlan(t *testing.T) {
	f := newRun(t, t.TempDir()).withCandidates(t, "c2", "c1", "c2").withPlan(t)

	f.mustRun(t)

	want := []string{planRel, "c2.go"}
	slices.Sort(want)
	if diff := cmp.Diff(want, f.changedFiles(t)); diff != "" {
		t.Errorf("the run's branch changes (-want +got):\n%s", diff)
	}
	if n := f.repo.merges(runBranch); n != 1 {
		t.Errorf("merge commits on the run's branch = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(f.worktree, filepath.FromSlash(planRel))); !os.IsNotExist(err) {
		t.Errorf("promote wrote the plan into the run's own worktree (stat error %v)", err)
	}
}

// TestPromoteReRunAddsNoPlanAfterItsCommit is promote's one-commit rule
// holding up against the plan. The first attempt could not read the plan —
// a file somebody had open, a disk that hiccupped — so it committed the
// change without it, and then died before the dispatcher heard back. By the
// re-run the plan reads fine. Writing it now would dirty a tree whose commit
// is already on the branch, and the re-run would commit a second time and
// report a different Result from the one the first attempt would have: the
// run's branch would change under a promotion already made. So once the
// branch holds this run's commit, promote leaves the tree alone, and the
// plan stays in the run directory for the CLI to report.
func TestPromoteReRunAddsNoPlanAfterItsCommit(t *testing.T) {
	f := newRun(t, t.TempDir(), "a.go").withPlan(t)
	artifact := filepath.Join(f.rc.Layout.ArtifactsDir(), "plan.md")
	aside := artifact + ".aside"
	if err := os.Rename(artifact, aside); err != nil {
		t.Fatalf("hide the plan from the first attempt: %v", err)
	}
	f.log.crashAt = "Commit " + f.worktree
	if _, crashed, _ := f.run(t); !crashed {
		t.Fatal("the first attempt did not reach its commit")
	}
	committed := f.repo.tipOf(runBranch)
	if err := os.Rename(aside, artifact); err != nil {
		t.Fatalf("give the plan back: %v", err)
	}

	f.rc.Attempt = 2
	res := f.mustRun(t)

	if got := res.Patch.Promotion; got == nil || got.Commit != committed {
		t.Errorf("Promotion = %+v, want the commit the first attempt made, %s", got, committed)
	}
	if n := f.repo.commitsWithSubject(runBranch, goal); n != 1 {
		t.Errorf("commits on the run's branch = %d, want exactly 1", n)
	}
	if _, err := os.Stat(filepath.Join(f.worktree, filepath.FromSlash(planRel))); !os.IsNotExist(err) {
		t.Errorf("the re-run wrote the plan into a tree whose commit was already made (stat error %v)", err)
	}
}

// TestInterruptedPromoteWithAPlanReRunsToTheSameResult is
// TestInterruptedPromoteReRunsToTheSameResult for runs that archive their
// plan: cut short at any point and run again, promote returns the Result an
// uninterrupted promote returns, byte for byte, with the plan in the one
// commit and never a second copy or a second commit.
func TestInterruptedPromoteWithAPlanReRunsToTheSameResult(t *testing.T) {
	single := func(t *testing.T, root string) *fixture { return newRun(t, root, "a.go").withPlan(t) }
	fanout := func(t *testing.T, root string) *fixture {
		return newRun(t, root).withCandidates(t, "c2", "c1", "c2").withPlan(t)
	}
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, root string) *fixture
		crashAt string
	}{
		{name: "single run: the Result was lost", setup: single},
		{name: "single run: after the commit", setup: single, crashAt: "Commit {worktree}"},
		{name: "fanout: after the winner's work was committed", setup: fanout, crashAt: "Commit {c2}"},
		{name: "fanout: after the merge", setup: fanout, crashAt: "Integrate {c2-branch}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			want := tc.setup(t, root).mustRun(t)
			if err := os.RemoveAll(root); err != nil {
				t.Fatalf("clear the first run: %v", err)
			}

			f := tc.setup(t, root)
			f.log.crashAt = strings.NewReplacer(
				"{worktree}", f.worktree,
				"{c2}", f.candidateDir("c2"),
				"{c2-branch}", f.candidateBranch("c2"),
			).Replace(tc.crashAt)
			if tc.crashAt == "" {
				f.mustRun(t)
			} else if _, crashed, _ := f.run(t); !crashed {
				t.Fatalf("the first attempt did not reach %q", tc.crashAt)
			}
			f.rc.Attempt = 2
			got := f.mustRun(t)

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("the re-run's Result differs from an uninterrupted promote's (-uninterrupted +re-run):\n%s", diff)
			}
			if !slices.Contains(f.changedFiles(t), planRel) {
				t.Errorf("the run's branch changes %v, without the plan", f.changedFiles(t))
			}
		})
	}
}

// TestPromoteArchivesOnlyWhatTheRunAskedFor covers the two cases where the
// commit holds exactly what it held before archival existed: a run whose
// manifest pinned no directory, and a destination somebody's file already
// occupies, which promote never replaces. Either way the promotion goes
// ahead, because a run whose change passed its gate has succeeded whether or
// not its plan could be filed.
func TestPromoteArchivesOnlyWhatTheRunAskedFor(t *testing.T) {
	t.Run("a run that pinned no directory", func(t *testing.T) {
		f := newRun(t, t.TempDir(), "a.go").withPlan(t)
		f.rc.ArchiveDir = ""

		f.mustRun(t)

		if diff := cmp.Diff([]string{"a.go"}, f.changedFiles(t)); diff != "" {
			t.Errorf("the run's branch changes (-want +got):\n%s", diff)
		}
	})
	t.Run("somebody's file where the plan would go", func(t *testing.T) {
		f := newRun(t, t.TempDir(), "a.go").withPlan(t)
		theirs := filepath.Join(f.worktree, filepath.FromSlash(planRel))
		if err := os.MkdirAll(filepath.Dir(theirs), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(theirs, []byte("their notes\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		res := f.mustRun(t)

		if res.Patch.Promotion == nil {
			t.Fatal("promote did not promote the run's change")
		}
		got, err := os.ReadFile(theirs) //nolint:gosec // under the test's own t.TempDir()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "their notes\n" {
			t.Errorf("promote replaced the file at %s with %q", planRel, got)
		}
	})
}
