package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/promote"
	"github.com/dhaam-ai/belay/internal/state"
)

// gitPrompt is what a skipped test tells its reader to ask a coding agent,
// so the test runs next time instead of skipping again. internal/git says
// why these tests use the real binary rather than faking it.
const gitPrompt = "Install git so it is on PATH and `git --version` prints 2.38 or newer, " +
	"then run `go test ./internal/cli/... -v` and confirm the tests pass instead of skipping."

// requireGit skips the calling test unless a usable git is on PATH.
func requireGit(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := exec.CommandContext(ctx, "git", "--version").Output(); err != nil {
		t.Skipf("skipping: needs a working git on PATH (%v). "+
			"Install it with `xcode-select --install`, `brew install git` or from "+
			"https://git-scm.com/downloads, or ask your coding agent: %q", err, gitPrompt)
	}
}

// gitIn runs one raw git command in dir. It never goes through the code
// under test: a fixture built by the thing being tested proves nothing.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // every argument comes from a test literal
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Belay Test", "GIT_AUTHOR_EMAIL=belay-test@example.invalid",
		"GIT_COMMITTER_NAME=Belay Test", "GIT_COMMITTER_EMAIL=belay-test@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitWorkspace returns a go.mod-only workspace that is also a git repository
// with one commit on main.
func gitWorkspace(t *testing.T) string {
	t.Helper()
	requireGit(t)
	dir := goWorkspace(t)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	gitIn(t, resolved, "init", "-q", "-b", "main")
	gitIn(t, resolved, "add", "-A")
	gitIn(t, resolved, "commit", "-qm", "first")
	return resolved
}

// planFor resolves a plan for dir with mode written into belay.yaml.
func planFor(t *testing.T, dir string, cfg func(*config.Config)) *runPlan {
	t.Helper()
	p := &runPlan{workspace: dir, config: config.Default(), logger: quietLogger()}
	if cfg != nil {
		cfg(&p.config)
	}
	return p
}

// resolveOutput answers the one question a run must settle before it creates
// anything: does this run work on a branch of its own, or edit the user's
// checkout the way belay always did?
//
// Every row is a workspace crossed with an output.mode, because the answer is
// a function of both. auto is the one that has to be right without being
// asked: it picks branch only when a fresh checkout of HEAD faithfully
// reproduces the target, and it must say which it chose and why, since the
// difference is whether the user's files change under them.
func TestResolveOutput(t *testing.T) {
	requireGit(t)

	plain := goWorkspace(t)

	commitless := goWorkspace(t)
	gitIn(t, commitless, "init", "-q", "-b", "main")

	withSubmodules := gitWorkspace(t)
	if err := os.WriteFile(filepath.Join(withSubmodules, ".gitmodules"), []byte("[submodule \"x\"]\n"), 0o600); err != nil {
		t.Fatalf("write .gitmodules: %v", err)
	}
	gitIn(t, withSubmodules, "add", "-A")
	gitIn(t, withSubmodules, "commit", "-qm", "add a submodule")

	tests := []struct {
		name       string
		workspace  string
		cfg        func(*config.Config)
		wantMode   state.OutputMode
		wantReason []string
		wantBlock  []string
		wantWarn   []string
	}{
		{
			name: "auto picks a branch in a git repository with a commit",
			// The whole point of the slice: a repository that can take a
			// branch gets one, without anyone asking for it.
			workspace: gitWorkspace(t), wantMode: state.OutputBranch,
			wantReason: []string{"git"},
		},
		{
			name:      "auto falls back outside a git repository, and says so",
			workspace: plain, wantMode: state.OutputInPlace,
			wantReason: []string{"not", "git"},
		},
		{
			name: "auto falls back in a repository with no commits",
			// One commit fixes this, so the reason must not read as "never".
			workspace: commitless, wantMode: state.OutputInPlace,
			wantReason: []string{"commit"},
		},
		{
			name:      "auto falls back when the repository has submodules",
			workspace: withSubmodules, wantMode: state.OutputInPlace,
			wantReason: []string{"submodule"},
		},
		{
			name:      "in_place is honoured in a git repository",
			workspace: gitWorkspace(t),
			cfg:       func(c *config.Config) { c.Output.Mode = config.OutputInPlace },
			wantMode:  state.OutputInPlace, wantReason: []string{"output.mode"},
		},
		{
			name:      "branch is honoured where auto would have chosen it",
			workspace: gitWorkspace(t),
			cfg:       func(c *config.Config) { c.Output.Mode = config.OutputBranch },
			wantMode:  state.OutputBranch, wantReason: []string{"output.mode"},
		},
		{
			name: "branch is refused where it cannot work, rather than silently ignored",
			// Falling back here would edit the user's checkout after they
			// asked belay never to.
			workspace: plain,
			cfg:       func(c *config.Config) { c.Output.Mode = config.OutputBranch },
			wantBlock: []string{"output.mode", "branch", "git"},
		},
		// Whether the isolator suits the output is judged with the run's
		// fanout, after both are settled, by checkFanoutIsolator:
		// TestFanoutRefusesAnIsolatorItsOutputCannotUse.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := planFor(t, tt.workspace, tt.cfg)
			p.resolveOutput(t.Context())

			if len(tt.wantBlock) > 0 {
				if len(p.blockers) == 0 {
					t.Fatalf("resolveOutput blocked nothing, want a blocker mentioning %v", tt.wantBlock)
				}
				said := p.blockers[0].problem + " " + p.blockers[0].fix
				for _, want := range tt.wantBlock {
					if !strings.Contains(said, want) {
						t.Errorf("blocker %q does not mention %q", said, want)
					}
				}
				return
			}
			if len(p.blockers) > 0 {
				t.Fatalf("resolveOutput blocked the run: %v", p.blockers)
			}
			if got := p.output.Mode; got != tt.wantMode {
				t.Errorf("mode = %q, want %q (reason: %s)", got, tt.wantMode, p.outputReason)
			}
			for _, want := range tt.wantReason {
				if !strings.Contains(p.outputReason, want) {
					t.Errorf("reason %q does not mention %q", p.outputReason, want)
				}
			}
			if tt.wantMode == state.OutputBranch {
				if p.output.Repo == "" || p.output.Base == "" || p.output.BaseRef == "" {
					t.Errorf("branch output is incomplete: %+v", p.output)
				}
				if p.repo == nil {
					t.Error("branch output resolved no repository for the run to commit to")
				}
			}
		})
	}
}

// A dirty checkout is not a refusal, but it must not be a silence either: a
// branch run starts from the commit, so uncommitted work is simply not in it,
// and somebody expecting otherwise finds out at the end.
func TestResolveOutputWarnsAboutUncommittedWork(t *testing.T) {
	dir := gitWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "scratch.go"), []byte("package scratch\n"), 0o600); err != nil {
		t.Fatalf("write scratch.go: %v", err)
	}

	p := planFor(t, dir, nil)
	p.resolveOutput(t.Context())

	if p.output.Mode != state.OutputBranch {
		t.Fatalf("mode = %q, want %q: a dirty tree does not stop a branch run", p.output.Mode, state.OutputBranch)
	}
	joined := strings.Join(p.warnings, "\n")
	for _, want := range []string{"uncommitted", "not part of"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q do not mention %q", joined, want)
		}
	}
}

// belay's own directory is not the user's uncommitted work: a nested run
// worktree makes `git status` report .belay/ as untracked, and warning about
// it every run would train people to ignore the warning that matters.
func TestResolveOutputDoesNotCountBelaysOwnDirectory(t *testing.T) {
	dir := gitWorkspace(t)
	if err := os.MkdirAll(filepath.Join(dir, ".belay", "runs"), 0o750); err != nil {
		t.Fatalf("mkdir .belay/runs: %v", err)
	}

	p := planFor(t, dir, nil)
	p.resolveOutput(t.Context())

	if joined := strings.Join(p.warnings, "\n"); strings.Contains(joined, "uncommitted") {
		t.Errorf("warned about belay's own directory: %q", joined)
	}
}

// The graph a run walks follows the output the run was pinned to, not the
// belay.yaml in front of belay right now.
//
// This is the first pinned Option (internal/nodes says why one is pinned at
// all), so it is also the first chance for the registry and the route to
// disagree with each other, or with the manifest. A run whose registry lacks
// promote but whose route names it shows a person a step the dispatcher
// cannot resolve; the reverse ends the run on an unknown node after it has
// spent its budget.
func TestRegistryAndRouteFollowThePinnedOutput(t *testing.T) {
	tests := []struct {
		name        string
		output      state.Output
		wantPromote bool
	}{
		{"in place", state.Output{Mode: state.OutputInPlace}, false},
		{"no output block at all", state.Output{}, false},
		{"branch", state.Output{Mode: state.OutputBranch}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := planFor(t, t.TempDir(), nil)
			p.output = tt.output
			p.resolveRegistry()

			if len(p.blockers) > 0 {
				t.Fatalf("resolveRegistry blocked: %v", p.blockers)
			}
			registered := slices.Contains(p.registry.Names(), graph.NodePromote)
			if registered != tt.wantPromote {
				t.Errorf("registry has promote = %v, want %v", registered, tt.wantPromote)
			}
			if onRoute := slices.Contains(p.route, graph.NodePromote); onRoute != tt.wantPromote {
				t.Errorf("route %v has promote = %v, want %v", p.route, onRoute, tt.wantPromote)
			}
			if tt.wantPromote && p.route[len(p.route)-1] != graph.NodePromote {
				t.Errorf("route %v does not end at promote", p.route)
			}
		})
	}
}

// Creating a branch run creates the branch and the worktree it works in, and
// pins both in the manifest, before the manifest exists to be found.
//
// The worktree is what makes "belay never edits your checkout" true: the
// run's Layout is redirected at it, so every node writes there. The pin is
// what makes a resume days later find the same branch. Both go through
// Manifest.SetOutput, never a field assignment, so the capability a branch
// run depends on is recorded with the field (ADR-0011).
func TestPrepareWorkspaceCreatesTheRunBranchAndItsWorktree(t *testing.T) {
	dir := gitWorkspace(t)
	p := planFor(t, dir, nil)
	p.resolveOutput(t.Context())
	if !p.output.Branched() {
		t.Fatalf("resolveOutput did not choose a branch in a git repository: %s", p.outputReason)
	}

	const runID = "20260101T000000Z-c0ffee000000"
	store, err := createRun(t.Context(), p, runSpec{Workspace: dir, RunID: runID, Goal: "add a --json flag"})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}

	manifest, err := state.LoadManifest(store.Layout().ManifestPath())
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	branch := "belay/" + runID
	if manifest.Output.Branch != branch {
		t.Errorf("manifest branch = %q, want %q", manifest.Output.Branch, branch)
	}
	if manifest.Output.Base == "" || manifest.Output.WorkTree == "" {
		t.Errorf("manifest output is incomplete: %+v", manifest.Output)
	}
	if !slices.Contains(manifest.Requires, state.CapabilityOutputBranch) {
		t.Errorf("manifest requires %v, want it to include %q; build output through SetOutput",
			manifest.Requires, state.CapabilityOutputBranch)
	}

	// The branch exists, at the commit the manifest recorded as the base.
	if got := gitIn(t, dir, "rev-parse", branch); got != manifest.Output.Base {
		t.Errorf("%s is at %s, want the recorded base %s", branch, got, manifest.Output.Base)
	}
	// The worktree exists, is registered, and is where the run's nodes work.
	if got := gitIn(t, manifest.Output.WorkTree, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Errorf("the run worktree is on %q, want %q", got, branch)
	}
	if got := store.Layout().WorkspaceDir(); got != manifest.Output.WorkTree {
		t.Errorf("the run's workspace is %q, want the run worktree %q", got, manifest.Output.WorkTree)
	}
	if got := store.Layout().RunDir(); got != filepath.Join(dir, ".belay", "runs", runID) {
		t.Errorf("the run directory moved to %q", got)
	}

	// The user's checkout is untouched: still on its own branch, still clean.
	if got := gitIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("the user's checkout is on %q, want main", got)
	}
	if got := gitIn(t, dir, "status", "--porcelain", "--", ":(exclude).belay"); got != "" {
		t.Errorf("the user's checkout is dirty:\n%s", got)
	}
}

// createRun writes the manifest last, so a failure at that last write leaves
// a branch and a worktree that no manifest names.
//
// A run directory without a manifest is already understood: nothing takes it
// for a run, and `belay runs` reports it. A branch is not — it is outside the
// run directory, in the repository the user works in, and nothing would ever
// connect it back to the run that made it. So the one failure createRun can
// see, it undoes.
func TestCreateRunLeavesNoBranchBehindWhenTheManifestCannotBeWritten(t *testing.T) {
	dir := gitWorkspace(t)
	p := planFor(t, dir, nil)
	p.resolveOutput(t.Context())

	const runID = "20260101T000000Z-c0ffee000000"
	// A directory where the manifest goes: the write fails, everything
	// before it succeeded.
	layout, err := state.NewLayout(dir, runID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := os.MkdirAll(layout.ManifestPath(), 0o750); err != nil {
		t.Fatalf("mkdir over the manifest path: %v", err)
	}

	if _, err := createRun(t.Context(), p, runSpec{Workspace: dir, RunID: runID, Goal: "add a --json flag"}); err == nil {
		t.Fatal("createRun succeeded, want the manifest write to fail")
	}

	branch := "belay/" + runID
	refs := gitIn(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/belay/")
	if strings.Contains(refs, branch) {
		t.Errorf("%s was left behind; nothing would ever clean it up (refs: %q)", branch, refs)
	}
	if list := gitIn(t, dir, "worktree", "list", "--porcelain"); strings.Contains(list, runID) {
		t.Errorf("the run worktree is still registered:\n%s", list)
	}
}

// The dispatcher can only hand promote a repository and an isolator if the
// plan gives it both. Nothing else in belay constructs them, so a branch run
// whose options carry neither reaches promote and fails there — after the
// agent has been paid for every step before it.
func TestOptionsCarryTheRepositoryAndIsolatorOfABranchRun(t *testing.T) {
	dir := gitWorkspace(t)
	p := planFor(t, dir, nil)
	p.resolveOutput(t.Context())

	const runID = "20260101T000000Z-c0ffee000000"
	store, err := createRun(t.Context(), p, runSpec{Workspace: dir, RunID: runID, Goal: "add a --json flag"})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}

	opts := p.options(store)
	if opts.VCS == nil {
		t.Error("options carry no VCS; promote would refuse the run it was wired into")
	}
	if opts.Isolator == nil {
		t.Error("options carry no Isolator; a fanout winner could never be merged")
	}

	inPlace := planFor(t, goWorkspace(t), func(c *config.Config) { c.Output.Mode = config.OutputInPlace })
	inPlace.resolveOutput(t.Context())
	if got := inPlace.options(nil).VCS; got != nil {
		t.Errorf("an in-place run's options carry a VCS (%T); nothing in such a run commits", got)
	}
}

// writesAFile is a node that writes one file into the workspace it is given
// and then routes to the quality gate's successor, standing in for the agent,
// the tests and the gate. What it proves is where it wrote.
type writesAFile struct {
	name string
	rel  string
	body string
	next string
	// where records the workspace the node was actually handed.
	where *string
}

func (n writesAFile) Name() string { return n.name }

func (n writesAFile) Run(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
	*n.where = rc.Workspace
	path := filepath.Join(rc.Workspace, filepath.FromSlash(n.rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return graph.Result{Status: journal.StatusFailed}, err
	}
	if err := os.WriteFile(path, []byte(n.body), 0o600); err != nil {
		return graph.Result{Status: journal.StatusFailed}, err
	}
	// Recorded as the write step records a change, because promote commits
	// only what the run's steps recorded.
	code := rc.State.Code
	code.ChangedFiles = []string{n.rel}
	return graph.Result{Status: journal.StatusOK, Next: n.next, Note: "gate passed", Patch: state.Patch{Code: &code}}, nil
}

// This is the assertion the whole slice exists for: a real run in branch
// mode, against a real git repository, leaves the user's checkout exactly as
// it found it.
//
// Every other test here checks one seam. This one drives the seams belay
// itself drives — resolve, createRun, dispatchOnce — with the real promote
// node, the real internal/git repository and a real repository on disk, and
// then asks git, not belay, what happened. The run's change is on
// belay/<run-id> as one commit carrying the run's trailer; the user's HEAD,
// its commit and its working tree are untouched, down to the file the run
// wrote, which must not exist in the checkout at all.
func TestRunOnABranchNeverTouchesTheUsersCheckout(t *testing.T) {
	dir := gitWorkspace(t)
	before := gitIn(t, dir, "rev-parse", "HEAD")
	beforeRef := gitIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD")

	plan, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !plan.output.Branched() {
		t.Fatalf("resolve did not choose branch output: %s", plan.outputReason)
	}
	if !slices.Contains(plan.route, graph.NodePromote) {
		t.Fatalf("route %v has no promote node, so nothing would commit", plan.route)
	}

	const (
		runID = "20260101T000000Z-c0ffee000000"
		goal  = "add an Add helper to src/app.go"
	)
	store, err := createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: runID, Goal: goal})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}

	// The graph is belay's own, with the steps that call an agent, a test
	// runner and a linter replaced by one node that writes a file. promote,
	// which is what this test is about, is the real one.
	var wroteIn string
	registry := graph.NewRegistry()
	registry.MustRegister(
		writesAFile{name: journal.FirstNode, rel: "src/app.go",
			body: "package src\n\nfunc Add(a, b int) int { return a + b }\n",
			next: graph.NodePromote, where: &wroteIn},
		promote.New(),
	)
	plan.registry = registry

	view, err := dispatchOnce(t.Context(), plan, store, false, func(o *graph.Options) {
		o.Config.Graph.Approval = false
	})
	if err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}
	if view.outcome.Status != state.RunStatusCompleted {
		t.Fatalf("run status = %v (error %v), want %v", view.outcome.Status, view.runErr, state.RunStatusCompleted)
	}

	branch := "belay/" + runID
	manifest, err := store.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	// The node worked in the run's worktree, not in the checkout.
	if wroteIn != manifest.Output.WorkTree {
		t.Errorf("the run's node worked in %q, want the run worktree %q", wroteIn, manifest.Output.WorkTree)
	}

	// The change is on the run's branch, as exactly one commit off the base.
	if got := gitIn(t, dir, "rev-list", "--count", before+".."+branch); got != "1" {
		t.Errorf("%s has %s commits since the base, want exactly 1", branch, got)
	}
	if got := gitIn(t, dir, "show", "--name-only", "--format=", branch); got != "src/app.go" {
		t.Errorf("%s's commit changed %q, want src/app.go", branch, got)
	}
	msg := gitIn(t, dir, "log", "-1", "--format=%B", branch)
	if !strings.Contains(msg, "Belay-Run: "+runID) {
		t.Errorf("the commit message has no Belay-Run trailer:\n%s", msg)
	}
	if !strings.Contains(msg, goal) {
		t.Errorf("the commit message does not say what the run was for:\n%s", msg)
	}

	// The commit is the user's own identity, not a bot's: nothing here set
	// an author, so git used whatever the person's git is configured with.
	if author := gitIn(t, dir, "log", "-1", "--format=%an <%ae>", branch); !strings.Contains(author, "@") {
		t.Errorf("the commit has no author: %q", author)
	}

	// And the checkout: same branch, same commit, no new file, nothing dirty.
	if got := gitIn(t, dir, "rev-parse", "HEAD"); got != before {
		t.Errorf("the user's HEAD moved from %s to %s", before, got)
	}
	if got := gitIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != beforeRef {
		t.Errorf("the user's checkout was switched from %s to %s", beforeRef, got)
	}
	if got := gitIn(t, dir, "status", "--porcelain", "--", ":(exclude).belay"); got != "" {
		t.Errorf("the user's checkout is no longer clean:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "app.go")); !os.IsNotExist(err) {
		t.Errorf("the run's file exists in the user's checkout (stat error %v); the whole point is that it does not", err)
	}

	// The promotion is recorded where `belay runs` and the timeline read it.
	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.Promotion.Branch != branch || st.Promotion.Files != 1 {
		t.Errorf("state.Promotion = %+v, want %s with 1 changed file", st.Promotion, branch)
	}
	if got := gitIn(t, dir, "rev-parse", branch); st.Promotion.Commit != got {
		t.Errorf("state.Promotion.Commit = %q, want the branch tip %q", st.Promotion.Commit, got)
	}
}

// A resume finishes a run the way the run started, whatever belay.yaml says
// by the time it is resumed.
//
// resolve builds a plan from the configuration on disk now, which may be days
// newer than the run. A run started on a branch and resumed in place would
// lose its promote node, end without committing, and edit the checkout its
// owner was promised belay would not touch. So the pin in the manifest wins,
// and the drift is said out loud rather than silently resolved.
func TestResumeKeepsTheOutputTheRunStartedWith(t *testing.T) {
	dir := gitWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const runID = "20260101T000000Z-c0ffee000000"
	store, err := createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: runID, Goal: "add a --json flag"})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}

	// belay.yaml now says in_place, which is what a fresh resolve would give
	// a resume.
	writeConfig(t, dir, "version: 1\noutput:\n  mode: \"in_place\"\n")
	resumed, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve after the config changed: %v", err)
	}
	if resumed.output.Branched() {
		t.Fatal("the rewritten belay.yaml did not take effect, so this test proves nothing")
	}

	if err := resumed.resumePreflight(t.Context(), store); err != nil {
		t.Fatalf("resumePreflight: %v", err)
	}
	if len(resumed.blockers) > 0 {
		t.Fatalf("resumePreflight blocked a resumable run: %v", resumed.blockers)
	}

	if !resumed.output.Branched() {
		t.Fatalf("the resumed plan is %q, want the branch output the run started with", resumed.output.Mode)
	}
	if !slices.Contains(resumed.route, graph.NodePromote) {
		t.Errorf("route %v has no promote node; the run would end without committing", resumed.route)
	}
	if !slices.Contains(resumed.registry.Names(), graph.NodePromote) {
		t.Errorf("registry %v has no promote node; the run would end on an unknown node", resumed.registry.Names())
	}
	if resumed.options(store).VCS == nil {
		t.Error("the resumed plan has no repository, so promote would refuse the run")
	}
	joined := strings.Join(resumed.warnings, "\n")
	for _, want := range []string{"output.mode", "in_place", "branch"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q do not mention the drift (%q)", joined, want)
		}
	}
}

// A run whose worktree is gone cannot be continued, and belay must say so
// before it spends anything rather than after.
//
// Without the check the dispatcher runs the run's nodes against a directory
// that is not there, and it presents as a node failing — after the agent has
// been called and paid for. The refusal names the commands that get the
// person somewhere, and writes nothing at all to the run.
func TestResumeRefusesAMissingRunWorktree(t *testing.T) {
	dir := gitWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const runID = "20260101T000000Z-c0ffee000000"
	store, err := createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: runID, Goal: "add a --json flag"})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}
	worktree := store.Layout().WorkTreeDir()
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the run worktree: %v", err)
	}

	resumed, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := resumed.resumePreflight(t.Context(), store); err != nil {
		t.Fatalf("resumePreflight returned an error, want a blocker: %v", err)
	}
	if len(resumed.blockers) == 0 {
		t.Fatal("resumePreflight allowed a run whose worktree is gone")
	}
	said := resumed.blockers[0].problem + " " + resumed.blockers[0].fix
	for _, want := range []string{worktree, "belay/" + runID, "git worktree"} {
		if !strings.Contains(said, want) {
			t.Errorf("blocker %q does not mention %q", said, want)
		}
	}

	// Nothing is written: the run is exactly as it was found.
	if _, err := os.Stat(store.Layout().JournalPath()); !os.IsNotExist(err) {
		t.Errorf("resumePreflight opened the run's journal (stat error %v); it must write nothing", err)
	}
}

// A resumed branch run must work in the run's worktree, exactly as the run
// that created it did. The redirection lives in one place for both commands,
// because `belay run` and `belay resume` disagreeing about where a run's
// nodes write is the shape of bug nobody finds until a checkout is dirty.
func TestWorkStoreRedirectsAResumedBranchRun(t *testing.T) {
	dir := gitWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const runID = "20260101T000000Z-c0ffee000000"
	if _, err := createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: runID, Goal: "add a --json flag"}); err != nil {
		t.Fatalf("createRun: %v", err)
	}

	layout, err := state.NewLayout(dir, runID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	resumed, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := resumed.resumePreflight(t.Context(), state.NewStore(layout)); err != nil {
		t.Fatalf("resumePreflight: %v", err)
	}

	store, err := resumed.workStore(layout)
	if err != nil {
		t.Fatalf("workStore: %v", err)
	}
	if got, want := store.Layout().WorkspaceDir(), layout.WorkTreeDir(); got != want {
		t.Errorf("a resumed branch run works in %q, want the run worktree %q", got, want)
	}
	if got := store.Layout().RunDir(); got != layout.RunDir() {
		t.Errorf("the run directory moved to %q, want %q", got, layout.RunDir())
	}

	// An in-place run is unchanged: it works where it was started.
	inPlace := planFor(t, dir, nil)
	inPlace.output = state.Output{Mode: state.OutputInPlace}
	plain, err := inPlace.workStore(layout)
	if err != nil {
		t.Fatalf("workStore(in place): %v", err)
	}
	if got := plain.Layout().WorkspaceDir(); got != dir {
		t.Errorf("an in-place run works in %q, want the workspace %q", got, dir)
	}
}

// What happens to the run's worktree when a dispatch returns depends on
// whether the run is over.
//
// A completed run's result is on its branch, and the worktree is a second
// checkout of the repository taking up disk for nothing; leaving one behind
// per run fills a disk and clutters `git worktree list`. A run that paused or
// failed is not over: its worktree is where the person reads what belay did,
// finishes a merge by hand, or resumes. Removing it would throw away the
// uncommitted work a resume needs.
//
// The branch is never deleted, whatever happened. It is the deliverable.
func TestFinishRunKeepsTheBranchAndOnlyTidiesAFinishedRun(t *testing.T) {
	tests := []struct {
		name       string
		status     state.RunStatus
		wantRemove bool
	}{
		{"completed", state.RunStatusCompleted, true},
		{"paused", state.RunStatusPaused, false},
		{"failed", state.RunStatusFailed, false},
		{"aborted", state.RunStatusAborted, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := gitWorkspace(t)
			plan, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			const runID = "20260101T000000Z-c0ffee000000"
			store, err := createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: runID, Goal: "add a --json flag"})
			if err != nil {
				t.Fatalf("createRun: %v", err)
			}
			worktree := plan.output.WorkTree

			plan.finishRun(t.Context(), outcomeView{
				layout:  store.Layout(),
				outcome: graph.Outcome{Status: tt.status},
			})

			_, statErr := os.Stat(worktree)
			if removed := os.IsNotExist(statErr); removed != tt.wantRemove {
				t.Errorf("worktree removed = %v, want %v (stat error %v)", removed, tt.wantRemove, statErr)
			}
			registered := strings.Contains(gitIn(t, dir, "worktree", "list", "--porcelain"), worktree)
			if registered == tt.wantRemove {
				t.Errorf("worktree still registered = %v, want %v", registered, !tt.wantRemove)
			}
			// The branch survives every outcome: it is the deliverable.
			if got := gitIn(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/belay/"); got != "belay/"+runID {
				t.Errorf("belay/%s did not survive a %v run (refs: %q)", runID, tt.status, got)
			}
		})
	}
}

// A run paused at promote is paused for a reason nobody can guess from
// "read the plan": two branches disagree, and the person has to decide. The
// closing report has to name the worktree to do it in and the command that
// picks the run back up.
func TestNextStepsAfterAPromoteConflict(t *testing.T) {
	layout, err := state.NewLayout("/repos/app", "20260101T000000Z-c0ffee000000")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	out := state.Output{
		Mode: state.OutputBranch, Repo: "/repos/app",
		Branch: "belay/20260101T000000Z-c0ffee000000", Base: "a0aa486bdfbf",
		WorkTree: "/repos/app/.belay/runs/20260101T000000Z-c0ffee000000/worktree",
	}
	lines := strings.Join(nextSteps(outcomeView{
		layout: layout,
		output: out,
		outcome: graph.Outcome{
			Status: state.RunStatusPaused, Node: graph.NodePromote,
			Note: "candidate c1 conflicts with belay/20260101T000000Z-c0ffee000000 in a.go",
		},
	}), "\n")

	for _, want := range []string{out.WorkTree, "git status", "belay resume 20260101T000000Z-c0ffee000000"} {
		if !strings.Contains(lines, want) {
			t.Errorf("next steps do not mention %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "read the plan") {
		t.Errorf("next steps still tell the person to read a plan nobody is waiting on:\n%s", lines)
	}
}

// A completed branch run must be reported by its branch, not by "review the
// change in your repository, then commit it" — which names a change that is
// not in the repository and a commit belay already made.
func TestNextStepsAfterACompletedBranchRun(t *testing.T) {
	layout, err := state.NewLayout("/repos/app", "20260101T000000Z-c0ffee000000")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	branch := "belay/20260101T000000Z-c0ffee000000"
	lines := strings.Join(nextSteps(outcomeView{
		layout:  layout,
		output:  state.Output{Mode: state.OutputBranch, Repo: "/repos/app", Branch: branch, BaseRef: "main"},
		outcome: graph.Outcome{Status: state.RunStatusCompleted, Node: graph.End},
	}), "\n")

	for _, want := range []string{branch, "git log", "git switch"} {
		if !strings.Contains(lines, want) {
			t.Errorf("next steps do not mention %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "then commit it") {
		t.Errorf("next steps tell the person to make a commit belay already made:\n%s", lines)
	}
}

// Whether belay is about to edit your files or work somewhere else entirely
// is the single most consequential thing a run does, so it is said before
// anything is spent — in the dry run, and again as a real run starts.
//
// The dry run must also still create nothing. Describing a branch run reads
// the repository, and `git status` without --no-optional-locks rewrites
// .git/index, which would make "nothing was created" untrue in the one way a
// person would never think to check.
func TestDryRunDescribesBranchOutputAndCreatesNothing(t *testing.T) {
	ws := gitWorkspace(t)
	index := filepath.Join(ws, ".git", "index")
	stampIndex(t, index)
	before := listTree(t, ws)
	beforeIndex := modTime(t, index)

	out, _, err := runCLI(t, "--dry-run", "run", "--workspace", ws, "add tests")
	if err != nil {
		t.Fatalf("belay --dry-run run: %v\n%s", err, out)
	}

	for _, want := range []string{"belay/<run-id>", "never edits your checkout"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run does not say %q:\n%s", want, out)
		}
	}
	if diff := cmp.Diff(before, listTree(t, ws)); diff != "" {
		t.Errorf("a dry run created or removed files (-before +after):\n%s", diff)
	}
	if got := modTime(t, index); !got.Equal(beforeIndex) {
		t.Errorf(".git/index was rewritten (%v -> %v); belay's git reads must pass --no-optional-locks", beforeIndex, got)
	}
	if refs := gitIn(t, ws, "for-each-ref", "--format=%(refname:short)", "refs/heads/belay/"); refs != "" {
		t.Errorf("a dry run created branches: %q", refs)
	}
	if list := gitIn(t, ws, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Errorf("a dry run created worktrees:\n%s", list)
	}
}

// stampIndex gives .git/index a mod time in the past, so a rewrite of it
// during the test is visible however coarse the filesystem's timestamps are.
func stampIndex(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes(%q): %v", path, err)
	}
}

// modTime reads a file's modification time.
func modTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	return info.ModTime()
}

// TestOpenRunWorksInTheRunsWorktree pins the one path every command that
// continues a run takes. belay plan --regenerate once built its own store and
// dispatched a branch run with the user's checkout as its working directory,
// because it skipped the redirect belay resume applied.
func TestOpenRunWorksInTheRunsWorktree(t *testing.T) {
	dir := gitWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const runID = "20260101T000000Z-c0ffee000001"
	if _, err := createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: runID, Goal: "add a --json flag"}); err != nil {
		t.Fatalf("createRun: %v", err)
	}
	layout, err := state.NewLayout(dir, runID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	resumed, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	store, err := resumed.openRun(t.Context(), layout)
	if err != nil {
		t.Fatalf("openRun: %v", err)
	}
	if got, want := store.Layout().WorkspaceDir(), layout.WorkTreeDir(); got != want {
		t.Errorf("a continued branch run works in %q, want the run worktree %q: its nodes would read and write the user's checkout", got, want)
	}
	if got := store.Layout().RunDir(); got != layout.RunDir() {
		t.Errorf("the run directory moved to %q, want %q", got, layout.RunDir())
	}
}

// A resume judges the run's pinned output, never what belay.yaml would give a
// run starting now. resolve judges the file first, and whatever it concluded
// about the output belongs to that hypothetical new run: a warning that
// uncommitted work is left out of a branch run starting from HEAD, or a
// refusal of a branch the checkout can no longer take. Neither is about the
// run being resumed, which has its own output already, and neither may
// survive the bind -- a warning would be false, and a refusal would stop a
// run that is fine.
func TestAResumeJudgesThePinNotWhatBelayYamlWouldStartNow(t *testing.T) {
	tests := []struct {
		name string
		// started is belay.yaml when the run is created; then changes
		// the repository or belay.yaml before the resume.
		started string
		then    func(t *testing.T, dir string)
		// wantWarn must each be in the warnings, and wantNot none of them.
		wantWarn, wantNot []string
	}{
		{
			name:    "pinned in place, and belay.yaml now resolves a branch over uncommitted work",
			started: "version: 1\noutput:\n  mode: in_place\n",
			then: func(t *testing.T, dir string) {
				writeConfig(t, dir, "version: 1\noutput:\n  mode: auto\n")
				if err := os.WriteFile(filepath.Join(dir, "scratch.go"), []byte("package scratch\n"), 0o600); err != nil {
					t.Fatalf("write scratch.go: %v", err)
				}
			},
			wantWarn: []string{"output.mode"},
			wantNot:  []string{"uncommitted"},
		},
		{
			name:    "pinned on a branch, and the checkout can no longer take one",
			started: "version: 1\noutput:\n  mode: branch\n",
			then: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte("[submodule \"x\"]\n"), 0o600); err != nil {
					t.Fatalf("write .gitmodules: %v", err)
				}
				gitIn(t, dir, "add", ".gitmodules")
				gitIn(t, dir, "commit", "-qm", "add a submodule")
			},
			wantNot: []string{"output.mode"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := gitWorkspace(t)
			writeConfig(t, dir, tt.started)
			plan, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			const runID = "20260101T000000Z-c0ffee000000"
			store, err := createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: runID, Goal: "add a --json flag"})
			if err != nil {
				t.Fatalf("createRun: %v", err)
			}
			tt.then(t, dir)

			resumed, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
			if err != nil {
				t.Fatalf("resolve for the resume: %v", err)
			}
			if err := resumed.resumePreflight(t.Context(), store); err != nil {
				t.Fatalf("resumePreflight: %v", err)
			}
			if len(resumed.blockers) > 0 {
				t.Errorf("the resume was refused for what belay.yaml would give a new run: %s", blockerText(resumed))
			}
			joined := strings.Join(resumed.warnings, "\n")
			for _, want := range tt.wantWarn {
				if !strings.Contains(joined, want) {
					t.Errorf("warnings never mention %q:\n%s", want, joined)
				}
			}
			for _, not := range tt.wantNot {
				if strings.Contains(joined, not) {
					t.Errorf("warnings mention %q, about a run this is not:\n%s", not, joined)
				}
			}
		})
	}
}

// runPrinted runs a command belay printed for a person to paste, through a
// shell, as they would, with git kept away from the machine's own settings.
func runPrinted(t *testing.T, command string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // running what belay printed, as pasted, is the test
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the command belay printed failed: %v\n$ %s\n%s", err, command, out)
	}
}

// TestTheCommandThatRemakesAMissingWorktreeWorks: a branch run whose worktree
// is gone is refused before anything is spent, with the command that makes
// the worktree again. That command has to work as printed. When the
// directory was deleted, git still has the worktree registered and refuses a
// plain `worktree add` ("is a missing but already registered worktree");
// only `worktree add -f` takes the path back. When the registration was
// pruned as well, a plain add works, and -f is not printed: it would also
// check the branch out a second time if it were checked out somewhere else.
// Either way, the printed command is run as printed, and then the run
// resumes, in its worktree.
func TestTheCommandThatRemakesAMissingWorktreeWorks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pruned bool
		force  bool
	}{
		{name: "the directory was deleted and git still has it registered", force: true},
		{name: "the directory was deleted and git pruned it", pruned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := gitWorkspace(t)
			plan, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			const runID = "20260101T000000Z-c0ffee000002"
			store, err := createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: runID, Goal: "add a --json flag"})
			if err != nil {
				t.Fatalf("createRun: %v", err)
			}
			layout := store.Layout()
			if err := os.RemoveAll(layout.WorkTreeDir()); err != nil {
				t.Fatalf("remove the run worktree: %v", err)
			}
			if tc.pruned {
				gitIn(t, dir, "worktree", "prune")
			}

			refused, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if err := refused.resumePreflight(t.Context(), state.NewStore(layout)); err != nil {
				t.Fatalf("resumePreflight: %v", err)
			}
			fix := blockerText(refused)
			start := strings.Index(fix, "`git -C ")
			if start < 0 {
				t.Fatalf("the refusal prints no git command to make the worktree again:\n%s", fix)
			}
			command, _, closed := strings.Cut(fix[start+1:], "`")
			if !closed {
				t.Fatalf("the refusal's git command is never closed:\n%s", fix)
			}
			if got := strings.Contains(command, " add -f "); got != tc.force {
				t.Errorf("the printed command %q uses -f = %v, want %v", command, got, tc.force)
			}
			runPrinted(t, command)

			resumed, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			work, err := resumed.openRun(t.Context(), layout)
			if err != nil {
				t.Fatalf("openRun: %v", err)
			}
			if len(resumed.blockers) > 0 {
				t.Fatalf("the resume after running the printed command was refused: %s", blockerText(resumed))
			}
			var where string
			registry := graph.NewRegistry()
			registry.MustRegister(writesAFile{name: graph.NodePlan, rel: "proof.txt", body: "here\n", next: graph.End, where: &where})
			resumed.registry = registry
			view, err := dispatchOnce(t.Context(), resumed, work, true)
			if err != nil || view.outcome.Status != state.RunStatusCompleted {
				t.Fatalf("the resume: %v, %v %q; want it completed", err, view.outcome.Status, view.outcome.Note)
			}
			if where != layout.WorkTreeDir() {
				t.Errorf("the resumed run worked in %s, want its remade worktree %s", where, layout.WorkTreeDir())
			}
			if _, err := os.Stat(filepath.Join(layout.WorkTreeDir(), "proof.txt")); err != nil {
				t.Errorf("the resumed run's file is not in its worktree: %v", err)
			}
		})
	}
}
