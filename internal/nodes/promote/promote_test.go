package promote_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/promote"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/internal/vcs"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

const (
	runID     = "20260101T000000Z-c0ffee000000"
	runBranch = "belay/" + runID
	// base is the commit the run branch was created from.
	base = "commit-base"
	goal = "add a --json flag to belay runs"
)

// fixture is everything one promote execution sees: a run in branch output,
// its repository, and an isolator whose workspaces are branches.
type fixture struct {
	log      *callLog
	repo     *fakeRepo
	iso      *branchingIsolator
	rc       *graph.RunContext
	root     string
	worktree string
}

// newRun builds a branch-output run under root whose worktree holds
// uncommitted: the change the quality gate has just passed.
func newRun(t *testing.T, root string, uncommitted ...string) *fixture {
	t.Helper()
	repoDir := filepath.Join(root, "repo")
	worktree := filepath.Join(repoDir, ".belay", "runs", runID, "worktree")
	if err := os.MkdirAll(worktree, 0o750); err != nil {
		t.Fatalf("create the run worktree: %v", err)
	}
	log := &callLog{}
	repo := newFakeRepo(log, base)
	repo.branch(worktree, runBranch, base, uncommitted...)
	iso := newBranchingIsolator(runBranch, repo, log)
	return &fixture{
		log:      log,
		repo:     repo,
		iso:      iso,
		root:     root,
		worktree: worktree,
		rc: &graph.RunContext{
			Goal:      goal,
			State:     state.NewState(goal),
			Config:    config.Default(),
			Workspace: worktree,
			RunID:     runID,
			NodeName:  graph.NodePromote,
			Step:      9,
			Attempt:   1,
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Isolator:  iso,
			Output: state.Output{
				Mode:     state.OutputBranch,
				Repo:     repoDir,
				Branch:   runBranch,
				Base:     base,
				BaseRef:  "main",
				WorkTree: worktree,
			},
			VCS: repo,
		},
	}
}

// withCandidates adds fanout candidates, each a worktree on its own branch
// holding one uncommitted file named after it, and records winner as join's
// choice.
func (f *fixture) withCandidates(t *testing.T, winner string, ids ...string) *fixture {
	t.Helper()
	candidates := make([]state.Candidate, 0, len(ids))
	for _, id := range ids {
		dir := f.candidateDir(id)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("create candidate %s: %v", id, err)
		}
		f.repo.branch(dir, f.candidateBranch(id), base, id+".go")
		candidates = append(candidates, state.Candidate{ID: id, Dir: dir, TestPassed: true, Ephemeral: true})
	}
	f.rc.State.Candidates = candidates
	f.rc.State.Winner = winner
	return f
}

func (f *fixture) candidateDir(id string) string {
	return filepath.Join(f.root, "repo", ".belay", "runs", runID, "candidates", id)
}

func (f *fixture) candidateBranch(id string) string { return runBranch + "-" + id }

// run executes promote once. A fake that crashes the attempt part-way does
// so by panicking, the way a kill -9 stops a process: run reports it as
// crashed, and the Result that attempt would have returned never exists.
func (f *fixture) run(t *testing.T) (res graph.Result, crashed bool, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(crash); !ok {
				panic(r)
			}
			crashed = true
		}
	}()
	res, err = promote.New().Run(context.Background(), f.rc)
	return res, false, err
}

// mustRun executes promote once and fails the test on anything but a
// Result.
func (f *fixture) mustRun(t *testing.T) graph.Result {
	t.Helper()
	res, crashed, err := f.run(t)
	if crashed {
		t.Fatal("promote crashed where no crash was scripted")
	}
	if err != nil {
		t.Fatalf("Run() error = %v, want a Result", err)
	}
	if verr := res.Validate(); verr != nil {
		t.Fatalf("Run() returned a Result the dispatcher cannot act on: %v", verr)
	}
	return res
}

func trailers(pairs ...string) []vcs.Trailer {
	out := make([]vcs.Trailer, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, vcs.Trailer{Key: pairs[i], Value: pairs[i+1]})
	}
	return out
}

func TestNodeName(t *testing.T) {
	if got := promote.New().Name(); got != graph.NodePromote {
		t.Fatalf("Name() = %q, want %q", got, graph.NodePromote)
	}
}

// TestPromoteCommitsTheRunsGatedResult is the path every branch-output run
// takes: review passed, and the tree it passed becomes one commit on the
// run's branch, under the user's own identity, with a trailer saying which
// run made it.
func TestPromoteCommitsTheRunsGatedResult(t *testing.T) {
	f := newRun(t, t.TempDir(), "cmd/runs.go", "cmd/runs_test.go")

	res := f.mustRun(t)

	if res.Next != graph.End || res.Status != journal.StatusOK {
		t.Errorf("Next, Status = %q, %v; want %q, %v", res.Next, res.Status, graph.End, journal.StatusOK)
	}
	if (res.Usage != belay.Usage{}) {
		t.Errorf("Usage = %+v, want zero: promote calls no agent", res.Usage)
	}

	tip := f.repo.tipOf(runBranch)
	if tip == base {
		t.Fatalf("the run branch is still at its base; nothing was committed")
	}
	want := state.Patch{Promotion: &state.Promotion{Branch: runBranch, Base: base, Commit: tip, Files: 2}}
	if diff := cmp.Diff(want, res.Patch); diff != "" {
		t.Errorf("Patch mismatch (-want +got):\n%s", diff)
	}

	wantMsg := vcs.Message{Subject: goal, Trailers: trailers("Belay-Run", runID)}
	if diff := cmp.Diff(wantMsg, f.repo.messageOf(tip)); diff != "" {
		t.Errorf("commit message mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"Commit " + f.worktree}, f.log.snapshot()); diff != "" {
		t.Errorf("calls mismatch (-want +got):\n%s", diff)
	}
	for _, want := range []string{runBranch, tip, "2 files changed", base} {
		if !strings.Contains(res.Note, want) {
			t.Errorf("Note %q does not mention %q", res.Note, want)
		}
	}
}

// TestPromoteCommitsTheWholeWorkTree covers a run started in a subdirectory
// of its repository. Its workspace is that subdirectory of the run's
// worktree, but the change it is promoting is the whole worktree's: an agent
// that also edited go.mod at the top has changed go.mod, and a commit made
// from the subdirectory could leave it out. The fake repository knows only
// the worktree's top, so a commit asked for anywhere else fails.
func TestPromoteCommitsTheWholeWorkTree(t *testing.T) {
	f := newRun(t, t.TempDir(), "go.mod", "sub/main.go")
	f.rc.Output.Prefix = "sub/"
	f.rc.Workspace = filepath.Join(f.worktree, "sub")

	res := f.mustRun(t)

	if got := res.Patch.Promotion; got == nil || got.Files != 2 {
		t.Errorf("Promotion = %+v, want both files committed", got)
	}
}

// TestPromoteWritesACommitMessageAPersonCanRead: the goal is the commit's
// subject, because it is the only description of the change belay has. A
// subject git's tools can show on one line needs one short line, so a long
// or multi-line goal keeps its first line, shortened if need be, and the
// whole goal moves to the body where nothing is lost.
func TestPromoteWritesACommitMessageAPersonCanRead(t *testing.T) {
	long := strings.Repeat("make the runs listing easier to scan ", 4)
	for _, tc := range []struct {
		name string
		goal string
		want vcs.Message
	}{
		{
			name: "a short one-line goal is the subject and nothing else",
			goal: goal,
			want: vcs.Message{Subject: goal},
		},
		{
			name: "a multi-line goal keeps its first line as the subject",
			goal: "add a --json flag\n\nIt should print one object per run.",
			want: vcs.Message{
				Subject: "add a --json flag",
				Body:    "add a --json flag\n\nIt should print one object per run.",
			},
		},
		{
			name: "a long goal is shortened to 72 characters",
			goal: long,
			want: vcs.Message{
				Subject: strings.TrimSpace(long)[:69] + "...",
				Body:    strings.TrimSpace(long),
			},
		},
		{
			name: "runs of whitespace in the subject collapse to one space",
			goal: "  fix   the\tflaky   test  ",
			want: vcs.Message{Subject: "fix the flaky test", Body: "fix   the\tflaky   test"},
		},
		{
			name: "an empty goal still gives a subject",
			goal: "   ",
			want: vcs.Message{Subject: "belay run " + runID},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRun(t, t.TempDir(), "a.go")
			f.rc.Goal = tc.goal

			res := f.mustRun(t)

			tc.want.Trailers = trailers("Belay-Run", runID)
			if diff := cmp.Diff(tc.want, f.repo.messageOf(res.Patch.Promotion.Commit)); diff != "" {
				t.Errorf("commit message mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPromoteWithNothingToCommit: a run whose gate passed without changing a
// file has nothing to promote. That is still a completed run, and the branch
// stays at its base rather than gaining an empty commit.
func TestPromoteWithNothingToCommit(t *testing.T) {
	f := newRun(t, t.TempDir())

	res := f.mustRun(t)

	want := state.Patch{Promotion: &state.Promotion{Branch: runBranch, Base: base, Commit: base}}
	if diff := cmp.Diff(want, res.Patch); diff != "" {
		t.Errorf("Patch mismatch (-want +got):\n%s", diff)
	}
	if calls := f.log.snapshot(); len(calls) != 0 {
		t.Errorf("calls = %v, want none: there was nothing to commit", calls)
	}
	if !strings.Contains(res.Note, "no files") {
		t.Errorf("Note %q does not say the run changed no files", res.Note)
	}
}

// TestPromoteMergesTheWinner is the fanout path: the winner's own work is
// committed on its branch, merged into the run's branch as one merge commit,
// and only then are the candidates destroyed. Destroying first would throw
// away the winner's uncommitted work, which is the whole of what it did.
func TestPromoteMergesTheWinner(t *testing.T) {
	f := newRun(t, t.TempDir()).withCandidates(t, "c2", "c1", "c2")

	res := f.mustRun(t)

	if res.Next != graph.End || res.Status != journal.StatusOK {
		t.Errorf("Next, Status = %q, %v; want %q, %v", res.Next, res.Status, graph.End, journal.StatusOK)
	}
	wantCalls := []string{
		"Commit " + f.candidateDir("c2"),
		"Integrate " + f.candidateBranch("c2"),
		"Destroy c1",
		"Destroy c2",
	}
	if diff := cmp.Diff(wantCalls, f.log.snapshot()); diff != "" {
		t.Errorf("calls mismatch (-want +got):\n%s", diff)
	}

	tip := f.repo.tipOf(runBranch)
	want := state.Patch{Promotion: &state.Promotion{Branch: runBranch, Base: base, Commit: tip, Candidate: "c2", Files: 1}}
	if diff := cmp.Diff(want, res.Patch); diff != "" {
		t.Errorf("Patch mismatch (-want +got):\n%s", diff)
	}
	if got := f.repo.merges(runBranch); got != 1 {
		t.Errorf("merge commits on the run branch = %d, want 1", got)
	}

	wantMerge := vcs.Message{
		Subject:  "Merge candidate c2",
		Body:     goal,
		Trailers: trailers("Belay-Run", runID, "Belay-Candidate", "c2"),
	}
	if diff := cmp.Diff(wantMerge, f.repo.messageOf(tip)); diff != "" {
		t.Errorf("merge message mismatch (-want +got):\n%s", diff)
	}
	if got := f.repo.commitsWithSubject(runBranch, goal); got != 1 {
		t.Errorf("commits of the winner's work on the run branch = %d, want 1", got)
	}

	wantDestroyed := []belay.Workspace{
		{ID: "c1", Dir: f.candidateDir("c1"), Ephemeral: true},
		{ID: "c2", Dir: f.candidateDir("c2"), Ephemeral: true},
	}
	if diff := cmp.Diff(wantDestroyed, f.iso.DestroyCalls()); diff != "" {
		t.Errorf("destroyed workspaces mismatch (-want +got):\n%s", diff)
	}
	for _, want := range []string{"c2", runBranch, "1 file changed"} {
		if !strings.Contains(res.Note, want) {
			t.Errorf("Note %q does not mention %q", res.Note, want)
		}
	}
}

// TestInterruptedPromoteReRunsToTheSameResult is the property the node is
// built around. The dispatcher re-runs a node whose start has no finish, so
// a promote cut short at any point is run again against the repository it
// left behind. It must converge on the Result an uninterrupted promote
// returns, byte for byte, without committing or merging a second time.
//
// It can, because it remembers nothing: whether the work is committed,
// whether the winner is merged, and what the branch holds are all asked of
// the repository, and the Result is built from the answers.
func TestInterruptedPromoteReRunsToTheSameResult(t *testing.T) {
	single := func(t *testing.T, root string) *fixture { return newRun(t, root, "a.go", "b.go") }
	fanout := func(t *testing.T, root string) *fixture {
		return newRun(t, root).withCandidates(t, "c2", "c1", "c2", "c3")
	}

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string) *fixture
		// crashAt is the call the first attempt dies straight after, or
		// empty for an attempt that finished but whose Result was lost
		// before the dispatcher could journal it.
		crashAt string
		// merges is how many merge commits the run branch must end with.
		merges int
	}{
		{name: "single run: the Result was lost", setup: single},
		{name: "single run: after the commit", setup: single, crashAt: "Commit {worktree}"},
		{name: "fanout: the Result was lost", setup: fanout, merges: 1},
		{name: "fanout: after the winner's work was committed", setup: fanout, crashAt: "Commit {c2}", merges: 1},
		{name: "fanout: after the merge", setup: fanout, crashAt: "Integrate {c2-branch}", merges: 1},
		{name: "fanout: after the first candidate was destroyed", setup: fanout, crashAt: "Destroy c1", merges: 1},
		{name: "fanout: after the winner was destroyed", setup: fanout, crashAt: "Destroy c2", merges: 1},
		{name: "fanout: after every candidate was destroyed", setup: fanout, crashAt: "Destroy c3", merges: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both runs are built at the same paths, so the only thing
			// that can differ between their Results is what promote did.
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
				f.mustRun(t) // finished, but its Result never reached the journal
			} else if _, crashed, _ := f.run(t); !crashed {
				t.Fatalf("the first attempt did not reach %q", tc.crashAt)
			}
			f.rc.Attempt = 2
			got := f.mustRun(t)

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("the re-run's Result differs from an uninterrupted promote's (-uninterrupted +re-run):\n%s", diff)
			}
			if n := f.repo.merges(runBranch); n != tc.merges {
				t.Errorf("merge commits on the run branch = %d, want %d", n, tc.merges)
			}
			if n := f.repo.commitsWithSubject(runBranch, goal); n != 1 {
				t.Errorf("commits of the work on the run branch = %d, want exactly 1", n)
			}
		})
	}
}

// TestPromoteConflictPausesAndTouchesNothing: when the winner cannot merge
// cleanly, a person has to decide what to keep, which is a pause and not a
// failure. A failed run could not be resumed, and would strand a winner that
// was already selected and paid for. So nothing is destroyed, nothing is
// patched, and the Note says what to do.
func TestPromoteConflictPausesAndTouchesNothing(t *testing.T) {
	f := newRun(t, t.TempDir()).withCandidates(t, "c2", "c1", "c2")
	f.repo.conflicts[f.candidateBranch("c2")] = []string{"z.go", "a.go"}

	res, _, err := f.run(t)
	if err != nil {
		t.Fatalf("Run() error = %v, want a paused Result: a conflict is a verdict", err)
	}

	if res.Status != journal.StatusPaused {
		t.Errorf("Status = %v, want %v", res.Status, journal.StatusPaused)
	}
	if diff := cmp.Diff(state.Patch{}, res.Patch); diff != "" {
		t.Errorf("Patch mismatch (-want +got):\n%s", diff)
	}
	if calls := f.iso.DestroyCalls(); len(calls) != 0 {
		t.Errorf("Destroy was called on %v; a paused promote must leave every candidate in place", calls)
	}
	if got := f.repo.tipOf(runBranch); got != base {
		t.Errorf("the run branch moved to %s on a conflict", got)
	}
	for _, want := range []string{
		"a.go, z.go", f.candidateBranch("c2"), f.worktree, "git merge", "belay resume " + runID,
	} {
		if !strings.Contains(res.Note, want) {
			t.Errorf("Note %q does not mention %q", res.Note, want)
		}
	}
}

// TestPromoteNamesAFewConflictingFilesAndCountsTheRest keeps the Note short
// when a merge conflicts almost everywhere: it goes into the journal and the
// timeline, where one line is what a person reads.
func TestPromoteNamesAFewConflictingFilesAndCountsTheRest(t *testing.T) {
	f := newRun(t, t.TempDir()).withCandidates(t, "c1", "c1")
	f.repo.conflicts[f.candidateBranch("c1")] = []string{"a", "b", "c", "d", "e", "f", "g"}

	res, _, err := f.run(t)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(res.Note, "a, b, c, d, e and 2 more") {
		t.Errorf("Note %q does not name five files and count the rest", res.Note)
	}
}

// TestPromoteResumesAfterAPersonResolvesTheConflict: the person resumes by
// merging the winner in the run's worktree themselves. Promote then finds
// the work already there, merges nothing more, and records the person's
// merge as the branch's tip.
func TestPromoteResumesAfterAPersonResolvesTheConflict(t *testing.T) {
	f := newRun(t, t.TempDir()).withCandidates(t, "c2", "c1", "c2")
	f.repo.conflicts[f.candidateBranch("c2")] = []string{"a.go"}
	if res, _, _ := f.run(t); res.Status != journal.StatusPaused {
		t.Fatalf("Status = %v, want the conflict to pause", res.Status)
	}

	merged := f.repo.humanMerge(f.worktree, f.candidateBranch("c2"))
	res := f.mustRun(t)

	want := state.Patch{Promotion: &state.Promotion{Branch: runBranch, Base: base, Commit: merged, Candidate: "c2", Files: 1}}
	if diff := cmp.Diff(want, res.Patch); diff != "" {
		t.Errorf("Patch mismatch (-want +got):\n%s", diff)
	}
	if got := f.repo.merges(runBranch); got != 1 {
		t.Errorf("merge commits on the run branch = %d, want only the person's", got)
	}
	// The winner's work reached the branch in a commit carrying its
	// trailers, so a later re-run can recognise it without the workspace,
	// and both candidates can go.
	if got := len(f.iso.DestroyCalls()); got != 2 {
		t.Errorf("Destroy calls = %d, want both candidates destroyed", got)
	}
}

// TestPromoteTreatsTheWinnerAsPromotedOnlyWhenTheRunBranchRecordsIt closes
// the gap the design review found. Promote destroys candidates, and a crash
// can land after that and before the dispatcher records the node as
// finished; the re-run then finds the winner's workspace, and perhaps its
// branch, gone. It must not guess. The winner counts as promoted only when a
// commit on the run's branch carries both this run's Belay-Run trailer and
// the winner's Belay-Candidate trailer; without that record and without the
// workspace, there is nothing promote can vouch for.
func TestPromoteTreatsTheWinnerAsPromotedOnlyWhenTheRunBranchRecordsIt(t *testing.T) {
	t.Run("the winner is gone, and the run branch records merging it", func(t *testing.T) {
		f := newRun(t, t.TempDir()).withCandidates(t, "c2", "c1", "c2")
		// What a crashed attempt leaves: the merge, then the winner
		// destroyed, worktree and branch.
		merge := f.repo.commitOn(runBranch, vcs.Message{
			Subject:  "Merge candidate c2",
			Trailers: trailers("Belay-Run", runID, "Belay-Candidate", "c2"),
		}, "c2.go")
		f.repo.removeWorktree(f.candidateDir("c2"))
		delete(f.repo.tips, f.candidateBranch("c2"))
		if err := os.RemoveAll(f.candidateDir("c2")); err != nil {
			t.Fatalf("remove the winner: %v", err)
		}

		res := f.mustRun(t)

		want := state.Patch{Promotion: &state.Promotion{Branch: runBranch, Base: base, Commit: merge, Candidate: "c2", Files: 1}}
		if diff := cmp.Diff(want, res.Patch); diff != "" {
			t.Errorf("Patch mismatch (-want +got):\n%s", diff)
		}
		for _, call := range f.log.snapshot() {
			if strings.HasPrefix(call, "Commit") || strings.HasPrefix(call, "Integrate") {
				t.Errorf("promote called %q for a winner the branch already records", call)
			}
		}
	})

	t.Run("the winner is gone, and nothing records merging it", func(t *testing.T) {
		f := newRun(t, t.TempDir()).withCandidates(t, "c2", "c1", "c2")
		f.repo.removeWorktree(f.candidateDir("c2"))
		if err := os.RemoveAll(f.candidateDir("c2")); err != nil {
			t.Fatalf("remove the winner: %v", err)
		}

		res, _, err := f.run(t)

		if !errors.Is(err, promote.ErrWinnerMissing) {
			t.Fatalf("Run() error = %v, want one wrapping ErrWinnerMissing", err)
		}
		if res.Status != journal.StatusFailed {
			t.Errorf("Status = %v, want %v", res.Status, journal.StatusFailed)
		}
		if diff := cmp.Diff(state.Patch{}, res.Patch); diff != "" {
			t.Errorf("Patch mismatch (-want +got):\n%s", diff)
		}
		if calls := f.log.snapshot(); len(calls) != 0 {
			t.Errorf("calls = %v, want none: promote could vouch for nothing", calls)
		}
		if !strings.Contains(err.Error(), f.candidateDir("c2")) {
			t.Errorf("error %q does not name the missing workspace", err)
		}
	})

	t.Run("a commit from an earlier run naming the same candidate does not count", func(t *testing.T) {
		// Candidate IDs repeat from run to run, and an earlier run's
		// branch may already be part of the history this run started
		// from. Its Belay-Candidate trailer alone would read as "already
		// promoted" and skip the merge this run still owes.
		f := newRun(t, t.TempDir())
		f.repo.commitOn(runBranch, vcs.Message{
			Subject:  "Merge candidate c2",
			Trailers: trailers("Belay-Run", "20251231T000000Z-0ld0ld0ld0ld", "Belay-Candidate", "c2"),
		}, "old.go")
		f.withCandidates(t, "c2", "c1", "c2")

		res := f.mustRun(t)

		if got := f.repo.merges(runBranch); got != 1 {
			t.Errorf("merge commits on the run branch = %d, want this run's winner merged", got)
		}
		if got := res.Patch.Promotion; got == nil || got.Candidate != "c2" {
			t.Errorf("Promotion = %+v, want candidate c2 promoted", got)
		}
	})
}

// TestPromoteKeepsAWinnerTheRunBranchCannotVouchFor: a winner whose work is
// already on the run branch, but in no commit carrying its trailers — a
// person merged commits the candidate made itself — is promoted, yet kept.
// Destroying it would leave a re-run with neither the workspace nor a record,
// which is the one state promote cannot recover from, so the winner stays and
// the Note says where. The losers go as usual.
func TestPromoteKeepsAWinnerTheRunBranchCannotVouchFor(t *testing.T) {
	f := newRun(t, t.TempDir()).withCandidates(t, "c2", "c1", "c2")
	f.repo.commitOn(f.candidateBranch("c2"), vcs.Message{Subject: "the candidate's own commit"}, "c2.go")
	f.repo.dirty[f.candidateDir("c2")] = nil
	merged := f.repo.humanMerge(f.worktree, f.candidateBranch("c2"))

	first := f.mustRun(t)

	want := state.Patch{Promotion: &state.Promotion{Branch: runBranch, Base: base, Commit: merged, Candidate: "c2", Files: 1}}
	if diff := cmp.Diff(want, first.Patch); diff != "" {
		t.Errorf("Patch mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"Destroy c1"}, f.log.snapshot()); diff != "" {
		t.Errorf("calls mismatch (-want +got):\n%s", diff)
	}
	for _, want := range []string{"kept", f.candidateDir("c2")} {
		if !strings.Contains(first.Note, want) {
			t.Errorf("Note %q does not mention %q", first.Note, want)
		}
	}

	// And it is a stable answer: the re-run finds the same repository and
	// says the same thing.
	if diff := cmp.Diff(first, f.mustRun(t)); diff != "" {
		t.Errorf("a re-run's Result differs (-first +re-run):\n%s", diff)
	}
}

// TestPromoteReportsButDoesNotFailOnCleanupErrors: once the winner is on
// the run's branch, the run has succeeded. A candidate workspace that could
// not be removed is litter, reported in the Note and logged at Error; it is
// never a reason to fail a run whose result is already committed. And a
// candidate the isolator did not mark ephemeral is the user's own tree, so it
// is never destroyed at all.
func TestPromoteReportsButDoesNotFailOnCleanupErrors(t *testing.T) {
	f := newRun(t, t.TempDir()).withCandidates(t, "c2", "c1", "c2", "c3")
	f.rc.State.Candidates[2].Ephemeral = false
	destroy := f.iso.DestroyFunc
	f.iso.DestroyFunc = func(ctx context.Context, ws belay.Workspace) error {
		if ws.ID == "c1" {
			return errors.New("device busy")
		}
		return destroy(ctx, ws)
	}
	var logs bytes.Buffer
	f.rc.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	res := f.mustRun(t)

	if res.Status != journal.StatusOK || res.Next != graph.End {
		t.Errorf("Status, Next = %v, %q; want %v, %q", res.Status, res.Next, journal.StatusOK, graph.End)
	}
	if !strings.Contains(res.Note, f.candidateDir("c1")) {
		t.Errorf("Note %q does not name the workspace that was left behind", res.Note)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "device busy") {
		t.Errorf("the failed cleanup was not logged at Error with its cause:\n%s", logs.String())
	}
	var destroyed []string
	for _, ws := range f.iso.DestroyCalls() {
		destroyed = append(destroyed, ws.ID)
	}
	if slices.Contains(destroyed, "c3") {
		t.Errorf("destroyed %v, including c3, which is not ephemeral", destroyed)
	}
	if !slices.Contains(destroyed, "c2") {
		t.Errorf("destroyed %v, want the winner destroyed despite c1's failure", destroyed)
	}
}

// TestPromoteWiringBugsAreErrors: every one of these is a graph wired
// wrongly, not a verdict about the run's change, so each is an error that
// names what is missing, and promote touches nothing first.
func TestPromoteWiringBugsAreErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, f *fixture)
		want    error
	}{
		{
			name:    "an in-place run",
			arrange: func(_ *testing.T, f *fixture) { f.rc.Output = state.Output{} },
			want:    promote.ErrNotBranchOutput,
		},
		{
			name:    "an explicitly in-place run",
			arrange: func(_ *testing.T, f *fixture) { f.rc.Output = state.Output{Mode: state.OutputInPlace} },
			want:    promote.ErrNotBranchOutput,
		},
		{
			name:    "no repository",
			arrange: func(_ *testing.T, f *fixture) { f.rc.VCS = nil },
			want:    promote.ErrNoVCS,
		},
		{
			name:    "a branch with no worktree",
			arrange: func(_ *testing.T, f *fixture) { f.rc.Output.WorkTree = "" },
			want:    promote.ErrIncompleteOutput,
		},
		{
			name:    "a branch with no base",
			arrange: func(_ *testing.T, f *fixture) { f.rc.Output.Base = "" },
			want:    promote.ErrIncompleteOutput,
		},
		{
			name:    "a branch with no name",
			arrange: func(_ *testing.T, f *fixture) { f.rc.Output.Branch = "" },
			want:    promote.ErrIncompleteOutput,
		},
		{
			// git would resolve it against belay's own working directory.
			name:    "a worktree that is not an absolute path",
			arrange: func(_ *testing.T, f *fixture) { f.rc.Output.WorkTree = "repo/worktree" },
			want:    promote.ErrIncompleteOutput,
		},
		{
			name:    "candidates but no winner",
			arrange: func(t *testing.T, f *fixture) { f.withCandidates(t, "", "c1", "c2") },
			want:    promote.ErrNoWinner,
		},
		{
			name:    "a winner that is not a candidate",
			arrange: func(t *testing.T, f *fixture) { f.withCandidates(t, "c9", "c1", "c2") },
			want:    promote.ErrUnknownWinner,
		},
		{
			name:    "a winner with no candidates",
			arrange: func(_ *testing.T, f *fixture) { f.rc.State.Winner = "c1" },
			want:    promote.ErrUnknownWinner,
		},
		{
			name: "an isolator whose workspaces are not branches",
			arrange: func(t *testing.T, f *fixture) {
				f.withCandidates(t, "c1", "c1", "c2")
				f.rc.Isolator = &belaytest.FakeIsolator{}
			},
			want: promote.ErrUnpromotable,
		},
		{
			name: "no isolator",
			arrange: func(t *testing.T, f *fixture) {
				f.withCandidates(t, "c1", "c1", "c2")
				f.rc.Isolator = nil
			},
			want: promote.ErrUnpromotable,
		},
		{
			name: "an isolator that cannot name the winner's branch",
			arrange: func(t *testing.T, f *fixture) {
				f.withCandidates(t, "c1", "c1", "c2")
				f.iso.branchErr = errors.New("invalid id")
			},
			want: promote.ErrUnpromotable,
		},
		{
			name: "a winner whose workspace is not an absolute path",
			arrange: func(t *testing.T, f *fixture) {
				f.withCandidates(t, "c1", "c1", "c2")
				f.rc.State.Candidates[0].Dir = "c1"
			},
			want: promote.ErrUnpromotable,
		},
		{
			name: "a repository that cannot say what its branch records",
			arrange: func(t *testing.T, f *fixture) {
				f.withCandidates(t, "c1", "c1", "c2")
				f.rc.VCS = plainRepo{Repository: f.repo}
			},
			want: promote.ErrUnpromotable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRun(t, t.TempDir(), "a.go")
			tc.arrange(t, f)

			res, _, err := f.run(t)

			if !errors.Is(err, tc.want) {
				t.Fatalf("Run() error = %v, want one wrapping %v", err, tc.want)
			}
			if res.Status != journal.StatusFailed {
				t.Errorf("Status = %v, want %v", res.Status, journal.StatusFailed)
			}
			if diff := cmp.Diff(state.Patch{}, res.Patch); diff != "" {
				t.Errorf("Patch mismatch (-want +got):\n%s", diff)
			}
			if calls := f.log.snapshot(); len(calls) != 0 {
				t.Errorf("calls = %v, want none before a wiring bug is refused", calls)
			}
			if f.repo.tipOf(runBranch) != base {
				t.Error("the run branch moved although promote refused")
			}
		})
	}
}

// TestPromoteRepositoryFailuresAreErrors: a repository that cannot answer
// leaves promote without a verdict, so the answer is an error and no Patch
// — never a Result claiming a promotion nobody could confirm.
func TestPromoteRepositoryFailuresAreErrors(t *testing.T) {
	broken := errors.New("repository unavailable")
	for _, tc := range []struct {
		name   string
		fanout bool
		method string
	}{
		{name: "committing the run", method: "Commit"},
		{name: "listing what changed", method: "Changed"},
		{name: "asking whether the winner is recorded", fanout: true, method: "Carries"},
		{name: "committing the winner's work", fanout: true, method: "Commit"},
		{name: "merging the winner", fanout: true, method: "Integrate"},
		{name: "reading the branch's tip", fanout: true, method: "Tip"},
		{name: "listing what the merge changed", fanout: true, method: "Changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRun(t, t.TempDir(), "a.go")
			if tc.fanout {
				f = newRun(t, t.TempDir()).withCandidates(t, "c1", "c1")
			}
			f.repo.failures[tc.method] = broken

			res, _, err := f.run(t)

			if !errors.Is(err, broken) {
				t.Fatalf("Run() error = %v, want one wrapping the repository's", err)
			}
			if res.Status != journal.StatusFailed {
				t.Errorf("Status = %v, want %v", res.Status, journal.StatusFailed)
			}
			if diff := cmp.Diff(state.Patch{}, res.Patch); diff != "" {
				t.Errorf("Patch mismatch (-want +got):\n%s", diff)
			}
			if calls := f.iso.DestroyCalls(); len(calls) != 0 {
				t.Errorf("Destroy was called on %v without a confirmed promotion", calls)
			}
		})
	}
}

// TestPromoteRespectsACancelledContext: a cancelled run does no work, and
// says it was stopped rather than that it failed.
func TestPromoteRespectsACancelledContext(t *testing.T) {
	f := newRun(t, t.TempDir(), "a.go")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := promote.New().Run(ctx, f.rc)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want one wrapping context.Canceled", err)
	}
	if res.Status != journal.StatusAborted {
		t.Errorf("Status = %v, want %v", res.Status, journal.StatusAborted)
	}
	if calls := f.log.snapshot(); len(calls) != 0 {
		t.Errorf("calls = %v, want none", calls)
	}
}

func TestPromoteRefusesANilRunContext(t *testing.T) {
	res, err := promote.New().Run(context.Background(), nil)
	if err == nil {
		t.Fatal("Run(nil) returned no error")
	}
	if res.Status != journal.StatusFailed {
		t.Errorf("Status = %v, want %v", res.Status, journal.StatusFailed)
	}
}
