//go:build fixrate

package fixrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/runner"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// widgetRepo is the repository every synthetic corpus task starts from: a Go
// module whose Resize rounds down where its issue asks it to round half up.
// It stands in for an instance image's /testbed. Its own test passes before
// any fix, as a SWE-bench repository's do: the test that proves the fix is
// the dataset's, and no arm is shown it.
var widgetRepo = map[string]string{
	"go.mod":    "module example.com/widget\n\ngo 1.22\n",
	"widget.go": "package widget\n\n// Resize scales n by f.\nfunc Resize(n int, f float64) int { return int(float64(n) * f) }\n",
	"widget_test.go": "package widget\n\nimport \"testing\"\n\nfunc TestResizeIdentity(t *testing.T) {\n" +
		"\tif got := Resize(3, 1); got != 3 {\n\t\tt.Fatalf(\"Resize(3, 1) = %d, want 3\", got)\n\t}\n}\n",
}

// widgetFix is the repair a scripted agent makes: the fixed source, which
// carries fakePython's FIXED marker in a comment, and a regression test.
var widgetFix = map[string]string{
	"widget.go": "package widget\n\nimport \"math\"\n\n// Resize scales n by f, rounding half up. FIXED\n" +
		"func Resize(n int, f float64) int { return int(math.Floor(float64(n)*f + 0.5)) }\n",
	"widget_regression_test.go": "package widget\n\nimport \"testing\"\n\nfunc TestResizeRoundsHalfUp(t *testing.T) {\n" +
		"\tif got := Resize(3, 0.5); got != 2 {\n\t\tt.Fatalf(\"Resize(3, 0.5) = %d, want 2\", got)\n\t}\n}\n",
}

// widgetInstance is the synthetic SWE-bench record for widget task n.
func widgetInstance(n int) Instance {
	return Instance{
		InstanceID:             fmt.Sprintf("example__widget-%d", n),
		Repo:                   "example/widget",
		BaseCommit:             strings.Repeat("ab", 20),
		EnvironmentSetupCommit: strings.Repeat("cd", 20),
		ProblemStatement:       fmt.Sprintf("Resize rounds down (report %d); it should round half up.", n),
		Patch:                  "diff --git a/widget.go b/widget.go\n",
		TestPatch:              "diff --git a/widget_regression_test.go b/widget_regression_test.go\n",
		Version:                "1.0",
		Difficulty:             "synthetic",
		EvalScript:             "#!/bin/bash\ngo test ./...\n",
		EvalType:               "pass_and_fail",
		Image:                  "example.invalid/synthetic:widget",
		LogParser:              "parse_log_example",
		FailToPass:             []string{"TestResizeRoundsHalfUp"},
		PassToPass:             []string{"TestResizeIdentity"},
	}
}

// widgetTasks returns n synthetic tasks, widget-1 to widget-n.
func widgetTasks(t *testing.T, n int) []Task {
	t.Helper()
	tasks := make([]Task, 0, n)
	for i := 1; i <= n; i++ {
		task, err := widgetInstance(i).Task(PinnedSource)
		if err != nil {
			t.Fatalf("Task(): %v", err)
		}
		tasks = append(tasks, task)
	}
	return tasks
}

// templateProvisioner gives each cell a workspace copied from widgetRepo,
// committed, as an instance image gives one at its HEAD. It counts what it
// was asked to do, so a test can tell an adopted workspace from a fresh one.
type templateProvisioner struct {
	mu       sync.Mutex
	fresh    map[string]int
	released map[string]int
	linter   belay.Linter
}

func newTemplateProvisioner() *templateProvisioner {
	return &templateProvisioner{
		fresh:    map[string]int{},
		released: map[string]int{},
		linter: &belaytest.FakeLinter{Responses: []belay.QualityReport{{
			Source: "fake-linter", Gate: belay.GatePass, Issues: []belay.Issue{}, Summary: "fake-linter: no issues; gate pass",
		}}},
	}
}

func (p *templateProvisioner) Provision(ctx context.Context, cell Cell, dir string, fresh bool) (Workspace, error) {
	ws := filepath.Join(dir, "workspace")
	if fresh {
		if err := os.RemoveAll(ws); err != nil {
			return Workspace{}, err
		}
	}
	if _, err := os.Stat(ws); errors.Is(err, os.ErrNotExist) {
		p.mu.Lock()
		p.fresh[cell.Key()]++
		p.mu.Unlock()
		for name, content := range widgetRepo {
			if err := os.MkdirAll(ws, 0o750); err != nil {
				return Workspace{}, err
			}
			if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o600); err != nil {
				return Workspace{}, err
			}
		}
		for _, args := range [][]string{{"init", "--quiet"}, {"add", "--all"}, {"commit", "--quiet", "--message", "base"}} {
			if err := runGit(ctx, ws, args...); err != nil {
				return Workspace{}, err
			}
		}
	}
	out, err := exec.CommandContext(ctx, "git", "-C", ws, "rev-parse", "HEAD").Output() //nolint:gosec // this test's own temp directory
	if err != nil {
		return Workspace{}, fmt.Errorf("rev-parse: %w", err)
	}
	return Workspace{
		Dir:      ws,
		Base:     strings.TrimSpace(string(out)),
		Runner:   runner.NewGo(nil, nil),
		Linter:   p.linter,
		Adapters: "test: go test; quality gate: a fake linter that always passes",
	}, nil
}

func (p *templateProvisioner) Release(_ context.Context, cell Cell, dir string) error {
	p.mu.Lock()
	p.released[cell.Key()]++
	p.mu.Unlock()
	return os.RemoveAll(filepath.Join(dir, "workspace"))
}

func (p *templateProvisioner) freshCount(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fresh[key]
}

// runGit runs git in dir with a fixed identity and none of the caller's
// configuration.
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{ //nolint:gosec // fixed binary; arguments are the caller's own constants
		"-c", "user.name=fixrate test", "-c", "user.email=fixrate@example.invalid", "-c", "init.defaultBranch=main",
	}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}

// cellAgent is a scripted backend for one cell. It repairs the widget from
// its repairFrom'th editing call on (zero: never), reports usd per call, and
// runs onCall, if set, before answering call n -- which is how a test
// interrupts a corpus in the middle of a cell.
type cellAgent struct {
	repairFrom int
	usd        float64
	onCall     func(ctx context.Context, n int, req belay.AgentRequest) error

	calls atomic.Int32
	edits atomic.Int32
	plans atomic.Int32
}

func (a *cellAgent) backend() *belaytest.FakeAgent {
	return &belaytest.FakeAgent{NameValue: "fixrate-corpus-scripted", Func: a.invoke}
}

func (a *cellAgent) invoke(ctx context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
	n := int(a.calls.Add(1))
	usage := belay.Usage{USD: a.usd, InputTokens: int64(a.usd * 1000)}
	if a.onCall != nil {
		if err := a.onCall(ctx, n, req); err != nil {
			return belay.AgentResponse{}, err
		}
	}
	if isReadOnlyRequest(req) {
		a.plans.Add(1)
		return belay.AgentResponse{Text: planPromptText, Usage: usage}, nil
	}
	edit := int(a.edits.Add(1))
	if a.repairFrom == 0 || edit < a.repairFrom {
		return belay.AgentResponse{Text: changedFilesReply(nil), Turns: 1, Usage: usage}, nil
	}
	var touched []string
	for name, content := range widgetFix {
		if err := os.WriteFile(filepath.Join(req.WorkDir, name), []byte(content), 0o600); err != nil {
			return belay.AgentResponse{}, err
		}
		touched = append(touched, name)
	}
	slices.Sort(touched)
	return belay.AgentResponse{Text: changedFilesReply(touched), Turns: 1, Usage: usage}, nil
}

// fakeEvaluator grades by fakePython's markers without starting a process,
// and counts what it was asked, so a test can tell a cell graded once from
// one graded twice.
type fakeEvaluator struct {
	mu    sync.Mutex
	calls map[string]int
	fail  error
}

func (f *fakeEvaluator) Evaluate(_ context.Context, task Task, system, patch string) (Verdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[task.ID+"/"+system]++
	if f.fail != nil {
		return Verdict{}, f.fail
	}
	if strings.TrimSpace(patch) == "" {
		return Verdict{EmptyPatch: true}, nil
	}
	resolved := strings.Contains(patch, markFixed)
	return Verdict{Evaluated: true, Resolved: resolved, PatchApplied: true,
		Port: PortCheck{Checked: true, Resolved: resolved, Reason: "fake"}}, nil
}

func (f *fakeEvaluator) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

// corpusConfig is the belay configuration the corpus tests run both arms
// under.
func corpusConfig() config.Config {
	cfg := config.Default()
	cfg.Graph.Approval = false
	cfg.Budget.MaxUSD = 2
	return cfg
}

// corpusOptions returns options for a scripted corpus over tasks in dir,
// with agents looked up per cell in agents (a cell with none gets a noop).
func corpusOptions(t *testing.T, dir string, tasks []Task, agents map[string]*cellAgent, eval Evaluator, prov Provisioner) CorpusOptions {
	t.Helper()
	return CorpusOptions{
		Mode:          ModeReplay,
		Dir:           dir,
		RunID:         "corpus-test",
		Corpus:        PinnedSource.String(),
		RecordsSHA256: strings.Repeat("0", 64),
		Selection:     "test: synthetic widget tasks",
		Tasks:         tasks,
		Replicates:    1,
		Config:        corpusConfig(),
		AgentFor: func(c Cell) belay.AgentBackend {
			if a, ok := agents[c.Key()]; ok {
				return a.backend()
			}
			return (&cellAgent{}).backend()
		},
		Provisioner: prov,
		Evaluator:   eval,
		Now:         fixedClock(t),
	}
}

func TestRunCorpus_Refusals(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 1)
	tests := []struct {
		name    string
		mutate  func(*CorpusOptions)
		wantErr error
	}{
		{"an unknown mode", func(o *CorpusOptions) { o.Mode = "bogus" }, ErrUnknownMode},
		{"live without the confirm token", func(o *CorpusOptions) { o.Mode = ModeLive; o.MaxUSD = 10 }, ErrLiveConfirmationRequired},
		{"live with the wrong confirm token", func(o *CorpusOptions) { o.Mode = ModeLive; o.MaxUSD = 10; o.Confirm = "yes" }, ErrLiveConfirmationRequired},
		{"live without a spend cap", func(o *CorpusOptions) { o.Mode = ModeLive; o.Confirm = ConfirmLiveSpend }, ErrNoSpendCap},
		{"no per-task ceiling to reserve", func(o *CorpusOptions) { o.Config.Budget.MaxUSD = 0 }, errCorpusConfig},
		{"no tasks", func(o *CorpusOptions) { o.Tasks = nil }, errCorpusConfig},
		{"a task twice", func(o *CorpusOptions) { o.Tasks = append(o.Tasks, o.Tasks[0]) }, errCorpusConfig},
		{"a task id that is a path", func(o *CorpusOptions) { o.Tasks = []Task{{ID: "../x", FailToPass: []string{"f"}}} }, errCorpusConfig},
		{"no replicate", func(o *CorpusOptions) { o.Replicates = 0 }, errCorpusConfig},
		{"three replicates", func(o *CorpusOptions) { o.Replicates = 3 }, errCorpusConfig},
		{"an unknown arm", func(o *CorpusOptions) { o.Arms = []Arm{"bare"} }, errCorpusConfig},
		{"no agent", func(o *CorpusOptions) { o.AgentFor = nil }, errCorpusConfig},
		{"no provisioner", func(o *CorpusOptions) { o.Provisioner = nil }, errCorpusConfig},
		{"no evaluator", func(o *CorpusOptions) { o.Evaluator = nil }, errCorpusConfig},
		{"no run id", func(o *CorpusOptions) { o.RunID = "" }, errCorpusConfig},
		{"a relative directory", func(o *CorpusOptions) { o.Dir = "corpus" }, errCorpusConfig},
		{"fanout, which the corpus does not measure", func(o *CorpusOptions) { o.Config.Fanout.Enabled = true }, errCorpusConfig},
		{"a review mode the corpus does not supply", func(o *CorpusOptions) { o.Config.Review.Mode = config.ReviewModeSonar }, errCorpusConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := corpusOptions(t, t.TempDir(), tasks, nil, &fakeEvaluator{}, newTemplateProvisioner())
			tt.mutate(&opts)
			if _, err := RunCorpus(t.Context(), opts); !errors.Is(err, tt.wantErr) {
				t.Errorf("RunCorpus() error = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

// TestRunCorpus_ScriptedEndToEnd drives two synthetic tasks through both
// arms -- belay's real graph and the one raw call -- and scores every cell
// with the official harness's real exec path (fakePython standing in for
// swebench), in scripted mode, at zero cost. The scripts make the arms
// disagree on one task, so the report has a delta to carry.
func TestRunCorpus_ScriptedEndToEnd(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 2)
	h, _ := fakeHarness(t)
	dataset := filepath.Join(t.TempDir(), "records.jsonl")
	writeRecords(t, dataset, widgetInstance(1), widgetInstance(2))
	h.Dataset = dataset

	agents := map[string]*cellAgent{
		"example__widget-1/belay-r1":   {repairFrom: 1},
		"example__widget-2/belay-r1":   {repairFrom: 1},
		"example__widget-1/control-r1": {repairFrom: 1},
		// widget-2's control call changes nothing.
	}
	dir := t.TempDir()
	report, err := RunCorpus(t.Context(), corpusOptions(t, dir, tasks, agents, h, newTemplateProvisioner()))
	if err != nil {
		t.Fatalf("RunCorpus() error = %v", err)
	}

	if report.Status != CorpusComplete {
		t.Errorf("Status = %q, want %q", report.Status, CorpusComplete)
	}
	if report.Mode != ModeReplay || !strings.Contains(report.Provenance, "NO real coding agent") {
		t.Errorf("Mode = %q, Provenance = %q; a scripted corpus must say it is one", report.Mode, report.Provenance)
	}
	if report.Belay.Resolved != 2 || report.Control.Resolved != 1 {
		t.Errorf("resolved: belay %d, control %d; want 2 and 1", report.Belay.Resolved, report.Control.Resolved)
	}
	if c := report.Comparison; c.BothResolved != 1 || c.BelayOnly != 1 || c.ControlOnly != 0 || c.NeitherResolved != 0 {
		t.Errorf("Comparison = %+v, want both=1 belay_only=1", c)
	}
	if report.Spend.TotalUSD != 0 {
		t.Errorf("Spend = %+v, want nothing spent in scripted mode", report.Spend)
	}
	for _, task := range tasks {
		for _, system := range []string{"belay-r1", "control-r1"} {
			cellDir := filepath.Join(dir, "cells", task.ID, system)
			for _, name := range []string{"result.json", "verdict.json"} {
				if _, err := os.Stat(filepath.Join(cellDir, name)); err != nil {
					t.Errorf("%s/%s/%s: %v", task.ID, system, name, err)
				}
			}
		}
	}
	// belay's own journal for the belay arm lives beside the cell, outside
	// the workspace the patch is taken from.
	if _, err := os.Stat(filepath.Join(dir, "cells", "example__widget-1", "belay-r1", ".belay", "runs", "belay", "journal.ndjson")); err != nil {
		t.Errorf("the belay cell's journal is not where the corpus keeps it: %v", err)
	}
}

// writeRecords writes instances as a JSON-lines records file.
func writeRecords(t *testing.T, path string, instances ...Instance) {
	t.Helper()
	var out []byte
	for _, inst := range instances {
		line, err := json.Marshal(inst)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		out = append(append(out, line...), '\n')
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readFile returns path's content, failing t if it cannot be read.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path under this test's own temp directory
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// TestRunCorpus_ResumeNeverRepeatsAFinishedCell is the property the owner
// cares about most. A ~1,000-cell live run will be interrupted; a resumed run
// must never run, and never pay for, a cell that finished. The corpus is
// interrupted inside the second task's belay cell, during its code step,
// after its plan step has finished.
//
// On resume: the finished cells are not touched -- no agent call, no
// provisioning, no re-evaluation, and their result files are byte for byte
// what they were; the interrupted belay cell resumes from belay's own
// journal, so its finished plan step is not paid for again; and a third run
// finds nothing to do at all.
func TestRunCorpus_ResumeNeverRepeatsAFinishedCell(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 2)
	dir := t.TempDir()
	eval := &fakeEvaluator{}
	prov := newTemplateProvisioner()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	interrupting := &cellAgent{repairFrom: 1, onCall: func(ctx context.Context, n int, _ belay.AgentRequest) error {
		if n == 2 { // the code step; the plan step was call 1
			cancel()
			return ctx.Err()
		}
		return nil
	}}
	first := map[string]*cellAgent{
		"example__widget-1/belay-r1":   {repairFrom: 1},
		"example__widget-1/control-r1": {repairFrom: 1},
		"example__widget-2/belay-r1":   interrupting,
		"example__widget-2/control-r1": {repairFrom: 1},
	}
	_, err := RunCorpus(ctx, corpusOptions(t, dir, tasks, first, eval, prov))
	if !errors.Is(err, ErrCorpusInterrupted) {
		t.Fatalf("first RunCorpus() error = %v, want it to wrap ErrCorpusInterrupted", err)
	}
	interrupted := filepath.Join(dir, "cells", "example__widget-2", "belay-r1")
	if _, err := os.Stat(filepath.Join(interrupted, "result.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the interrupted cell has a result (stat: %v); an unfinished cell must not", err)
	}
	finished := filepath.Join(dir, "cells", "example__widget-1", "belay-r1", "result.json")
	before := readFile(t, finished)

	second := map[string]*cellAgent{
		"example__widget-1/belay-r1":   {repairFrom: 1},
		"example__widget-1/control-r1": {repairFrom: 1},
		"example__widget-2/belay-r1":   {repairFrom: 1},
		"example__widget-2/control-r1": {repairFrom: 1},
	}
	report, err := RunCorpus(t.Context(), corpusOptions(t, dir, tasks, second, eval, prov))
	if err != nil {
		t.Fatalf("second RunCorpus() error = %v", err)
	}
	for _, key := range []string{"example__widget-1/belay-r1", "example__widget-1/control-r1"} {
		if n := second[key].calls.Load(); n != 0 {
			t.Errorf("%s finished before the interruption, and the resume made %d agent call(s) for it", key, n)
		}
		if n := eval.count(key); n != 1 {
			t.Errorf("%s was evaluated %d time(s), want once", key, n)
		}
	}
	if n := prov.freshCount("example__widget-1/belay-r1"); n != 1 {
		t.Errorf("a finished cell's workspace was provisioned %d time(s), want once", n)
	}
	if after := readFile(t, finished); string(after) != string(before) {
		t.Errorf("a finished cell's result changed on resume:\nbefore: %s\nafter:  %s", before, after)
	}
	resumed := second["example__widget-2/belay-r1"]
	if n := resumed.plans.Load(); n != 0 {
		t.Errorf("the interrupted belay cell planned %d time(s) on resume; its plan step had finished, and belay's journal says so", n)
	}
	if n := resumed.edits.Load(); n == 0 {
		t.Error("the interrupted belay cell never re-ran the step it was interrupted in")
	}
	if n := prov.freshCount("example__widget-2/belay-r1"); n != 1 {
		t.Errorf("the interrupted belay cell's workspace was made %d time(s); a resumed run must find the one it left", n)
	}
	if report.Status != CorpusComplete || report.Belay.Resolved != 2 || report.Control.Resolved != 2 {
		t.Errorf("after resume: status %q, belay %d, control %d resolved; want complete, 2, 2",
			report.Status, report.Belay.Resolved, report.Control.Resolved)
	}

	third := map[string]*cellAgent{}
	for key := range second {
		third[key] = &cellAgent{repairFrom: 1}
	}
	if _, err := RunCorpus(t.Context(), corpusOptions(t, dir, tasks, third, eval, prov)); err != nil {
		t.Fatalf("third RunCorpus() error = %v", err)
	}
	for key, a := range third {
		if n := a.calls.Load(); n != 0 {
			t.Errorf("a complete corpus made %d agent call(s) for %s when run again", n, key)
		}
	}
}

// TestRunCorpus_AnInterruptedControlCallStartsAgainFromTheBase: a single call
// cannot be resumed, so a control cell interrupted mid-call runs again -- but
// against a fresh workspace, never against whatever half an edit the cut-off
// call left behind, which would hand the second call a head start no
// uninterrupted control cell gets.
func TestRunCorpus_AnInterruptedControlCallStartsAgainFromTheBase(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 1)
	dir := t.TempDir()
	prov := newTemplateProvisioner()
	opts := func(a *cellAgent) CorpusOptions {
		o := corpusOptions(t, dir, tasks, map[string]*cellAgent{"example__widget-1/control-r1": a}, &fakeEvaluator{}, prov)
		o.Arms = []Arm{ArmControl}
		return o
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cutOff := &cellAgent{onCall: func(ctx context.Context, _ int, req belay.AgentRequest) error {
		if err := os.WriteFile(filepath.Join(req.WorkDir, "half-done.txt"), []byte("partial\n"), 0o600); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	}}
	if _, err := RunCorpus(ctx, opts(cutOff)); !errors.Is(err, ErrCorpusInterrupted) {
		t.Fatalf("first RunCorpus() error = %v, want ErrCorpusInterrupted", err)
	}

	var sawHalfDone atomic.Bool
	again := &cellAgent{repairFrom: 1, onCall: func(_ context.Context, _ int, req belay.AgentRequest) error {
		if _, err := os.Stat(filepath.Join(req.WorkDir, "half-done.txt")); err == nil {
			sawHalfDone.Store(true)
		}
		return nil
	}}
	if _, err := RunCorpus(t.Context(), opts(again)); err != nil {
		t.Fatalf("second RunCorpus() error = %v", err)
	}
	if sawHalfDone.Load() {
		t.Error("the re-run control call found the interrupted call's half-done edit in its workspace")
	}
	if n := again.calls.Load(); n != 1 {
		t.Errorf("the re-run control cell made %d call(s), want 1", n)
	}
	var res CellResult
	if err := json.Unmarshal(readFile(t, filepath.Join(dir, "cells", "example__widget-1", "control-r1", "result.json")), &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2: the interrupted call and the one that finished", res.Attempts)
	}
}

// TestRunCorpus_AFinishedControlCallIsNeverMadeAgain: once a control call has
// returned, what it returned is kept before anything else is done with it,
// so an interruption after the call -- while the patch is being taken, say --
// does not cost the call again.
func TestRunCorpus_AFinishedControlCallIsNeverMadeAgain(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 1)
	dir := t.TempDir()
	prov := newTemplateProvisioner()
	opts := func(a *cellAgent) CorpusOptions {
		o := corpusOptions(t, dir, tasks, map[string]*cellAgent{"example__widget-1/control-r1": a}, &fakeEvaluator{}, prov)
		o.Arms = []Arm{ArmControl}
		return o
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The call finishes, and the corpus is interrupted at that same moment.
	finishing := &cellAgent{repairFrom: 1, onCall: func(context.Context, int, belay.AgentRequest) error {
		cancel()
		return nil
	}}
	if _, err := RunCorpus(ctx, opts(finishing)); !errors.Is(err, ErrCorpusInterrupted) {
		t.Fatalf("first RunCorpus() error = %v, want ErrCorpusInterrupted", err)
	}
	if n := finishing.calls.Load(); n != 1 {
		t.Fatalf("first run made %d call(s), want 1", n)
	}

	again := &cellAgent{repairFrom: 1}
	report, err := RunCorpus(t.Context(), opts(again))
	if err != nil {
		t.Fatalf("second RunCorpus() error = %v", err)
	}
	if n := again.calls.Load(); n != 0 {
		t.Errorf("the resume made %d call(s) for a control cell whose call had already returned", n)
	}
	if report.Control.Resolved != 1 {
		t.Errorf("Control.Resolved = %d, want the first call's repair to count", report.Control.Resolved)
	}
}

// TestRunCorpus_StopsBeforeTheSpendCap: the cap is checked before each cell
// starts, against everything spent so far plus the most the cell may spend --
// belay's per-run budget.max_usd, which both arms are reserved -- so the corpus
// stops before a cell that could take it past the cap, not after. Raising the
// cap and resuming continues where it stopped, the way raising
// budget.max_usd and resuming continues a belay run.
func TestRunCorpus_StopsBeforeTheSpendCap(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 4)
	dir := t.TempDir()
	agents := map[string]*cellAgent{}
	for _, task := range tasks {
		agents[task.ID+"/control-r1"] = &cellAgent{repairFrom: 1, usd: 1.5}
	}
	opts := corpusOptions(t, dir, tasks, agents, &fakeEvaluator{}, newTemplateProvisioner())
	opts.Mode, opts.Confirm = ModeLive, ConfirmLiveSpend
	opts.Arms = []Arm{ArmControl}
	opts.MaxUSD = 5 // reservation 2 per cell: 0+2, 1.5+2 and 3+2 fit; 4.5+2 does not

	report, err := RunCorpus(t.Context(), opts)
	if !errors.Is(err, ErrSpendCapReached) {
		t.Fatalf("RunCorpus() error = %v, want it to wrap ErrSpendCapReached", err)
	}
	var calls int32
	for _, a := range agents {
		calls += a.calls.Load()
	}
	if calls != 3 {
		t.Errorf("%d cell(s) were run, want 3: the fourth could have taken the corpus past its cap", calls)
	}
	if report.Status != CorpusStoppedAtCap || report.Spend.TotalUSD != 4.5 || report.Spend.CapUSD != 5 {
		t.Errorf("report: status %q, spend %+v; want stopped at the cap having spent 4.5 of 5", report.Status, report.Spend)
	}

	opts.MaxUSD = 10
	report, err = RunCorpus(t.Context(), opts)
	if err != nil {
		t.Fatalf("resumed RunCorpus() error = %v", err)
	}
	calls = 0
	for _, a := range agents {
		calls += a.calls.Load()
	}
	if calls != 4 || report.Status != CorpusComplete || report.Spend.TotalUSD != 6 {
		t.Errorf("after raising the cap: %d call(s), status %q, spend %+v; want 4, complete, 6", calls, report.Status, report.Spend)
	}
}

// TestRunCorpus_ChargesAnInterruptedAttempt: a call cut off mid-flight may
// have spent anything, and nothing reports it. The corpus charges it the
// cell's whole reservation, the way belay charges a step a crash left open
// its run's costliest step: the direction that can stop a run early, never
// the one that lets it overrun its cap.
func TestRunCorpus_ChargesAnInterruptedAttempt(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 1)
	dir := t.TempDir()
	opts := func(a *cellAgent) CorpusOptions {
		o := corpusOptions(t, dir, tasks, map[string]*cellAgent{"example__widget-1/control-r1": a}, &fakeEvaluator{}, newTemplateProvisioner())
		o.Mode, o.Confirm, o.MaxUSD = ModeLive, ConfirmLiveSpend, 100
		o.Arms = []Arm{ArmControl}
		return o
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cutOff := &cellAgent{onCall: func(ctx context.Context, _ int, _ belay.AgentRequest) error {
		cancel()
		return ctx.Err()
	}}
	if _, err := RunCorpus(ctx, opts(cutOff)); !errors.Is(err, ErrCorpusInterrupted) {
		t.Fatalf("first RunCorpus() error = %v, want ErrCorpusInterrupted", err)
	}
	report, err := RunCorpus(t.Context(), opts(&cellAgent{repairFrom: 1, usd: 0.75}))
	if err != nil {
		t.Fatalf("second RunCorpus() error = %v", err)
	}
	if s := report.Spend; s.RecordedUSD != 0.75 || s.ChargedUSD != 2 || s.TotalUSD != 2.75 || !s.Estimated {
		t.Errorf("Spend = %+v, want 0.75 recorded, the 2 reserved charged for the cut-off call, 2.75 total, estimated", s)
	}
}

// TestRunCorpus_RefusesAChangedCorpus: a corpus is one measurement, and every
// cell in it must have been run under the same settings. A resume asked to
// run with a different model, different tasks or different replicates would
// silently mix two measurements into one number, so it is refused, by name,
// before anything runs.
func TestRunCorpus_RefusesAChangedCorpus(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 2)
	dir := t.TempDir()
	base := func() CorpusOptions {
		o := corpusOptions(t, dir, tasks, map[string]*cellAgent{}, &fakeEvaluator{}, newTemplateProvisioner())
		o.Arms = []Arm{ArmControl}
		return o
	}
	if _, err := RunCorpus(t.Context(), base()); err != nil {
		t.Fatalf("first RunCorpus() error = %v", err)
	}

	tests := []struct {
		name   string
		field  string
		mutate func(*CorpusOptions)
	}{
		{"another model", "config", func(o *CorpusOptions) { o.Config.Agent.Model = "claude-opus-5" }},
		{"other tasks", "tasks", func(o *CorpusOptions) { o.Tasks = o.Tasks[:1] }},
		{"another replicate count", "replicates", func(o *CorpusOptions) { o.Replicates = 2 }},
		{"another arm", "arms", func(o *CorpusOptions) { o.Arms = []Arm{ArmBelay, ArmControl} }},
		{"other records", "records", func(o *CorpusOptions) { o.RecordsSHA256 = strings.Repeat("1", 64) }},
		{"another mode", "mode", func(o *CorpusOptions) { o.Mode, o.Confirm, o.MaxUSD = ModeLive, ConfirmLiveSpend, 10 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := base()
			tt.mutate(&o)
			_, err := RunCorpus(t.Context(), o)
			if !errors.Is(err, ErrCorpusMismatch) {
				t.Fatalf("RunCorpus() error = %v, want it to wrap ErrCorpusMismatch", err)
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Errorf("error = %v, want it to name %q", err, tt.field)
			}
		})
	}
}

// TestRunCorpus_ScriptedModeRefusesSpend keeps scripted mode's zero-cost
// promise for the corpus, as runArm keeps it for one task.
func TestRunCorpus_ScriptedModeRefusesSpend(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 1)
	opts := corpusOptions(t, t.TempDir(), tasks, map[string]*cellAgent{"example__widget-1/control-r1": {usd: 0.01}},
		&fakeEvaluator{}, newTemplateProvisioner())
	opts.Arms = []Arm{ArmControl}
	if _, err := RunCorpus(t.Context(), opts); !errors.Is(err, ErrReplayModeSpentMoney) {
		t.Errorf("RunCorpus() error = %v, want it to wrap ErrReplayModeSpentMoney", err)
	}
}

// TestRunCorpus_EvaluatesEachCellOnce: grading is free but slow, and the
// harness caches by run id anyway; still, a resumed corpus must not ask for a
// verdict it already has, and a harness that could not run leaves the cell
// waiting for one rather than recording a guess.
func TestRunCorpus_EvaluatesEachCellOnce(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 1)
	dir := t.TempDir()
	eval := &fakeEvaluator{fail: errors.New("docker: cannot connect to the Docker daemon")}
	prov := newTemplateProvisioner()
	opts := func(a *cellAgent) CorpusOptions {
		o := corpusOptions(t, dir, tasks, map[string]*cellAgent{"example__widget-1/control-r1": a}, eval, prov)
		o.Arms = []Arm{ArmControl}
		return o
	}
	if _, err := RunCorpus(t.Context(), opts(&cellAgent{repairFrom: 1})); err == nil {
		t.Fatal("RunCorpus() = nil error with a harness that cannot run, want an error")
	}
	cell := filepath.Join(dir, "cells", "example__widget-1", "control-r1")
	if _, err := os.Stat(filepath.Join(cell, "verdict.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a verdict was recorded although the harness could not run (stat: %v)", err)
	}

	eval.mu.Lock()
	eval.fail = nil
	eval.mu.Unlock()
	again := &cellAgent{repairFrom: 1}
	if _, err := RunCorpus(t.Context(), opts(again)); err != nil {
		t.Fatalf("second RunCorpus() error = %v", err)
	}
	if n := again.calls.Load(); n != 0 {
		t.Errorf("waiting for a verdict cost %d agent call(s); the cell had finished", n)
	}
	if _, err := RunCorpus(t.Context(), opts(&cellAgent{})); err != nil {
		t.Fatalf("third RunCorpus() error = %v", err)
	}
	if n := eval.count("example__widget-1/control-r1"); n != 2 {
		t.Errorf("the cell was evaluated %d time(s), want 2: the failed attempt, then once", n)
	}
}

// brokenRunner fails every Test the way a container that died does: not a
// failing test run, but no test run at all.
type brokenRunner struct{ belay.TestRunner }

func (brokenRunner) Test(context.Context, string) (belay.TestReport, error) {
	return belay.TestReport{}, errors.New("docker: Error response from daemon: container is not running")
}

// brokenProvisioner gives the belay arm a test runner that cannot run, until
// it is mended.
type brokenProvisioner struct {
	*templateProvisioner
	broken atomic.Bool
}

func (p *brokenProvisioner) Provision(ctx context.Context, cell Cell, dir string, fresh bool) (Workspace, error) {
	ws, err := p.templateProvisioner.Provision(ctx, cell, dir, fresh)
	if err == nil && p.broken.Load() {
		ws.Runner = brokenRunner{ws.Runner}
	}
	return ws, err
}

// TestRunCorpus_AnInfrastructureFailureIsNotTheArmsOutcome: a test run that
// could not happen -- the container died, the daemon went away -- is the
// harness's failure, not belay's. Left to belay's own rules it would end the
// run as failed, the corpus would record the cell as finished, and a finished
// cell is never run again: a Docker hiccup would be scored as belay losing
// the task. Instead the run pauses, the corpus stops and says why, nothing is
// recorded for the cell, and once the infrastructure is mended a resume
// continues the run from the step that could not run -- without planning or
// coding again.
func TestRunCorpus_AnInfrastructureFailureIsNotTheArmsOutcome(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 1)
	dir := t.TempDir()
	prov := &brokenProvisioner{templateProvisioner: newTemplateProvisioner()}
	prov.broken.Store(true)
	opts := func(a *cellAgent) CorpusOptions {
		o := corpusOptions(t, dir, tasks, map[string]*cellAgent{"example__widget-1/belay-r1": a}, &fakeEvaluator{}, prov)
		o.Arms = []Arm{ArmBelay}
		return o
	}

	first := &cellAgent{repairFrom: 1}
	_, err := RunCorpus(t.Context(), opts(first))
	if !errors.Is(err, ErrInfrastructure) {
		t.Fatalf("RunCorpus() error = %v, want it to wrap ErrInfrastructure", err)
	}
	cell := filepath.Join(dir, "cells", "example__widget-1", "belay-r1")
	if _, err := os.Stat(filepath.Join(cell, "result.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a cell whose tests could not run was recorded as finished (stat: %v)", err)
	}

	prov.broken.Store(false)
	again := &cellAgent{repairFrom: 1}
	report, err := RunCorpus(t.Context(), opts(again))
	if err != nil {
		t.Fatalf("resumed RunCorpus() error = %v", err)
	}
	if n := again.calls.Load(); n != 0 {
		t.Errorf("the resume made %d agent call(s); the plan and code steps had finished before the tests could not run", n)
	}
	if report.Belay.Resolved != 1 {
		t.Errorf("Belay.Resolved = %d, want the resumed run's repair to count", report.Belay.Resolved)
	}
}

// failingBackend fails every call before spending anything, the way a
// backend with an expired key or no credit left does.
func failingBackend() *belaytest.FakeAgent {
	return &belaytest.FakeAgent{NameValue: "fixrate-no-credit", Func: func(context.Context, belay.AgentRequest) (belay.AgentResponse, error) {
		return belay.AgentResponse{}, errors.New("claude failed with exit code 1: Credit balance is too low")
	}}
}

// TestRunCorpus_AnArmThatSpentNothingDidNotAttemptTheTask: in a live corpus, a
// call that fails before spending a cent never reached the task -- an expired
// key, credit that ran out in the night. Recorded as the arm's outcome, it
// would turn the rest of a thousand-cell run into a thousand recorded
// failures in minutes, none of which could ever be run again. So the corpus
// stops, says why, and records nothing, in either arm; mend the key and the
// same command resumes.
func TestRunCorpus_AnArmThatSpentNothingDidNotAttemptTheTask(t *testing.T) {
	t.Parallel()

	for _, arm := range []Arm{ArmBelay, ArmControl} {
		t.Run(string(arm), func(t *testing.T) {
			t.Parallel()
			tasks := widgetTasks(t, 1)
			dir := t.TempDir()
			key := "example__widget-1/" + string(arm) + "-r1"
			working := &cellAgent{repairFrom: 1, usd: 0.25}
			var broken atomic.Bool
			broken.Store(true)
			opts := corpusOptions(t, dir, tasks, nil, &fakeEvaluator{}, newTemplateProvisioner())
			opts.Mode, opts.Confirm, opts.MaxUSD = ModeLive, ConfirmLiveSpend, 100
			opts.Arms = []Arm{arm}
			opts.AgentFor = func(Cell) belay.AgentBackend {
				if broken.Load() {
					return failingBackend()
				}
				return working.backend()
			}

			if _, err := RunCorpus(t.Context(), opts); !errors.Is(err, ErrInfrastructure) {
				t.Fatalf("RunCorpus() error = %v, want it to wrap ErrInfrastructure", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "cells", "example__widget-1", string(arm)+"-r1", "result.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s: a cell whose backend spent nothing and failed was recorded (stat: %v)", key, err)
			}

			broken.Store(false)
			report, err := RunCorpus(t.Context(), opts)
			if err != nil {
				t.Fatalf("resumed RunCorpus() error = %v", err)
			}
			if n := working.calls.Load(); n == 0 {
				t.Errorf("%s: the resume made no call; the failed attempt had not reached the task", key)
			}
			summary := report.Belay
			if arm == ArmControl {
				summary = report.Control
			}
			if summary.Resolved != 1 {
				t.Errorf("%s: Resolved = %d after the backend was mended, want 1", key, summary.Resolved)
			}
			if report.Spend.ChargedUSD != 0 {
				t.Errorf("%s: Spend = %+v; an attempt the backend reported spending nothing on is not charged", key, report.Spend)
			}
		})
	}
}

// TestRunCorpus_AfterTaskRunsOnceATaskIsScored: a SWE-bench instance image is
// gigabytes, and a full corpus is 500 of them. AfterTask is where a caller
// frees what a task needed, so it must run once every cell of the task has a
// verdict -- not before, when a cell may still need the image, and not again
// on a resume.
func TestRunCorpus_AfterTaskRunsOnceATaskIsScored(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 2)
	dir := t.TempDir()
	var mu sync.Mutex
	var done []string
	opts := corpusOptions(t, dir, tasks, nil, &fakeEvaluator{}, newTemplateProvisioner())
	opts.Arms = []Arm{ArmControl}
	opts.AfterTask = func(_ context.Context, task Task) error {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range []string{"result.json", "verdict.json"} {
			if !exists(filepath.Join(dir, "cells", task.ID, "control-r1", c)) {
				t.Errorf("AfterTask(%s) ran before its cell had %s", task.ID, c)
			}
		}
		done = append(done, task.ID)
		return errors.New("an image that would not be removed")
	}
	if _, err := RunCorpus(t.Context(), opts); err != nil {
		t.Fatalf("RunCorpus() error = %v; a failed AfterTask is logged, not fatal", err)
	}
	if _, err := RunCorpus(t.Context(), opts); err != nil {
		t.Fatalf("second RunCorpus() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"example__widget-1", "example__widget-2"}; !slices.Equal(done, want) {
		t.Errorf("AfterTask ran for %v, want %v: once each, and not again on a resume", done, want)
	}
}
