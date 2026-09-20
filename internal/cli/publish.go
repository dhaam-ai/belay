package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/internal/state"
)

// ghProgram is the command belay opens pull requests with. It is GitHub's own
// command line rather than an HTTP client of belay's, for one reason: gh is
// already signed in. Reading a person's GitHub token out of their keychain,
// their gh configuration or their environment, and then holding it, is a
// thing belay would rather not do at all.
const ghProgram = "gh"

// publishRequest is everything opening one pull request needs, which is a
// struct rather than eight parameters so that a test can build one without a
// run, a repository or a network.
type publishRequest struct {
	// output is the run's pinned branch output.
	output state.Output
	// goal is what the person asked for, which becomes the title.
	goal string
	// runID names the run, for the body and the fallback title.
	runID string
	// remote is the remote the branch was pushed to.
	remote string
	// files is how many files the promotion changed. Zero means there is
	// nothing to propose.
	files int
	// push sends the branch. It is nil in a test that is only pinning what
	// belay would ask gh.
	push func(ctx context.Context, remote, branch string) error
	// gh runs the GitHub command line.
	gh ghRunner
}

// ghRunner runs one gh command. *exec.Runner satisfies it; a test
// substitutes one that records the argv and answers from a script.
type ghRunner interface {
	Run(ctx context.Context, c bexec.Command) (bexec.Result, error)
}

// resolvePullRequest checks, before a run starts, every reason belay could
// not open the pull request --pr promises.
//
// --pr is a promise about the end of a run, and the end of a run is the worst
// possible moment to discover that gh is not installed: the agent has been
// called, the budget has been spent, and the one thing the person asked for
// on top of all that cannot happen. So each condition is a blocker here,
// beside the plan's others, and `--dry-run` lists them without spending
// anything.
//
// want is whether --pr was given, and look is how gh is found; both are
// parameters so that neither this function's answer nor a test of it depends
// on a package-level flag or on what happens to be installed on the machine.
func (p *runPlan) resolvePullRequest(ctx context.Context, want bool, look func(string) (string, error)) {
	if !want {
		return
	}
	if !p.output.Branched() {
		p.block(
			"--pr asks belay to open a pull request, but this run edits your workspace in place: it makes no "+
				"commit and no branch, so there would be nothing to propose.",
			"set output.mode to \"branch\" or \"auto\" in belay.yaml, or drop --pr",
		)
		return
	}
	if _, err := look(ghProgram); err != nil {
		p.block(
			fmt.Sprintf("--pr asks belay to open a pull request, and it opens one through GitHub's own command "+
				"line, which is not on PATH: %v.", err),
			"install gh from https://cli.github.com, sign in with `gh auth login`, or drop --pr and "+
				"open the pull request yourself when the run finishes",
		)
		return
	}
	if p.repo == nil {
		return
	}
	remote, err := p.repo.Remote(ctx, p.output.BaseRef)
	if err != nil {
		p.block(
			fmt.Sprintf("--pr asks belay to open a pull request, and belay cannot tell where to push this run's "+
				"branch: %v.", err),
			"add a remote with `git remote add origin <url>`, or drop --pr",
		)
		return
	}
	p.remote = remote
}

// publishRun pushes a completed branch run's work and opens its draft pull
// request, and records the result on the view the report is built from.
//
// It is a step of afterCompletion, so it cannot change how the run is
// reported: the run's result is committed on its branch whether or not a
// pull request could be opened, and reporting a finished run as failed would
// tell a script to redo work that is already done. A failure is said on
// stderr and turned into the two commands a person runs themselves, which
// nextSteps prints.
func (p *runPlan) publishRun(ctx context.Context, u *ui, view *outcomeView) {
	out := view.output
	if out.Branch == "" {
		out = p.output
	}
	if !out.Branched() || !out.PullRequest || view.outcome.Status != state.RunStatusCompleted {
		return
	}

	view.remote = p.remote
	// The promotion's file count is what says whether there is anything to
	// propose, and a state.json that cannot be read is not "nothing
	// changed": treating it as zero would quietly skip the pull request a
	// person asked for. It is reported like any other publish failure, with
	// the commands to finish by hand.
	st, err := state.LoadState(view.layout.StatePath())
	if err != nil {
		u.note("belay: cannot read what run %s promoted, so it opened no pull request: %v", view.layout.RunID(), err)
		view.publishErr = err
		return
	}

	url, err := publish(ctx, u, publishRequest{
		output: out, goal: p.goal, runID: view.layout.RunID(),
		remote: p.remote, files: st.Promotion.Files, push: p.pushBranch, gh: p.ghRunner(),
	})
	if err != nil {
		u.note("belay: %v", err)
		view.publishErr = err
		return
	}
	view.pullRequest = url
}

// pushBranch sends a branch to a remote through the run's repository.
func (p *runPlan) pushBranch(ctx context.Context, remote, branch string) error {
	if p.repo == nil {
		return errors.New("belay has no repository to push from")
	}
	return p.repo.Push(ctx, remote, branch)
}

// ghRunner returns what gh commands are run through, which a test replaces.
func (p *runPlan) ghRunner() ghRunner {
	if p.gh != nil {
		return p.gh
	}
	return bexec.New(p.logger)
}

// publish pushes the run's branch and opens one draft pull request for it,
// and returns that pull request's URL.
//
// It asks whether one is already open before it creates anything. `gh pr
// create` does refuse a duplicate, but it refuses with an error, and a run
// that completed must not be reported as having failed because its work was
// already proposed. Asking is also what makes this safe to run again after a
// crash between the push and the create.
//
// It passes no base. decisions.md settled that belay does not choose one: gh
// uses the repository's default branch, or the merge base the person
// configured for this branch, which is the answer their own `gh pr create`
// would give.
//
// The pull request is a draft. belay wrote the change; a person decides
// whether it is ready to be reviewed, and a draft is the state that says so
// without anyone having to undo anything.
func publish(ctx context.Context, u *ui, req publishRequest) (string, error) {
	if req.files == 0 {
		u.say("  nothing to propose: %s changed no files, so belay opened no pull request", req.output.Branch)
		return "", nil
	}

	if url, err := existingPullRequest(ctx, req); err != nil {
		return "", err
	} else if url != "" {
		u.say("  %s already has a pull request: %s", req.output.Branch, url)
		return url, nil
	}

	if req.push != nil {
		if err := req.push(ctx, req.remote, req.output.Branch); err != nil {
			return "", fmt.Errorf("cannot push %s to %s: %w", req.output.Branch, req.remote, err)
		}
	}

	title, body := pullRequestText(req)
	res, err := req.gh.Run(ctx, ghCommand(req, bexec.Command{
		Args:  []string{"pr", "create", "--draft", "--head", req.output.Branch, "--title", title, "--body-file", "-"},
		Stdin: strings.NewReader(body),
	}))
	if err != nil {
		return "", fmt.Errorf("cannot open a pull request for %s: %w%s", req.output.Branch, err, ghDetail(res))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// existingPullRequest returns the URL of a pull request already open for the
// run's branch, or "" when there is none.
//
// It asks about every state, not only open ones: a closed pull request for
// this branch means somebody has already seen this work and decided, and
// opening a second one would put that decision back in front of them.
func existingPullRequest(ctx context.Context, req publishRequest) (string, error) {
	res, err := req.gh.Run(ctx, ghCommand(req, bexec.Command{
		Args: []string{"pr", "list", "--head", req.output.Branch, "--state", "all", "--limit", "1", "--json", "url,state"},
	}))
	if err != nil {
		return "", fmt.Errorf("cannot ask whether %s already has a pull request: %w%s", req.output.Branch, err, ghDetail(res))
	}
	var found []struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &found); err != nil {
		return "", fmt.Errorf("cannot read what gh said about %s: %w", req.output.Branch, err)
	}
	if len(found) == 0 {
		return "", nil
	}
	return found[0].URL, nil
}

// ghCommand fills in everything every gh command shares: the program, the
// repository to run in, and the environment.
//
// GH_TOKEN and GITHUB_TOKEN are passed through by name because internal/exec
// drops the parent environment by default, and they are how gh is
// authenticated in CI. They are named in SecretEnv so their values seed the
// redactor and never reach a journal or a log line. GH_PROMPT_DISABLED stops
// gh asking a question nobody is there to answer.
func ghCommand(req publishRequest, c bexec.Command) bexec.Command {
	c.Path = ghProgram
	c.Dir = req.output.Repo
	c.SecretEnv = []string{"GH_TOKEN", "GITHUB_TOKEN"}
	c.EnvAllow = []string{"GH_HOST", "GH_CONFIG_DIR", "XDG_CONFIG_HOME", "SSH_AUTH_SOCK"}
	c.ExtraEnv = map[string]string{"GH_PROMPT_DISABLED": "1", "GH_NO_UPDATE_NOTIFIER": "1"}
	return c
}

// ghDetail appends what gh printed on stderr, when it printed anything. gh's
// own messages say what is wrong -- not signed in, no repository, no
// permission -- far better than a wrapper could.
func ghDetail(res bexec.Result) string {
	if detail := strings.TrimSpace(res.Stderr); detail != "" {
		return ": " + detail
	}
	return ""
}

// pullRequestText is the title and body of the pull request belay opens.
//
// The body says what made the change and how to see what it did, because the
// first question anyone reviewing an agent's work asks is "what produced
// this?" and the second is "what else did it do?". It names the run, so the
// journal and the timeline can be found from the pull request.
func pullRequestText(req publishRequest) (title, body string) {
	title = strings.Join(strings.Fields(firstLine(req.goal)), " ")
	if title == "" {
		title = "belay run " + req.runID
	}
	if len(title) > subjectWidth {
		title = title[:subjectWidth-3] + "..."
	}

	var b strings.Builder
	if goal := strings.TrimSpace(req.goal); goal != "" {
		fmt.Fprintf(&b, "%s\n\n", goal)
	}
	fmt.Fprintf(&b, "Made by belay, run `%s`.\n\n", req.runID)
	fmt.Fprintf(&b, "- branch: `%s`, from `%s`\n", req.output.Branch, shortCommit(req.output.Base))
	fmt.Fprintf(&b, "- files changed: %d\n", req.files)
	fmt.Fprintf(&b, "- what it did, step by step: `belay timeline %s`\n\n", req.runID)
	b.WriteString("It is a draft: belay wrote the change, and whether it is ready to review is yours to say.\n")
	return title, b.String()
}

// subjectWidth is the longest title belay writes: the width GitHub and git's
// own tools show on one line.
const subjectWidth = 72

// firstLine returns everything before the first newline.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// lookGh finds the GitHub command line on PATH.
func lookGh(name string) (string, error) { return exec.LookPath(name) }
