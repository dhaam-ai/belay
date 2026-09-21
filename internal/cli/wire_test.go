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

	"github.com/dhaam-ai/belay/internal/agent/claude"
	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/isolate/dircopy"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
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
	// gosec's taint analysis cannot see that dir is always a t.TempDir() and
	// that the name is a constant, so it reads this as attacker-controlled.
	//nolint:gosec // G703: dir comes from t.TempDir(); config.ProjectFile is a constant.
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
	if plan.registry == nil || plan.registry.Len() != len(plan.route) {
		t.Errorf("registry = %v, want the %d nodes of the route %v", plan.registry, len(plan.route), plan.route)
	}
	if len(plan.projects) != 1 || plan.projects[0].Kind.String() != "go" {
		t.Errorf("projects = %+v, want one Go project", plan.projects)
	}
}

// Fanout used to be refused because the join node did not exist. Now that it
// does, an enabled fanout must produce a graph carrying BOTH halves -- a
// registry with fanout and no join would create N candidate workspaces, spend
// N times the budget, and then die routing to an unregistered node.
func TestResolveBuildsTheFanoutGraph(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nfanout:\n  enabled: true\n  candidates: 3\n")

	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.registry == nil {
		t.Fatalf("no registry built; blockers: %s", blockerText(plan))
	}
	for _, name := range []string{graph.NodeFanout, graph.NodeJoin} {
		if _, err := plan.registry.Get(name); err != nil {
			t.Errorf("Get(%q): %v", name, err)
		}
	}
	if got := blockerText(plan); strings.Contains(got, "fanout") {
		t.Errorf("fanout is supported now; nothing should block on it:\n%s", got)
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

// TestResolveBlocksOnUnwiredReviewMode pins the refusal of review.mode "ai"
// to what docs/config.md says about it: the reviewer asks SonarQube through
// the Sonar MCP server, and belay has not wired it in. The refusal once
// called it "the agent-driven reviewer" and "not built yet", which is neither
// what the mode is nor why belay refuses it: internal/nodes/aireview exists
// and is tested, and nothing in belay run constructs it.
func TestResolveBlocksOnUnwiredReviewMode(t *testing.T) {
	ws := goWorkspace(t)
	// The url is there because review.mode "ai" with the default mcp mode,
	// "cloud", requires one to load at all; this test is about the refusal
	// that follows loading.
	writeConfig(t, ws, "version: 1\nreview:\n  mode: \"ai\"\n  ai:\n    mcp:\n      url: \"https://mcp.example.test\"\n")

	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := blockerText(plan)
	for _, want := range []string{"SonarQube", "Sonar MCP server", "not wired in yet", `set review.mode to "lint" or "sonar"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal of review.mode ai does not say %q; got:\n%s", want, got)
		}
	}
	for _, wrong := range []string{"agent-driven", "not built"} {
		if strings.Contains(got, wrong) {
			t.Errorf("the refusal of review.mode ai says %q, which is not why belay refuses it; got:\n%s", wrong, got)
		}
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

// TestRedactorCoversOAuthToken covers the subscription credential: a run
// signed in with CLAUDE_CODE_OAUTH_TOKEN must journal that value no more
// readily than an API key.
func TestRedactorCoversOAuthToken(t *testing.T) {
	secret := notARealKey("belay-test-", "oauth-credential-longenoughtomatter")
	t.Setenv(claudeOAuthTokenEnv, secret)

	plan, err := resolve(resolveRequest{workspace: goWorkspace(t), logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	redact := plan.options(state.NewStore(state.Layout{})).Redact
	want := "claude failed: [REDACTED:" + claudeOAuthTokenEnv + "]"
	if got := redact("claude failed: " + secret); got != want {
		t.Errorf("Redact() = %q, want %q", got, want)
	}
}

// TestSignInWarning pins when a plan warns that no credential is set. Either
// credential is enough to stay quiet; with neither, the warning names both so
// a subscription user learns the variable exists.
func TestSignInWarning(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		oauth    string
		wantWarn bool
	}{
		{name: "neither set", wantWarn: true},
		{name: "API key only", key: "belay-test-api-credential"},
		{name: "OAuth token only", oauth: "belay-test-oauth-credential"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(anthropicKeyEnv, tt.key)
			t.Setenv(claudeOAuthTokenEnv, tt.oauth)

			plan, err := resolve(resolveRequest{workspace: goWorkspace(t), logger: quietLogger()})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			var warning string
			for _, w := range plan.warnings {
				if strings.Contains(w, "signed in") {
					warning = w
				}
			}
			if !tt.wantWarn {
				if warning != "" {
					t.Errorf("unexpected warning: %q", warning)
				}
				return
			}
			for _, name := range []string{anthropicKeyEnv, claudeOAuthTokenEnv} {
				if !strings.Contains(warning, name) {
					t.Errorf("warning = %q, want it to name %s", warning, name)
				}
			}
		})
	}
}

// TestAgentWarningsNameOnlyTheBackendInUse pins agentWarnings on its own
// terms, beneath TestSignInWarning: a warning about how one backend signs in
// is given only for that backend, so a run on another is not told to sign in
// to a command line it will never call.
func TestAgentWarningsNameOnlyTheBackendInUse(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		key     string
		want    []string
	}{
		{name: "claude, signed out", backend: claude.BackendName, want: []string{anthropicKeyEnv, claudeOAuthTokenEnv}},
		{name: "claude, signed in", backend: claude.BackendName, key: "belay-test-api-credential"},
		{name: "another backend, signed out", backend: "some-other-cli"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(anthropicKeyEnv, tt.key)
			t.Setenv(claudeOAuthTokenEnv, "")
			cfg := config.Default()
			cfg.Agent.Backend = tt.backend

			got := agentWarnings(cfg)
			if len(tt.want) == 0 {
				if len(got) != 0 {
					t.Errorf("agentWarnings() = %q, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("agentWarnings() = %q, want one warning", got)
			}
			for _, name := range tt.want {
				if !strings.Contains(got[0], name) {
					t.Errorf("warning = %q, want it to name %s", got[0], name)
				}
			}
		})
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
	if err := state.SaveState(layout.StatePath(), state.NewState(goal)); err != nil {
		t.Fatalf("save state: %v", err)
	}
	now := time.Now().UTC()
	if err := state.SaveManifest(layout.ManifestPath(),
		state.NewManifest(now, runID, plan.workspace, goal, plan.config)); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	return state.NewStore(layout)
}

// TestEveryRunRecordsAWorkspaceDigest: a fork asks whether the workspace has
// changed since the visit it re-runs, and can only answer for a run whose
// snapshots carry a digest. So every run belay dispatches fingerprints its
// workspace, with the survey a copy of the workspace would agree with —
// belay's own writes under .belay do not change it.
func TestEveryRunRecordsAWorkspaceDigest(t *testing.T) {
	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	reached := false
	registry := graph.NewRegistry()
	registry.MustRegister(
		planWriter{body: "# Plan\n\nwrite a test\n"},
		routesTo{name: graph.NodeApprove, next: graph.NodeCode},
		endsTheRun{reached: &reached},
	)
	plan.registry = registry
	store := seedRunDir(t, plan, "add tests")

	dispatcher, err := graph.NewDispatcher(plan.options(store))
	if err != nil {
		t.Fatalf("graph.NewDispatcher: %v", err)
	}
	if _, err := dispatcher.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	read, err := journal.ReadFile(store.Layout().JournalPath())
	if err != nil {
		t.Fatalf("journal.ReadFile: %v", err)
	}
	visits, err := journal.Visits(read.Records)
	if err != nil || len(visits) == 0 {
		t.Fatalf("journal.Visits() = %v, %v", visits, err)
	}
	want, err := dircopy.SurveyTree(context.Background(), ws, dircopy.DefaultSkip())
	if err != nil {
		t.Fatalf("SurveyTree: %v", err)
	}
	for _, v := range visits {
		path, err := store.Layout().SnapshotPath(v.StartSeq, v.Node)
		if err != nil {
			t.Fatalf("SnapshotPath: %v", err)
		}
		snap, err := state.LoadSnapshot(path)
		if err != nil {
			t.Fatalf("no snapshot for %s: %v", v.Node, err)
		}
		if snap.WorkspaceDigest != want.Digest {
			t.Errorf("snapshot of %s records workspace digest %q, want %q", v.Node, snap.WorkspaceDigest, want.Digest)
		}
	}
}
