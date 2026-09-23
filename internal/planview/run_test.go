package planview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// notTerminals opens one of each kind of file that is not a terminal and
// that belay's standard streams are, in practice: a pipe (`belay plan |
// less`, a CI runner), /dev/null (`belay run </dev/null` from cron), and a
// regular file (a redirect).
func notTerminals(t *testing.T) map[string]*os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	regular, err := os.Create(filepath.Join(t.TempDir(), "transcript.txt"))
	if err != nil {
		t.Fatalf("create a regular file: %v", err)
	}
	t.Cleanup(func() {
		for _, f := range []*os.File{r, w, devNull, regular} {
			_ = f.Close()
		}
	})
	return map[string]*os.File{"a pipe": r, "/dev/null": devNull, "a regular file": regular}
}

// TestIsTerminalAsksTheTerminalDriver: /dev/null is a character device, so
// a check on the file mode -- the one belay's colour decision makes -- would
// take it for a terminal and open the viewer under cron. IsTerminal asks the
// terminal driver instead.
func TestIsTerminalAsksTheTerminalDriver(t *testing.T) {
	for name, f := range notTerminals(t) {
		if IsTerminal(f) {
			t.Errorf("IsTerminal(%s) = true, want false", name)
		}
	}
	if IsTerminal(nil) {
		t.Error("IsTerminal(nil) = true, want false")
	}
	info, err := os.Stat(os.DevNull)
	if err != nil {
		t.Fatalf("stat %s: %v", os.DevNull, err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("%s is not a character device here, so this test proves less than it claims", os.DevNull)
	}
}

// TestRunRefusesFilesThatAreNotTerminals: Run checks for itself, before it
// builds a program, that it was handed a terminal to read and one to draw
// on. Left to bubbletea, a missing input opens /dev/tty and takes the
// controlling terminal from under a piped or cron invocation; handed a pipe,
// it would wait on the pipe for keys nobody will type. Either way belay
// would hang where it used to print the plan and exit.
func TestRunRefusesFilesThatAreNotTerminals(t *testing.T) {
	files := notTerminals(t)
	cases := []struct {
		name    string
		in, out *os.File
	}{
		{name: "input is a pipe", in: files["a pipe"], out: files["/dev/null"]},
		{name: "output is a pipe", in: files["/dev/null"], out: files["a pipe"]},
		{name: "both are /dev/null", in: files["/dev/null"], out: files["/dev/null"]},
		{name: "both are a regular file", in: files["a regular file"], out: files["a regular file"]},
		{name: "no files at all", in: nil, out: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			var (
				decision Decision
				err      error
			)
			go func() {
				defer close(done)
				decision, err = Run(context.Background(), tc.in, tc.out, Options{})
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return: it went on to start a program on files that are not a terminal")
			}
			if !errors.Is(err, ErrNotATerminal) {
				t.Errorf("Run = %v, want ErrNotATerminal", err)
			}
			if decision != (Decision{}) {
				t.Errorf("Run decided %+v, want nothing decided", decision)
			}
		})
	}
}

// TestAnInterruptQuitsTheWaySafeForTheTerminal: bubbletea raises an
// interrupt for a SIGINT from outside, and ends the program on it the way
// that races its own key reader (see Run). The filter turns it into an
// ordinary quit, and leaves every other message alone.
func TestAnInterruptQuitsTheWaySafeForTheTerminal(t *testing.T) {
	if got := quitOnInterrupt(nil, tea.InterruptMsg{}); got != (tea.QuitMsg{}) {
		t.Errorf("quitOnInterrupt(InterruptMsg) = %#v, want QuitMsg", got)
	}
	key := tea.KeyPressMsg{Code: 'q', Text: "q"}
	if got := quitOnInterrupt(nil, key); got != key {
		t.Errorf("quitOnInterrupt(%v) = %#v, want the key untouched", key, got)
	}
}
