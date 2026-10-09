//go:build fixrate

// Command swebench runs belay's SWE-bench Verified corpus: both arms --
// belay's graph and the same backend called once with no graph -- over the
// pinned dataset, scored by SWE-bench's official harness, resumable at any
// point. test/fixrate/README.md's runbook says how to run it and what it
// costs; this comment says what it guarantees.
//
// # Nothing is spent by accident
//
// It has no default mode. A live run is refused unless -confirm carries
// fixrate.ConfirmLiveSpend typed out in full and -max-usd names a total cap,
// and both are checked before anything is asked of the machine. Then, still
// before a byte is downloaded or a cent spent, it checks the host the run
// needs: an x86_64 machine (the official instance images are built for
// linux/amd64), an API credential for Claude Code, a running Docker daemon,
// swebench at exactly the version the scoring was written against, the
// claude, ruff and git commands, and that every model the configuration
// routes to is one the backend can price -- a model it cannot price is a
// budget it cannot enforce.
//
// # The model comes from configuration
//
// -config names the belay.yaml both arms run under, and only that file: no
// project or user configuration is merged in, so the configuration a corpus
// pins is the one its owner named. agent.model in it is what both arms run
// on (claude-sonnet-5 is the roadmap's choice); nothing here names a model.
//
// # Why this is a separate command
//
// It constructs the live backend, internal/agent/claude. test/fixrate never
// does -- that package's promise is that it has no code path able to start
// a paid agent -- so the decision to spend lives here, in the caller, as it
// always has.
//
// # The free mode
//
// -mode replay runs the same pipeline on the same machine with a scripted
// stand-in for the agent that applies each instance's own gold patch and
// spends nothing. Every cell should then resolve, so anything that does not
// is a fault in the pipeline -- an image that will not start, a test command
// that does not run, a harness that cannot grade -- found before a live run
// pays for it. Its numbers measure the harness, and its report says so.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/dhaam-ai/belay/internal/agent/claude"
	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
	"github.com/dhaam-ai/belay/test/fixrate"
)

// Exit statuses.
const (
	// exitComplete: every cell finished and was scored.
	exitComplete = 0
	// exitError: something went wrong that running again will not mend
	// by itself; stderr says what.
	exitError = 1
	// exitRefused: the invocation was refused before anything was spent.
	exitRefused = 2
	// exitStopped: the corpus stopped part-way -- interrupted, at its cap,
	// or on an infrastructure failure -- and running the same command again
	// resumes it.
	exitStopped = 3
)

// environment is everything run asks of the machine, so a test can be a
// different machine.
type environment struct {
	goarch   string
	getenv   func(string) string
	probe    func(ctx context.Context, name string, args ...string) (string, error)
	endpoint string
}

// flags is the invocation, parsed.
type flags struct {
	mode           string
	out            string
	config         string
	confirm        string
	maxUSD         float64
	pilot          bool
	pilotSize      int
	seed           uint64
	python         string
	docker         string
	runID          string
	harnessTimeout time.Duration
	prologue       string
	keepImages     bool
}

// refusal is an invocation refused before anything was spent.
type refusal struct{ msg string }

func (r refusal) Error() string { return r.msg }

func refuse(format string, args ...any) error { return refusal{fmt.Sprintf(format, args...)} }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, environment{
		goarch:   runtime.GOARCH,
		getenv:   os.Getenv,
		probe:    probe,
		endpoint: fixrate.HuggingFaceEndpoint,
	})
	stop()
	os.Exit(code)
}

// probe runs a program and returns what it printed.
func probe(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // the host's own tools, with fixed arguments
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// run is the command. See the package comment.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, env environment) int {
	f, err := parseFlags(args, stderr)
	if err != nil {
		return report(stderr, err)
	}
	mode, cfg, err := settle(f, env)
	if err != nil {
		return report(stderr, err)
	}
	if err := preflight(ctx, f, mode, env); err != nil {
		return report(stderr, err)
	}
	if mode == fixrate.ModeReplay && env.goarch != "amd64" {
		if err := say(stderr, "warning: this host is %s; the instance images are linux/amd64 and run under emulation "+
			"here, slowly. A live run refuses such a host.\n", env.goarch); err != nil {
			return exitError
		}
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	opts, err := plan(ctx, f, mode, cfg, env, logger)
	if err != nil {
		return report(stderr, err)
	}
	result, err := fixrate.RunCorpus(ctx, opts)
	if result != nil {
		if err := say(stdout, "%s\nreport: %s and %s (aggregates only; safe to publish)\n", result.HumanSummary(),
			filepath.Join(f.out, "report.json"), filepath.Join(f.out, "report.txt")); err != nil {
			return exitError
		}
	}
	switch {
	case err == nil:
		return exitComplete
	case errors.Is(err, fixrate.ErrCorpusInterrupted), errors.Is(err, fixrate.ErrSpendCapReached),
		errors.Is(err, fixrate.ErrInfrastructure):
		return statusAfter(exitStopped, say(stderr, "stopped: %v\nEvery finished cell is kept. Run the same command again to resume.\n", err))
	default:
		return report(stderr, err)
	}
}

// report prints err and returns the exit status it calls for.
func report(stderr io.Writer, err error) int {
	var r refusal
	if errors.As(err, &r) {
		return statusAfter(exitRefused, say(stderr, "refused: %s\n", r.msg))
	}
	return statusAfter(exitError, say(stderr, "error: %v\n", err))
}

// statusAfter is code, unless the message that explains it could not be
// written, which is an error of its own.
func statusAfter(code int, writeErr error) int {
	if writeErr != nil {
		return exitError
	}
	return code
}

// say writes a formatted message, and reports whether it could.
func say(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format, args...)
	return err
}

// parseFlags parses args.
func parseFlags(args []string, stderr io.Writer) (flags, error) {
	var f flags
	fs := flag.NewFlagSet("swebench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.mode, "mode", "", `"live" to spend money on a real agent, or "replay" for the free gold-patch check; required`)
	fs.StringVar(&f.out, "out", "", "corpus directory: created on the first run, resumed on every later one; required")
	fs.StringVar(&f.config, "config", "", "the belay.yaml both arms run under (agent.model, budget.max_usd); required")
	fs.StringVar(&f.confirm, "confirm", "", "for -mode live, exactly: "+fixrate.ConfirmLiveSpend)
	fs.Float64Var(&f.maxUSD, "max-usd", 0, "for -mode live, the total spend cap in USD; required")
	fs.BoolVar(&f.pilot, "pilot", false, "run the stratified pilot with an A/A replicate, instead of the full corpus")
	fs.IntVar(&f.pilotSize, "pilot-size", 50, "tasks in the pilot")
	fs.Uint64Var(&f.seed, "seed", 1, "seed that chooses the pilot")
	fs.StringVar(&f.python, "python", "python3", "the Python with swebench=="+fixrate.SWEBenchHarnessVersion+" installed")
	fs.StringVar(&f.docker, "docker", "docker", "the docker command")
	fs.StringVar(&f.runID, "run-id", "", "the evaluation's run id; defaults to belay-swebench-pilot or belay-swebench-full")
	fs.DurationVar(&f.harnessTimeout, "harness-timeout", 30*time.Minute, "how long the official harness may run one instance's tests")
	fs.StringVar(&f.prologue, "prologue", fixrate.DefaultContainerPrologue, "shell that sets up an instance container's environment")
	fs.BoolVar(&f.keepImages, "keep-images", false, "keep each instance image once its task is scored, instead of removing it")
	if err := fs.Parse(args); err != nil {
		return flags{}, refuse("%v", err)
	}
	if fs.NArg() > 0 {
		return flags{}, refuse("unexpected argument %q", fs.Arg(0))
	}
	return f, nil
}

// settle checks everything about the invocation that needs nothing from the
// host, and loads the configuration.
func settle(f flags, env environment) (fixrate.Mode, config.Config, error) {
	mode, err := fixrate.ParseMode(f.mode)
	if err != nil {
		return "", config.Config{}, refuse("-mode must be %q or %q; there is no default, so a run cannot spend money "+
			"because a flag was left out", fixrate.ModeLive, fixrate.ModeReplay)
	}
	if mode == fixrate.ModeLive {
		if f.confirm != fixrate.ConfirmLiveSpend {
			return "", config.Config{}, refuse("-mode live spends real money; pass -confirm %q to say you mean it",
				fixrate.ConfirmLiveSpend)
		}
		if f.maxUSD <= 0 {
			return "", config.Config{}, refuse("-mode live needs -max-usd, the total the corpus may spend " +
				"(the runbook's estimates: about 350 for the pilot, 1500 for the full run)")
		}
	}
	if f.out == "" {
		return "", config.Config{}, refuse("-out names the corpus directory, and is required")
	}
	if f.config == "" {
		return "", config.Config{}, refuse("-config names the belay.yaml both arms run under, and is required")
	}
	cfg, err := loadConfig(f.config)
	if err != nil {
		return "", config.Config{}, refuse("%v", err)
	}
	if cfg.Agent.Backend != claude.BackendName {
		return "", config.Config{}, refuse("agent.backend is %q; the corpus drives %q", cfg.Agent.Backend, claude.BackendName)
	}
	backend := claude.New(nil)
	for _, node := range config.RoutableNodes() {
		model := cfg.Agent.ModelFor(node)
		rate, err := backend.ListPrice(model)
		if err != nil {
			return "", config.Config{}, refuse("the %s step would run on model %q, which the backend cannot price, so "+
				"its spend could not be held to a budget: %v", node, model, err)
		}
		// A run may spend against an assumed rate -- the ceiling is held at the
		// safe end -- but the corpus publishes cost per resolved task, and a
		// cost computed from a stand-in rate would be a guess presented as a
		// measurement.
		if rate.Assumed {
			return "", config.Config{}, refuse("the %s step would run on model %q, which the backend cannot price "+
				"exactly -- it would assume the highest rate it knows -- so the corpus's cost per resolved task "+
				"would be a guess; pin a model belay has a price for", node, model)
		}
	}
	if mode == fixrate.ModeLive {
		if env.goarch != "amd64" {
			return "", config.Config{}, refuse("this host is %s; a live run needs an x86_64 host, because SWE-bench's "+
				"official instance images are built for linux/amd64 and a number measured under emulation is not "+
				"comparable", env.goarch)
		}
		if env.getenv("ANTHROPIC_API_KEY") == "" && env.getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
			return "", config.Config{}, refuse("neither ANTHROPIC_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN is set; a live run " +
				"needs a dedicated API key, so its spend is billed, reported per call, and nobody else's")
		}
	}
	return mode, cfg, nil
}

// loadConfig reads exactly the file at path onto belay's defaults, with no
// project or user configuration merged in.
func loadConfig(path string) (config.Config, error) {
	src, err := os.ReadFile(path) //nolint:gosec // the configuration the owner named
	if err != nil {
		return config.Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	cfg := config.Default()
	if err := cfg.Merge(src, path); err != nil {
		return config.Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// preflight checks the tools the run needs, and refuses without them.
func preflight(ctx context.Context, f flags, mode fixrate.Mode, env environment) error {
	checks := []struct {
		name    string
		args    []string
		want    string
		install string
	}{
		{f.docker, []string{"info", "--format", "Server Version: {{.ServerVersion}}"}, "", "Docker is not running or not installed. " +
			"Install Docker Engine (https://docs.docker.com/engine/install/), start it, and give it at least 120GB of disk"},
		{f.python, []string{"-c", "import swebench; print(swebench.__version__)"}, fixrate.SWEBenchHarnessVersion,
			"install the official harness at the version the scoring was written against: " + f.python +
				" -m pip install swebench==" + fixrate.SWEBenchHarnessVersion},
		{"ruff", []string{"--version"}, "", "install ruff, the belay arm's lint gate: pipx install ruff"},
		{"git", []string{"--version"}, "", "install git: https://git-scm.com/downloads"},
	}
	if mode == fixrate.ModeLive {
		checks = append(checks, struct {
			name    string
			args    []string
			want    string
			install string
		}{"claude", []string{"--version"}, "", "install Claude Code, the backend both arms drive: npm install -g @anthropic-ai/claude-code"})
	}
	for _, c := range checks {
		out, err := env.probe(ctx, c.name, c.args...)
		switch {
		case err != nil:
			return refuse("%s: %v. To fix: %s", c.name, err, c.install)
		case c.want != "" && strings.TrimSpace(out) != c.want:
			return refuse("%s has swebench %q, and the corpus is scored by swebench %s. To fix: %s",
				c.name, strings.TrimSpace(out), c.want, c.install)
		}
	}
	return nil
}

// plan downloads and checks the corpus, chooses the tasks, and assembles the
// corpus options.
func plan(ctx context.Context, f flags, mode fixrate.Mode, cfg config.Config, env environment, logger *slog.Logger) (fixrate.CorpusOptions, error) {
	out, err := filepath.Abs(f.out)
	if err != nil {
		return fixrate.CorpusOptions{}, err
	}
	py := fixrate.Python{Path: f.python}
	parquet, err := fixrate.Download(ctx, http.DefaultClient, env.endpoint, fixrate.PinnedDataFile, filepath.Join(out, "dataset"))
	if err != nil {
		return fixrate.CorpusOptions{}, err
	}
	records, err := fixrate.ConvertRecords(ctx, py, parquet, filepath.Join(out, "dataset", "instances.jsonl"), fixrate.PinnedRecordCount)
	if err != nil {
		return fixrate.CorpusOptions{}, err
	}
	var tasks []fixrate.Task
	patches := map[string]string{}
	var invalid []string
	for _, inst := range records.Instances {
		task, err := inst.Task(fixrate.PinnedSource)
		if err != nil {
			invalid = append(invalid, err.Error())
			continue
		}
		tasks = append(tasks, task)
		patches[task.ID] = inst.Patch
	}
	if len(invalid) > 0 {
		return fixrate.CorpusOptions{}, fmt.Errorf("%d of the pinned records cannot become tasks; the first: %s", len(invalid), invalid[0])
	}
	selected, replicates, label, err := selection(tasks, f)
	if err != nil {
		return fixrate.CorpusOptions{}, err
	}
	runID := f.runID
	if runID == "" {
		runID = "belay-swebench-full"
		if f.pilot {
			runID = "belay-swebench-pilot"
		}
	}

	var agentFor func(fixrate.Cell) belay.AgentBackend
	if mode == fixrate.ModeLive {
		backend := claude.New(logger)
		agentFor = func(fixrate.Cell) belay.AgentBackend { return backend }
	} else {
		label += "; every arm driven by a stand-in that applies the instance's gold patch"
		agentFor = func(c fixrate.Cell) belay.AgentBackend { return goldAgent(patches[c.Task.ID]) }
	}
	prov := &fixrate.ContainerProvisioner{Docker: f.docker, RunID: runID, Prologue: f.prologue}
	afterTask := prov.RemoveImage
	if f.keepImages {
		afterTask = nil
	}
	return fixrate.CorpusOptions{
		Mode:          mode,
		Confirm:       f.confirm,
		Dir:           out,
		RunID:         runID,
		Corpus:        fixrate.PinnedSource.String(),
		RecordsSHA256: records.SHA256,
		Selection:     label,
		Tasks:         selected,
		Replicates:    replicates,
		Config:        cfg,
		AgentFor:      agentFor,
		Provisioner:   prov,
		AfterTask:     afterTask,
		Evaluator: fixrate.Harness{
			Python: py, Dataset: records.Path, Dir: filepath.Join(out, "evaluation"), RunID: runID, Timeout: f.harnessTimeout,
		},
		MaxUSD: f.maxUSD,
		Logger: logger,
	}, nil
}

// selection chooses the corpus's tasks and replicates from the flags: the
// stratified pilot with the A/A replicate, or every task once. The A/A
// replicate runs on the pilot only, as the roadmap settled: it measures the
// run-to-run noise the full run's delta is read against, at a pilot's cost.
func selection(tasks []fixrate.Task, f flags) ([]fixrate.Task, int, string, error) {
	if !f.pilot {
		return tasks, 1, fmt.Sprintf("all %d tasks of %s, each arm once", len(tasks), fixrate.PinnedSource), nil
	}
	pilot, err := fixrate.StratifiedPilot(tasks, f.pilotSize, f.seed)
	if err != nil {
		return nil, 0, "", err
	}
	return pilot, 2, fmt.Sprintf("stratified pilot: %d of %d tasks of %s by repository, seed %d, each arm twice (an A/A replicate)",
		len(pilot), len(tasks), fixrate.PinnedSource, f.seed), nil
}

// goldAgent is the free mode's stand-in for the agent: its planning call
// returns a plan, and every editing call applies patch, the instance's gold
// patch, if it is not applied already. It spends nothing.
func goldAgent(patch string) belay.AgentBackend {
	return &belaytest.FakeAgent{NameValue: "swebench-gold-patch", Func: func(ctx context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
		if len(req.AllowedTools) > 0 && !slices.Contains(req.AllowedTools, "Edit") {
			return belay.AgentResponse{Text: "Apply the change the issue describes."}, nil
		}
		if err := gitApply(ctx, req.WorkDir, patch, "--check", "--reverse"); err != nil {
			if err := gitApply(ctx, req.WorkDir, patch); err != nil {
				return belay.AgentResponse{}, fmt.Errorf("swebench: applying the gold patch: %w", err)
			}
		}
		var files []string
		for _, line := range strings.Split(patch, "\n") {
			if rest, ok := strings.CutPrefix(line, "+++ b/"); ok {
				files = append(files, strings.TrimSpace(rest))
			}
		}
		return belay.AgentResponse{
			Text:  "Applied the change.\n\n```" + changedFilesTag + "\n" + strings.Join(files, "\n") + "\n```\n",
			Turns: 1,
		}, nil
	}}
}

// changedFilesTag is the fence belay's code and fix nodes read an agent's
// list of changed files from (internal/nodes/code's ChangedFilesTag).
const changedFilesTag = "changed-files"

// gitApply runs git apply with args over patch, in dir.
func gitApply(ctx context.Context, dir, patch string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"apply"}, args...)...) //nolint:gosec // fixed binary and arguments
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
