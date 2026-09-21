package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/nodes/plan"
	"github.com/dhaam-ai/belay/internal/state"
)

// planFlags holds `belay plan`'s own settings.
var planFlags struct {
	regenerate string
}

var planCmd = &cobra.Command{
	Use:   "plan [run-id]",
	Short: "Read the plan a run is waiting on, or ask for a different one",
	Long: `Read the plan a run stopped to show you.

Given no run id, belay shows the plan of the most recent run in this
repository that is still waiting — and says which one it picked:

    belay plan

The plan is a file, at <workspace>/.belay/runs/<run-id>/artifacts/plan.md.
Edit it in whatever you write code in; belay reads it again as the run
continues, so your edits are what the agent is given. "belay resume" is how
you approve it.

If the plan is not what you wanted, you can have the agent write another one
instead of editing this one yourself. Say what it should do differently:

    belay plan --regenerate "fewer steps, and leave the parser alone"

That records what you asked for, keeps a copy of the plan it replaces beside
it, and continues the run: the plan step runs again, with your words in its
prompt, and the run stops at the plan again so you can read the new one. It
spends what a planning step spends, which belay says before it starts.

You can ask for a new plan only while a run is waiting at its plan for you;
belay says where the run is instead if it is somewhere else.`,
	Example: `  belay plan
  belay plan 20260905T153301Z-1f2e3d4c5b6a
  belay plan --regenerate "fewer steps; do not touch the parser"`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var runID string
		if len(args) == 1 {
			runID = strings.TrimSpace(args[0])
		}
		if cmd.Flags().Changed("regenerate") {
			return regeneratePlan(cmd, runID, planFlags.regenerate)
		}
		return showPlan(cmd, runID)
	},
}

func init() {
	addRunFlags(planCmd)
	planCmd.Flags().StringVar(&planFlags.regenerate, "regenerate", "",
		`ask the agent for a new plan, saying what it should do differently`)
	rootCmd.AddCommand(planCmd)
}

// showPlan prints the plan of the run belay picked, and what can be done
// with it.
//
// It deliberately does not go through resolve: reading the plan of a run
// that has already stopped needs no agent, no test runner and no quality
// gate, and refusing to show a person their own plan because the backend
// they configured is not installed would be absurd.
func showPlan(cmd *cobra.Command, runID string) error {
	u := newUI(cmd)

	workspace, err := workspaceDirOf(cmd)
	if err != nil {
		return err
	}
	chosen, err := chooseRun(workspace, runID)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: err.Error(), Err: err}
	}
	layout, err := state.NewLayout(workspace, chosen.id)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	st, err := state.NewStore(layout).LoadState()
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: cannot read this run's state: " + err.Error(), Err: err}
	}
	// A run belay has not planned yet has nothing to show. It is said in
	// those words, rather than in the gate's ("path is not recorded in
	// state"), because the person reading this did not ask about state.json.
	if st.Plan.Path == "" {
		err := fmt.Errorf("run %s has not written a plan yet; it is %s.\nWatch it, or pick it up, with: belay resume %s",
			chosen.id, statusSentence(chosen.status, chosen.node), chosen.id)
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	_, abs, err := approve.PlanArtifact(layout, st.Plan.Path)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	body, err := os.ReadFile(abs) //nolint:gosec // path is constrained by approve.PlanArtifact
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: cannot read " + abs + ": " + err.Error(), Err: err}
	}

	u.say("run %s", chosen.describe(runID == ""))
	u.say("  what you asked for:  %s", orUnrecorded(chosen.goal))
	u.say("  the plan is at:      %s", abs)
	u.say("")
	u.say("%s", strings.TrimRight(string(body), "\n"))
	u.say("")
	u.say("Edit that file if you want to, then approve it and carry on with:")
	u.say("")
	u.say("    belay resume %s", chosen.id)
	u.say("")
	u.say("Or have the agent write a different plan:")
	u.say("")
	u.say(`    belay plan --regenerate "what it should do differently" %s`, chosen.id)
	return nil
}

// regeneratePlan records a person's request for a new plan and continues the
// run, so that the plan node writes one.
//
// It works with no terminal, which is the whole point: it is the same
// journalled edge the plan viewer drives, reachable from a script, an SSH
// session or a machine with no TTY at all. Nothing here calls the agent.
// Recording the request and dispatching the run is all it does; the plan
// node does the work, inside the dispatcher, where it is journalled,
// charged to the budget and resumable.
func regeneratePlan(cmd *cobra.Command, runID, guidance string) error {
	guidance = strings.TrimSpace(guidance)
	if guidance == "" {
		err := errors.New(`say what the new plan should do differently: belay plan --regenerate "fewer steps" <run-id>`)
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}

	u := newUI(cmd)
	runPlan, err := resolve(resolveRequest{
		workspace:  rootFlags.workspace,
		configPath: rootFlags.configPath,
		cassette:   runFlags.cassette,
		logger:     newLogger(u),
	})
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}

	chosen, err := chooseRun(runPlan.workspace, runID)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: err.Error(), Err: err}
	}
	layout, err := state.NewLayout(runPlan.workspace, chosen.id)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	store, err := runPlan.openRun(cmd.Context(), layout)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}

	// A new plan is only ever asked for by somebody looking at the plan the
	// gate stopped to show them. A run that is somewhere else entirely
	// would take the request, sit on it until it next reached the gate, and
	// regenerate then — long after the person had forgotten asking.
	awaiting, err := awaitsApproval(chosen, layout)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	if !awaiting {
		err := fmt.Errorf("belay cannot ask for a new plan for run %s: it is %s, not waiting at its plan.\nSee where it is with: belay timeline %s",
			chosen.id, statusSentence(chosen.status, chosen.node), chosen.id)
		return &ExitError{Code: graph.ExitFailed, Message: err.Error(), Err: err}
	}

	if rootFlags.dryRun {
		return describeDryRun(u, runPlan, chosen.goal, chosen.describe(runID == ""))
	}
	if len(runPlan.blockers) > 0 {
		return refuseToStart(u, runPlan)
	}

	announceContinue(u, runPlan, chosen, layout, runID == "")

	// Preflight before the request is recorded, for the reason continueRun
	// gives: recording it is the first thing belay writes to this run, and a
	// crash afterwards must not leave an older belay a manifest it accepts
	// over a state.json it does not (ADR-0011).
	if err := store.Preflight(); err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	if err := requestRegeneration(store, guidance); err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	u.say("  belay is writing a new plan; the run stops at the plan again when it has one")
	u.say("")

	return driveRun(cmd, u, runPlan, store, true)
}

// requestRegeneration records, on the run's blackboard, that a person has
// asked for a new plan and what they said it should do differently.
//
// It writes two things, in this order:
//
//  1. A copy of the plan that is about to be replaced, under
//     plan.ReplacedArtifact's name. Whoever asked has read that plan, and
//     may want it back.
//  2. The request itself, with the approval withdrawn: a plan somebody has
//     asked to replace must not be handed to the code node, and a resume
//     that found Approved still set would do exactly that.
//
// The order is what makes a crash between them harmless. Stopping after the
// copy leaves an unreferenced file and a run nobody has changed; stopping
// after the request would leave the plan node free to overwrite a document
// no copy was kept of. Running it again converges either way: an
// outstanding request keeps its number and takes the new guidance, so a
// person who asks twice — or a command re-run after a crash — has asked
// once.
//
// It does not fail on a plan it cannot read. A plan somebody emptied or
// deleted is one of the reasons to ask for a new one, and refusing the
// request would leave that run with no way out but hand-writing a plan.
// There is simply nothing to keep a copy of.
func requestRegeneration(store *state.Store, guidance string) error {
	st, err := store.LoadState()
	if err != nil {
		return fmt.Errorf("cannot read this run's state: %w", err)
	}

	request := st.Plan.Regenerate
	if !request.Pending() {
		request.Request = max(request.Request, request.Served) + 1
	}
	request.Guidance = guidance

	if err := keepReplacedPlan(store, st.Plan.Path, request.Request); err != nil {
		return err
	}

	st.Plan.Approved = false
	st.SetRegeneration(request)
	if err := store.SaveState(st); err != nil {
		return fmt.Errorf("cannot record your request: %w", err)
	}
	return nil
}

// keepReplacedPlan copies the run's current plan to the artifact that keeps
// the plan request replaced, and reports only the failures that mean the
// copy could not be made although there was one to make.
//
// Whether what is on disk is a plan at all is the approve gate's judgement
// (approve.CheckPlan), so that this never keeps a copy the gate would not
// have shown anybody, and never refuses a request because the plan is in
// the state that prompted it.
func keepReplacedPlan(store *state.Store, recorded string, request int) error {
	name, abs, err := approve.PlanArtifact(store.Layout(), recorded)
	if err != nil {
		return nil //nolint:nilerr // nothing to keep is not a failure to keep it; see the doc comment
	}
	body, err := os.ReadFile(abs) //nolint:gosec // path is constrained by approve.PlanArtifact
	if err != nil {
		return nil //nolint:nilerr // as above
	}
	if err := approve.CheckPlan(body); err != nil {
		return nil //nolint:nilerr // as above
	}
	if name == plan.ReplacedArtifact(request) {
		return nil
	}
	if err := state.ReplaceArtifact(store.Layout(), plan.ReplacedArtifact(request), body); err != nil {
		return fmt.Errorf("cannot keep a copy of the plan being replaced: %w", err)
	}
	return nil
}
