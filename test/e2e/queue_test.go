//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/isolate/dircopy"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/queue"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Queue suite: a goal queue worked through by belay's own worker, over the
// real dispatcher, journal, run state and directory copies, with a reboot in
// the middle of a goal.
//
// The driver here is the test's, not internal/cli's, which is unexported; it
// does what that one does, in the fewest lines: make a run the way createRun
// lays one down, and resume it through a dispatcher with PauseOnCancel. The
// nodes are fakes for the reason the durability suite gives: what is under
// test is that a goal is claimed once and its run picked up where it died,
// which is a property of the queue, the dispatcher, the journal and the
// state, not of any agent.

// queueCrash is what a node panics with to stand in for the machine going
// down while it runs. Nothing after the panic runs, as nothing runs after a
// power cut.
type queueCrash struct{ goal string }

// queueNode is a node that reports spending usd and routes to next, and
// panics the first time it runs for the goal named in crashFor.
type queueNode struct {
	name, next string
	usd        float64
	mu         *sync.Mutex
	crashFor   *string
	ran        map[string]int // runs per goal, shared across both workers
}

func (n queueNode) Name() string { return n.name }

func (n queueNode) Run(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
	n.mu.Lock()
	n.ran[rc.Goal]++
	if *n.crashFor == rc.Goal {
		*n.crashFor = ""
		n.mu.Unlock()
		panic(queueCrash{goal: rc.Goal})
	}
	n.mu.Unlock()
	return graph.Result{Status: journal.StatusOK, Next: n.next, Usage: belay.Usage{USD: n.usd}}, nil
}

// queueDriver makes and drives goal runs the way belay queue start does.
type queueDriver struct {
	t        *testing.T
	cfg      config.Config
	registry *graph.Registry

	mu      sync.Mutex
	created map[string]int // runs made, by run id
}

func (d *queueDriver) layout(e queue.Entry) state.Layout {
	d.t.Helper()
	layout, err := state.NewLayout(e.Dir, e.RunID)
	if err != nil {
		d.t.Fatalf("state.NewLayout(%s, %s): %v", e.Dir, e.RunID, err)
	}
	return layout
}

func (d *queueDriver) Observe(e queue.Entry) (queue.RunView, error) {
	man, err := state.LoadManifest(d.layout(e).ManifestPath())
	if errors.Is(err, fs.ErrNotExist) {
		return queue.RunView{}, nil
	}
	if err != nil {
		return queue.RunView{}, err
	}
	return queue.RunView{Exists: true, Status: man.Status}, nil
}

func (d *queueDriver) Create(_ context.Context, e queue.Entry) error {
	layout := d.layout(e)
	if _, err := os.Stat(layout.ManifestPath()); err == nil {
		return fmt.Errorf("run %s already exists", e.RunID)
	}
	if err := os.MkdirAll(layout.ArtifactsDir(), 0o750); err != nil {
		return err
	}
	if err := state.SaveState(layout.StatePath(), state.NewState(e.Goal)); err != nil {
		return err
	}
	if err := state.SaveManifest(layout.ManifestPath(),
		state.NewManifest(time.Now().UTC(), e.RunID, e.Dir, e.Goal, d.cfg)); err != nil {
		return err
	}
	d.mu.Lock()
	d.created[e.RunID]++
	d.mu.Unlock()
	return nil
}

func (d *queueDriver) Drive(ctx context.Context, e queue.Entry) (queue.Outcome, error) {
	dispatcher, err := graph.NewDispatcher(graph.Options{
		Store: state.NewStore(d.layout(e)), Registry: d.registry, Config: d.cfg, PauseOnCancel: true,
	})
	if err != nil {
		return queue.Outcome{}, err
	}
	out, runErr := dispatcher.Resume(ctx)
	if errors.Is(runErr, graph.ErrInterrupted) {
		return queue.Outcome{Interrupted: true}, nil
	}
	switch out.Status {
	case state.RunStatusCompleted:
		return queue.Outcome{State: queue.StateFinished}, nil
	case state.RunStatusFailed:
		return queue.Outcome{State: queue.StateFailed, Note: out.Note}, nil
	default:
		return queue.Outcome{}, fmt.Errorf("goal %d's run ended %s: %w", e.ID, out.Status, runErr)
	}
}

// TestQueue_RebootMidGoal_FinishesEveryGoalOnce is the reboot promise: a
// worker dies in the middle of goal 2 of 3; `belay queue start` again picks
// up goal 2 in the run it was already in, at the step that died, and then
// goal 3. Every goal is claimed once and its run made once; goal 2's plan is
// not paid for twice; and goal 2's ledger carries on from what it had spent,
// with the step the reboot cut short charged as the dispatcher charges any,
// rather than starting again from nothing.
func TestQueue_RebootMidGoal_FinishesEveryGoalOnce(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("the project\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	store, err := queue.Open(ws)
	if err != nil {
		t.Fatalf("queue.Open: %v", err)
	}
	for _, g := range []string{"add tests", "add docs", "fix the typos"} {
		if _, err := store.Add(g, true); err != nil {
			t.Fatalf("Add(%q): %v", g, err)
		}
	}

	cfg := config.Default()
	cfg.Budget.MaxUSD = 10
	var mu sync.Mutex
	crashFor := "add docs"
	ran := map[string]int{}
	plans := map[string]int{}
	registry := graph.NewRegistry()
	registry.MustRegister(
		queueNode{name: graph.NodePlan, next: graph.NodeCode, usd: 1, mu: &mu, crashFor: new(string), ran: plans},
		queueNode{name: graph.NodeCode, next: graph.End, usd: 1, mu: &mu, crashFor: &crashFor, ran: ran},
	)
	driver := &queueDriver{t: t, cfg: cfg, registry: registry, created: map[string]int{}}
	copier, err := dircopy.New(store.WorkRoot())
	if err != nil {
		t.Fatalf("dircopy.New: %v", err)
	}
	worker := func() *queue.Worker {
		s, err := queue.Open(ws)
		if err != nil {
			t.Fatalf("queue.Open: %v", err)
		}
		return &queue.Worker{Store: s, Source: ws, Copier: copier, Driver: driver}
	}

	// The first worker: goal 1 runs, then the machine goes down inside goal
	// 2's code step.
	func() {
		defer func() {
			if r := recover(); r == nil {
				return
			} else if _, ok := r.(queueCrash); !ok {
				panic(r)
			}
		}()
		report, err := worker().Work(context.Background())
		t.Fatalf("the first worker was meant to die inside goal 2, and returned (%+v, %v)", report, err)
	}()
	q, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	first, _ := q.Entry(1)
	second, _ := q.Entry(2)
	third, _ := q.Entry(3)
	if first.State != queue.StateFinished || second.State != queue.StateClaimed || third.State != queue.StateQueued {
		t.Fatalf("after the reboot goals are %s, %s, %s; want finished, claimed, queued",
			first.State, second.State, third.State)
	}
	open, err := journal.ReadFile(driver.layout(second).JournalPath())
	if err != nil || len(open.Records) == 0 || open.Records[len(open.Records)-1].Event != journal.EventNodeStarted {
		t.Fatalf("goal 2's journal does not end in the step that died: %+v (%v)", open.Records, err)
	}

	// The next `belay queue start`, after the reboot.
	report, err := worker().Work(context.Background())
	if err != nil {
		t.Fatalf("the worker after the reboot: %v", err)
	}
	if !report.PreviousCutOff || report.Stop != queue.StopDrained {
		t.Errorf("report = %+v, want it to say the last start was cut off, and to drain the queue", report)
	}
	q, err = store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, e := range q.Entries {
		if e.State != queue.StateFinished {
			t.Errorf("goal %d is %s, want finished", e.ID, e.State)
		}
		if n := driver.created[e.RunID]; n != 1 {
			t.Errorf("goal %d's run %s was made %d times, want once", e.ID, e.RunID, n)
		}
		if _, err := os.Stat(filepath.Join(e.Dir, "README.md")); err != nil {
			t.Errorf("goal %d's copy %s has no README.md: %v", e.ID, e.Dir, err)
		}
	}
	if again, _ := q.Entry(2); again.RunID != second.RunID {
		t.Errorf("goal 2 finished in run %s, want the run its claim named, %s", again.RunID, second.RunID)
	}
	if plans["add docs"] != 1 || ran["add docs"] != 2 {
		t.Errorf("goal 2 planned %d times and coded %d times, want its plan once and its code again after the reboot",
			plans["add docs"], ran["add docs"])
	}
	man, err := state.LoadManifest(driver.layout(second).ManifestPath())
	if err != nil {
		t.Fatalf("goal 2's manifest: %v", err)
	}
	// $1 for the plan, $1 the dispatcher charges the code step the reboot
	// cut short (the costliest step the run had paid for, estimated, see
	// ADR-0007), and $1 for the code step run again. A fresh ledger would
	// show $1.
	if !durabilityCloseTo(man.Budget.SpentUSD, 3) || !man.Budget.Estimated {
		t.Errorf("goal 2's run spent $%.2f (estimated %v), want $3.00, estimated: its ledger carries on across the reboot",
			man.Budget.SpentUSD, man.Budget.Estimated)
	}
	if _, err := os.Stat(filepath.Join(ws, ".belay", "runs")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the checkout has runs of its own (stat error %v); every goal runs in its copy", err)
	}
}
