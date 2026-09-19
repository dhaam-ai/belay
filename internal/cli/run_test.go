package cli

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// runCLI executes the command tree the way a shell would, with both streams
// captured.
//
// The command tree is a package-level singleton, because subcommands register
// themselves through init(); flag values therefore outlive one invocation.
// runCLI puts every flag in the tree back to its default first, so a command
// or flag added later is covered without editing this helper. Tests using it
// must not run in parallel.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	resetFlags(t, rootCmd)

	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})

	err = rootCmd.Execute()
	return out.String(), errOut.String(), err
}

// resetFlags sets every flag of cmd and of every command under it back to its
// default, and forgets that it was set. That includes the --help flag cobra
// adds to whichever command last printed help, which would otherwise turn
// every later invocation into another help screen.
func resetFlags(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	reset := func(f *pflag.Flag) {
		if err := setToDefault(f); err != nil {
			t.Fatalf("reset --%s on %q to %q: %v", f.Name, cmd.Name(), f.DefValue, err)
		}
		f.Changed = false
	}
	cmd.PersistentFlags().VisitAll(reset)
	cmd.Flags().VisitAll(reset)
	for _, sub := range cmd.Commands() {
		resetFlags(t, sub)
	}
}

// setToDefault sets f to its DefValue. A list flag needs Replace: its Set
// appends once the flag has been set, so setting the default would add the
// defaults to what the last invocation left.
func setToDefault(f *pflag.Flag) error {
	list, ok := f.Value.(pflag.SliceValue)
	if !ok {
		return f.Value.Set(f.DefValue)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(f.DefValue, "["), "]")
	if inner == "" {
		return list.Replace(nil)
	}
	values, err := csv.NewReader(strings.NewReader(inner)).Read()
	if err != nil {
		return err
	}
	return list.Replace(values)
}

// assertPlainText is the non-TTY contract: piping belay anywhere must produce
// text a person can read, with no escape sequences in it.
func assertPlainText(t *testing.T, label, s string) {
	t.Helper()
	if strings.ContainsRune(s, '\x1b') {
		t.Errorf("%s contains an ANSI escape sequence; output to a non-terminal must be plain:\n%q", label, s)
	}
}

// TestRunCLIStartsEachInvocationFromTheDefaults pins runCLI's promise to
// every test that uses it: a flag one invocation sets does not carry into
// the next, whichever command owns the flag. The command tree is one
// package-level value, so without the reset a --json from one test would
// silently change the output of the next.
func TestRunCLIStartsEachInvocationFromTheDefaults(t *testing.T) {
	ws := t.TempDir()

	asJSON, _, err := runCLI(t, "runs", "--workspace", ws, "--json")
	if err != nil {
		t.Fatalf("belay runs --json: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(asJSON), "{") {
		t.Fatalf("belay runs --json did not print JSON, so this test proves nothing:\n%s", asJSON)
	}

	plain, _, err := runCLI(t, "runs", "--workspace", ws)
	if err != nil {
		t.Fatalf("belay runs: %v", err)
	}
	if strings.HasPrefix(strings.TrimSpace(plain), "{") {
		t.Errorf("--json from the previous invocation carried into this one:\n%s", plain)
	}
}

// TestSetToDefaultRestoresAListFlag covers the flag kind whose Set appends
// once the flag has been set, so that setting its default would keep what
// the last invocation left. No command has a list flag yet; runCLI must not
// start leaking one when a command does.
func TestSetToDefaultRestoresAListFlag(t *testing.T) {
	tests := []struct {
		name     string
		defaults []string
	}{
		{name: "empty default", defaults: []string{}},
		{name: "default with values", defaults: []string{"a", "b,c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
			got := flags.StringSlice("names", tt.defaults, "")
			if err := flags.Parse([]string{"--names", "x", "--names", "y"}); err != nil {
				t.Fatalf("parse: %v", err)
			}

			if err := setToDefault(flags.Lookup("names")); err != nil {
				t.Fatalf("setToDefault: %v", err)
			}
			if !slices.Equal(*got, tt.defaults) {
				t.Errorf("--names = %q after reset, want its default %q", *got, tt.defaults)
			}
		})
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
		// belay run --help promises that a run stopped with Ctrl-C is
		// picked up by resume; resume's own help must agree.
		"Ctrl-C",
		"runs that step again",
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

// TestDryRunWithApprovalOffShowsTheRouteWithoutTheGate: a dry run prints the
// walk the run really takes, and with graph.approval false the plan node
// routes straight to code, so the gate is not on it. Printing it there
// contradicted the line underneath, which says belay will not stop.
func TestDryRunWithApprovalOffShowsTheRouteWithoutTheGate(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\ngraph:\n  approval: false\n")

	out, _, err := runCLI(t, "--dry-run", "run", "--workspace", ws, "add tests")
	if err != nil {
		t.Fatalf("belay --dry-run run: %v\n%s", err, out)
	}
	const want = "plan → code → write → test → fix → review → done"
	if !strings.Contains(out, want) {
		t.Errorf("the dry run never says %q; got:\n%s", want, out)
	}
	if !strings.Contains(out, "graph.approval is false") {
		t.Errorf("the dry run should still explain why there is no gate; got:\n%s", out)
	}
}

// TestDryRunSaysFanoutDoesNothingYet: fanout.enabled is a setting belay
// accepts and acts on in no way at all — no step of the graph leads to the
// fanout node, so a run never reaches it. Someone who turned it on is
// expecting N attempts and paying attention to the cost; a dry run that says
// nothing lets them believe they are getting them.
func TestDryRunSaysFanoutDoesNothingYet(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantSay bool
	}{
		{
			name:    "fanout enabled",
			config:  "version: 1\nfanout:\n  enabled: true\n  candidates: 3\n",
			wantSay: true,
		},
		{name: "fanout left alone", config: "version: 1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := goWorkspace(t)
			writeConfig(t, ws, tt.config)

			out, _, err := runCLI(t, "--dry-run", "run", "--workspace", ws, "add tests")
			if err != nil {
				t.Fatalf("belay --dry-run run: %v\n%s", err, out)
			}
			const phrase = "fanout.enabled is true"
			if said := strings.Contains(out, phrase); said != tt.wantSay {
				t.Errorf("the dry run says %q: %v, want %v; got:\n%s", phrase, said, tt.wantSay, out)
			}
			if !tt.wantSay {
				return
			}
			for _, want := range []string{"worth knowing", "nothing runs the candidates", "ADR-0019"} {
				if !strings.Contains(out, want) {
					t.Errorf("the warning never says %q; got:\n%s", want, out)
				}
			}
		})
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

// TestDryRunReportsOAuthToken shows a subscription user that belay saw their
// token, without printing it.
func TestDryRunReportsOAuthToken(t *testing.T) {
	secret := notARealKey("belay-test-", "oauth-credential-longenoughtomatter")
	t.Setenv(anthropicKeyEnv, "")
	t.Setenv(claudeOAuthTokenEnv, secret)

	out, _, err := runCLI(t, "--dry-run", "run", "--workspace", goWorkspace(t), "add tests")
	if err != nil {
		t.Fatalf("belay --dry-run run: %v", err)
	}
	if strings.Contains(out, secret) {
		t.Fatal("the dry run printed the OAuth token")
	}
	if !strings.Contains(out, claudeOAuthTokenEnv+" is set") {
		t.Errorf("the dry run should say the token is set; got:\n%s", out)
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

// Fanout is runnable now that the join node exists, so a dry run must plan it
// rather than refuse it -- and must still create nothing.
func TestDryRunPlansAFanoutRun(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nfanout:\n  enabled: true\n  candidates: 3\n")

	out, _, err := runCLI(t, "--dry-run", "run", "--workspace", ws, "add tests")
	if err != nil {
		t.Fatalf("a dry run with fanout enabled must succeed, got: %v\n%s", err, out)
	}
	assertPlainText(t, "fanout dry run", out)

	if strings.Contains(out, "belay will not start this run.") {
		t.Errorf("fanout is supported now; belay must not refuse it:\n%s", out)
	}
	if strings.Contains(out, "ErrFanoutUnavailable") || strings.Contains(out, "nodes:") {
		t.Errorf("output leaks a Go error name:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(ws, ".belay")); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a dry run must not create a run directory")
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

// TestCreateRunLaysDownTheRunItWasAskedFor pins createRun's promise to its
// caller: the run it makes is under the workspace and id the caller chose,
// the dispatcher can load both of its files, and belay resume can find it.
func TestCreateRunLaysDownTheRunItWasAskedFor(t *testing.T) {
	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const runID = "20260905T120000Z-abcdef123456"

	store, err := createRun(plan, runSpec{Workspace: ws, RunID: runID, Goal: "add tests"})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}
	if store == nil {
		t.Fatal("createRun returned no store and no error")
	}
	if got := store.Layout().RunID(); got != runID {
		t.Errorf("run id = %q, want the one the caller chose, %q", got, runID)
	}

	manifest, err := store.LoadManifest()
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.RunID != runID || manifest.Workspace != ws || manifest.Goal != "add tests" {
		t.Errorf("manifest names run %q in %q for %q, want run %q in %q for %q",
			manifest.RunID, manifest.Workspace, manifest.Goal, runID, ws, "add tests")
	}
	blackboard, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if blackboard.Goal != "add tests" {
		t.Errorf("state goal = %q, want %q", blackboard.Goal, "add tests")
	}

	chosen, err := chooseRun(ws, "")
	if err != nil {
		t.Fatalf("belay resume cannot find the run createRun made: %v", err)
	}
	if chosen.id != runID {
		t.Errorf("belay resume chose %q, want %q", chosen.id, runID)
	}
}

// TestCreateRunWritesTheManifestLast closes the window between run
// creation's two writes. A manifest is what makes a directory a run: belay
// resume and belay runs read manifest.json and nothing else. Each row puts a
// directory where one of the two files must go, so writing it fails, and
// checks that nothing which looks for runs takes what is left for one. The
// idea is slice 3's (design:replay-queue), which found the bug.
//
// Each row then takes the obstacle away and creates the same run again, which
// must succeed: a caller that retries after a failure converges on one whole
// run rather than being stuck with half of one.
func TestCreateRunWritesTheManifestLast(t *testing.T) {
	tests := []struct {
		name    string
		blocked func(state.Layout) string
	}{
		{name: "state.json cannot be written", blocked: state.Layout.StatePath},
		{name: "manifest.json cannot be written", blocked: state.Layout.ManifestPath},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := goWorkspace(t)
			plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			const runID = "20260905T120000Z-abcdef123456"
			spec := runSpec{Workspace: ws, RunID: runID, Goal: "add tests"}
			layout, err := state.NewLayout(ws, runID)
			if err != nil {
				t.Fatalf("state.NewLayout: %v", err)
			}
			obstacle := tc.blocked(layout)
			if err := os.MkdirAll(obstacle, 0o750); err != nil {
				t.Fatalf("put a directory at %s: %v", obstacle, err)
			}

			if _, err := createRun(plan, spec); err == nil {
				t.Fatalf("createRun succeeded with a directory at %s", obstacle)
			}

			if _, err := state.LoadManifest(layout.ManifestPath()); err == nil {
				t.Error("createRun failed but left a readable manifest.json, which makes the half-made directory look like a run")
			}
			if chosen, err := chooseRun(ws, ""); err == nil {
				t.Errorf("belay resume would continue %s, a run that was never finished being made", chosen.id)
			}
			listed, err := ScanRuns(ws)
			if err != nil {
				t.Fatalf("ScanRuns: %v", err)
			}
			for _, s := range listed {
				if s.RunID == runID && (!s.Unreadable || s.Resumable) {
					t.Errorf("belay runs lists %s as a %s run (resumable %v); it may only report a damaged directory",
						runID, s.Status, s.Resumable)
				}
			}

			if err := os.Remove(obstacle); err != nil {
				t.Fatalf("remove %s: %v", obstacle, err)
			}
			store, err := createRun(plan, spec)
			if err != nil {
				t.Fatalf("createRun again, with nothing in the way: %v", err)
			}
			if _, err := store.LoadState(); err != nil {
				t.Errorf("the retried run has no state: %v", err)
			}
			if chosen, err := chooseRun(ws, ""); err != nil || chosen.id != runID {
				t.Errorf("belay resume after the retry chose (%q, %v), want %q", chosen.id, err, runID)
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
		name        string
		outcome     graph.Outcome
		runErr      error
		wantCode    int
		wantSaid    []string
		wantNotSaid []string
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
			// Ctrl-C with belay run and belay resume: the run is paused at
			// the step it was in, which is not a request to read the plan.
			name: "interrupted by Ctrl-C says how to pick it up again",
			outcome: graph.Outcome{
				Status: state.RunStatusPaused, ExitCode: graph.ExitAborted,
				Node: graph.NodeCode, Steps: 3,
			},
			runErr:   fmt.Errorf("%w: at %q: %w", graph.ErrInterrupted, graph.NodeCode, context.Canceled),
			wantCode: graph.ExitAborted,
			wantSaid: []string{
				"belay stopped because you asked it to",
				"code — asking the agent to write the change",
				"pick up where it stopped with:",
				"belay resume 20260905T120000Z-abcdef123456",
			},
			wantNotSaid: []string{"read the plan", "plan.md"},
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
			// A run that ended as aborted cannot be resumed: belay resume
			// refuses it as "already stopped for good". So the report must
			// not offer the command, but say what to do instead.
			name: "aborted by the step cap",
			outcome: graph.Outcome{
				Status: state.RunStatusAborted, ExitCode: graph.ExitAborted,
				Node: graph.NodeFix, Steps: 60,
			},
			runErr:   graph.ErrMaxSteps,
			wantCode: graph.ExitAborted,
			wantSaid: []string{
				"stopped itself", "kept going round without finishing", "graph.max_steps",
				"this run has ended", `belay run "what you want built"`,
			},
			wantNotSaid: []string{"belay resume"},
		},
		{
			name: "aborted by the node timeout",
			outcome: graph.Outcome{
				Status: state.RunStatusAborted, ExitCode: graph.ExitAborted,
				Node: graph.NodeTest, Steps: 5,
			},
			runErr:   graph.ErrNodeTimeout,
			wantCode: graph.ExitAborted,
			wantSaid: []string{
				"graph.node_timeout", "this run has ended", `belay run "what you want built"`,
			},
			wantNotSaid: []string{"belay resume"},
		},
		{
			name: "aborted by the budget ceiling",
			outcome: graph.Outcome{
				Status: state.RunStatusAborted, ExitCode: graph.ExitAborted,
				Node: graph.NodeFix, Steps: 9,
			},
			runErr:   fmt.Errorf("graph: budget ceiling reached before %q: %w", graph.NodeFix, belay.ErrBudgetExceeded),
			wantCode: graph.ExitAborted,
			wantSaid: []string{
				"stopped itself", "this run has ended",
				"belay timeline 20260905T120000Z-abcdef123456",
			},
			wantNotSaid: []string{"belay resume"},
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

			err := reportOutcome(u, outcomeView{layout: layout, outcome: tc.outcome, runErr: tc.runErr})

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
			for _, phrase := range tc.wantNotSaid {
				if strings.Contains(got, phrase) {
					t.Errorf("report says %q, which is not what happened; got:\n%s", phrase, got)
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

// routesTo is a node that succeeds and hands the run to next.
type routesTo struct{ name, next string }

func (n routesTo) Name() string { return n.name }

func (n routesTo) Run(context.Context, *graph.RunContext) (graph.Result, error) {
	return graph.Result{Status: journal.StatusOK, Next: n.next}, nil
}

// TestCtrlCLeavesARunBelayResumeContinues is the promise belay run --help
// makes: "Press Ctrl-C whenever you like: the run stops, its state stays, and
// belay resume picks it up from the last completed step."
//
// The run is interrupted while code runs, after plan has finished. belay
// exits as a Ctrl-C always has, and says how to pick the run up. belay resume
// then chooses the run, by id and by default, and continues it: code runs
// again, and plan does not.
func TestCtrlCLeavesARunBelayResumeContinues(t *testing.T) {
	ws := goWorkspace(t)
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}

	plan, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupted := graph.NewRegistry()
	interrupted.MustRegister(
		routesTo{name: graph.NodePlan, next: graph.NodeCode},
		pressesCtrlC{name: graph.NodeCode, cancel: cancel},
	)
	plan.registry = interrupted
	store := seedRunDir(t, plan, "add tests")
	runID := store.Layout().RunID()

	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	err = driveRun(cmd, u, plan, store, false)
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != graph.ExitAborted {
		t.Fatalf("driveRun = %v, want an ExitError with the exit code a Ctrl-C has always had, %d", err, graph.ExitAborted)
	}
	got := out.String()
	t.Logf("verbatim output after Ctrl-C:\n%s", got)
	for _, phrase := range []string{
		"belay stopped because you asked it to",
		"pick up where it stopped with:",
		"belay resume " + runID,
	} {
		if !strings.Contains(got, phrase) {
			t.Errorf("after Ctrl-C belay never says %q; got:\n%s", phrase, got)
		}
	}

	for _, id := range []string{runID, ""} {
		chosen, err := chooseRun(ws, id)
		if err != nil {
			t.Fatalf("chooseRun(%q) = %v, want the interrupted run: its next step is belay resume", id, err)
		}
		if chosen.id != runID {
			t.Errorf("chooseRun(%q) chose %s, want %s", id, chosen.id, runID)
		}
	}

	// belay resume, with a context nobody cancels.
	reached := false
	finishing := graph.NewRegistry()
	finishing.MustRegister(
		routesTo{name: graph.NodePlan, next: graph.NodeCode},
		endsTheRun{reached: &reached},
	)
	plan.registry = finishing
	out.Reset()
	cmd.SetContext(context.Background())
	if err := driveRun(cmd, u, plan, store, true); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got = out.String()
	t.Logf("verbatim resume output:\n%s", got)
	if !reached {
		t.Error("resuming must run the step Ctrl-C interrupted")
	}
	for _, phrase := range []string{"→ code", "attempt 2", "belay finished"} {
		if !strings.Contains(got, phrase) {
			t.Errorf("the resume never says %q; got:\n%s", phrase, got)
		}
	}
	if strings.Contains(got, "→ plan") {
		t.Errorf("the resume ran plan again, which finished before Ctrl-C; got:\n%s", got)
	}
}

// finishes is a node that ends the run the moment it runs.
type finishes struct{ name string }

func (n finishes) Name() string { return n.name }

func (finishes) Run(context.Context, *graph.RunContext) (graph.Result, error) {
	return graph.Result{Status: journal.StatusOK, Next: graph.End, Note: "done"}, nil
}

// TestDispatchOnceAppliesEachOptionsChange pins the seam a caller uses to
// dispatch a run differently from belay run and belay resume: every change
// it passes reaches the dispatcher, in the order it was passed, on top of the
// Options every run shares.
func TestDispatchOnceAppliesEachOptionsChange(t *testing.T) {
	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	reached := false
	shared := graph.NewRegistry()
	shared.MustRegister(planWriter{body: "# Plan\n\nwrite a test\n"}, approve.New(), endsTheRun{reached: &reached})
	plan.registry = shared
	store := seedRunDir(t, plan, "add tests")

	// The first change swaps in a graph that ends at its first node; the
	// second sees what the first did, which is only so if they run in order.
	short := graph.NewRegistry()
	short.MustRegister(finishes{name: graph.NodePlan})
	var secondSaw *graph.Registry
	view, err := dispatchOnce(context.Background(), plan, store, false,
		func(o *graph.Options) { o.Registry = short },
		func(o *graph.Options) { secondSaw = o.Registry },
	)
	if err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}

	if view.outcome.Status != state.RunStatusCompleted || view.outcome.Steps != 1 {
		t.Errorf("outcome = %+v (error %v), want the changed graph to complete in one step",
			view.outcome, view.runErr)
	}
	if secondSaw != short {
		t.Error("the second change did not see the first change's registry; changes must apply in order")
	}
	if view.layout.RunID() != store.Layout().RunID() {
		t.Errorf("view names run %q, want %q", view.layout.RunID(), store.Layout().RunID())
	}
	if plan.registry != shared {
		t.Error("a change for one dispatch leaked into the plan every later dispatch is built from")
	}
}

// TestAfterCompletionCannotChangeTheExitStatus pins afterCompletion's
// contract with a step that fails: the recording cannot be saved, because
// its directory would have to go where a file already is. belay says so on
// stderr, still reports the run, and exits as the run ended, which here is a
// pause and therefore success.
func TestAfterCompletionCannotChangeTheExitStatus(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nagent:\n  mode: record\n")
	var out, errOut bytes.Buffer
	u := &ui{out: &out, err: &errOut}
	unwritable := filepath.Join(ws, "go.mod", "cassette.json")

	plan, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u), cassette: unwritable})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.recorder == nil {
		t.Fatal("agent.mode record did not arm the recorder, so this test would save nothing")
	}
	reached := false
	registry := graph.NewRegistry()
	registry.MustRegister(planWriter{body: "# Plan\n\nwrite a test\n"}, approve.New(), endsTheRun{reached: &reached})
	plan.registry = registry
	store := seedRunDir(t, plan, "add tests")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := driveRun(cmd, u, plan, store, false); err != nil {
		t.Errorf("driveRun = %v, want nil: the run paused, and a failed save must not change that", err)
	}
	if !strings.Contains(errOut.String(), "belay cannot create the directory for "+unwritable) {
		t.Errorf("stderr = %q, want it to say the recording was not saved", errOut.String())
	}
	if !strings.Contains(out.String(), "belay paused after 2 steps, and is waiting for you.") {
		t.Errorf("the run was not reported after the failed save; stdout:\n%s", out.String())
	}
}
