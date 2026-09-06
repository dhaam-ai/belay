//go:build fixrate

package fixrate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/linter"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/runner"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// belayRunID names the run directory (<workspace>/.belay/runs/<id>) every
// harness-driven run uses. A fixed constant is safe — never reused across
// two concurrent runs on the same directory — because every call site
// gives runBelay a workspace that is itself a fresh, unique temporary
// directory (see run.go), so there is never more than one run per
// workspace to disambiguate.
const belayRunID = "fixrate"

// belayGoal is the run's objective, sent to the plan node and echoed into
// every agent prompt. It deliberately names no defect IDs or file paths:
// the point of driving belay's real graph is to exercise the same
// plan -> code -> test -> fix -> review loop a real run takes, not to hand
// the agent (or a human reading the journal) a spoiler.
const belayGoal = "Make the repository's test suite and quality gate pass."

// belayRunResult is what runBelay hands back to Run: the dispatcher's own
// Outcome, plus enough of the run's identity for a caller to inspect the
// run directory afterward (evidence files, journal, artifacts) if a
// result needs explaining.
type belayRunResult struct {
	Outcome graph.Outcome
	RunDir  string
	Config  config.Config
	// RunErr is dispatcher.Run's own error message, if any, or empty.
	//
	// It is deliberately NOT returned as runBelay's error: a run ending in
	// journal.StatusFailed (the fix loop exhausting graph.give_up on a
	// defect this harness's own configuration left unfixed, for instance)
	// is dispatcher.Run's normal, documented way of reporting that
	// outcome (see graph.ErrNodeFailed's doc comment) — an expected
	// result this harness must classify, not a reason to abort before
	// classifying anything. runBelay's own error return is reserved for
	// failures before Run is ever called: this package's wiring is
	// broken, not the defect set's fix rate.
	RunErr string
}

// belayConfig returns the config.Config a fix-rate measurement run uses.
//
// It starts from config.Default() and changes exactly one thing:
// Graph.Approval is false, so the run never pauses for a human — a fix-
// rate harness has no terminal attached and must run to completion
// unattended. Everything else (graph.give_up, graph.max_steps,
// review.mode "lint" at review.fail_on "major", the budget ceiling) is
// belay's own shipped default, deliberately: measuring belay "as it
// ships" is the point, not a harness-tuned configuration nothing else
// uses.
func belayConfig() config.Config {
	cfg := config.Default()
	cfg.Graph.Approval = false
	return cfg
}

// runBelay drives belay's real, unmodified dispatcher (internal/graph)
// over the default node graph (internal/nodes.Default:
// plan -> approve -> code -> write -> test -> fix -> review) against
// workspace, using agent as the sole AgentBackend and real, local
// TestRunner (go test) and Linter (golangci-lint) adapters — both of
// which run entirely on-machine and touch neither the network nor any
// paid service, regardless of Mode.
//
// workspace must be a fresh directory belonging to exactly this run: see
// belayRunID.
func runBelay(ctx context.Context, workspace string, agent belay.AgentBackend, now func() time.Time, logger *slog.Logger) (belayRunResult, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = time.Now
	}

	cfg := belayConfig()

	layout, err := state.NewLayout(workspace, belayRunID)
	if err != nil {
		return belayRunResult{}, fmt.Errorf("fixrate: run layout: %w", err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		return belayRunResult{}, fmt.Errorf("fixrate: create run directory: %w", err)
	}

	store := state.NewStore(layout)
	manifest := state.NewManifest(now(), belayRunID, workspace, belayGoal, cfg)
	if err := store.SaveManifest(manifest); err != nil {
		return belayRunResult{}, fmt.Errorf("fixrate: save manifest: %w", err)
	}
	if err := store.SaveState(state.NewState(belayGoal)); err != nil {
		return belayRunResult{}, fmt.Errorf("fixrate: save initial state: %w", err)
	}

	registry, err := nodes.Default(cfg)
	if err != nil {
		return belayRunResult{}, fmt.Errorf("fixrate: wire node graph: %w", err)
	}

	dispatcher, err := graph.NewDispatcher(graph.Options{
		Store:    store,
		Registry: registry,
		Config:   cfg,
		Logger:   logger,
		Now:      now,
		Agent:    agent,
		Runner:   runner.NewGo(nil, logger),
		Linter:   linter.NewGolangCI(),
		// Reviewer and Isolator are nil: review.mode "lint" needs only
		// Linter, and fanout (the only consumer of Isolator) is disabled
		// by belayConfig's use of config.Default().
	})
	if err != nil {
		return belayRunResult{}, fmt.Errorf("fixrate: construct dispatcher: %w", err)
	}

	outcome, runErr := dispatcher.Run(ctx)
	result := belayRunResult{Outcome: outcome, RunDir: layout.RunDir(), Config: cfg}
	if runErr != nil {
		result.RunErr = runErr.Error()
	}
	return result, nil
}
