package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/isolate/dircopy"
	"github.com/dhaam-ai/belay/internal/queue"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// queueFlags holds the settings belonging to `belay queue` and its
// subcommands alone.
var queueFlags struct {
	noApproval bool
	json       bool
}

var queueCmd = &cobra.Command{
	Use:   "queue",
	Short: "Line up goals, and let belay work through them one at a time while you are away",
	Long: `Line up goals, and let belay work through them one at a time, unattended.

    belay queue add "add table-driven tests for the config parser"
    belay queue add "make the CLI accept a --json flag"
    belay queue start

Each goal gets a run of its own, in a copy of this workspace made when belay
reaches the goal, under .belay/queue/work/. Your checkout is never edited, and
taking a goal's change from its copy is up to you.

A goal whose plan is written stops for you to read it, as every run does: the
queue parks it and moves on to the next goal. Approve it when you are back,
with "belay queue approve <goal>", and the next "belay queue start" carries it
on. Only a goal you add with --no-approval goes from its plan to its code
without waiting for you. belay never decides that for you.

Every goal is held to budget.max_usd on its own, and "belay queue start" says
the most the whole queue can spend before it spends anything.

Press Ctrl-C and the goal in progress pauses where it stands. If the machine
reboots or belay is killed, nothing is lost either: "belay queue start" carries
on with the goal it had in hand, then the rest.

"belay queue" on its own says how the queue stands, and what to do next.`,
	Example: `  belay queue add "add tests for the config parser"
  belay queue add --no-approval "fix the typos in README.md"
  belay queue start
  belay queue
  belay queue approve 1`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error { return showQueueStatus(cmd) },
}

var queueAddCmd = &cobra.Command{
	Use:   `add "<what you want built>"`,
	Short: "Add a goal to the end of the queue",
	Long: `Add a goal to the end of the queue, in your own words, as one argument.

When belay reaches it, the goal's plan stops for you to read, and the queue
parks it and moves on. Add --no-approval to let this one goal go from its plan
to its code without you.`,
	Example: `  belay queue add "add tests for the config parser"
  belay queue add --no-approval "fix the typos in README.md"`,
	Args: oneQueuedGoal,
	RunE: func(cmd *cobra.Command, args []string) error {
		return addToQueue(cmd, strings.TrimSpace(args[0]))
	},
}

var queueListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the queue's goals, in the order belay runs them",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return listQueue(cmd) },
}

var queueStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Say how the queue stands, what it has spent, and what to do next",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return showQueueStatus(cmd) },
}

var queueRmCmd = &cobra.Command{
	Use:   "rm <goal>",
	Short: "Take a goal back out of the queue, before belay has started it",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return removeFromQueue(cmd, args[0]) },
}

var queueApproveCmd = &cobra.Command{
	Use:   "approve <goal>",
	Short: "Approve the plan of a goal that is waiting for you",
	Long: `Approve the plan of a goal the queue parked for you.

Read the plan first, and edit it if you want to: belay reads the file again
when it carries the goal on, so your edits are what the agent is given.
"belay queue" says where each waiting goal's plan is. The next "belay queue
start" carries the goal on, before any goal still waiting its turn.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error { return approveQueuedGoal(cmd, args[0]) },
}

var queueStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Work through the queue, one goal at a time, until nothing is left that can run",
	Long: `Work through the queue, one goal at a time.

belay first says how many goals it will run, the most each can spend
(budget.max_usd), and the most the whole queue can spend, before it spends
anything. It carries on with a goal the last start had in hand, then goals you
approved, then the goals waiting their turn, and stops when nothing is left it
can run. A goal whose plan stops for you is parked, and belay moves on.

Only one "belay queue start" works through a queue at a time. Press Ctrl-C
once to stop: the goal in progress pauses where it stands, and the next start
carries it on.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error { return startTheQueue(cmd) },
}

func init() {
	queueAddCmd.Flags().BoolVar(&queueFlags.noApproval, "no-approval", false,
		"let this goal go from its plan to its code without waiting for you to approve the plan")
	queueListCmd.Flags().BoolVar(&queueFlags.json, "json", false,
		"print the goals as JSON instead of a table")
	addRunFlags(queueStartCmd)
	queueCmd.AddCommand(queueAddCmd, queueListCmd, queueStatusCmd, queueRmCmd, queueApproveCmd, queueStartCmd)
	rootCmd.AddCommand(queueCmd)
}

// oneQueuedGoal insists on one goal, in the words a person would use.
func oneQueuedGoal(_ *cobra.Command, args []string) error {
	switch {
	case len(args) == 0:
		return errors.New(`say what you want built, in quotes: belay queue add "add tests for the parser"`)
	case len(args) > 1:
		return fmt.Errorf(`belay takes one goal, in quotes, but got %d arguments; `+
			`did you forget the quotes around "%s"?`, len(args), strings.Join(args, " "))
	case strings.TrimSpace(args[0]) == "":
		return errors.New("that goal is empty; tell belay what you want built")
	default:
		return nil
	}
}

// failed is the ExitError every queue command gives when it cannot do what
// it was asked.
func failed(err error) error {
	return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
}

// openWorkspaceQueue resolves --workspace and opens its queue.
func openWorkspaceQueue() (string, *queue.Store, error) {
	ws, err := resolveWorkspace(rootFlags.workspace)
	if err != nil {
		return "", nil, err
	}
	store, err := queue.Open(ws)
	if err != nil {
		return "", nil, err
	}
	return ws, store, nil
}

// addToQueue is `belay queue add`.
func addToQueue(cmd *cobra.Command, goal string) error {
	u := newUI(cmd)
	ws, store, err := openWorkspaceQueue()
	if err != nil {
		return failed(err)
	}
	if rootFlags.dryRun {
		u.say("This is a dry run. belay would add this goal to the queue in %s:", ws)
		u.say("  %s", goal)
		u.say("")
		u.say("Nothing was created and nothing was spent.")
		return nil
	}
	// The queue lives under .belay, which holds every goal as typed: the
	// first queue in a repository keeps it out of commits, as the first run
	// does.
	if err := ignoreRunState(ws); err != nil {
		return failed(err)
	}
	added, err := store.Add(goal, queueFlags.noApproval)
	if err != nil {
		return failed(err)
	}
	q, err := store.Read()
	if err != nil {
		return failed(err)
	}

	u.say("added goal %d to the queue: %s", added.ID, added.Goal)
	switch ahead := len(runnable(q)) - 1; ahead {
	case 0:
		u.say("  nothing is ahead of it.")
	default:
		u.say("  %s ahead of it.", runsPlural(ahead, "goal is", "goals are"))
	}
	if added.NoApproval {
		u.say("  you added it with --no-approval, so it goes from its plan to its code without waiting for you.")
	} else {
		u.say("  when its plan is written, the queue parks it for you to read, and moves on.")
		u.say("  approve it then with:  belay queue approve %d", added.ID)
	}
	if pid, active, err := store.WorkerActive(); err == nil && active {
		u.say("  belay queue start is working through the queue now (pid %d), and takes it in its turn.", pid)
		return nil
	}
	u.say("  start the queue with:  belay queue start")
	return nil
}

// QueueListing is the documented shape of `belay queue list --json`.
type QueueListing struct {
	// Workspace is the absolute path of the workspace whose queue this is.
	Workspace string `json:"workspace"`
	// Goals is every goal ever added, removed ones included, in the order
	// they were added. It is never null.
	Goals []QueueGoal `json:"goals"`
}

// QueueGoal is one goal as `belay queue list --json` reports it. Every field
// is always present, as in `belay runs --json`, so a script never has to tell
// a missing key from an empty one.
type QueueGoal struct {
	// ID is the goal's number, which `belay queue approve` and `belay queue
	// rm` take.
	ID int `json:"id"`
	// Goal is what was asked for, exactly as it was added.
	Goal string `json:"goal"`
	// NoApproval is whether it was added with --no-approval.
	NoApproval bool `json:"no_approval"`
	// State is where the goal stands: queued, claimed, waiting, approved,
	// finished, failed, stopped, paused or removed.
	State string `json:"state"`
	// AddedAt is when the goal was added, and UpdatedAt when anything last
	// happened to it.
	AddedAt   time.Time `json:"added_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// RunID is the goal's run, and Copy the copy of the workspace it works
	// in. Both are empty until belay claims the goal. The run's own files
	// are under Copy, in .belay/runs/<run_id>.
	RunID string `json:"run_id"`
	Copy  string `json:"copy"`
	// Note is what belay last said about the goal: where its plan is, or why
	// it stopped.
	Note string `json:"note"`
	// SpentUSD is what the goal's run has spent, from its manifest, and
	// Estimated whether that rests on an estimate. Zero before it has a run.
	SpentUSD  float64 `json:"spent_usd"`
	Estimated bool    `json:"estimated"`
}

// queueGoals turns the queue into what the queue commands show: each goal,
// with what its run has spent read from the run's manifest.
func queueGoals(q queue.Queue) []QueueGoal {
	goals := make([]QueueGoal, 0, len(q.Entries))
	for _, e := range q.Entries {
		g := QueueGoal{
			ID: e.ID, Goal: e.Goal, NoApproval: e.NoApproval, State: e.State.String(),
			AddedAt: e.AddedAt, UpdatedAt: e.UpdatedAt, RunID: e.RunID, Copy: e.Dir, Note: e.Note,
		}
		if man, ok := goalManifest(e); ok {
			g.SpentUSD, g.Estimated = man.Budget.SpentUSD, man.Budget.Estimated
		}
		goals = append(goals, g)
	}
	return goals
}

// goalManifest reads the manifest of e's run, if it has one belay can read.
func goalManifest(e queue.Entry) (state.Manifest, bool) {
	if e.RunID == "" {
		return state.Manifest{}, false
	}
	layout, err := state.NewLayout(e.Dir, e.RunID)
	if err != nil {
		return state.Manifest{}, false
	}
	man, err := state.LoadManifest(layout.ManifestPath())
	return man, err == nil
}

// listQueue is `belay queue list`.
func listQueue(cmd *cobra.Command) error {
	u := newUI(cmd)
	ws, store, err := openWorkspaceQueue()
	if err != nil {
		return failed(err)
	}
	q, err := store.Read()
	if err != nil {
		return failed(err)
	}
	goals := queueGoals(q)
	if queueFlags.json {
		return writeQueueJSON(u.out, QueueListing{Workspace: ws, Goals: goals})
	}
	if len(visibleGoals(goals)) == 0 {
		u.say(`The queue in %s is empty. Add a goal with: belay queue add "what you want built"`, ws)
		return nil
	}
	renderQueueTable(u, goals)
	return nil
}

// writeQueueJSON writes the listing as indented JSON.
func writeQueueJSON(w io.Writer, listing QueueListing) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(listing); err != nil {
		return fmt.Errorf("cli: write queue JSON: %w", err)
	}
	return nil
}

// visibleGoals leaves out the goals a person took back.
func visibleGoals(goals []QueueGoal) []QueueGoal {
	out := make([]QueueGoal, 0, len(goals))
	for _, g := range goals {
		if g.State != queue.StateRemoved.String() {
			out = append(out, g)
		}
	}
	return out
}

// queueStateWords says where a goal stands, in the words a person would use.
var queueStateWords = map[string]string{
	queue.StateQueued.String():   "queued",
	queue.StateClaimed.String():  "in progress",
	queue.StateWaiting.String():  "waiting for plan approval",
	queue.StateApproved.String(): "approved, runs next",
	queue.StateFinished.String(): "finished",
	queue.StateFailed.String():   "failed",
	queue.StateStopped.String():  "stopped",
	queue.StatePaused.String():   "paused at a limit",
	queue.StateRemoved.String():  "removed",
}

// renderQueueTable writes the goals a person has not removed as a table, in
// the order belay runs them.
func renderQueueTable(u *ui, goals []QueueGoal) {
	shown := visibleGoals(goals)
	cols := []runsColumn{
		{header: "GOAL", right: true},
		{header: "STATE", emph: true},
		{header: "SPENT", right: true, emph: true},
		{header: "RUN"},
		{header: "WHAT YOU ASKED FOR"},
	}
	for _, g := range shown {
		spent := runsMissing
		if g.RunID != "" {
			spent = runsSpent(RunCost{SpentUSD: g.SpentUSD, Estimated: g.Estimated})
		}
		asked := runsTruncate(sanitizeRunsText(g.Goal), runsGoalWidth)
		if g.NoApproval {
			asked += " (--no-approval)"
		}
		cols[0].cells = append(cols[0].cells, strconv.Itoa(g.ID))
		cols[1].cells = append(cols[1].cells, queueStateWords[g.State])
		cols[2].cells = append(cols[2].cells, spent)
		cols[3].cells = append(cols[3].cells, runsOrMissing(g.RunID))
		cols[4].cells = append(cols[4].cells, asked)
	}

	widths := make([]int, len(cols))
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = c.header
		widths[i] = runsRuneLen(c.header)
		for _, cell := range c.cells {
			widths[i] = max(widths[i], runsRuneLen(cell))
		}
	}
	u.say("%s", runsLine(u, cols, widths, header, false))
	for row := range shown {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = c.cells[row]
		}
		u.say("%s", runsLine(u, cols, widths, cells, true))
	}
}

// showQueueStatus is `belay queue status`, and `belay queue` on its own: the
// report a person reads when they come back to a queue.
func showQueueStatus(cmd *cobra.Command) error {
	u := newUI(cmd)
	ws, store, err := openWorkspaceQueue()
	if err != nil {
		return failed(err)
	}
	q, err := store.Read()
	if err != nil {
		return failed(err)
	}
	goals := queueGoals(q)
	pid, active, err := store.WorkerActive()
	if err != nil {
		return failed(err)
	}
	if len(visibleGoals(goals)) == 0 && q.Last.At.IsZero() {
		u.say(`The queue in %s is empty. Add a goal with: belay queue add "what you want built"`, ws)
		return nil
	}

	u.say("The queue in %s", ws)
	if active {
		u.say("  right now:   belay queue start is working through it (pid %d)", pid)
	} else {
		u.say("  right now:   nothing is working through it")
	}
	u.say("  last start:  %s", lastStartText(q.Last, active))
	u.say("  goals:       %s", goalCounts(goals))
	u.say("  spent:       %s", u.emph(queueSpent(goals)))
	u.say("")
	if len(visibleGoals(goals)) > 0 {
		renderQueueTable(u, goals)
	}
	sayQueueNextSteps(u, q, active, pid)
	return nil
}

// lastStartText says when the last `belay queue start` ran and how it ended.
func lastStartText(last queue.Start, active bool) string {
	if last.At.IsZero() {
		return "never: nothing has worked through this queue yet"
	}
	began := "began " + clock(last.At)
	switch {
	case active && !last.Ended:
		return began + ", and is still working"
	case !last.Ended:
		return began + " and was cut off before it could record stopping, by a crash, a kill or a reboot; " +
			"belay queue start carries on where it stopped"
	case last.Stop == queue.StopDrained:
		return began + " and ended " + clock(last.EndedAt) + ", with nothing left it could run"
	case last.Stop == queue.StopInterrupted:
		return began + " and was stopped " + clock(last.EndedAt) + ", by Ctrl-C or SIGTERM"
	default:
		return began + " and could not go on, " + clock(last.EndedAt) + ": " + sanitizeRunsText(last.Note)
	}
}

// clock renders a time the way the queue's report does: UTC, to the minute.
func clock(t time.Time) string { return t.UTC().Format("2006-01-02 15:04Z") }

// goalCounts says how many goals stand where, leaving out where none do.
func goalCounts(goals []QueueGoal) string {
	order := []queue.State{
		queue.StateClaimed, queue.StateApproved, queue.StateQueued, queue.StateWaiting,
		queue.StatePaused, queue.StateFinished, queue.StateFailed, queue.StateStopped,
	}
	counts := map[string]int{}
	for _, g := range goals {
		counts[g.State]++
	}
	var parts []string
	for _, s := range order {
		if n := counts[s.String()]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, queueStateWords[s.String()]))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// queueSpent sums what the goals' runs have spent.
func queueSpent(goals []QueueGoal) string {
	var cost RunCost
	runs := 0
	for _, g := range goals {
		if g.RunID == "" {
			continue
		}
		runs++
		cost.SpentUSD += g.SpentUSD
		cost.Estimated = cost.Estimated || g.Estimated
	}
	return fmt.Sprintf("%s across %s", runsSpent(cost), runsPlural(runs, "run", "runs"))
}

// sayQueueNextSteps writes the exact commands that move each goal on.
func sayQueueNextSteps(u *ui, q queue.Queue, active bool, pid int) {
	lines := queueNextSteps(q, active, pid)
	if len(lines) == 0 {
		return
	}
	u.say("")
	u.say("next:")
	for _, line := range lines {
		u.say("  %s", line)
	}
}

// queueNextSteps is the wayfinding for a queue: for each goal that needs a
// person, what to do about it, written out so nobody has to reconstruct it.
//
// A goal waiting for its plan, or stopped at a limit, needs a person however
// long ago that happened, so it is always named. A goal that finished, failed
// or stopped is named only if the last start is what ended it: those are the
// results a person comes back to read, and a queue used for weeks would
// otherwise repeat every goal it ever finished each time. The list keeps them
// all.
func queueNextSteps(q queue.Queue, active bool, pid int) []string {
	var lines []string
	for _, e := range q.Entries {
		resume := "belay --workspace " + shellWord(e.Dir) + " resume " + e.RunID
		timeline := "belay --workspace " + shellWord(e.Dir) + " timeline " + e.RunID
		ended := e.State == queue.StateFinished || e.State == queue.StateFailed || e.State == queue.StateStopped
		if ended && e.UpdatedAt.Before(q.Last.At) {
			continue
		}
		switch e.State {
		case queue.StateWaiting:
			plan := filepath.Join(e.Dir, ".belay", "runs", e.RunID, "artifacts", "plan.md")
			lines = append(lines,
				fmt.Sprintf("goal %d's plan is at %s", e.ID, plan),
				fmt.Sprintf("  read it, edit it if you want to, then:  belay queue approve %d", e.ID))
		case queue.StatePaused:
			lines = append(lines,
				fmt.Sprintf("goal %d stopped at one of belay's limits: %s", e.ID, sanitizeRunsText(e.Note)),
				fmt.Sprintf("  raise it in %s, then carry the goal on yourself:  %s",
					filepath.Join(e.Dir, config.ProjectFile), resume))
		case queue.StateFailed, queue.StateStopped:
			lines = append(lines, fmt.Sprintf("see what happened to goal %d:  %s", e.ID, timeline))
		case queue.StateFinished:
			finished := fmt.Sprintf("goal %d finished in its copy, %s; take what you want from it by hand", e.ID, e.Dir)
			if where, fanned := winnerWorkspace(e); fanned {
				finished = fmt.Sprintf("goal %d finished; its change is %s; take what you want from it by hand", e.ID, where)
			}
			lines = append(lines, finished, fmt.Sprintf("  see what it did:  %s", timeline))
		}
	}
	if len(runnable(q)) > 0 {
		if active {
			lines = append(lines, fmt.Sprintf("belay queue start (pid %d) is working through the rest now", pid))
		} else {
			lines = append(lines, "work through the rest with:  belay queue start")
		}
	}
	return lines
}

// winnerWorkspace says where a finished goal's change is when its run fanned
// out, and reports whether it did.
//
// A goal's copy is where its change is, except when its run fans out. Then
// the copy is as it was claimed: each candidate worked in a workspace of its
// own, under the run's candidates directory, and belay does not merge a run
// that edits in place -- which every queued goal's run does -- so the change
// stayed in the winner's. A person sent to the copy to take the change by
// hand would find nothing there. The answer is read from the goal's run, its
// manifest's fanout pin and its blackboard's winner, as completedFanoutSteps
// reads them for a run started by hand.
func winnerWorkspace(e queue.Entry) (string, bool) {
	man, ok := goalManifest(e)
	if !ok || man.Fanout == nil {
		return "", false
	}
	layout, err := state.NewLayout(e.Dir, e.RunID)
	if err != nil {
		return "", false
	}
	st, err := state.LoadState(layout.StatePath())
	if err != nil || st.Winner == "" {
		// A finished fanout always has a winner: join fails a run with
		// none. Not finding one, belay says where to look, not what to guess.
		return "in its winning candidate's own workspace, which belay timeline names", true
	}
	winner := state.Candidate{ID: st.Winner}
	for _, c := range st.Candidates {
		if c.ID == st.Winner {
			winner = c
		}
	}
	return fmt.Sprintf("in the workspace of %s, the candidate that won: %s", winner.ID, dirOrUnrecorded(winner.Dir)), true
}

// runnable lists the goals a start would run, in the order it runs them: the
// goal in hand, then the approved ones, then the queued ones (queue.Next).
func runnable(q queue.Queue) []queue.Entry {
	var out []queue.Entry
	for _, s := range []queue.State{queue.StateClaimed, queue.StateApproved, queue.StateQueued} {
		for _, e := range q.Entries {
			if e.State == s {
				out = append(out, e)
			}
		}
	}
	return out
}

// goalNumber reads a goal number off the command line.
func goalNumber(arg string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a goal number; belay queue list shows them", arg)
	}
	return id, nil
}

// removeFromQueue is `belay queue rm`.
func removeFromQueue(cmd *cobra.Command, arg string) error {
	u := newUI(cmd)
	id, err := goalNumber(arg)
	if err != nil {
		return failed(err)
	}
	_, store, err := openWorkspaceQueue()
	if err != nil {
		return failed(err)
	}
	removed, err := store.Remove(id)
	var wrong *queue.StateError
	switch {
	case errors.Is(err, queue.ErrNoSuchGoal):
		return failed(fmt.Errorf("the queue has no goal %d; belay queue list shows the goals it has", id))
	case errors.As(err, &wrong) && wrong.State == queue.StateRemoved:
		return failed(fmt.Errorf("goal %d was already removed", id))
	case errors.As(err, &wrong):
		started := goalOrZero(store, id)
		return failed(fmt.Errorf("goal %d has started, so it cannot be taken out of the queue: "+
			"it is %s, and its run %s is in %s", id, queueStateWords[wrong.State.String()], started.RunID, started.Dir))
	case err != nil:
		return failed(err)
	}
	u.say("removed goal %d from the queue: %s", removed.ID, removed.Goal)
	return nil
}

// goalOrZero reads goal id for a message, or returns the zero Entry.
func goalOrZero(store *queue.Store, id int) queue.Entry {
	q, err := store.Read()
	if err != nil {
		return queue.Entry{}
	}
	e, _ := q.Entry(id)
	return e
}

// approveQueuedGoal is `belay queue approve`.
//
// It records the approval in the goal's run first, the way `belay resume`
// does, and in the queue second. The gate reads the run's approval; the
// queue's record only sends the next worker to the gate. In the other order,
// a failure in between would send a worker to a gate with nothing approved,
// which would park the goal again. In this order, a failure in between leaves
// a run approved and a goal still waiting, and approving it again, which is
// harmless for the run, finishes the job.
func approveQueuedGoal(cmd *cobra.Command, arg string) error {
	u := newUI(cmd)
	id, err := goalNumber(arg)
	if err != nil {
		return failed(err)
	}
	_, store, err := openWorkspaceQueue()
	if err != nil {
		return failed(err)
	}
	q, err := store.Read()
	if err != nil {
		return failed(err)
	}
	e, ok := q.Entry(id)
	switch {
	case !ok:
		return failed(fmt.Errorf("the queue has no goal %d; belay queue list shows the goals it has", id))
	case e.State != queue.StateWaiting:
		return failed(fmt.Errorf("goal %d is not waiting for its plan to be approved: it is %s",
			id, queueStateWords[e.State.String()]))
	}

	layout, err := state.NewLayout(e.Dir, e.RunID)
	if err != nil {
		return failed(err)
	}
	asking, err := runAsksForApproval(layout)
	if err != nil {
		return failed(err)
	}
	if asking {
		// Preflight first, as recordApproval does: the approval is the first
		// thing written to this run since it paused (ADR-0011).
		run := state.NewStore(layout)
		if err := run.Preflight(); err != nil {
			return failed(err)
		}
		if _, err := approvePlan(run); err != nil {
			return failed(err)
		}
	}
	if _, err := store.Approve(id); err != nil {
		return failed(err)
	}

	u.say("approved goal %d's plan: %s", id, e.Goal)
	if pid, active, err := store.WorkerActive(); err == nil && active {
		u.say("  belay queue start (pid %d) carries it on after the goal it is on now.", pid)
		return nil
	}
	u.say("  carry it on with:  belay queue start")
	return nil
}

// runAsksForApproval reports whether the run at layout is paused at the
// approve gate because the gate asked about its plan. A run carried on by
// hand since it was parked may not be, and then there is nothing for the
// run to record.
func runAsksForApproval(layout state.Layout) (bool, error) {
	man, err := state.LoadManifest(layout.ManifestPath())
	if err != nil {
		return false, fmt.Errorf("cannot read the goal's run: %w", err)
	}
	return awaitsApproval(resumableRun{status: man.Status, node: man.CurrentNode}, layout)
}

// startTheQueue is `belay queue start`.
func startTheQueue(cmd *cobra.Command) error {
	u := newUI(cmd)
	ws, store, err := openWorkspaceQueue()
	if err != nil {
		return failed(err)
	}
	q, err := store.Read()
	if err != nil {
		return failed(err)
	}
	toRun := runnable(q)
	if len(toRun) == 0 {
		u.say("There is nothing in the queue in %s that belay can run.", ws)
		sayQueueNextSteps(u, q, false, 0)
		return nil
	}

	cassette := cassettePath(ws, runFlags.cassette)
	plan, err := resolve(resolveRequest{
		workspace:  ws,
		configPath: rootFlags.configPath,
		cassette:   cassette,
		logger:     newLogger(u),
		context:    cmd.Context(),
		override:   queueOverride(false),
	})
	if err != nil {
		return failed(err)
	}
	refuseUnwatchedSpend(plan)
	if pid, active, err := store.WorkerActive(); err == nil && active {
		plan.block(fmt.Sprintf("another belay queue start is already working through this queue (pid %d).", pid),
			"wait for it to finish, or stop it with Ctrl-C in its terminal; belay queue status says how it is getting on")
	}

	if rootFlags.dryRun {
		u.say("This is a dry run. belay is only describing the work; none of it has happened.")
		u.say("")
		announceQueue(u, plan, ws, store, toRun, "would work")
		if len(plan.blockers) > 0 {
			return refuseQueue(u, plan, "belay would refuse to work through the queue.")
		}
		u.say("Nothing was created and nothing was spent. Run the same command without --dry-run to start.")
		return nil
	}
	if len(plan.blockers) > 0 {
		return refuseQueue(u, plan, "belay will not work through the queue in "+ws+".")
	}
	announceQueue(u, plan, ws, store, toRun, "is working")

	ctx, stop := stopOnSignal(cmd.Context(), u)
	defer stop()
	w, err := newQueueWorker(u, ws, store, newQueueDriver(u, ws, rootFlags.configPath, cassette))
	if err != nil {
		return failed(err)
	}
	return workQueue(ctx, u, w)
}

// refuseQueue reports every reason belay will not work through the queue,
// together, and stops, as refuseToStart does for a run.
func refuseQueue(u *ui, plan *runPlan, headline string) error {
	u.say("%s", headline)
	for _, b := range plan.blockers {
		u.say("")
		u.say("  %s", b.problem)
		u.say("  to fix it: %s", b.fix)
	}
	u.say("")
	u.say("Nothing was created and nothing was spent.")
	// Everything is said, so Execute prints nothing more.
	return &ExitError{Code: graph.ExitFailed}
}

// refuseUnwatchedSpend blocks a queue whose ceiling would not stop a goal.
//
// budget.on_exceed "warn" records an overrun and keeps going. At a terminal a
// person can see it happen and stop it; a queue runs with nobody watching,
// goal after goal, so the warning would reach nobody until the money was
// gone. The per-goal ceiling is the queue's only guard on spend.
func refuseUnwatchedSpend(plan *runPlan) {
	if plan.config.Budget.OnExceed != config.OnExceedWarn {
		return
	}
	plan.block(`budget.on_exceed is "warn", so a goal that reached budget.max_usd would keep spending, `+
		`and nobody would be watching to stop it.`,
		`set budget.on_exceed to "abort" in belay.yaml, so each goal stops at its ceiling`)
}

// queueOverride is what the queue decides about every goal's run, whatever
// belay.yaml says (see resolveRequest.override).
//
// The run works in place, in its copy: the copy is the run's isolation, and a
// branch run would look for a git repository from inside it, find the
// person's own a few directories up, and branch that. It files no plan into
// the repository, because the copy is not the repository, and a plan filed
// into a copy nobody keeps would be lost with it.
//
// And its plan stops for a person unless the goal was added with
// --no-approval: that is the one way a queued goal skips review, and the
// person chose it, for that goal. graph.approval is not consulted at all.
// Turned off in a belay.yaml, or in the user's own config.yaml, which reaches
// every repository they own, it would take every goal from its plan to its
// code with nobody watching -- belay removing the gate on its own, which a
// queue never does. A person at a terminal who turns it off for `belay run`
// is there to watch the run; a queue's goals run while they are away.
func queueOverride(noApproval bool) func(*config.Config) {
	return func(c *config.Config) {
		c.Output.Mode = config.OutputInPlace
		c.Plan.Archive = false
		c.Graph.Approval = !noApproval
	}
}

// announceQueue says what a start will do and the most it can spend, before
// anything is spent. verb says whether it is about to ("is working") or a dry
// run is describing it ("would work").
func announceQueue(u *ui, plan *runPlan, ws string, store *queue.Store, toRun []queue.Entry, verb string) {
	ids := make([]int, len(toRun))
	var stops []int
	for i, e := range toRun {
		ids[i] = e.ID
		if e.State == queue.StateQueued && !e.NoApproval {
			stops = append(stops, e.ID)
		}
	}
	each := plan.config.Budget.MaxUSD
	n := len(toRun)

	u.say("belay %s through the queue in %s", verb, ws)
	if n == 1 {
		u.say("  goals to run:         goal %d", ids[0])
	} else {
		u.say("  goals to run:         %d goals, in this order: %s", n, goalList(ids))
	}
	u.say("  most each can spend:  %s, budget.max_usd, on its own run", dollars(each))
	u.say("  most it can spend:    %s in all, %d × %s, and none of it is spent yet",
		u.emph(dollars(float64(n)*each)), n, dollars(each))
	switch {
	case len(stops) == 1:
		u.say("  plans to approve:     goal %d stops at its plan for you; the queue parks it and moves on", stops[0])
	case len(stops) > 1:
		u.say("  plans to approve:     goals %s stop at their plans for you; the queue parks each one and moves on",
			goalList(stops))
	default:
		u.say("  plans to approve:     none")
	}
	u.say("  where each one runs:  in its own copy of this workspace, under %s; your checkout is never edited",
		store.WorkRoot())
	u.say("  to stop it:           press Ctrl-C once; the goal in progress pauses where it stands")
	u.say("")
}

// goalList joins goal numbers as a sentence does: "1", "1 and 3", "1, 2 and 5".
func goalList(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	if len(parts) <= 1 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// queueDriver is the queue.Driver `belay queue start` hands its worker: it
// makes, reads and drives each goal's run the way `belay run` and `belay
// resume` do, in the goal's copy.
type queueDriver struct {
	u          *ui
	source     string
	configPath string
	cassette   string
	// adjust, when set, changes each goal's plan once it is final: resolved,
	// and for a run being driven, bound to the run's pins, which rebuilds
	// its registry. Tests use it to give a goal's run a graph of fake nodes;
	// nothing else sets it.
	adjust func(*runPlan)
}

var _ queue.Driver = (*queueDriver)(nil)

// newQueueDriver returns the driver for the queue of source. cassette is the
// recording every goal's run answers from or records to, which is the
// workspace's own, not one in the goal's copy.
func newQueueDriver(u *ui, source, configPath, cassette string) *queueDriver {
	return &queueDriver{u: u, source: source, configPath: configPath, cassette: cassette}
}

// plan resolves goal e's run in its copy, with the queue's override on top.
// Each goal's run reads the configuration in its copy, which is the
// workspace's as it stood when the goal was claimed.
func (d *queueDriver) plan(ctx context.Context, e queue.Entry) (*runPlan, error) {
	plan, err := resolve(resolveRequest{
		workspace:  e.Dir,
		configPath: d.configPath,
		cassette:   d.cassette,
		logger:     newLogger(d.u),
		context:    ctx,
		override:   queueOverride(e.NoApproval),
	})
	if err != nil {
		return nil, err
	}
	plan.goal = e.Goal
	return plan, nil
}

// adjusted applies the test seam adjust to a goal's final plan.
func (d *queueDriver) adjusted(plan *runPlan) {
	if d.adjust != nil {
		d.adjust(plan)
	}
}

// Observe implements queue.Driver.
func (d *queueDriver) Observe(e queue.Entry) (queue.RunView, error) {
	layout, err := state.NewLayout(e.Dir, e.RunID)
	if err != nil {
		return queue.RunView{}, err
	}
	man, err := state.LoadManifest(layout.ManifestPath())
	if errors.Is(err, fs.ErrNotExist) {
		return queue.RunView{}, nil
	}
	if err != nil {
		return queue.RunView{}, err
	}
	view := queue.RunView{Exists: true, Status: man.Status}
	if man.Status == state.RunStatusPaused && man.CurrentNode == graph.NodeApprove {
		if view.GateAsked, err = gateAsked(layout); err != nil {
			return queue.RunView{}, err
		}
	}
	return view, nil
}

// Create implements queue.Driver: it makes goal e's run in its copy, as
// `belay run` makes one, under the run id the goal's claim recorded.
// createRun refuses a run that already exists.
func (d *queueDriver) Create(ctx context.Context, e queue.Entry) error {
	plan, err := d.plan(ctx, e)
	if err != nil {
		return err
	}
	d.adjusted(plan)
	if err := blocked(plan); err != nil {
		return err
	}
	_, err = createRun(ctx, plan, runSpec{Workspace: e.Dir, RunID: e.RunID, Goal: e.Goal})
	return err
}

// Drive implements queue.Driver: it continues goal e's run once, as `belay
// resume` does, and settles nothing itself.
//
// It dispatches through dispatchOnce with pauseOnCancel, so a Ctrl-C pauses
// the run where it stands rather than ending it, and calls afterCompletion,
// so a recording is saved whatever happened, as for any run. It always
// resumes: a new run's journal is empty and a crashed one's is open, and
// Resume treats both as Run would, while a run paused by a Ctrl-C can only be
// continued by Resume. Resuming a run whose gate asked is safe too, because
// it never approves anything: the gate asks again unless the run's own
// state says the plan was approved, which only `belay queue approve` writes.
func (d *queueDriver) Drive(ctx context.Context, e queue.Entry) (queue.Outcome, error) {
	plan, err := d.plan(ctx, e)
	if err != nil {
		return queue.Outcome{}, err
	}
	layout, err := state.NewLayout(e.Dir, e.RunID)
	if err != nil {
		return queue.Outcome{}, err
	}
	store, err := plan.openRun(ctx, layout)
	if err != nil {
		return queue.Outcome{}, err
	}
	d.adjusted(plan)
	if err := blocked(plan); err != nil {
		return queue.Outcome{}, err
	}

	d.u.say("")
	d.u.say("goal %d: %s", e.ID, e.Goal)
	d.u.say("  run:   %s, in %s", e.RunID, e.Dir)
	sayWarnings(d.u, plan)
	view, err := dispatchOnce(ctx, plan, store, true, pauseOnCancel)
	if err != nil {
		return queue.Outcome{}, err
	}
	// afterCompletion is given a context nobody has cancelled, as driveRun
	// gives it, so a Ctrl-C does not cost the recording.
	afterCompletion(context.WithoutCancel(ctx), d.u, plan, &view)

	out, err := goalOutcome(view)
	if err != nil {
		return queue.Outcome{}, err
	}
	d.sayOutcome(e, out)
	return out, nil
}

// blocked turns a plan's blockers into the error a worker stops on.
func blocked(plan *runPlan) error {
	if len(plan.blockers) == 0 {
		return nil
	}
	problems := make([]string, len(plan.blockers))
	for i, b := range plan.blockers {
		problems[i] = b.problem + " (to fix it: " + b.fix + ")"
	}
	return errors.New("belay will not start this goal's run: " + strings.Join(problems, "; "))
}

// goalOutcome says how a goal settles, given how one dispatch of its run
// ended.
func goalOutcome(view outcomeView) (queue.Outcome, error) {
	outcome := view.outcome
	if errors.Is(view.runErr, graph.ErrInterrupted) {
		return queue.Outcome{Interrupted: true}, nil
	}
	note := sanitizeRunsText(outcome.Note)
	switch outcome.Status {
	case state.RunStatusCompleted:
		return queue.Outcome{State: queue.StateFinished, Note: note}, nil
	case state.RunStatusFailed:
		return queue.Outcome{State: queue.StateFailed, Note: note}, nil
	case state.RunStatusAborted:
		return queue.Outcome{State: queue.StateStopped, Note: note}, nil
	case state.RunStatusPaused:
		asked, err := gateAsked(view.layout)
		if err != nil {
			return queue.Outcome{}, err
		}
		if asked && outcome.Node == graph.NodeApprove {
			return queue.Outcome{State: queue.StateWaiting,
				Note: "the plan is at " + filepath.Join(view.layout.ArtifactsDir(), "plan.md")}, nil
		}
		return queue.Outcome{State: queue.StatePaused, Note: note}, nil
	default:
		// The dispatch returned without the run reaching a state of its
		// own, which it does only when it could not write one: the goal
		// stays in hand, and the next start tries it again.
		return queue.Outcome{}, fmt.Errorf("its run stopped without an outcome: %w", view.runErr)
	}
}

// sayOutcome tells the person how one goal's dispatch ended.
func (d *queueDriver) sayOutcome(e queue.Entry, out queue.Outcome) {
	switch {
	case out.Interrupted:
		d.u.say("goal %d is paused where it stood; belay queue start carries it on", e.ID)
	case out.State == queue.StateWaiting:
		d.u.say("goal %d is waiting for you to approve its plan; %s", e.ID, out.Note)
		d.u.say("  the queue moves on; approve it later with:  belay queue approve %d", e.ID)
	case out.State == queue.StateFinished:
		if where, fanned := winnerWorkspace(e); fanned {
			d.u.say("goal %d finished; its change is %s", e.ID, where)
			return
		}
		d.u.say("goal %d finished, in its copy %s", e.ID, e.Dir)
	case out.State == queue.StatePaused:
		d.u.say("goal %d stopped at one of belay's limits: %s", e.ID, out.Note)
	default:
		d.u.say("goal %d %s: %s", e.ID, queueStateWords[out.State.String()], out.Note)
	}
}

// narratingCopier is dircopy's isolator, saying so as it copies: a copy of a
// large workspace takes a while, and silence then reads as a hang.
type narratingCopier struct {
	u   *ui
	iso *dircopy.Isolator
}

// Create implements queue.Copier.
func (c narratingCopier) Create(ctx context.Context, src, id string) (belay.Workspace, error) {
	c.u.say("")
	c.u.say("copying %s for the next goal's run, %s", src, id)
	return c.iso.Create(ctx, src, id)
}

// Reclaim implements queue.Copier.
func (c narratingCopier) Reclaim(ctx context.Context, id string) error { return c.iso.Reclaim(ctx, id) }

// newQueueWorker builds the worker `belay queue start` works through the
// queue with: copies made by dircopy under the queue's work root, with its
// skip list, and runs made and driven by driver.
func newQueueWorker(u *ui, workspace string, store *queue.Store, driver *queueDriver) (*queue.Worker, error) {
	iso, err := dircopy.New(store.WorkRoot(), dircopy.WithLogger(newLogger(u)))
	if err != nil {
		return nil, err
	}
	return &queue.Worker{
		Store: store, Source: workspace, Copier: narratingCopier{u: u, iso: iso}, Driver: driver,
		Logger: newLogger(u),
	}, nil
}

// workQueue works through the queue with w, then reports, and turns how it
// stopped into the exit status: 0 once nothing is left it can run, whatever
// became of each goal, which the report says; ExitAborted after a Ctrl-C, as
// for a run; ExitFailed when it could not go on.
func workQueue(ctx context.Context, u *ui, w *queue.Worker) error {
	report, err := w.Work(ctx)
	var active *queue.ActiveError
	if errors.As(err, &active) {
		return failed(fmt.Errorf("another belay queue start is already working through this queue (pid %d); "+
			"belay queue status says how it is getting on", active.PID))
	}

	q, readErr := w.Store.Read()
	u.say("")
	switch {
	case err != nil:
		u.say("belay could not go on working through the queue: %v", err)
		u.say("The goal it had in hand stays in hand, and belay queue start tries it again.")
	case report.Stop == queue.StopInterrupted:
		u.say("belay stopped working through the queue because you asked it to. The goal in progress is paused where it stood.")
	default:
		u.say("belay worked through the queue, and nothing is left it can run.")
	}
	if report.PreviousCutOff {
		u.say("  the start before this one was cut off, by a crash, a kill or a reboot; this one carried on from where it stopped.")
	}
	if readErr == nil {
		goals := queueGoals(q)
		u.say("  goals:  %s", goalCounts(goals))
		u.say("  spent:  %s", u.emph(queueSpent(goals)))
		sayQueueNextSteps(u, q, false, 0)
	}

	switch {
	case err != nil:
		return &ExitError{Code: graph.ExitFailed, Err: err}
	case report.Stop == queue.StopInterrupted:
		return &ExitError{Code: graph.ExitAborted, Err: graph.ErrInterrupted}
	default:
		return nil
	}
}

// sayQueueRuns is what `belay runs` says, after its table, about the runs
// the queue's goals have. Each lives in its goal's copy of the workspace, not
// in this workspace's .belay/runs, so the listing above does not show it, and
// a person who queued goals and came back for them would otherwise find
// nothing.
//
// Each run gets one line: the command that lists it, and which goal it is and
// where that goal stands. It reads the queue and never writes it, says
// nothing about a queue it cannot read or one with no runs, and is not part
// of --json, whose shape is documented and does not change for these runs.
func sayQueueRuns(u *ui, workspace string) {
	store, err := queue.Open(workspace)
	if err != nil {
		return
	}
	q, err := store.Read()
	if err != nil {
		return
	}
	var lines []string
	for _, e := range q.Entries {
		if _, ok := goalManifest(e); !ok {
			continue
		}
		lines = append(lines, fmt.Sprintf("  belay --workspace %s runs    goal %d, %s",
			shellWord(e.Dir), e.ID, queueStateWords[e.State.String()]))
	}
	if len(lines) == 0 {
		return
	}
	u.say("")
	if len(lines) == 1 {
		u.say("A queued goal's run lives in a copy of this workspace. List it with:")
	} else {
		u.say("%d queued goals' runs live in copies of this workspace. List one with:", len(lines))
	}
	for _, line := range lines {
		u.say("%s", line)
	}
}

// shellWord returns s as a shell reads it back unchanged: as it is when it
// holds nothing a shell would act on, and in single quotes otherwise. Every
// command the queue writes out is meant to be pasted, and a workspace on
// macOS is as likely as not to have a space in its path.
//
// It is slice 3c's shellQuote under another name. The two lanes were built
// side by side, and one name in both would not compile once they meet; the
// first change to touch both after the merge should keep one.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:@%+=,-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
