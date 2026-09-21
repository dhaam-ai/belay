//go:build darwin || linux

package planview

import (
	"bytes"
	"context"
	"io"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// screen collects what the viewer draws, read off the master side of a
// pseudo-terminal.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *screen) String() string {
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
	drawn         *screen
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
	tm := &terminal{master: master, slave: slave, drawn: &screen{}, done: make(chan runResult, 1), before: before}
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
