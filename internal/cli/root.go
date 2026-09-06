// Package cli assembles belay's cobra command tree. Subcommands register
// themselves from their own packages via init(); only the root and version
// commands are defined here.
//
// # Two streams, on purpose
//
// A command writes the story of the run — what belay is doing, where it is in
// the graph, what it has cost — to stdout, and everything belay says about
// itself — its structured log, its warnings — to stderr. Piping stdout to a
// file therefore captures a readable transcript and nothing else, which is
// also why nothing on stdout is coloured unless stdout is a terminal.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/graph"
)

// Version information, injected via ldflags during build.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

// ExitError carries a process exit status out of a command, so the status
// belay reports is chosen where the outcome is known instead of inferred from
// an error string.
//
// Message is already written for a person to read: [Execute] prints it
// verbatim and then exits with Code. Err carries the machine-readable cause so
// that errors.Is still works on the way out.
type ExitError struct {
	// Code is the process exit status. It is one of the internal/graph
	// Exit* constants, which are belay's user-visible contract.
	Code int
	// Message is printed before exiting. An empty Message prints nothing,
	// which is what a command that has already explained itself wants.
	Message string
	// Err is the underlying cause, or nil when there is none.
	Err error
}

// Error implements error. It prefers Message, which is the sentence written
// for the person running belay, and falls back to the underlying cause.
func (e *ExitError) Error() string {
	switch {
	case e.Message != "":
		return e.Message
	case e.Err != nil:
		return e.Err.Error()
	default:
		return fmt.Sprintf("belay exited with status %d", e.Code)
	}
}

// Unwrap reports the underlying cause.
func (e *ExitError) Unwrap() error { return e.Err }

// said returns what is left to print, which is nothing when the command has
// already said everything it had to say.
func (e *ExitError) said() string {
	switch {
	case e.Message != "":
		return e.Message
	case e.Err != nil:
		return e.Err.Error()
	default:
		return ""
	}
}

// rootFlags holds the settings every belay command shares. They are
// persistent flags, so they are accepted either before or after the
// subcommand: `belay --dry-run run "..."` and `belay run --dry-run "..."` are
// the same command.
var rootFlags struct {
	configPath string
	verbose    bool
	noColor    bool
	dryRun     bool
	workspace  string
}

var rootCmd = &cobra.Command{
	Use:   "belay",
	Short: "Durable, resumable orchestrator for autonomous coding agents",
	Long: `belay is a durable, resumable orchestrator for autonomous coding agents.

Instead of one unreliable AI call, it runs a state-machine graph where a dispatcher
persists state after every node, so a crashed run resumes from the last completed
node instead of restarting.`,
	SilenceUsage: true,
	// Errors are printed by Execute, in belay's own words and on belay's own
	// stream, rather than by cobra as "Error: <go error>".
	SilenceErrors: true,
}

func init() {
	f := rootCmd.PersistentFlags()
	f.StringVar(&rootFlags.configPath, "config", "",
		"read settings from this file instead of the belay.yaml belay would find on its own")
	f.BoolVar(&rootFlags.verbose, "verbose", false,
		"also print belay's own log to stderr while it works")
	f.BoolVar(&rootFlags.noColor, "no-color", false,
		"never colour the output (setting NO_COLOR, or piping belay to a file, does the same)")
	f.BoolVar(&rootFlags.dryRun, "dry-run", false,
		"say what belay would do, then stop without creating anything or spending anything")
	// --workspace is persistent so that every command names the repository
	// the same way. It was briefly defined three times, with three different
	// descriptions and one command missing it entirely -- the exact
	// fragmentation ADR 0010 exists to catch.
	f.StringVar(&rootFlags.workspace, "workspace", ".",
		"the repository belay reads and edits")

	rootCmd.AddCommand(versionCmd)
	// NOTE: Other subcommands (run, resume, runs, timeline) are registered by
	// their respective packages via init(). Do not add them here.
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Printf("belay version %s (commit: %s, built: %s)\n", Version, Commit, BuildDate)
	},
}

// Execute runs the root command and turns its result into a process exit
// status.
//
// A command that knows how its run ended returns an [ExitError] carrying the
// exit code the internal/graph package defines for that outcome; Execute
// prints its message and exits with that code. Anything else is a mistake in
// how the command was typed, which Execute reports with the same status as an
// ordinary failure and a pointer at --help.
func Execute() error {
	err := rootCmd.Execute()
	if err == nil {
		return nil
	}

	out := rootCmd.ErrOrStderr()
	var exit *ExitError
	if errors.As(err, &exit) {
		// A command that already printed its explanation carries neither a
		// Message nor a cause, and gets the last word: repeating "belay
		// exited with status 1" under a refusal a person has just read
		// undoes the work of writing the refusal.
		if msg := strings.TrimSpace(exit.said()); msg != "" {
			_, _ = fmt.Fprintln(out, msg)
		}
		os.Exit(exit.Code)
	}

	_, _ = fmt.Fprintf(out, "belay: %s\nRun \"belay --help\" to see the commands and flags belay accepts.\n", err)
	os.Exit(graph.ExitFailed)
	return nil
}

// ui is one command invocation's output surface: the two streams it writes to
// and whether they may carry colour.
//
// Writes are serialized. The signal handler that announces "stopping" runs on
// its own goroutine while the dispatcher is still narrating from the main
// one, and two goroutines interleaving inside a single Fprintf is how a
// reassuring message arrives shredded.
type ui struct {
	mu      sync.Mutex
	out     io.Writer
	err     io.Writer
	color   bool
	verbose bool
}

// newUI builds the output surface for cmd from the root's persistent flags.
func newUI(cmd *cobra.Command) *ui {
	out := cmd.OutOrStdout()
	return &ui{
		out:     out,
		err:     cmd.ErrOrStderr(),
		color:   colorEnabled(out, rootFlags.noColor),
		verbose: rootFlags.verbose,
	}
}

// say writes one line of the run's story to stdout.
//
// A write error is dropped on purpose. The usual one is a closed pipe — `belay
// run | head` — and turning that into a cascade of error handling would end a
// run that is otherwise fine, over an audience that has already left.
func (u *ui) say(format string, a ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, _ = fmt.Fprintf(u.out, format+"\n", a...)
}

// note writes one line about belay itself to stderr, where it cannot
// contaminate a piped transcript.
func (u *ui) note(format string, a ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, _ = fmt.Fprintf(u.err, format+"\n", a...)
}

// emph marks the only two things belay emphasises: what it is doing now, and
// what it has cost. Everything else is plain, because an interface that
// shouts everywhere shouts nowhere.
func (u *ui) emph(s string) string {
	if !u.color || s == "" {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}

// colorEnabled reports whether w may carry ANSI escape sequences.
//
// Any one of three things alone switches colour off: the --no-color flag, a
// non-empty NO_COLOR in the environment (https://no-color.org/, "when present
// and not an empty string, regardless of its value"), and a destination that
// is not a character device. The last is what makes `belay run > run.log`
// produce a file of plain text rather than a file of escape sequences, and it
// is also why every test in this package gets uncoloured output for free: a
// bytes.Buffer is not an *os.File.
func colorEnabled(w io.Writer, disabled bool) bool {
	if disabled {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	// A "dumb" terminal is one that has told us it cannot render escapes.
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// workspaceDirOf resolves the repository a command should act on.
//
// It reads the value off cmd rather than from rootFlags, because
// cobra's Flags() already includes a parent's persistent flags and a package
// global would be shared mutable state across parallel tests. The result is
// made absolute here: state.NewLayout refuses a relative workspace, and the
// path is printed, so "." would tell a reader nothing about where belay
// actually looked.
func workspaceDirOf(cmd *cobra.Command) (string, error) {
	dir, err := cmd.Flags().GetString("workspace")
	if err != nil || dir == "" {
		cwd, wderr := os.Getwd()
		if wderr != nil {
			return "", fmt.Errorf("cli: locate the current directory: %w", wderr)
		}
		dir = cwd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("cli: resolve %s: %w", dir, err)
	}
	return abs, nil
}
