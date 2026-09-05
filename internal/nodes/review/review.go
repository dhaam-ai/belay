// Package review implements belay's quality gate: the node that asks the
// configured reviewer whether the code is good enough to finish, and routes
// the run to the fix loop when it is not.
//
// # Three outcomes, three behaviours
//
// The distinction this node exists to enforce is that a failed gate is not a
// Go error. It is the ordinary, expected branch to graph.NodeFix, returned as
// (Result, nil) exactly like a failing test — the fix node is the whole point
// of gating, so reaching it is the gate succeeding, not failing.
//
//   - Gate passes: (Result{Next: graph.End, Status: StatusOK}, nil).
//   - Gate fails:  (Result{Next: graph.NodeFix, Status: StatusOK}, nil).
//   - No verdict:  (Result{Status: StatusFailed}, err). The tool ran but
//     reached no conclusion, or could not run at all.
//
// The third case is the one worth being careful about, because collapsing it
// into either neighbour is a real bug. Reported as a pass, an unmeasured
// change ships. Reported as a failure, the run sends a coding agent to edit
// source because golangci-lint is not installed — burning a fix attempt, and
// possibly the whole give_up budget, on a problem no edit can fix. "We never
// learned the answer" is not "the answer was no". See ErrGateUnresolved.
//
// # Whose threshold wins
//
// The node's. review.fail_on is recomputed against Counts.AtOrAbove on every
// path, and the adapter's own pass/fail verdict is advisory — see resolveGate
// for why that is required for a Linter and harmless for a Reviewer.
//
// # Persistence
//
// Per ADR 0002 this node writes no state.json, manifest.json or journal. It
// returns a state.Patch carrying the Review block and writes one artifact,
// review-<step>.json, which is content rather than control state.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
)

// Errors returned by this package.
//
// Every one of them means the gate could not be evaluated. None of them means
// the gate was evaluated and failed: that outcome is a Result routing to
// graph.NodeFix with a nil error, and deliberately has no sentinel, so no
// caller can accidentally treat it as a malfunction.
var (
	// ErrUnknownMode reports a review.mode this node cannot dispatch on.
	// Configuration validation should reject it first; reaching this node
	// with one is a wiring bug, not a user error.
	ErrUnknownMode = errors.New("belay/review: unknown review mode")

	// ErrAdapterMissing reports that the adapter the configured mode needs
	// is nil on the RunContext — review.mode "lint" with no Linter, or
	// "sonar"/"ai" with no Reviewer. It names both the mode and the
	// adapter, because the fix is always to wire one to the other.
	ErrAdapterMissing = errors.New("belay/review: no adapter configured for review mode")

	// ErrInvalidThreshold reports a review.fail_on that does not name a
	// belay.Severity. The node refuses to guess a default: every guess
	// either loosens a gate the operator meant to tighten or the reverse.
	ErrInvalidThreshold = errors.New("belay/review: invalid review.fail_on")

	// ErrNoWorkspace reports a RunContext whose Layout carries no run
	// directory, leaving the node nothing to derive the workspace from.
	ErrNoWorkspace = errors.New("belay/review: run context has no run directory")

	// ErrGateUnresolved reports that the reviewer ran but reached no
	// verdict — belay.GateError.
	//
	// This is an error rather than a route to graph.NodeFix on purpose. A
	// fix attempt is a scarce, budgeted resource aimed at a concrete
	// finding, and a report that reached no verdict carries no finding to
	// aim it at. Routing here would spend a give_up attempt asking an
	// agent to repair a tool failure it cannot see, and would do it again
	// on the next pass. Failing the node instead surfaces the tool problem
	// to the human who can actually fix it.
	ErrGateUnresolved = errors.New("belay/review: quality gate reached no verdict")
)

// Node is belay's quality gate.
//
// It holds no state: everything it needs arrives on the graph.RunContext, and
// everything it produces leaves in the graph.Result. One Node is therefore
// safe to register once and re-run for every step of every run, which is what
// the dispatcher's crash-recovery contract requires of it.
type Node struct{}

// New returns a review Node.
func New() *Node { return &Node{} }

// Name implements graph.Node and returns graph.NodeReview.
func (*Node) Name() string { return graph.NodeReview }

var _ graph.Node = (*Node)(nil)

// Run implements graph.Node: it runs the configured quality gate and routes
// on the verdict.
//
// A passing gate ends the run (graph.End); a failing gate routes to
// graph.NodeFix. Both are (Result, nil). A non-nil error means no verdict was
// reached, and every such Result leaves Next empty so that no error path can
// route to the fix loop.
//
// Result.Usage is always zero. Neither belay.Linter nor belay.Reviewer
// reports cost: a Reviewer's only channel for it would be QualityReport.Raw,
// which ADR 0006 forbids the graph from reading. An "ai" reviewer that drives
// an agent internally must therefore account for its own spend at the adapter
// or budget layer; this node cannot see it, and reporting a fabricated zero
// is honest where inventing a number would not be.
func (*Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc == nil {
		return graph.Result{Status: journal.StatusFailed}, errors.New("belay/review: nil run context")
	}
	if err := ctx.Err(); err != nil {
		return graph.Result{Status: journal.StatusAborted}, fmt.Errorf("belay/review: %w", err)
	}

	failOn, err := threshold(rc.Config.Review.FailOn)
	if err != nil {
		return graph.Result{Status: journal.StatusFailed}, err
	}
	dir, err := workspaceDir(rc)
	if err != nil {
		return graph.Result{Status: journal.StatusFailed}, err
	}

	report, err := runGate(ctx, rc, dir, failOn)
	if err != nil {
		return abortedOrFailed(err), err
	}
	return verdict(rc, report, failOn)
}

// threshold converts review.fail_on, a config.Severity string, into the
// belay.Severity that Counts.AtOrAbove compares against.
func threshold(s config.Severity) (belay.Severity, error) {
	failOn, err := belay.ParseSeverity(string(s))
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrInvalidThreshold, err)
	}
	return failOn, nil
}

// workspaceDir recovers the target repository's root from rc.Layout.
//
// A graph.RunContext carries no workspace path of its own; the run directory
// is the only anchor a node is given. state.NewLayout builds that directory
// as <workspace>/.belay/runs/<run-id>, so walking three parents back is the
// exact inverse of its construction — and the only derivation available to a
// node that must not read manifest.json itself (ADR 0002).
func workspaceDir(rc *graph.RunContext) (string, error) {
	if rc.Workspace == "" {
		return "", ErrNoWorkspace
	}
	return rc.Workspace, nil
}

// runGate dispatches on review.mode and returns the adapter's report.
//
// "lint" uses rc.Linter; "sonar" and "ai" both use rc.Reviewer, because they
// differ in which Reviewer the wiring layer supplies, not in how this node
// talks to it. The node never calls Linter.Detect: rc.Linter is already the
// adapter the dispatcher resolved for this workspace.
func runGate(ctx context.Context, rc *graph.RunContext, dir string, failOn belay.Severity) (belay.QualityReport, error) {
	mode := rc.Config.Review.Mode
	switch mode {
	case config.ReviewModeLint:
		if rc.Linter == nil {
			return belay.QualityReport{}, fmt.Errorf("%w: mode %q needs a belay.Linter but RunContext.Linter is nil", ErrAdapterMissing, mode)
		}
		report, err := rc.Linter.Lint(ctx, dir)
		if err != nil {
			// %w keeps errors.Is(err, belay.ErrToolchainMissing) true, which
			// is what tells "install golangci-lint" apart from "your code
			// has problems" at every layer above this one.
			return belay.QualityReport{}, fmt.Errorf("belay/review: linter %q: %w", rc.Linter.Name(), err)
		}
		return report, nil

	case config.ReviewModeSonar, config.ReviewModeAI:
		if rc.Reviewer == nil {
			return belay.QualityReport{}, fmt.Errorf("%w: mode %q needs a belay.Reviewer but RunContext.Reviewer is nil", ErrAdapterMissing, mode)
		}
		report, err := rc.Reviewer.Review(ctx, reviewRequest(rc, dir, failOn))
		if err != nil {
			return belay.QualityReport{}, fmt.Errorf("belay/review: reviewer for mode %q: %w", mode, err)
		}
		return report, nil

	default:
		return belay.QualityReport{}, fmt.Errorf("%w: %q (want %q, %q or %q)",
			ErrUnknownMode, mode, config.ReviewModeLint, config.ReviewModeSonar, config.ReviewModeAI)
	}
}

// reviewRequest builds the one Reviewer input.
//
// ChangedFiles is cloned rather than aliased: rc.State is a snapshot, but its
// slice header still points at the dispatcher's backing array, and an adapter
// that sorted or truncated the slice in place would silently rewrite the
// blackboard through a value it was handed read-only.
func reviewRequest(rc *graph.RunContext, dir string, failOn belay.Severity) belay.ReviewRequest {
	return belay.ReviewRequest{
		WorkDir:      dir,
		ChangedFiles: slices.Clone(rc.State.Code.ChangedFiles),
		ProjectKey:   projectKey(dir),
		FailOn:       failOn,
	}
}

// projectKey names the project to a project-keyed backend such as SonarQube.
//
// internal/config has no project_key field today — config.Sonar carries a
// runner, an image and the gate-wait settings, nothing identifying — so the
// key is derived from the workspace directory name. That is stable across
// runs of the same checkout, which is the property SonarQube needs to compare
// a scan against the project's history, and it is what a human would type by
// hand. A Reviewer with no project concept ignores it (belay.ReviewRequest);
// one that requires it, like internal/sonar/scanner, would otherwise fail
// with ErrMissingProjectKey on every run.
func projectKey(dir string) string { return filepath.Base(dir) }

// abortedOrFailed picks the Status for an error path. A cancelled context cut
// the node short (StatusAborted); anything else is a node that ran and could
// not produce a verdict (StatusFailed). Next is empty either way, so no error
// path can route to the fix loop.
func abortedOrFailed(err error) graph.Result {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return graph.Result{Status: journal.StatusAborted}
	}
	return graph.Result{Status: journal.StatusFailed}
}

// verdict resolves the gate, records the report, and routes.
func verdict(rc *graph.RunContext, report belay.QualityReport, failOn belay.Severity) (graph.Result, error) {
	log := logger(rc)
	gate := resolveGate(report.Gate, report.Counts, failOn)
	if gate != report.Gate {
		// Not fatal, but worth surfacing: for a Linter it usually means the
		// adapter was constructed with a different fail_on than this run's,
		// and for a Reviewer it means the adapter broke its contract to
		// honour req.FailOn.
		log.Warn("review: adapter gate overridden by review.fail_on",
			slog.String("source", report.Source),
			slog.String("reported", report.Gate.String()),
			slog.String("resolved", gate.String()),
			slog.String("fail_on", failOn.String()))
	}
	report.Gate = gate
	if report.Issues == nil {
		// belay.QualityReport.Issues is contractually never nil, but this
		// node is the last thing between an adapter and the blackboard, and
		// state.NewReview preserves nil exactly. A nil here would serialize
		// as "issues": null in both the artifact and state.json, breaking
		// the identical-key-shape guarantee ADR 0006 rests on — and it would
		// break first for the emptiest, most-fixtured report of all.
		report.Issues = []belay.Issue{}
	}

	path, err := writeReport(rc, report, log)
	if err != nil {
		return graph.Result{Status: journal.StatusFailed}, err
	}

	source := report.Source
	if source == "" {
		source = string(rc.Config.Review.Mode)
	}
	note := summarizeVerdict(gate, source, report.Counts, failOn, path)

	// The artifact keeps the adapter's own Summary; the blackboard's copy
	// gets the artifact path appended, because state.Review has no path
	// field of its own (state.Test has ReportPath, state.Review does not)
	// and the projection drops Raw. Without this, nothing in state.json
	// points at the full report.
	blackboard := state.NewReview(report, path)
	patch := state.Patch{Review: &blackboard}

	log.Info("review: gate evaluated",
		slog.String("gate", gate.String()),
		slog.String("source", source),
		slog.String("fail_on", failOn.String()),
		slog.Int("at_or_above", report.Counts.AtOrAbove(failOn)),
		slog.Int("total_issues", report.Counts.Total()),
		slog.String("report", path))

	switch gate {
	case belay.GatePass:
		return graph.Result{Next: graph.End, Status: journal.StatusOK, Patch: patch, Note: note}, nil
	case belay.GateFail:
		return graph.Result{Next: graph.NodeFix, Status: journal.StatusOK, Patch: patch, Note: note}, nil
	default:
		// GateError. The patch and note still travel, so a human resuming
		// the run can see what the tool managed to report, but Next stays
		// empty: an unresolved gate must not spend a fix attempt.
		return graph.Result{Status: journal.StatusFailed, Patch: patch, Note: note},
			fmt.Errorf("%w: %s reported %s at fail_on=%s (report %s)",
				ErrGateUnresolved, source, belay.GateError, failOn, path)
	}
}

// resolveGate reduces the adapter's reported gate to the verdict this node
// acts on. The node's threshold wins.
//
// It has to, for the Linter path. belay.Linter.Lint takes only a directory —
// no ReviewRequest, no caller-supplied FailOn — so a Linter's Gate reflects
// whatever threshold that adapter was built with, which need not be this
// run's review.fail_on. belay.Linter's own doc comment says as much, and
// tells a caller needing a specific threshold to "ignore the Linter's Gate
// and compute its own from Counts.AtOrAbove". This is that caller.
//
// Applying the same rule to the Reviewer path costs nothing and is worth the
// uniformity: a Reviewer is handed req.FailOn and is contractually required
// to make Gate agree with Counts.AtOrAbove(req.FailOn), so recomputing
// reproduces its answer whenever it kept its promise — and catches it when it
// did not, rather than trusting a verdict the report's own numbers contradict.
//
// GateError and GateUnknown are the exception, and are preserved as
// GateError. They are not verdicts about issues; they are the absence of a
// verdict, and Counts cannot tell "measured, found nothing" apart from
// "measured nothing at all". Recomputing them would turn a tool that fell
// over into a clean pass — the single most dangerous mistranslation available
// here, since it ships unreviewed code silently. GateUnknown joins GateError
// because a conforming adapter never returns it from a successful call, so
// the only adapter it can penalize is one already off-contract, and it
// penalizes it in the safe direction.
func resolveGate(reported belay.GateStatus, counts belay.Counts, failOn belay.Severity) belay.GateStatus {
	if reported == belay.GateError || reported == belay.GateUnknown {
		return belay.GateError
	}
	if counts.AtOrAbove(failOn) > 0 {
		return belay.GateFail
	}
	return belay.GatePass
}

// writeReport archives the full report as review-<step>.json and returns its
// path relative to the run directory.
func writeReport(rc *graph.RunContext, report belay.QualityReport, log *slog.Logger) (string, error) {
	name := fmt.Sprintf("review-%d.json", rc.Step)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		// Raw is the only field that can fail to marshal — it is opaque
		// bytes an adapter supplied. Dropping it is strictly better than
		// discarding a verdict belay already reached over a debug field
		// ADR 0006 forbids the graph from reading anyway.
		log.Warn("review: dropping unmarshalable Raw from report artifact",
			slog.String("source", report.Source), slog.String("error", err.Error()))
		report.Raw = nil
		if data, err = json.MarshalIndent(report, "", "  "); err != nil {
			return "", fmt.Errorf("belay/review: encode %s: %w", name, err)
		}
	}
	path, err := rc.WriteArtifact(name, append(data, '\n'))
	if err != nil {
		return "", fmt.Errorf("belay/review: write %s: %w", name, err)
	}
	return path, nil
}

// summarizeVerdict renders graph.Result.Note, for example
//
//	gate fail: 1 blocker, 3 major at fail_on=major (source=golangci-lint, report=artifacts/review-6.json)
//
// It is redacted by construction. Every part is either a severity count, a
// constant, an adapter name or a path belay chose — never captured tool
// output. QualityReport.Summary is deliberately excluded for that reason:
// it is adapter-authored prose, and a Note is not the place to find out
// whether a given adapter redacts.
func summarizeVerdict(gate belay.GateStatus, source string, c belay.Counts, failOn belay.Severity, path string) string {
	var head string
	switch gate {
	case belay.GatePass:
		head = fmt.Sprintf("gate pass: no issues at or above fail_on=%s", failOn)
	case belay.GateFail:
		head = fmt.Sprintf("gate fail: %s at fail_on=%s", breakdown(c, failOn), failOn)
	default:
		head = fmt.Sprintf("gate error: no verdict reached at fail_on=%s", failOn)
	}
	return fmt.Sprintf("%s (source=%s, report=%s)", head, source, path)
}

// breakdown renders the non-zero severity counts at or above failOn, worst
// first: "1 blocker, 3 major". Counts below the threshold are omitted because
// they did not contribute to the verdict the note is explaining.
func breakdown(c belay.Counts, failOn belay.Severity) string {
	rows := []struct {
		sev belay.Severity
		n   int
	}{
		{belay.SeverityBlocker, c.Blocker},
		{belay.SeverityCritical, c.Critical},
		{belay.SeverityMajor, c.Major},
		{belay.SeverityMinor, c.Minor},
		{belay.SeverityInfo, c.Info},
	}
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.sev >= failOn && row.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", row.n, row.sev))
		}
	}
	if len(parts) == 0 {
		return "0 issues"
	}
	return strings.Join(parts, ", ")
}

// logger returns rc.Logger, or the default logger if the dispatcher left it
// unset. A nil logger must not be the reason a quality gate panics.
func logger(rc *graph.RunContext) *slog.Logger {
	if rc.Logger != nil {
		return rc.Logger
	}
	return slog.Default()
}
