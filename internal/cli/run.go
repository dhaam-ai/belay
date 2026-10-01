package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/budget"
	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/detect"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/nodes/fanout"
	"github.com/dhaam-ai/belay/internal/planarchive"
	"github.com/dhaam-ai/belay/internal/policy"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// runFlags holds the settings `belay run` and `belay resume` share. Both
// commands need to know which repository they are working on and where a
// recorded conversation lives, and they answer to the same flag names so a
// person only learns them once.
var runFlags struct {
	cassette    string
	pullRequest bool
}

// addRunFlags gives cmd the flags a run needs.
func addRunFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&runFlags.cassette, "cassette", "",
		"the recorded conversation to replay from, or record to (default <workspace>/.belay/cassette.json)")
}

// addPullRequestFlag gives cmd the --pr flag.
//
// Only `belay run` has it. A resume needs none, because the intent is pinned
// in the run's manifest when the run is created: somebody who started a run
// with --pr and resumes it three days later still gets the pull request they
// asked for, and somebody who did not cannot acquire one by resuming.
func addPullRequestFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&runFlags.pullRequest, "pr", false,
		"when the run finishes, push its branch and open a draft pull request through gh")
}

var runCmd = &cobra.Command{
	Use:   `run "<what you want built>"`,
	Short: "Start a run: say what you want built, and belay builds it",
	Long: `Start a run.

Say what you want built, in your own words, as one argument:

    belay run "add table-driven tests for the config parser"

belay writes a plan first and then stops, so you can read the plan — and edit
it — before anything happens. Continue with "belay resume". From there belay
writes the code, records which files it changed, runs your tests, fixes what
fails, and puts the result through a quality gate.

In a git repository belay works on its own branch, belay/<run-id>, in its own
worktree, and commits once the quality gate has passed. Your checkout is never
edited, and uncommitted work in it is not part of the run. Outside a git
repository, or with output.mode "in_place", belay edits the workspace
directly. It says which it is doing before it starts.

Add --pr and belay pushes that branch and opens a draft pull request through
gh when the run finishes.

belay stops on its own at the budget ceiling set in belay.yaml, and it prints
that ceiling before it spends anything.

Every step is written to disk as it happens, under <workspace>/.belay/runs/.
Press Ctrl-C whenever you like: the run stops, its state stays, and
"belay resume" picks it up from the last completed step.

Use --dry-run to see exactly what a run would do — the settings it read, the
project it found, the tools it would use, and the most it could spend —
without creating anything or spending anything.`,
	Example: `  belay run "add tests for the config parser"
  belay run --pr "make the CLI accept a --json flag"
  belay run --workspace ../other-repo "upgrade to the v2 client"
  belay --dry-run run "upgrade to the v2 client"`,
	Args: exactlyOneDescription,
	RunE: func(cmd *cobra.Command, args []string) error {
		return startRun(cmd, strings.TrimSpace(args[0]))
	},
}

func init() {
	addRunFlags(runCmd)
	addPullRequestFlag(runCmd)
	rootCmd.AddCommand(runCmd)
}

// exactlyOneDescription insists on the one thing `belay run` needs, and says
// so in the words a person would use rather than cobra's "accepts 1 arg(s)".
func exactlyOneDescription(_ *cobra.Command, args []string) error {
	switch {
	case len(args) == 0:
		return errors.New(`say what you want built, in quotes: belay run "add tests for the parser"`)
	case len(args) > 1:
		return fmt.Errorf(`belay takes one description, in quotes, but got %d arguments; `+
			`did you forget the quotes around "%s"?`, len(args), strings.Join(args, " "))
	case strings.TrimSpace(args[0]) == "":
		return errors.New("that description is empty; tell belay what you want built")
	default:
		return nil
	}
}

// startRun resolves a run, creates it on disk, and drives it to its end.
func startRun(cmd *cobra.Command, want string) error {
	u := newUI(cmd)

	plan, err := resolve(resolveRequest{
		workspace:  rootFlags.workspace,
		configPath: rootFlags.configPath,
		cassette:   runFlags.cassette,
		logger:     newLogger(u),
		context:    cmd.Context(),
	})
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}

	plan.goal = want
	if rootFlags.dryRun {
		return describeDryRun(u, plan, want, "")
	}
	if len(plan.blockers) > 0 {
		return refuseToStart(u, plan)
	}

	runID, err := state.NewRunID()
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	store, err := createRun(cmd.Context(), plan, runSpec{Workspace: plan.workspace, RunID: runID, Goal: want})
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}

	announceStart(u, plan, store.Layout(), want)
	return driveRun(cmd, u, plan, store, false)
}

// runSpec names the run createRun makes: where it lives, what it is called,
// and what it is for.
//
// The caller chooses the run id, not createRun, so that a caller can record
// the id somewhere of its own before the run exists and find the run again
// after a crash. `belay run` has nowhere to record it and simply asks
// state.NewRunID for one.
type runSpec struct {
	// Workspace is the absolute path of the repository the run works on.
	// The run's directory is made under it, in .belay/runs/.
	Workspace string
	// RunID names the run and its directory.
	RunID string
	// Goal is what the person asked for, exactly as they typed it.
	Goal string
}

// createRun lays down the run directory, its initial state and its manifest,
// in that order. It is the first thing in `belay run` that writes anything at
// all; everything before it is resolution, which is what lets --dry-run stop
// here.
//
// The manifest goes last because writing it is what creates the run.
// Everything that looks for runs reads manifest.json and nothing else: belay
// resume chooses from the manifests it finds, and belay runs lists them. The
// dispatcher, for its part, cannot continue a run without state.json. When
// the manifest came first, a failure or a crash before state.json was written
// left a manifest saying "running" over a run with no state: resume chose it,
// and the dispatcher failed to load it. Now a failure or a crash anywhere
// before the manifest leaves a directory without one, which resume never
// chooses and belay runs reports as unreadable.
//
// createRun does not delete that directory when it fails. A crash leaves the
// same directory with no chance to delete anything, so every reader must
// already treat a directory without a manifest as not a run; deleting it on
// the error path would add a way to remove files without sparing any reader
// that work. Calling createRun again for the same run id converges instead:
// it writes the state again, whole, then the manifest. It must not be called
// for a run whose manifest exists, which it would start over.
func createRun(ctx context.Context, plan *runPlan, spec runSpec) (*state.Store, error) {
	layout, err := state.NewLayout(spec.Workspace, spec.RunID)
	if err != nil {
		return nil, err
	}
	// Creating artifacts/ creates the run directory above it in one call.
	if err := os.MkdirAll(layout.ArtifactsDir(), 0o750); err != nil {
		return nil, fmt.Errorf("cannot create the run directory %s: %w", layout.RunDir(), err)
	}
	// The first run in a repository is what creates .belay, so it is also
	// what keeps .belay out of commits.
	if err := ignoreRunState(spec.Workspace); err != nil {
		return nil, err
	}

	if err := state.SaveState(layout.StatePath(), state.NewState(spec.Goal)); err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", layout.StatePath(), err)
	}
	now := time.Now().UTC()
	manifest := state.NewManifest(now, spec.RunID, spec.Workspace, spec.Goal, plan.config)
	if err := plan.prepareWorkspace(ctx, layout, &manifest); err != nil {
		return nil, err
	}
	// After prepareWorkspace, because the baseline is a survey of the tree
	// this run's nodes will work in, and for a branch run that tree does not
	// exist until the worktree has been made. Before the manifest, so the pin
	// lands in the manifest's one write.
	if err := plan.pinPolicy(ctx, layout, &manifest); err != nil {
		plan.unwindWorkspace(ctx, manifest.Output)
		return nil, err
	}
	// The fanout's shape lands in the manifest's one write too, through
	// SetFanout, so the field and the capability an older belay would have
	// to refuse are one decision (ADR-0011): a belay that dropped the pin
	// would resume this run as a single attempt and spend the ceiling again.
	// From here on the pin, not belay.yaml, decides how this run is walked.
	if plan.fanout != nil {
		manifest.SetFanout(*plan.fanout)
	}
	if err := state.SaveManifest(layout.ManifestPath(), manifest); err != nil {
		plan.unwindWorkspace(ctx, manifest.Output)
		return nil, fmt.Errorf("cannot write %s: %w", layout.ManifestPath(), err)
	}
	// Only now, with the run findable, is the Layout pointed at the tree its
	// nodes work in; workStore is the same step a resume takes.
	return plan.workStore(layout)
}

// pinPolicy records the guardrails this run is held to, and the baseline they
// are measured against, in the run's manifest.
//
// The baseline is surveyed here rather than at resolve time because it must
// describe the tree the run's nodes will really work in: a branch run's
// worktree, which prepareWorkspace has just created, and whose contents can
// differ from the person's checkout because the worktree starts from the
// commit and the checkout may have uncommitted work. Surveying the wrong tree
// would report a dependency as new the first time the agent touched a
// manifest.
//
// A survey it cannot complete refuses the run. The alternative -- start the
// run and pin no baseline -- is a run whose belay.yaml declares guardrails and
// whose graph has no gate, which is the silently-does-nothing failure this
// whole feature exists to prevent, and the person would have no way to tell.
//
// A run that declares no policy pins nothing, surveys nothing, and requires
// no capability. Guardrails are opt-in, and a repository that declares none
// starts runs exactly as it always has.
func (p *runPlan) pinPolicy(ctx context.Context, layout state.Layout, manifest *state.Manifest) error {
	if p.policy == nil {
		return nil
	}
	dir := layout.WorkspaceDir()
	if manifest.Output.Branched() {
		dir = workTreeFor(layout, manifest.Output)
	}
	baseline, err := policy.Survey(ctx, dir)
	if err != nil {
		return fmt.Errorf("belay.yaml declares a policy, and belay cannot record what %s declares now, "+
			"so it has nothing to hold this run's change to: %w", dir, err)
	}
	// Through SetPolicy, never by assigning Manifest.Policy: the field and
	// the capability an older belay would have to refuse are one decision
	// (ADR-0011, and state.CapabilityPolicyGate).
	manifest.SetPolicy(state.PolicyPin{Policy: p.policy.Policy, Baseline: baseline})
	return nil
}

// prepareWorkspace readies the workspace a new run will change, and records in
// the run's manifest whatever a later resume must know about it. createRun
// calls it once, after the run's state is written and before its manifest is.
//
// An in-place run changes the workspace it was started in, so there is
// nothing to create and nothing to record. A branch run works somewhere else:
// this creates the branch belay/<run-id> at the commit HEAD is on, and a git
// worktree for it under the run directory, and pins both through
// Manifest.SetOutput — never by assigning Output, because the field and the
// capability an older belay would have to refuse are one decision (ADR-0011,
// and state.CapabilityOutputBranch).
//
// It keeps to the order around it. It runs after state.json, so a failure
// here leaves a directory with no manifest, which nothing takes for a run. It
// runs before the manifest, so what it records lands in the manifest's one
// write. And it converges: called again for the same run id it adopts a
// branch already at the same base and a worktree already registered, rather
// than failing on what a previous attempt left.
//
// A failure part way leaves nothing behind. The branch is created first
// because AddWorktree refuses to create one (internal/git says why: `worktree
// add -b` leaks its branch when the add fails), so the only window is between
// the two, and unwindWorkspace closes it.
func (p *runPlan) prepareWorkspace(ctx context.Context, layout state.Layout, manifest *state.Manifest) error {
	if !p.output.Branched() || p.repo == nil {
		return nil
	}
	out := p.output
	out.Branch = runBranch(layout.RunID())
	out.WorkTree = layout.WorkTreeDir()
	out.PullRequest = runFlags.pullRequest

	if err := p.createRunBranch(ctx, out); err != nil {
		return err
	}
	manifest.SetOutput(out)
	p.output = out
	return nil
}

// createRunBranch creates the run's branch and the worktree it works in, and
// leaves neither behind if the other cannot be made.
func (p *runPlan) createRunBranch(ctx context.Context, out state.Output) error {
	created, err := p.claimBranch(ctx, out)
	if err != nil {
		return err
	}
	if err := p.claimWorkTree(ctx, out); err != nil {
		if created {
			// Only a branch this call made is removed, and only at the
			// commit this call put it at: DeleteBranch compares and swaps,
			// so a branch something else moved in between is left alone.
			if undo := p.repo.DeleteBranch(ctx, out.Branch, out.Base); undo != nil {
				return errors.Join(err, fmt.Errorf("belay also could not remove the branch %s it had just created: %w",
					out.Branch, undo))
			}
		}
		return err
	}
	return nil
}

// claimBranch makes the run's branch, and reports whether this call made it.
// A branch already at the run's base is adopted: createRun converges when it
// is called again for the same run id, and its own crash is the reason it has
// to.
func (p *runPlan) claimBranch(ctx context.Context, out state.Output) (bool, error) {
	switch tip, ok, err := p.repo.BranchTip(ctx, out.Branch); {
	case err != nil:
		return false, fmt.Errorf("cannot look for the branch %s: %w", out.Branch, err)
	case ok && tip == out.Base:
		return false, nil
	case ok:
		return false, fmt.Errorf("the branch %s already exists, at %s rather than this run's base %s; "+
			"belay will not move a branch that may hold somebody's work",
			out.Branch, shortCommit(tip), shortCommit(out.Base))
	}
	if err := p.repo.CreateBranch(ctx, out.Branch, out.Base); err != nil {
		return false, fmt.Errorf("cannot create the branch %s at %s: %w", out.Branch, shortCommit(out.Base), err)
	}
	return true, nil
}

// claimWorkTree makes the worktree the run's nodes work in, adopting one
// already checked out on the run's branch.
func (p *runPlan) claimWorkTree(ctx context.Context, out state.Output) error {
	list, err := p.repo.Worktrees(ctx)
	if err != nil {
		return fmt.Errorf("cannot list this repository's worktrees: %w", err)
	}
	for _, w := range list {
		if w.Path != out.WorkTree {
			continue
		}
		if w.Branch != out.Branch {
			return fmt.Errorf("%s is already a worktree on %s, not on this run's branch %s",
				out.WorkTree, orDetached(w.Branch), out.Branch)
		}
		return nil
	}
	if err := p.repo.AddWorktree(ctx, out.WorkTree, out.Branch); err != nil {
		return fmt.Errorf("cannot create the run's worktree at %s: %w", out.WorkTree, err)
	}
	return nil
}

// unwindWorkspace removes what prepareWorkspace made, for the one failure
// createRun can see: the manifest write, which is the last thing it does.
//
// A run directory with no manifest is already understood — nothing takes it
// for a run, and `belay runs` reports it — but a branch is not. It lives in
// the repository the person works in, outside any run directory, and with no
// manifest naming it nothing would ever connect it back to the run that made
// it. A crash here cannot be undone by anybody; docs/troubleshooting.md says
// how to find such branches by hand.
//
// It reports what it could not remove rather than failing: createRun is
// already returning an error, and replacing it with a cleanup error would
// hide the reason the run did not start.
func (p *runPlan) unwindWorkspace(ctx context.Context, out state.Output) {
	if !out.Branched() || p.repo == nil {
		return
	}
	if err := p.repo.RemoveWorktree(ctx, out.WorkTree); err != nil {
		p.logger.Error("belay could not remove the worktree it had just created",
			slog.String("worktree", out.WorkTree), slog.Any("error", err))
		return
	}
	if err := p.repo.DeleteBranch(ctx, out.Branch, out.Base); err != nil {
		p.logger.Error("belay could not remove the branch it had just created",
			slog.String("branch", out.Branch), slog.Any("error", err))
	}
}

// orDetached names a worktree's branch, or says it has none.
func orDetached(branch string) string {
	if branch == "" {
		return "a detached HEAD"
	}
	return branch
}

// driveRun dispatches an existing run once, does what follows a dispatch,
// and reports how the run ended. `belay run` and `belay resume` differ only
// in the resuming flag, so the two commands cannot narrate the same events
// differently.
//
// Ctrl-C is handled from before the dispatch until the report is written, so
// a first Ctrl-C after the dispatch has returned still lets afterCompletion
// finish rather than killing belay part-way through saving the recording.
//
// A Ctrl-C during the dispatch pauses the run rather than ending it (see
// pauseOnCancel), which is what both commands' help promises.
func driveRun(cmd *cobra.Command, u *ui, plan *runPlan, store *state.Store, resuming bool) error {
	ctx, stop := stopOnSignal(cmd.Context(), u)
	defer stop()

	for {
		view, err := dispatchOnce(ctx, plan, store, resuming, pauseOnCancel)
		if err != nil {
			return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
		}
		// A run that stopped at the gate is offered to a person at a
		// terminal, who may approve it or ask for a different plan; the run
		// is then dispatched again, from the state their decision was
		// recorded in. Every other invocation — no terminal, CI, --no-input,
		// a decision to leave — falls straight through, exactly as belay has
		// always behaved. A Ctrl-C ends it too: the person asked to stop.
		if ctx.Err() != nil || !reviewPause(cmd, u, plan, store, view) {
			// The context here is the run's, which a Ctrl-C has already
			// cancelled by the time a paused run reaches this.
			// afterCompletion's steps must still run, so they are given the
			// command's context rather than that one.
			afterCompletion(cmd.Context(), u, plan, &view)
			return reportOutcome(u, view)
		}
		resuming = true
	}
}

// dispatchOnce builds the dispatcher for an existing run and dispatches it
// once: Resume when resuming, Run otherwise.
//
// Each mut changes the dispatcher's Options, in order, after runPlan.options
// has built them. It is for what a caller decides about this dispatch rather
// than about the run's wiring: driveRun passes pauseOnCancel, and a caller
// that dispatches a run differently, such as one that stops after a single
// node, passes its own. runPlan.options stays the one place the wiring every
// run shares is built, and a mut only departs from it.
//
// The error is not how the run went. It is set only when no dispatcher could
// be built, so nothing ran and there is nothing to report. How the run went,
// a failed run included, is in the returned outcomeView.
//
// dispatchOnce neither saves nor says anything once the dispatch returns:
// afterCompletion and reportOutcome are the caller's to run, so a caller can
// dispatch a run more than once, through a pause and on, and report once.
func dispatchOnce(ctx context.Context, plan *runPlan, store *state.Store, resuming bool,
	mut ...func(*graph.Options)) (outcomeView, error) {
	opts := plan.options(store)
	for _, change := range mut {
		change(&opts)
	}
	dispatcher, err := graph.NewDispatcher(opts)
	if err != nil {
		return outcomeView{}, err
	}

	// Run and Resume differ in exactly one thing: whether a run parked at
	// an approval gate may be stepped past. Calling both would step past a
	// gate that a plain `belay run` had just been told to respect.
	dispatch := dispatcher.Run
	if resuming {
		dispatch = dispatcher.Resume
	}
	outcome, runErr := dispatch(ctx)
	view := outcomeView{
		layout: store.Layout(), outcome: outcome, runErr: runErr, output: plan.output,
		fanout: plan.fanout != nil,
	}
	view.winner = winnerOf(store, view.fanout)
	return view, nil
}

// winnerOf reads the candidate a fanout run's join selected from the run's
// blackboard, after the dispatch has saved it. It is the zero Candidate for a
// run that does not fan out, one with no winner yet, or a blackboard it
// cannot read: what to do next is reported either way, and a report must not
// fail over a read that only makes it more specific.
func winnerOf(store *state.Store, fanned bool) state.Candidate {
	if !fanned {
		return state.Candidate{}
	}
	st, err := store.LoadState()
	if err != nil || st.Winner == "" {
		return state.Candidate{}
	}
	for _, c := range st.Candidates {
		if c.ID == st.Winner {
			return c
		}
	}
	return state.Candidate{ID: st.Winner}
}

// pauseOnCancel is the change driveRun makes to the dispatcher's Options:
// when the person running belay presses Ctrl-C, or the process is sent
// SIGTERM, the run is paused where it stands and `belay resume` continues it,
// re-running the step that was interrupted. Without it the dispatcher ends
// the run as aborted, and resume refuses an aborted run, although belay run's
// help says Ctrl-C leaves a run resume picks up.
//
// It is passed to dispatchOnce rather than built into runPlan.options because
// it answers how this invocation stops, not how the run is wired. driveRun
// owns the signal handling, so it owns what a signal does to the run. A
// caller that dispatches runs no person is watching chooses for itself.
func pauseOnCancel(o *graph.Options) { o.PauseOnCancel = true }

// afterCompletion does what belay does once a dispatch has returned, before
// it reports how the run went. Its steps run in this order, every time:
//
//  1. Save the recording, when agent.mode is "record". This happens whatever
//     the outcome: a run that failed or was stopped still made the calls it
//     recorded.
//  2. Move a completed run's plan out of the run directory (archivePlan). It
//     goes after the recording because the recording exists only in memory
//     until it is saved, and a crash here must not cost it, while the plan is
//     whole on disk in at least one place for the whole of its move. It goes
//     before the pull request because it is quick, local and durable, and a
//     push that hangs or fails must not hold it up or skip it; the push does
//     not need it, because a branch run's plan is already in the commit being
//     pushed. And it goes before the worktree is tidied because in a branch
//     run it confirms the plan against the worktree promote committed it
//     from, which step 4 removes: run after that, it would find no plan and
//     leave the run's copy behind.
//  3. Push the branch and open the draft pull request --pr asked for. It
//     goes before the worktree is tidied only because the two are
//     independent and this is the one a person is waiting on.
//  4. Tidy a finished branch run's worktree. It goes after the recording
//     because saving the recording must not depend on a directory this step
//     removes, and it reads how the run ended: a paused or failed run's
//     worktree is where the person finishes, so only a completed run's goes.
//
// A new step goes into this list at the position its inputs require, with
// the reason for that position beside it.
//
// No step can change the process's exit status, and the signature is what
// guarantees it: afterCompletion returns nothing, and reportOutcome derives
// the status from view, which afterCompletion is given a copy of. A step that
// fails says so on stderr, and the steps after it still run. A run that
// finished has finished whether or not a step after it worked; reporting it
// as failed would tell a script to redo work that is already done.
func afterCompletion(ctx context.Context, u *ui, plan *runPlan, view *outcomeView) {
	if path, err := plan.saveCassette(); err != nil {
		u.note("belay: %v", err)
	} else if path != "" {
		u.say("  recording saved to %s", path)
	}
	archivePlan(u, *view)
	plan.publishRun(ctx, u, view)
	plan.finishRun(ctx, *view)
}

// stopOnSignal cancels ctx when the person running belay asks it to stop, and
// says on screen that stopping is safe.
//
// The first SIGINT or SIGTERM cancels the run's context: the dispatcher
// finishes the durable write it is in the middle of, pauses the run and
// returns, so the run stops with its state intact and `belay resume` picks it
// up (see pauseOnCancel). If the second signal kills belay before that, the
// run is left as a crash leaves it, which resume also continues. signal.Stop then hands the
// next signal back to Go's default handler, which ends the process at once —
// somebody holding Ctrl-C must never be left wondering whether belay is
// ignoring them.
func stopOnSignal(parent context.Context, u *ui) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ch:
			signal.Stop(ch)
			u.say("")
			u.say("Stopping. belay saves its state after every step, so nothing is lost.")
			u.say("Press Ctrl-C again to quit immediately.")
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx, func() {
		signal.Stop(ch)
		cancel()
		<-done
	}
}

// announceStart prints what a person needs before belay spends anything: what
// it was asked for, what it may cost, how to stop it, and where its state
// lives if they do.
func announceStart(u *ui, plan *runPlan, layout state.Layout, want string) {
	u.say("belay is starting a run in %s", plan.workspace)
	u.say("  what you asked for:  %s", want)
	u.say("  where it goes:       %s", plan.outputReason)
	u.say("  most it can spend:   %s", u.emph(budgetCeiling(plan.config.Budget)))
	u.say("  when it gets there:  %s", budgetOnExceed(plan.config.Budget))
	if plan.fanout != nil {
		u.say("  candidates:          %d, %s", plan.fanout.Candidates, candidateShare(plan))
	}
	u.say("  to stop it:          press Ctrl-C once; belay saves its state and exits")
	u.say("")
	u.say("run %s", layout.RunID())
	u.say("  state:  %s", layout.RunDir())
	if where := plan.planGoesTo(layout.RunID(), want); where != "" {
		u.say("  plan:   %s", where)
	}
	if plan.output.Branched() {
		u.say("  branch: %s, from %s", plan.output.Branch, shortCommit(plan.output.Base))
		u.say("  tree:   %s", plan.output.WorkTree)
	}
	u.say("  route:  %s", routeLine(plan.route))
	u.say("")
}

// outcomeView is everything belay's closing report reads about how one
// dispatch of a run ended: which run it was, what the dispatcher returned,
// and the error it gave as the reason a run stopped short.
//
// It is one value rather than a list of parameters so that the report can
// learn something new about a run, such as where the run's work went, by
// gaining a field, instead of by changing the signature of every function
// that reads it.
type outcomeView struct {
	// layout locates the run's files. The report names the plan, the
	// journal and the command that continues the run.
	layout state.Layout
	// outcome is what the dispatcher returned.
	outcome graph.Outcome
	// runErr is the dispatcher's error. It is part of how the run went, such
	// as graph.ErrMaxSteps, not a failure to find out.
	runErr error
	// pullRequest is the URL of the draft pull request a --pr run opened,
	// and publishErr is why it could not be opened. At most one is set.
	pullRequest string
	publishErr  error
	// remote is where the branch was to be pushed, so a report that has to
	// name the push command by hand names the right remote.
	remote string
	// output is how the run delivers its result, as its manifest pinned it.
	// The zero Output is an in-place run, and the report then says nothing
	// about branches.
	output state.Output
	// fanout is whether the run fans out, as its manifest pinned it.
	fanout bool
	// winner is the candidate join selected, read from the blackboard once
	// the dispatch returned. It is the zero Candidate for a run with no
	// winner, or one that does not fan out.
	winner state.Candidate
}

// reportOutcome turns an Outcome into the last thing a person reads, and into the
// process's exit status.
//
// Every branch answers the same three questions in the same order: where the
// run is, what it cost, and what to do next. A person who has read one of
// these has read all of them.
func reportOutcome(u *ui, view outcomeView) error {
	outcome, runErr := view.outcome, view.runErr
	u.say("")
	u.say("%s", outcomeHeadline(outcome, runErr))
	u.say("  where:  %s", whereText(outcome.Node))
	u.say("  cost:   %s", u.emph(spendText(outcome)))
	// The label sits on the first line only: repeating "next:" down a
	// paragraph turns one instruction into four, which is exactly the
	// reading a person then has to undo.
	for i, line := range nextSteps(view) {
		switch {
		case line == "":
			u.say("")
		case i == 0:
			u.say("  next:   %s", line)
		default:
			u.say("          %s", line)
		}
	}
	if note := strings.TrimSpace(outcome.Note); note != "" && u.verbose {
		u.say("  note:   %s", note)
	}

	if outcome.ExitCode == graph.ExitOK {
		return nil
	}
	return &ExitError{Code: outcome.ExitCode, Err: runErr}
}

// outcomeHeadline states, in one sentence, how this invocation ended.
func outcomeHeadline(outcome graph.Outcome, runErr error) string {
	steps := "step"
	if outcome.Steps != 1 {
		steps = "steps"
	}
	did := fmt.Sprintf("after %d %s", outcome.Steps, steps)

	switch {
	case errors.Is(runErr, graph.ErrRunPaused):
		return "This run is waiting for you, so belay did nothing."
	case errors.Is(runErr, graph.ErrRunEnded):
		return "This run has already ended, so belay did nothing."
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, graph.ErrNodeAborted) && outcome.Steps == 0:
		return "belay stopped because you asked it to. Everything it had done is saved."
	case errors.Is(runErr, graph.ErrMaxSteps), errors.Is(runErr, graph.ErrNodeTimeout),
		errors.Is(runErr, belay.ErrBudgetExceeded):
		// One of belay's own limits. The run is paused rather than ended,
		// and what follows says which setting to raise, so this must not
		// read as "waiting for you" the way the approval gate does.
		return fmt.Sprintf("belay stopped itself %s. Everything it did is saved.", did)
	}

	switch outcome.Status {
	case state.RunStatusCompleted:
		return fmt.Sprintf("belay finished %s.", did)
	case state.RunStatusPaused:
		return fmt.Sprintf("belay paused %s, and is waiting for you.", did)
	case state.RunStatusFailed:
		return fmt.Sprintf("belay could not finish; it stopped %s. Everything it did is saved.", did)
	case state.RunStatusAborted:
		return fmt.Sprintf("belay stopped itself %s. Everything it did is saved.", did)
	default:
		return fmt.Sprintf("belay stopped %s.", did)
	}
}

// whereText explains a node name rather than printing it bare. A person who has
// never read belay's source has no way to know what "approve" means, and a
// message they cannot act on is worse than no message.
func whereText(node string) string {
	switch node {
	case "":
		return "belay had not reached a step yet"
	case graph.End:
		return "the end of the graph — there is nothing left to do"
	default:
		if meaning, ok := nodeMeaning[node]; ok {
			return node + " — " + meaning
		}
		return node
	}
}

// nodeMeaning says what each step of the graph is, in one clause.
var nodeMeaning = map[string]string{
	graph.NodePlan:    "writing the plan",
	graph.NodeApprove: "waiting for you to read the plan and approve it",
	graph.NodeCode:    "asking the agent to write the change",
	graph.NodeWrite:   "checking the files the agent reported changing, and recording them",
	graph.NodeTest:    "running your tests",
	graph.NodeFix:     "asking the agent to repair what failed",
	graph.NodeReview:  "running the quality gate over the result",
}

// spendText renders what the run has cost so far against what it was allowed.
func spendText(outcome graph.Outcome) string {
	used := dollars(outcome.Usage.USD)
	if outcome.Usage.Estimated {
		used += " (estimated)"
	}
	tokens := outcome.Usage.InputTokens + outcome.Usage.OutputTokens
	if tokens == 0 {
		return used
	}
	return fmt.Sprintf("%s, %s tokens", used, thousands(tokens))
}

// nextSteps is the wayfinding line: the exact command that continues from
// here, written out so nobody has to remember or reconstruct it.
func nextSteps(view outcomeView) []string {
	layout, outcome, runErr := view.layout, view.outcome, view.runErr
	resume := "belay resume " + layout.RunID()

	switch {
	case errors.Is(runErr, graph.ErrInterrupted):
		// Paused, but by Ctrl-C and not by the graph: nothing is waiting
		// for the person to read, and resume re-runs the interrupted step.
		return []string{"pick up where it stopped with:", "", "    " + resume}
	// The three limits belay stops itself at come before the general paused
	// case, which they would otherwise fall into and be told to read a plan
	// nobody is waiting on. Each one is a setting the person chose and can
	// change, and the run is paused, not ended: raising the limit and
	// resuming continues this run, keeping everything it has already spent.
	// Limits are re-read on the resume, which is why that works.
	case errors.Is(runErr, graph.ErrMaxSteps):
		return []string{
			"the graph kept going round without finishing, so belay stopped it.",
			"raise graph.max_steps in belay.yaml, then pick this run up again:",
			"", "    " + resume,
		}
	case errors.Is(runErr, graph.ErrNodeTimeout):
		return []string{
			"one step ran longer than graph.node_timeout allows.",
			"raise graph.node_timeout in belay.yaml, then pick this run up again:",
			"", "    " + resume,
		}
	case errors.Is(runErr, belay.ErrBudgetExceeded):
		// Crossing either ceiling stops a run with the same error, which
		// does not say which one, so both settings are named.
		return []string{
			"the run reached its budget ceiling, so belay stopped it.",
			"raise budget.max_usd or budget.max_tokens in belay.yaml, whichever it reached, then pick this run up again:",
			"", "    " + resume,
		}
	case errors.Is(runErr, graph.ErrRunPaused), outcome.Status == state.RunStatusPaused:
		// A run paused at promote is not waiting for a plan to be read: two
		// lines of work disagree, and only a person can say what to keep.
		// Telling them to read a plan nobody is waiting on sends them to the
		// wrong file and leaves the actual work unnamed.
		if outcome.Node == graph.NodePromote {
			return promoteSteps(view, resume)
		}
		// Nor is a run paused at the fanout: some candidates could not be
		// finished for a reason a resume can fix, and the fanout's own note,
		// printed above, says which.
		if outcome.Node == graph.NodeFanout {
			return fanoutPausedSteps(view, resume)
		}
		lines := []string{"read the plan, edit it if you want to, then run:", "", "    " + resume}
		if outcome.Node == graph.NodeApprove {
			plan := filepath.Join(layout.ArtifactsDir(), "plan.md")
			lines = append([]string{"the plan is at " + plan}, lines...)
		}
		return lines
	case errors.Is(runErr, graph.ErrNoResumePoint):
		return []string{
			"belay cannot tell where this run left off, so it will not guess.",
			"start a fresh run; the journal of this one is at " + layout.JournalPath(),
		}
	// A run that ended as aborted cannot be resumed: belay resume refuses
	// it. Only a run that is paused or was interrupted is offered the
	// command, so the report never names one that would refuse.
	case outcome.Status == state.RunStatusCompleted:
		// A branch run has already committed, on a branch the person's
		// checkout is not on. "Review the change in your repository, then
		// commit it" would name a change that is not there and a commit
		// belay already made.
		if view.output.Branched() {
			return completedBranchSteps(view)
		}
		// An in-place run that fanned out changed nothing in the person's
		// checkout either: its winner worked in a workspace of its own.
		if view.fanout {
			return completedFanoutSteps(view)
		}
		return []string{"review the change in your repository, then commit it"}
	case outcome.Status == state.RunStatusAborted:
		return []string{
			"this run has ended. see what happened with:", "", "    belay timeline " + layout.RunID(),
		}
	case outcome.Status == state.RunStatusFailed:
		lines := []string{
			"see what happened with:", "", "    belay timeline " + layout.RunID(),
		}
		// A fanout fails when no candidate could win, and the reasons are
		// in the candidates' own runs.
		if view.fanout {
			lines = append(lines,
				"", "and open any candidate's own run, such as the first, with:",
				"", "    belay timeline "+layout.RunID()+" --candidate cand-01")
		}
		return lines
	default:
		return []string{"continue with:", "", "    " + resume}
	}
}

// promoteSteps is what to do about a run paused at promote: the worktree the
// disagreement is in, and the command that picks the run back up once it is
// settled. The node's own Note names the conflicting files and the branch to
// merge, and reportOutcome prints it, so this does not repeat them.
func promoteSteps(view outcomeView, resume string) []string {
	out := view.output
	if !out.Branched() || out.WorkTree == "" {
		return []string{"belay could not finish putting this run's change on its branch.", "pick it up again with:", "", "    " + resume}
	}
	return []string{
		"belay could not finish putting this run's change on " + out.Branch + " on its own.",
		"settle it in the run's own worktree, then resume:",
		"",
		"    cd " + out.WorkTree,
		"    git status",
		"    " + resume,
	}
}

// fanoutPausedSteps is what to do about a run paused at its fanout: some
// candidates have no durable end state because of something a resume can
// fix -- a full disk, a git lock, a deadline -- and the fanout node's note,
// which reportOutcome prints, names them and the reason. The finished
// candidates are not run again: the runner adopts them from their own
// journals, which is the property the whole candidate-run design rests on,
// and a person deciding whether resuming will cost them twice is owed it.
func fanoutPausedSteps(view outcomeView, resume string) []string {
	return []string{
		"some candidates could not be finished; the note above says which, and why.",
		"fix that, then resume: the ones that finished are kept, and resuming runs only the others:",
		"",
		"    " + resume,
		"",
		"see every candidate so far with:",
		"",
		"    belay timeline " + view.layout.RunID(),
	}
}

// completedFanoutSteps is what to do with a finished in-place run that fanned
// out. Its winner's change is in the winner's own workspace, not in the
// person's checkout: belay does not merge a run that edits in place, and
// join's note says so. So this names the workspace, and the command that
// shows what the winner did there.
func completedFanoutSteps(view outcomeView) []string {
	w := view.winner
	if w.ID == "" {
		// Completed with no recorded winner cannot come from join, which
		// fails a run with none; the blackboard could not be read. Say
		// where to look rather than guess.
		return []string{"see which candidate won, and where its change is, with:", "",
			"    belay timeline " + view.layout.RunID()}
	}
	return []string{
		"the winner is " + w.ID + ". belay does not merge a run that edits in place, so its change is",
		"in its own workspace, not in your checkout:",
		"",
		"    " + dirOrUnrecorded(w.Dir),
		"",
		"review it there and copy back what you want to keep. what it did is in:",
		"",
		"    belay timeline " + view.layout.RunID() + " --candidate " + w.ID,
	}
}

// dirOrUnrecorded names a winner's workspace, or says the blackboard holds
// none, so a person is never told to look in "".
func dirOrUnrecorded(dir string) string {
	if dir == "" {
		return "(its workspace is not recorded; belay timeline shows the candidate's run)"
	}
	return dir
}

// completedBranchSteps is what to do with a finished branch run: read what
// belay committed, and take it, in the two commands that do each.
//
// A run that opened a pull request is told about that instead of about
// merging by hand, and a run that asked for one and could not get it is given
// the two commands that finish the job — because the change is committed
// either way, and the only thing missing is one push and one gh call.
func completedBranchSteps(view outcomeView) []string {
	out := view.output
	opening := "belay committed its change to " + out.Branch + ", and your checkout was never touched."
	if view.pullRequest != "" {
		return []string{opening, "it is proposed as a draft pull request:", "", "    " + view.pullRequest}
	}
	if view.publishErr != nil {
		return []string{
			opening,
			"belay could not open the pull request; finish it yourself with:",
			"",
			"    git push " + orOrigin(view.remote) + " " + out.Branch,
			"    gh pr create --draft --head " + out.Branch,
		}
	}
	lines := []string{
		opening,
		"read it, then take it:",
		"",
		"    git log -p " + baseRange(out),
		"    git switch " + out.Branch,
	}
	if out.BaseRef != "" {
		lines = append(lines, "    git merge "+out.Branch+"   # from "+out.BaseRef)
	}
	return lines
}

// orOrigin names the remote a run would have pushed to, filling in the name
// every clone gets when belay never got as far as resolving one.
func orOrigin(remote string) string {
	if remote == "" {
		return "origin"
	}
	return remote
}

// baseRange is the revision range holding exactly what a run added, written
// the way a person types it.
func baseRange(out state.Output) string {
	if out.Base == "" {
		return out.Branch
	}
	return shortCommit(out.Base) + ".." + out.Branch
}

// refuseToStart reports every reason belay will not start, together, and stops.
func refuseToStart(u *ui, plan *runPlan) error {
	u.say("belay will not start this run.")
	for _, b := range plan.blockers {
		u.say("")
		u.say("  %s", b.problem)
		u.say("  to fix it: %s", b.fix)
	}
	u.say("")
	u.say("Nothing was created and nothing was spent.")
	return &ExitError{Code: graph.ExitFailed}
}

// describeDryRun prints the whole of what a run would do and then stops,
// having created nothing and spent nothing.
//
// It reads from the same resolved plan a real run would use, so it cannot
// describe a run different from the one that would happen. resuming, when not
// empty, names the run this would have continued.
func describeDryRun(u *ui, plan *runPlan, want, resuming string) error {
	u.say("This is a dry run. belay is only describing the work; none of it has happened.")
	u.say("")

	for _, section := range dryRunSections(u, plan, want, resuming) {
		u.say("%s", section.title)
		for _, line := range section.lines {
			u.say("  %s", line)
		}
		u.say("")
	}

	if len(plan.blockers) > 0 {
		u.say("belay would refuse to start this run.")
		for _, b := range plan.blockers {
			u.say("")
			u.say("  %s", b.problem)
			u.say("  to fix it: %s", b.fix)
		}
		u.say("")
		u.say("Nothing was created and nothing was spent.")
		return &ExitError{Code: graph.ExitFailed}
	}

	u.say("Nothing was created and nothing was spent. Run the same command without --dry-run to start.")
	return nil
}

// dryRunSection is one titled block of a dry run: a heading, then its lines,
// each indented under it.
type dryRunSection struct {
	title string
	lines []string
}

// dryRunSections lists what a dry run describes, in the order it is printed.
//
// Each section is built by its own function, and a section that has nothing
// to say is left out rather than printed empty. Something new a run would do
// gets a section of its own in this list, or a line in the section it belongs
// to, instead of more statements in describeDryRun.
func dryRunSections(u *ui, plan *runPlan, want, resuming string) []dryRunSection {
	var sections []dryRunSection
	if want != "" {
		sections = append(sections, dryRunSection{title: "what you asked for", lines: []string{want}})
	}
	if resuming != "" {
		sections = append(sections, dryRunSection{title: "which run it would continue", lines: []string{resuming}})
	}
	sections = append(sections,
		dryRunSection{title: "where belay would work", lines: []string{plan.workspace, projectLine(plan.projects)}},
		dryRunOutput(plan),
		dryRunSpend(u, plan),
	)
	// A run that does not fan out gets no section: one attempt is what every
	// other section already describes.
	if candidates := dryRunFanout(plan, resuming != ""); len(candidates.lines) > 0 {
		sections = append(sections, candidates)
	}
	// A run whose graph could not be assembled has no nodes to price, and an
	// empty section is a heading over nothing.
	if models := dryRunModels(plan); len(models.lines) > 0 {
		sections = append(sections, models)
	}
	sections = append(sections, dryRunSettings(plan))
	// A run with no policy gets no section rather than a heading over
	// "none": the absence is the whole answer, and a heading claiming it
	// would be one more thing to read on every dry run of every repository
	// that has never declared a rule.
	if guardrails := dryRunPolicy(plan); len(guardrails.lines) > 0 {
		sections = append(sections, guardrails)
	}
	sections = append(sections,
		dryRunRoute(plan),
		dryRunCreates(plan, want, resuming != ""),
	)
	for _, w := range plan.warnings {
		sections = append(sections, dryRunSection{title: "worth knowing", lines: []string{w}})
	}
	return sections
}

// dryRunOutput is where a run's result would go, and why.
//
// It is the most consequential thing in the whole description — whether belay
// edits the person's own files or works somewhere else entirely — so it is
// said right after where belay would work, not buried among the settings. The
// reason is printed with the answer because "auto" chose it, and a choice
// nobody was told the reason for reads as belay ignoring the setting.
func dryRunOutput(plan *runPlan) dryRunSection {
	lines := []string{plan.outputReason}
	if plan.output.Branched() {
		lines = append(lines,
			"branch       "+branchPrefix+"<run-id>, from "+shortCommit(plan.output.Base)+" on "+orDetached(plan.output.BaseRef),
			"worktree     "+filepath.Join(plan.workspace, ".belay", "runs", "<run-id>", "worktree"),
			"it commits once, after the quality gate passes, and never edits your checkout",
		)
	}
	return dryRunSection{title: "where its result would go", lines: lines}
}

// dryRunSpend is the ceiling a run is held to, and what reaching it does.
func dryRunSpend(u *ui, plan *runPlan) dryRunSection {
	return dryRunSection{title: "the most it could spend", lines: []string{
		u.emph(budgetCeiling(plan.config.Budget)),
		budgetOnExceed(plan.config.Budget),
	}}
}

// dryRunFanout is how a fanout run divides its work and its money, and it
// is empty for a run that does not fan out.
//
// It follows the discipline dryRunModels states: a share and never a total.
// belay knows what each candidate may spend, because the fanout divides the
// ceiling once, evenly, before any candidate runs; it does not know what
// each will spend, so it says nothing about that. It also says the one thing
// a person reading budget.max_usd as a hard cap would get wrong -- that a
// candidate is checked before a step and not during one -- so the run's
// final bill can pass the ceiling by a bounded amount, and the bound is
// named.
func dryRunFanout(plan *runPlan, resuming bool) dryRunSection {
	if plan.fanout == nil {
		return dryRunSection{}
	}
	n := plan.fanout.Candidates
	share := fmt.Sprintf("%d candidates, %s", n, candidateShare(plan))
	if resuming {
		share += "; a candidate already claimed keeps the share it was given, or more if budget.max_usd " +
			"has been raised since"
	}
	return dryRunSection{title: "its candidates", lines: []string{
		share,
		"budget.max_usd, less what the plan has cost, is divided evenly between them once, before any of them runs",
		"each walks " + routeLine(plan.candidateRoute) + ", in a workspace of its own",
		winnerLine(plan.fanout.Select),
		"a candidate is checked before each step and not during one, so the run can end up to one step " +
			"per candidate over budget.max_usd (two, if every candidate is stopped part-way through a step " +
			"and resumed)",
	}}
}

// candidateShare is how many of a fanout run's candidates run at a time, and
// the most each may spend: "3 at a time, up to $1.00 and 666,666 tokens
// each".
//
// The share is the one the fanout node computes, from the same function,
// with nothing spent yet. The real fanout runs after the plan has been paid
// for, so each real share is a little smaller, which is why this says "up
// to". How many run at a time is fanout.concurrency, never more than there
// are candidates, read from belay.yaml as it is now.
func candidateShare(plan *runPlan) string {
	n := plan.fanout.Candidates
	together := min(max(plan.config.Fanout.Concurrency, 1), max(n, 1))
	atATime := fmt.Sprintf("%d at a time", together)
	if together == 1 {
		atATime = "one at a time"
	}
	env, err := fanout.Envelopes(budget.Snapshot{}, plan.config.Budget, n)
	if err != nil {
		return atATime + ", and belay cannot divide budget.max_usd between them: " + err.Error()
	}
	each := "up to " + dollars(env.LimitUSD)
	if env.LimitTokens > 0 {
		each += " and " + thousands(env.LimitTokens) + " tokens"
	}
	return atATime + ", " + each + " each"
}

// winnerLine says how join will choose, in the words of the strategy the run
// is pinned to. A strategy this belay does not know is named as it is
// written: join refuses it, in one place, and this only describes.
func winnerLine(strategy string) string {
	const among = ", among those that passed their tests"
	switch config.SelectStrategy(strategy) {
	case config.SelectFewestIssues:
		return "the winner is the candidate with the fewest review issues" + among
	case config.SelectFirstPass:
		return "the winner is the first candidate, in order, that passed its tests"
	case config.SelectFastest:
		return "the winner is the candidate that finished in the least time" + among
	default:
		return fmt.Sprintf("the winner is chosen by %q", strategy)
	}
}

// dryRunModels is what each step that calls a model would be sent, and what
// that model lists at.
//
// It states rates and never a total, which is the whole discipline of this
// section. belay cannot know how many tokens a goal will take -- that is what
// the run is for -- so any dollar figure it printed here would be a guess
// dressed as an estimate, and a person would budget against it. A rate is
// something belay actually knows, and the ceiling above says where the run
// stops regardless.
//
// One row per node, including the nodes sent agent.model, because "which
// model runs this step" is the question, and answering it only for the nodes
// somebody overrode would make routing look like it applies to some steps and
// not others. The rows line up so the models can be compared down the column,
// which is how a person sees that the expensive one is on the step they
// meant.
func dryRunModels(plan *runPlan) dryRunSection {
	if len(plan.models) == 0 {
		return dryRunSection{}
	}
	var nodeWidth, modelWidth int
	for _, m := range plan.models {
		nodeWidth = max(nodeWidth, len(m.node))
		modelWidth = max(modelWidth, len(m.model))
	}

	var (
		lines  []string
		priced bool
	)
	for _, m := range plan.models {
		row := fmt.Sprintf("%-*s %-*s", nodeWidth, m.node, modelWidth, m.model)
		switch {
		case m.priced:
			priced = true
			row += fmt.Sprintf(" %s in / %s out per million tokens", dollars(m.rate.Input), dollars(m.rate.Output))
		case m.note != "":
			row += " " + m.note
		}
		lines = append(lines, strings.TrimRight(row, " "))
	}
	switch {
	case priced:
		lines = append(lines, "list rates: belay cannot know how many tokens your goal will take, so it says "+
			"the rate and not a total")
	case plan.modelNote != "":
		lines = append(lines, plan.modelNote)
	}
	return dryRunSection{title: "what each step would cost", lines: lines}
}

// dryRunPolicy is the guardrails a run's change would be held to.
//
// It is the setting a person would most want confirmed before leaving belay
// running unattended, and the one they cannot confirm any other way: a policy
// that loads but never fires looks exactly like one that works, right up to
// the moment something gets through. So every rule is printed, in the words
// the person wrote it in, rather than summarised as a count.
//
// Each line says which kind of violation the rule is, because that is the
// difference that decides what an unattended run does about it: ending the
// run brings a person back, and a repair does not. The words match the ones
// docs/config.md uses for the same split.
//
// It reads the pin, not p.config.Policy, so a resumed run is described by the
// rules it is really held to. That is the same rule the route and the output
// section follow, and it is what stops a dry run of a resume from showing
// guardrails the run does not have.
func dryRunPolicy(plan *runPlan) dryRunSection {
	if plan.policy == nil {
		return dryRunSection{}
	}
	rules := plan.policy.Policy
	var lines []string
	add := func(ends bool, format string, a ...any) {
		kind := "asks for a repair"
		if ends {
			kind = "ends the run"
		}
		lines = append(lines, fmt.Sprintf(format, a...)+"  ("+kind+")")
	}
	for _, pattern := range rules.ForbiddenPaths {
		add(true, "must not touch      %s", pattern)
	}
	if rules.MaxChangedFiles > 0 {
		add(true, "at most             %d changed files", rules.MaxChangedFiles)
	}
	if rules.MaxChangedLines > 0 {
		add(true, "at most             %d lines across the files it touches", rules.MaxChangedLines)
	}
	if rules.NoNewDependencies {
		add(false, "no new dependency   beyond what this workspace declares now")
	}
	for _, rule := range rules.RequiredTests {
		add(false, "a change to         %s must bring a change to %s", rule.When, rule.Require)
	}
	for _, rule := range rules.ForbiddenContent {
		where := "any changed file"
		if len(rule.Paths) > 0 {
			where = strings.Join(rule.Paths, ", ")
		}
		line := fmt.Sprintf("must not contain    %s  in %s", rule.Pattern, where)
		if reason := strings.TrimSpace(rule.Reason); reason != "" {
			line += " (" + reason + ")"
		}
		add(false, "%s", line)
	}
	lines = append(lines, "belay records these rules and this workspace's dependencies when the run starts, and "+
		"holds the run to them even if belay.yaml changes")
	return dryRunSection{title: "the guardrails its change must obey", lines: lines}
}

// dryRunSettings is where the settings came from and the tools they chose.
func dryRunSettings(plan *runPlan) dryRunSection {
	agent := plan.config.Agent
	return dryRunSection{title: "settings it would use", lines: []string{
		"read from    " + sourcesLine(plan.configSources),
		fmt.Sprintf("agent        %s, model %s, %s, up to %d turns per step",
			agent.Backend, agent.Model, agentModeLine(agent.Mode, plan.cassettePath), agent.MaxTurns),
		"tests        " + plan.runnerName(),
		fmt.Sprintf("quality gate %s, failing the run on %s issues or worse", plan.gateName(), plan.config.Review.FailOn),
		"credentials  " + credentialsLine(),
	}}
}

// dryRunRoute is the walk through the graph, and what bounds it.
func dryRunRoute(plan *runPlan) dryRunSection {
	work := graph.NodeCode
	limits := fmt.Sprintf("it gives up after %d rounds of the test-and-fix loop, or %d steps in all",
		plan.config.Graph.GiveUp, plan.config.Graph.MaxSteps)
	// A fanout run has no test-and-fix loop of its own: each candidate has
	// one, and each candidate run is held to these limits separately.
	if plan.fanout != nil {
		work = graph.NodeFanout
		limits = fmt.Sprintf("each candidate gives up after %d rounds of its test-and-fix loop, or %d steps of its own",
			plan.config.Graph.GiveUp, plan.config.Graph.MaxSteps)
	}
	return dryRunSection{title: "the route it would take", lines: []string{
		routeLine(plan.route),
		approvalLine(plan.config.Graph, work),
		limits,
	}}
}

// dryRunCreates is what a real run would write, and where.
//
// Two of those writes land in the person's repository rather than in belay's
// run directory, so each is named when it would happen: .belay/.gitignore,
// which only the first run in a repository writes, and the plan, which a
// finished run moves out beside its code. A resume creates neither — the
// run already exists, and its plan goes where the run said when it started —
// so neither is listed for one.
func dryRunCreates(plan *runPlan, want string, resuming bool) dryRunSection {
	lines := []string{
		filepath.Join(plan.workspace, ".belay", "runs", "<run-id>"),
		"holding manifest.json, state.json, journal.ndjson and the plan",
	}
	if resuming {
		return dryRunSection{title: "what it would create", lines: lines}
	}
	gitignore := filepath.Join(plan.workspace, ".belay", ".gitignore")
	if _, err := os.Lstat(gitignore); errors.Is(err, fs.ErrNotExist) {
		lines = append(lines, gitignore+", so git never offers any of that for a commit")
	}
	if where := plan.planGoesTo("<run-id>", want); where != "" {
		lines = append(lines, "the plan, "+where)
	}
	return dryRunSection{title: "what it would create", lines: lines}
}

// planGoesTo says where a new run's plan ends up, or "" when plan.archive is
// off: moved into the workspace for a run in place, committed with the change
// for a run on a branch. It reads the settings NewManifest pins when the run
// is created, cleaned the way it cleans them, so the promise made before the
// run is the one the run keeps.
func (p *runPlan) planGoesTo(runID, goal string) string {
	if !p.config.Plan.Archive {
		return ""
	}
	rel := planarchive.Rel(path.Clean(p.config.Plan.ArchiveDir), runID, goal)
	if p.output.Branched() {
		return "committed with the change, at " + rel
	}
	return "moved to " + filepath.Join(p.workspace, filepath.FromSlash(rel)) + " when the run finishes"
}

// budgetCeiling renders the budget in the two units it is capped in.
func budgetCeiling(b config.Budget) string {
	return fmt.Sprintf("%s or %s tokens, whichever comes first", dollars(b.MaxUSD), thousands(b.MaxTokens))
}

// budgetOnExceed says what reaching the ceiling actually does, because "abort" and
// "warn" are the difference between a cap and a suggestion.
func budgetOnExceed(b config.Budget) string {
	if b.OnExceed == config.OnExceedWarn {
		return "belay records the overrun and keeps going — set budget.on_exceed to \"abort\" to make it a hard stop"
	}
	return "belay stops the run before the next step"
}

// routeLine is the walk through the graph, written out so a person can see
// where they are in it. route is what nodes.Route returned for this run.
func routeLine(route []string) string {
	return strings.Join(append(slices.Clone(route), "done"), " → ")
}

// approvalLine explains the one step that stops for a human. work is where
// the run goes once the plan is accepted: code, or the fanout.
func approvalLine(g config.Graph, work string) string {
	if g.Approval {
		return "approve stops the run so you can read the plan before any code is written"
	}
	return "graph.approval is false, so belay will not stop for you: it goes from plan straight to " + work
}

// projectLine names what belay recognised in the workspace.
func projectLine(projects []detect.Project) string {
	if len(projects) == 0 {
		return "belay recognises no Go, Node or Python project here"
	}
	// Count kinds rather than listing one entry per detected module. A
	// repository with nested modules -- belay's own, with two fixture
	// modules under it -- otherwise renders as "go, go, go", which repeats a
	// word three times and tells the reader nothing they can act on.
	counts := make(map[string]int, len(projects))
	order := make([]string, 0, len(projects))
	for _, p := range projects {
		k := p.Kind.String()
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	kinds := make([]string, 0, len(order))
	for _, k := range order {
		if n := counts[k]; n > 1 {
			kinds = append(kinds, fmt.Sprintf("%s (%d modules)", k, n))
			continue
		}
		kinds = append(kinds, k)
	}
	return "belay found: " + strings.Join(kinds, ", ")
}

// sourcesLine names the configuration files belay read.
func sourcesLine(sources []string) string {
	if len(sources) == 0 {
		return "belay's built-in defaults (no belay.yaml found)"
	}
	return strings.Join(sources, ", then ")
}

// agentModeLine says what agent.mode means in terms of what will happen.
func agentModeLine(mode config.AgentMode, cassette string) string {
	switch mode {
	case config.AgentModeReplay:
		return "replaying the recording at " + cassette + " (it will not call the agent at all)"
	case config.AgentModeRecord:
		return "calling the agent for real and recording it to " + cassette
	default:
		return "calling the agent for real"
	}
}

// credentialsLine reports whether each credential belay reads is set. It never
// reports their values.
func credentialsLine() string {
	names := []string{anthropicKeyEnv, claudeOAuthTokenEnv, sonarTokenEnv}
	parts := make([]string, 0, len(names))
	for _, name := range names {
		if os.Getenv(name) == "" {
			parts = append(parts, name+" is not set")
			continue
		}
		parts = append(parts, name+" is set")
	}
	return strings.Join(parts, "; ")
}

// dollars renders an amount the way a person reads money.
func dollars(v float64) string { return fmt.Sprintf("$%.2f", v) }

// thousands groups a number's digits in threes. A budget ceiling is not a place
// to make somebody count zeroes: 2000000 and 20000000 are the same shape at a
// glance, and 2,000,000 and 20,000,000 are not.
func thousands(n int64) string {
	digits := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	var b strings.Builder
	for i := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(digits[i])
	}
	return sign + b.String()
}

// newLogger returns the logger a run's dispatcher and adapters write to: the
// narrator, which renders the two events a person cares about and, with
// --verbose, forwards everything to stderr as well.
func newLogger(u *ui) *slog.Logger {
	var next slog.Handler
	if u.verbose {
		next = slog.NewTextHandler(u.err, &slog.HandlerOptions{Level: slog.LevelDebug})
	}
	return slog.New(&narrator{ui: u, next: next})
}

// Messages the dispatcher logs as it walks the graph. The narrator turns
// exactly these two into the run's on-screen story; everything else is
// belay talking to its operator, not to the person waiting.
const (
	msgNodeStarted  = "belay/graph: node started"
	msgNodeFinished = "belay/graph: node finished"
)

// nodeColumn is the width the node names are padded to, so the story reads
// down a column instead of zig-zagging. graph's longest node name is seven
// characters; the extra two are the gap.
const nodeColumn = 9

// narrator renders the dispatcher's structured log as the run's on-screen
// story.
//
// The dispatcher is the only thing that knows which node it is entering and
// which way it left, and it publishes that through its slog.Logger. Reading
// it here — rather than making the dispatcher print — keeps the graph free of
// a terminal, and keeps the wayfinding in the layer that owns the words.
type narrator struct {
	ui      *ui
	attrs   []slog.Attr
	grouped bool
	next    slog.Handler
}

var _ slog.Handler = (*narrator)(nil)

// Enabled implements slog.Handler.
func (n *narrator) Enabled(ctx context.Context, level slog.Level) bool {
	if n.next != nil {
		return n.next.Enabled(ctx, level)
	}
	return level >= slog.LevelInfo
}

// Handle implements slog.Handler. It narrates the two node events and passes
// every record on to the verbose log when there is one.
func (n *narrator) Handle(ctx context.Context, rec slog.Record) error {
	if !n.grouped {
		switch rec.Message {
		case msgNodeStarted:
			n.entered(n.fields(rec))
		case msgNodeFinished:
			n.left(n.fields(rec))
		}
	}
	if n.next != nil {
		return n.next.Handle(ctx, rec)
	}
	return nil
}

// WithAttrs implements slog.Handler. The dispatcher attaches node, step and
// attempt this way, so they have to be accumulated rather than read off the
// record alone.
func (n *narrator) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := n.clone()
	c.attrs = append(slices.Clip(c.attrs), attrs...)
	if c.next != nil {
		c.next = c.next.WithAttrs(attrs)
	}
	return c
}

// WithGroup implements slog.Handler. Grouped attributes are namespaced, so a
// narrator inside a group stops reading them and only forwards.
func (n *narrator) WithGroup(name string) slog.Handler {
	if name == "" {
		return n
	}
	c := n.clone()
	c.grouped = true
	if c.next != nil {
		c.next = c.next.WithGroup(name)
	}
	return c
}

func (n *narrator) clone() *narrator {
	c := *n
	return &c
}

// nodeEvent is one node transition, as the narrator reads it off the log.
type nodeEvent struct {
	node    string
	step    int64
	attempt int64
	status  string
	next    string
	elapsed time.Duration
	// candidate is the fanout candidate whose run this transition belongs
	// to, or empty for the run's own. The candidate runner attaches it to
	// every candidate dispatcher's logger.
	candidate string
}

// who prefixes a candidate's lines with its id. Several candidates narrate at
// once under fanout.concurrency, and five "→ test step 3" lines from five
// candidates would otherwise be indistinguishable. The run's own lines get
// nothing, so a run that does not fan out reads exactly as it always has.
func (ev nodeEvent) who() string {
	if ev.candidate == "" {
		return ""
	}
	return "[" + ev.candidate + "] "
}

// fields collects the attributes the dispatcher attached, from the handler's
// accumulated set and from the record itself.
func (n *narrator) fields(rec slog.Record) nodeEvent {
	var ev nodeEvent
	read := func(a slog.Attr) {
		switch a.Key {
		case "node":
			ev.node = a.Value.String()
		case "step":
			ev.step = a.Value.Int64()
		case "attempt":
			ev.attempt = a.Value.Int64()
		case "status":
			ev.status = a.Value.String()
		case "next":
			ev.next = a.Value.String()
		case "elapsed":
			ev.elapsed = a.Value.Duration()
		case "candidate":
			ev.candidate = a.Value.String()
		}
	}
	for _, a := range n.attrs {
		read(a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		read(a)
		return true
	})
	return ev
}

// entered prints the line that answers "what is belay doing now". It is the
// only line in a run that is emphasised, along with the closing cost.
func (n *narrator) entered(ev nodeEvent) {
	line := fmt.Sprintf("  %s→ %s step %d", ev.who(), padNode(ev.node), ev.step)
	if ev.attempt > 1 {
		line += fmt.Sprintf(", attempt %d", ev.attempt)
	}
	n.ui.say("%s", n.ui.emph(line))
}

// left prints the line that answers "which way did it go". Naming the route
// out of a node is what lets somebody follow a loop — test back to fix, fix
// back to test — instead of watching the same names scroll past.
func (n *narrator) left(ev nodeEvent) {
	n.ui.say("    %s%s %s", ev.who(), padNode(ev.node), nodeVerdict(ev))
}

// nodeVerdict renders how a node ended and where the run went next.
func nodeVerdict(ev nodeEvent) string {
	took := "took " + ev.elapsed.Round(time.Millisecond).String()
	switch ev.status {
	case "ok":
		if ev.next == graph.End {
			return took + " → done"
		}
		return took + " → " + ev.next
	case "paused":
		return took + " → paused, waiting for you"
	case "aborted":
		return took + " → stopped here"
	case "failed":
		return took + " → failed, the run stops here"
	default:
		return took
	}
}

// padNode lines the node names up into a column.
func padNode(node string) string {
	if len(node) >= nodeColumn {
		return node
	}
	return node + strings.Repeat(" ", nodeColumn-len(node))
}
