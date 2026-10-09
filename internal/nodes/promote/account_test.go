package promote_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/policy"
	"github.com/dhaam-ai/belay/internal/state"
)

// The docs reviewer's reproduction (M6): a branch run paused, a person
// appended a line to pyproject.toml in the run's worktree, and the resumed
// run committed it while the timeline said one file had been verified.
// promote commits only what the run's steps recorded and judged, so a change
// nobody accounted for pauses the run, names the file and says how to go on,
// and nothing is committed.
func TestPromotePausesOnAChangeNoStepRecorded(t *testing.T) {
	f := newRun(t, t.TempDir(), "src/app.go", "pyproject.toml")
	f.rc.State.Code.ChangedFiles = []string{"src/app.go"}

	res := f.mustRun(t)
	if res.Status != journal.StatusPaused {
		t.Fatalf("Status = %s, want %s: pyproject.toml was changed and no step recorded it (note %q)",
			res.Status, journal.StatusPaused, res.Note)
	}
	if res.Patch.Promotion != nil {
		t.Errorf("Patch.Promotion = %+v, want none: nothing was promoted", res.Patch.Promotion)
	}
	if tip := f.repo.tipOf(runBranch); tip != base {
		t.Errorf("%s moved to %s; nothing may be committed", runBranch, tip)
	}
	for _, fragment := range []string{"pyproject.toml", f.worktree, "belay resume " + runID} {
		if !strings.Contains(res.Note, fragment) {
			t.Errorf("Note %q never says %q", res.Note, fragment)
		}
	}
	if strings.Contains(res.Note, "src/app.go") {
		t.Errorf("Note %q names src/app.go, which the run recorded", res.Note)
	}
}

// The same holds for a fanout winner: its worktree may hold nothing beyond
// what its own candidate run recorded.
func TestPromotePausesOnAWinnerChangeNoStepRecorded(t *testing.T) {
	f := newRun(t, t.TempDir()).withCandidates(t, "cand-02", "cand-01", "cand-02")
	f.repo.branch(f.candidateDir("cand-02"), f.candidateBranch("cand-02"), base, "cand-02.go", "stray.txt")

	res := f.mustRun(t)
	if res.Status != journal.StatusPaused {
		t.Fatalf("Status = %s, want %s (note %q)", res.Status, journal.StatusPaused, res.Note)
	}
	if !strings.Contains(res.Note, "stray.txt") {
		t.Errorf("Note %q never names stray.txt", res.Note)
	}
	if f.repo.merges(runBranch) != 0 {
		t.Errorf("the winner was merged; nothing may be promoted")
	}
}

// The coordinator's M1: the set the gate judged and the set promote commits
// must be the same set, in content as well as in names. A change that
// passed the gate and was altered afterwards, by the test run or by hand, is
// judged again before it is committed, and one that breaks a rule now
// pauses the run rather than shipping.
func TestPromoteJudgesWhatItIsAboutToCommitAgainstThePolicy(t *testing.T) {
	f := newRun(t, t.TempDir(), "src/app.py")
	if err := os.MkdirAll(filepath.Join(f.worktree, "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "src", "app.py"), []byte("KEY = 'AKIAIOSFODNN7EXAMPLE'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rc.Policy = &state.PolicyPin{Policy: policy.Policy{
		ForbiddenContent: []policy.ContentRule{{Pattern: "AKIA[0-9A-Z]{16}", Reason: "an AWS access key id"}},
	}}

	res := f.mustRun(t)
	if res.Status != journal.StatusPaused {
		t.Fatalf("Status = %s, want %s (note %q)", res.Status, journal.StatusPaused, res.Note)
	}
	for _, fragment := range []string{"policy:forbidden_content", "belay resume " + runID} {
		if !strings.Contains(res.Note, fragment) {
			t.Errorf("Note %q never says %q", res.Note, fragment)
		}
	}
	if tip := f.repo.tipOf(runBranch); tip != base {
		t.Errorf("%s moved to %s; nothing may be committed", runBranch, tip)
	}
}

// A file the agent changed and never reported is recorded and judged like
// any other, and it ships, but it is named plainly in the commit, which is
// what a pull request's reader sees.
func TestPromoteNamesTheFilesTheAgentDidNotReportInTheCommit(t *testing.T) {
	f := newRun(t, t.TempDir(), "src/app.go", "src/helper.go")
	f.rc.State.Code.Unreported = []string{"src/helper.go"}

	res := f.mustRun(t)
	if res.Status != journal.StatusOK {
		t.Fatalf("Status = %s, want %s (note %q)", res.Status, journal.StatusOK, res.Note)
	}
	msg := f.repo.messageOf(f.repo.tipOf(runBranch))
	if !strings.Contains(msg.Body, "src/helper.go") || !strings.Contains(msg.Body, "the agent did not report") {
		t.Errorf("the commit body %q never names src/helper.go as a file the agent did not report", msg.Body)
	}
}
