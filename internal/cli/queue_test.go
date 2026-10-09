package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/queue"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestCreateRunRefusesARunThatAlreadyExists pins the guard the queue needs
// from createRun. Every earlier caller asked state.NewRunID for a fresh id, so
// the run it named could not exist yet. The queue is the first caller that
// supplies its own id -- it records the id in its log before the run exists,
// so a crash can find the run again -- and a caller that remembers an id can
// also be wrong about whether the run behind it was made. Creating a run whose
// manifest is already there would start it over: a fresh blackboard over its
// history, its spend reset to nothing, its approval forgotten. So createRun
// refuses, and touches neither file.
func TestCreateRunRefusesARunThatAlreadyExists(t *testing.T) {
	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const runID = "20260905T120000Z-abcdef123456"
	store, err := createRun(t.Context(), plan, runSpec{Workspace: ws, RunID: runID, Goal: "add tests"})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}

	// The run has moved on since it was made: its plan is approved and it
	// has spent money.
	blackboard, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	blackboard.Plan.Approved = true
	if err := store.SaveState(blackboard); err != nil {
		t.Fatalf("save state: %v", err)
	}
	manifest, err := store.LoadManifest()
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.Step = 4
	manifest.Budget.SpentUSD = 1.25
	if err := store.SaveManifest(manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	layout := store.Layout()
	before := map[string]string{}
	for _, path := range []string{layout.StatePath(), layout.ManifestPath()} {
		before[path] = readFile(t, path)
	}

	_, err = createRun(t.Context(), plan, runSpec{Workspace: ws, RunID: runID, Goal: "something else"})
	if !errors.Is(err, errRunExists) {
		t.Fatalf("createRun over an existing run = %v, want an error wrapping errRunExists", err)
	}
	for path, want := range before {
		if got := readFile(t, path); got != want {
			t.Errorf("createRun refused, but changed %s:\nbefore: %s\nafter:  %s", path, want, got)
		}
	}
	if got, err := state.LoadManifest(layout.ManifestPath()); err != nil || got.Budget.SpentUSD != 1.25 {
		t.Errorf("the run's spend after the refusal = %v (error %v), want the $1.25 it had spent", got.Budget.SpentUSD, err)
	}
}

// TestResolveAppliesAnOverrideAndChecksIt: a queued goal's run is resolved
// like any other, from the configuration its copy holds, with the queue's
// own decisions on top -- in place, no archive, and no gate for a goal the
// person added with --no-approval. Those are applied before anything is
// built from the configuration, so the route a person is shown is the one
// the run walks, and the result is validated again, so an override can never
// produce a configuration belay.yaml itself could not.
func TestResolveAppliesAnOverrideAndChecksIt(t *testing.T) {
	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger(), override: func(c *config.Config) {
		c.Graph.Approval = false
		c.Plan.Archive = false
	}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.config.Graph.Approval || plan.config.Plan.Archive {
		t.Errorf("the override did not reach the plan's configuration: approval %v, archive %v",
			plan.config.Graph.Approval, plan.config.Plan.Archive)
	}
	if slices.Contains(plan.route, graph.NodeApprove) {
		t.Errorf("route = %v, want no approve gate: the route is built after the override", plan.route)
	}

	if _, err := resolve(resolveRequest{workspace: ws, logger: quietLogger(), override: func(c *config.Config) {
		c.Budget.MaxUSD = -1
	}}); err == nil {
		t.Error("resolve accepted an override that leaves budget.max_usd negative")
	}
}

// reaches stands in for the code node and everything after it, and notes
// which goal got there: the one thing a parked goal must never do.
type reaches struct{ goals *[]string }

func (reaches) Name() string { return graph.NodeCode }

func (n reaches) Run(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
	*n.goals = append(*n.goals, rc.Goal)
	return graph.Result{Status: journal.StatusOK, Next: graph.End, Note: "done"}, nil
}

// fakeGoals returns a registry of a plan writer, the real approve gate, and
// nodes, so a queued goal's run is real in every way that matters here --
// its manifest, journal, gate and pause -- except the agent.
func fakeGoals(nodes ...graph.Node) func() *graph.Registry {
	return func() *graph.Registry {
		r := graph.NewRegistry()
		r.MustRegister(append([]graph.Node{planWriter{body: "# Plan\n\nwrite a test\n"}, approve.New()}, nodes...)...)
		return r
	}
}

// startQueue does what `belay queue start` does once it has decided to
// start: build its worker, work through the queue and report. registry
// replaces the graph each goal's run would get, so no agent is called.
func startQueue(ctx context.Context, t *testing.T, ws string, registry func() *graph.Registry) (string, error) {
	t.Helper()
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	store := openQueue(t, ws)
	driver := newQueueDriver(u, ws, "", "")
	driver.adjust = func(p *runPlan) { p.registry = registry() }
	w, err := newQueueWorker(u, ws, store, driver)
	if err != nil {
		t.Fatalf("newQueueWorker: %v", err)
	}
	err = workQueue(ctx, u, w)
	t.Logf("verbatim output of the start:\n%s", out.String())
	return out.String(), err
}

// openQueue opens the queue of ws.
func openQueue(t *testing.T, ws string) *queue.Store {
	t.Helper()
	store, err := queue.Open(ws)
	if err != nil {
		t.Fatalf("queue.Open: %v", err)
	}
	return store
}

// goal reads goal id from the queue of ws.
func goal(t *testing.T, ws string, id int) queue.Entry {
	t.Helper()
	q, err := openQueue(t, ws).Read()
	if err != nil {
		t.Fatalf("read the queue: %v", err)
	}
	e, ok := q.Entry(id)
	if !ok {
		t.Fatalf("the queue has no goal %d: %+v", id, q.Entries)
	}
	return e
}

// queueAdd runs `belay queue add`, failing the test if it fails.
func queueAdd(t *testing.T, ws string, args ...string) string {
	t.Helper()
	out, _, err := runCLI(t, append([]string{"queue", "add", "--workspace", ws}, args...)...)
	if err != nil {
		t.Fatalf("belay queue add %v: %v\n%s", args, err, out)
	}
	return out
}

// runLayout returns the layout of goal e's run, in its copy.
func runLayout(t *testing.T, e queue.Entry) state.Layout {
	t.Helper()
	layout, err := state.NewLayout(e.Dir, e.RunID)
	if err != nil {
		t.Fatalf("state.NewLayout(%s, %s): %v", e.Dir, e.RunID, err)
	}
	return layout
}

// TestQueueAddPutsTheGoalAtTheEnd covers adding and listing: each goal gets
// the next id and says what will happen to its plan, and the listing --
// table and JSON -- shows the goals in the order they will run.
func TestQueueAddPutsTheGoalAtTheEnd(t *testing.T) {
	ws := goWorkspace(t)

	first := queueAdd(t, ws, "add tests for the parser")
	second := queueAdd(t, ws, "--no-approval", "add docs")
	t.Logf("verbatim output of the adds:\n%s%s", first, second)
	for _, phrase := range []string{"added goal 1", "nothing is ahead of it", "belay queue approve 1", "belay queue start"} {
		if !strings.Contains(first, phrase) {
			t.Errorf("adding the first goal never says %q:\n%s", phrase, first)
		}
	}
	for _, phrase := range []string{"added goal 2", "1 goal is ahead of it", "--no-approval"} {
		if !strings.Contains(second, phrase) {
			t.Errorf("adding the second goal never says %q:\n%s", phrase, second)
		}
	}
	if strings.Contains(second, "belay queue approve 2") {
		t.Errorf("a goal added with --no-approval was told to wait for an approval:\n%s", second)
	}

	table, _, err := runCLI(t, "queue", "list", "--workspace", ws)
	if err != nil {
		t.Fatalf("belay queue list: %v", err)
	}
	t.Logf("verbatim list:\n%s", table)
	if i, j := strings.Index(table, "add tests for the parser"), strings.Index(table, "add docs"); i < 0 || j < i {
		t.Errorf("the list does not show both goals in the order they run:\n%s", table)
	}

	raw, _, err := runCLI(t, "queue", "list", "--workspace", ws, "--json")
	if err != nil {
		t.Fatalf("belay queue list --json: %v", err)
	}
	var listing QueueListing
	if err := json.Unmarshal([]byte(raw), &listing); err != nil {
		t.Fatalf("belay queue list --json is not the documented shape: %v\n%s", err, raw)
	}
	if listing.Workspace != ws || len(listing.Goals) != 2 {
		t.Fatalf("listing = %+v, want both goals of %s", listing, ws)
	}
	if g := listing.Goals[1]; g.ID != 2 || g.Goal != "add docs" || !g.NoApproval || g.State != "queued" || g.RunID != "" {
		t.Errorf("goal 2 in JSON = %+v, want it queued, with no run, added with --no-approval", g)
	}
	for _, key := range []string{`"id"`, `"goal"`, `"no_approval"`, `"state"`, `"run_id"`, `"copy"`, `"spent_usd"`, `"note"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("belay queue list --json has no %s key; every documented key is always present:\n%s", key, raw)
		}
	}
}

// TestQueueRmTakesBackOnlyAGoalNothingStarted: rm removes a goal that is
// still waiting its turn, and says why it will not remove one that has
// started, or one that is not there.
func TestQueueRmTakesBackOnlyAGoalNothingStarted(t *testing.T) {
	ws := goWorkspace(t)
	queueAdd(t, ws, "add tests for the parser")
	queueAdd(t, ws, "add docs")

	out, _, err := runCLI(t, "queue", "rm", "--workspace", ws, "1")
	if err != nil || !strings.Contains(out, "removed goal 1") {
		t.Fatalf("belay queue rm 1 = (%v)\n%s", err, out)
	}
	if got := goal(t, ws, 1); got.State != queue.StateRemoved {
		t.Errorf("goal 1 is %s after rm, want removed", got.State)
	}
	list, _, err := runCLI(t, "queue", "list", "--workspace", ws)
	if err != nil || strings.Contains(list, "add tests for the parser") {
		t.Errorf("the list still shows the removed goal (%v):\n%s", err, list)
	}

	for _, tc := range []struct{ id, want string }{
		{id: "1", want: "removed"},
		{id: "9", want: "no goal 9"},
		{id: "two", want: "goal number"},
	} {
		_, _, err := runCLI(t, "queue", "rm", "--workspace", ws, tc.id)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("belay queue rm %s = %v, want a refusal saying %q", tc.id, err, tc.want)
		}
	}
}

// TestTheQueueParksAGoalAtItsPlanAndRunsTheNext is the crux of the queue,
// end to end through belay's own worker and dispatcher. A goal whose plan
// stops for review is parked and the queue moves on to the next; only the
// goal the person added with --no-approval skips the gate. Every goal runs in
// its own copy, in place, filing no plan, and the checkout gets no run of its
// own. Approving the parked goal and starting again continues that goal's
// own run past its gate.
func TestTheQueueParksAGoalAtItsPlanAndRunsTheNext(t *testing.T) {
	ws := goWorkspace(t)
	queueAdd(t, ws, "add tests for the parser")
	queueAdd(t, ws, "--no-approval", "add docs")
	var reached []string
	registry := fakeGoals(reaches{goals: &reached})

	out, err := startQueue(t.Context(), t, ws, registry)
	if err != nil {
		t.Fatalf("the first start: %v", err)
	}
	parked, done := goal(t, ws, 1), goal(t, ws, 2)
	if parked.State != queue.StateWaiting || done.State != queue.StateFinished {
		t.Fatalf("goal 1 is %s and goal 2 is %s, want goal 1 parked and goal 2 finished", parked.State, done.State)
	}
	if !slices.Equal(reached, []string{"add docs"}) {
		t.Errorf("goals that reached code = %v, want only the one added with --no-approval", reached)
	}
	for _, phrase := range []string{"goal 1", "waiting for you", "plan.md", "belay queue approve 1", "goal 2", "finished"} {
		if !strings.Contains(out, phrase) {
			t.Errorf("the start never says %q", phrase)
		}
	}

	work := filepath.Join(ws, ".belay", "queue", "work")
	for _, e := range []queue.Entry{parked, done} {
		if filepath.Dir(e.Dir) != work || filepath.Base(e.Dir) != e.RunID {
			t.Errorf("goal %d's copy is %s, want %s", e.ID, e.Dir, filepath.Join(work, e.RunID))
		}
		man, err := state.LoadManifest(runLayout(t, e).ManifestPath())
		if err != nil {
			t.Fatalf("goal %d's run has no manifest in its copy: %v", e.ID, err)
		}
		if man.Workspace != e.Dir || man.Goal != e.Goal {
			t.Errorf("goal %d's run works in %s on %q, want its copy %s and its goal", e.ID, man.Workspace, man.Goal, e.Dir)
		}
		if man.Output.Branched() || man.ArchivedPlan.Dir != "" {
			t.Errorf("goal %d's run pinned output %+v and archive %q, want in place and no archive",
				e.ID, man.Output, man.ArchivedPlan.Dir)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, ".belay", "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the checkout has a run of its own (stat error %v); every goal runs in its copy", err)
	}

	if out, _, err := runCLI(t, "queue", "approve", "--workspace", ws, "1"); err != nil {
		t.Fatalf("belay queue approve 1: %v\n%s", err, out)
	}
	blackboard, err := state.LoadState(runLayout(t, parked).StatePath())
	if err != nil || !blackboard.Plan.Approved {
		t.Fatalf("after belay queue approve the run's plan is approved = %v (%v), want true", blackboard.Plan.Approved, err)
	}
	if got := goal(t, ws, 1); got.State != queue.StateApproved {
		t.Errorf("goal 1 is %s after approve, want approved", got.State)
	}

	if _, err := startQueue(t.Context(), t, ws, registry); err != nil {
		t.Fatalf("the second start: %v", err)
	}
	if got := goal(t, ws, 1); got.State != queue.StateFinished || got.RunID != parked.RunID {
		t.Errorf("goal 1 after the approval = %+v, want it finished, in the run it parked in, %s", got, parked.RunID)
	}
	if !slices.Equal(reached, []string{"add docs", "add tests for the parser"}) {
		t.Errorf("goals that reached code = %v, want goal 1 to follow once approved", reached)
	}
}

// TestQueueApproveRecordsTheRunsApprovalFirst pins the order of approve's two
// writes. The approval the gate reads is the run's own; the queue's record
// only sends the next worker to it. Written the other way round, a failure
// in between would send a worker into a gate with nothing approved. Here the
// queue's log refuses the write: the run is approved, the goal still waits,
// and approving again once the log takes writes finishes the job.
func TestQueueApproveRecordsTheRunsApprovalFirst(t *testing.T) {
	ws := goWorkspace(t)
	queueAdd(t, ws, "add tests for the parser")
	var reached []string
	if _, err := startQueue(t.Context(), t, ws, fakeGoals(reaches{goals: &reached})); err != nil {
		t.Fatalf("start: %v", err)
	}
	parked := goal(t, ws, 1)
	log := openQueue(t, ws).LogPath()
	if err := os.Chmod(log, 0o400); err != nil {
		t.Fatalf("make the log read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(log, 0o600) })

	if _, _, err := runCLI(t, "queue", "approve", "--workspace", ws, "1"); err == nil {
		t.Fatal("belay queue approve succeeded with a log it cannot write to")
	}
	blackboard, err := state.LoadState(runLayout(t, parked).StatePath())
	if err != nil || !blackboard.Plan.Approved {
		t.Errorf("the run's plan approved = %v (%v), want true: the run's approval is written first", blackboard.Plan.Approved, err)
	}
	if got := goal(t, ws, 1); got.State != queue.StateWaiting {
		t.Errorf("goal 1 is %s, want it still waiting", got.State)
	}

	if err := os.Chmod(log, 0o600); err != nil {
		t.Fatalf("make the log writable: %v", err)
	}
	if out, _, err := runCLI(t, "queue", "approve", "--workspace", ws, "1"); err != nil {
		t.Fatalf("belay queue approve again: %v\n%s", err, out)
	}
	if got := goal(t, ws, 1); got.State != queue.StateApproved {
		t.Errorf("goal 1 is %s, want approved", got.State)
	}

	if _, _, err := runCLI(t, "queue", "approve", "--workspace", ws, "1"); err == nil ||
		!strings.Contains(err.Error(), "not waiting") {
		t.Errorf("approving an approved goal = %v, want a refusal saying it is not waiting", err)
	}
}

// TestCtrlCLeavesTheGoalInHandForTheNextStart: Ctrl-C during a goal pauses
// that goal's run where it stands, the start says how to carry on, and the
// next start continues the same run -- re-running the step that was cut
// short -- before it touches the next goal.
func TestCtrlCLeavesTheGoalInHandForTheNextStart(t *testing.T) {
	ws := goWorkspace(t)
	queueAdd(t, ws, "--no-approval", "add tests for the parser")
	queueAdd(t, ws, "--no-approval", "add docs")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out, err := startQueue(ctx, t, ws, fakeGoals(pressesCtrlC{name: graph.NodeCode, cancel: cancel}))
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != graph.ExitAborted {
		t.Fatalf("start = %v, want the exit code a Ctrl-C always has, %d", err, graph.ExitAborted)
	}
	if !strings.Contains(out, "belay queue start") {
		t.Errorf("after Ctrl-C the start never says how to carry on:\n%s", out)
	}
	first := goal(t, ws, 1)
	if first.State != queue.StateClaimed {
		t.Fatalf("goal 1 is %s after Ctrl-C, want it still in hand", first.State)
	}
	man, err := state.LoadManifest(runLayout(t, first).ManifestPath())
	if err != nil || man.Status != state.RunStatusPaused {
		t.Fatalf("goal 1's run is %v (%v), want paused where Ctrl-C stopped it", man.Status, err)
	}
	if got := goal(t, ws, 2); got.State != queue.StateQueued {
		t.Errorf("goal 2 is %s, want it untouched", got.State)
	}

	var reached []string
	if _, err := startQueue(t.Context(), t, ws, fakeGoals(reaches{goals: &reached})); err != nil {
		t.Fatalf("the next start: %v", err)
	}
	if got := goal(t, ws, 1); got.State != queue.StateFinished || got.RunID != first.RunID {
		t.Errorf("goal 1 = %+v, want it finished in the run Ctrl-C paused, %s", got, first.RunID)
	}
	if !slices.Equal(reached, []string{"add tests for the parser", "add docs"}) {
		t.Errorf("goals that reached code = %v, want goal 1 continued before goal 2", reached)
	}
	read, err := journal.ReadFile(runLayout(t, first).JournalPath())
	if err != nil {
		t.Fatalf("read goal 1's journal: %v", err)
	}
	plans := 0
	for _, rec := range read.Records {
		if rec.Event == journal.EventNodeStarted && rec.Node == graph.NodePlan {
			plans++
		}
	}
	if plans != 1 {
		t.Errorf("goal 1's plan ran %d times, want once: the steps that finished are not run again", plans)
	}
}

// TestQueueStartRefusesWhatItShouldNotStart: nothing is claimed, copied or
// spent when the queue cannot be worked through safely.
func TestQueueStartRefusesWhatItShouldNotStart(t *testing.T) {
	t.Run("a ceiling nobody watches", func(t *testing.T) {
		ws := goWorkspace(t)
		writeConfig(t, ws, "version: 1\nbudget:\n  max_usd: 5\n  on_exceed: warn\n")
		queueAdd(t, ws, "add tests for the parser")
		before := readFile(t, openQueue(t, ws).LogPath())

		out, _, err := runCLI(t, "queue", "start", "--workspace", ws)
		if err == nil || !strings.Contains(out, "budget.on_exceed") {
			t.Errorf("start with budget.on_exceed warn = %v, want a refusal naming the setting:\n%s", err, out)
		}
		assertNothingStarted(t, ws, before)
	})

	t.Run("another start working through the queue", func(t *testing.T) {
		ws := goWorkspace(t)
		queueAdd(t, ws, "add tests for the parser")
		lock, err := openQueue(t, ws).LockWorker()
		if err != nil {
			t.Fatalf("LockWorker: %v", err)
		}
		defer func() { _ = lock.Release() }()
		before := readFile(t, openQueue(t, ws).LogPath())

		out, _, err := runCLI(t, "queue", "start", "--workspace", ws)
		var exit *ExitError
		if !errors.As(err, &exit) || exit.Code != graph.ExitFailed || !strings.Contains(out, "already working through this queue") {
			t.Errorf("start while another holds the queue = %v, want a refusal saying so:\n%s", err, out)
		}
		assertNothingStarted(t, ws, before)
	})
}

// assertNothingStarted fails the test unless the queue's log is as it was and
// no copy was made.
func assertNothingStarted(t *testing.T, ws, before string) {
	t.Helper()
	if got := readFile(t, openQueue(t, ws).LogPath()); got != before {
		t.Errorf("the queue's log changed:\nbefore: %s\nafter:  %s", before, got)
	}
	if _, err := os.Stat(filepath.Join(ws, ".belay", "queue", "work")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a copy directory was made (stat error %v)", err)
	}
}

// TestQueueStartAnnouncesTheMostItCanSpend: before anything is spent, a start
// says how many goals it will run, the most each can spend, and the product,
// which is the most the whole queue can spend. A dry run says the same and
// creates nothing.
func TestQueueStartAnnouncesTheMostItCanSpend(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nbudget:\n  max_usd: 2.5\n")
	queueAdd(t, ws, "add tests for the parser")
	queueAdd(t, ws, "--no-approval", "add docs")
	queueAdd(t, ws, "upgrade the client")
	before := readFile(t, openQueue(t, ws).LogPath())

	out, _, err := runCLI(t, "queue", "start", "--workspace", ws, "--dry-run")
	if err != nil {
		t.Fatalf("belay queue start --dry-run: %v\n%s", err, out)
	}
	t.Logf("verbatim dry run:\n%s", out)
	for _, phrase := range []string{"3 goals", "$2.50", "3 × $2.50", "$7.50", "goals 1 and 3", "Nothing was created and nothing was spent"} {
		if !strings.Contains(out, phrase) {
			t.Errorf("the dry run never says %q", phrase)
		}
	}
	assertNothingStarted(t, ws, before)
}

// TestQueueStartWithNothingToRun: an empty queue, or one whose goals are all
// parked or ended, is not an error.
func TestQueueStartWithNothingToRun(t *testing.T) {
	ws := goWorkspace(t)
	out, _, err := runCLI(t, "queue", "start", "--workspace", ws)
	if err != nil || !strings.Contains(out, "nothing in the queue") {
		t.Errorf("start with an empty queue = %v, want it to say there is nothing to run:\n%s", err, out)
	}
}

// TestQueueStatusIsTheMorningReport: after a night's start, status says
// what happened to each goal, what was spent, and the exact commands that
// continue -- with no subcommand at all, too.
func TestQueueStatusIsTheMorningReport(t *testing.T) {
	ws := goWorkspace(t)
	queueAdd(t, ws, "add tests for the parser")
	queueAdd(t, ws, "--no-approval", "add docs")
	var reached []string
	if _, err := startQueue(t.Context(), t, ws, fakeGoals(reaches{goals: &reached})); err != nil {
		t.Fatalf("start: %v", err)
	}
	finished := goal(t, ws, 2)

	for _, args := range [][]string{{"queue", "status"}, {"queue"}} {
		out, _, err := runCLI(t, append(args, "--workspace", ws)...)
		if err != nil {
			t.Fatalf("belay %v: %v", args, err)
		}
		t.Logf("verbatim belay %v:\n%s", args, out)
		for _, phrase := range []string{
			"1 waiting for plan approval", "1 finished",
			"belay queue approve 1",
			"belay --workspace " + finished.Dir + " timeline " + finished.RunID,
			"nothing left it could run",
		} {
			if !strings.Contains(out, phrase) {
				t.Errorf("belay %v never says %q", args, phrase)
			}
		}
	}
}

// TestBelayRunsPointsAtRunsInQueueCopies: a queued goal's run lives in its
// copy, so `belay runs` in the checkout does not list it. Its JSON is what it
// always was; the table says where those runs are and how to list them.
func TestBelayRunsPointsAtRunsInQueueCopies(t *testing.T) {
	ws := goWorkspace(t)
	queueAdd(t, ws, "--no-approval", "add docs")
	var reached []string
	if _, err := startQueue(t.Context(), t, ws, fakeGoals(reaches{goals: &reached})); err != nil {
		t.Fatalf("start: %v", err)
	}
	ran := goal(t, ws, 1)

	table, _, err := runCLI(t, "runs", "--workspace", ws)
	if err != nil {
		t.Fatalf("belay runs: %v", err)
	}
	t.Logf("verbatim belay runs:\n%s", table)
	if want := "belay --workspace " + ran.Dir + " runs"; !strings.Contains(table, want) {
		t.Errorf("belay runs never says %q", want)
	}

	raw, _, err := runCLI(t, "runs", "--workspace", ws, "--json")
	if err != nil {
		t.Fatalf("belay runs --json: %v", err)
	}
	var listing map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &listing); err != nil {
		t.Fatalf("belay runs --json: %v\n%s", err, raw)
	}
	keys := make([]string, 0, len(listing))
	for k := range listing {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"hidden", "runs", "shown", "total", "workspace"}; !slices.Equal(keys, want) {
		t.Errorf("belay runs --json keys = %v, want the documented %v, unchanged", keys, want)
	}
}

// TestTheReportShowsTheLastStartsResults: the closing report of a start, and
// the status after it, say what became of the goals that start ran. A queue
// used for weeks must not repeat every goal it ever finished each morning;
// the list still has them all.
func TestTheReportShowsTheLastStartsResults(t *testing.T) {
	ws := goWorkspace(t)
	var reached []string
	registry := fakeGoals(reaches{goals: &reached})

	queueAdd(t, ws, "--no-approval", "add tests for the parser")
	if _, err := startQueue(t.Context(), t, ws, registry); err != nil {
		t.Fatalf("the first start: %v", err)
	}
	earlier := goal(t, ws, 1)

	queueAdd(t, ws, "--no-approval", "add docs")
	out, err := startQueue(t.Context(), t, ws, registry)
	if err != nil {
		t.Fatalf("the second start: %v", err)
	}
	status, _, err := runCLI(t, "queue", "status", "--workspace", ws)
	if err != nil {
		t.Fatalf("belay queue status: %v", err)
	}
	t.Logf("verbatim status:\n%s", status)

	latest := goal(t, ws, 2)
	for name, text := range map[string]string{"the second start": out, "the status after it": status} {
		if !strings.Contains(text, "timeline "+latest.RunID) {
			t.Errorf("%s never points at goal 2, which it finished:\n%s", name, text)
		}
		if strings.Contains(text, "timeline "+earlier.RunID) {
			t.Errorf("%s repeats goal 1, which an earlier start finished:\n%s", name, text)
		}
	}
	if !strings.Contains(status, "add tests for the parser") {
		t.Errorf("the status's list lost goal 1:\n%s", status)
	}
}

// TestAGoalStoppedAtALimitIsPausedNotParked: the budget ceiling stops a run
// before the step that would cross it, and when that step is the approve gate
// the run is paused at approve without the gate ever having asked anything.
// That goal is not waiting for an approval: it is stopped at one of belay's
// limits, which only a person can raise, so the queue settles it as paused,
// says which limit and how to carry on, refuses to approve it, and never runs
// it again by itself.
func TestAGoalStoppedAtALimitIsPausedNotParked(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nbudget:\n  max_usd: 0.01\n")
	queueAdd(t, ws, "add tests for the parser")
	expensive := func() *graph.Registry {
		r := graph.NewRegistry()
		r.MustRegister(planWriter{body: "# Plan\n\nwrite a test\n", usage: belay.Usage{USD: 0.05}}, approve.New())
		return r
	}

	if _, err := startQueue(t.Context(), t, ws, expensive); err != nil {
		t.Fatalf("start: %v", err)
	}
	stopped := goal(t, ws, 1)
	if stopped.State != queue.StatePaused || !strings.Contains(stopped.Note, "budget") {
		t.Fatalf("goal 1 = %+v, want it paused at the budget ceiling, saying so", stopped)
	}
	man, err := state.LoadManifest(runLayout(t, stopped).ManifestPath())
	if err != nil || man.Status != state.RunStatusPaused || man.CurrentNode != graph.NodeApprove {
		t.Fatalf("goal 1's run = %v at %q (%v), want this test's premise: paused at approve", man.Status, man.CurrentNode, err)
	}

	status, _, err := runCLI(t, "queue", "status", "--workspace", ws)
	if err != nil {
		t.Fatalf("belay queue status: %v", err)
	}
	for _, phrase := range []string{
		"paused at a limit", "stopped at one of belay's limits",
		"belay --workspace " + stopped.Dir + " resume " + stopped.RunID,
	} {
		if !strings.Contains(status, phrase) {
			t.Errorf("belay queue status never says %q:\n%s", phrase, status)
		}
	}
	if strings.Contains(status, "belay queue approve 1") {
		t.Errorf("belay queue status offers to approve a goal no gate asked about:\n%s", status)
	}
	if _, _, err := runCLI(t, "queue", "approve", "--workspace", ws, "1"); err == nil {
		t.Error("belay queue approve approved a goal that was stopped at a limit")
	}

	before := readFile(t, runLayout(t, stopped).JournalPath())
	out, err := startQueue(t.Context(), t, ws, expensive)
	if err != nil {
		t.Fatalf("the next start: %v", err)
	}
	if got := readFile(t, runLayout(t, stopped).JournalPath()); got != before {
		t.Errorf("the next start ran goal 1 again; the queue never retries a goal stopped at a limit:\n%s", out)
	}
}

// TestCtrlCAtTheGateIsNotAnApproval: a Ctrl-C while the approve gate runs
// pauses the run at the gate too, but nobody has been shown the plan. The
// goal is still in hand, not parked, so it cannot be approved yet; the next
// start runs the gate again, which asks, and only then is the goal parked.
// Nothing about it ever reaches the code.
func TestCtrlCAtTheGateIsNotAnApproval(t *testing.T) {
	ws := goWorkspace(t)
	queueAdd(t, ws, "add tests for the parser")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	atTheGate := func() *graph.Registry {
		r := graph.NewRegistry()
		r.MustRegister(planWriter{body: "# Plan\n\nwrite a test\n"}, pressesCtrlC{name: graph.NodeApprove, cancel: cancel})
		return r
	}
	if _, err := startQueue(ctx, t, ws, atTheGate); err == nil {
		t.Fatal("the start was interrupted and reported no interruption")
	}
	if got := goal(t, ws, 1); got.State != queue.StateClaimed {
		t.Fatalf("goal 1 is %s after Ctrl-C at the gate, want it still in hand", got.State)
	}
	if _, _, err := runCLI(t, "queue", "approve", "--workspace", ws, "1"); err == nil {
		t.Error("belay queue approve approved a plan nobody had been asked about")
	}

	var reached []string
	if _, err := startQueue(t.Context(), t, ws, fakeGoals(reaches{goals: &reached})); err != nil {
		t.Fatalf("the next start: %v", err)
	}
	if got := goal(t, ws, 1); got.State != queue.StateWaiting {
		t.Errorf("goal 1 is %s, want it parked once the gate has asked", got.State)
	}
	if len(reached) != 0 {
		t.Errorf("goals that reached code = %v, want none", reached)
	}
}

// TestQueueRmRefusesAGoalThatHasStarted: once a goal has a run, taking it out
// of the queue would leave the run belonging to nothing.
func TestQueueRmRefusesAGoalThatHasStarted(t *testing.T) {
	ws := goWorkspace(t)
	queueAdd(t, ws, "add tests for the parser")
	var reached []string
	if _, err := startQueue(t.Context(), t, ws, fakeGoals(reaches{goals: &reached})); err != nil {
		t.Fatalf("start: %v", err)
	}
	parked := goal(t, ws, 1)

	_, _, err := runCLI(t, "queue", "rm", "--workspace", ws, "1")
	if err == nil || !strings.Contains(err.Error(), "has started") || !strings.Contains(err.Error(), parked.RunID) {
		t.Errorf("belay queue rm of a parked goal = %v, want a refusal saying it has started, naming its run", err)
	}
	if got := goal(t, ws, 1); got.State != queue.StateWaiting {
		t.Errorf("goal 1 is %s after the refused rm, want it still waiting", got.State)
	}
}

// rebooted is what crashesOnce panics with: the machine going down while a
// node runs, so nothing after the panic happens.
type rebooted struct{}

// crashesOnce stands in for the code node, and the first time it runs the
// machine goes down under it.
type crashesOnce struct{ crashed *bool }

func (crashesOnce) Name() string { return graph.NodeCode }

func (n crashesOnce) Run(context.Context, *graph.RunContext) (graph.Result, error) {
	if !*n.crashed {
		*n.crashed = true
		panic(rebooted{})
	}
	return graph.Result{Status: journal.StatusOK, Next: graph.End}, nil
}

// TestARebootMidGoalCarriesOnInTheSameRun is the reboot promise through belay
// queue start's own driver. The machine goes down inside goal 1's code step,
// leaving its run marked running and its journal open. The next start
// continues goal 1 in that run, re-running only the step that died, then runs
// goal 2, and says the start before it was cut off.
func TestARebootMidGoalCarriesOnInTheSameRun(t *testing.T) {
	ws := goWorkspace(t)
	queueAdd(t, ws, "--no-approval", "add tests for the parser")
	queueAdd(t, ws, "--no-approval", "add docs")

	crashed := false
	func() {
		defer func() {
			if r := recover(); r == nil {
				return
			} else if _, ok := r.(rebooted); !ok {
				panic(r)
			}
		}()
		_, err := startQueue(t.Context(), t, ws, fakeGoals(crashesOnce{crashed: &crashed}))
		t.Fatalf("the start was meant to go down with the machine, and returned %v", err)
	}()
	cut := goal(t, ws, 1)
	man, err := state.LoadManifest(runLayout(t, cut).ManifestPath())
	if cut.State != queue.StateClaimed || err != nil || man.Status != state.RunStatusRunning {
		t.Fatalf("after the crash goal 1 is %s and its run %v (%v); want claimed, and running as a crash leaves it",
			cut.State, man.Status, err)
	}

	var reached []string
	out, err := startQueue(t.Context(), t, ws, fakeGoals(reaches{goals: &reached}))
	if err != nil {
		t.Fatalf("the start after the reboot: %v", err)
	}
	if got := goal(t, ws, 1); got.State != queue.StateFinished || got.RunID != cut.RunID {
		t.Errorf("goal 1 = %+v, want it finished in the run the crash cut short, %s", got, cut.RunID)
	}
	if got := goal(t, ws, 2); got.State != queue.StateFinished {
		t.Errorf("goal 2 is %s, want finished after goal 1", got.State)
	}
	for _, phrase := range []string{"→ code      step 3, attempt 2", "was cut off"} {
		if !strings.Contains(out, phrase) {
			t.Errorf("the start after the reboot never says %q", phrase)
		}
	}
	if !slices.Equal(reached, []string{"add tests for the parser", "add docs"}) {
		t.Errorf("goals that reached code = %v, want goal 1 carried on before goal 2", reached)
	}
}

// TestABelayCopyIsNotTheCheckoutAroundIt: a queued goal's copy lives inside
// the person's repository, under .belay. Opened as a workspace -- `belay
// --workspace <copy> resume <run-id>` is what belay queue tells a person to
// type for a goal stopped at a limit -- git finds the person's own repository
// a few directories up. That repository is not this workspace's to branch:
// the copy is belay's scratch, and work in it happens in place. Read the
// other way, a resume there warns that belay.yaml now asks for a branch,
// which nobody changed, and a run started there branches the person's repo.
func TestABelayCopyIsNotTheCheckoutAroundIt(t *testing.T) {
	ws := gitWorkspace(t)
	inside := filepath.Join(ws, ".belay", "queue", "work", "20260930T220000Z-000000000001")
	if err := os.MkdirAll(inside, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inside, "go.mod"), []byte("module example.com/scratch\n\ngo 1.26.3\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	plan, err := resolve(resolveRequest{workspace: inside, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.output.Branched() {
		t.Errorf("a copy under .belay was taken for the checkout around it: output %+v (%s)", plan.output, plan.outputReason)
	}
	if !strings.Contains(plan.outputReason, ".belay") {
		t.Errorf("the reason belay edits the copy in place does not say why: %q", plan.outputReason)
	}

	writeConfig(t, inside, "version: 1\noutput:\n  mode: branch\n")
	plan, err = resolve(resolveRequest{workspace: inside, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(blockerText(plan), "output.mode is \"branch\"") {
		t.Errorf("output.mode branch in a copy under .belay was not refused; blockers: %s", blockerText(plan))
	}
}

// TestQueueCommandsSurviveAPathWithASpace: every command belay queue writes
// out is meant to be pasted, and a workspace on macOS is as likely as not to
// have a space in its path. A command that breaks on paste is worse than one
// never written out.
func TestQueueCommandsSurviveAPathWithASpace(t *testing.T) {
	ws := filepath.Join(goWorkspace(t), "my project")
	if err := os.MkdirAll(ws, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module example.com/scratch\n\ngo 1.26.3\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	queueAdd(t, ws, "--no-approval", "add docs")
	var reached []string
	if _, err := startQueue(t.Context(), t, ws, fakeGoals(reaches{goals: &reached})); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := goal(t, ws, 1)
	quoted := "'" + done.Dir + "'"

	status, _, err := runCLI(t, "queue", "status", "--workspace", ws)
	if err != nil {
		t.Fatalf("belay queue status: %v", err)
	}
	if want := "belay --workspace " + quoted + " timeline " + done.RunID; !strings.Contains(status, want) {
		t.Errorf("belay queue status never says %q:\n%s", want, status)
	}
	runs, _, err := runCLI(t, "runs", "--workspace", ws)
	if err != nil {
		t.Fatalf("belay runs: %v", err)
	}
	if want := "belay --workspace " + quoted + " runs"; !strings.Contains(runs, want) {
		t.Errorf("belay runs never says %q:\n%s", want, runs)
	}
}

// TestAQueueCopyResumesWhateverOutputModeSays: for a goal stopped at a limit,
// belay queue says to raise the limit in the copy's belay.yaml and resume the
// goal's run in its copy. Every queued goal's run is pinned in place, and the
// copy's belay.yaml says output.mode "branch" when the checkout's did, which
// the copy cannot take. That refusal outlived the bind to the run's own pin,
// so belay refused the command it had printed. The pin wins: the resume is
// not refused, and says that belay.yaml asks for something this run is not
// doing.
func TestAQueueCopyResumesWhateverOutputModeSays(t *testing.T) {
	ws := gitWorkspace(t)
	writeConfig(t, ws, "version: 1\noutput:\n  mode: branch\nbudget:\n  max_usd: 0.01\n")
	queueAdd(t, ws, "add tests for the parser")
	expensive := func() *graph.Registry {
		r := graph.NewRegistry()
		r.MustRegister(planWriter{body: "# Plan\n\nwrite a test\n", usage: belay.Usage{USD: 0.05}}, approve.New())
		return r
	}
	if _, err := startQueue(t.Context(), t, ws, expensive); err != nil {
		t.Fatalf("start: %v", err)
	}
	stopped := goal(t, ws, 1)
	if stopped.State != queue.StatePaused {
		t.Fatalf("goal 1 = %+v, want this test's premise: paused at a limit", stopped)
	}
	status, _, err := runCLI(t, "queue", "status", "--workspace", ws)
	if err != nil {
		t.Fatalf("belay queue status: %v", err)
	}
	printed := "belay --workspace " + shellWord(stopped.Dir) + " resume " + stopped.RunID
	if !strings.Contains(status, printed) {
		t.Fatalf("belay queue status never says %q, so this test is not running the command belay printed:\n%s",
			printed, status)
	}

	// Raise the limit where belay queue said to, and run what it printed.
	writeConfig(t, stopped.Dir, "version: 1\noutput:\n  mode: branch\nbudget:\n  max_usd: 5\n")
	stdout, stderr, err := runCLI(t, "--workspace", stopped.Dir, "--dry-run", "resume", stopped.RunID)
	t.Logf("verbatim output:\n%s%s", stdout, stderr)
	if err != nil {
		t.Fatalf("the resume belay queue printed was refused: %v", err)
	}
	for _, phrase := range []string{`output.mode is "branch"`, "carries on in place"} {
		if !strings.Contains(stdout, phrase) {
			t.Errorf("the resume's dry run never says %q", phrase)
		}
	}
}
