//go:build unix

package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// crash is what a fake panics with to stand in for the process dying where
// it stands. The dispatcher does not recover panics, so nothing after the
// panic runs, exactly as nothing runs after a kill -9.
type crash struct{ where string }

// fakeRun is one run as the fake driver keeps it: what would be on disk.
type fakeRun struct {
	status    state.RunStatus
	gateAsked bool
}

// fakeDriver is a Driver over runs it keeps in memory, which outlive a
// simulated crash the way files outlive a killed process.
type fakeDriver struct {
	t     *testing.T
	store *Store

	mu      sync.Mutex
	runs    map[string]*fakeRun
	created []string // run ids, each time a run was made
	driven  []int    // goal ids, each time a run was driven
	seen    []Entry  // every Entry Create or Drive was handed
	// crashAt makes the named step panic once: "create", "drive" (mid-run)
	// or "settle" (after the run ended, before the worker hears of it).
	crashAt string
	// drive decides how a goal's run ends; nil completes every run.
	drive func(e Entry, run *fakeRun) Outcome
}

func newFakeDriver(t *testing.T, s *Store) *fakeDriver {
	return &fakeDriver{t: t, store: s, runs: map[string]*fakeRun{}}
}

func (d *fakeDriver) Observe(e Entry) (RunView, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	run, ok := d.runs[e.RunID]
	if !ok {
		return RunView{}, nil
	}
	return RunView{Exists: true, Status: run.status, GateAsked: run.gateAsked}, nil
}

func (d *fakeDriver) Create(_ context.Context, e Entry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, e)
	requireClaimed(d.t, d.store, e)
	if _, ok := d.runs[e.RunID]; ok {
		return fmt.Errorf("run %s already exists", e.RunID)
	}
	if _, err := os.Stat(filepath.Join(e.Dir, "copied-from")); err != nil {
		d.t.Errorf("Create for goal %d before its copy was made: %v", e.ID, err)
	}
	if d.crashAt == "create" {
		d.crashAt = ""
		panic(crash{"create"})
	}
	d.runs[e.RunID] = &fakeRun{status: state.RunStatusRunning}
	d.created = append(d.created, e.RunID)
	return nil
}

func (d *fakeDriver) Drive(_ context.Context, e Entry) (Outcome, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, e)
	d.driven = append(d.driven, e.ID)
	run := d.runs[e.RunID]
	if run == nil {
		return Outcome{}, fmt.Errorf("drive of goal %d, whose run %s does not exist", e.ID, e.RunID)
	}
	switch d.crashAt {
	case "drive":
		d.crashAt = ""
		run.status = state.RunStatusRunning
		panic(crash{"drive"})
	case "settle":
		d.crashAt = ""
		run.status = state.RunStatusCompleted
		panic(crash{"settle"})
	}
	if d.drive != nil {
		return d.drive(e, run), nil
	}
	run.status = state.RunStatusCompleted
	return Outcome{State: StateFinished}, nil
}

// fakeCopier makes a copy by writing one file, so a test can see whether a
// copy was made and whether it was taken away again.
type fakeCopier struct {
	root string

	mu        sync.Mutex
	made      []string // ids, each time a copy was completed
	reclaimed []string // ids, each time something was there to reclaim
	// crashAt makes the next copy panic: "before" anything is made, or
	// "midway" through, leaving a partial copy behind.
	crashAt string
	store   *Store
	t       *testing.T
}

func (c *fakeCopier) Create(_ context.Context, src, id string) (belay.Workspace, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store != nil {
		q, err := c.store.Read()
		if err != nil {
			c.t.Fatalf("Read: %v", err)
		}
		claimedFor := false
		for _, e := range q.Entries {
			claimedFor = claimedFor || (e.State == StateClaimed && e.RunID == id)
		}
		if !claimedFor {
			c.t.Errorf("a copy named %s was made before any claim named it", id)
		}
	}
	dst := filepath.Join(c.root, id)
	if c.crashAt == "before" {
		c.crashAt = ""
		panic(crash{"copy before"})
	}
	if entries, err := os.ReadDir(dst); err == nil && len(entries) > 0 {
		return belay.Workspace{}, fmt.Errorf("%s is not empty", dst)
	}
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return belay.Workspace{}, err
	}
	if err := os.WriteFile(filepath.Join(dst, "half-a-file"), []byte("x"), 0o600); err != nil {
		return belay.Workspace{}, err
	}
	if c.crashAt == "midway" {
		c.crashAt = ""
		panic(crash{"copy midway"})
	}
	if err := os.Remove(filepath.Join(dst, "half-a-file")); err != nil {
		return belay.Workspace{}, err
	}
	if err := os.WriteFile(filepath.Join(dst, "copied-from"), []byte(src), 0o600); err != nil {
		return belay.Workspace{}, err
	}
	c.made = append(c.made, id)
	return belay.Workspace{ID: id, Dir: dst, Ephemeral: true}, nil
}

func (c *fakeCopier) Reclaim(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	dst := filepath.Join(c.root, id)
	if _, err := os.Stat(dst); err == nil {
		c.reclaimed = append(c.reclaimed, id)
	}
	return os.RemoveAll(dst)
}

// requireClaimed fails the test unless the log, read from disk, already
// names e's run in a claim: the protocol's one promise.
func requireClaimed(t *testing.T, s *Store, e Entry) {
	t.Helper()
	q, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got, ok := q.Entry(e.ID)
	if !ok || got.RunID != e.RunID || (got.State != StateClaimed && got.State != StateApproved) {
		t.Errorf("goal %d's run %s was made before the log recorded its claim (the log says %+v)", e.ID, e.RunID, got)
	}
}

// harness is one workspace's queue with a fake copier and driver, whose
// state survives the simulated crash of any worker built over them.
type harness struct {
	t      *testing.T
	ws     string
	store  *Store
	copier *fakeCopier
	driver *fakeDriver
	ids    int
}

func newHarness(t *testing.T) *harness {
	ws := t.TempDir()
	s := mustOpen(t, ws)
	return &harness{
		t: t, ws: ws, store: s,
		copier: &fakeCopier{root: s.WorkRoot(), store: s, t: t},
		driver: newFakeDriver(t, s),
	}
}

func (h *harness) add(goals ...string) {
	h.t.Helper()
	for _, g := range goals {
		if _, err := h.store.Add(g, strings.HasPrefix(g, "unreviewed:")); err != nil {
			h.t.Fatalf("Add(%q): %v", g, err)
		}
	}
}

// worker builds a brand-new Worker over the harness's disk, as a fresh
// `belay queue start` would.
func (h *harness) worker() *Worker {
	return &Worker{
		Store: mustOpen(h.t, h.ws), Source: h.ws, Copier: h.copier, Driver: h.driver,
		NewRunID: func() (string, error) { h.ids++; return runIDFor(h.ids), nil },
	}
}

// work runs one worker to its end and returns what it reported.
func (h *harness) work(ctx context.Context) (Report, error) {
	h.t.Helper()
	return h.worker().Work(ctx)
}

// workUntilCrash runs one worker until a fake panics, as a kill would stop it.
func (h *harness) workUntilCrash() string {
	h.t.Helper()
	var where string
	func() {
		defer func() {
			r := recover()
			if r == nil {
				// No panic: t.Fatalf below is ending the test.
				return
			}
			c, ok := r.(crash)
			if !ok {
				panic(r)
			}
			where = c.where
		}()
		report, err := h.worker().Work(context.Background())
		h.t.Fatalf("the worker was meant to crash, and returned (%+v, %v)", report, err)
	}()
	return where
}

func (h *harness) states() map[int]State {
	h.t.Helper()
	q, err := h.store.Read()
	if err != nil {
		h.t.Fatalf("Read: %v", err)
	}
	out := map[int]State{}
	for _, e := range q.Entries {
		out[e.ID] = e.State
	}
	return out
}

// TestWorkerClaimsBeforeItCreates is the property the queue rests on: the
// log names a goal's run before its copy or its run exists, so a crash at any
// moment after leaves a claim that names exactly what was being made, and the
// next worker makes that same run rather than a second one. Both fakes check
// the log on disk at the moment they are called.
func TestWorkerClaimsBeforeItCreates(t *testing.T) {
	h := newHarness(t)
	h.add("add tests", "add docs")

	report, err := h.work(context.Background())
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if report.Stop != StopDrained || len(report.Settled) != 2 {
		t.Errorf("report = %+v, want both goals settled and the queue drained", report)
	}
	if got := h.states(); got[1] != StateFinished || got[2] != StateFinished {
		t.Errorf("states = %v, want both finished", got)
	}
	if want := []string{runIDFor(1), runIDFor(2)}; !slices.Equal(h.driver.created, want) {
		t.Errorf("runs made = %v, want one per goal, %v", h.driver.created, want)
	}
}

// TestWorkerRecoversFromEveryCrashWindow kills the worker at each point of
// one goal's life and starts a new one over the same disk. Every goal must
// finish exactly once, under the run its first claim named, and the goal
// behind it must run after it.
func TestWorkerRecoversFromEveryCrashWindow(t *testing.T) {
	tests := []struct {
		name      string
		copyCrash string
		runCrash  string
		where     string
		reclaimed int // how many leftover copies the second worker must clear
		drives    int // how many times goal 1's run is driven in all
	}{
		{name: "after the claim, before the copy", copyCrash: "before", where: "copy before", drives: 1},
		{name: "half way through the copy", copyCrash: "midway", where: "copy midway", reclaimed: 1, drives: 1},
		{name: "after the copy, before the run is made", runCrash: "create", where: "create", reclaimed: 1, drives: 1},
		{name: "in the middle of the run", runCrash: "drive", where: "drive", drives: 2},
		{name: "after the run ended, before it was settled", runCrash: "settle", where: "settle", drives: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.add("add tests", "add docs")
			h.copier.crashAt, h.driver.crashAt = tt.copyCrash, tt.runCrash

			if where := h.workUntilCrash(); where != tt.where {
				t.Fatalf("the worker crashed at %q, want %q", where, tt.where)
			}
			claim, err := h.store.Read()
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			first, _ := claim.Entry(1)
			if first.State != StateClaimed || first.RunID != runIDFor(1) {
				t.Fatalf("after the crash goal 1 is %+v, want it still claimed for %s", first, runIDFor(1))
			}

			report, err := h.work(context.Background())
			if err != nil {
				t.Fatalf("the second Work: %v", err)
			}
			if !report.PreviousCutOff {
				t.Error("the second worker's report does not say the first was cut off")
			}
			after, err := h.store.Read()
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			for _, id := range []int{1, 2} {
				e, _ := after.Entry(id)
				if e.State != StateFinished || e.RunID != runIDFor(id) {
					t.Errorf("goal %d ended as %+v, want finished under %s", id, e, runIDFor(id))
				}
			}
			if want := []string{runIDFor(1), runIDFor(2)}; !slices.Equal(h.driver.created, want) {
				t.Errorf("runs made = %v, want exactly one per goal, %v", h.driver.created, want)
			}
			if got := len(h.copier.reclaimed); got != tt.reclaimed {
				t.Errorf("leftover copies cleared = %v, want %d", h.copier.reclaimed, tt.reclaimed)
			}
			drives := 0
			for _, id := range h.driver.driven {
				if id == 1 {
					drives++
				}
			}
			if drives != tt.drives {
				t.Errorf("goal 1's run was driven %d times, want %d", drives, tt.drives)
			}
		})
	}
}

// TestWorkerParksAGoalAtItsGateAndMovesOn is the crux. A goal whose plan
// stops for review is parked, the queue moves on to the next goal, and the
// parked goal is never driven again until a person approves it.
func TestWorkerParksAGoalAtItsGateAndMovesOn(t *testing.T) {
	h := newHarness(t)
	h.add("add tests", "add docs")
	h.driver.drive = func(e Entry, run *fakeRun) Outcome {
		if e.ID == 1 && e.State != StateApproved {
			run.status, run.gateAsked = state.RunStatusPaused, true
			return Outcome{State: StateWaiting, Note: "the plan is at plan.md"}
		}
		run.status, run.gateAsked = state.RunStatusCompleted, false
		return Outcome{State: StateFinished}
	}

	report, err := h.work(context.Background())
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := h.states(); got[1] != StateWaiting || got[2] != StateFinished {
		t.Fatalf("states = %v, want goal 1 parked and goal 2 finished", got)
	}
	if len(report.Settled) != 2 || report.Settled[0].State != StateWaiting || report.Settled[0].Note != "the plan is at plan.md" {
		t.Errorf("report = %+v, want goal 1 reported as waiting, with its note", report)
	}

	// A second start with nothing approved does nothing to the parked goal.
	driven := len(h.driver.driven)
	if _, err := h.work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(h.driver.driven) != driven {
		t.Fatalf("a start with nothing approved drove %v; a parked goal waits for a person", h.driver.driven[driven:])
	}

	if _, err := h.store.Approve(1); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := h.work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := h.states(); got[1] != StateFinished {
		t.Errorf("after the approval goal 1 is %s, want finished", got[1])
	}
	if want := []string{runIDFor(1), runIDFor(2)}; !slices.Equal(h.driver.created, want) {
		t.Errorf("runs made = %v, want %v: the approved goal continues its own run", h.driver.created, want)
	}
}

// TestWorkerCarriesNoApprovalThroughRecovery: --no-approval is the person's
// decision, made when they added the goal. It lives in the log, not in a
// worker's memory, so a goal recovered after a crash is still the goal they
// added.
func TestWorkerCarriesNoApprovalThroughRecovery(t *testing.T) {
	h := newHarness(t)
	h.add("unreviewed: add tests", "add docs")
	h.driver.crashAt = "drive"
	h.workUntilCrash()
	if _, err := h.work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	for _, e := range h.driver.seen {
		if want := e.ID == 1; e.NoApproval != want {
			t.Errorf("goal %d reached the driver with NoApproval %v, want %v", e.ID, e.NoApproval, want)
		}
	}
	if len(h.driver.seen) < 3 {
		t.Errorf("the driver saw only %d calls, so this proves little", len(h.driver.seen))
	}
}

// TestAnInterruptionLeavesTheGoalInHand: Ctrl-C pauses the goal's run where
// it stands. The worker records that it stopped and settles nothing, so the
// next start continues that goal before it claims another.
func TestAnInterruptionLeavesTheGoalInHand(t *testing.T) {
	h := newHarness(t)
	h.add("add tests", "add docs")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.driver.drive = func(_ Entry, run *fakeRun) Outcome {
		cancel()
		run.status = state.RunStatusPaused
		return Outcome{Interrupted: true}
	}

	report, err := h.work(ctx)
	if err != nil {
		t.Fatalf("Work = %v, want an interruption to be no error", err)
	}
	if report.Stop != StopInterrupted || len(report.Settled) != 0 {
		t.Errorf("report = %+v, want an interrupted stop that settled nothing", report)
	}
	q, err := h.store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got, _ := q.Entry(1); got.State != StateClaimed {
		t.Errorf("goal 1 is %s after the interruption, want it still claimed", got.State)
	}
	if !q.Last.Ended || q.Last.Stop != StopInterrupted {
		t.Errorf("the log's last start = %+v, want it to end interrupted", q.Last)
	}

	h.driver.drive = nil
	report, err = h.work(context.Background())
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if report.PreviousCutOff {
		t.Error("the report says the last start was cut off; it recorded stopping")
	}
	if got := h.states(); got[1] != StateFinished || got[2] != StateFinished {
		t.Errorf("states = %v, want both finished", got)
	}
	if len(h.driver.driven) < 2 || h.driver.driven[0] != 1 || h.driver.driven[1] != 1 {
		t.Errorf("drives = %v, want goal 1 continued before goal 2", h.driver.driven)
	}
}

// TestAFailedStepStopsTheWorkerAndKeepsTheGoal: a step that cannot be done
// (a disk that will not take the copy, a run that cannot be made) stops the
// worker with the reason, and the goal stays claimed, so the next start
// tries the same goal again rather than losing it or skipping to the next.
func TestAFailedStepStopsTheWorkerAndKeepsTheGoal(t *testing.T) {
	h := newHarness(t)
	h.add("add tests", "add docs")
	broken := errors.New("the disk is full")
	w := h.worker()
	w.Driver = failingDriver{Driver: h.driver, err: broken}

	_, err := w.Work(context.Background())
	if !errors.Is(err, broken) {
		t.Fatalf("Work = %v, want the step's error", err)
	}
	q, err := h.store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got, _ := q.Entry(1); got.State != StateClaimed {
		t.Errorf("goal 1 is %s, want it still claimed for the next start", got.State)
	}
	if q.Last.Stop != StopFailed || !strings.Contains(q.Last.Note, "the disk is full") {
		t.Errorf("the log's last start = %+v, want it to end failed, saying why", q.Last)
	}
}

// failingDriver refuses to make any run.
type failingDriver struct {
	Driver
	err error
}

func (f failingDriver) Create(context.Context, Entry) error { return f.err }

// TestWorkerWillNotShareTheQueue: while one worker holds the queue, another
// is refused and writes nothing.
func TestWorkerWillNotShareTheQueue(t *testing.T) {
	h := newHarness(t)
	h.add("add tests")
	lock, err := h.store.LockWorker()
	if err != nil {
		t.Fatalf("LockWorker: %v", err)
	}
	defer func() { _ = lock.Release() }()
	before := readFile(t, h.store.LogPath())

	if _, err := h.work(context.Background()); !errors.Is(err, ErrWorkerActive) {
		t.Fatalf("Work while another worker holds the queue = %v, want ErrWorkerActive", err)
	}
	if got := readFile(t, h.store.LogPath()); got != before {
		t.Errorf("a refused worker wrote to the log:\n%s", got)
	}
}

// appearingRun is a driver whose goal's run is not there when the worker
// decides what to do, and is there a moment later.
type appearingRun struct {
	*fakeDriver
	looks int
}

func (d *appearingRun) Observe(Entry) (RunView, error) {
	d.looks++
	if d.looks == 1 {
		return RunView{}, nil
	}
	return RunView{Exists: true, Status: state.RunStatusRunning}, nil
}

// TestWorkerNeverClearsACopyWhoseRunExists: making a goal's copy starts by
// clearing whatever a crash left at its path, which deletes a directory. That
// is only ever right when no run exists there, so the worker looks again
// itself, right before it clears anything, rather than trusting what it saw
// when it decided. A run that is there by then is never deleted.
func TestWorkerNeverClearsACopyWhoseRunExists(t *testing.T) {
	h := newHarness(t)
	h.add("add tests")
	if _, err := h.store.claim(1, runIDFor(1), filepath.Join(h.store.WorkRoot(), runIDFor(1))); err != nil {
		t.Fatalf("claim: %v", err)
	}
	precious := filepath.Join(h.store.WorkRoot(), runIDFor(1), "work-in-progress.go")
	if err := os.MkdirAll(filepath.Dir(precious), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(precious, []byte("package x\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	w := h.worker()
	w.Driver = &appearingRun{fakeDriver: h.driver}

	if _, err := w.Work(context.Background()); err == nil {
		t.Error("Work went on with a copy it could neither clear nor make")
	}
	if _, err := os.Stat(precious); err != nil {
		t.Errorf("the copy of a run that exists was cleared away: %v", err)
	}
	if len(h.copier.reclaimed) != 0 {
		t.Errorf("reclaimed %v; nothing may be cleared once its run exists", h.copier.reclaimed)
	}
}
