//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/isolate/dircopy"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// Fanout suite: a run that fans out, walked by belay's real graph from plan
// to winner, with real candidate runs in real directory copies.
//
// Every piece slice 7 built is the real one here: the parent's graph from
// nodes.Default with WithFanout, the candidates' from nodes.Candidate, the
// dispatcher's own candidate runner, the dircopy isolator, and the journals,
// blackboards and manifests each candidate run keeps under
// candidate-runs/<id>. Only the outside world is scripted -- the agent, the
// test command and the linter -- because what is under test is what belay
// does with N candidates, not whether a model can write Go. The scripted
// world is a pure function of the candidate's id, so a crash-free run, a run
// killed half-way and its resume all ask it the same questions and get the
// same answers, which is what lets the crash test compare them at all.
//
// Every package-level identifier here and in fanout_test.go is prefixed
// "fanout", the convention the durability and loop suites keep.

// fanoutRunID is the parent run every scenario in this suite creates.
const fanoutRunID = "20260101T000000Z-fa0700000000"

// fanoutGoal is what every fanout run here is asked to do.
const fanoutGoal = "add a Greeting function"

// fanoutChildEnv names the workspace a child process continues; see
// TestFanoutCrashChildProcess. It is unset in every ordinary run of the
// suite.
const fanoutChildEnv = "BELAY_E2E_FANOUT_CHILD_WORKSPACE"

// fanoutVerdict is what the scripted world says about one candidate's work.
type fanoutVerdict struct {
	// passes is whether its tests pass. A candidate whose tests never pass
	// goes round its fix loop until graph.give_up ends its run, failed.
	passes bool
	// issues is how many minor review findings its change has. Minor is
	// below the default fail_on, so the gate still passes, and join's
	// fewest_issues strategy has something to compare.
	issues int
}

// fanoutVerdicts are the five candidates' results. They make exactly one
// winner under fewest_issues: cand-03, with no issues. cand-05 never passes
// its tests, so it is ineligible and exercises a candidate run that fails
// on its own while its siblings complete.
var fanoutVerdicts = map[string]fanoutVerdict{
	"cand-01": {passes: true, issues: 2},
	"cand-02": {passes: true, issues: 1},
	"cand-03": {passes: true, issues: 0},
	"cand-04": {passes: true, issues: 3},
	"cand-05": {passes: false},
}

// fanoutWinner is the candidate fanoutVerdicts make the winner.
const fanoutWinner = "cand-03"

// fanoutConfig is the configuration of every fanout run here: five
// candidates, all five at once, chosen by fewest issues, under a ceiling the
// scripted world never comes near. Approval is off so the plan goes straight
// to the fanout; the gate's edge into fanout has its own tests.
func fanoutConfig() config.Config {
	cfg := config.Default()
	cfg.Graph.Approval = false
	cfg.Fanout.Enabled = true
	cfg.Fanout.Candidates = len(fanoutVerdicts)
	cfg.Fanout.Concurrency = len(fanoutVerdicts)
	cfg.Fanout.Select = config.SelectFewestIssues
	cfg.Budget.MaxUSD = 5.00
	return cfg
}

// fanoutHarness owns one fanout run's directory across a scenario. Like
// durabilityHarness it holds no dispatcher between calls: those are what a
// crash destroys, so each call to dispatcher builds a new one.
type fanoutHarness struct {
	t         *testing.T
	workspace string
	layout    state.Layout
	cfg       config.Config
}

// fanoutTightEnv, set in a child process's environment, gives the child
// fanoutTightConfig and a $1.00 plan in place of the suite's defaults.
const fanoutTightEnv = fanoutChildEnv + "_TIGHT"

// fanoutTightPlanUSD is what the plan costs in a run under
// fanoutTightConfig.
const fanoutTightPlanUSD = 1.00

// fanoutTightConfig is a fanout whose ceiling leaves little slack: two
// candidates under $3.50, after a plan that costs $1.00, so each candidate's
// envelope is $1.25 and the launch projection ($1.00 a candidate on top of
// $1.00 spent) fits with $0.50 to spare. Any figure a resume wrongly adds to
// the spend before the fanout shows up as a refusal.
func fanoutTightConfig() config.Config {
	cfg := fanoutConfig()
	cfg.Fanout.Candidates = 2
	cfg.Fanout.Concurrency = 2
	cfg.Budget.MaxUSD = 3.50
	return cfg
}

// newFanoutRun creates a workspace holding a small project, and a run in it
// whose manifest pins a fanout, exactly as createRun writes one.
func newFanoutRun(t *testing.T) *fanoutHarness {
	t.Helper()
	return newFanoutRunWith(t, fanoutConfig())
}

// newFanoutRunWith is newFanoutRun under cfg.
func newFanoutRunWith(t *testing.T, cfg config.Config) *fanoutHarness {
	t.Helper()
	ws := t.TempDir()
	fanoutSeed(t, ws, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.22\n",
		"app.go": "package app\n\n// Name is the app's name.\nfunc Name() string { return \"app\" }\n",
	})
	h := openFanoutRun(t, ws)
	h.cfg = cfg
	if err := os.MkdirAll(h.layout.RunDir(), 0o750); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", h.layout.RunDir(), err)
	}
	man := state.NewManifest(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), fanoutRunID, ws, fanoutGoal, h.cfg)
	// Through SetFanout, as createRun does, so the capability is recorded
	// with the pin.
	man.SetFanout(state.FanoutPin{Candidates: h.cfg.Fanout.Candidates, Select: string(h.cfg.Fanout.Select)})
	if err := state.SaveManifest(h.layout.ManifestPath(), man); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}
	if err := state.SaveState(h.layout.StatePath(), state.NewState(fanoutGoal)); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
	return h
}

// openFanoutRun attaches to the fanout run in ws without writing anything,
// which is what a child process and a resume both do.
func openFanoutRun(t *testing.T, ws string) *fanoutHarness {
	t.Helper()
	layout, err := state.NewLayout(ws, fanoutRunID)
	if err != nil {
		t.Fatalf("state.NewLayout() error = %v", err)
	}
	return &fanoutHarness{t: t, workspace: ws, layout: layout, cfg: fanoutConfig()}
}

// fanoutSeed writes files under dir.
func fanoutSeed(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
}

// fanoutTools are the test command and the linter one dispatcher is given.
type fanoutTools struct {
	runner belay.TestRunner
	linter belay.Linter
}

// fanoutScriptedTools answers for each candidate from fanoutVerdicts, keyed on
// the workspace the dircopy isolator made, which is candidates/<id>.
func fanoutScriptedTools() fanoutTools {
	return fanoutTools{
		runner: &belaytest.FakeRunner{
			NameValue: "fake-runner",
			Func: func(_ context.Context, dir string) (belay.TestReport, error) {
				if fanoutVerdicts[filepath.Base(dir)].passes {
					return loopPassingTestReport(4), nil
				}
				return loopFailingTestReport(4, 1, "TestGreeting", "Greeting() = \"\", want \"hello\""), nil
			},
		},
		linter: &belaytest.FakeLinter{
			NameValue: "fake-linter",
			Func: func(_ context.Context, dir string) (belay.QualityReport, error) {
				n := fanoutVerdicts[filepath.Base(dir)].issues
				issues := make([]belay.Issue, 0, n)
				for i := range n {
					issues = append(issues, belay.Issue{
						RuleID: "style:naming", Severity: belay.SeverityMinor,
						File: "greeting.go", Line: i + 1, Message: "a name could be clearer",
					})
				}
				return belay.QualityReport{
					Source: "fake-linter", Gate: belay.GatePass, Counts: belay.Counts{Minor: n},
					Issues: issues, Summary: fmt.Sprintf("%d minor issues", n),
				}, nil
			},
		},
	}
}

// dispatcher builds a brand-new dispatcher over the run, with the real
// parent and candidate graphs, the real candidate runner and a real
// directory-copy isolator, and the outside world scripted by world and tools.
// Never reuse what it returns across a simulated crash.
func (h *fanoutHarness) dispatcher(world *fanoutWorld, tools fanoutTools, logger *slog.Logger) *graph.Dispatcher {
	h.t.Helper()
	parent, err := nodes.Default(h.cfg, nodes.WithFanout())
	if err != nil {
		h.t.Fatalf("nodes.Default() error = %v", err)
	}
	candidates, err := nodes.Candidate()
	if err != nil {
		h.t.Fatalf("nodes.Candidate() error = %v", err)
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	iso, err := dircopy.New(h.layout.CandidatesDir(), dircopy.WithLogger(logger))
	if err != nil {
		h.t.Fatalf("dircopy.New() error = %v", err)
	}
	d, err := graph.NewDispatcher(graph.Options{
		Store: state.NewStore(h.layout), Registry: parent, Config: h.cfg, Logger: logger,
		Agent:             world.planner(),
		CandidateRegistry: candidates,
		CandidateAgent:    world.candidate,
		Concurrency:       h.cfg.Fanout.Concurrency,
		Runner:            tools.runner,
		Linter:            tools.linter,
		Isolator:          iso,
		// How `belay run` and `belay resume` build theirs.
		PauseOnCancel: true,
	})
	if err != nil {
		h.t.Fatalf("graph.NewDispatcher() error = %v", err)
	}
	return d
}

// candidateLayout is one candidate run's Layout under this run.
func (h *fanoutHarness) candidateLayout(id string) state.Layout {
	h.t.Helper()
	l, err := state.NewCandidateRunLayout(h.layout, id)
	if err != nil {
		h.t.Fatalf("NewCandidateRunLayout(%q) error = %v", id, err)
	}
	return l
}

// records reads a journal without healing it: journal.Open rewrites a torn
// tail, and a test reading what a kill left behind must not be what
// repairs it.
func fanoutRecords(t *testing.T, path string) []journal.Record {
	t.Helper()
	res, err := journal.ReadFile(path)
	if err != nil {
		t.Fatalf("journal.ReadFile(%s) error = %v", path, err)
	}
	return res.Records
}

// fanoutState reads the parent's blackboard.
func (h *fanoutHarness) state() state.State {
	h.t.Helper()
	st, err := state.LoadState(h.layout.StatePath())
	if err != nil {
		h.t.Fatalf("LoadState() error = %v", err)
	}
	return st
}

// fanoutWorld is the scripted outside world one process sees: the agent that
// writes the plan, and one agent per candidate, each answering as
// fanoutVerdicts says and counting what it was asked.
type fanoutWorld struct {
	// park names the candidates whose first call never returns. Only the
	// child process of the crash test parks anyone: it is how that process
	// is caught with candidates in flight.
	park map[string]bool
	// planUSD, when above zero, is what the plan costs instead of the
	// scripted plan's own figure.
	planUSD float64

	mu    sync.Mutex
	calls map[string]int
}

// newFanoutWorld returns a world in which every call answers.
func newFanoutWorld() *fanoutWorld { return &fanoutWorld{calls: map[string]int{}} }

// planner is the agent the run's own plan node calls. It reports a small,
// real-looking usage: the crash test depends on the parent having paid for
// something before its fanout, which is what an interrupted attempt is
// charged against.
func (w *fanoutWorld) planner() belay.AgentBackend {
	plan := loopPlanResponse()
	if w.planUSD > 0 {
		plan.Usage.USD = w.planUSD
	}
	return &belaytest.FakeAgent{NameValue: "claude-code", Responses: []belay.AgentResponse{plan}}
}

// candidate is graph.Options.CandidateAgent: the backend candidate id calls.
func (w *fanoutWorld) candidate(id string) belay.AgentBackend {
	return &fanoutCandidateAgent{id: id, world: w}
}

// called reports how many times candidate id's agent was asked anything.
func (w *fanoutWorld) called(id string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[id]
}

func (w *fanoutWorld) record(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls[id]++
}

// fanoutCandidateAgent is one candidate's agent. Every call -- the code
// node's, and every fix node's -- does what a real agent does: it edits the
// candidate's own workspace and reports what it changed, in the
// changed-files block the write node verifies against the disk.
type fanoutCandidateAgent struct {
	id    string
	world *fanoutWorld
}

// Name implements belay.AgentBackend.
func (a *fanoutCandidateAgent) Name() string { return "claude-code" }

// Invoke implements belay.AgentBackend.
func (a *fanoutCandidateAgent) Invoke(ctx context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
	a.world.record(a.id)
	if a.world.park[a.id] {
		return belay.AgentResponse{}, fanoutParkUntilKilled(ctx)
	}
	body := fmt.Sprintf("package app\n\n// Greeting is %s's answer to the goal.\nfunc Greeting() string { return %q }\n",
		a.id, "hello from "+a.id)
	if err := os.WriteFile(filepath.Join(req.WorkDir, "greeting.go"), []byte(body), 0o600); err != nil {
		return belay.AgentResponse{}, fmt.Errorf("%s: write greeting.go: %w", a.id, err)
	}
	return belay.AgentResponse{
		Text:      "Added the Greeting function.\n\n" + loopChangedFilesBlock("greeting.go"),
		SessionID: "session-" + a.id,
		Turns:     3,
		Usage:     belay.Usage{InputTokens: 1000, OutputTokens: 200, USD: 0.05},
	}, nil
}

// fanoutParkUntilKilled never returns in the child process, whose context is
// never cancelled: the process is killed with the candidate parked here,
// mid-node, its node_started journalled and its node_finished never written.
//
// It sleeps rather than waiting on a channel alone. With every goroutine in
// the process parked on channels, the runtime would declare a deadlock and
// exit the child by itself -- a crash, but not the one this suite means to
// cause, and not at the moment it chose.
func fanoutParkUntilKilled(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
