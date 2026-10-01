//go:build unix

package queue

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAddAssignsUniqueIDsAcrossHandles: two terminals adding goals at once
// must never be given one id between them. Each Store here stands for a
// separate process: it holds no file between calls, so the log lock is the
// only thing ordering them.
func TestAddAssignsUniqueIDsAcrossHandles(t *testing.T) {
	ws := t.TempDir()
	stores := []*Store{mustOpen(t, ws), mustOpen(t, ws)}
	const each = 25

	var (
		mu  sync.Mutex
		ids []int
		wg  sync.WaitGroup
	)
	for _, s := range stores {
		for range each {
			wg.Go(func() {
				e, err := s.Add("a goal", false)
				if err != nil {
					t.Errorf("Add: %v", err)
					return
				}
				mu.Lock()
				ids = append(ids, e.ID)
				mu.Unlock()
			})
		}
	}
	wg.Wait()

	slices.Sort(ids)
	for i, id := range ids {
		if id != i+1 {
			t.Fatalf("ids given out = %v, want 1..%d with none repeated or skipped", ids, 2*each)
		}
	}
	q, err := stores[0].Read()
	if err != nil {
		t.Fatalf("Read after concurrent adds = %v; two writers must never leave the log out of sequence", err)
	}
	if len(q.Entries) != 2*each {
		t.Errorf("the log holds %d goals, want %d", len(q.Entries), 2*each)
	}
}

// TestAGoalMovesOnlyAlongItsEdges drives one goal through the life the worker
// gives it, through the store, and checks that each change is refused when
// the goal is anywhere it cannot be made from -- before anything is written.
func TestAGoalMovesOnlyAlongItsEdges(t *testing.T) {
	ws := t.TempDir()
	s := mustOpen(t, ws, WithClock(func() time.Time { return at }))

	e, err := s.Add("  add tests for the parser  ", true)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if e.ID != 1 || e.State != StateQueued || !e.NoApproval || e.Goal != "add tests for the parser" {
		t.Errorf("Add gave %+v, want goal 1, queued, no approval, the goal trimmed of the space around it", e)
	}

	refused := func(what string, err error) {
		t.Helper()
		var wrong *StateError
		if !errors.As(err, &wrong) || !errors.Is(err, ErrWrongState) {
			t.Errorf("%s = %v, want a *StateError wrapping ErrWrongState", what, err)
		}
	}
	before := readFile(t, s.LogPath())
	_, err = s.Approve(1)
	refused("Approve of a queued goal", err)
	_, err = s.settle(1, StateFinished, "")
	refused("settle of a queued goal", err)
	if got := readFile(t, s.LogPath()); got != before {
		t.Fatalf("a refused change wrote to the log:\nbefore: %s\nafter:  %s", before, got)
	}

	dir := filepath.Join(s.WorkRoot(), runIDFor(1))
	if e, err = s.claim(1, runIDFor(1), dir); err != nil || e.State != StateClaimed || e.RunID != runIDFor(1) || e.Dir != dir {
		t.Fatalf("claim = (%+v, %v), want goal 1 claimed for run %s in %s", e, err, runIDFor(1), dir)
	}
	_, err = s.claim(1, runIDFor(2), dir)
	refused("a second claim", err)
	_, err = s.Remove(1)
	refused("Remove of a claimed goal", err)

	if e, err = s.settle(1, StateWaiting, "the plan is at plan.md"); err != nil || e.State != StateWaiting {
		t.Fatalf("settle waiting = (%+v, %v)", e, err)
	}
	if e, err = s.Approve(1); err != nil || e.State != StateApproved {
		t.Fatalf("Approve = (%+v, %v)", e, err)
	}
	if e, err = s.settle(1, StateFinished, ""); err != nil || e.State != StateFinished {
		t.Fatalf("settle finished = (%+v, %v)", e, err)
	}
	if _, err := s.Approve(7); !errors.Is(err, ErrNoSuchGoal) {
		t.Errorf("Approve of a goal nobody added = %v, want ErrNoSuchGoal", err)
	}
	if _, err := s.Add(" \n", false); err == nil {
		t.Error("Add of an empty goal succeeded")
	}

	q, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got, _ := q.Entry(1); got.State != StateFinished || got.RunID != runIDFor(1) {
		t.Errorf("goal 1 read back as %+v", got)
	}
}

// TestRemoveOnlyQueuedEntries: `belay queue rm` takes back a goal nothing has
// started. Once a worker has claimed one, its run exists or is about to, and
// removing it from the queue would orphan both.
func TestRemoveOnlyQueuedEntries(t *testing.T) {
	ws := t.TempDir()
	s := mustOpen(t, ws)
	for range 3 {
		if _, err := s.Add("a goal", false); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if _, err := s.claim(2, runIDFor(2), filepath.Join(s.WorkRoot(), runIDFor(2))); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if e, err := s.Remove(1); err != nil || e.State != StateRemoved {
		t.Errorf("Remove(1) = (%+v, %v), want it removed", e, err)
	}
	var wrong *StateError
	if _, err := s.Remove(2); !errors.As(err, &wrong) || wrong.ID != 2 || wrong.State != StateClaimed {
		t.Errorf("Remove(2) = %v, want a *StateError saying goal 2 is claimed", err)
	}
	if _, err := s.Remove(1); !errors.As(err, &wrong) || wrong.State != StateRemoved {
		t.Errorf("Remove(1) again = %v, want a *StateError saying it is already removed", err)
	}
	q, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var states []State
	for _, e := range q.Entries {
		states = append(states, e.State)
	}
	if want := []State{StateRemoved, StateClaimed, StateQueued}; !slices.Equal(states, want) {
		t.Errorf("states = %v, want %v", states, want)
	}
}

// TestOneGoalInHandAtATime: a worker claims a goal only once the one before
// it is settled. The worker's own order already guarantees it; the store
// refuses it as well, so a mistake there cannot start two goals.
func TestOneGoalInHandAtATime(t *testing.T) {
	ws := t.TempDir()
	s := mustOpen(t, ws)
	for range 2 {
		if _, err := s.Add("a goal", false); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if _, err := s.claim(1, runIDFor(1), filepath.Join(s.WorkRoot(), runIDFor(1))); err != nil {
		t.Fatalf("claim(1): %v", err)
	}
	if _, err := s.claim(2, runIDFor(2), filepath.Join(s.WorkRoot(), runIDFor(2))); !errors.Is(err, ErrGoalInHand) {
		t.Fatalf("claim(2) while goal 1 is in hand = %v, want ErrGoalInHand", err)
	}
	if _, err := s.settle(1, StateFailed, ""); err != nil {
		t.Fatalf("settle(1): %v", err)
	}
	if _, err := s.claim(2, runIDFor(2), filepath.Join(s.WorkRoot(), runIDFor(2))); err != nil {
		t.Fatalf("claim(2) once goal 1 settled = %v", err)
	}
}

// TestATornFinalLineIsHealedBeforeTheNextAppend: a process killed while it
// appended leaves half a line at the end of the log. Reading must not choke
// on it or change the file; the next writer must cut it off before it adds
// its own line, or the two would fuse into one unreadable line.
func TestATornFinalLineIsHealedBeforeTheNextAppend(t *testing.T) {
	ws := t.TempDir()
	s := mustOpen(t, ws)
	if _, err := s.Add("first", false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	f, err := os.OpenFile(s.LogPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	if _, err := f.WriteString(`{"seq":2,"event":"entry_add`); err != nil {
		t.Fatalf("tear the log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	torn := readFile(t, s.LogPath())

	q, err := s.Read()
	if err != nil || len(q.Entries) != 1 {
		t.Fatalf("Read of a torn log = (%d goals, %v), want the 1 goal written whole", len(q.Entries), err)
	}
	if got := readFile(t, s.LogPath()); got != torn {
		t.Error("Read changed the log; only a writer may heal it")
	}

	e, err := s.Add("second", false)
	if err != nil || e.ID != 2 {
		t.Fatalf("Add after the tear = (%+v, %v), want goal 2", e, err)
	}
	log := readFile(t, s.LogPath())
	if strings.Contains(log, "entry_add`") || strings.Count(log, "\n") != 2 || !strings.HasSuffix(log, "\n") {
		t.Errorf("the log after healing is not two whole lines:\n%s", log)
	}
}

// TestACorruptLogIsRefusedNotRepaired: damage anywhere but the last line is
// not something an interrupted append can do. belay will not read around it
// and will not write after it.
func TestACorruptLogIsRefusedNotRepaired(t *testing.T) {
	ws := t.TempDir()
	s := mustOpen(t, ws)
	for range 2 {
		if _, err := s.Add("a goal", false); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	lines := strings.SplitAfter(readFile(t, s.LogPath()), "\n")
	damaged := "not json at all\n" + lines[1]
	if err := os.WriteFile(s.LogPath(), []byte(damaged), 0o600); err != nil {
		t.Fatalf("damage the log: %v", err)
	}

	if _, err := s.Read(); !errors.Is(err, ErrCorruptQueue) {
		t.Errorf("Read of a damaged log = %v, want ErrCorruptQueue", err)
	}
	if _, err := s.Add("a third goal", false); !errors.Is(err, ErrCorruptQueue) {
		t.Errorf("Add onto a damaged log = %v, want ErrCorruptQueue", err)
	}
	if got := readFile(t, s.LogPath()); got != damaged {
		t.Errorf("belay wrote to a damaged log:\n%s", got)
	}
}

// TestReadingAQueueNobodyMadeMakesNothing: `belay runs` and `belay queue
// list` read a queue that may not exist, and reading must not create one.
func TestReadingAQueueNobodyMadeMakesNothing(t *testing.T) {
	ws := t.TempDir()
	s := mustOpen(t, ws)
	q, err := s.Read()
	if err != nil || len(q.Entries) != 0 {
		t.Fatalf("Read of no queue = (%+v, %v), want an empty queue", q, err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".belay")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reading the queue created %s (stat error %v)", filepath.Join(ws, ".belay"), err)
	}
}

// readFile returns a file's contents, failing the test if it cannot.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path inside this test's own t.TempDir()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
