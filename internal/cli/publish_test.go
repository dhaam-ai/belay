package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
)

// scriptedExecer stands in for the gh command line. It records every argv it
// is given and answers from a script, so the tests can pin exactly what belay
// would run without a GitHub account, a network or a repository on it.
type scriptedExecer struct {
	mu    sync.Mutex
	calls []bexec.Command
	// reply answers one command, chosen by the test from its argv.
	reply func(bexec.Command) (bexec.Result, error)
}

func (s *scriptedExecer) Run(_ context.Context, c bexec.Command) (bexec.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
	if s.reply == nil {
		return bexec.Result{}, nil
	}
	return s.reply(c)
}

// commands returns every argv recorded, program first.
func (s *scriptedExecer) commands() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, 0, len(s.calls))
	for _, c := range s.calls {
		out = append(out, append([]string{c.Path}, c.Args...))
	}
	return out
}

// find returns the first recorded argv whose second word is want.
func (s *scriptedExecer) find(want ...string) []string {
	for _, argv := range s.commands() {
		if len(argv) > len(want) && slices.Equal(argv[1:1+len(want)], want) {
			return argv
		}
	}
	return nil
}

// --pr promises something belay may not be able to keep, so every reason it
// could not is found before the run starts rather than after it has finished
// and spent its budget.
func TestPullRequestBlockers(t *testing.T) {
	requireGit(t)

	withRemote := gitWorkspace(t)
	gitIn(t, withRemote, "remote", "add", "origin", "https://example.invalid/app.git")

	tests := []struct {
		name      string
		workspace string
		inPlace   bool
		gh        string
		wantBlock []string
	}{
		{
			name: "no pull request without a branch",
			// in_place makes no commit and no branch, so there is nothing
			// a pull request could propose.
			workspace: withRemote, inPlace: true, gh: "gh",
			wantBlock: []string{"--pr", "output.mode"},
		},
		{
			name:      "no pull request without gh",
			workspace: withRemote, gh: "",
			wantBlock: []string{"gh", "cli.github.com"},
		},
		{
			name:      "no pull request without a remote",
			workspace: gitWorkspace(t), gh: "gh",
			wantBlock: []string{"remote"},
		},
		{
			name:      "everything present",
			workspace: withRemote, gh: "gh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := planFor(t, tt.workspace, func(cfg *config.Config) {
				if tt.inPlace {
					cfg.Output.Mode = config.OutputInPlace
				}
			})
			p.resolveOutput(t.Context())
			p.resolvePullRequest(t.Context(), true, func(string) (string, error) {
				if tt.gh == "" {
					return "", os.ErrNotExist
				}
				return filepath.Join("/usr/local/bin", tt.gh), nil
			})

			if len(tt.wantBlock) == 0 {
				if len(p.blockers) > 0 {
					t.Fatalf("--pr was blocked: %v", p.blockers)
				}
				return
			}
			if len(p.blockers) == 0 {
				t.Fatalf("--pr was allowed, want a blocker mentioning %v", tt.wantBlock)
			}
			said := p.blockers[0].problem + " " + p.blockers[0].fix
			for _, want := range tt.wantBlock {
				if !strings.Contains(said, want) {
					t.Errorf("blocker %q does not mention %q", said, want)
				}
			}
		})
	}
}

// publish pushes the run's branch and opens one draft pull request, and asks
// first whether one is already open.
//
// Asking first is what keeps a second invocation from creating a second pull
// request for the same branch: `gh pr create` does refuse a duplicate, but it
// refuses with an error, and a run that completed must not be reported as
// having failed because the work was already proposed.
func TestPublishOpensOneDraftPullRequest(t *testing.T) {
	const branch = "belay/20260101T000000Z-c0ffee000000"
	out := state.Output{
		Mode: state.OutputBranch, Repo: "/repos/app", Branch: branch,
		Base: "a0aa486bdfbfbd2e69f2a08dc947c786f4b56650", BaseRef: "main",
		WorkTree:    "/repos/app/.belay/runs/20260101T000000Z-c0ffee000000/worktree",
		PullRequest: true,
	}

	gh := &scriptedExecer{}
	gh.reply = func(c bexec.Command) (bexec.Result, error) {
		if slices.Contains(c.Args, "create") {
			return bexec.Result{Stdout: "https://example.invalid/app/pull/7\n"}, nil
		}
		return bexec.Result{Stdout: "[]\n"}, nil
	}

	var said bytes.Buffer
	u := &ui{out: &said, err: &said}
	url, err := publish(t.Context(), u, publishRequest{
		output: out, goal: "add a --json flag", runID: "20260101T000000Z-c0ffee000000",
		remote: "origin", files: 3, gh: gh,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if url != "https://example.invalid/app/pull/7" {
		t.Errorf("publish returned %q, want the created pull request's URL", url)
	}

	list := gh.find("pr", "list")
	if list == nil {
		t.Fatalf("publish did not ask whether a pull request already exists:\n%v", gh.commands())
	}
	if !slices.Contains(list, "--head") || !slices.Contains(list, branch) {
		t.Errorf("the list argv %v does not ask about this run's branch", list)
	}

	create := gh.find("pr", "create")
	if create == nil {
		t.Fatalf("publish opened no pull request:\n%v", gh.commands())
	}
	if !slices.Contains(create, "--draft") {
		t.Errorf("the pull request is not a draft: %v", create)
	}
	if !slices.Contains(create, "--head") || !slices.Contains(create, branch) {
		t.Errorf("the create argv %v does not name this run's branch as the head", create)
	}
	// decisions.md: there is no --base flag. gh picks the base itself, from
	// the repository's default branch or a configured merge base.
	if slices.Contains(create, "--base") || slices.Contains(create, "-B") {
		t.Errorf("the create argv %v passes a base; belay does not choose one", create)
	}
}

// A pull request that is already there is reported, not made again, and the
// branch is not pushed a second time either.
func TestPublishReportsAPullRequestThatIsAlreadyOpen(t *testing.T) {
	gh := &scriptedExecer{}
	gh.reply = func(bexec.Command) (bexec.Result, error) {
		return bexec.Result{Stdout: `[{"url":"https://example.invalid/app/pull/4","state":"OPEN"}]` + "\n"}, nil
	}

	var said bytes.Buffer
	u := &ui{out: &said, err: &said}
	url, err := publish(t.Context(), u, publishRequest{
		output: state.Output{
			Mode: state.OutputBranch, Repo: "/repos/app",
			Branch: "belay/20260101T000000Z-c0ffee000000", PullRequest: true,
		},
		goal: "add a --json flag", runID: "20260101T000000Z-c0ffee000000",
		remote: "origin", files: 3, gh: gh,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if url != "https://example.invalid/app/pull/4" {
		t.Errorf("publish returned %q, want the pull request that is already open", url)
	}
	if create := gh.find("pr", "create"); create != nil {
		t.Errorf("publish opened a second pull request: %v", create)
	}
}

// A run that changed nothing has nothing to propose, and an empty pull
// request is worse than none: somebody has to open it, read it and close it.
func TestPublishSkipsARunThatChangedNothing(t *testing.T) {
	gh := &scriptedExecer{}
	var said bytes.Buffer
	u := &ui{out: &said, err: &said}

	url, err := publish(t.Context(), u, publishRequest{
		output: state.Output{
			Mode: state.OutputBranch, Repo: "/repos/app",
			Branch: "belay/20260101T000000Z-c0ffee000000", PullRequest: true,
		},
		runID: "20260101T000000Z-c0ffee000000", remote: "origin", files: 0, gh: gh,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if url != "" {
		t.Errorf("publish returned %q for a run that changed nothing", url)
	}
	if cmds := gh.commands(); len(cmds) > 0 {
		t.Errorf("publish ran gh for a run that changed nothing: %v", cmds)
	}
	if !strings.Contains(said.String(), "nothing to propose") {
		t.Errorf("publish said %q, want it to say there was nothing to propose", said.String())
	}
}

// A pull request that could not be opened must not turn a finished run into
// a failed one. The work is committed on the branch either way; what is left
// is one command a person runs themselves, so the report names it.
func TestPublishFailureNeverFailsACompletedRun(t *testing.T) {
	dir := gitWorkspace(t)
	gitIn(t, dir, "remote", "add", "origin", "https://example.invalid/app.git")

	plan, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	plan.output.PullRequest = true
	plan.output.Branch = "belay/20260101T000000Z-c0ffee000000"
	plan.output.WorkTree = filepath.Join(dir, ".belay", "runs", "20260101T000000Z-c0ffee000000", "worktree")
	plan.gh = &scriptedExecer{reply: func(bexec.Command) (bexec.Result, error) {
		return bexec.Result{Stderr: "gh: not authenticated", ExitCode: 1}, os.ErrPermission
	}}

	layout, err := state.NewLayout(dir, "20260101T000000Z-c0ffee000000")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	var said bytes.Buffer
	u := &ui{out: &said, err: &said}
	view := outcomeView{
		layout:  layout,
		output:  plan.output,
		outcome: graph.Outcome{Status: state.RunStatusCompleted, Node: graph.End, ExitCode: graph.ExitOK},
	}
	plan.publishRun(t.Context(), u, &view)

	if err := reportOutcome(u, view); err != nil {
		t.Errorf("reportOutcome = %v, want nil: a pull request that could not be opened is not a failed run", err)
	}
	lines := strings.Join(nextSteps(view), "\n")
	for _, want := range []string{"git push origin", "gh pr create"} {
		if !strings.Contains(lines, want) {
			t.Errorf("next steps do not name the command to run by hand (%q):\n%s", want, lines)
		}
	}
}
