package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/graph"
)

func TestColorEnabled(t *testing.T) {
	// A pipe is the shape of `belay run > run.log`: a real *os.File that is
	// not a character device, which is exactly the case a naive "is it an
	// *os.File" check gets wrong. /dev/null is the opposite: a character
	// device, so it stands in for a terminal without needing one.
	pipe := func(t *testing.T) io.Writer {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		t.Cleanup(func() {
			_ = r.Close()
			_ = w.Close()
		})
		return w
	}
	devNull := func(t *testing.T) io.Writer {
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open %s: %v", os.DevNull, err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	buffer := func(*testing.T) io.Writer { return &bytes.Buffer{} }

	tests := []struct {
		name     string
		writer   func(*testing.T) io.Writer
		disabled bool
		noColor  string
		term     string
		want     bool
	}{
		{name: "buffer is never a terminal", writer: buffer},
		{name: "pipe is not a terminal", writer: pipe},
		{name: "terminal gets colour", writer: devNull, want: true},
		{name: "flag disables a terminal", writer: devNull, disabled: true},
		{name: "NO_COLOR disables a terminal", writer: devNull, noColor: "1"},
		{name: "empty NO_COLOR leaves a terminal alone", writer: devNull, noColor: "", want: true},
		{name: "dumb terminal disables", writer: devNull, term: "dumb"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", tc.noColor)
			t.Setenv("TERM", tc.term)

			if got := colorEnabled(tc.writer(t), tc.disabled); got != tc.want {
				t.Errorf("colorEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUIEmphIsPlainWithoutColor(t *testing.T) {
	var out bytes.Buffer
	u := &ui{out: &out, err: &out, color: false}
	if got := u.emph("$0.03 of $5.00"); got != "$0.03 of $5.00" {
		t.Errorf("emph() = %q, want the string unchanged", got)
	}

	u.color = true
	got := u.emph("$0.03 of $5.00")
	if !strings.Contains(got, "\x1b[1m") || !strings.Contains(got, "\x1b[0m") {
		t.Errorf("emph() with colour = %q, want it wrapped in bold escapes", got)
	}
	if u.emph("") != "" {
		t.Error("emph(\"\") should stay empty rather than emit bare escapes")
	}
}

func TestUIStreamsAreSeparate(t *testing.T) {
	var out, errOut bytes.Buffer
	u := &ui{out: &out, err: &errOut}

	u.say("the run's story")
	u.note("belay talking about itself")

	if got := out.String(); got != "the run's story\n" {
		t.Errorf("stdout = %q, want only the run's story", got)
	}
	if got := errOut.String(); got != "belay talking about itself\n" {
		t.Errorf("stderr = %q, want only belay's own note", got)
	}
}

func TestExitError(t *testing.T) {
	cause := errors.New("underlying cause")

	tests := []struct {
		name string
		err  *ExitError
		want string
	}{
		{
			name: "message wins",
			err:  &ExitError{Code: graph.ExitFailed, Message: "belay could not read the plan.", Err: cause},
			want: "belay could not read the plan.",
		},
		{
			name: "falls back to the cause",
			err:  &ExitError{Code: graph.ExitAborted, Err: cause},
			want: "underlying cause",
		},
		{
			name: "never renders empty",
			err:  &ExitError{Code: graph.ExitPaused},
			want: "belay exited with status 4",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}

	wrapped := &ExitError{Code: graph.ExitFailed, Message: "nope", Err: cause}
	if !errors.Is(wrapped, cause) {
		t.Error("errors.Is should see through ExitError to its cause")
	}
}

func TestRootPersistentFlags(t *testing.T) {
	// The acceptance criterion `belay --dry-run run "..."` only parses if
	// --dry-run is persistent on the root, so assert the flag's home rather
	// than only its existence.
	for _, name := range []string{"config", "verbose", "no-color", "dry-run"} {
		if rootCmd.PersistentFlags().Lookup(name) == nil {
			t.Errorf("--%s must be a persistent flag on the root command", name)
		}
	}
}

// Every command names the repository the same way.
//
// --workspace was briefly defined three times with three different
// descriptions, and `belay runs` had none at all -- the exact fragmentation
// ADR 0010 exists to catch, and the predictable result of three people
// writing commands in parallel. One persistent definition, asserted here so
// it cannot drift back.
func TestWorkspaceFlagIsSharedByEveryCommand(t *testing.T) {
	const want = "the repository belay reads and edits"

	for _, name := range []string{"run", "resume", "runs", "timeline"} {
		var found *cobra.Command
		for _, c := range rootCmd.Commands() {
			if c.Name() == name {
				found = c
				break
			}
		}
		if found == nil {
			t.Errorf("command %q is not registered on the root", name)
			continue
		}
		// InheritedFlags exposes the root's PersistentFlags. Flags() merges
		// them in too, but only lazily once the tree has been executed, so
		// asking directly keeps this test independent of that ordering.
		f := found.InheritedFlags().Lookup("workspace")
		if f == nil {
			f = found.Flags().Lookup("workspace")
		}
		if f == nil {
			t.Errorf("%s: no --workspace flag; every command must name the repository the same way", name)
			continue
		}
		if f.Usage != want {
			t.Errorf("%s: --workspace usage = %q, want the shared %q", name, f.Usage, want)
		}
		if f.DefValue != "." {
			t.Errorf("%s: --workspace default = %q, want %q", name, f.DefValue, ".")
		}
	}

	// Defined once, on the root, so there is nothing to drift.
	if rootCmd.PersistentFlags().Lookup("workspace") == nil {
		t.Error("--workspace is not a persistent root flag; per-command copies will diverge again")
	}
}
