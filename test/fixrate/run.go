//go:build fixrate

package fixrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/belay-dev/belay/pkg/belay"
)

// ErrNoAgent reports a RunOptions with no Agent: Run has nothing to drive
// belay's graph with.
var ErrNoAgent = errors.New("fixrate: RunOptions.Agent is required")

// ErrLiveConfirmationRequired reports a ModeLive RunOptions whose Confirm
// field does not exactly equal ConfirmLiveSpend. See ConfirmLiveSpend's
// doc comment for why this exists.
var ErrLiveConfirmationRequired = fmt.Errorf("fixrate: Mode is %q but Confirm does not equal ConfirmLiveSpend; refusing to spend money implicitly", ModeLive)

// ErrReplayModeSpentMoney reports that a ModeReplay run recorded non-zero
// belay.Usage — the automatic, after-the-fact check backing this
// package's zero-token-spend guarantee (see Run's doc comment). It means
// RunOptions.Agent was not actually a zero-cost backend, which is a
// caller wiring bug: ModeReplay promises free measurement, and Run
// refuses to hand back a Result that would misrepresent one that was not.
var ErrReplayModeSpentMoney = errors.New("fixrate: ModeReplay recorded non-zero usage; refusing to report a result " +
	"(RunOptions.Agent is not a zero-cost backend — this is a caller wiring bug, not a belay bug)")

// RunOptions configures one Run call.
type RunOptions struct {
	// Mode is required and must be ModeReplay or ModeLive; there is no
	// default.
	Mode Mode

	// FixtureDir is fixtures/seeded-bug's path. Empty resolves via
	// DefaultFixtureDir.
	FixtureDir string

	// Selection chooses which catalogued defects to measure. Required;
	// see Selection's doc comment for its two mutually exclusive modes.
	Selection Selection

	// Agent drives every plan/code/fix invocation belay's graph makes.
	// Required for both modes: this package never constructs one itself
	// (see Mode's doc comment on why ModeLive is entirely the caller's
	// responsibility). For ModeReplay, ordinarily NewScriptedAgent's
	// result, or a *belaytest.FakeAgent, or an internal/agent/replay
	// Backend built with Replay(cassette) — any belay.AgentBackend whose
	// Invoke never actually reports spend.
	Agent belay.AgentBackend

	// Confirm must exactly equal ConfirmLiveSpend when Mode is ModeLive.
	// Ignored for ModeReplay.
	Confirm string

	// WorkDir is the parent directory Run creates its injected copy and
	// belay run under. Empty makes Run create and (unless KeepWorkDir)
	// remove its own temporary directory.
	WorkDir string
	// KeepWorkDir, if true, leaves WorkDir's contents on disk after Run
	// returns — the injected copy, the .belay/ run directory with its
	// full journal and node evidence, and this harness's own snapshots —
	// for a human to inspect a surprising result. Ignored when WorkDir
	// was supplied by the caller (a caller-supplied WorkDir is never
	// removed by Run either way).
	KeepWorkDir bool

	// Logger receives belay's own structured run log. Defaults to a
	// discarding logger.
	Logger *slog.Logger
	// Now supplies the clock Run and the underlying belay run use, so a
	// test can assert an exact GeneratedAt. Defaults to time.Now.
	Now func() time.Time
}

// Run injects Selection's defects into a fresh copy of the fixture named
// by FixtureDir, drives belay's real, unmodified dispatcher graph against
// that copy using Agent, and returns the diffed, cheat-checked result.
//
// # The zero-cost guarantee, and how it is enforced rather than merely
// claimed
//
// For Mode: ModeReplay, Run sums belay.Usage.USD across the whole run
// (graph.Outcome.Usage, itself the dispatcher's own cumulative ledger —
// see internal/graph) and returns ErrReplayModeSpentMoney instead of a
// Result if that sum is non-zero. This is not a documentation promise:
// it is a runtime assertion that fires on every single call, live or
// test, in the same build as everything else in this package. Combined
// with the fact that ModeReplay never touches the Agent field for
// ModeLive-only wiring (there is no live-backend constructor anywhere in
// this package's import graph — see doc.go), the two together are how
// "replay mode spends nothing" is guaranteed rather than asserted: it is
// checked, not trusted.
//
// # What "belay" means here
//
// Run drives internal/graph.Dispatcher over internal/nodes.Default's
// unmodified registry (plan -> approve -> code -> write -> test -> fix ->
// review), with real internal/runner (go test) and internal/linter
// (golangci-lint) adapters — neither of which touches the network. Only
// belay.AgentBackend is swappable, and it is the one piece of "belay" a
// fix-rate number is actually trying to hold constant while varying.
func Run(ctx context.Context, opts RunOptions) (*Result, error) {
	if !opts.Mode.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrUnknownMode, opts.Mode)
	}
	if opts.Agent == nil {
		return nil, ErrNoAgent
	}
	if opts.Mode == ModeLive && opts.Confirm != ConfirmLiveSpend {
		return nil, ErrLiveConfirmationRequired
	}
	if err := opts.Selection.validate(); err != nil {
		return nil, err
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	fixtureDir, err := resolveFixtureDir(opts.FixtureDir)
	if err != nil {
		return nil, err
	}
	pristineSrcDir := filepath.Join(fixtureDir, "src")

	parent := opts.WorkDir
	ownWorkDir := parent == ""
	if ownWorkDir {
		parent, err = os.MkdirTemp("", "belay-fixrate-")
		if err != nil {
			return nil, fmt.Errorf("fixrate: create work directory: %w", err)
		}
	}
	if ownWorkDir && !opts.KeepWorkDir {
		defer func() { _ = os.RemoveAll(parent) }()
	}

	workspace := filepath.Join(parent, "workspace-"+opts.Selection.label())

	manifest, err := injectDefects(ctx, fixtureDir, workspace, opts.Selection)
	if err != nil {
		return nil, err
	}

	before, err := snapshotWorkspace(workspace, defectFiles(manifest.Defects))
	if err != nil {
		return nil, fmt.Errorf("fixrate: pre-run snapshot: %w", err)
	}

	// See golangcicache.go: every injected copy shares one hardcoded
	// module path, which can otherwise leak a stale golangci-lint cache
	// entry from an earlier Run's already-deleted temp directory into
	// this one — belay's own review node (unmodified) then fails its
	// gate on an issue attributed to a file that no longer exists, and
	// the fix loop can never resolve it. Both the run itself (whose
	// review node invokes golangci-lint) and this package's own post-run
	// lint check happen inside the isolated-cache window.
	var (
		belayResult belayRunResult
		after       snapshot
		postTests   map[string]string
		lintOutput  string
		lintAvail   bool
	)
	err = withIsolatedGolangciCache(parent, func() error {
		var runErr error
		belayResult, runErr = runBelay(ctx, workspace, opts.Agent, now, logger)
		if runErr != nil {
			return runErr
		}

		usage := belayResult.Outcome.Usage
		if opts.Mode == ModeReplay && (usage.USD != 0 || usage.InputTokens != 0 || usage.OutputTokens != 0) {
			return fmt.Errorf("%w (recorded $%.6f, %d input token(s), %d output token(s))",
				ErrReplayModeSpentMoney, usage.USD, usage.InputTokens, usage.OutputTokens)
		}

		var snapErr error
		after, snapErr = snapshotWorkspace(workspace, defectFiles(manifest.Defects))
		if snapErr != nil {
			return fmt.Errorf("fixrate: post-run snapshot: %w", snapErr)
		}

		var testErr error
		postTests, testErr = leafTestResults(ctx, workspace)
		if testErr != nil {
			return fmt.Errorf("fixrate: post-run test verification: %w", testErr)
		}

		lintOutput, lintAvail = runGolangciLint(ctx, workspace)
		return nil
	})
	if err != nil {
		return nil, err
	}
	usage := belayResult.Outcome.Usage
	lintAvailable := lintAvail

	defects := make([]DefectOutcome, 0, len(manifest.Defects))
	for _, d := range manifest.Defects {
		defects = append(defects, evaluateDefect(d, before, after, postTests, lintOutput, lintAvailable))
	}
	totals, breakdown := summarize(defects)

	var warnings []string
	if !lintAvailable {
		warnings = append(warnings, "golangci-lint was not found on PATH; lint-channel defects report status unverified")
	}

	result := &Result{
		SchemaVersion: SchemaVersion,
		Mode:          opts.Mode,
		Provenance:    opts.Mode.provenance(),
		GeneratedAt:   now().UTC(),
		FixtureSrc:    pristineSrcDir,
		Selection: SelectionInfo{
			Mode:   manifest.Selection.Mode,
			Bugs:   manifest.Selection.Bugs,
			Random: manifest.Selection.Random,
			Seed:   manifest.Selection.Seed,
		},
		BelayStatus:    belayResult.Outcome.Status.String(),
		BelaySteps:     belayResult.Outcome.Steps,
		BelayNote:      belayNote(belayResult),
		UsageUSD:       usage.USD,
		UsageEstimated: usage.Estimated,
		LintAvailable:  lintAvailable,
		Totals:         totals,
		ClassBreakdown: breakdown,
		Defects:        defects,
		Warnings:       warnings,
	}
	return result, nil
}

// belayNote picks the most informative single line to summarize how the
// belay run ended: the dispatcher's own Outcome.Note when there is one,
// falling back to its (non-fatal-to-this-harness — see belayRunResult)
// RunErr.
func belayNote(r belayRunResult) string {
	if r.Outcome.Note != "" {
		return r.Outcome.Note
	}
	if r.RunErr != "" {
		return r.RunErr
	}
	return ""
}

// defectFiles returns the distinct InjectedDefect.File values in defects,
// in first-appearance order — the source files evaluateDefect (via
// hash.go's sourceChanged) needs tracked in every snapshotWorkspace call.
func defectFiles(defects []InjectedDefect) []string {
	seen := make(map[string]bool, len(defects))
	out := make([]string, 0, len(defects))
	for _, d := range defects {
		if seen[d.File] {
			continue
		}
		seen[d.File] = true
		out = append(out, d.File)
	}
	return out
}
