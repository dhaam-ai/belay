package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
)

var resumeCmd = &cobra.Command{
	Use:   "resume [run-id]",
	Short: "Continue a run that stopped part-way",
	Long: `Continue a run.

Given no run id, belay continues the most recent run in this repository that is
still waiting — and says which one it picked before it does anything:

    belay resume

Give a run id to continue a particular one:

    belay resume 20260905T153301Z-1f2e3d4c5b6a

If the run stopped at the plan for your approval, resuming is how you approve
it. Read the plan first, and edit it if you want to: belay reads the file again
as it starts, so your edits are what the agent is given.

Runs live under <workspace>/.belay/runs/, and "belay runs" lists them. A run
that already finished, failed or was stopped for good cannot be continued;
start a new one instead.`,
	Example: `  belay resume
  belay resume 20260905T153301Z-1f2e3d4c5b6a
  belay --dry-run resume`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var runID string
		if len(args) == 1 {
			runID = strings.TrimSpace(args[0])
		}
		return continueRun(cmd, runID)
	},
}

func init() {
	addRunFlags(resumeCmd)
	rootCmd.AddCommand(resumeCmd)
}

// continueRun picks the run to continue, says which one it picked, and drives
// it.
func continueRun(cmd *cobra.Command, runID string) error {
	u := newUI(cmd)

	plan, err := resolve(resolveRequest{
		workspace:  rootFlags.workspace,
		configPath: rootFlags.configPath,
		cassette:   runFlags.cassette,
		logger:     newLogger(u),
	})
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}

	chosen, err := chooseRun(plan.workspace, runID)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: err.Error(), Err: err}
	}

	layout, err := state.NewLayout(plan.workspace, chosen.id)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	store := state.NewStore(layout)

	if rootFlags.dryRun {
		return describeDryRun(u, plan, chosen.goal, chosen.describe(runID == ""))
	}
	if len(plan.blockers) > 0 {
		return refuseToStart(u, plan)
	}

	announceContinue(u, plan, chosen, layout, runID == "")

	if chosen.node == graph.NodeApprove && chosen.status == state.RunStatusPaused {
		approved, err := approvePlan(store)
		if err != nil {
			return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
		}
		if approved {
			u.say("  the plan is approved; belay reads it again now, so any edit you made counts")
			u.say("")
		}
	}

	return driveRun(cmd, u, plan, store, true)
}

// approvePlan records that the human approved the plan, and reports whether
// it had to change anything.
//
// Resuming past the approval gate is the approval: there is no other gesture
// a person makes, and asking them to also edit state.json would be absurd.
// Only the flag is set here. The approve node re-reads plan.md and
// re-fingerprints it as it runs, so an edit made during the pause is recorded
// as what was actually approved — which is exactly why this must not try to
// record the plan's contents itself.
func approvePlan(store *state.Store) (bool, error) {
	st, err := store.LoadState()
	if err != nil {
		return false, fmt.Errorf("cannot read this run's state: %w", err)
	}
	if st.Plan.Approved {
		return false, nil
	}
	st.Plan.Approved = true
	if err := store.SaveState(st); err != nil {
		return false, fmt.Errorf("cannot record your approval: %w", err)
	}
	return true, nil
}

// resumableRun is one run belay found on disk, reduced to what choosing
// between them needs.
type resumableRun struct {
	id      string
	goal    string
	status  state.RunStatus
	node    string
	updated string
}

// describe names this run in one line, and says how belay came to be pointed
// at it. Acting on an implicit target without saying so is how a person stops
// trusting a tool.
func (r resumableRun) describe(implicit bool) string {
	line := fmt.Sprintf("%s — %s", r.id, statusSentence(r.status, r.node))
	if implicit {
		return line + " (the most recent run that was still waiting)"
	}
	return line
}

// statusSentence says what a run's state means, rather than printing the word
// "paused" or "running" and leaving a person to guess.
func statusSentence(status state.RunStatus, node string) string {
	switch status {
	case state.RunStatusPaused:
		return "stopped at " + nodeClause(node)
	case state.RunStatusRunning:
		return "interrupted part-way through " + nodeClause(node)
	case state.RunStatusCompleted:
		return "already finished"
	case state.RunStatusFailed:
		return "already ended in failure"
	case state.RunStatusAborted:
		return "already stopped for good"
	default:
		return "in an unknown state"
	}
}

// nodeClause names a step and says what it does, in a form that reads inside
// a longer sentence. whereText is its sibling for a line of its own.
func nodeClause(node string) string {
	if meaning, ok := nodeMeaning[node]; ok {
		return node + " (" + meaning + ")"
	}
	if node == "" {
		return "a step belay had not reached"
	}
	return node
}

// chooseRun resolves which run to continue: the one named, or the most recent
// one still waiting.
func chooseRun(workspace, runID string) (resumableRun, error) {
	runs, err := scanRunsForResume(workspace)
	if err != nil {
		return resumableRun{}, err
	}

	if runID != "" {
		for _, r := range runs {
			if r.id != runID {
				continue
			}
			if !resumable(r.status) {
				return resumableRun{}, fmt.Errorf(
					"belay cannot continue run %s: it is %s.\nStart a new run instead: belay run \"what you want built\"",
					r.id, statusSentence(r.status, r.node))
			}
			return r, nil
		}
		return resumableRun{}, unknownRun(workspace, runID, runs)
	}

	for _, r := range runs {
		if resumable(r.status) {
			return r, nil
		}
	}
	return resumableRun{}, nothingToResume(workspace, runs)
}

// resumable reports whether a run in this state can be continued. A paused
// run is waiting for a person; a running one is a run whose process died, and
// the dispatcher heals its journal on the way back in.
func resumable(status state.RunStatus) bool {
	return status == state.RunStatusPaused || status == state.RunStatusRunning
}

// nothingToResume explains an empty result in a sentence, and points at the
// two commands that get somewhere from here.
func nothingToResume(workspace string, runs []resumableRun) error {
	if len(runs) == 0 {
		return fmt.Errorf(
			"belay has not run in %s yet, so there is nothing to continue.\nStart one: belay run \"what you want built\"",
			workspace)
	}
	return fmt.Errorf(
		"belay found %d run(s) in %s, and every one of them has already ended.\n"+
			"See them with: belay runs\nStart a new one: belay run \"what you want built\"",
		len(runs), workspace)
}

// unknownRun explains a run id belay cannot find, and lists what it does have.
func unknownRun(workspace, runID string, runs []resumableRun) error {
	if len(runs) == 0 {
		return fmt.Errorf("belay has no run called %s; in fact it has not run in %s at all", runID, workspace)
	}
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, "  "+r.id+" — "+statusSentence(r.status, r.node))
	}
	if len(ids) > 5 {
		ids = append(ids[:5], fmt.Sprintf("  ...and %d more; see them all with: belay runs", len(runs)-5))
	}
	return fmt.Errorf("belay has no run called %s in %s.\nIt does have:\n%s",
		runID, workspace, strings.Join(ids, "\n"))
}

// scanRunsForResume reads every run directory's manifest, most recently
// touched first.
//
// This deliberately does not go through the listing `belay runs` renders:
// choosing what to resume needs a manifest's status and current node, and a
// command that decides which run to spend money on should not depend on a
// display type. A run whose manifest cannot be read is skipped rather than
// reported, because a damaged run is one belay must not resume, and `belay
// runs` is the command that exists to show it.
func scanRunsForResume(workspace string) ([]resumableRun, error) {
	dir := filepath.Join(workspace, ".belay", "runs")
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("belay cannot read %s: %w", dir, err)
	}

	found := make([]resumableRun, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		layout, err := state.NewLayout(workspace, entry.Name())
		if err != nil {
			continue
		}
		manifest, err := state.LoadManifest(layout.ManifestPath())
		if err != nil {
			continue
		}
		found = append(found, resumableRun{
			id:      manifest.RunID,
			goal:    manifest.Goal,
			status:  manifest.Status,
			node:    manifest.CurrentNode,
			updated: manifest.UpdatedAt.UTC().Format("2006-01-02 15:04:05Z"),
		})
	}

	// Newest first. Run IDs begin with a fixed-width UTC timestamp, so
	// comparing them as strings orders them chronologically; UpdatedAt
	// breaks the tie for two runs started in the same second, and is what
	// makes "the most recent run" mean the one last worked on rather than
	// the one started last.
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].updated != found[j].updated {
			return found[i].updated > found[j].updated
		}
		return found[i].id > found[j].id
	})
	return found, nil
}

// announceContinue says which run belay is about to continue, and what that
// run is going to cost, before it spends anything.
func announceContinue(u *ui, plan *runPlan, chosen resumableRun, layout state.Layout, implicit bool) {
	u.say("belay is continuing a run in %s", plan.workspace)
	u.say("  run:                 %s", chosen.describe(implicit))
	u.say("  what you asked for:  %s", orUnrecorded(chosen.goal))
	u.say("  most it can spend:   %s", u.emph(budgetCeiling(plan.config.Budget)))
	u.say("  when it gets there:  %s", budgetOnExceed(plan.config.Budget))
	u.say("  to stop it:          press Ctrl-C once; belay saves its state and exits")
	u.say("")
	u.say("  state:  %s", layout.RunDir())
	u.say("  route:  %s", routeLine())
	u.say("")
}

// orUnrecorded fills in for a manifest that never recorded what was asked for.
func orUnrecorded(goal string) string {
	if strings.TrimSpace(goal) == "" {
		return "(this run did not record what it was asked for)"
	}
	return goal
}
