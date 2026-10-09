package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
