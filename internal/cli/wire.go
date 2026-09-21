package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dhaam-ai/belay/internal/agent/claude"
	"github.com/dhaam-ai/belay/internal/agent/replay"
	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/detect"
	"github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/internal/git"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/isolate/dircopy"
	"github.com/dhaam-ai/belay/internal/isolate/worktree"
	"github.com/dhaam-ai/belay/internal/linter"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/runner"
	"github.com/dhaam-ai/belay/internal/sonar/scanner"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/internal/vcs"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Environment variables belay reads for credentials. They are never read from
// a configuration file — internal/config rejects a file that names one — and
// belay never prints their values: they are here so that the redactor can be
// seeded with them, and so a plan can report whether they are set.
const (
	anthropicKeyEnv     = "ANTHROPIC_API_KEY"
	claudeOAuthTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN" // #nosec G101 -- a variable name, not a credential.
	sonarTokenEnv       = "SONAR_TOKEN"
)

// defaultCassetteName is where a recorded conversation lives when --cassette
// says nothing. It sits beside the run directories rather than inside one,
// because a cassette recorded by one run is replayed by a different run.
const defaultCassetteName = "cassette.json"

// resolveRequest is what a caller knows before belay resolves anything: the
// directory to work in and the flags that were set.
type resolveRequest struct {
	workspace  string
	configPath string
	cassette   string
	logger     *slog.Logger
	// ctx bounds the commands resolving runs, which is git and nothing
	// else. It is optional so that a caller with no context of its own —
	// every test that only wants a plan — still gets one.
	context context.Context
}

// ctx returns the request's context, or a background one.
func (r resolveRequest) ctx() context.Context {
	if r.context != nil {
		return r.context
	}
	return context.Background()
}

// blocker is one reason belay would refuse to start a run, paired with the
// change that clears it.
//
// A blocker is collected rather than returned as an error so that --dry-run
// can report every one of them at once. Discovering three problems one run at
// a time is three rounds of frustration; discovering them together is one.
type blocker struct {
	problem string
	fix     string
}

// runPlan is one resolved run: every decision belay made and every adapter it
// would use, assembled without creating a file or spending a cent.
//
// Resolving and starting are deliberately separate. --dry-run needs
// everything a real run decides and none of what it creates, and the only way
// to guarantee the two agree is for the dry run to be the real run's first
// half rather than a second implementation of it.
type runPlan struct {
	workspace     string
	configSources []string
	config        config.Config
	projects      []detect.Project
	cassettePath  string

	// output is how this run delivers its result, as resolveOutput settled
	// it, and outputReason is why. repo is the repository a branch run
	// commits to, and nil otherwise.
	output       state.Output
	outputReason string
	repo         *git.Repo
	// remote is where a --pr run pushes its branch, resolved before the run
	// starts so a missing remote is a blocker rather than a surprise at the
	// end. gh runs the GitHub command line, and is nil outside a test.
	remote string
	gh     ghRunner
	// goal is what the person asked for, which a pull request's title and
	// body are written from. startRun sets it; a resume reads it from the
	// run's manifest.
	goal string

	agent    belay.AgentBackend
	runner   belay.TestRunner
	linter   belay.Linter
	reviewer belay.Reviewer
	registry *graph.Registry
	// route is the order registry is walked in, for showing a person; see
	// resolveRegistry.
	route  []string
	redact func(string) string

	// recorder is the record-mode wrapper whose cassette must be saved when
	// the run ends. It is nil in every other mode.
	recorder *replay.Backend

	blockers []blocker
	warnings []string

	logger *slog.Logger
}

// block records a reason belay would refuse to start.
func (p *runPlan) block(problem, fix string) {
	p.blockers = append(p.blockers, blocker{problem: problem, fix: fix})
}

// warn records something a person should know that does not stop the run.
func (p *runPlan) warn(format string, a ...any) {
	p.warnings = append(p.warnings, fmt.Sprintf(format, a...))
}

// options assembles the dispatcher's Options for this plan.
//
// It is the only place graph.Options is built, which is what stops `belay
// run` and `belay resume` from drifting into two differently wired runs of
// the same graph.
func (p *runPlan) options(store *state.Store) graph.Options {
	return graph.Options{
		Store:    store,
		Registry: p.registry,
		Config:   p.config,
		Logger:   p.logger,
		Redact:   p.redact,
		Agent:    p.agent,
		Runner:   p.runner,
		Linter:   p.linter,
		Reviewer: p.reviewer,
		Isolator: p.isolator(store),
		VCS:      p.vcs(),
		// Before each node the dispatcher snapshots the blackboard; this puts
		// a fingerprint of the workspace beside it, so a fork of the run can
		// tell whether the files have changed since the visit it re-runs.
		Fingerprint: surveyWorkspace,
	}
}

// vcs is the repository the promote node commits to, and nil for an in-place
// run, which commits nothing. Nothing else in belay makes one, so a branch
// run whose options carried none would reach promote and refuse there, after
// paying for every step before it.
func (p *runPlan) vcs() vcs.Repository {
	if !p.output.Branched() || p.repo == nil {
		// A typed nil in an interface is not nil, and promote checks for
		// nil, so an in-place run must get an untyped one.
		return nil
	}
	return p.repo
}

// isolator is what a fanout candidate's workspace is made with: a git
// worktree on a branch of its own for a branch run, a directory copy
// otherwise (see resolveOutput's isolatorName for how "auto" is read).
//
// It is built per store, because both isolators are rooted at one run's
// candidates directory. A store it cannot be built for — no run yet, or a
// root it cannot create — is not an error here: fanout is on no route belay
// walks today, and refusing to start a run over an isolator nothing reaches
// would stop runs that are fine. The failure is logged and the run proceeds
// without one, which fanout itself refuses.
func (p *runPlan) isolator(store *state.Store) belay.Isolator {
	if store == nil {
		return nil
	}
	root := store.Layout().CandidatesDir()
	var (
		iso belay.Isolator
		err error
	)
	switch p.isolatorName() {
	case config.IsolatorWorktree:
		if p.repo == nil {
			return nil
		}
		iso, err = worktree.New(p.repo, root, runBranch(store.Layout().RunID()), worktree.WithLogger(p.logger))
	default:
		iso, err = dircopy.New(root, dircopy.WithLogger(p.logger))
	}
	if err != nil {
		p.logger.Error("belay could not build the isolator candidates would be made with",
			slog.String("isolator", string(p.isolatorName())), slog.Any("error", err))
		return nil
	}
	return iso
}

// surveyWorkspace is graph.Options.Fingerprint for every run belay starts or
// resumes: dircopy's survey of the tree a copy of the workspace would hold,
// with a copy's default skip list, so belay's own writes under .belay — the
// run's journal among them — never read as a change to the person's files.
func surveyWorkspace(ctx context.Context, dir string) (string, error) {
	survey, err := dircopy.SurveyTree(ctx, dir, dircopy.DefaultSkip())
	if err != nil {
		return "", err
	}
	return survey.Digest, nil
}

// gateName describes the quality gate this plan would run, in words.
func (p *runPlan) gateName() string {
	switch {
	case p.linter != nil:
		return p.linter.Name()
	case p.reviewer != nil:
		return "SonarQube (" + p.config.Review.Sonar.Runner + ")"
	default:
		return "none"
	}
}

// runnerName describes the test command this plan would run, in words.
func (p *runPlan) runnerName() string {
	if p.runner == nil {
		return "none"
	}
	return p.runner.Name()
}

// resolve turns a request and a directory on disk into a plan for a run.
//
// It reads the filesystem and the environment and writes neither. Every
// problem that would stop a real run is collected in runPlan.blockers instead
// of being returned, so that a caller can report all of them; the error
// return is reserved for the two things that make planning itself impossible
// — an unusable workspace and an unreadable configuration.
func resolve(req resolveRequest) (*runPlan, error) {
	workspace, err := resolveWorkspace(req.workspace)
	if err != nil {
		return nil, err
	}

	cfg, err := config.Load(config.Options{Path: req.configPath, Dir: workspace})
	if err != nil {
		return nil, fmt.Errorf("that configuration cannot be used:\n  %w", err)
	}

	logger := req.logger
	if logger == nil {
		logger = slog.Default()
	}

	p := &runPlan{
		workspace:     workspace,
		configSources: configSources(workspace, req.configPath),
		config:        cfg,
		cassettePath:  cassettePath(workspace, req.cassette),
		redact:        newRedactor().Redact,
		logger:        logger,
	}

	if projects, err := detect.Detect(workspace); err == nil {
		p.projects = projects
	} else {
		p.warn("belay could not survey %s to work out what kind of project it is: %v", workspace, err)
	}

	execer := exec.New(logger)
	p.resolveTestRunner(execer)
	p.resolveGate(execer)
	p.resolveAgent()
	// The output comes before the registry, because the registry follows
	// from it: a branch run has a promote node and an in-place run does not.
	p.resolveOutput(req.ctx())
	p.resolvePullRequest(req.ctx(), runFlags.pullRequest, lookGh)
	p.resolveRegistry()
	p.warnAboutFanout()

	return p, nil
}

// warnAboutFanout says that fanout.enabled changes nothing yet.
//
// It is a warning rather than a blocker: the setting is valid, the loader
// checks it, and a run with it on is exactly the run it would be with it off.
// That is the problem. Somebody who turned it on is expecting N attempts at
// their goal and watching what N attempts would cost, and belay would take
// one attempt and say nothing about it. Silence is only safe for a setting
// that does what it says.
func (p *runPlan) warnAboutFanout() {
	if !p.config.Fanout.Enabled {
		return
	}
	p.warn("fanout.enabled is true, but nothing runs the candidates yet: no step of the graph leads to "+
		"the fanout node, so this run takes the ordinary route once and fanout.candidates (%d) changes "+
		"nothing it does or spends. ADR-0019 is reserved for the decision.", p.config.Fanout.Candidates)
}

// resolveWorkspace turns the --workspace flag into the absolute path the rest
// of belay requires, and refuses anything that is not a directory.
//
// state.NewLayout rejects a relative workspace outright, for a good reason: a
// relative path silently resolves against whatever directory belay happens to
// be running in, which would point an agent with write access at the wrong
// tree. Converting once, here, is what keeps that check from firing halfway
// through a run instead of before it.
func resolveWorkspace(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("belay cannot work out the full path of %q: %w", dir, err)
	}

	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("there is no directory at %s, so belay has nothing to work on", abs)
	case err != nil:
		return "", fmt.Errorf("belay cannot read %s: %w", abs, err)
	case !info.IsDir():
		return "", fmt.Errorf("%s is a file, not a directory; belay works on a whole repository", abs)
	}
	return abs, nil
}

// newRedactor builds the run's redactor.
//
// This is a security requirement, not a nicety. graph.Options.Redact defaults
// to a no-op, and the dispatcher composes a journal note out of an adapter's
// own error string — which can quote a command line, and a command line can
// quote a token. Seeding the redactor with the credentials belay hands to its
// adapters means such a value is replaced by a label on its way into a
// file that people commit, paste into issues and read over each other's
// shoulders.
func newRedactor() *exec.Redactor {
	return exec.NewRedactor(
		exec.Secret{Label: anthropicKeyEnv, Value: os.Getenv(anthropicKeyEnv)},
		exec.Secret{Label: claudeOAuthTokenEnv, Value: os.Getenv(claudeOAuthTokenEnv)},
		exec.Secret{Label: sonarTokenEnv, Value: os.Getenv(sonarTokenEnv)},
	)
}

// cassettePath resolves where a recorded conversation is read from or written
// to. An explicit --cassette wins; otherwise it sits under the workspace's
// .belay directory, next to the runs rather than inside one.
func cassettePath(workspace, explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		if abs, err := filepath.Abs(explicit); err == nil {
			return abs
		}
		return explicit
	}
	return filepath.Join(workspace, ".belay", defaultCassetteName)
}

// configSources lists the configuration files belay actually read, in the
// order it merged them, so that a plan can say where a setting came from.
//
// It stats the same paths internal/config's documented layering describes,
// because config reports the merged result and not its provenance. This is
// display only: a mistake here misnames a file in a report, it cannot change
// a single setting.
func configSources(workspace, explicit string) []string {
	var found []string
	add := func(path string) {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			found = append(found, path)
		}
	}
	if home := xdgConfigHome(); home != "" {
		add(filepath.Join(home, filepath.FromSlash(config.UserFile)))
	}
	add(filepath.Join(workspace, config.ProjectFile))
	if explicit != "" {
		// An explicit --config must exist; config.Load already failed if it
		// did not, so it is reported without a stat.
		found = append(found, explicit)
	}
	return found
}

// xdgConfigHome mirrors internal/config's own lookup: $XDG_CONFIG_HOME, or
// $HOME/.config as the XDG Base Directory Specification requires.
func xdgConfigHome() string {
	if env := os.Getenv("XDG_CONFIG_HOME"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config")
}

// resolveTestRunner picks the command that proves the change works.
//
// A run with no runner is not merely degraded: the test node would fail after
// the agent has already planned, coded and written, which is the most
// expensive possible moment to discover a setup problem. It is a blocker.
func (p *runPlan) resolveTestRunner(execer *exec.Runner) {
	r, err := runner.Select(p.config.Test, p.workspace, execer, p.logger)
	if err == nil {
		p.runner = r
		return
	}
	switch {
	case errors.Is(err, runner.ErrNoProject):
		p.block(
			fmt.Sprintf("belay found no Go, Node or Python project in %s, so it has no way to test its own work.", p.workspace),
			"point --workspace at the repository, or set test.runner and test.custom_cmd in belay.yaml",
		)
	case errors.Is(err, runner.ErrNoCommand):
		p.block(
			"belay.yaml sets test.runner to \"custom\" but leaves test.custom_cmd empty, so there is no command to run.",
			"set test.custom_cmd to the command that runs your tests",
		)
	default:
		p.block(
			fmt.Sprintf("belay cannot use test.runner %q: %v", p.config.Test.Runner, err),
			"set test.runner to one of: auto, go, node, python, custom",
		)
	}
}

// resolveGate picks the quality gate that decides whether the work is good
// enough to finish, following review.mode.
func (p *runPlan) resolveGate(execer *exec.Runner) {
	switch p.config.Review.Mode {
	case config.ReviewModeLint:
		p.linter = pickLinter(p.workspace, p.config, execer, p.logger)
		if p.linter == nil {
			p.block(
				fmt.Sprintf("review.mode is \"lint\", but belay recognises no Go, Node or Python project in %s to lint.", p.workspace),
				"point --workspace at the repository, or set review.mode to \"sonar\" in belay.yaml",
			)
		}
	case config.ReviewModeSonar:
		p.reviewer = newScanner(p.config, execer, p.logger)
		if os.Getenv(sonarTokenEnv) == "" {
			p.warn("%s is not set, so the SonarQube scan will not be able to authenticate.", sonarTokenEnv)
		}
	case config.ReviewModeAI:
		// docs/config.md describes the mode and this refusal; the two say
		// the same thing. The reviewer exists (internal/nodes/aireview);
		// what is missing is the wiring that would construct it here.
		p.block(
			"review.mode is \"ai\": belay would ask SonarQube for its verdict through the Sonar MCP server, but that reviewer is not wired in yet.",
			"set review.mode to \"lint\" or \"sonar\" in belay.yaml",
		)
	default:
		p.block(
			fmt.Sprintf("belay does not know a review.mode called %q.", p.config.Review.Mode),
			"set review.mode to one of: lint, sonar",
		)
	}
}

// pickLinter returns the linter for the workspace's ecosystem, or nil when
// belay recognises none. The order — Go, Node, Python — matches
// runner.Select's, so a repository that is two things at once gets a gate
// from the same ecosystem as its tests.
func pickLinter(dir string, cfg config.Config, execer *exec.Runner, logger *slog.Logger) belay.Linter {
	opts := []linter.Option{linter.WithRunner(execer), linter.WithLogger(logger)}
	if failOn, err := belay.ParseSeverity(string(cfg.Review.FailOn)); err == nil {
		opts = append(opts, linter.WithFailOn(failOn))
	}
	for _, l := range []belay.Linter{
		linter.NewGolangCI(opts...),
		linter.NewESLint(opts...),
		linter.NewRuff(opts...),
	} {
		if l.Detect(dir) {
			return l
		}
	}
	return nil
}

// newScanner builds the SonarQube reviewer from review.sonar.
func newScanner(cfg config.Config, execer *exec.Runner, logger *slog.Logger) *scanner.Scanner {
	return scanner.New(
		scanner.WithExecer(execer),
		scanner.WithLogger(logger),
		scanner.WithRunner(scanner.Runner(cfg.Review.Sonar.Runner)),
		scanner.WithImage(cfg.Review.Sonar.Image),
		scanner.WithQualityGateWait(cfg.Review.Sonar.QualityGateWait),
		scanner.WithQualityGateTimeout(time.Duration(cfg.Review.Sonar.QualityGateTimeout)*time.Second),
	)
}

// resolveAgent builds the coding agent and wraps it for agent.mode.
//
// In replay mode no live backend is constructed at all. replay.Replay stores
// no inner backend, so there is no code path from a replayed run to a
// subprocess — the guarantee is structural rather than a flag that could be
// read the wrong way.
func (p *runPlan) resolveAgent() {
	if p.config.Agent.Backend != claude.BackendName {
		p.block(
			fmt.Sprintf("belay does not have a backend called %q.", p.config.Agent.Backend),
			fmt.Sprintf("set agent.backend to %q in belay.yaml", claude.BackendName),
		)
		return
	}

	mode := replay.Mode(p.config.Agent.Mode)
	if mode == replay.ModeReplay {
		p.resolveReplayAgent()
		return
	}

	backend, err := replay.New(mode, p.newLiveAgent(), nil, replay.Provenance{
		BelayVersion: Version,
		Model:        p.config.Agent.Model,
		RecordedAt:   time.Now().UTC(),
		Note:         "recorded by belay " + Version,
	})
	if err != nil {
		p.block(
			fmt.Sprintf("belay does not know an agent.mode called %q.", p.config.Agent.Mode),
			"set agent.mode to one of: live, record, replay",
		)
		return
	}

	p.agent = backend
	if mode == replay.ModeRecord {
		p.recorder = backend
	}
	for _, w := range agentWarnings(p.config) {
		p.warn("%s", w)
	}
}

// newLiveAgent builds the backend agent.backend names, the one that calls a
// real model. resolveAgent calls it only for a run that will call one, live
// or recording, which keeps a replayed run from constructing a backend at all.
//
// Choosing a backend by name is a switch over the backends belay ships rather
// than a table of constructors, because a backend can need checks of its own
// before it is used, and a switch keeps them beside the backend they check.
// It returns nil for a name it does not know. resolveAgent has already
// refused every other name before calling it.
func (p *runPlan) newLiveAgent() belay.AgentBackend {
	switch p.config.Agent.Backend {
	case claude.BackendName:
		return claude.New(p.logger)
	default:
		return nil
	}
}

// agentWarnings lists what a person should know, without being stopped by it,
// about the backend a live or recording run will call. It reads whether a
// credential is set, and never its value. Each warning is about one backend,
// so each is given only when cfg names that backend.
func agentWarnings(cfg config.Config) []string {
	var warnings []string
	signedOut := os.Getenv(anthropicKeyEnv) == "" && os.Getenv(claudeOAuthTokenEnv) == ""
	if cfg.Agent.Backend == claude.BackendName && signedOut {
		warnings = append(warnings, fmt.Sprintf(
			"neither %s nor %s is set; belay will rely on however the claude command line is already signed in.",
			anthropicKeyEnv, claudeOAuthTokenEnv))
	}
	return warnings
}

// resolveReplayAgent loads the cassette a replayed run answers from.
func (p *runPlan) resolveReplayAgent() {
	cassette, err := replay.LoadCassette(p.cassettePath)
	if err != nil {
		p.block(
			fmt.Sprintf("agent.mode is \"replay\", and belay cannot read the recorded conversation at %s: %v", p.cassettePath, err),
			"record one first with agent.mode \"record\", or pass --cassette with the path to an existing recording",
		)
		return
	}
	backend, err := replay.Replay(cassette)
	if err != nil {
		p.block(
			fmt.Sprintf("the recording at %s cannot be replayed: %v", p.cassettePath, err),
			"re-record it with this version of belay",
		)
		return
	}
	p.agent = backend
}

// resolveRegistry builds the node graph the dispatcher walks, and the route
// belay shows a person, together. Whatever options nodes.Default is given,
// nodes.Route must be given too, or belay shows one graph and walks another.
//
// It reads p.output, never p.config.Output, and that is the whole rule
// internal/nodes states from the other side: whether a run has the promote
// node is pinned in its manifest when the run is created, and belay.yaml may
// have changed by the time the run is resumed. resolveOutput sets p.output
// for a new run from belay.yaml; bindOutput sets it for a resume from the
// chosen run's manifest, and calls this again. It never refuses a run: a pin
// this plan cannot honour is resumePreflight's to report.
//
// The route is set even when the registry cannot be built, so that a dry run
// refusing to start still shows the route it would have taken.
func (p *runPlan) resolveRegistry() {
	var opts []nodes.Option
	if p.output.Branched() {
		opts = append(opts, nodes.WithBranchOutput())
	}
	p.route = nodes.Route(p.config, opts...)
	registry, err := nodes.Default(p.config, opts...)
	if err != nil {
		p.block(
			fmt.Sprintf("belay cannot assemble its own graph: %v", err),
			"this is a bug in belay; please report it",
		)
		return
	}
	p.registry = registry
}

// saveCassette persists a record-mode run's conversation, and reports what it
// wrote so the run can say where it went. It does nothing in any other mode.
func (p *runPlan) saveCassette() (string, error) {
	if p.recorder == nil {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(p.cassettePath), 0o750); err != nil {
		return "", fmt.Errorf("belay cannot create the directory for %s: %w", p.cassettePath, err)
	}
	cassette := p.recorder.Cassette()
	if err := cassette.Save(p.cassettePath); err != nil {
		return "", fmt.Errorf("belay cannot write the recording to %s: %w", p.cassettePath, err)
	}
	return p.cassettePath, nil
}
