package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/dhaam-ai/belay/internal/policy"
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
	// override changes the loaded configuration before anything is resolved
	// from it, for a command whose runs differ from belay.yaml in ways it
	// decides itself: `belay fork` always edits its copy in place, and sends
	// its node the model --model names. Nil changes nothing.
	override func(*config.Config)
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
	// key names a blocker that is re-derived rather than accumulated: one
	// that depends on a pin, which a resume settles from the run's manifest
	// after resolve has already judged belay.yaml. Re-deriving one first
	// removes every blocker with its key. Empty for every other blocker.
	key string
}

// fanoutReviewKey is the key of the blocker refusing a fanout over a review
// mode its candidates cannot share; see checkFanoutReview.
const fanoutReviewKey = "fanout-review"

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
	// policy is the guardrails this run is held to, or nil for a run that
	// declares none. It is a pin, not a reading of the file: resolvePolicy
	// builds it for a new run from belay.yaml, createRun completes it with
	// the baseline it surveys, and bindPolicy replaces it on a resume with
	// what the run's own manifest recorded.
	policy *state.PolicyPin
	// fanout is this run's fanout, or nil for a run that does not fan out.
	// It is a pin, not a reading of the file, exactly as policy is:
	// resolveFanout builds it for a new run from belay.yaml, createRun
	// writes it into the manifest, and bindFanout replaces it on a resume
	// with what the run's own manifest recorded.
	fanout *state.FanoutPin
	// goal is what the person asked for, which a pull request's title and
	// body are written from. startRun sets it; a resume reads it from the
	// run's manifest.
	goal string

	agent belay.AgentBackend
	// models is what each node that calls a model would be sent, in route
	// order, with what that model lists at. modelNote is why none of them
	// has a rate, when the backend prices nothing at all.
	models    []nodeModel
	modelNote string

	runner   belay.TestRunner
	linter   belay.Linter
	reviewer belay.Reviewer
	registry *graph.Registry
	// route is the order registry is walked in, for showing a person; see
	// resolveRegistry.
	route []string
	// candidates is the graph each candidate of a fanout run is driven
	// over, and candidateRoute the order it walks it in. Both are nil for a
	// run that does not fan out; resolveRegistry sets them with the rest.
	candidates     *graph.Registry
	candidateRoute []string
	redact         func(string) string

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
		// These three are what make a fanout run's candidates run at all.
		// Without a candidate registry the dispatcher builds no candidate
		// runner, RunContext.CandidateRuns is nil, and the fanout node
		// refuses with graph.ErrNoCandidateRunner. A run that does not fan
		// out passes nil and gets none.
		CandidateRegistry: p.candidates,
		CandidateAgent:    p.candidateAgent(),
		// Read from belay.yaml as it is now, on every invocation, and never
		// from the run's pin: lowering it and resuming is how a person backs
		// off a rate limit or a machine that cannot take the load.
		Concurrency: p.config.Fanout.Concurrency,
	}
}

// candidateAgent is the backend each candidate of a fanout run is given, by
// candidate id, or nil for a run that does not fan out.
//
// Every backend belay builds is a replay.Backend -- in live mode a
// pass-through, in record and replay mode the cassette -- and each candidate
// gets Scoped(id) of it. That matters only for a cassette, and there it is
// the whole difference: N candidates given one goal ask the same question, a
// fingerprint deliberately leaves the workspace out, and an unscoped cassette
// would hand whichever candidate asked first whichever answer was recorded
// first. Scoped by the candidate's id, each records and replays its own
// conversation in whatever order the candidates run. A live pass-through
// ignores the scope.
//
// A backend that is not a replay.Backend is shared as it is, which
// pkg/belay's contract already makes safe: Invoke must be safe for
// concurrent use.
func (p *runPlan) candidateAgent() func(id string) belay.AgentBackend {
	if p.candidates == nil {
		return nil
	}
	cassette, ok := p.agent.(*replay.Backend)
	if !ok {
		return nil
	}
	return func(id string) belay.AgentBackend { return cassette.Scoped(id) }
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
// root it cannot create — is not an error here, because most runs never fan
// out and refusing to start one over an isolator it will not use would stop
// runs that are fine. The failure is logged and the run proceeds without
// one. A run that does fan out then cannot make any candidate's workspace:
// every candidate reports that it has no isolator, and the fanout pauses
// with that reason rather than settling, so `belay resume` tries again once
// the cause is fixed.
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
	// Before anything is resolved from it, and held to belay.yaml's own
	// rules: a model routed to a node that asks for none is refused here
	// exactly as it would be in the file.
	if req.override != nil {
		req.override(&cfg)
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("that configuration cannot be used:\n  %w", err)
		}
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
	// The policy comes before the registry, for the reason the output does:
	// whether a run has a gate follows from it. So does the fanout: whether
	// the run's own walk holds the work or hands it to candidates.
	p.resolvePolicy()
	p.resolveFanout()
	p.resolveRegistry()
	p.resolveModels()

	return p, nil
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

// resolvePolicy settles the guardrails a NEW run would be held to, from
// belay.yaml as it stands now.
//
// It records the rules and not the baseline. The baseline is a survey of the
// tree the run's nodes will work in, and for a branch run that tree does not
// exist until createRun has made the worktree, so createRun completes the pin
// (see pinPolicy). What this decides is the part the registry and the dry run
// need: whether this run has a gate at all.
//
// A policy that asks for nothing is no policy. Pinning an empty rule set
// would put a gate on the route of a run with nothing to judge, and would
// make every run declare a capability that older belays would then refuse,
// for guardrails nobody configured.
func (p *runPlan) resolvePolicy() {
	rules := policy.From(p.config.Policy)
	if rules.Empty() {
		p.policy = nil
		return
	}
	p.policy = &state.PolicyPin{Policy: rules}
}

// resolveFanout settles whether a NEW run fans out, from belay.yaml as it
// stands now, and with what shape: how many candidates, and which strategy
// picks the winner. createRun pins exactly this in the manifest, and a
// resume replaces it with that pin (bindFanout), so a run never changes
// shape because the file did.
//
// fanout.concurrency is deliberately not part of it: it is read live on
// every invocation, because lowering it on a resume is how a person backs off
// a rate limit (see config.Fanout.Concurrency).
func (p *runPlan) resolveFanout() {
	if !p.config.Fanout.Enabled {
		p.fanout = nil
		return
	}
	p.fanout = &state.FanoutPin{
		Candidates: p.config.Fanout.Candidates,
		Select:     string(p.config.Fanout.Select),
	}
}

// resolveRegistry builds the node graph the dispatcher walks, and the route
// belay shows a person, together. Whatever options nodes.Default is given,
// nodes.Route must be given too, or belay shows one graph and walks another.
//
// It reads p.output, p.policy and p.fanout, never p.config.Output,
// p.config.Policy or p.config.Fanout, and that is the whole rule
// internal/nodes states from the other side: whether a run has the promote
// node, the policy gate or a fanout is pinned in its manifest when the run is
// created, and belay.yaml may have changed by the time the run is resumed.
// resolveOutput, resolvePolicy and resolveFanout set them for a new run from
// belay.yaml; bindOutput, bindPolicy and bindFanout set them for a resume
// from the chosen run's manifest, and call this again. It never refuses a
// run: a pin this plan cannot honour is resumePreflight's to report.
//
// The route is set even when the registry cannot be built, so that a dry run
// refusing to start still shows the route it would have taken.
func (p *runPlan) resolveRegistry() {
	var opts []nodes.Option
	if p.output.Branched() {
		opts = append(opts, nodes.WithBranchOutput())
	}
	if p.policy != nil {
		opts = append(opts, nodes.WithPolicyGate())
	}
	if p.fanout != nil {
		opts = append(opts, nodes.WithFanout())
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
	p.resolveCandidates()
	p.checkFanoutReview()
}

// checkFanoutReview refuses a run that fans out into a quality gate its
// candidates cannot each run: SonarQube, or the AI review that asks
// SonarQube for its verdict.
//
// Both name the SonarQube project after the directory they review, and a
// candidate's workspace is called cand-01, cand-02 and so on -- in every run,
// in every repository. So every candidate would analyse into projects shared
// with every other run's candidates, and each quality gate would read results
// another candidate or another run wrote; the project's history, which is
// what SonarQube judges new code against, would be a jumble of unrelated
// changes. That is not a verdict about this candidate's code at all. Lint
// review runs in the candidate's own directory and reports on it alone, so
// it is unaffected -- and since golangci-lint is now told to wait for its
// machine-wide lock, several of them can run at once.
//
// It judges the fanout this run walks -- the pin, on a resume -- against the
// review mode belay.yaml gives now, which is read live. resolveRegistry calls
// it on every rebuild, so it first removes what an earlier call found: a
// resume settles the fanout from the manifest after resolve has judged the
// file's, and a run started without a fanout must not be refused for one it
// does not have.
func (p *runPlan) checkFanoutReview() {
	p.blockers = slices.DeleteFunc(p.blockers, func(b blocker) bool { return b.key == fanoutReviewKey })
	if p.fanout == nil {
		return
	}
	mode := p.config.Review.Mode
	if mode != config.ReviewModeSonar && mode != config.ReviewModeAI {
		return
	}
	p.blockers = append(p.blockers, blocker{
		problem: fmt.Sprintf("this run fans out, and review.mode is %q: belay names the SonarQube project after "+
			"the directory it reviews, so every candidate would share projects called cand-01, cand-02 and so on "+
			"with every other run's candidates, and its quality gate would read results it did not produce.", mode),
		fix: "set review.mode to lint for a run that fans out; SonarQube project keys are not yet scoped per " +
			"candidate. A run not yet started can instead set fanout.enabled to false.",
		key: fanoutReviewKey,
	})
}

// resolveCandidates builds the graph a fanout run's candidates are driven
// over, from the same pins as the run's own: the policy gate is in it when
// the run has a policy, because a fanout run's change is made by its
// candidates and has to be judged there. Branch output is not: a candidate's
// passing gate ends the candidate, and promoting the winner is the parent's.
//
// It clears both fields for a run that does not fan out, so a resume that
// rebinds a pin never leaves the previous plan's candidates behind.
func (p *runPlan) resolveCandidates() {
	p.candidates, p.candidateRoute = nil, nil
	if p.fanout == nil {
		return
	}
	var opts []nodes.Option
	if p.policy != nil {
		opts = append(opts, nodes.WithPolicyGate())
	}
	p.candidateRoute = nodes.CandidateRoute(opts...)
	candidates, err := nodes.Candidate(opts...)
	if err != nil {
		p.block(
			fmt.Sprintf("belay cannot assemble the graph its candidates run: %v", err),
			"this is a bug in belay; please report it",
		)
		return
	}
	p.candidates = candidates
}

// nodeModel is one node of the route, the model it would be sent, and what
// that model lists at.
type nodeModel struct {
	// node is the node's name and model is what [config.Agent.ModelFor]
	// routes it to.
	node, model string
	// rate is the model's list price, valid only when priced is true.
	rate   belay.ModelRate
	priced bool
	// note is why this row has no rate, when the backend priced every other
	// model but not this one.
	note string
}

// resolveModels works out which model each node would be sent and asks the
// backend what each one costs, so that a model belay cannot price stops the
// run here rather than in the middle of it.
//
// It walks the route, not agent.models, and that is the whole difference
// between a check and an obstruction. A model routed to a node this run never
// reaches costs nothing, and refusing to start over it would be belay
// enforcing a rule about a step that is not going to happen. Walking the
// route also means every node is checked, including the ones sent agent.model
// by default: before routing there was one model to get wrong, and now there
// are as many as there are nodes that spend.
//
// Pricing is the backend's answer, never belay's. A backend that does not
// implement belay.ModelPricer, or one that reports belay.ErrUnsupported, has
// not refused the model: it has declined to say, which is what a replayed run
// does because it spends nothing. Those runs show models with no rates and
// are not blocked. Only a backend that answers with an error of its own
// blocks, and then once per distinct model, because three nodes sharing one
// bad agent.model is one thing to fix, not three.
func (p *runPlan) resolveModels() {
	pricer, _ := p.agent.(belay.ModelPricer)
	routable := config.RoutableNodes()
	blocked := map[string]bool{}

	// A fanout run's own walk calls one model, the plan's; its candidates
	// call the rest, once each. Walking only the run's route would wave a
	// bad agent.models.code through here and fail it inside every
	// candidate instead, so the candidates' walk is checked too. The
	// combined walk reaches each routable node once, in the order a run
	// reaches it, whether or not the run fans out.
	for _, node := range slices.Concat(p.route, p.candidateRoute) {
		if !slices.Contains(routable, node) {
			continue
		}
		entry := nodeModel{node: node, model: p.config.Agent.ModelFor(node)}
		switch rate, err := p.listPrice(pricer, entry.model); {
		case pricer == nil:
			// Nothing to ask: resolveAgent has already blocked, or the
			// backend prices nothing.
		case err == nil:
			entry.rate, entry.priced = rate, true
		case errors.Is(err, belay.ErrUnsupported):
			p.modelNote = "this run answers from a recording, so it calls no model and spends nothing"
		default:
			entry.note = "belay does not know this model"
			if !blocked[entry.model] {
				blocked[entry.model] = true
				p.blockModel(node, entry.model, err)
			}
		}
		p.models = append(p.models, entry)
	}
}

// listPrice asks pricer about model, tolerating a nil pricer so that
// resolveModels reads as one switch rather than a nest of them.
func (p *runPlan) listPrice(pricer belay.ModelPricer, model string) (belay.ModelRate, error) {
	if pricer == nil {
		return belay.ModelRate{}, nil
	}
	return pricer.ListPrice(model)
}

// blockModel refuses a run over a model the backend will not price, naming
// the setting that holds it.
//
// Naming the setting is the point. Routing gives a run four places a model
// can come from -- agent.model and one key per node -- so "the model is
// wrong" would leave a person reading their whole belay.yaml to find which.
// The backend's own words are quoted rather than paraphrased: whether a model
// exists is its knowledge, not belay's, and belay guessing at the reason is
// how a wrong explanation gets printed confidently.
func (p *runPlan) blockModel(node, model string, err error) {
	setting := "agent.model"
	if p.config.Agent.Models[node] != "" {
		setting = "agent.models." + node
	}
	backend := p.config.Agent.Backend
	if p.agent != nil {
		backend = p.agent.Name()
	}
	fix := fmt.Sprintf("set %s to a model %s can run", setting, backend)
	if setting != "agent.model" {
		fix += ", or remove it so " + node + " uses agent.model"
	}
	fix += "; docs/config.md lists what agent.model accepts"
	p.block(fmt.Sprintf("%s names the model %q, which the %s backend cannot price, so belay could not hold "+
		"this run to budget.max_usd: %v", setting, model, backend, err), fix)
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
