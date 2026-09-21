package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/internal/git"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/planarchive"
	"github.com/dhaam-ai/belay/internal/state"
)

// archivePlan moves a completed run's plan out of the run directory and says
// where it went. It is a step of afterCompletion, which says why it comes
// where it does, and like every step there it cannot change how the run is
// reported.
//
// internal/planarchive does the moving: in a run that edited its workspace
// in place it writes the plan into docs/plans (or wherever the run's
// manifest pinned) and removes the run's copy; in a run on a branch, whose
// promote node already put the plan in the run's commit, it confirms the
// plan is there and removes the run's copy. It is given the Store the run's
// nodes worked through, rebuilt from view.layout, because for a branch run
// that is the one pointed at the run's worktree.
//
// A plan that stays in the run directory is said on stderr, with the reason
// and where the plan is, because the person has to know their plan did not
// go where the run said it would, and with the command that finishes the
// move once the way is clear. That command is left out only where it could
// not help: a branch run whose commit does not have the plan. A run that
// never asked for archival, or did not complete, gets no line at all: its
// plan is where it always was.
func archivePlan(u *ui, view outcomeView) {
	if view.outcome.Status != state.RunStatusCompleted {
		return
	}
	res, err := planarchive.File(state.NewStore(view.layout))
	retry := func() {
		u.note("belay: once that is fixed, move it with: belay plan --archive %s", view.layout.RunID())
	}
	switch {
	case err != nil:
		u.note("belay: the plan stays in the run directory, at %s: belay could not move it: %v", orUnknown(res.Source), err)
		retry()
	case res.Outcome == planarchive.Kept:
		u.note("belay: the plan stays in the run directory, at %s: %s", orUnknown(res.Source), res.Reason)
		if !view.output.Branched() {
			retry()
		}
	case res.Outcome == planarchive.NotAsked:
	case view.output.Branched():
		u.say("  plan committed on %s, at %s", view.output.Branch, res.Rel)
	default:
		u.say("  plan moved to %s", res.Path)
	}
}

// archivePlanNow is `belay plan --archive <run-id>`: it finishes moving a
// completed run's plan into the repository, if belay has not already.
//
// A run moves its plan itself as it finishes (archivePlan). This is the way
// to finish the move when the run could not — somebody's file was in the
// way, a symbolic link led out of the workspace, or belay was stopped
// between the run completing and the plan moving — once the way is clear.
// It is the same move, planarchive.File, so a retry after a crash picks up
// where the crash left it and never files the plan twice.
//
// It needs a run id, because it writes into the repository, and doing that
// for a run belay picked on the person's behalf is not something to leave
// implicit (ADR-0010). The plan goes where the run's manifest pinned when
// the run started, never where belay.yaml says now, so a retry keeps the
// promise the run made. A run that has not completed is refused before
// anything is read: its plan describes code that does not exist yet. For a
// run on a branch it can only finish a move whose plan is already in the
// run's commit, which it finds in the run's worktree; belay keeps that
// worktree until the plan has moved.
func archivePlanNow(cmd *cobra.Command, runID string) error {
	fail := func(err error) error {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	if runID == "" {
		return fail(errors.New("name the run whose plan to move: belay plan --archive <run-id>"))
	}
	u := newUI(cmd)
	workspace, err := workspaceDirOf(cmd)
	if err != nil {
		return err
	}
	chosen, err := choosePlanRun(workspace, runID)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: err.Error(), Err: err}
	}
	if chosen.status != state.RunStatusCompleted {
		return fail(fmt.Errorf("belay will not move the plan of run %s: the run has not finished (it is %s), and only a completed run's plan moves",
			chosen.id, statusSentence(chosen.status, chosen.node)))
	}
	layout, err := state.NewLayout(workspace, chosen.id)
	if err != nil {
		return fail(err)
	}
	man, err := state.LoadManifest(layout.ManifestPath())
	if err != nil {
		return fail(fmt.Errorf("cannot read this run's manifest: %w", err))
	}
	if man.ArchivedPlan.Dir == "" {
		return fail(fmt.Errorf("run %s was started with plan.archive off, so belay leaves its plan in the run directory", chosen.id))
	}
	if man.Output.Branched() {
		if layout, err = layout.WithWorkTree(workTreeFor(layout, man.Output)); err != nil {
			return fail(err)
		}
	}
	if rootFlags.dryRun {
		u.say("This is a dry run: belay would finish moving the plan of run %s, to %s. Nothing was moved.",
			chosen.id, planarchive.Rel(man.ArchivedPlan.Dir, man.RunID, man.Goal))
		return nil
	}

	store := state.NewStore(layout)
	// Recording where the plan went writes the manifest, and every writer
	// to an existing run checks first that this belay may (ADR-0011).
	if err := store.Preflight(); err != nil {
		return fail(err)
	}
	res, err := planarchive.File(store)
	switch {
	case err != nil:
		return fail(fmt.Errorf("could not move the plan of run %s: %w; it is still at %s", chosen.id, err, orUnknown(res.Source)))
	case res.Outcome == planarchive.Kept:
		return fail(fmt.Errorf("did not move the plan of run %s: %s. It is still at %s", chosen.id, res.Reason, orUnknown(res.Source)))
	case res.Outcome == planarchive.AlreadyFiled && man.Output.Branched():
		u.say("belay had already moved the plan of run %s: it is committed on %s, at %s", chosen.id, man.Output.Branch, res.Rel)
	case res.Outcome == planarchive.AlreadyFiled:
		u.say("belay had already moved the plan of run %s, to %s", chosen.id, filepath.Join(workspace, filepath.FromSlash(res.Rel)))
	case man.Output.Branched():
		u.say("  plan committed on %s, at %s", man.Output.Branch, res.Rel)
	default:
		u.say("  plan moved to %s", res.Path)
	}
	return nil
}

// choosePlanRun picks the run `belay plan` is about: the one named, whatever
// state it is in, or, given no id, the most recent run still waiting.
//
// Reading a plan is not continuing a run, so a run named outright is shown
// even when it is over. It used to be refused, through belay resume's own
// picker, with "it is already finished" — and a finished run is exactly the
// one whose plan has moved out of its run directory and is hardest to find.
// Given no id, belay still picks among waiting runs only: the plan a person
// reaches for without naming a run is the one a run is waiting on.
func choosePlanRun(workspace, runID string) (resumableRun, error) {
	if runID == "" {
		return chooseRun(workspace, "")
	}
	runs, err := scanRunsForResume(workspace)
	if err != nil {
		return resumableRun{}, err
	}
	for _, r := range runs {
		if r.id != runID {
			continue
		}
		switch {
		case r.needsNewer():
			return resumableRun{}, fmt.Errorf("belay cannot read run %s: it needs a newer belay.\n%w", r.id, r.unreadable)
		case r.unreadable != nil:
			return resumableRun{}, damagedRun(workspace, r)
		}
		return r, nil
	}
	return resumableRun{}, unknownRun(workspace, runID, runs)
}

// showFinishedPlan prints the plan of a run that is over — completed,
// failed or stopped — from wherever it is now, and says what it is: a
// record. Nothing reads it again, so none of the advice a waiting run gets
// about editing and resuming applies. A completed run whose plan belay was
// to move and did not is told how to finish the move.
func showFinishedPlan(ctx context.Context, u *ui, workspace string, chosen resumableRun, layout state.Layout, st state.State) error {
	man, err := state.LoadManifest(layout.ManifestPath())
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: cannot read this run's manifest: " + err.Error(), Err: err}
	}
	body, where, err := finishedPlan(ctx, u, workspace, layout, man, st)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}

	u.say("run %s", chosen.describe(false))
	u.say("  what you asked for:  %s", orUnrecorded(chosen.goal))
	u.say("  the plan is at:      %s", where)
	u.say("")
	u.say("%s", strings.TrimRight(string(body), "\n"))
	u.say("")
	if chosen.status == state.RunStatusCompleted && man.ArchivedPlan.Dir != "" && man.ArchivedPlan.Path == "" {
		u.say("belay was to move this plan out of the run directory when the run finished, and did not. Move it with:")
		u.say("")
		u.say("    belay plan --archive %s", chosen.id)
		return nil
	}
	u.say("This run is over, so its plan is a record of what it set out to do; nothing reads it again.")
	return nil
}

// finishedPlan returns a finished run's plan and where it is. It looks where
// the plan can be, in this order: the archived copy in the workspace, which
// is where an in-place run's plan went and where a branch run's arrives once
// the person takes the branch; the run's commit, for a branch run whose
// branch has not been taken; and the run's own copy, for a run whose plan
// never moved, or whose move was cut short before the copy was removed.
func finishedPlan(ctx context.Context, u *ui, workspace string, layout state.Layout, man state.Manifest, st state.State) ([]byte, string, error) {
	if rel := man.ArchivedPlan.Path; rel != "" {
		abs := filepath.Join(workspace, filepath.FromSlash(rel))
		if body, err := os.ReadFile(abs); err == nil { //nolint:gosec // the manifest's own relative path, under the workspace
			return body, abs, nil
		}
		if man.Output.Branched() {
			body, err := planInCommit(ctx, u, man, st, rel)
			if err != nil {
				return nil, "", fmt.Errorf("run %s committed its plan on %s, at %s, and belay cannot read it from there: %w",
					man.RunID, man.Output.Branch, rel, err)
			}
			return body, fmt.Sprintf("%s, in commit %s on %s", rel, shortCommit(st.Promotion.Commit), man.Output.Branch), nil
		}
	}
	_, abs, err := approve.PlanArtifact(layout, st.Plan.Path)
	if err != nil {
		return nil, "", err
	}
	body, err := os.ReadFile(abs) //nolint:gosec // path is constrained by approve.PlanArtifact
	if err == nil {
		return body, abs, nil
	}
	if rel := man.ArchivedPlan.Path; rel != "" {
		return nil, "", fmt.Errorf("run %s moved its plan to %s, and it is not there any more",
			man.RunID, filepath.Join(workspace, filepath.FromSlash(rel)))
	}
	return nil, "", fmt.Errorf("cannot read %s: %w", abs, err)
}

// planInCommit reads a branch run's plan from the commit its promote node
// made. The run filed it relative to its workspace, which the repository
// sees under the run's prefix.
func planInCommit(ctx context.Context, u *ui, man state.Manifest, st state.State, rel string) ([]byte, error) {
	if st.Promotion.Commit == "" {
		return nil, errors.New("the run recorded no commit")
	}
	repo, err := git.Open(ctx, exec.New(newLogger(u)), man.Output.Repo)
	if err != nil {
		return nil, err
	}
	return repo.FileAt(ctx, st.Promotion.Commit, man.Output.Prefix+rel)
}

// orUnknown fills in for a path belay could not work out.
func orUnknown(path string) string {
	if path == "" {
		return "an unknown path"
	}
	return path
}
