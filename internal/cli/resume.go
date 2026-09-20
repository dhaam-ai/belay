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
	"github.com/dhaam-ai/belay/internal/journal"
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

If you stopped the run with Ctrl-C, or belay was killed part-way through a
step, resuming runs that step again from its start. The steps that finished
are not run again.

If belay stopped the run at one of its own limits — the budget ceiling,
graph.max_steps or graph.node_timeout — it is paused, not over. Raise that
setting in belay.yaml and resume: belay reads the limits again as it starts,
so the run carries on with everything it has already spent still counted.
Leave the setting as it is and the run simply stops at it again, having done
nothing and spent nothing.

Runs live under <workspace>/.belay/runs/, and "belay runs" lists them. A run
that already finished or failed cannot be continued; start a new one instead.

A run a newer belay wrote cannot be continued by this one: upgrade belay. While
such a run is in the workspace, belay will not choose a run for you, since it
cannot tell whether that run is the one still waiting; name the run instead.

A run belay cannot read, because its manifest.json is missing or damaged,
cannot be continued either. Name it, and belay says what is wrong with it and
where it is; give no run id, and belay passes over it and chooses from the
others.`,
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
	if err := plan.resumePreflight(store); err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}

	if rootFlags.dryRun {
		return describeDryRun(u, plan, chosen.goal, chosen.describe(runID == ""))
	}
	if len(plan.blockers) > 0 {
		return refuseToStart(u, plan)
	}

	announceContinue(u, plan, chosen, layout, runID == "")

	// Recording an approval below saves state.json, and that is the first
	// thing belay writes to this run — before the dispatcher, which
	// establishes the run for itself before it opens the journal.
	// state.Store.Preflight refuses a run belay may not read, and saves the
	// manifest at the schema version belay writes, so a crash after the
	// approval cannot leave an older belay a manifest it still accepts over a
	// state.json it does not (ADR-0011).
	if err := store.Preflight(); err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}

	awaiting, err := awaitsApproval(chosen, layout)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	if awaiting {
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

// resumePreflight checks, before anything is spent, that this plan can
// continue the run in store, and records a blocker for each reason it cannot.
// continueRun calls it once, after choosing the run and before either the dry
// run or the dispatch, so a dry run lists the same refusals a real resume
// would.
//
// It only refuses. resolve built the plan from belay.yaml as it is now, and
// the file may have changed since the run began. Whatever decides the shape
// of a run is pinned in the run's manifest when the run is created, and a
// resume must walk the graph those pins describe, not the one belay.yaml
// describes today (internal/nodes explains why). Rebuilding the registry and
// the route from the pins is resolveRegistry's job. This is where a pin that
// this belay or this plan cannot honour becomes a blocker.
//
// It is not state.Store.Preflight, which continueRun calls later, only for a
// real resume and before it can record an approval, and which the dispatcher
// calls before it opens the journal. That one refuses, with an error, a run
// this belay may not read at all (a capability it lacks, or a schema version
// it cannot read, in state.json), and it writes: it saves a manifest below
// the current schema version at that version. This one writes nothing, runs
// for a dry run too, and reports what it finds as blockers, beside the
// plan's others.
//
// Today it checks nothing: no manifest pins anything yet. An error means the
// check itself could not be made, such as a manifest that cannot be read. A
// run that cannot be continued is a blocker instead, so that it is reported
// with the others.
//
//nolint:revive // store is unused until a run's manifest pins something a resume must check; it is the hook's contract.
func (p *runPlan) resumePreflight(store *state.Store) error {
	return nil
}

// awaitsApproval reports whether resuming chosen is how a person approves its
// plan: the run is paused at the approve gate because the gate asked.
//
// A run is also paused at approve when Ctrl-C lands while the gate is running
// (see pauseOnCancel). Nobody was shown the plan then, or told that
// resuming approves it, so resuming must run the gate again, which asks,
// instead of approving on the person's behalf. The manifest cannot tell the
// two apart; the journal can. The gate's own pause is the run's last
// node_finished, with status paused. An interrupted attempt is closed as
// aborted (journal.Record.Interrupted).
func awaitsApproval(chosen resumableRun, layout state.Layout) (bool, error) {
	if chosen.node != graph.NodeApprove || chosen.status != state.RunStatusPaused {
		return false, nil
	}
	read, err := journal.ReadFile(layout.JournalPath())
	if err != nil {
		return false, fmt.Errorf("cannot read this run's journal: %w", err)
	}
	for i := len(read.Records) - 1; i >= 0; i-- {
		if rec := read.Records[i]; rec.Event == journal.EventNodeFinished {
			return rec.Node == graph.NodeApprove && rec.Status == journal.StatusPaused, nil
		}
	}
	return false, nil
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
	// unreadable is why this belay cannot read the run's manifest: a run a
	// newer belay wrote (see needsNewerBelay), or a damaged one. Every field
	// but id is zero when it is set.
	unreadable error
}

// needsNewer reports whether this run is one a newer belay wrote, rather
// than a damaged one. The two are refused differently, because only one of
// them is a fault (ADR-0011).
func (r resumableRun) needsNewer() bool { return needsNewerBelay(r.unreadable) }

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
			if r.needsNewer() {
				return resumableRun{}, fmt.Errorf(
					"belay cannot continue run %s: it needs a newer belay.\n%w", r.id, r.unreadable)
			}
			if r.unreadable != nil {
				return resumableRun{}, damagedRun(workspace, r)
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

	// A run a newer belay wrote may be the most recent one still waiting,
	// and this belay cannot read whether it is waiting or when it was last
	// worked on. Choosing past it would continue, and spend on, a run the
	// person may not have meant, so belay refuses to choose at all. Naming a
	// run still continues it.
	for _, r := range runs {
		if r.needsNewer() {
			return resumableRun{}, fmt.Errorf(
				"belay will not choose a run to continue: run %s needs a newer belay, and this belay cannot tell whether it is the one still waiting.\n%w\n"+
					"To continue another run with this belay, name it: belay resume <run-id>",
				r.id, r.unreadable)
		}
	}

	// A damaged run is passed over rather than refused: belay cannot tell
	// whether it was the one still waiting, and refusing to choose would make
	// one broken directory stop every later `belay resume` in the workspace.
	// `belay runs` is where it is reported, and naming it says what is wrong.
	for _, r := range runs {
		if r.unreadable == nil && resumable(r.status) {
			return r, nil
		}
	}
	return resumableRun{}, nothingToResume(workspace, runs)
}

// needsNewerBelay reports whether err, from loading a run's manifest, says
// a newer belay wrote the run: it requires a capability this belay does not
// know, or its schema version is above the one this belay reads. Either way
// the run is not damaged, and the error says to upgrade belay (ADR-0011).
func needsNewerBelay(err error) bool {
	var versionErr *state.ManifestVersionError
	return errors.Is(err, state.ErrUnknownCapability) || (errors.As(err, &versionErr) && versionErr.Newer())
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
	ended, unreadable := 0, 0
	for _, r := range runs {
		if r.unreadable != nil {
			unreadable++
			continue
		}
		ended++
	}
	// A run belay cannot read has not "already ended" — belay has no idea how
	// it ended — so it is counted and described separately rather than folded
	// into a sentence that would be a guess.
	switch {
	case ended == 0:
		return fmt.Errorf(
			"belay found %d run(s) in %s, and cannot read any of them.\n"+
				"See what is wrong with them: belay runs\nStart a new one: belay run \"what you want built\"",
			unreadable, workspace)
	case unreadable > 0:
		return fmt.Errorf(
			"belay found %d run(s) in %s, and every one of them has already ended; %d more cannot be read.\n"+
				"See them with: belay runs\nStart a new one: belay run \"what you want built\"",
			ended, workspace, unreadable)
	default:
		return fmt.Errorf(
			"belay found %d run(s) in %s, and every one of them has already ended.\n"+
				"See them with: belay runs\nStart a new one: belay run \"what you want built\"",
			ended, workspace)
	}
}

// damagedRun explains a run belay found but cannot read, for someone who
// named it outright.
//
// It says the run is there, what is wrong with it in the words `belay runs`
// uses for the same run, and where the directory is — because the one thing a
// person can do about a damaged manifest is look at it. Reporting it as a run
// that does not exist, which is what skipping it amounted to, sends them
// hunting for a typo in an id they copied from `belay runs`.
func damagedRun(workspace string, r resumableRun) error {
	where := filepath.Join(workspace, ".belay", "runs", r.id)
	if layout, err := state.NewLayout(workspace, r.id); err == nil {
		where = layout.RunDir()
	}
	return fmt.Errorf(
		"belay cannot continue run %s: %s.\nIt is at %s, and `belay runs` lists it with everything else.\n"+
			"belay resume needs that file to know where the run stopped, so start a new run instead: belay run \"what you want built\"\n%w",
		r.id, runsUnreadableReason(r.unreadable), where, r.unreadable)
}

// unknownRun explains a run id belay cannot find, and lists what it does have.
func unknownRun(workspace, runID string, runs []resumableRun) error {
	if len(runs) == 0 {
		return fmt.Errorf("belay has no run called %s; in fact it has not run in %s at all", runID, workspace)
	}
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		sentence := statusSentence(r.status, r.node)
		if r.unreadable != nil {
			sentence = runsUnreadableReason(r.unreadable)
		}
		ids = append(ids, "  "+r.id+" — "+sentence)
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
// display type.
//
// A run whose manifest cannot be read is kept, named by its directory and
// carrying the error. belay must not resume it, but "must not resume" is not
// "is not there": skipping it made `belay resume <id>` answer "belay has no
// run called <id>" for an id a person had just read off `belay runs`. So it
// is kept, chooseRun refuses it by name with what is wrong, and no implicit
// choice ever lands on it. A run a newer belay wrote is the same shape and
// not a fault at all (ADR-0011); needsNewerBelay tells the two apart. Neither
// has a readable update time, so both sort after every run that has one.
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
			found = append(found, resumableRun{id: entry.Name(), unreadable: err})
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
	u.say("  route:  %s", routeLine(plan.route))
	u.say("")
}

// orUnrecorded fills in for a manifest that never recorded what was asked for.
func orUnrecorded(goal string) string {
	if strings.TrimSpace(goal) == "" {
		return "(this run did not record what it was asked for)"
	}
	return goal
}
