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
// always hands the program its input, its output and its context.
//
// It also always hands it a colour profile, rather than letting the library
// detect one. The library's detection reads NO_COLOR as a boolean, so
// NO_COLOR=yes leaves colour on, which no-color.org says it must not, and it
// runs `tmux info` as a subprocess inside tmux. The profile is ASCII: no
// colour reaches the terminal.
//
// An error means the viewer could not be shown or stopped working; nothing
// was decided, and whatever the person had not saved is gone. A SIGINT from
// outside — Ctrl-C itself is a key while the viewer is open — ends it as a
// person leaving without saving would, with no error.
func Run(ctx context.Context, in, out *os.File, opts Options) (Decision, error) {
	if !IsTerminal(in) || !IsTerminal(out) {
		return Decision{}, ErrNotATerminal
	}
	program := tea.NewProgram(New(opts),
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithColorProfile(colorprofile.ASCII),
	)
	final, err := program.Run()
	switch {
	case errors.Is(err, tea.ErrInterrupted):
		return Decision{}, nil
	case err != nil:
		return Decision{}, fmt.Errorf("planview: %w", err)
	}
	m, ok := final.(Model)
	if !ok {
		return Decision{}, fmt.Errorf("planview: the viewer ended as a %T", final)
	}
	return m.Decision(), nil
}
