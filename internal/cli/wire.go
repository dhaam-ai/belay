package cli

import (
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
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/linter"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/runner"
	"github.com/dhaam-ai/belay/internal/sonar/scanner"
	"github.com/dhaam-ai/belay/internal/state"
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

// defaultRoute is the path a standard run walks, mirroring nodes.Default.
//
// It is written out here rather than read back from the registry because a
// graph.Registry is a set of nodes and not an order — Registry.Names returns
// map keys. A person asking "where am I and what happens next" needs the
// order, so it is stated once, and TestDefaultRouteMatchesRegistry keeps this
// list and nodes.Default from drifting apart.
var defaultRoute = []string{
	graph.NodePlan, graph.NodeApprove, graph.NodeCode,
	graph.NodeWrite, graph.NodeTest, graph.NodeFix, graph.NodeReview,
}

// resolveRequest is what a caller knows before belay resolves anything: the
// directory to work in and the flags that were set.
type resolveRequest struct {
	workspace  string
	configPath string
	cassette   string
	logger     *slog.Logger
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

	agent    belay.AgentBackend
	runner   belay.TestRunner
	linter   belay.Linter
	reviewer belay.Reviewer
	registry *graph.Registry
	redact   func(string) string

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
	}
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
	p.resolveRegistry()

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
		p.block(
			"review.mode is \"ai\", and the agent-driven reviewer is not built yet.",
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

	backend, err := replay.New(mode, claude.New(p.logger), nil, replay.Provenance{
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
	if os.Getenv(anthropicKeyEnv) == "" && os.Getenv(claudeOAuthTokenEnv) == "" {
		p.warn("neither %s nor %s is set; belay will rely on however the claude command line is already signed in.",
			anthropicKeyEnv, claudeOAuthTokenEnv)
	}
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

// resolveRegistry builds the node graph the dispatcher walks.
func (p *runPlan) resolveRegistry() {
	registry, err := nodes.Default(p.config)
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
