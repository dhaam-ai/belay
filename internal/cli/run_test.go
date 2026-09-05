package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/nodes/approve"
	"github.com/belay-dev/belay/internal/state"
)

// runCLI executes the command tree the way a shell would, with both streams
// captured.
//
// The command tree is a package-level singleton, because subcommands register
// themselves through init(); flag values therefore outlive one invocation and
// have to be reset by hand. Tests using it must not run in parallel.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	rootFlags.configPath, rootFlags.verbose, rootFlags.noColor, rootFlags.dryRun = "", false, false, false
	rootFlags.workspace, runFlags.cassette = ".", ""

	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})

	resetHelpFlags()
	err = rootCmd.Execute()
	return out.String(), errOut.String(), err
}

// resetHelpFlags clears the --help flag cobra sets on whichever command last
// printed help. Without it, one `belay run --help` in a test file turns every
// later invocation into another help screen.
func resetHelpFlags() {
	for _, cmd := range append([]*cobra.Command{rootCmd}, rootCmd.Commands()...) {
		_ = cmd.Flags().Set("help", "false")
	}
}

// assertPlainText is the non-TTY contract: piping belay anywhere must produce
// text a person can read, with no escape sequences in it.
func assertPlainText(t *testing.T, label, s string) {
	t.Helper()
	if strings.ContainsRune(s, '\x1b') {
		t.Errorf("%s contains an ANSI escape sequence; output to a non-terminal must be plain:\n%q", label, s)
	}
}

func TestRunHelpReadsAsEnglish(t *testing.T) {
	out, _, err := runCLI(t, "run", "--help")
	if err != nil {
		t.Fatalf("belay run --help: %v", err)
	}
	assertPlainText(t, "belay run --help", out)

	// Every one of these is a question a person has before they let an agent
	// loose on their repository.
	want := []string{
		"what you want built",
		"budget ceiling set in belay.yaml",
		"Ctrl-C",
		"belay resume",
		"--dry-run",
		"without creating anything or spending anything",
	}
	for _, phrase := range want {
		if !strings.Contains(out, phrase) {
			t.Errorf("belay run --help never mentions %q", phrase)
		}
	}
}

func TestResumeHelpReadsAsEnglish(t *testing.T) {
	out, _, err := runCLI(t, "resume", "--help")
	if err != nil {
		t.Fatalf("belay resume --help: %v", err)
	}
	assertPlainText(t, "belay resume --help", out)

	for _, phrase := range []string{
		"most recent run",
		"says which one it picked",
		"resuming is how you approve",
		"belay runs",
	} {
		if !strings.Contains(out, phrase) {
			t.Errorf("belay resume --help never mentions %q", phrase)
		}
	}
}

// TestDryRunCreatesNothing is acceptance criterion 3: a full plan, printed
// against a git-less directory, with nothing left behind.
func TestDryRunCreatesNothing(t *testing.T) {
	ws := goWorkspace(t)
	before := listTree(t, ws)

	out, _, err := runCLI(t, "--dry-run", "run", "--workspace", ws, "add tests")
	if err != nil {
		t.Fatalf("belay --dry-run run: %v\n%s", err, out)
	}
	assertPlainText(t, "dry run", out)

	if _, statErr := os.Stat(filepath.Join(ws, ".belay")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf(".belay exists after a dry run; a dry run must create nothing")
	}
	if after := listTree(t, ws); len(after) != len(before) {
		t.Errorf("the workspace changed during a dry run:\n before %v\n after  %v", before, after)
	}

	// The whole plan, not a summary of it: settings, project, tools, ceiling,
	// route, and what a real run would write.
	for _, phrase := range []string{
		"This is a dry run",
		"add tests",
		ws,
		"belay found: go",
		"$5.00 or 2,000,000 tokens",
		"belay stops the run before the next step",
		"claude-code",
		"go-test",
		"golangci-lint",
		"plan → approve → code → write → test → fix → review → done",
		"approve stops the run so you can read the plan",
		filepath.Join(ws, ".belay", "runs", "<run-id>"),
		"Nothing was created and nothing was spent",
	} {
		if !strings.Contains(out, phrase) {
			t.Errorf("the dry run never says %q; got:\n%s", phrase, out)
		}
	}
}

// TestDryRunNeverPrintsCredentialValues guards the one thing a plan must not
// leak while reporting on the environment it read.
func TestDryRunNeverPrintsCredentialValues(t *testing.T) {
	secret := notARealKey("sk-ant-", "api03-notarealkeybutlongenoughtomatter")
	t.Setenv(anthropicKeyEnv, secret)

	out, _, err := runCLI(t, "--dry-run", "run", "--workspace", goWorkspace(t), "add tests")
	if err != nil {
		t.Fatalf("belay --dry-run run: %v", err)
	}
	if strings.Contains(out, secret) {
		t.Fatal("the dry run printed the API key")
	}
	if !strings.Contains(out, anthropicKeyEnv+" is set") {
		t.Errorf("the dry run should say whether the key is set; got:\n%s", out)
	}
}

// TestRunRefusesMissingWorkspace is acceptance criterion 4.
func TestRunRefusesMissingWorkspace(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-repo")

	_, _, err := runCLI(t, "run", "--workspace", missing, "add tests")
	if err == nil {
		t.Fatal("belay must refuse a workspace that does not exist")
	}
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != graph.ExitFailed {
		t.Fatalf("error = %#v, want an ExitError with code %d", err, graph.ExitFailed)
	}
	if !strings.Contains(exit.Message, missing) {
		t.Errorf("the refusal must name the path; got %q", exit.Message)
	}
	if !strings.Contains(exit.Message, "nothing to work on") {
		t.Errorf("the refusal must be a sentence, not a Go error; got %q", exit.Message)
	}
}

// TestRunRefusesFanoutInPlainWords is acceptance criterion 6, through the
// command a person actually types.
func TestRunRefusesFanoutInPlainWords(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nfanout:\n  enabled: true\n  candidates: 3\n")

	out, _, err := runCLI(t, "run", "--workspace", ws, "add tests")
	if err == nil {
		t.Fatal("belay must refuse a run it cannot finish")
	}
	assertPlainText(t, "fanout refusal", out)

	for _, phrase := range []string{
		"belay will not start this run.",
		"fanout.enabled is true",
		"copy your repository 3 times",
		"to fix it: set fanout.enabled to false",
		"Nothing was created and nothing was spent.",
	} {
		if !strings.Contains(out, phrase) {
			t.Errorf("the refusal never says %q; got:\n%s", phrase, out)
		}
	}
	if strings.Contains(out, "ErrFanoutUnavailable") {
		t.Errorf("the refusal leaks a Go error name:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(ws, ".belay")); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a refused run must not create a run directory")
	}
}

func TestRunArgumentErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no description", args: []string{"run"}, want: `say what you want built, in quotes`},
		{name: "blank description", args: []string{"run", "   "}, want: "that description is empty"},
		{
			name: "forgot the quotes",
			args: []string{"run", "add", "some", "tests"},
			want: `did you forget the quotes around "add some tests"?`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runCLI(t, tc.args...)
			if err == nil {
				t.Fatalf("belay %v should be refused", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to say %q", err, tc.want)
			}
		})
	}
}

func TestReportOutcomeMapsExitCodesAndExplainsItself(t *testing.T) {
	layout, err := state.NewLayout(t.TempDir(), "20260905T120000Z-abcdef123456")
	if err != nil {
		t.Fatalf("state.NewLayout: %v", err)
	}

	tests := []struct {
		name     string
		outcome  graph.Outcome
		runErr   error
		wantCode int
		wantSaid []string
	}{
		{
			name:     "completed",
			outcome:  graph.Outcome{Status: state.RunStatusCompleted, ExitCode: graph.ExitOK, Node: graph.End, Steps: 7},
			wantCode: graph.ExitOK,
			wantSaid: []string{"belay finished after 7 steps.", "nothing left to do", "commit it"},
		},
		{
			name: "paused waits for a person and prints the command",
			outcome: graph.Outcome{
				Status: state.RunStatusPaused, ExitCode: graph.ExitOK,
				Node: graph.NodeApprove, Steps: 2,
			},
			wantCode: graph.ExitOK,
			wantSaid: []string{
				"waiting for you",
				"approve — waiting for you to read the plan and approve it",
				"belay resume 20260905T120000Z-abcdef123456",
				filepath.Join(layout.ArtifactsDir(), "plan.md"),
			},
		},
		{
			name: "run called on a paused run",
			outcome: graph.Outcome{
				Status: state.RunStatusPaused, ExitCode: graph.ExitPaused,
				Node: graph.NodeApprove,
			},
			runErr:   graph.ErrRunPaused,
			wantCode: graph.ExitPaused,
			wantSaid: []string{"This run is waiting for you", "belay resume 20260905T120000Z-abcdef123456"},
		},
		{
			name: "failed",
			outcome: graph.Outcome{
				Status: state.RunStatusFailed, ExitCode: graph.ExitFailed,
				Node: graph.NodeReview, Steps: 5,
			},
			runErr:   graph.ErrNodeFailed,
			wantCode: graph.ExitFailed,
			wantSaid: []string{"could not finish", "review — running the quality gate", "belay timeline"},
		},
		{
			name: "aborted by the step cap",
			outcome: graph.Outcome{
				Status: state.RunStatusAborted, ExitCode: graph.ExitAborted,
				Node: graph.NodeFix, Steps: 60,
			},
			runErr:   graph.ErrMaxSteps,
			wantCode: graph.ExitAborted,
			wantSaid: []string{"stopped itself", "kept going round without finishing", "graph.max_steps"},
		},
		{
			name: "aborted with no resume point",
			outcome: graph.Outcome{
				Status: state.RunStatusFailed, ExitCode: graph.ExitFailed,
			},
			runErr:   graph.ErrNoResumePoint,
			wantCode: graph.ExitFailed,
			wantSaid: []string{"cannot tell where this run left off", layout.JournalPath()},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			u := &ui{out: &out, err: &out}

			err := reportOutcome(u, layout, tc.outcome, tc.runErr)

			switch {
			case tc.wantCode == graph.ExitOK && err != nil:
				t.Fatalf("reportOutcome() = %v, want nil for an ok outcome", err)
			case tc.wantCode != graph.ExitOK:
				var exit *ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("reportOutcome() = %#v, want an ExitError", err)
				}
				if exit.Code != tc.wantCode {
					t.Errorf("exit code = %d, want %d", exit.Code, tc.wantCode)
				}
			}

			got := out.String()
			assertPlainText(t, tc.name, got)
			for _, phrase := range tc.wantSaid {
				if !strings.Contains(got, phrase) {
					t.Errorf("report never says %q; got:\n%s", phrase, got)
				}
			}
			// Nothing a person cannot act on.
			for _, jargon := range []string{"StatusAborted", "ErrNoResumePoint", "graph.Outcome"} {
				if strings.Contains(got, jargon) {
					t.Errorf("report leaks the identifier %q to the reader:\n%s", jargon, got)
				}
			}
		})
	}
}

func TestNarratorTellsTheRouteThroughTheGraph(t *testing.T) {
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	logger := slog.New(&narrator{ui: u})

	// Exactly what internal/graph logs as it walks: attributes attached with
	// With, then one message per transition.
	node := logger.With(slog.String("node", graph.NodeTest), slog.Int("step", 5), slog.Int("attempt", 1))
	node.Info(msgNodeStarted)
	node.Info(msgNodeFinished,
		slog.String("status", "failed"), slog.String("next", ""), slog.Duration("elapsed", 1500*time.Millisecond))

	retry := logger.With(slog.String("node", graph.NodeFix), slog.Int("step", 6), slog.Int("attempt", 2))
	retry.Info(msgNodeStarted)
	retry.Info(msgNodeFinished,
		slog.String("status", "ok"), slog.String("next", graph.NodeTest), slog.Duration("elapsed", 2*time.Second))

	logger.Info("belay/graph: something operational", slog.String("node", "not-a-transition"))

	got := out.String()
	assertPlainText(t, "narration", got)

	want := []string{
		"→ test      step 5",
		"test      took 1.5s → failed, the run stops here",
		"→ fix       step 6, attempt 2",
		"fix       took 2s → test",
	}
	for _, line := range want {
		if !strings.Contains(got, line) {
			t.Errorf("narration never says %q; got:\n%s", line, got)
		}
	}
	if strings.Contains(got, "something operational") {
		t.Errorf("the narration should carry node transitions only; got:\n%s", got)
	}
}

func TestNarratorEmphasisesOnlyTheCurrentStep(t *testing.T) {
	var out bytes.Buffer
	u := &ui{out: &out, err: &out, color: true}
	logger := slog.New(&narrator{ui: u})

	node := logger.With(slog.String("node", graph.NodePlan), slog.Int("step", 1), slog.Int("attempt", 1))
	node.Info(msgNodeStarted)
	node.Info(msgNodeFinished,
		slog.String("status", "ok"), slog.String("next", graph.NodeApprove), slog.Duration("elapsed", time.Second))

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want two lines, got %d:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "\x1b[1m") {
		t.Errorf("the step belay is entering should be emphasised; got %q", lines[0])
	}
	if strings.ContainsRune(lines[1], '\x1b') {
		t.Errorf("the step belay has left should be plain; got %q", lines[1])
	}
}

func TestNarratorPassesEverythingToTheVerboseLog(t *testing.T) {
	var story, log bytes.Buffer
	u := &ui{out: &story, err: &log, verbose: true}
	logger := newLogger(u)

	logger.With(slog.String("node", graph.NodePlan), slog.Int("step", 1)).
		Info(msgNodeStarted)
	logger.Debug("belay/exec: starting a subprocess", slog.String("tool", "go"))

	if !strings.Contains(story.String(), "→ plan") {
		t.Errorf("the story should still narrate; got:\n%s", story.String())
	}
	if !strings.Contains(log.String(), "belay/exec: starting a subprocess") {
		t.Errorf("--verbose should forward everything to stderr; got:\n%s", log.String())
	}
	if strings.Contains(story.String(), "belay/exec") {
		t.Error("belay's own log must not contaminate the piped story on stdout")
	}
}

func TestThousandsAndDollars(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"}, {7, "7"}, {999, "999"}, {1000, "1,000"},
		{2_000_000, "2,000,000"}, {20_000_000, "20,000,000"}, {-1234, "-1,234"},
	}
	for _, tc := range tests {
		if got := thousands(tc.in); got != tc.want {
			t.Errorf("thousands(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := dollars(5); got != "$5.00" {
		t.Errorf("dollars(5) = %q, want $5.00", got)
	}
}

func TestStopOnSignalCleansUpWithoutASignal(t *testing.T) {
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}

	ctx, stop := stopOnSignal(context.Background(), u)
	stop()

	if ctx.Err() == nil {
		t.Error("stop() should cancel the run's context")
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be said when no signal arrived; got %q", out.String())
	}
}

// TestDriveRunNarratesTheWholeStory is the end-to-end shape of a real
// invocation — narration, cost, wayfinding and exit code — with the agent
// replaced by nodes that call nothing. No test in this package ever reaches a
// live backend.
func TestDriveRunNarratesTheWholeStory(t *testing.T) {
	ws := goWorkspace(t)
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}

	plan, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	reached := false
	registry := graph.NewRegistry()
	registry.MustRegister(
		planWriter{body: "# Plan\n\nwrite a test\n"},
		approve.New(),
		endsTheRun{reached: &reached},
	)
	plan.registry = registry

	store := seedRunDir(t, plan, "add tests for the config parser")
	announceStart(u, plan, store.Layout(), "add tests for the config parser")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := driveRun(cmd, u, plan, store, false); err != nil {
		t.Fatalf("driveRun: %v", err)
	}

	got := out.String()
	t.Logf("verbatim output:\n%s", got)
	assertPlainText(t, "run", got)

	for _, phrase := range []string{
		"belay is starting a run in " + ws,
		"what you asked for:  add tests for the config parser",
		"most it can spend:   $5.00 or 2,000,000 tokens, whichever comes first",
		"to stop it:          press Ctrl-C once",
		"route:  plan → approve → code → write → test → fix → review → done",
		"→ plan      step 1",
		"plan      took",
		"→ approve",
		"belay paused after 2 steps, and is waiting for you.",
		"approve — waiting for you to read the plan and approve it",
		"belay resume " + store.Layout().RunID(),
		filepath.Join(store.Layout().ArtifactsDir(), "plan.md"),
	} {
		if !strings.Contains(got, phrase) {
			t.Errorf("the run never says %q; got:\n%s", phrase, got)
		}
	}
}

// TestNoColorLeavesOutputPlain is acceptance criterion 7 at the level a
// person experiences it: the whole command, with NO_COLOR set.
func TestNoColorLeavesOutputPlain(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	out, errOut, err := runCLI(t, "--dry-run", "run", "--workspace", goWorkspace(t), "add tests")
	if err != nil {
		t.Fatalf("belay --dry-run run: %v", err)
	}
	assertPlainText(t, "stdout", out)
	assertPlainText(t, "stderr", errOut)

	// And the explicit flag, on a writer that really is a character device.
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = devNull.Close() })
	if colorEnabled(devNull, false) {
		t.Error("NO_COLOR must switch colour off even on a terminal")
	}
}

// TestDriveRunResumesPastTheApprovalGate covers the other half of the
// command layer: `belay resume` must dispatch through Resume and only
// Resume, since calling Run first would refuse the very gate it is here to
// step past.
func TestDriveRunResumesPastTheApprovalGate(t *testing.T) {
	ws := goWorkspace(t)
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}

	plan, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	reached := false
	registry := graph.NewRegistry()
	registry.MustRegister(
		planWriter{body: "# Plan\n\nwrite a test\n"},
		approve.New(),
		endsTheRun{reached: &reached},
	)
	plan.registry = registry

	store := seedRunDir(t, plan, "add tests")
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	if err := driveRun(cmd, u, plan, store, false); err != nil {
		t.Fatalf("first invocation: %v", err)
	}
	if reached {
		t.Fatal("the run went past the gate without anyone approving")
	}

	if _, err := approvePlan(store); err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	out.Reset()
	if err := driveRun(cmd, u, plan, store, true); err != nil {
		t.Fatalf("resume: %v", err)
	}

	got := out.String()
	t.Logf("verbatim resume output:\n%s", got)
	if !reached {
		t.Error("resuming must let the run continue past the gate")
	}
	for _, phrase := range []string{"→ approve", "→ code", "belay finished", "nothing left to do", "commit it"} {
		if !strings.Contains(got, phrase) {
			t.Errorf("the resume never says %q; got:\n%s", phrase, got)
		}
	}
}
