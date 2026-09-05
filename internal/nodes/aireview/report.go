//go:build unix

package aireview

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/belay-dev/belay/internal/sonar/mcp"
	"github.com/belay-dev/belay/pkg/belay"
)

// Source is the belay.QualityReport.Source every report this package
// produces carries, on every path including errors.
//
// It is the one field a conformance suite comparing this package's reports
// against internal/sonar/scanner's is expected to see differ; every other
// top-level key, and every rule below, is deliberately identical.
const Source = "sonar-mcp-ai-review"

// GateRuleID is the belay.Issue.RuleID of the synthesized issue that stands
// in for a server-side quality gate that failed on conditions no individual
// issue explains — coverage on new code, duplicated lines, a security
// hotspot review percentage.
//
// It is shared, verbatim, with internal/sonar/scanner's GateRuleID: the two
// adapters synthesize the same issue for the same reason, and a human or a
// downstream tool filtering it out should not have to know which reviewer
// produced the report. It is deliberately not shaped like a SonarQube rule
// key (which look like "go:S1005"), so nobody mistakes it for a finding
// SonarQube actually reported.
const GateRuleID = "belay:sonar-quality-gate"

// RawReview is the shape of belay.QualityReport.Raw for every report this
// package produces.
//
// belay.QualityReport documents Raw as the adapter's own tool-specific
// output, which the graph never reads (ADR 0006); it exists for logs,
// debugging and human forensics. Everything needed to reconstruct why a
// verdict was reached is here — most importantly the server's own gate
// word, which is not recoverable from Gate alone once belay's own fail_on
// threshold has been applied on top of it.
//
// Every string this struct carries originates either in belay's own
// configuration or in an error string internal/sonar/mcp's transports have
// already passed through an internal/exec.Redactor, so a SONAR_TOKEN a
// misbehaving server echoed back cannot reach an artifact through here.
type RawReview struct {
	// Mode is the config.MCPMode the session was opened with, or empty
	// when the Session was injected rather than dialed.
	Mode string `json:"mode"`
	// Endpoint is the MCP URL or container image the session used, or
	// empty for an injected Session. Never carries credentials: the token
	// travels in a header or a child environment variable, never in a URL
	// or on argv (see internal/sonar/mcp).
	Endpoint string `json:"endpoint"`
	// ProjectKey is the SonarQube project the review asked about.
	ProjectKey string `json:"project_key"`
	// ChangedFiles is belay.ReviewRequest.ChangedFiles, recorded but not
	// used as a filter. See the package doc.
	ChangedFiles []string `json:"changed_files"`
	// ServerStatus is SonarQube's own project status word — "OK",
	// "ERROR" or "NONE" — verbatim, or empty if it was never obtained.
	ServerStatus string `json:"server_status"`
	// ServerGate is the belay.GateStatus ServerStatus mapped to, before
	// belay's own fail_on threshold was applied.
	ServerGate string `json:"server_gate"`
	// Conditions is every quality gate condition the server evaluated.
	// Never nil.
	Conditions []mcp.QualityGateCondition `json:"conditions"`
	// IssuesTotal is the issue count the server reported for the search,
	// which may exceed IssuesFetched when Truncated is true.
	IssuesTotal int `json:"issues_total"`
	// IssuesFetched is how many issues were actually collected and mapped
	// into belay.QualityReport.Issues.
	IssuesFetched int `json:"issues_fetched"`
	// Pages is how many search pages were requested.
	Pages int `json:"pages"`
	// Truncated reports that the page cap was reached with more issues
	// outstanding. See the package doc for what that does and does not
	// put at risk.
	Truncated bool `json:"truncated"`
	// SyntheticGateIssue reports that the single GateRuleID issue was
	// added because the server failed the gate on conditions that produced
	// no issue at or above fail_on.
	SyntheticGateIssue bool `json:"synthetic_gate_issue"`
	// Gate is the belay.GateStatus finally reported, after fail_on.
	Gate string `json:"gate"`
	// DurationMS is the review's wall-clock time in milliseconds.
	DurationMS int64 `json:"duration_ms"`
	// Error is the rendered error, or empty on a pass or a failed gate.
	Error string `json:"error"`
	// QualityGateRaw is the exact tools/call result of
	// get_project_quality_gate_status, or JSON null if it was never
	// obtained.
	QualityGateRaw json.RawMessage `json:"quality_gate"`
	// IssuesRaw is the exact tools/call result of every
	// search_sonar_issues_in_projects page, in page order. Never nil.
	IssuesRaw []json.RawMessage `json:"issues"`
}

// finalize is the single constructor for every belay.QualityReport this
// package returns — the success paths, the "the tool ran but reached no
// verdict" path, and every error path alike.
//
// Routing every path through one constructor is what makes the
// field-population rules below true by construction rather than by each
// caller remembering them. They are, verbatim, the rules
// internal/sonar/scanner documents for itself, because a conformance suite
// asserts the two adapters' reports serialize to the same top-level key set
// with the same value shapes:
//
//	Source:  the Source constant, always.
//	Issues:  non-nil, always — make+copy, never append([]T(nil), ...).
//	Counts:  exactly the severity histogram of Issues.
//	Raw:     non-nil, valid JSON, always — an object described by RawReview.
//	Summary: a one-line human synopsis, never empty.
//	Gate:    GatePass, GateFail or GateError; never GateUnknown.
//
// # How serverGate and failOn combine
//
// belay.Reviewer requires Gate == GatePass if and only if
// Counts.AtOrAbove(failOn) == 0, and SonarQube's server-side gate knows
// nothing about belay's fail_on. finalize reconciles the two without
// letting either silently win:
//
//   - A server gate that reached no verdict (GateError, from status "NONE"
//     or an unrecognized word) stays GateError. It is the absence of a
//     verdict, and Counts cannot tell "measured, found nothing" apart from
//     "measured nothing at all"; recomputing it from an empty issue list
//     would turn a broken analysis into a clean pass.
//   - A server gate that failed while no fetched issue reaches failOn gets
//     one synthesized GateRuleID issue at belay.SeverityBlocker. A
//     SonarQube gate can fail on conditions no issue explains — coverage,
//     duplication — and dropping that to a pass because belay saw no
//     qualifying issue would ship code the project's own policy rejects.
//     Blocker, rather than a severity derived from failOn, keeps the
//     invariant true for every threshold without making the report's
//     contents depend on the caller's setting, exactly as
//     internal/sonar/scanner's gateIssue does.
//   - Otherwise the threshold decides: any issue at or above failOn is a
//     fail, none is a pass. A project whose server gate passes can still
//     fail belay's stricter fail_on, which is the whole point of the
//     setting.
func finalize(serverStatus string, issues []belay.Issue, raw RawReview, failOn belay.Severity, cause error) belay.QualityReport {
	out := cloneIssues(issues)
	serverGate := mcp.BelayGate(serverStatus)

	var gate belay.GateStatus
	switch {
	case cause != nil, serverGate == belay.GateError, serverGate == belay.GateUnknown:
		// No verdict was reached. Preserve that as GateError rather than
		// deriving a pass or a fail from an issue list nobody can trust.
		gate = belay.GateError
	default:
		if serverGate == belay.GateFail && countIssues(out).AtOrAbove(failOn) == 0 {
			out = append(out, gateIssue(raw))
			raw.SyntheticGateIssue = true
		}
		gate = belay.GatePass
		if countIssues(out).AtOrAbove(failOn) > 0 {
			gate = belay.GateFail
		}
	}

	counts := countIssues(out)
	raw.ServerStatus = serverStatus
	raw.ServerGate = serverGate.String()
	raw.Gate = gate.String()
	raw.IssuesFetched = len(issues)
	if cause != nil {
		raw.Error = cause.Error()
	}

	return belay.QualityReport{
		Source:  Source,
		Gate:    gate,
		Counts:  counts,
		Issues:  out,
		Summary: summarize(gate, serverStatus, counts, failOn, cause),
		Raw:     marshalRaw(raw),
	}
}

// gateIssue synthesizes the single issue that stands in for a failed
// server-side quality gate.
//
// It is a representation of the server's verdict, not an invented finding,
// and every part of it says so: RuleID is belay's own GateRuleID rather than
// a SonarQube rule key, the message states plainly that the failing
// conditions live on the server, and File and Line are left empty and zero
// because a project-level gate is not attributable to one line.
func gateIssue(raw RawReview) belay.Issue {
	msg := "SonarQube failed the project quality gate on conditions that produced no issue at or above " +
		"belay's fail_on threshold (typically coverage, duplication or a hotspot review percentage). " +
		"The failing conditions are on the SonarQube server"
	if raw.ProjectKey != "" {
		msg += " for project " + raw.ProjectKey
	}
	return belay.Issue{
		RuleID:   GateRuleID,
		Severity: belay.SeverityBlocker,
		Message:  msg + ".",
	}
}

// mapIssue turns one SonarQube issue into a belay.Issue, following the
// mapping internal/sonar/mcp's tools.go documents for this package field for
// field: RuleID, Message and Effort verbatim, Severity through
// SonarIssue.BelaySeverity, File through mcp.RelativeFile, and Line straight
// across (0 already means "not attributable to one line" on both sides).
func mapIssue(si mcp.SonarIssue, projectKey string) belay.Issue {
	return belay.Issue{
		RuleID:   si.Rule,
		Severity: si.BelaySeverity(),
		File:     mcp.RelativeFile(si.Component, projectKey),
		Line:     si.Line,
		Message:  si.Message,
		Effort:   si.Effort,
	}
}

// cloneIssues returns a non-nil copy of src.
//
// make-and-copy rather than append([]belay.Issue(nil), src...), which
// collapses a non-nil empty slice back to nil and would serialize "issues"
// as null for the emptiest, most-fixtured report of all. internal/state
// shipped exactly that bug.
func cloneIssues(src []belay.Issue) []belay.Issue {
	out := make([]belay.Issue, len(src))
	copy(out, src)
	return out
}

// countIssues builds the severity histogram belay.QualityReport.Counts is
// contractually required to be.
//
// The default arm counts an out-of-range Severity as Info rather than
// dropping it, so Counts.Total always equals len(issues): a count that
// silently disagreed with the slice it summarizes would be worse than a
// mis-bucketed issue, and Info is the arm that cannot cause a spurious gate
// failure.
func countIssues(issues []belay.Issue) belay.Counts {
	var c belay.Counts
	for _, issue := range issues {
		switch issue.Severity {
		case belay.SeverityBlocker:
			c.Blocker++
		case belay.SeverityCritical:
			c.Critical++
		case belay.SeverityMajor:
			c.Major++
		case belay.SeverityMinor:
			c.Minor++
		case belay.SeverityInfo:
			c.Info++
		default:
			c.Info++
		}
	}
	return c
}

// summarize renders the one-line human synopsis carried in
// belay.QualityReport.Summary. It is never empty, on any path.
//
// It is redacted by construction on the pass and fail arms — every part is a
// constant, a severity count, a threshold or SonarQube's own one-word status
// — and on the error arm carries only an error string internal/sonar/mcp has
// already put through internal/exec's redactor.
func summarize(gate belay.GateStatus, serverStatus string, c belay.Counts, failOn belay.Severity, cause error) string {
	switch gate {
	case belay.GatePass:
		return fmt.Sprintf("%s: server quality gate %s, %d issue(s) found, none at or above fail_on=%s; gate pass",
			Source, strings.ToLower(orUnknown(serverStatus)), c.Total(), failOn)
	case belay.GateFail:
		return fmt.Sprintf("%s: server quality gate %s, %d of %d issue(s) at or above fail_on=%s (%s); gate fail",
			Source, strings.ToLower(orUnknown(serverStatus)), c.AtOrAbove(failOn), c.Total(), failOn, breakdown(c, failOn))
	default:
		reason := "the server reported status " + orUnknown(serverStatus) + ", which is not a gate verdict"
		if cause != nil {
			reason = cause.Error()
		}
		return Source + ": could not produce a quality gate verdict; gate error: " + reason
	}
}

// breakdown renders the non-zero severity counts at or above failOn, worst
// first: "1 blocker, 3 major". Counts below the threshold are omitted
// because they did not contribute to the verdict the summary is explaining.
func breakdown(c belay.Counts, failOn belay.Severity) string {
	tiers := []struct {
		sev belay.Severity
		n   int
	}{
		{belay.SeverityBlocker, c.Blocker},
		{belay.SeverityCritical, c.Critical},
		{belay.SeverityMajor, c.Major},
		{belay.SeverityMinor, c.Minor},
		{belay.SeverityInfo, c.Info},
	}
	var parts []string
	for _, t := range tiers {
		if t.sev >= failOn && t.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", t.n, t.sev))
		}
	}
	if len(parts) == 0 {
		return "no issues at or above the threshold"
	}
	return strings.Join(parts, ", ")
}

// orUnknown substitutes a placeholder for an empty status word, so a
// summary never reads "quality gate ; gate error".
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// marshalRaw renders raw as belay.QualityReport.Raw.
//
// Raw is documented as always present, so a marshal failure falls back to a
// valid JSON object carrying the marshal error rather than to nil, which
// would serialize the field as null and break the identical-shape guarantee
// belay.QualityReport makes across adapters. The nil-slice normalization
// above it is the same rule applied one level down: "conditions": null and
// "issues": null inside Raw would be just as wrong.
func marshalRaw(raw RawReview) json.RawMessage {
	if raw.ChangedFiles == nil {
		raw.ChangedFiles = []string{}
	}
	if raw.Conditions == nil {
		raw.Conditions = []mcp.QualityGateCondition{}
	}
	if raw.IssuesRaw == nil {
		raw.IssuesRaw = []json.RawMessage{}
	}
	if len(raw.QualityGateRaw) == 0 {
		raw.QualityGateRaw = json.RawMessage("null")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return json.RawMessage(`{"error":` + strconv.Quote(Source+": raw output could not be encoded: "+err.Error()) + `}`)
	}
	return b
}
