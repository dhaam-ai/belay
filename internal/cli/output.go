package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/internal/git"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// branchPrefix is the namespace every branch belay creates lives under, so a
// person can see at a glance which branches are belay's and delete them
// together. The run's own branch is branchPrefix + its run id; a fanout
// candidate's is that branch, a "-" and the candidate's id (see
// internal/isolate/worktree for why the separator is not another "/").
const branchPrefix = "belay/"

// runBranch names the branch a run delivers its result on.
func runBranch(runID string) string { return branchPrefix + runID }

// resolveOutput settles how this run delivers its result, and why, before
// anything is created.
//
// It is the one place the question is asked. The answer is pinned in the
// run's manifest at createRun and never asked again, because the two answers
// differ in whether the user's own files change: a run that started on a
// branch and was resumed in place would edit a checkout its owner was
// promised belay would not touch (internal/state's Output says the same from
// the other side).
//
// "auto" is the default, so this has to be right without being asked. It
// chooses a branch only when a fresh checkout of HEAD faithfully reproduces
// what the run would otherwise edit: inside a git working tree, with at
// least one commit to branch from, no submodules, and a git new enough for
// the merge belay may have to make (git.MinVersion; internal/git says why
// 2.38). Anything else falls back to editing in place, and says which
// condition failed — a fallback nobody is told about is how a person comes
// to believe belay ignored their configuration.
//
// An explicit output.mode is obeyed or refused, never quietly downgraded.
// "branch" in a directory that cannot take one is a blocker: the person
// asked belay not to touch their checkout, and editing it anyway because the
// alternative was unavailable is the one outcome they ruled out.
//
// It writes nothing. Every git command it runs is a read, and `git status`
// goes through internal/git, which passes --no-optional-locks so even the
// index's mtime is left alone — which is what lets --dry-run describe a
// branch run while creating nothing at all.
func (p *runPlan) resolveOutput(ctx context.Context) {
	want := p.config.Output.Mode
	if want == config.OutputInPlace {
		p.output = state.Output{Mode: state.OutputInPlace}
		p.outputReason = "output.mode is \"in_place\", so belay edits this workspace directly"
		p.resolveIsolator()
		return
	}

	repo, why := p.openRepo(ctx)
	if repo == nil {
		if want == config.OutputBranch {
			p.block(
				fmt.Sprintf("output.mode is \"branch\", but %s cannot take one: %s.", p.workspace, why),
				"set output.mode to \"auto\" or \"in_place\" in belay.yaml, or point --workspace at a git repository "+
					"with at least one commit",
			)
			return
		}
		p.output = state.Output{Mode: state.OutputInPlace}
		p.outputReason = fmt.Sprintf("belay is editing this workspace directly: %s", why)
		p.resolveIsolator()
		return
	}

	head, err := repo.Head(ctx)
	if err != nil {
		// Only ErrNoCommits reaches here as a condition about the tree
		// rather than about git; openRepo has already refused the rest.
		p.fallBackOrBlock(want, fmt.Sprintf("belay cannot read HEAD in %s: %v", repo.Top(), err))
		return
	}

	p.repo = repo
	p.output = state.Output{
		Mode:    state.OutputBranch,
		Repo:    repo.Top(),
		Base:    head.Commit,
		BaseRef: head.Branch,
		Prefix:  repo.Prefix(),
	}
	p.outputReason = branchReason(want, repo, head)
	p.resolveIsolator()
	p.warnAboutUncommittedWork(ctx, repo)
}

// openRepo returns the repository a branch run would work in, or nil and the
// reason in words a person can act on.
//
// Every refusal is phrased as a fact about this workspace rather than as an
// error string, because it is printed in a sentence explaining a choice, not
// reported as a failure. The three that are not faults — not a repository, no
// working tree, no commits — read as conditions; a git too old to use is a
// toolchain problem and says which version is needed.
func (p *runPlan) openRepo(ctx context.Context) (*git.Repo, string) {
	repo, err := git.Open(ctx, exec.New(p.logger), p.workspace)
	switch {
	case errors.Is(err, git.ErrNotARepository):
		return nil, fmt.Sprintf("%s is not inside a git repository", p.workspace)
	case errors.Is(err, git.ErrBare):
		return nil, fmt.Sprintf("%s has no working tree to branch from", p.workspace)
	case errors.Is(err, belay.ErrToolchainMissing):
		return nil, fmt.Sprintf("belay needs git %s or newer to work on a branch: %v", git.MinVersion, err)
	case err != nil:
		return nil, fmt.Sprintf("belay cannot read the git repository at %s: %v", p.workspace, err)
	case repo.HasSubmodules():
		// A worktree checkout does not populate submodules, so the run's
		// tree would be missing whatever they hold, and its tests would
		// fail for a reason that has nothing to do with the change.
		return nil, fmt.Sprintf("%s has submodules, and a new worktree would not have their contents", repo.Top())
	}
	if _, err := repo.Head(ctx); errors.Is(err, git.ErrNoCommits) {
		// Deliberately phrased as something to do, not as "never": one
		// commit is the whole fix.
		return nil, fmt.Sprintf("%s has no commits yet, so there is nothing to branch from; make one first", repo.Top())
	}
	return repo, ""
}

// fallBackOrBlock records why a branch run is impossible: as a blocker when
// the person asked for one, and as the reason for editing in place otherwise.
func (p *runPlan) fallBackOrBlock(want config.OutputMode, why string) {
	if want == config.OutputBranch {
		p.block(
			fmt.Sprintf("output.mode is \"branch\", but %s cannot take one: %s.", p.workspace, why),
			"set output.mode to \"auto\" or \"in_place\" in belay.yaml",
		)
		return
	}
	p.output = state.Output{Mode: state.OutputInPlace}
	p.outputReason = "belay is editing this workspace directly: " + why
	p.resolveIsolator()
}

// branchReason says why this run works on a branch, naming the setting when
// the person chose it and the conditions when belay did.
func branchReason(want config.OutputMode, repo *git.Repo, head git.Head) string {
	from := "a detached HEAD"
	if head.Branch != "" {
		from = head.Branch
	}
	if want == config.OutputBranch {
		return fmt.Sprintf("output.mode is \"branch\", so belay works on its own branch off %s in %s", from, repo.Top())
	}
	return fmt.Sprintf("%s is a git repository with a commit to branch from, so belay works on its own branch off %s "+
		"and never edits your checkout", repo.Top(), from)
}

// resolveIsolator settles which isolator a fanout candidate would get, and
// refuses the one combination that cannot work.
//
// "auto" follows the output mode, because the two are one decision: a
// candidate is promoted by merging its branch, so a candidate without a
// branch cannot be promoted. Asking for dircopy in a branch run is therefore
// refused rather than honoured — belay would run the candidates, pick a
// winner, and have no way to deliver it.
func (p *runPlan) resolveIsolator() {
	if p.config.Fanout.Isolator != config.IsolatorDircopy || !p.output.Branched() {
		return
	}
	p.block(
		"fanout.isolator is \"dircopy\" and this run delivers its result on a branch, but a directory copy has no "+
			"branch to merge, so belay could not promote whichever candidate won.",
		"set fanout.isolator to \"auto\" or \"worktree\", or set output.mode to \"in_place\" in belay.yaml",
	)
}

// isolatorName is the isolator this plan would give a fanout candidate, with
// "auto" resolved: a git worktree for a branch run, a directory copy
// otherwise.
func (p *runPlan) isolatorName() config.IsolatorName {
	if p.config.Fanout.Isolator != config.IsolatorAuto {
		return p.config.Fanout.Isolator
	}
	if p.output.Branched() {
		return config.IsolatorWorktree
	}
	return config.IsolatorDircopy
}

// warnAboutUncommittedWork says that a branch run starts from the commit.
//
// It is a warning and not a blocker: the run works fine, and belay refusing
// to start over files it is not going to touch would be worse than the
// surprise. The surprise is real, though — a person with an hour of
// uncommitted work expects belay to build on it, and a branch run does not —
// so it is said once, with a count, before anything is spent.
//
// belay's own .belay directory is not counted. internal/git's Status excludes
// it, because a run's nested worktree makes git report it as untracked, and a
// warning that fires on every run is a warning people learn to skip.
func (p *runPlan) warnAboutUncommittedWork(ctx context.Context, repo *git.Repo) {
	dirty, err := repo.Status(ctx, p.workspace)
	if err != nil {
		p.warn("belay could not check whether %s has uncommitted changes: %v", p.workspace, err)
		return
	}
	if len(dirty) == 0 {
		return
	}
	p.warn("%s has %d uncommitted change(s), starting with %s. A branch run starts from the commit %s, "+
		"so they are not part of this run and belay will not touch them.",
		p.workspace, len(dirty), dirty[0], shortCommit(p.output.Base))
}

// workStore returns the Store this run's dispatcher uses, with its Layout
// pointed at the tree the run's nodes work in.
//
// A branch run works in its own git worktree; an in-place run works in the
// workspace it was started in, which is what a Layout already says. It is one
// function because `belay run` and `belay resume` must agree about where a
// run's nodes write, and the shape of bug they would otherwise produce is
// only found later, in a checkout that should have been clean.
//
// RunDir does not move either way: the manifest, the journal and the
// artifacts stay where every reader already looks for them.
func (p *runPlan) workStore(layout state.Layout) (*state.Store, error) {
	if !p.output.Branched() {
		return state.NewStore(layout), nil
	}
	redirected, err := layout.WithWorkTree(workTreeFor(layout, p.output))
	if err != nil {
		return nil, fmt.Errorf("cannot point run %s at its worktree: %w", layout.RunID(), err)
	}
	return state.NewStore(redirected), nil
}

// finishRun does what belay does to a branch run's worktree once a dispatch
// has returned. It is a step of afterCompletion, and like every step there it
// cannot change how the run is reported.
//
// A completed run's result is on its branch, and the worktree is a second
// checkout of the whole repository taking up disk for nothing. One left
// behind per run fills a disk and clutters `git worktree list`, so it goes.
//
// A run that paused, failed or was stopped is not over. Its worktree is where
// a person reads what belay did, finishes a conflicting merge by hand, or
// resumes from. Removing it would throw away exactly the uncommitted work a
// resume needs, which is why this asks the run's status rather than tidying
// unconditionally.
//
// The branch is never deleted, whatever happened. It is the deliverable, and
// deleting it would be deleting the run's result.
func (p *runPlan) finishRun(ctx context.Context, view outcomeView) {
	out := view.output
	if out.Branch == "" {
		out = p.output
	}
	if !out.Branched() || p.repo == nil || view.outcome.Status != state.RunStatusCompleted {
		return
	}
	if err := p.repo.RemoveWorktree(ctx, out.WorkTree); err != nil {
		// Said, never returned: the run's result is on its branch, and
		// reporting a completed run as failed would tell a script to redo
		// work that is already done.
		p.logger.Error("belay could not remove this run's worktree; the run's result is on its branch and unaffected",
			slog.String("worktree", out.WorkTree), slog.String("branch", out.Branch), slog.Any("error", err))
	}
}

// workTreeFor returns the directory a branch run's nodes work in: the run's
// own git worktree, plus the prefix the run was started at, so a run started
// in a subdirectory works on that subdirectory and not on the repository top.
func workTreeFor(layout state.Layout, out state.Output) string {
	if out.Prefix == "" {
		return layout.WorkTreeDir()
	}
	return filepath.Join(layout.WorkTreeDir(), filepath.FromSlash(strings.TrimSuffix(out.Prefix, "/")))
}

// shortCommit renders a commit id at the length a person types.
func shortCommit(id string) string {
	const width = 12
	if len(id) <= width {
		return id
	}
	return id[:width]
}

// openRun readies a chosen run for dispatch: it runs resumePreflight, which
// settles the run's output from its pin rather than from belay.yaml, and
// returns the Store the dispatcher must use -- redirected to the run's own
// worktree when the run delivers on a branch.
//
// Every command that continues a run goes through here, so that none of them
// can dispatch a branch run against the user's checkout. belay plan
// --regenerate once built its own store and did exactly that.
func (p *runPlan) openRun(ctx context.Context, layout state.Layout) (*state.Store, error) {
	if err := p.resumePreflight(ctx, state.NewStore(layout)); err != nil {
		return nil, err
	}
	// The preflight is what settles the output, so the store the dispatcher
	// gets is built after it, from the pin rather than from belay.yaml.
	return p.workStore(layout)
}
