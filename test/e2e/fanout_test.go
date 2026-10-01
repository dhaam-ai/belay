//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/linter"
	"github.com/dhaam-ai/belay/internal/runner"
	"github.com/dhaam-ai/belay/internal/state"
)

// fanoutIDs are the candidates fanoutVerdicts describes, in index order.
var fanoutIDs = []string{"cand-01", "cand-02", "cand-03", "cand-04", "cand-05"}

// TestFanoutRunEndToEnd is a whole run that fans out: plan, fanout, join,
// with five real candidate runs in five real directory copies, from the
// first node to the winner.
//
// It is the run belay's README used to describe and nothing in the graph
// could reach. What it pins is everything a person relies on when they turn
// fanout on: each candidate is a run with its own valid journal; the work
// happens in the candidates' workspaces and never in the person's checkout;
// the blackboard records every candidate in order with what it did; the
// winner is the one the strategy says; and the bill is the sum of what was
// spent, with nothing invented.
func TestFanoutRunEndToEnd(t *testing.T) {
	h := newFanoutRun(t)
	world := newFanoutWorld()

	out, err := h.dispatcher(world, fanoutScriptedTools(), nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (note %q)", err, out.Note)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed (note %q)", out.Status, out.Note)
	}

	// The parent's own walk: plan, then the fanout, then join -- and never
	// a node of the candidates' work.
	parent := fanoutRecords(t, h.layout.JournalPath())
	if err := journal.Validate(parent); err != nil {
		t.Fatalf("the parent journal does not validate: %v", err)
	}
	if got, want := fanoutWalk(parent), []string{"plan", "fanout", "join"}; !slices.Equal(got, want) {
		t.Errorf("the parent walked %v, want %v", got, want)
	}

	st := h.state()
	if st.Winner != fanoutWinner {
		t.Errorf("Winner = %q, want %q", st.Winner, fanoutWinner)
	}
	if len(st.Candidates) != len(fanoutIDs) {
		t.Fatalf("Candidates = %d, want %d: %+v", len(st.Candidates), len(fanoutIDs), st.Candidates)
	}

	var spent float64
	for i, c := range st.Candidates {
		id := fanoutIDs[i]
		v := fanoutVerdicts[id]
		if c.ID != id {
			t.Errorf("Candidates[%d].ID = %q, want %q: candidates must be recorded in index order", i, c.ID, id)
		}
		wantOutcome, wantMarker := "completed", journal.EventRunCompleted
		if !v.passes {
			wantOutcome, wantMarker = "failed", journal.EventRunFailed
		}
		if c.TestPassed != v.passes || c.Outcome != wantOutcome {
			t.Errorf("%s: TestPassed = %v, Outcome = %q; want %v and %q", id, c.TestPassed, c.Outcome, v.passes, wantOutcome)
		}
		if v.passes && c.IssueCount != v.issues {
			t.Errorf("%s: IssueCount = %d, want %d", id, c.IssueCount, v.issues)
		}
		if want := "candidate-runs/" + id; c.RunDir != want {
			t.Errorf("%s: RunDir = %q, want %q", id, c.RunDir, want)
		}
		spent += c.SpentUSD

		// Its own run, with its own valid journal, ended by its own marker.
		cl := h.candidateLayout(id)
		recs := fanoutRecords(t, cl.JournalPath())
		if err := journal.Validate(recs); err != nil {
			t.Errorf("%s: its journal does not validate: %v", id, err)
		}
		if last := recs[len(recs)-1]; last.Event != wantMarker {
			t.Errorf("%s: its journal ends with %v, want %v", id, last.Event, wantMarker)
		}
		if walked := fanoutWalk(recs); walked[0] != "code" || slices.Contains(walked, "plan") {
			t.Errorf("%s walked %v, want a run that starts at code and plans nothing", id, walked)
		}

		// Its work is in its own workspace, under its own name.
		body, err := os.ReadFile(filepath.Join(c.Dir, "greeting.go")) //nolint:gosec // a path this test's run made
		if err != nil {
			t.Errorf("%s: its workspace has no greeting.go: %v", id, err)
		} else if !strings.Contains(string(body), "hello from "+id) {
			t.Errorf("%s: its workspace holds another candidate's work:\n%s", id, body)
		}
	}

	// An in-place fanout edits nothing of the person's: the winner's change
	// is copied back by hand, as join's note and the CLI both say.
	if _, err := os.Stat(filepath.Join(h.workspace, "greeting.go")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the person's checkout gained greeting.go (stat error %v); a fanout must work only in its candidates", err)
	}

	// The bill is what was spent: the plan, and every candidate's own,
	// reported rather than estimated.
	if want := loopPlanResponse().Usage.USD + spent; !durabilityCloseTo(out.Usage.USD, want) {
		t.Errorf("Outcome.Usage.USD = %v, want the plan plus every candidate's spend, %v", out.Usage.USD, want)
	}
	if out.Usage.Estimated {
		t.Error("Outcome.Usage.Estimated = true for a run nothing interrupted, so nothing in it was estimated")
	}
}

// TestFanoutCrashWithThreeOfFiveInFlightConverges is the crash test the
// candidate-run design calls T25, with a real kill.
//
// A child process runs the fanout until two candidates have completed and
// the other three are part-way through their code node, and is then sent
// SIGKILL: no deferred function runs, no journal is closed, nothing is
// flushed that was not already durable. The parent's fanout node is left
// open, and so is each of the three candidates' code node. A fresh
// dispatcher then resumes the run in this process, exactly as `belay
// resume` would.
//
// What must hold, against a crash-free control run of the same scripted
// world:
//
//   - The two finished candidates execute nothing more: their agents are
//     never called, and their journals, blackboards, manifests and
//     workspaces are byte-identical before and after the resume.
//   - The three interrupted ones are healed exactly once each, and re-run
//     their code node as its second attempt.
//   - Every journal validates; the parent's shows one interrupted fanout
//     attempt and one that settled.
//   - The candidates, their order, what each did and the winner equal the
//     control's.
//   - The resumed run's bill is at least the control's and is marked
//     estimated: the interrupted fanout attempt is charged a cost nobody
//     reported, because the process that could have was killed (ADR-0007).
//
// What is deliberately NOT asserted is byte-equal spend or durations. A
// killed attempt is charged an estimate, and a duration measured across a
// kill is not the control's duration.
func TestFanoutCrashWithThreeOfFiveInFlightConverges(t *testing.T) {
	control := newFanoutRun(t)
	if out, err := control.dispatcher(newFanoutWorld(), fanoutScriptedTools(), nil).Run(context.Background()); err != nil ||
		out.Status != state.RunStatusCompleted {
		t.Fatalf("the control run: status %v, error %v (note %q)", out.Status, err, out.Note)
	}
	controlOut := fanoutOutcomeOf(t, control)

	h := newFanoutRun(t)
	finished, parked := fanoutIDs[:2], fanoutIDs[2:]
	fanoutKillWithCandidatesInFlight(t, h, finished, parked)

	// What the kill left: the parent's fanout open, and the three parked
	// candidates each open in its code node.
	if last := fanoutLast(t, h.layout.JournalPath()); last.Event != journal.EventNodeStarted || last.Node != graph.NodeFanout {
		t.Fatalf("the parent journal ends with %v %q, want an open fanout node", last.Event, last.Node)
	}
	before := map[string]string{}
	for _, id := range finished {
		before[id] = fanoutDigest(t, h, id)
	}

	world := newFanoutWorld()
	out, err := h.dispatcher(world, fanoutScriptedTools(), nil).Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume() error = %v (note %q)", err, out.Note)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed (note %q)", out.Status, out.Note)
	}

	// The finished candidates were adopted, not run again.
	for _, id := range finished {
		if n := world.called(id); n != 0 {
			t.Errorf("%s finished before the kill, but its agent was called %d time(s) after the resume", id, n)
		}
		if after := fanoutDigest(t, h, id); after != before[id] {
			t.Errorf("%s finished before the kill, but its run directory or workspace changed after the resume", id)
		}
	}

	// The interrupted ones were healed once each and picked up at code.
	for _, id := range parked {
		recs := fanoutRecords(t, h.candidateLayout(id).JournalPath())
		if err := journal.Validate(recs); err != nil {
			t.Errorf("%s: its journal does not validate after the resume: %v", id, err)
		}
		heals := 0
		secondCode := false
		for _, r := range recs {
			if r.Interrupted() {
				heals++
				if r.Node != graph.NodeCode {
					t.Errorf("%s: healed node %q, want code, the node it was killed in", id, r.Node)
				}
			}
			if r.Event == journal.EventNodeStarted && r.Node == graph.NodeCode && r.Attempt == 2 {
				secondCode = true
			}
		}
		if heals != 1 {
			t.Errorf("%s: %d heal record(s), want exactly 1", id, heals)
		}
		if !secondCode {
			t.Errorf("%s: no second attempt at code; the interrupted node was not re-run", id)
		}
		if world.called(id) == 0 {
			t.Errorf("%s was interrupted, but its agent was never called again", id)
		}
	}

	// The parent: one interrupted fanout attempt, one that settled.
	parent := fanoutRecords(t, h.layout.JournalPath())
	if err := journal.Validate(parent); err != nil {
		t.Fatalf("the parent journal does not validate after the resume: %v", err)
	}
	var interrupted, settled int
	for _, r := range parent {
		if r.Event != journal.EventNodeFinished || r.Node != graph.NodeFanout {
			continue
		}
		switch {
		case r.Interrupted():
			interrupted++
		case r.Status == journal.StatusOK:
			settled++
		}
	}
	if interrupted != 1 || settled != 1 {
		t.Errorf("the parent's fanout attempts: %d interrupted and %d settled, want 1 and 1", interrupted, settled)
	}

	// The same candidates, in the same order, doing the same things, and the
	// same winner, as a run nothing interrupted.
	got := fanoutOutcomeOf(t, h)
	if got.winner != controlOut.winner {
		t.Errorf("Winner = %q, want the control's %q", got.winner, controlOut.winner)
	}
	if !slices.Equal(got.candidates, controlOut.candidates) {
		t.Errorf("the candidates after the crash differ from the control's:\n got: %v\nwant: %v", got.candidates, controlOut.candidates)
	}

	controlTotal := control.manifest(t).Budget
	if !out.Usage.Estimated {
		t.Error("Outcome.Usage.Estimated = false, want true: the interrupted fanout attempt was charged an estimate")
	}
	if out.Usage.USD < controlTotal.SpentUSD-1e-9 {
		t.Errorf("Outcome.Usage.USD = %v, less than the control's %v: a crash must never make a run look cheaper",
			out.Usage.USD, controlTotal.SpentUSD)
	}
}

// TestFanoutCrashChildProcess is not a test on its own. It is the process
// TestFanoutCrashWithThreeOfFiveInFlightConverges starts and then kills:
// with fanoutChildEnv set, it runs that test's fanout with three candidates
// parked mid-node, and never returns. Run any other way, it skips.
func TestFanoutCrashChildProcess(t *testing.T) {
	ws := os.Getenv(fanoutChildEnv)
	if ws == "" {
		t.Skip("not a test on its own: TestFanoutCrashWithThreeOfFiveInFlightConverges runs it as a child process, and kills it")
	}
	h := openFanoutRun(t, ws)
	world := newFanoutWorld()
	world.park = map[string]bool{}
	for _, id := range strings.Split(os.Getenv(fanoutChildEnv+"_PARK"), ",") {
		world.park[id] = true
	}
	// Logged to stderr, which the killing test prints if the child never
	// reaches the state it is waiting for.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	out, err := h.dispatcher(world, fanoutScriptedTools(), logger).Run(context.Background())
	t.Fatalf("the child's run returned (status %v, error %v, note %q); it was meant to be killed with candidates in flight",
		out.Status, err, out.Note)
}

// fanoutKillWithCandidatesInFlight runs h's fanout in a child process, waits
// until every candidate in finished has completed its run and every one in
// parked is open in its code node, and then kills the child with SIGKILL.
func fanoutKillWithCandidatesInFlight(t *testing.T, h *fanoutHarness, finished, parked []string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	var stderr bytes.Buffer
	//nolint:gosec // the test binary itself, re-run with a fixed -test.run.
	child := exec.Command(exe, "-test.run=^TestFanoutCrashChildProcess$", "-test.count=1")
	child.Env = append(os.Environ(),
		fanoutChildEnv+"="+h.workspace,
		fanoutChildEnv+"_PARK="+strings.Join(parked, ","))
	child.Stdout, child.Stderr = &stderr, &stderr
	if err := child.Start(); err != nil {
		t.Fatalf("start the child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	deadline := time.Now().Add(2 * time.Minute)
	for !fanoutInFlight(t, h, finished, parked) {
		select {
		case err := <-exited:
			t.Fatalf("the child exited before it was killed (%v):\n%s", err, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			_ = child.Process.Kill() // #nosec G104 -- best effort; the test is failing anyway.
			t.Fatalf("the child never reached %v finished and %v in flight:\n%s", finished, parked, stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// SIGKILL on unix: the process gets no say in what is left on disk.
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill the child: %v", err)
	}
	if err := <-exited; err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("the child exited with %v, want it killed:\n%s", err, stderr.String())
	}
}

// fanoutInFlight reports whether every candidate in finished has a journal
// ending in run_completed and every one in parked has a journal ending in an
// open node_started for code. It reads without healing, as everything that
// looks at a run another process is writing must.
func fanoutInFlight(t *testing.T, h *fanoutHarness, finished, parked []string) bool {
	t.Helper()
	ended := func(id string, want func(journal.Record) bool) bool {
		res, err := journal.ReadFile(h.candidateLayout(id).JournalPath())
		if err != nil || len(res.Records) == 0 {
			return false
		}
		return want(res.Records[len(res.Records)-1])
	}
	for _, id := range finished {
		if !ended(id, func(r journal.Record) bool { return r.Event == journal.EventRunCompleted }) {
			return false
		}
	}
	for _, id := range parked {
		if !ended(id, func(r journal.Record) bool {
			return r.Event == journal.EventNodeStarted && r.Node == graph.NodeCode
		}) {
			return false
		}
	}
	return true
}

// fanoutLast is the last record of the journal at path.
func fanoutLast(t *testing.T, path string) journal.Record {
	t.Helper()
	recs := fanoutRecords(t, path)
	if len(recs) == 0 {
		t.Fatalf("%s holds no records", path)
	}
	return recs[len(recs)-1]
}

// fanoutDigest fingerprints everything a finished candidate owns: its run's
// journal, blackboard and manifest, and the change in its workspace. A
// candidate that was run again would change at least one of them.
func fanoutDigest(t *testing.T, h *fanoutHarness, id string) string {
	t.Helper()
	cl := h.candidateLayout(id)
	paths := []string{
		cl.JournalPath(), cl.StatePath(), cl.ManifestPath(),
		filepath.Join(h.layout.CandidatesDir(), id, "greeting.go"),
	}
	parts := make([]string, 0, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path) //nolint:gosec // paths this test's run made
		if err != nil {
			t.Fatalf("%s: read %s: %v", id, path, err)
		}
		sum := sha256.Sum256(body)
		parts = append(parts, filepath.Base(path)+"="+hex.EncodeToString(sum[:]))
	}
	return strings.Join(parts, " ")
}

// fanoutWalk is the nodes a journal started, in order.
func fanoutWalk(recs []journal.Record) []string {
	var walked []string
	for _, r := range recs {
		if r.Event == journal.EventNodeStarted {
			walked = append(walked, r.Node)
		}
	}
	return walked
}

// fanoutOutcome is what a fanout settled, in the terms two runs can be
// compared by: no spend and no duration, which a crash legitimately changes.
type fanoutOutcome struct {
	winner     string
	candidates []string
}

// fanoutOutcomeOf reads what h's run settled.
func fanoutOutcomeOf(t *testing.T, h *fanoutHarness) fanoutOutcome {
	t.Helper()
	st := h.state()
	out := fanoutOutcome{winner: st.Winner}
	for _, c := range st.Candidates {
		out.candidates = append(out.candidates, fmt.Sprintf("%s outcome=%s passed=%v issues=%d policy=%d",
			c.ID, c.Outcome, c.TestPassed, c.IssueCount, c.PolicyViolations))
	}
	return out
}

// manifest reads h's run manifest.
func (h *fanoutHarness) manifest(t *testing.T) state.Manifest {
	t.Helper()
	m, err := state.LoadManifest(h.layout.ManifestPath())
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	return m
}

// fanoutToolsPrompt is what a skipped real-tools test tells its reader to ask
// a coding agent, so it runs next time instead of skipping again.
const fanoutToolsPrompt = "Install Go and golangci-lint v2 so both are on PATH, " +
	"then run `go test ./test/e2e/... -tags=e2e -run TestFanoutRunWithRealToolsEndToEnd -v` " +
	"and confirm it passes instead of skipping."

// TestFanoutRunWithRealToolsEndToEnd is the same run with the scripted test
// command and linter swapped for the real ones: `go test` and golangci-lint,
// in five candidate workspaces, five candidates at a time.
//
// It is here for one finding. With fanout.concurrency above 1, several
// candidates reach their review node together, and golangci-lint takes one
// lock per machine; by default a process that waits for it more than about
// five seconds gives up with exit 3, which belay reports as a review that
// could not run, failing the candidate for nothing to do with its code
// (see test/lintgate, which pins that behaviour deterministically, and
// internal/linter's golangciSerial). Inside a real fanout every gate must
// produce a report: every candidate here writes clean code, so every one
// must complete with no issues, and the tie goes to the lowest id.
func TestFanoutRunWithRealToolsEndToEnd(t *testing.T) {
	fanoutRequireTools(t)
	t.Setenv("GOLANGCI_LINT_CACHE", t.TempDir())
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "off")

	h := newFanoutRun(t)
	fanoutSeed(t, h.workspace, map[string]string{
		"app_test.go": "package app\n\nimport \"testing\"\n\nfunc TestName(t *testing.T) {\n" +
			"\tif got := Name(); got != \"app\" {\n\t\tt.Fatalf(\"Name() = %q, want app\", got)\n\t}\n}\n",
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tools := fanoutTools{
		runner: runner.NewGo(bexec.New(logger), logger),
		linter: linter.NewGolangCI(linter.WithLogger(logger)),
	}

	out, err := h.dispatcher(newFanoutWorld(), tools, logger).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v (note %q)", err, out.Note)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed (note %q)", out.Status, out.Note)
	}
	st := h.state()
	for _, c := range st.Candidates {
		if c.Outcome != "completed" || !c.TestPassed || c.IssueCount != 0 {
			t.Errorf("%s: outcome %q, passed %v, %d issues; want a completed candidate with passing tests and no issues (note %q)",
				c.ID, c.Outcome, c.TestPassed, c.IssueCount, c.Note)
		}
		for _, r := range fanoutRecords(t, h.candidateLayout(c.ID).JournalPath()) {
			if r.Event == journal.EventNodeFinished && r.Node == graph.NodeReview && r.Status != journal.StatusOK {
				t.Errorf("%s: its review node ended %v (%q); a gate that could not run fails a candidate for nothing it did",
					c.ID, r.Status, r.Note)
			}
		}
	}
	if st.Winner != "cand-01" {
		t.Errorf("Winner = %q, want cand-01: every candidate tied on no issues, and a tie goes to the lowest id", st.Winner)
	}
}

// fanoutRequireTools skips t unless go and golangci-lint v2 both run. The
// skip message says how to install what is missing, for whoever reads it, a
// person or a coding agent.
func fanoutRequireTools(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, "go", "version").Run(); err != nil {
		t.Skipf("skipping: needs go on PATH (%v). Install it from https://go.dev/dl/, "+
			"or ask your coding agent: %q", err, fanoutToolsPrompt)
	}
	out, err := exec.CommandContext(ctx, "golangci-lint", "version", "--short").Output()
	if version := strings.TrimSpace(string(out)); err != nil || !strings.HasPrefix(version, "2.") {
		t.Skipf("skipping: needs golangci-lint v2 on PATH (version %q, error %v). "+
			"Install it with `brew install golangci-lint` or "+
			"`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`, "+
			"or ask your coding agent: %q", version, err, fanoutToolsPrompt)
	}
}
