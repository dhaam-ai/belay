package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/nodes"
	"github.com/belay-dev/belay/internal/state"
)

// quietLogger keeps a test's output to what the test itself prints.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// goWorkspace writes the smallest tree belay recognises as a testable,
// lintable Go project: a go.mod and nothing else. It is deliberately not a
// git repository — belay never shells out to git, and a plan must resolve
// without one.
func goWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	const gomod = "module example.com/scratch\n\ngo 1.26.3\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	return dir
}

// writeConfig drops a belay.yaml into dir.
func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, config.ProjectFile), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", config.ProjectFile, err)
	}
}

func TestResolveWorkspace(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	missing := filepath.Join(dir, "nowhere")

	tests := []struct {
		name     string
		in       string
		wantAbs  string
		wantErr  bool
		wantSaid []string
	}{
		{name: "directory resolves", in: dir, wantAbs: dir},
		{name: "empty means here", in: "", wantAbs: mustAbs(t, ".")},
		{
			name: "missing directory names the path",
			in:   missing, wantErr: true,
			wantSaid: []string{missing, "no directory"},
		},
		{
			name: "a file is refused in plain words",
			in:   file, wantErr: true,
			wantSaid: []string{file, "is a file, not a directory"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveWorkspace(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveWorkspace(%q) = %q, want an error", tc.in, got)
				}
				for _, want := range tc.wantSaid {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWorkspace(%q): %v", tc.in, err)
			}
			if got != tc.wantAbs {
				t.Errorf("resolveWorkspace(%q) = %q, want %q", tc.in, got, tc.wantAbs)
			}
		})
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", p, err)
	}
	return abs
}

func TestResolveCreatesNothing(t *testing.T) {
	ws := goWorkspace(t)

	before := listTree(t, ws)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.blockers) != 0 {
		t.Fatalf("a plain Go module should not block a run, got %+v", plan.blockers)
	}
	after := listTree(t, ws)

	if !slices.Equal(before, after) {
		t.Errorf("resolve changed the workspace:\n before %v\n after  %v", before, after)
	}
	if _, err := os.Stat(filepath.Join(ws, ".belay")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("resolve created a .belay directory; it must create nothing")
	}
}

// listTree returns every path under root, so a test can prove nothing
// appeared or disappeared.
func listTree(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		found = append(found, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	slices.Sort(found)
	return found
}

func TestResolvePicksAdapters(t *testing.T) {
	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := plan.runnerName(); got != "go-test" {
		t.Errorf("test runner = %q, want the Go runner", got)
	}
	if got := plan.gateName(); got != "golangci-lint" {
		t.Errorf("quality gate = %q, want golangci-lint", got)
	}
	if plan.agent == nil || plan.agent.Name() != "claude-code" {
		t.Errorf("agent = %v, want the claude-code backend", plan.agent)
	}
	if plan.recorder != nil {
		t.Error("live mode must not arm the cassette recorder")
	}
	if plan.registry == nil || plan.registry.Len() != len(defaultRoute) {
		t.Errorf("registry = %v, want the %d nodes of the default route", plan.registry, len(defaultRoute))
	}
	if len(plan.projects) != 1 || plan.projects[0].Kind.String() != "go" {
		t.Errorf("projects = %+v, want one Go project", plan.projects)
	}
}

func TestResolveBlocksOnFanout(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nfanout:\n  enabled: true\n  candidates: 3\n")

	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.registry != nil {
		t.Error("a graph belay cannot finish must not be handed to the dispatcher")
	}

	got := blockerText(plan)
	// The sentence has to explain the cost of the thing belay is refusing,
	// not restate a Go error the reader cannot act on.
	for _, want := range []string{"fanout.enabled", "copy your repository 3 times", "set fanout.enabled to false"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal does not say %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ErrFanoutUnavailable") || strings.Contains(got, "nodes:") {
		t.Errorf("refusal leaks the Go error to the reader:\n%s", got)
	}
}

func TestResolveBlocksWhenNothingToTest(t *testing.T) {
	plan, err := resolve(resolveRequest{workspace: t.TempDir(), logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	got := blockerText(plan)
	for _, want := range []string{"no Go, Node or Python project", "test.custom_cmd", "review.mode"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal does not say %q; got:\n%s", want, got)
		}
	}
}

func TestResolveBlocksOnUnbuiltReviewMode(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nreview:\n  mode: \"ai\"\n")

	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(blockerText(plan), "not built yet") {
		t.Errorf("review.mode ai should be refused in plain words; got:\n%s", blockerText(plan))
	}
}

func TestResolveBlocksOnMissingCassette(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nagent:\n  mode: \"replay\"\n")

	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.agent != nil {
		t.Error("replay with no cassette must not produce a backend")
	}
	got := blockerText(plan)
	if !strings.Contains(got, filepath.Join(ws, ".belay", defaultCassetteName)) {
		t.Errorf("refusal must name the cassette it looked for; got:\n%s", got)
	}
}

func TestResolveRejectsUnreadableConfig(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nnot_a_real_field: 3\n")

	if _, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()}); err == nil {
		t.Fatal("a belay.yaml belay cannot read must stop planning outright")
	} else if !strings.Contains(err.Error(), "not_a_real_field") {
		t.Errorf("error should name the offending key; got %q", err)
	}
}

// notARealKey builds a value shaped like a credential so the redactor's
// patterns match it, spelled in two halves so no secret scanner — belay's own
// included — mistakes the test fixture for a live key.
func notARealKey(prefix, rest string) string { return prefix + rest }

func blockerText(p *runPlan) string {
	var b strings.Builder
	for _, blk := range p.blockers {
		b.WriteString(blk.problem)
		b.WriteString("\n")
		b.WriteString(blk.fix)
		b.WriteString("\n")
	}
	return b.String()
}

// TestDefaultRouteMatchesRegistry keeps the route belay shows a person and
// the graph belay actually walks from drifting apart.
func TestDefaultRouteMatchesRegistry(t *testing.T) {
	registry, err := nodes.Default(config.Default(), t.TempDir())
	if err != nil {
		t.Fatalf("nodes.Default: %v", err)
	}
	if registry.Len() != len(defaultRoute) {
		t.Fatalf("defaultRoute has %d nodes, the registry has %d", len(defaultRoute), registry.Len())
	}
	for _, name := range defaultRoute {
		if _, err := registry.Get(name); err != nil {
			t.Errorf("defaultRoute names %q, which nodes.Default does not register", name)
		}
	}
	if defaultRoute[0] != journal.FirstNode {
		t.Errorf("the route starts at %q but a run starts at %q", defaultRoute[0], journal.FirstNode)
	}
}

// secretNode is a node that fails the way a real adapter fails: with an error
// whose text quotes the command line it ran, credential and all.
type secretNode struct {
	name   string
	secret string
}

func (n secretNode) Name() string { return n.name }

func (n secretNode) Run(context.Context, *graph.RunContext) (graph.Result, error) {
	return graph.Result{Status: journal.StatusFailed},
		errors.New("claude --api-key " + n.secret + " exited with status 1")
}

// TestRedactorReachesTheDispatchersNotes is the security test for
// graph.Options.Redact. The dispatcher composes a journal note out of an
// adapter's own error string; without a redactor wired through, a credential
// in that string lands in a file people commit and paste into issues.
func TestRedactorReachesTheDispatchersNotes(t *testing.T) {
	secret := notARealKey("sk-ant-", "api03-notarealkeybutlongenoughtomatter")
	t.Setenv(anthropicKeyEnv, secret)

	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Swap in a registry whose first node fails with the secret in its
	// error. Everything else — the store, the config, and crucially
	// Options.Redact — is exactly what a real run would use.
	registry := graph.NewRegistry()
	registry.MustRegister(secretNode{name: journal.FirstNode, secret: secret})
	plan.registry = registry

	store := seedRunDir(t, plan, "prove the redactor is wired")
	dispatcher, err := graph.NewDispatcher(plan.options(store))
	if err != nil {
		t.Fatalf("graph.NewDispatcher: %v", err)
	}

	outcome, err := dispatcher.Run(context.Background())
	if !errors.Is(err, graph.ErrNodeFailed) {
		t.Fatalf("Run() error = %v, want ErrNodeFailed", err)
	}
	if strings.Contains(outcome.Note, secret) {
		t.Fatalf("the credential survived into the run's note: %q", outcome.Note)
	}
	if !strings.Contains(outcome.Note, "[REDACTED:"+anthropicKeyEnv+"]") {
		t.Errorf("note = %q, want the credential replaced by its label", outcome.Note)
	}

	// The note is journalled, not only returned; check the file people
	// actually read.
	body, err := os.ReadFile(store.Layout().JournalPath())
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if strings.Contains(string(body), secret) {
		t.Error("the credential survived into journal.ndjson")
	}
}

// TestRedactorIsWiredNotNil guards the wiring itself: graph.Options.Redact
// defaults to a no-op, so a plan that forgot to set it would fail silently.
func TestRedactorIsWiredNotNil(t *testing.T) {
	secret := notARealKey("squ_", "thisisnotarealsonartokenbutitislong")
	t.Setenv(sonarTokenEnv, secret)

	plan, err := resolve(resolveRequest{workspace: goWorkspace(t), logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	opts := plan.options(state.NewStore(state.Layout{}))
	if opts.Redact == nil {
		t.Fatal("graph.Options.Redact is nil; a run would journal credentials verbatim")
	}
	if got := opts.Redact("token=" + secret); strings.Contains(got, secret) {
		t.Errorf("Redact(%q) = %q, want the value replaced", secret, got)
	}
}

// seedRunDir creates the run directory a dispatcher needs. It is the same
// sequence run.go performs, kept here so wire tests do not depend on the
// command layer.
func seedRunDir(t *testing.T, plan *runPlan, goal string) *state.Store {
	t.Helper()
	runID, err := state.NewRunID()
	if err != nil {
		t.Fatalf("state.NewRunID: %v", err)
	}
	layout, err := state.NewLayout(plan.workspace, runID)
	if err != nil {
		t.Fatalf("state.NewLayout: %v", err)
	}
	if err := os.MkdirAll(layout.ArtifactsDir(), 0o750); err != nil {
		t.Fatalf("create run directory: %v", err)
	}
	now := time.Now().UTC()
	if err := state.SaveManifest(layout.ManifestPath(),
		state.NewManifest(now, runID, plan.workspace, goal, plan.config)); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	if err := state.SaveState(layout.StatePath(), state.NewState(goal)); err != nil {
		t.Fatalf("save state: %v", err)
	}
	return state.NewStore(layout)
}
