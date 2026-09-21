//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/internal/git"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/isolate/worktree"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/planarchive"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// Branch-output suite: one whole run of belay's real node graph, in branch
// mode, against a real git repository on disk.
//
// Where loopHarness proves the graph's wiring fits together in a plain
// directory, this proves the claim branch output exists to make: belay works
// somewhere else entirely, and the repository a person is sitting in does not
// change while it does. Every node here is belay's own -- plan, code, write,
// test, fix, review and promote -- and every git command is real git, because
// the failures this is written against (a commit landing in the wrong
// worktree, a node writing through a path that resolves out of the tree) are
// not failures a fake can have.
//
// The agent, the test runner and the linter are belaytest fakes, for the
// reason the rest of this package uses them: what is under test is where the
// work lands, not whether a model can write Go.
const branchRunID = "20260101T000000Z-b4a9c4000000"

// branchAgentPrompt is what a skipped test tells its reader to ask a coding
// agent, so it runs next time instead of skipping again.
const branchAgentPrompt = "Install git so it is on PATH and `git --version` prints 2.38 or newer, " +
	"then run `go test ./test/e2e/... -tags=e2e -run TestBranch -v` and confirm the tests pass instead of skipping."

// branchGit runs one raw git command in dir, never through the code under
// test: a fixture built by the thing being tested proves nothing.
func branchGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // every argument comes from a test literal
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Belay Test", "GIT_AUTHOR_EMAIL=belay-test@example.invalid",
		"GIT_COMMITTER_NAME=Belay Test", "GIT_COMMITTER_EMAIL=belay-test@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// branchRepo builds a real git repository with one commit on main, holding
// the file the run's agent will claim to have changed.
func branchRepo(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := exec.CommandContext(ctx, "git", "--version").Output(); err != nil {
		t.Skipf("skipping: needs a working git on PATH (%v). "+
			"Install it with `xcode-select --install`, `brew install git` or from "+
			"https://git-scm.com/downloads, or ask your coding agent: %q", err, branchAgentPrompt)
	}

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o750); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "app.go"), []byte("package src\n"), 0o600); err != nil {
		t.Fatalf("write src/app.go: %v", err)
	}
	branchGit(t, dir, "init", "-q", "-b", "main")
	branchGit(t, dir, "add", "-A")
	branchGit(t, dir, "commit", "-qm", "first")
	return dir
}

// TestBranchRunCommitsToItsOwnBranchAndLeavesTheCheckoutAlone drives belay's
// whole real graph, in branch mode, against a real repository, and then asks
// git what happened.
//
// The assertion this exists for is the last one: the repository the person
// works in is exactly as they left it. Everything above it is what has to be
// true for that to be worth anything -- the run really ran, the change really
// landed, and it landed on the run's branch as one commit that says which run
// made it.
func TestBranchRunCommitsToItsOwnBranchAndLeavesTheCheckoutAlone(t *testing.T) {
	repoDir := branchRepo(t)
	baseCommit := branchGit(t, repoDir, "rev-parse", "HEAD")

	cfg := config.Default()
	cfg.Graph.Approval = false
	cfg.Output.Mode = config.OutputBranch

	h := newBranchHarness(t, cfg, repoDir, "add an Add helper")

	agent := &belaytest.FakeAgent{
		Responses: []belay.AgentResponse{
			loopPlanResponse(),
			loopCodeResponse("sess-1", "src/app.go", "src/add.go"),
		},
	}
	// The agent claims two files. src/app.go exists in both trees; src/add.go
	// is new, and is written only into the run's own worktree. The write node
	// checks every reported file against the filesystem it was given, so a
	// run whose nodes were pointed at the user's checkout would find src/add.go
	// absent -- which is what makes this an assertion about where the nodes
	// work, and not only about where promote commits.
	h.editInWorkTree(t, "src/app.go", "package src\n\nfunc Add(a, b int) int { return a + b }\n")
	h.editInWorkTree(t, "src/add.go", "package src\n\n// Add is documented here.\n")

	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{loopPassingTestReport(4)}}
	linter := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopPassingQualityReport("fake-linter")}}

	out, err := h.dispatcher(graph.Options{
		Agent: agent, Runner: runner, Linter: linter,
		VCS: h.repo, Isolator: h.isolator,
	}).Run(t.Context())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want %v (node %q, note %q)", out.Status, state.RunStatusCompleted, out.Node, out.Note)
	}

	// The graph really went through promote, which is the node branch output
	// adds and the only thing in belay that commits.
	if !slices.Contains(h.timeline(), graph.NodePromote+"#1 finished ok -> END") {
		t.Errorf("the run did not finish at promote:\n%s", strings.Join(h.timeline(), "\n"))
	}

	branch := "belay/" + branchRunID
	if got := branchGit(t, repoDir, "rev-list", "--count", baseCommit+".."+branch); got != "1" {
		t.Errorf("%s has %s commits since the base, want exactly 1:\n%s",
			branch, got, branchGit(t, repoDir, "log", "--oneline", baseCommit+".."+branch))
	}
	// The one commit carries the change and the plan it was written from:
	// promote writes the approved plan into the worktree, at the path the
	// run's manifest pinned, before it commits (ADR-0016).
	plan := planarchive.Rel("docs/plans", branchRunID, "add an Add helper")
	if got, want := branchGit(t, repoDir, "show", "--name-only", "--format=", branch), plan+"\nsrc/add.go\nsrc/app.go"; got != want {
		t.Errorf("%s's commit changed %q, want %q", branch, got, want)
	}
	approved, err := os.ReadFile(filepath.Join(h.layout.ArtifactsDir(), "plan.md")) //nolint:gosec // the run directory is under this test's own t.TempDir()
	if err != nil {
		t.Fatalf("read the run's approved plan: %v", err)
	}
	if got := branchGit(t, repoDir, "show", branch+":"+plan); got != strings.TrimSpace(string(approved)) {
		t.Errorf("the plan in %s's commit is not the run's approved plan:\n%s", branch, got)
	}
	// Every file the agent reported was found where the nodes were working.
	// The write node checks each reported path against the filesystem it was
	// given and records how many were there, so a run whose nodes were
	// pointed at the user's checkout counts src/add.go as deleted.
	if got := h.writeNote(); !strings.Contains(got, "2 present, 0 deleted") {
		t.Errorf("the write node reported %q; its nodes are not working in %s", got, h.workTree)
	}
	if msg := branchGit(t, repoDir, "log", "-1", "--format=%B", branch); !strings.Contains(msg, "Belay-Run: "+branchRunID) {
		t.Errorf("the commit carries no Belay-Run trailer, so nothing connects it to the run:\n%s", msg)
	}

	// The state the timeline and `belay runs` read agrees with the repository.
	if got := h.state().Promotion; got.Branch != branch || got.Files != 3 {
		t.Errorf("state.Promotion = %+v, want %s with 3 changed files: the change and its plan", got, branch)
	}

	// THE ASSERTION THIS SLICE EXISTS FOR.
	if got := branchGit(t, repoDir, "rev-parse", "HEAD"); got != baseCommit {
		t.Errorf("the user's HEAD moved from %s to %s", baseCommit, got)
	}
	if got := branchGit(t, repoDir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("the user's checkout was switched to %q, want main", got)
	}
	if got := branchGit(t, repoDir, "status", "--porcelain", "--", ":(exclude).belay"); got != "" {
		t.Errorf("the user's checkout is no longer clean:\n%s", got)
	}
	body, err := os.ReadFile(filepath.Join(repoDir, "src", "app.go")) //nolint:gosec // repoDir is this test's own t.TempDir()
	if err != nil {
		t.Fatalf("read the checkout's src/app.go: %v", err)
	}
	if string(body) != "package src\n" {
		t.Errorf("the run edited the user's own src/app.go:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "src", "add.go")); !os.IsNotExist(err) {
		t.Errorf("the run's new file is in the user's checkout (stat error %v); the whole point is that it is not", err)
	}
}

// branchHarness is loopHarness's branch-mode sibling: a run whose workspace
// is its own git worktree, with the real git repository and the real worktree
// isolator the CLI would build for it.
type branchHarness struct {
	*loopHarness
	repo     *git.Repo
	isolator *worktree.Isolator
	// workTree is the run's own worktree, which is where its nodes work.
	workTree string
}

// newBranchHarness creates a branch-mode run the way internal/cli's createRun
// does: the branch first, then the worktree, then the manifest, with the
// output pinned through SetOutput so the capability is recorded with it.
func newBranchHarness(t *testing.T, cfg config.Config, repoDir, goal string) *branchHarness {
	t.Helper()

	layout, err := state.NewLayout(repoDir, branchRunID)
	if err != nil {
		t.Fatalf("state.NewLayout: %v", err)
	}
	if err := os.MkdirAll(layout.ArtifactsDir(), 0o750); err != nil {
		t.Fatalf("MkdirAll(%s): %v", layout.ArtifactsDir(), err)
	}

	repo, err := git.Open(t.Context(), bexec.New(nil), repoDir)
	if err != nil {
		t.Fatalf("git.Open(%s): %v", repoDir, err)
	}
	head, err := repo.Head(t.Context())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	branch := "belay/" + branchRunID
	if err := repo.CreateBranch(t.Context(), branch, head.Commit); err != nil {
		t.Fatalf("CreateBranch(%s): %v", branch, err)
	}
	if err := repo.AddWorktree(t.Context(), layout.WorkTreeDir(), branch); err != nil {
		t.Fatalf("AddWorktree(%s): %v", layout.WorkTreeDir(), err)
	}

	out := state.Output{
		Mode: state.OutputBranch, Repo: repo.Top(), Branch: branch,
		Base: head.Commit, BaseRef: head.Branch, WorkTree: layout.WorkTreeDir(), Prefix: repo.Prefix(),
	}
	man := state.NewManifest(time.Now().UTC(), branchRunID, repoDir, goal, cfg)
	man.SetOutput(out)
	if err := state.SaveState(layout.StatePath(), state.NewState(goal)); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := state.SaveManifest(layout.ManifestPath(), man); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	// The run's nodes work in the worktree; everything else about the run
	// stays under the run directory, where every reader looks for it.
	working, err := layout.WithWorkTree(layout.WorkTreeDir())
	if err != nil {
		t.Fatalf("WithWorkTree: %v", err)
	}
	registry, err := nodes.Default(cfg, nodes.WithBranchOutput())
	if err != nil {
		t.Fatalf("nodes.Default(WithBranchOutput): %v", err)
	}
	iso, err := worktree.New(repo, layout.CandidatesDir(), branch)
	if err != nil {
		t.Fatalf("worktree.New: %v", err)
	}

	return &branchHarness{
		loopHarness: &loopHarness{
			t: t, workspace: working.WorkspaceDir(), goal: goal,
			layout: working, store: state.NewStore(working), cfg: cfg, registry: registry,
		},
		repo: repo, isolator: iso, workTree: layout.WorkTreeDir(),
	}
}

// writeNote returns what the write node recorded in the journal, which says
// how many of the files the agent reported were actually found.
func (h *branchHarness) writeNote() string {
	h.t.Helper()
	for _, r := range h.records() {
		if r.Node == graph.NodeWrite && r.Event == journal.EventNodeFinished {
			return r.Note
		}
	}
	h.t.Fatal("the run's journal holds no finished write node")
	return ""
}

// editInWorkTree writes a file into the run's own worktree, standing in for
// the edit the agent would have made there.
func (h *branchHarness) editInWorkTree(t *testing.T, rel, body string) {
	t.Helper()
	path := filepath.Join(h.workTree, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
