package cli

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/state"
)

// changesHostileFile stands in for the code node: it reports changing
// hostileFile, a name an agent chose, and goes on to the test node.
type changesHostileFile struct{}

func (changesHostileFile) Name() string { return graph.NodeCode }

func (changesHostileFile) Run(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
	code := rc.State.Code
	code.ChangedFiles = []string{hostileFile}
	return graph.Result{Status: journal.StatusOK, Next: graph.NodeTest, Patch: state.Patch{Code: &code},
		Note: "changed " + hostileFile}, nil
}

// failsQuoting stands in for a node whose error quotes what it was about --
// the review's policy scan error naming an agent-chosen file -- so the
// dispatcher composes the run's note, and the run's error, from hostile.
type failsQuoting struct{ name string }

func (n failsQuoting) Name() string { return n.name }

func (failsQuoting) Run(context.Context, *graph.RunContext) (graph.Result, error) {
	return graph.Result{Status: journal.StatusFailed}, errors.New(hostileNote)
}

// hostileRegistry is a run's graph with the agent replaced: the plan node
// writes hostilePlan, the real approve gate stops for it, the code node
// reports hostileFile, and the test node fails quoting hostileNote.
func hostileRegistry() *graph.Registry {
	r := graph.NewRegistry()
	r.MustRegister(planWriter{body: hostilePlan}, approve.New(), changesHostileFile{},
		failsQuoting{name: graph.NodeTest})
	return r
}

// sweepExempt are the commands that print nothing belay did not write, so
// there is no text from outside to sweep them with: help and completion
// print belay's own command tree, and version its build stamp.
var sweepExempt = map[string]string{
	"help":       "prints belay's own help",
	"completion": "prints a shell completion script generated from belay's own command tree",
	"version":    "prints belay's build stamp",
}

// TestEveryCommandShowsHostileTextAsStandIns sweeps every command belay has
// with a goal, a plan, a note and a file name full of escape sequences, C0
// and C1 controls and a DEL, and requires that nothing any of them prints --
// on stdout, on stderr, or as the last word Execute prints -- could drive a
// terminal. It drives each command through the CLI's own code, with only
// the agent replaced.
//
// It fails, too, for a command it does not sweep, so a command added later
// is swept or says here why it need not be.
func TestEveryCommandShowsHostileTextAsStandIns(t *testing.T) {
	swept := map[string]bool{}
	check := func(command, label, stdout, stderr string, err error) {
		t.Helper()
		swept[command] = true
		assertTerminalSafe(t, label+" (stdout)", stdout)
		assertTerminalSafe(t, label+" (stderr)", stderr)
		if err != nil {
			var last bytes.Buffer
			exitReport(&last, err)
			assertTerminalSafe(t, label+" (its last word)", last.String())
		}
	}
	cli := func(command string, args ...string) {
		t.Helper()
		out, errOut, err := runCLI(t, args...)
		check(command, "belay "+strings.Join(args, " "), out, errOut, err)
	}

	// belay run: the announcement, the narration and the report of a run
	// that stops at its plan. Then belay resume: one that goes on, records
	// the agent's file and fails with an error quoting it.
	ws := goWorkspace(t)
	var out, errOut bytes.Buffer
	u := &ui{out: &out, err: &errOut, verbose: true}
	p, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	p.registry = hostileRegistry()
	store := seedRunDir(t, p, hostileGoal)
	id := store.Layout().RunID()
	announceStart(u, p, store.Layout(), hostileGoal)
	err = driveRun(cmdWithContext(), u, p, store, false)
	check("run", "belay run", out.String(), errOut.String(), err)
	cli("run", "run", "--dry-run", "--workspace", ws, hostileGoal)

	// belay plan, in every mode that works without a terminal.
	cli("plan", "plan", "--no-input", "--workspace", ws, id)
	cli("plan", "plan", "--dry-run", "--regenerate", "fewer steps "+hostile, "--workspace", ws, id)
	cli("plan", "plan", "--archive", "--workspace", ws, id)

	cli("resume", "resume", "--dry-run", "--workspace", ws, id)
	if _, err := approvePlan(store); err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	out.Reset()
	errOut.Reset()
	announceContinue(u, p, resumableRun{id: id, goal: hostileGoal}, store.Layout(), false)
	err = driveRun(cmdWithContext(), u, p, store, true)
	check("resume", "belay resume", out.String(), errOut.String(), err)
	if !strings.Contains(out.String()+errOut.String(), "x"+hostileShown+"y.txt") {
		t.Errorf("the run's report never shows the note it failed with:\n%s\n%s", out.String(), errOut.String())
	}

	// What reads a run back: its timeline, its plan once it has stopped,
	// and the listing.
	cli("timeline", "timeline", "--workspace", ws, id)
	cli("timeline", "timeline", "--json", "--workspace", ws, id)
	cli("timeline", "timeline", "--candidate", "cand-01", "--workspace", ws, id)
	cli("plan", "plan", "--no-input", "--workspace", ws, id)
	cli("runs", "runs", "--workspace", ws)
	cli("runs", "runs", "--json", "--workspace", ws)

	// belay fork: described, then made, re-running the test node that
	// quotes the agent's file, in a copy.
	cli("fork", "fork", "--dry-run", "--workspace", ws, id, graph.NodeTest)
	out.Reset()
	errOut.Reset()
	req := forkRequest{runID: id, node: graph.NodeTest}
	fp, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u), override: req.configure})
	if err != nil {
		t.Fatalf("resolve the fork: %v", err)
	}
	fp.registry = hostileRegistry()
	err = makeFork(cmdWithContext(), u, fp, req)
	check("fork", "belay fork", out.String(), errOut.String(), err)
	if len(forksIn(t, ws)) != 1 {
		t.Errorf("the fork was not made, so its report was not swept:\n%s\n%s", out.String(), errOut.String())
	}

	// belay queue: a goal that stops at its plan, one added with
	// --no-approval that fails quoting the agent's file, and one taken
	// back out.
	qws := goWorkspace(t)
	cli("queue add", "queue", "add", "--workspace", qws, hostileGoal)
	cli("queue add", "queue", "add", "--no-approval", "--workspace", qws, "second "+hostileGoal)
	cli("queue add", "queue", "add", "--dry-run", "--workspace", qws, "third "+hostileGoal)
	cli("queue start", "queue", "start", "--dry-run", "--workspace", qws)
	started, err := startQueue(t.Context(), t, qws, hostileRegistry)
	check("queue start", "belay queue start", started, "", err)
	if !strings.Contains(started, "x"+hostileShown+"y.txt") {
		t.Errorf("the queue's report never shows the note goal 2 failed with:\n%s", started)
	}
	cli("queue list", "queue", "list", "--workspace", qws)
	cli("queue list", "queue", "list", "--json", "--workspace", qws)
	cli("queue status", "queue", "status", "--workspace", qws)
	cli("queue", "queue", "--workspace", qws)
	cli("queue approve", "queue", "approve", "--workspace", qws, "1")
	cli("queue add", "queue", "add", "--workspace", qws, "fourth "+hostileGoal)
	cli("queue rm", "queue", "rm", "--workspace", qws, "3")

	for _, command := range leafCommands(rootCmd, "") {
		if _, ok := sweepExempt[strings.Fields(command)[0]]; ok {
			continue
		}
		if !swept[command] {
			t.Errorf("belay %s is not swept: drive it with hostile text here, or say in sweepExempt why it prints none", command)
		}
	}
	if t.Failed() {
		t.Logf("swept: %v", slices.Sorted(func(yield func(string) bool) {
			for c := range swept {
				if !yield(c) {
					return
				}
			}
		}))
	}
}

// leafCommands names every command under cmd that runs something, the way a
// person types it after "belay": "run", "queue add", and "queue", which runs
// the status report on its own.
func leafCommands(cmd *cobra.Command, prefix string) []string {
	var names []string
	for _, sub := range cmd.Commands() {
		name := strings.TrimSpace(prefix + " " + sub.Name())
		if sub.Runnable() {
			names = append(names, name)
		}
		names = append(names, leafCommands(sub, name)...)
	}
	return names
}
