package planview

import (
	"context"
	"errors"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"
)

// ErrNotATerminal reports that Run was not handed a terminal to read from
// and one to draw on, so it showed nothing.
var ErrNotATerminal = errors.New("planview: not a terminal")

// IsTerminal reports whether f is a terminal, by asking the terminal driver.
//
// It does not look at the file's mode. /dev/null is a character device, so a
// mode check — the one belay's colour decision makes — takes it for a
// terminal, and `belay run </dev/null` under cron would open a viewer nobody
// can answer. It is also the check bubbletea itself makes, so belay and the
// library agree about what a terminal is.
func IsTerminal(f *os.File) bool {
	return f != nil && term.IsTerminal(f.Fd())
}

// Run shows the viewer on the terminal in and out and returns what the
// person decided, once they have left it.
//
// It refuses, with ErrNotATerminal, unless both in and out are terminals,
// and it checks before it builds a program. Left to itself bubbletea reads
// os.Stdin, and opens /dev/tty when that is not a terminal: it would take
// the controlling terminal from under a piped or scheduled invocation, or
// sit waiting on a pipe for keys nobody will type. For the same reason it
// always hands the program its input and its output.
//
// It also always hands it a colour profile, rather than letting the library
// detect one. The library's detection reads NO_COLOR as a boolean, so
// NO_COLOR=yes leaves colour on, which no-color.org says it must not, and it
// runs `tmux info` as a subprocess inside tmux. The profile is ASCII: no
// colour reaches the terminal.
//
// Every way the viewer stops is bubbletea's graceful quit. The library has
// a second way, for a cancelled context or a SIGINT, and in v2.0.9 that one
// closes the terminal reader while the goroutine reading keys is still
// using it (tea.go, Program.shutdown, which waits for the read loop only
// when it is not killing the program). So Run does not hand the program its
// context: it quits the program itself when ctx is done, and returns ctx's
// error. And a SIGINT from outside — Ctrl-C itself is a key while the
// viewer is open — is turned into a quit, which leaves the way a person
// leaving without saving would, with no error.
//
// Any other error means the viewer could not be shown or stopped working;
// nothing was decided, and whatever the person had not saved is gone.
func Run(ctx context.Context, in, out *os.File, opts Options) (Decision, error) {
	if !IsTerminal(in) || !IsTerminal(out) {
		return Decision{}, ErrNotATerminal
	}
	program := tea.NewProgram(New(opts),
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithColorProfile(colorprofile.ASCII),
		tea.WithFilter(quitOnInterrupt),
	)
	stop := context.AfterFunc(ctx, program.Quit)
	defer stop()
	final, err := program.Run()
	switch {
	case err != nil:
		return Decision{}, fmt.Errorf("planview: %w", err)
	case ctx.Err() != nil:
		return Decision{}, fmt.Errorf("planview: %w", ctx.Err())
	}
	m, ok := final.(Model)
	if !ok {
		return Decision{}, fmt.Errorf("planview: the viewer ended as a %T", final)
	}
	return m.Decision(), nil
}

// quitOnInterrupt turns the interrupt bubbletea raises for a SIGINT into an
// ordinary quit, so the viewer stops the one way that is safe (see Run).
func quitOnInterrupt(_ tea.Model, msg tea.Msg) tea.Msg {
	if _, ok := msg.(tea.InterruptMsg); ok {
		return tea.QuitMsg{}
	}
	return msg
}
