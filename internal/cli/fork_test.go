package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
	"syscall"
)

// TestResolveAppliesAnOverrideBeforeItResolvesAnything: a command that
// changes its run's configuration has to change it before belay decides
// anything from it -- where the output goes, which models are priced -- or
// the plan would describe one run and the dispatcher walk another. And the
// result is held to the same rules as belay.yaml.
func TestResolveAppliesAnOverrideBeforeItResolvesAnything(t *testing.T) {
	ws := goWorkspace(t)

	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger(), override: func(c *config.Config) {
		c.Output.Mode = config.OutputInPlace
		c.Agent.Models = config.Models{"fix": "opus"}
	}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := plan.config.Agent.ModelFor("fix"); got != "opus" {
		t.Errorf("fix is sent %q, want the override's opus", got)
	}
	var priced []string
	for _, m := range plan.models {
		priced = append(priced, m.node+"="+m.model)
	}
	if !strings.Contains(strings.Join(priced, " "), "fix=opus") {
		t.Errorf("models resolved %v, want fix=opus among them: the override comes before pricing", priced)
	}

	_, err = resolve(resolveRequest{workspace: ws, logger: quietLogger(), override: func(c *config.Config) {
		c.Agent.Models = config.Models{"test": "opus"}
	}})
	if err == nil || !strings.Contains(err.Error(), "agent.models") {
		t.Errorf("resolve with an override belay.yaml could not hold = %v, want the configuration refused", err)
	}
}

// TestAForkIsASingleRunWhateverBelayYamlSaysAboutFanout: a fork re-runs one
// node of the single-run graph, so it turns fanout off as it turns branch
// output off. With fanout.enabled left on, the fork's graph was the fanout
// graph -- plan, approve, fanout, join -- and forking test failed the new
// run with "unknown node". The fork resolves no fanout pin, its registry
// holds every node a fork can re-run and no fanout, and a review mode only a
// fanout refuses is not refused.
func TestAForkIsASingleRunWhateverBelayYamlSaysAboutFanout(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nfanout:\n  enabled: true\n  candidates: 3\n")
	req := forkRequest{runID: "20260902T000000Z-f0f0f0f0f0f0", node: graph.NodeTest}

	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger(), override: req.configure})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.fanout != nil {
		t.Errorf("the fork's plan fans out into %d candidates; a fork is a single run", plan.fanout.Candidates)
	}
	if plan.registry == nil {
		t.Fatalf("no registry built; blockers: %s", blockerText(plan))
	}
	names := plan.registry.Names()
	for _, node := range []string{graph.NodePlan, graph.NodeCode, graph.NodeWrite, graph.NodeTest, graph.NodeFix, graph.NodeReview} {
		if !slices.Contains(names, node) {
			t.Errorf("the fork's registry %v has no %s; forking it would fail the fork on an unknown node", names, node)
		}
	}
	if slices.Contains(names, graph.NodeFanout) || slices.Contains(plan.route, graph.NodeFanout) {
		t.Errorf("the fork's registry %v or route %v has the fanout", names, plan.route)
	}
}

// TestNextStepsFindARunInItsCopy: a fork lives in a copy of the workspace,
// so every command the closing report writes out for it names that copy, or
// a person pasting it would be told the run does not exist. A fork paused
// after the one node it re-ran is waiting for nothing, so it is not told to
// read a plan; a fork whose gate paused to ask is. A run that lives where
// the command ran is told what it always was.
func TestNextStepsFindARunInItsCopy(t *testing.T) {
	const id = "20260902T000000Z-f0f0f0f0f0f0"
	copyDir := filepath.Join(t.TempDir(), "my work", ".belay", "forks", id)
	layout, err := state.NewLayout(copyDir, id)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	quoted := "'" + copyDir + "'"
	in := "belay --workspace " + quoted + " "

	tests := []struct {
		name    string
		view    outcomeView
		want    []string
		wantNot []string
	}{
		{
			name: "paused after the node it re-ran",
			view: outcomeView{layout: layout, workspace: copyDir, pausedAsAsked: true,
				outcome: graph.Outcome{Status: state.RunStatusPaused, Node: graph.NodeTest, Steps: 1}},
			want:    []string{in + "timeline " + id, in + "resume " + id, "test"},
			wantNot: []string{"read the plan"},
		},
		{
			name: "its gate paused to ask",
			view: outcomeView{layout: layout, workspace: copyDir,
				outcome: graph.Outcome{Status: state.RunStatusPaused, Node: graph.NodeApprove, Steps: 1}},
			want: []string{"read the plan", in + "resume " + id},
		},
		{
			name: "interrupted",
			view: outcomeView{layout: layout, workspace: copyDir, runErr: graph.ErrInterrupted,
				outcome: graph.Outcome{Status: state.RunStatusPaused, Node: graph.NodeFix}},
			want: []string{in + "resume " + id},
		},
		{
			name: "failed",
			view: outcomeView{layout: layout, workspace: copyDir,
				outcome: graph.Outcome{Status: state.RunStatusFailed, Node: graph.NodeFix, Steps: 1}},
			want: []string{in + "timeline " + id},
		},
		{
			name: "ended",
			view: outcomeView{layout: layout, workspace: copyDir,
				outcome: graph.Outcome{Status: state.RunStatusAborted, Node: graph.NodeFix, Steps: 1}},
			want: []string{in + "timeline " + id},
		},
		{
			name: "finished in its copy",
			view: outcomeView{layout: layout, workspace: copyDir,
				outcome: graph.Outcome{Status: state.RunStatusCompleted, Node: graph.End, Steps: 1}},
			want:    []string{"its copy", quoted, in + "timeline " + id},
			wantNot: []string{"your repository"},
		},
		{
			name: "a run where the command ran",
			view: outcomeView{layout: layout,
				outcome: graph.Outcome{Status: state.RunStatusPaused, Node: graph.NodeApprove, Steps: 2}},
			want:    []string{"belay resume " + id},
			wantNot: []string{"--workspace"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(nextSteps(tt.view), "\n")
			for _, phrase := range tt.want {
				if !strings.Contains(got, phrase) {
					t.Errorf("next steps never say %q:\n%s", phrase, got)
				}
			}
			for _, phrase := range tt.wantNot {
				if strings.Contains(got, phrase) {
					t.Errorf("next steps say %q, and must not:\n%s", phrase, got)
				}
			}
		})
	}
}

const forkGoal = "make the widget work"

// scriptedNode stands in for one node of the graph: it records the context
// it was given, makes its edit, and goes where it is told, at $0.10 a run.
type scriptedNode struct {
	name, next string
	edit       func(rc *graph.RunContext) (state.Patch, error)
	seen       []graph.RunContext
}

func (n *scriptedNode) Name() string { return n.name }

func (n *scriptedNode) Run(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
	n.seen = append(n.seen, *rc)
	var patch state.Patch
	if n.edit != nil {
		p, err := n.edit(rc)
		if err != nil {
			return graph.Result{}, err
		}
		patch = p
	}
	return graph.Result{Status: journal.StatusOK, Next: n.next, Patch: patch, Usage: belay.Usage{USD: 0.10}}, nil
}

// forkScript is the run the fork tests fork, as stand-ins:
//
//	plan → approve → code → test → review → END
//
// plan writes the plan; code rewrites main.go and opens the agent session
// "sess-1", so the files change at code and at nothing after it.
func forkScript() (*graph.Registry, map[string]*scriptedNode) {
	const body = "# Plan\n\n1. Make the widget work.\n"
	nodes := map[string]*scriptedNode{
		graph.NodePlan: {name: graph.NodePlan, next: graph.NodeApprove, edit: func(rc *graph.RunContext) (state.Patch, error) {
			rel, err := rc.WriteArtifact("plan.md", []byte(body))
			return state.Patch{Plan: &state.Plan{Path: rel, Digest: approve.Digest([]byte(body)), Approved: true}}, err
		}},
		graph.NodeApprove: {name: graph.NodeApprove, next: graph.NodeCode},
		graph.NodeCode: {name: graph.NodeCode, next: graph.NodeTest, edit: func(rc *graph.RunContext) (state.Patch, error) {
			err := os.WriteFile(filepath.Join(rc.Workspace, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600)
			return state.Patch{Code: &state.Code{SessionID: "sess-1", ChangedFiles: []string{"main.go"}}}, err
		}},
		graph.NodeTest:   {name: graph.NodeTest, next: graph.NodeReview},
		graph.NodeReview: {name: graph.NodeReview, next: graph.End},
	}
	registry := graph.NewRegistry()
	for _, n := range nodes {
		registry.MustRegister(n)
	}
	return registry, nodes
}

// forkSource records a finished run of forkScript in a fresh Go workspace,
// through the CLI's own wiring, so its snapshots carry the fingerprint every
// run records, and returns the workspace and the run's id.
func forkSource(t *testing.T) (ws, runID string) {
	t.Helper()
	ws = goWorkspace(t)
	return ws, forkSourceIn(t, ws)
}

// forkSourceIn is forkSource in a workspace the caller made, and returns the
// run's id.
func forkSourceIn(t *testing.T, ws string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	plan.registry, _ = forkScript()
	store := seedRunDir(t, plan, forkGoal)
	d, err := graph.NewDispatcher(plan.options(store))
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if out, err := d.Run(context.Background()); err != nil || out.Status != state.RunStatusCompleted {
		t.Fatalf("recording the source run: %v, %v; want it completed", out.Status, err)
	}
	return store.Layout().RunID()
}

// forksIn lists the forks made in ws.
func forksIn(t *testing.T, ws string) []string {
	t.Helper()
	dir, err := state.ForksDir(ws)
	if err != nil {
		t.Fatalf("ForksDir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestForkCommand: what `belay fork` says, and what it refuses. Every
// refusal is made before anything is created or spent, and says what to do
// instead.
func TestForkCommand(t *testing.T) {
	ws, id := forkSource(t)

	tests := []struct {
		name       string
		args       []string
		wantErr    bool
		wantSaying []string
	}{
		{
			name: "help", args: []string{"fork", "--help"},
			wantSaying: []string{"copy", "--visit", "--current-files", "--model", "belay timeline", "resume"},
		},
		{name: "no step named", args: []string{"--workspace", ws, "fork", id}, wantErr: true, wantSaying: []string{"belay fork " + id + " <node>"}},
		{name: "nothing named", args: []string{"--workspace", ws, "fork"}, wantErr: true, wantSaying: []string{"belay fork <run-id> <node>"}},
		{
			name: "a run that is not there", args: []string{"--workspace", ws, "fork", "20990101T000000Z-000000000000", "test"},
			wantErr: true, wantSaying: []string{"no run called 20990101T000000Z-000000000000", id},
		},
		{
			name: "a step the run never reached", args: []string{"--workspace", ws, "fork", id, "fix"},
			wantErr: true, wantSaying: []string{"belay will not fork this run", "never ran fix", "belay timeline " + id, "Nothing was created"},
		},
		{
			name: "files that have changed", args: []string{"--workspace", ws, "fork", id, "code"},
			wantErr: true, wantSaying: []string{"have changed since code #1 ran", "--current-files"},
		},
		{
			name: "a model for a step that asks for none", args: []string{"--workspace", ws, "fork", id, "test", "--model", "opus"},
			wantErr: true, wantSaying: []string{"test asks no model", "plan, code, fix"},
		},
		{
			name: "a visit below 1", args: []string{"--workspace", ws, "fork", id, "test", "--visit", "-1"},
			wantErr: true, wantSaying: []string{"--visit counts from 1"},
		},
		{
			name: "fanout", args: []string{"--workspace", ws, "fork", id, "fanout"},
			wantErr: true, wantSaying: []string{"belay cannot fork fanout"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, err := runCLI(t, tt.args...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v\nstdout:\n%s\nstderr:\n%s", err, tt.wantErr, stdout, stderr)
			}
			said := stdout + stderr
			if err != nil {
				said += err.Error()
			}
			for _, phrase := range tt.wantSaying {
				if !strings.Contains(said, phrase) {
					t.Errorf("belay fork never says %q:\n%s", phrase, said)
				}
			}
			if got := forksIn(t, ws); len(got) != 0 {
				t.Errorf("a refusal made forks %v; it must create nothing", got)
			}
		})
	}
}

// TestForkDryRunDescribesTheForkAndMakesNothing: --dry-run says which visit
// would be re-run, where, how it would differ and what it could spend, from
// the same decision a real fork makes, and creates nothing at all.
func TestForkDryRunDescribesTheForkAndMakesNothing(t *testing.T) {
	ws, id := forkSource(t)

	stdout, stderr, err := runCLI(t, "--workspace", ws, "--dry-run", "fork", id, "code", "--current-files", "--model", "opus")
	if err != nil {
		t.Fatalf("error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	for _, phrase := range []string{
		"This is a dry run",
		id,
		"code #1 of 1",
		"a copy of " + ws,
		`code is sent the model "opus"`,
		"have changed since code #1 ran",
		"$0.20",
		"the most it could spend",
		"Nothing was created and nothing was spent",
	} {
		if !strings.Contains(stdout, phrase) {
			t.Errorf("the dry run never says %q:\n%s", phrase, stdout)
		}
	}
	if got := forksIn(t, ws); len(got) != 0 {
		t.Errorf("a dry run made forks %v", got)
	}
}

// TestForkRunsOneNodeInItsCopy: a fork runs the one node it was asked to, in
// its own copy, from the blackboard the visit was given, and pauses before
// the next; the report says how to see what it did and how to carry it on,
// from the copy; and the source run is exactly as it was.
func TestForkRunsOneNodeInItsCopy(t *testing.T) {
	ws, id := forkSource(t)
	runDir := filepath.Join(ws, ".belay", "runs", id)
	before := treeSums(t, runDir)

	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	req := forkRequest{runID: id, node: graph.NodeTest}
	plan, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u), override: req.configure})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	registry, nodes := forkScript()
	plan.registry = registry

	if err := makeFork(cmdWithContext(), u, plan, req); err != nil {
		t.Fatalf("makeFork: %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("verbatim output:\n%s", got)

	forks := forksIn(t, ws)
	if len(forks) != 1 {
		t.Fatalf("forks = %v, want exactly one", forks)
	}
	forkID := forks[0]
	copyDir := filepath.Join(ws, ".belay", "forks", forkID)
	test := nodes[graph.NodeTest]
	if len(test.seen) != 1 {
		t.Fatalf("test ran %d times, want once", len(test.seen))
	}
	if ran := test.seen[0].Workspace; evalPath(t, ran) != evalPath(t, copyDir) {
		t.Errorf("test ran in %s, want the fork's copy %s", ran, copyDir)
	}
	for _, other := range []string{graph.NodePlan, graph.NodeApprove, graph.NodeCode, graph.NodeReview} {
		if n := len(nodes[other].seen); n != 0 {
			t.Errorf("%s ran %d times; a fork runs only the node it re-runs", other, n)
		}
	}
	if sid := test.seen[0].State.Code.SessionID; sid != "" {
		t.Errorf("test was given the session %q; a fork starts a new conversation", sid)
	}

	for _, phrase := range []string{
		"belay is forking run " + id,
		"test #1 of 1",
		"fork " + forkID,
		"belay --workspace " + shellQuote(evalPath(t, copyDir)) + " timeline " + forkID,
		"belay --workspace " + shellQuote(evalPath(t, copyDir)) + " resume " + forkID,
	} {
		if !strings.Contains(got, phrase) {
			t.Errorf("belay fork never says %q:\n%s", phrase, got)
		}
	}

	man, err := state.LoadManifest(filepath.Join(copyDir, ".belay", "runs", forkID, "manifest.json"))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if man.Origin == nil || man.Origin.RunID != id || man.Status != state.RunStatusPaused {
		t.Errorf("fork manifest: origin %+v, status %v; want a paused fork of %s", man.Origin, man.Status, id)
	}
	if diff := cmp.Diff(before, treeSums(t, runDir)); diff != "" {
		t.Errorf("the source run changed (-before +after):\n%s", diff)
	}
}

// TestForkNeverOpensItsSourceJournal: belay fork reads its source run and
// never writes to it, and the journal is where that is easiest to break
// without noticing. journal.Open heals a torn final line by rewriting the
// file, so opening the source's journal to read it is a write -- and on a
// source with a clean journal, which is every other fork test's, the heal
// changes nothing and the write cannot be seen. This source's journal ends in
// a record cut off as it was written, which is a run that may still have
// been running: the fork is refused for it, and the journal, like the rest
// of the source, is byte for byte what it was.
func TestForkNeverOpensItsSourceJournal(t *testing.T) {
	ws, id := forkSource(t)
	source, err := state.NewLayout(ws, id)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	runDir := source.RunDir()
	f, err := os.OpenFile(source.JournalPath(), os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: the test's own run
	if err != nil {
		t.Fatalf("open the source journal: %v", err)
	}
	if _, err := f.WriteString(`{"seq":99,"event":"node_st`); err != nil {
		t.Fatalf("tear the source journal: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close the source journal: %v", err)
	}
	before := treeSums(t, runDir)

	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	req := forkRequest{runID: id, node: graph.NodeTest}
	plan, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u), override: req.configure})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	plan.registry, _ = forkScript()
	err = makeFork(cmdWithContext(), u, plan, req)
	said := out.String()
	if err == nil {
		t.Errorf("belay fork forked a run whose journal ends in a record cut off as it was written:\n%s", said)
	}
	if !strings.Contains(said, "cut off") {
		t.Errorf("the refusal does not say the journal was cut off:\n%s", said)
	}
	if got := forksIn(t, ws); len(got) != 0 {
		t.Errorf("a refused fork made forks %v", got)
	}
	if diff := cmp.Diff(before, treeSums(t, runDir)); diff != "" {
		t.Errorf("the source run changed (-before +after):\n%s", diff)
	}
}

// evalPath resolves path's symbolic links, which on macOS turn /var into
// /private/var: dircopy names its copies by their resolved path.
func evalPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	return resolved
}

// treeSums hashes every file under dir, by path.
func treeSums(t *testing.T, dir string) map[string]string {
	t.Helper()
	sums := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // the test's own run
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		sum := sha256.Sum256(data)
		sums[rel] = info.ModTime().String() + " " + hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	return sums
}

// TestRunsPointsAtTheForksOfItsRuns: a fork's run lives in its copy, so it is
// not among this workspace's runs, and somebody who forked a run and came
// back for it would find nothing. belay runs names each fork and the command
// that lists it. The JSON listing is a documented shape, and does not change.
func TestRunsPointsAtTheForksOfItsRuns(t *testing.T) {
	ws, id := forkSource(t)
	var made bytes.Buffer
	u := &ui{out: &made, err: &made}
	req := forkRequest{runID: id, node: graph.NodeTest}
	plan, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u), override: req.configure})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	plan.registry, _ = forkScript()
	if err := makeFork(cmdWithContext(), u, plan, req); err != nil {
		t.Fatalf("makeFork: %v\n%s", err, made.String())
	}
	forks := forksIn(t, ws)
	if len(forks) != 1 {
		t.Fatalf("forks = %v, want one", forks)
	}
	// Named where it is found, under this workspace, not where its manifest
	// says it was made: the two differ once a repository is moved.
	copyDir := filepath.Join(ws, ".belay", "forks", forks[0])

	var out bytes.Buffer
	if err := runRuns(&ui{out: &out, err: &out}, runsOptions{workspace: ws, limit: runsDefaultLimit, now: time.Now()}); err != nil {
		t.Fatalf("runRuns: %v", err)
	}
	t.Logf("verbatim output:\n%s", out.String())
	for _, phrase := range []string{"belay --workspace " + shellQuote(copyDir) + " runs", "test #1 of run " + id} {
		if !strings.Contains(out.String(), phrase) {
			t.Errorf("belay runs never says %q:\n%s", phrase, out.String())
		}
	}

	out.Reset()
	if err := runRuns(&ui{out: &out, err: &out}, runsOptions{workspace: ws, limit: runsDefaultLimit, asJSON: true, now: time.Now()}); err != nil {
		t.Fatalf("runRuns --json: %v", err)
	}
	var listing RunsListing
	if err := json.Unmarshal(out.Bytes(), &listing); err != nil {
		t.Fatalf("belay runs --json is not one JSON document: %v\n%s", err, out.String())
	}
}

// TestAForkResumesFromItsCopy: carrying a fork on is an ordinary resume of
// the new run, pointed at its copy, which is the command the fork's report
// writes out. A dry run of it finds the fork, works in the copy, edits it in
// place as the fork's manifest pinned, and is refused nothing.
func TestAForkResumesFromItsCopy(t *testing.T) {
	ws, id := forkSource(t)
	var made bytes.Buffer
	u := &ui{out: &made, err: &made}
	req := forkRequest{runID: id, node: graph.NodeTest}
	plan, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u), override: req.configure})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	plan.registry, _ = forkScript()
	if err := makeFork(cmdWithContext(), u, plan, req); err != nil {
		t.Fatalf("makeFork: %v\n%s", err, made.String())
	}
	forks := forksIn(t, ws)
	if len(forks) != 1 {
		t.Fatalf("forks = %v, want one", forks)
	}
	copyDir := filepath.Join(ws, ".belay", "forks", forks[0])

	stdout, stderr, err := runCLI(t, "--workspace", copyDir, "--dry-run", "resume", forks[0])
	if err != nil {
		t.Fatalf("dry run of the resume: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	t.Logf("verbatim output:\n%s", stdout)
	for _, phrase := range []string{forks[0] + " — stopped at review", copyDir, "belay is editing this workspace directly"} {
		if !strings.Contains(stdout, phrase) {
			t.Errorf("the resume's dry run never says %q:\n%s", phrase, stdout)
		}
	}
}

// TestAForkResumesFromItsCopyWhateverOutputModeSays: the command a fork's
// report writes out to carry it on is a resume of the fork in its copy, and
// the copy holds belay.yaml as the checkout had it. With output.mode
// "branch" there, resolving the copy refused it -- a copy under .belay is not
// a checkout to branch -- and that refusal outlived the bind to the fork's
// own pin, which is in place, so belay refused the very command it had just
// printed. The pin wins: the resume is not refused, says that belay.yaml asks
// for something this run is not doing, and says where its result goes.
func TestAForkResumesFromItsCopyWhateverOutputModeSays(t *testing.T) {
	ws := gitWorkspace(t)
	writeConfig(t, ws, "version: 1\noutput:\n  mode: in_place\n")
	id := forkSourceIn(t, ws)
	writeConfig(t, ws, "version: 1\noutput:\n  mode: branch\n")

	var made bytes.Buffer
	u := &ui{out: &made, err: &made}
	req := forkRequest{runID: id, node: graph.NodeTest, currentFiles: true}
	plan, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u), override: req.configure})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	plan.registry, _ = forkScript()
	if err := makeFork(cmdWithContext(), u, plan, req); err != nil {
		t.Fatalf("makeFork: %v\n%s", err, made.String())
	}
	forks := forksIn(t, ws)
	if len(forks) != 1 {
		t.Fatalf("forks = %v, want one", forks)
	}
	copyDir := filepath.Join(ws, ".belay", "forks", forks[0])
	printed := "belay --workspace " + shellQuote(evalPath(t, copyDir)) + " resume " + forks[0]
	if !strings.Contains(made.String(), printed) {
		t.Fatalf("the fork's report never says %q, so this test is not running the command belay printed:\n%s",
			printed, made.String())
	}

	stdout, stderr, err := runCLI(t, "--workspace", evalPath(t, copyDir), "--dry-run", "resume", forks[0])
	t.Logf("verbatim output:\n%s%s", stdout, stderr)
	if err != nil {
		t.Fatalf("the resume the fork printed was refused: %v", err)
	}
	for _, phrase := range []string{`output.mode is "branch"`, "carries on in place", "started to edit its workspace in place"} {
		if !strings.Contains(stdout, phrase) {
			t.Errorf("the resume's dry run never says %q", phrase)
		}
	}
}

// gaveUpFixture is a run whose fix loop gave up, and the test doubles that
// stand in for its tests (failing, always) and its quality gate (passing).
type gaveUpFixture struct {
	ws, runID string
	failing   *belaytest.FakeRunner
	gate      *belaytest.FakeLinter
}

// repair is the agent's answer to fix attempt n.
func repair(n int) belay.AgentResponse {
	return belay.AgentResponse{Text: fmt.Sprintf("Fix attempt %d.", n), SessionID: "sess-1", Usage: belay.Usage{USD: 0.03}}
}

// fixLoopThatGaveUp records, over belay's real graph, a run whose fix loop
// used both of graph.give_up's rounds and gave up: plan, code, two repairs,
// and then fix gives up without calling the agent.
func fixLoopThatGaveUp(t *testing.T) gaveUpFixture {
	t.Helper()
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\noutput:\n  mode: in_place\ngraph:\n  approval: false\n  give_up: 2\n")
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	const planText = "## Summary\nAdd an Add helper to main.go.\n\n## Context\nmain.go is the only file.\n\n" +
		"## Steps\n1. Add `Add(a, b int) int` to main.go.\n\n## Tests\nCover it in main_test.go.\n\n" +
		"## Risks\nNone.\n\n## Out of scope\nEverything else.\n"
	f := gaveUpFixture{
		ws: ws, runID: "20260101T000000Z-6a7e0b000000",
		failing: &belaytest.FakeRunner{Responses: []belay.TestReport{{Total: 1, Failed: 1,
			Failures: []belay.TestFailure{{Name: "TestAdd", File: "main_test.go", Line: 3, Message: "Add(2, 2) = 3, want 4"}}}}},
		gate: &belaytest.FakeLinter{Responses: []belay.QualityReport{{Source: "fake", Gate: belay.GatePass,
			Issues: []belay.Issue{}, Summary: "no issues"}}},
	}

	source, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	source.agent = &belaytest.FakeAgent{Responses: []belay.AgentResponse{
		{Text: planText, Usage: belay.Usage{USD: 0.02}},
		{Text: "Added it.\n\n```changed-files\nmain.go\n```\n", SessionID: "sess-1", Usage: belay.Usage{USD: 0.05}},
		repair(1), repair(2),
	}}
	source.runner, source.linter = f.failing, f.gate
	store, err := createRun(t.Context(), source, runSpec{Workspace: ws, RunID: f.runID, Goal: "add Add"})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}
	gaveUp, err := dispatchOnce(t.Context(), source, store, false)
	if err != nil || gaveUp.outcome.Status != state.RunStatusFailed || gaveUp.outcome.Node != graph.NodeFix {
		t.Fatalf("the source = %v at %q, %v (%q); want this test's premise, a run whose fix loop gave up",
			gaveUp.outcome.Status, gaveUp.outcome.Node, err, gaveUp.outcome.Note)
	}
	return f
}

// TestForkingAFixLoopThatGaveUpCallsTheStrongerModel is the README's own fork
// example, end to end over belay's real graph: a run's fix loop gives up, and
// the person types `belay fork <run-id> fix --model opus`. That forks the
// last fix visit -- the one that gave up -- whose blackboard has every round
// used, so the fix node gave up again without calling anything and --model
// changed nothing. The fork gets one more round instead, says so before it
// spends anything, and calls the agent exactly once, with opus.
func TestForkingAFixLoopThatGaveUpCallsTheStrongerModel(t *testing.T) {
	f := fixLoopThatGaveUp(t)

	// The fork, as the README types it.
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	req := forkRequest{runID: f.runID, node: graph.NodeFix, model: "opus"}
	plan, err := resolve(resolveRequest{workspace: f.ws, logger: newLogger(u), override: req.configure})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	stronger := &belaytest.FakeAgent{Responses: []belay.AgentResponse{repair(3)}}
	plan.agent, plan.runner, plan.linter = stronger, f.failing, f.gate
	err = makeFork(cmdWithContext(), u, plan, req)
	t.Logf("verbatim output:\n%s", out.String())
	if err != nil {
		t.Fatalf("makeFork: %v", err)
	}
	calls := stronger.Calls()
	if len(calls) != 1 {
		t.Fatalf("the fork called the agent %d times, want once, for the fix it re-ran", len(calls))
	}
	if calls[0].Model != "opus" {
		t.Errorf("the fork's fix was sent model %q, want opus", calls[0].Model)
	}
	if !strings.Contains(out.String(), "graph.give_up") {
		t.Errorf("the fork never says it gave the fix loop another round")
	}
}

// cancelsItsOwnCall is an agent whose call is cut short by a Ctrl-C: it
// cancels the context it was given, as stopOnSignal does on SIGINT, and
// reports what a killed agent CLI reports, nothing.
type cancelsItsOwnCall struct{ cancel context.CancelFunc }

func (cancelsItsOwnCall) Name() string { return "fake-agent" }

func (a cancelsItsOwnCall) Invoke(ctx context.Context, _ belay.AgentRequest) (belay.AgentResponse, error) {
	a.cancel()
	<-ctx.Done()
	return belay.AgentResponse{}, ctx.Err()
}

// TestAResumedForkKeepsTheRoundItWasGiven: a fork of a fix loop that gave up
// is given one more round, graph.give_up one above the rounds used. That is
// a decision the fork made, not something its belay.yaml says: the copy's
// belay.yaml is the source's, with graph.give_up as the source had it. So a
// fork stopped part-way through that round -- a Ctrl-C, a crash -- and
// resumed from its copy must still have it. Read from belay.yaml again, the
// round was lost: the re-run fix found every round used and gave up without
// calling the agent, which is exactly what the fork was made to get past.
func TestAResumedForkKeepsTheRoundItWasGiven(t *testing.T) {
	f := fixLoopThatGaveUp(t)

	// The fork, stopped by a Ctrl-C during its one fix call.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := cmdWithContext()
	cmd.SetContext(ctx)
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	req := forkRequest{runID: f.runID, node: graph.NodeFix}
	plan, err := resolve(resolveRequest{workspace: f.ws, logger: newLogger(u), override: req.configure})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	plan.agent, plan.runner, plan.linter = cancelsItsOwnCall{cancel: cancel}, f.failing, f.gate
	err = makeFork(cmd, u, plan, req)
	t.Logf("the fork, interrupted: error %v\n%s", err, out.String())
	forks := forksIn(t, f.ws)
	if len(forks) != 1 {
		t.Fatalf("forks = %v, want one", forks)
	}
	copyDir := filepath.Join(f.ws, ".belay", "forks", forks[0])
	layout, err := state.NewLayout(copyDir, forks[0])
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if man, err := state.LoadManifest(layout.ManifestPath()); err != nil || man.Status != state.RunStatusPaused {
		t.Fatalf("the interrupted fork is %v (%v), want it paused, so a resume can carry it on", man.Status, err)
	}

	// The resume, as `belay --workspace <copy> resume <fork-id>` does it:
	// resolved from the copy's belay.yaml, which still says give_up: 2.
	resumed, err := resolve(resolveRequest{workspace: copyDir, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve the copy: %v", err)
	}
	store, err := resumed.openRun(t.Context(), layout)
	if err != nil {
		t.Fatalf("openRun: %v", err)
	}
	if len(resumed.blockers) > 0 {
		t.Fatalf("the fork's resume was refused: %+v", resumed.blockers)
	}
	if got := resumed.config.Graph.GiveUp; got != 3 {
		t.Errorf("the resumed fork is described with graph.give_up %d, want the 3 it was given", got)
	}
	agent := &belaytest.FakeAgent{Responses: []belay.AgentResponse{repair(3), repair(4)}}
	resumed.agent, resumed.runner, resumed.linter = agent, f.failing, f.gate
	if _, err := dispatchOnce(t.Context(), resumed, store, true, pauseOnCancel); err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}
	if len(agent.Calls()) == 0 {
		t.Error("the resumed fork's fix gave up without calling the agent: the round the fork was given was lost")
	}
}

// TestCtrlCDuringAForksCopyUnwindsIt: belay fork says "press Ctrl-C once;
// belay saves its state and exits" before it copies the tree, and a Ctrl-C
// during the copy must do that. With no handler installed until the
// dispatch, it killed belay part-way through the copy, leaving a half-made
// copy that `belay runs` lists as unreadable.
//
// The Ctrl-C is a real SIGINT to this process, sent from inside the copy,
// once the copied tree exists and before the fork is written into it.
func TestCtrlCDuringAForksCopyUnwindsIt(t *testing.T) {
	ws, id := forkSource(t)
	runDir := filepath.Join(ws, ".belay", "runs", id)
	before := treeSums(t, runDir)

	pressed := false
	survey := forkSurvey
	t.Cleanup(func() { forkSurvey = survey })
	forkSurvey = func(ctx context.Context, dir string) (string, error) {
		if pressed || len(forksIn(t, ws)) == 0 {
			return survey(ctx, dir)
		}
		pressed = true
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatalf("press Ctrl-C: %v", err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
			return "", errors.New("Ctrl-C did not cancel the copy")
		}
	}

	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	req := forkRequest{runID: id, node: graph.NodeTest}
	plan, err := resolve(resolveRequest{workspace: ws, logger: newLogger(u), override: req.configure})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	plan.registry, _ = forkScript()

	err = makeFork(cmdWithContext(), u, plan, req)
	t.Logf("verbatim output:\n%s", out.String())

	if !pressed {
		t.Fatal("the copy never reached the point the test presses Ctrl-C at")
	}
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != graph.ExitAborted {
		t.Errorf("makeFork = %v, want the exit code a Ctrl-C always has, %d", err, graph.ExitAborted)
	}
	if !strings.Contains(out.String(), "because you asked it to") {
		t.Errorf("the fork never says it stopped because it was asked to:\n%s", out.String())
	}
	if forks := forksIn(t, ws); len(forks) != 0 {
		t.Errorf("a Ctrl-C during the copy left %v under .belay/forks", forks)
	}
	if diff := cmp.Diff(before, treeSums(t, runDir)); diff != "" {
		t.Errorf("the source run changed (-before +after):\n%s", diff)
	}
}
