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

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
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
	cmd := exec.CommandContext(ctx, "git", args...)
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
		{
			name:      "dircopy candidates cannot be merged onto a branch",
			workspace: gitWorkspace(t),
			cfg: func(c *config.Config) {
				c.Output.Mode = config.OutputBranch
				c.Fanout.Enabled = true
				c.Fanout.Isolator = config.IsolatorDircopy
			},
			wantBlock: []string{"fanout.isolator", "dircopy"},
		},
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
