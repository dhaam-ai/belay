//go:build darwin || linux

package planview

import (
	"bytes"
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// drawing collects what the viewer draws, read off the master side of a
// pseudo-terminal.
type drawing struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *drawing) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *drawing) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// runResult is what Run returned.
type runResult struct {
	decision Decision
	err      error
}

// terminal is a viewer running on a pseudo-terminal, as it runs on the
// terminal a person types into.
type terminal struct {
	master, slave *os.File
	drawn         *drawing
	done          chan runResult
	cancel        context.CancelFunc
	before        *term.State
}

// startOnTerminal opens a pseudo-terminal of 80 by 24 cells and runs the
// viewer on it.
func startOnTerminal(t *testing.T, opts Options) *terminal {
	t.Helper()
	master, slave := openPTY(t)
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		t.Fatalf("size the pseudo-terminal: %v", err)
	}
	before, err := term.GetState(slave.Fd())
	if err != nil {
		t.Fatalf("read the terminal's mode: %v", err)
	}
	tm := &terminal{master: master, slave: slave, drawn: &drawing{}, done: make(chan runResult, 1), before: before}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(tm.drawn, master) //nolint:errcheck // ends with EIO when the slave closes
	}()
	ctx, cancel := context.WithCancel(context.Background())
	tm.cancel = cancel
	go func() {
		d, err := Run(ctx, slave, slave, opts)
		tm.done <- runResult{decision: d, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		_ = slave.Close()
		_ = master.Close()
		<-copied
	})
	return tm
}

// waitUntil polls cond for up to five seconds.
func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// result waits for Run to return.
func (tm *terminal) result(t *testing.T) runResult {
	t.Helper()
	select {
	case r := <-tm.done:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not return; the terminal shows:\n%q", tm.drawn.String())
		return runResult{}
	}
}

// assertRestored checks the terminal is in the mode belay found it in:
// raw mode left behind is a shell that no longer echoes what is typed.
func (tm *terminal) assertRestored(t *testing.T) {
	t.Helper()
	after, err := term.GetState(tm.slave.Fd())
	if err != nil {
		t.Fatalf("read the terminal's mode: %v", err)
	}
	if !reflect.DeepEqual(tm.before, after) {
		t.Error("the viewer left the terminal in a different mode from the one it found")
	}
}

// TestRunTakesOverATerminalAndGivesItBack runs the viewer on a real
// (pseudo-)terminal: it draws, it waits for the person rather than
// returning, and when it is stopped the terminal is back in the mode it was
// in, which is what a shell prompt after belay depends on.
func TestRunTakesOverATerminalAndGivesItBack(t *testing.T) {
	tm := startOnTerminal(t, Options{})

	select {
	case r := <-tm.done:
		t.Fatalf("Run returned at once (%+v, %v); on a terminal it waits for the person", r.decision, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	waitUntil(t, func() bool { return tm.drawn.String() != "" }, "the viewer drew nothing on the terminal")

	tm.cancel()
	r := tm.result(t)
	if r.decision != (Decision{}) {
		t.Errorf("Run decided %+v when it was stopped, want nothing decided", r.decision)
	}
	tm.assertRestored(t)
}

// TestAPersonCanReadAndLeaveThePlanOnARealTerminal drives the viewer the
// way a person does: over a pseudo-terminal, with the keys decoded from
// bytes by the input reader and the page drawn by the renderer. Everything
// else in this package drives the model directly, which cannot show that
// raw mode, the decoder and the renderer are wired together at all.
//
// A guard, not RED: the behaviour it pins came with the model.
func TestAPersonCanReadAndLeaveThePlanOnARealTerminal(t *testing.T) {
	const plan = "## Steps\n\n1. Add a dryRun field to runOptions.\n2. Bind the flag in newRunCmd.\n"
	tm := startOnTerminal(t, Options{
		RunID: "20260921T101500Z-1f2e3d4c5b6a",
		Goal:  "add a --dry-run flag",
		Path:  "/work/.belay/runs/20260921T101500Z-1f2e3d4c5b6a/artifacts/plan.md",
		Plan:  []byte(plan),
		Mode:  Review,
	})

	// It draws the plan, on the alternate screen so the run's scrollback is
	// still there afterwards.
	waitUntil(t, func() bool { return strings.Contains(tm.drawn.String(), "Add a dryRun field") },
		"the viewer never drew the plan on the terminal")
	if drawn := tm.drawn.String(); !strings.Contains(drawn, "\x1b[?1049h") {
		t.Errorf("the viewer did not take the alternate screen:\n%q", drawn)
	}
	for _, want := range []string{"20260921T101500Z-1f2e3d4c5b6a", "add a --dry-run flag", "c approve", "q leave"} {
		if !strings.Contains(tm.drawn.String(), want) {
			t.Errorf("the terminal does not show %q", want)
		}
	}

	// A key typed into the terminal reaches the model: it is read as a byte
	// and decoded by the input reader, not handed over by a test. ? is the
	// key to watch for, because the page it opens is all new text; the
	// renderer draws only what changed, so moving the cursor one step down
	// redraws two characters, which proves nothing to a reader of the
	// stream.
	if _, err := tm.master.WriteString("?"); err != nil {
		t.Fatalf("type ?: %v", err)
	}
	waitUntil(t, func() bool { return strings.Contains(tm.drawn.String(), "put belay in the background") },
		"? did not open the list of keys")

	// The first q closes that list, the second leaves.
	if _, err := tm.master.WriteString("qq"); err != nil {
		t.Fatalf("type q: %v", err)
	}
	r := tm.result(t)
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
	if r.decision != (Decision{Action: ActionLeave}) {
		t.Errorf("q decided %+v, want to leave the run as it is", r.decision)
	}
	if !strings.Contains(tm.drawn.String(), "\x1b[?1049l") {
		t.Error("the viewer did not give the alternate screen back")
	}
	tm.assertRestored(t)
}
