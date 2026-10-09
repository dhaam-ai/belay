package state

import (
	"github.com/dhaam-ai/belay/internal/policy"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// CapabilityPolicyGate is the capability a guarded run's manifest declares in
// its requires list (ADR-0011).
//
// Manifest.Policy is a control field twice over. It decides whether the run
// has a gate at all — an absent pin means no gate node is registered — and it
// carries the baseline the gate judges against. A belay that did not know it
// would decode a guarded run's manifest, drop the block, resume the run with
// every guardrail gone and say nothing, and then write the manifest back
// without it, so nothing afterwards could tell the run had ever been guarded.
// That is the failure this whole feature exists to prevent, arriving by a
// different door. Naming the capability turns it into a refusal that says to
// upgrade.
//
// Only a run that has a policy declares it, so an unguarded run — which is
// most of them — requires nothing and stays resumable by any belay that reads
// schema version 2.
const CapabilityPolicyGate = "policy.gate"

// PolicyPin is a run's guardrails: the rules it is held to, and the record of
// the workspace those rules are measured against.
//
// The two travel together because neither is usable without the other. Rules
// with no baseline cannot judge a new dependency; a baseline with no rules is
// a survey nobody asked for. Pinning them as one value also means there is
// exactly one question to ask about a run — does it have a pin — rather than
// two that could disagree.
type PolicyPin struct {
	// Policy is the rule set, as policy.From built it from the policy
	// section of the configuration the run was started with.
	Policy policy.Policy `json:"policy"`
	// Baseline is what the workspace declared when the run was created.
	Baseline policy.Baseline `json:"baseline"`
	// FoldCase says the workspace's file system treats two names that
	// differ only in case as one file, as macOS does by default, so the
	// rules match paths without regard to case. It is decided when the run
	// is created, by asking the file system the run directory is on.
	FoldCase bool `json:"fold_case,omitempty"`
}

// SetPolicy pins the guardrails this run's change must obey, and records the
// capability that pin depends on.
//
// It is one call rather than two for the reason SetOutput and SetFanout are:
// the field and its capability are one decision, and a caller that assigned
// Manifest.Policy directly would write a manifest an older belay silently
// strips the guardrails out of, with nothing to catch it until that run was
// resumed unguarded. See CapabilityPolicyGate.
func (m *Manifest) SetPolicy(p PolicyPin) {
	m.Policy = &p
	m.Requires = addCapability(m.Requires, CapabilityPolicyGate)
}

// Policy is the blackboard's projection of the policy gate's verdict: every
// field the graph acts on, and where the full report was archived.
//
// It is deliberately the same shape as Review, because it is the same kind of
// thing: a verdict about the code, expressed through belay's one quality
// report contract (ADR-0006). The fix node's prompt builder renders a
// belay.Issue whatever produced it, so a policy violation reaches an agent in
// the same words a lint finding does.
//
// Unlike Review it carries no Source. A quality gate has several possible
// reviewers and has to say which one spoke; there is only ever one policy
// engine, and a field that could hold only policy.Source would be a column of
// one repeated value in every state.json belay writes.
type Policy struct {
	// Gate is the verdict: belay.GatePass for a change that broke nothing,
	// belay.GateFail for one that broke something. It is never
	// belay.GateError — a policy belay could not evaluate is no verdict at
	// all, which the gate node reports as an error rather than recording
	// here.
	Gate belay.GateStatus `json:"gate"`
	// Violations is every violation the gate found, as belay.Issue, whose
	// RuleID is the policy.Rule that fired. Empty, never nil, for a change
	// that broke nothing.
	Violations []belay.Issue `json:"violations"`
	// Counts summarizes Violations by severity, which is how a reader tells
	// a run stopped by a guardrail from one merely asked to repair
	// something: a fail-class violation is the only kind that is a blocker.
	Counts belay.Counts `json:"counts"`
	// Summary is the gate's one-line synopsis, carried through verbatim.
	Summary string `json:"summary"`
	// ReportPath is the run-relative path of the archived full report,
	// mirroring Test.ReportPath and Review.ReportPath.
	ReportPath string `json:"report_path"`
}

// NewPolicy projects q onto the blackboard's Policy shape, recording where
// the full report was archived. Pass an empty reportPath when the report was
// not written to disk.
func NewPolicy(q belay.QualityReport, reportPath string) Policy {
	return Policy{
		Gate:       q.Gate,
		Violations: cloneSlice(q.Issues),
		Counts:     q.Counts,
		Summary:    q.Summary,
		ReportPath: reportPath,
	}
}

// QualityReport reconstructs the belay.QualityReport p was projected from,
// preserving every field the graph acts on. Raw is not part of the projection
// and always comes back nil; Violations comes back non-nil even if p's was
// nil, matching belay.QualityReport.Issues's own "never nil" contract.
func (p Policy) QualityReport() belay.QualityReport {
	issues := cloneSlice(p.Violations)
	if issues == nil {
		issues = []belay.Issue{}
	}
	return belay.QualityReport{
		Source:  policy.Source,
		Gate:    p.Gate,
		Counts:  p.Counts,
		Issues:  issues,
		Summary: p.Summary,
	}
}
