// Package policy implements belay's policy gate: the node that holds a run's
// change to the guardrails declared in belay.yaml, whatever its tests say.
//
// It sits between write and test, so a change is judged before anything else
// is spent on it — and on every pass of the fix loop, because fix routes back
// through write and so through here. The rules themselves live in
// internal/policy, which this node imports as `rules`; everything here is
// about turning a verdict into a graph.Result.
//
// # What it judges
//
// The files the workspace says changed, and never only the files the agent
// says it changed. An agent's report is a claim, and a gate that judged it
// was a gate the agent could walk past: one that edited infra/main.tf and
// belay.yaml and reported only src/app.py passed every rule, and both edits
// were committed. So the gate judges the run's recorded change, which the
// write node built from the report and the workspace's own answer, together
// with that answer asked again here (graph.RunContext.Changes): git against
// the run's base commit, or the record of the workspace's files taken when
// the run began. Every file the agent changed and did not report is judged
// like any other and named in the verdict, so a run cannot hide an edit.
//
// A run whose workspace keeps no record to measure against — a fork's copy,
// a run created before belay recorded one — is judged on the agent's
// report, and every verdict it gets says so and why: "policy judges the
// files the agent reports: ...". The limitation is stated where it applies,
// not in a document a person might not read.
//
// A workspace that cannot answer at all is no verdict, an error, and never
// a fall back to the report: an agent can break the answer on purpose, by
// rewriting the .git file in its workspace, and that must not make its own
// word the thing that is judged.
//
// Paths are judged in the file system's own names, so a report spelled
// "Infra/main.tf" cannot dodge "infra/**"; and on a disk that folds case,
// which the run records when it is created (state.PolicyPin.FoldCase), the
// rules match without regard to case, because there the two spellings are
// one file.
//
// What it cannot see is anything the agent writes outside the workspace,
// belay's own .belay directory included: belay does not sandbox the agent,
// and the gate judges the workspace. Nor, in a git checkout, a file git
// ignores, which a commit never carries either.
//
// # Three outcomes, and none of them is an error
//
//   - Nothing broken: (Result{Next: graph.NodeTest, StatusOK}, nil).
//   - Something the agent can put right: (Result{Next: graph.NodeFix,
//     StatusOK}, nil) — the ordinary branch into the repair loop, exactly as
//     a failing test or a failed quality gate is.
//   - Something it cannot: (Result{Status: StatusFailed}, nil). The run ends.
//
// A non-nil error is reserved for "no verdict was reachable": a pinned rule
// this belay cannot compile, a changed file it cannot read, a workspace that
// cannot say what changed. That distinction
// is the one worth being careful about here, because collapsing it into a
// pass is the failure the whole feature exists to prevent. A guardrail belay
// skipped must never be recorded as a guardrail belay satisfied.
//
// # Which violations end the run, and why not all of them do
//
// The split is internal/policy's, by rule (see rules.Rule.Class), and this
// node only reads it. A forbidden path and a size ceiling end the run,
// because asking an agent to un-touch a file is not a repair anybody can
// perform; a missing test, a new dependency and forbidden content go to the
// fix loop, which is bounded by graph.give_up like any other repair.
//
// When both kinds are present, the run ends. The repairable ones are still
// recorded and archived — a person coming back to a stopped run wants the
// whole list, not the half that stopped it.
//
// # A run without a baseline gets a verdict, never an error
//
// The gate is registered only for a run whose manifest pinned a policy, so a
// run that started unguarded cannot reach it. Run still handles being reached
// without a pin, and handles it as a Result: the hazard it guards is a run
// that crashed with no policy configured and was resumed after somebody
// added one. Such a run never took a baseline, so the only honest thing to
// say is that nothing was checked — and it is said in the Note, with no
// Patch, because recording a passing verdict would be claiming a check that
// never happened. Erroring instead would end a run that has done nothing
// wrong, and end it again on every resume.
//
// # Persistence and re-runs
//
// Per ADR-0002 this node writes no state.json, manifest.json or journal. It
// returns a Patch carrying the Policy block and writes one artifact,
// policy-<step>.json, holding the full belay.QualityReport — the same
// contract the review node's artifact uses, because a policy violation is a
// verdict about code like any other (ADR-0006).
//
// It converges on re-run. The verdict is a pure function of the pinned rules
// and the workspace as it stands — its bytes, and what git or the run's file
// record says differs from the run's start — and the artifact is named by
// step, so the re-run the
// dispatcher performs after a crash inside this node rewrites the same file
// with the same content and reaches the same verdict as the attempt that
// crashed.
//
// It is deliberately not marked graph.Unpriced. This node calls no agent, so
// the budget check before it can never save a penny — but neither can the one
// before write or test, and neither of those is marked either. The marker is
// for a node delivering work a run has already paid for, and a gate in the
// middle of the graph is not that.
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	rules "github.com/dhaam-ai/belay/internal/policy"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// ErrNilRunContext reports a Run called with no RunContext at all, which is
// a wiring bug rather than anything about the change.
var ErrNilRunContext = errors.New("nodes/policy: nil run context")

// Node is belay's policy gate.
//
// It holds no state: the rules and the baseline arrive on the
// graph.RunContext, and the verdict leaves in the graph.Result. One Node is
// therefore safe to register once and re-run for every step of every run,
// which is what the dispatcher's crash-recovery contract requires, and safe
// for the concurrent use best-of-N fanout makes of every registered node.
type Node struct{}

// Node implements graph.Node. Asserted at compile time so a drift in the
// graph contract is a build failure here rather than a nil entry in the
// dispatcher's registry at runtime.
var _ graph.Node = (*Node)(nil)

// New returns a policy gate. It takes no options: unlike the review node,
// this one has a single successor for each outcome, fixed by the graph rather
// than by how a run delivers its result.
func New() *Node { return &Node{} }

// Name returns graph.NodePolicy.
func (n *Node) Name() string { return graph.NodePolicy }

// Run judges the run's change against the policy its manifest pinned. See
// the package documentation for the outcomes and what each one means.
func (n *Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc == nil {
		return graph.Result{Status: journal.StatusFailed}, ErrNilRunContext
	}
	if err := ctx.Err(); err != nil {
		return graph.Result{Status: journal.StatusAborted}, fmt.Errorf("nodes/policy: %w", err)
	}
	log := logger(rc)

	if rc.Policy == nil {
		log.Info("policy: this run has no baseline, so there is nothing to hold its change to")
		return graph.Result{
			Next:   graph.NodeTest,
			Status: journal.StatusOK,
			Note: "this run was started without a policy baseline, so belay has nothing to hold its " +
				"change to; a policy applies to the runs started after it is configured",
		}, nil
	}

	files, missed, why, err := judged(ctx, rc)
	if err != nil {
		// The workspace could not say what changed. That is no verdict,
		// never the agent's report standing in for one.
		return graph.Result{Status: journal.StatusFailed}, err
	}
	report, err := rules.Evaluate(ctx, rc.Policy.Policy, rc.Policy.Baseline, rules.Change{
		Workspace: rc.Workspace,
		Files:     files,
		FoldCase:  rc.Policy.FoldCase,
	})
	if err != nil {
		// No verdict, so no Patch and no artifact: the blackboard must not
		// end up holding a verdict nobody reached.
		return graph.Result{Status: journal.StatusFailed}, err
	}

	quality := report.QualityReport()
	path, err := archive(rc, quality)
	if err != nil {
		return graph.Result{Status: journal.StatusFailed}, err
	}
	block := state.NewPolicy(quality, path)
	patch := state.Patch{Policy: &block}

	checked := "; " + plural(len(files)) + " checked" + truthNote(missed, why)
	if len(missed) > 0 {
		log.Warn("policy: judged files the agent changed and did not report",
			slog.String("files", strings.Join(missed, ", ")))
	}
	if why != "" {
		log.Warn("policy: judging only the files the agent reports", slog.String("why", why))
	}
	switch {
	case report.Clean():
		log.Info("policy: the change obeys every rule", slog.String("artifact", path))
		return graph.Result{
			Next: graph.NodeTest, Status: journal.StatusOK, Patch: patch,
			Note: note(report, "the change obeys every rule"+checked),
		}, nil
	case report.Failed():
		log.Warn("policy: the change breaks a rule the run cannot continue past",
			slog.String("rules", broken(report)), slog.String("artifact", path))
		return graph.Result{
			Status: journal.StatusFailed, Patch: patch,
			Note: note(report, "the run cannot continue past "+broken(report)+checked),
		}, nil
	default:
		log.Info("policy: sending the change to be repaired",
			slog.String("rules", broken(report)), slog.String("artifact", path))
		return graph.Result{
			Next: graph.NodeFix, Status: journal.StatusOK, Patch: patch,
			Note: note(report, "asking for a repair of "+broken(report)+checked),
		}, nil
	}
}

// judged returns the files the gate judges: the run's recorded change, which
// the write node built from the agent's report and the workspace's own
// answer, together with the workspace's answer asked again here, so the
// gate never judges less than the workspace says changed whatever reached
// it. missed is every file in that answer the agent did not report.
//
// why is set, and nothing is asked, when the run keeps no record to measure
// against: then the gate judges what the agent reported, and says so in its
// verdict. Any other failure to answer is returned as an error.
func judged(ctx context.Context, rc *graph.RunContext) (files, missed []string, why string, err error) {
	recorded := rc.State.Code.ChangedFiles
	if rc.Changes == nil {
		return recorded, nil, "nothing checks the report against the workspace", nil
	}
	truth, err := rc.Changes.Changes(ctx, rc.Workspace)
	switch {
	case errors.Is(err, graph.ErrUnrecorded):
		return recorded, nil, strings.TrimPrefix(err.Error(), graph.ErrUnrecorded.Error()+": "), nil
	case err != nil:
		return nil, nil, "", fmt.Errorf("nodes/policy: read what the workspace changed: %w", err)
	}
	files = slices.Concat(recorded, truth)
	slices.Sort(files)
	files = slices.Compact(files)
	missed = slices.Clone(rc.State.Code.Unreported)
	for _, t := range truth {
		if !slices.Contains(recorded, t) {
			missed = append(missed, t)
		}
	}
	slices.Sort(missed)
	return files, slices.Compact(missed), "", nil
}

// truthNote says what the verdict rests on beyond the files themselves: the
// files the agent changed and did not report, which were judged all the
// same, or that the gate had only the agent's report, and why.
func truthNote(missed []string, why string) string {
	switch {
	case why != "":
		return "; policy judges the files the agent reports: " + why
	case len(missed) == 0:
		return ""
	}
	const named = 5
	list := strings.Join(missed[:min(len(missed), named)], ", ")
	if len(missed) > named {
		list += fmt.Sprintf(" and %d more", len(missed)-named)
	}
	return fmt.Sprintf("; %d the agent did not report (%s), judged all the same", len(missed), list)
}

// archive writes the full report beside the run's other evidence and returns
// its run-relative path.
//
// The artifact is named by step rather than by node, for the reason the write
// node's patch is: each execution occupies its own step, so the record of one
// pass of the fix loop is not overwritten by the next.
func archive(rc *graph.RunContext, q belay.QualityReport) (string, error) {
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return "", fmt.Errorf("nodes/policy: render the report: %w", err)
	}
	name := fmt.Sprintf("policy-%04d.json", max(rc.Step, 0))
	path, err := rc.WriteArtifact(name, append(data, '\n'))
	if err != nil {
		return "", fmt.Errorf("nodes/policy: record %s: %w", name, err)
	}
	return path, nil
}

// note composes the journal note: the report's own one-line summary, then
// what this node did about it.
func note(report rules.Report, action string) string {
	return report.Summary() + "; " + action
}

// broken names the rules that fired, each once, in the order internal/policy
// applies them.
//
// It names rules rather than counting them because the rule is what a person
// acts on: "policy:forbidden_path" sends them to one line of belay.yaml,
// while "2 violations" sends them to the archived report to find out which.
// Each is named once however many times it fired, so one rule breached by
// forty files is one thing to read.
func broken(report rules.Report) string {
	var fired []string
	for _, rule := range rules.Rules() {
		if slices.ContainsFunc(report.Violations, func(v rules.Violation) bool { return v.Rule == rule }) {
			fired = append(fired, string(rule))
		}
	}
	return strings.Join(fired, ", ")
}

// plural renders "1 file" or "3 files".
func plural(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// logger returns rc's logger, or a discarding default when the dispatcher
// did not scope one. A node must not panic on a partially populated
// RunContext.
func logger(rc *graph.RunContext) *slog.Logger {
	if rc.Logger != nil {
		return rc.Logger
	}
	return slog.New(slog.DiscardHandler)
}
