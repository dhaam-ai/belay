//go:build unix

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/belay-dev/belay/pkg/belay"
)

// This file provides typed wrappers for the five SonarQube MCP tools this
// package supports, and the mapping from their responses to
// belay.QualityReport that a Reviewer (T32) builds on top of Client.
//
// # Parameter names
//
// Every request struct's JSON field names match the parameter names
// documented at https://docs.sonarsource.com/sonarqube-mcp-server/reference/tools
// (verified 2026-09), not names inferred from SonarQube's older Web API —
// the two have diverged (for example the search tool's project filter is
// "projectKeys", plural, where the legacy api/issues/search endpoint uses
// "componentKeys"). Response field names, which that page does not fully
// document, follow the corresponding SonarQube Web API response shapes
// (api/qualitygates/list, api/qualitygates/project_status,
// api/issues/search), since the MCP tools are documented as thin wrappers
// over the same server-side operations. This is a stated assumption, not a
// verified fact: this package was written and tested with no access to a
// live SonarQube MCP server (see the package doc and the acceptance
// report), and every response type tolerates unknown fields so a real
// server's actual shape can drift from this guess without breaking
// decoding — only the fields this package actually reads need to match.
//
// # Field mapping to belay.QualityReport (for T32)
//
//   - QualityReport.Gate: BelayGate(GetProjectQualityGateStatusResponse.
//     ProjectStatus.Status). "OK" is GatePass; "ERROR" is GateFail, never
//     GateError — see BelayGate's doc comment for why that distinction is
//     the one bug belay.GateError itself warns adapters about; anything
//     else (including "NONE", meaning no analysis has run) is GateError.
//   - QualityReport.Counts: the severity histogram of QualityReport.Issues
//     below, exactly as internal/linter's newReport computes it — this
//     package does not compute Counts itself, since belay.Counts has no
//     public constructor and T32 already has to build one to satisfy
//     belay.Reviewer for a deterministic adapter too.
//   - QualityReport.Issues: one belay.Issue per SonarIssue, gathered from
//     whichever of AnalyzeCodeSnippetResponse.Issues and
//     SearchSonarIssuesInProjectsResponse.Issues the reviewer chose to
//     call. Per issue:
//     -- RuleID = SonarIssue.Rule, verbatim (opaque, as belay.Issue.RuleID
//     documents).
//     -- Severity = SonarIssue.BelaySeverity().
//     -- File = RelativeFile(SonarIssue.Component, projectKey).
//     -- Line = SonarIssue.Line (0 for a project- or file-level finding
//     with no line, which already matches belay.Issue.Line's zero-means-
//     unattributed contract).
//     -- Message = SonarIssue.Message, verbatim.
//     -- Effort = SonarIssue.Effort, verbatim (already an opaque,
//     tool-native estimate, per belay.Issue.Effort).
//   - QualityReport.Summary: not produced by this package; T32 renders one
//     from the assembled Counts and Gate, exactly as
//     internal/linter/linter.go's summarize does for the deterministic
//     adapters, so the two Reviewer families read the same at a glance.
//   - QualityReport.Raw: the reviewer's choice of which underlying
//     response(s) informed the report. Every response type here exposes
//     its own Raw ([]byte of the exact tools/call result), so a reviewer
//     that queries multiple tools per review can compose them into one
//     JSON object (for example {"quality_gate": <raw>, "issues": <raw>})
//     — this package takes no position on that shape, since
//     belay.QualityReport.Raw is documented as legitimately differing
//     between adapters and never read by the graph.
//   - QualityReport.Source: the reviewer's own identifier (for example
//     "sonar-mcp-ai-review"), not derived from anything in this package.
//
// Every slice-typed response field is normalized through nonNilSlice
// before being returned, so an empty result from the server (an issue-free
// search, a project with no quality gate conditions) still round-trips to
// belay.QualityReport.Issues's documented "never nil" guarantee once T32
// assembles it, rather than requiring T32 to remember to do that
// normalization itself.

// Tool names, exactly as advertised by tools/list and required by
// Client.CallTool's advertised-tool check.
const (
	toolListQualityGates            = "list_quality_gates"
	toolGetRawSource                = "get_raw_source"
	toolAnalyzeCodeSnippet          = "analyze_code_snippet"
	toolGetProjectQualityGateStatus = "get_project_quality_gate_status"
	toolSearchSonarIssuesInProjects = "search_sonar_issues_in_projects"
)

// SonarImpact is one entry of SonarIssue.Impacts: SonarQube's "Clean Code"
// severity taxonomy, which scores an issue's severity per software quality
// it affects rather than with one flat severity.
type SonarImpact struct {
	// SoftwareQuality is one of MAINTAINABILITY, RELIABILITY, SECURITY.
	SoftwareQuality string `json:"softwareQuality"`
	// Severity is one of INFO, LOW, MEDIUM, HIGH, BLOCKER.
	Severity string `json:"severity"`
}

// SonarIssue is one issue as returned by SonarQube's issue search,
// surfaced by both AnalyzeCodeSnippet and SearchSonarIssuesInProjects.
//
// Only the fields this package or a Reviewer built on it needs are
// modeled; SonarQube's actual issue objects carry many more (tags, author,
// creation date, and so on), which are silently ignored on decode and
// remain available in the response's Raw bytes for a caller that wants
// them.
type SonarIssue struct {
	// Key is SonarQube's opaque issue identifier.
	Key string `json:"key"`
	// Rule is the rule key that fired, such as "python:S1481". Carried
	// through verbatim as belay.Issue.RuleID.
	Rule string `json:"rule"`
	// Severity is the legacy severity vocabulary (BLOCKER, CRITICAL,
	// MAJOR, MINOR, INFO). May be empty on a server that reports only
	// Impacts for this issue; see BelaySeverity.
	Severity string `json:"severity"`
	// Impacts is the Clean Code severity taxonomy: zero or more
	// (softwareQuality, severity) pairs. See BelaySeverity.
	Impacts []SonarImpact `json:"impacts"`
	// Component is SonarQube's component key, typically
	// "<projectKey>:<path>". See RelativeFile.
	Component string `json:"component"`
	// Line is the 1-indexed line the issue was reported at, or 0 for a
	// project- or file-level finding not attributable to one line.
	Line int `json:"line"`
	// Message is the human-readable description of the issue.
	Message string `json:"message"`
	// Effort is SonarQube's opaque remediation-effort estimate, such as
	// "5min", or empty if not estimated.
	Effort string `json:"effort"`
	// Status is the issue's workflow status, such as "OPEN" or "FIXED".
	Status string `json:"status"`
}

// cleanCodeSeverity maps SonarQube's Clean Code taxonomy (the values
// search_sonar_issues_in_projects's own "severities" parameter documents)
// to belay.Severity. The two are both five-level ordinal scales in the
// same worst-to-least order, so the mapping is a direct correspondence
// rather than a judgment call.
var cleanCodeSeverity = map[string]belay.Severity{
	"INFO":    belay.SeverityInfo,
	"LOW":     belay.SeverityMinor,
	"MEDIUM":  belay.SeverityMajor,
	"HIGH":    belay.SeverityCritical,
	"BLOCKER": belay.SeverityBlocker,
}

// legacySeverity maps SonarQube's older severity vocabulary to
// belay.Severity, consulted only when an issue carries no Impacts.
var legacySeverity = map[string]belay.Severity{
	"INFO":     belay.SeverityInfo,
	"MINOR":    belay.SeverityMinor,
	"MAJOR":    belay.SeverityMajor,
	"CRITICAL": belay.SeverityCritical,
	"BLOCKER":  belay.SeverityBlocker,
}

// BelaySeverity maps this issue's severity to belay's five-level ordering,
// for a Reviewer assembling a belay.QualityReport from this package's
// responses.
//
// It prefers Impacts over the legacy Severity field: a modern SonarQube
// server reports Impacts on every issue and may leave Severity empty, and
// impactSoftwareQualities/severities are the vocabulary
// search_sonar_issues_in_projects's own parameters document, so it is the
// forward-looking source of truth. An issue can carry more than one
// Impact — it can affect Reliability and Security at different severities
// at once — so BelaySeverity returns the worst of them, matching
// review.fail_on's "does anything here cross the line" semantics: an issue
// this package under-reports would let a real problem through, while
// over-reporting only costs an extra look. Legacy Severity is consulted
// only when Impacts is empty. An unrecognized value in either vocabulary
// resolves to belay.SeverityInfo, the same "do not trust an unrecognized
// signal to stop a run" choice internal/linter's defaultGolangCISeverity
// documents for the same reason.
func (i SonarIssue) BelaySeverity() belay.Severity {
	if len(i.Impacts) > 0 {
		worst := belay.SeverityInfo
		for _, imp := range i.Impacts {
			if s, ok := cleanCodeSeverity[strings.ToUpper(strings.TrimSpace(imp.Severity))]; ok && s > worst {
				worst = s
			}
		}
		return worst
	}
	if s, ok := legacySeverity[strings.ToUpper(strings.TrimSpace(i.Severity))]; ok {
		return s
	}
	return belay.SeverityInfo
}

// RelativeFile strips componentKey's leading "<projectKey>:" prefix, so
// belay.Issue.File — documented as relative to the reviewed directory —
// is a path rather than a raw SonarQube component key. If componentKey
// does not start with projectKey+":", it is returned unchanged:
// belay.Issue.File must never fabricate a plausible-looking wrong path.
func RelativeFile(componentKey, projectKey string) string {
	if projectKey == "" {
		return componentKey
	}
	if rest, ok := strings.CutPrefix(componentKey, projectKey+":"); ok {
		return rest
	}
	return componentKey
}

// BelayGate maps a SonarQube project quality gate status string to
// belay.GateStatus.
//
// "OK" is GatePass and "ERROR" is GateFail — never GateError. This is the
// exact trap belay.GateError's own doc comment warns Sonar-backed adapters
// about: SonarQube's "ERROR" string means the gate was evaluated and
// failed, which is belay's GateFail, not "the gate computation itself
// broke". GateError is reserved for "NONE" (no analysis has run, so there
// is no verdict to report at all) and any value this package does not
// recognize.
func BelayGate(status string) belay.GateStatus {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "OK":
		return belay.GatePass
	case "ERROR":
		return belay.GateFail
	default:
		return belay.GateError
	}
}

// ListQualityGatesRequest is the input to ListQualityGates. The underlying
// tool takes no parameters.
type ListQualityGatesRequest struct{}

// QualityGate is one entry of a list_quality_gates response.
type QualityGate struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
	IsBuiltIn bool   `json:"isBuiltIn"`
}

// ListQualityGatesResponse is the decoded result of list_quality_gates.
type ListQualityGatesResponse struct {
	QualityGates []QualityGate `json:"qualityGates"`
	// Raw is the exact bytes of the tools/call result.
	Raw json.RawMessage `json:"-"`
}

// ListQualityGates calls the list_quality_gates tool.
func (c *Client) ListQualityGates(ctx context.Context, _ ListQualityGatesRequest) (ListQualityGatesResponse, error) {
	result, err := c.CallTool(ctx, toolListQualityGates, struct{}{})
	if err != nil {
		return ListQualityGatesResponse{}, err
	}
	var resp ListQualityGatesResponse
	if err := result.JSON(&resp); err != nil {
		return ListQualityGatesResponse{}, fmt.Errorf("belay/sonar/mcp: %s: %w", toolListQualityGates, err)
	}
	resp.QualityGates = nonNilSlice(resp.QualityGates)
	resp.Raw = result.Raw
	return resp, nil
}

// GetRawSourceRequest is the input to GetRawSource.
type GetRawSourceRequest struct {
	// Key is the SonarQube file key. Required.
	Key string `json:"key"`
	// Branch optionally names a branch to read the file from.
	Branch string `json:"branch,omitempty"`
	// PullRequest optionally names a pull request to read the file from.
	PullRequest string `json:"pullRequest,omitempty"`
}

// GetRawSourceResponse is the decoded result of get_raw_source.
//
// Unlike the other four tools, get_raw_source's payload is the file's raw
// text, not JSON, so Source is populated from ToolResult.Text() rather
// than ToolResult.JSON.
type GetRawSourceResponse struct {
	// Source is the file's raw source text.
	Source string `json:"-"`
	// Raw is the exact bytes of the tools/call result.
	Raw json.RawMessage `json:"-"`
}

// GetRawSource calls the get_raw_source tool.
func (c *Client) GetRawSource(ctx context.Context, req GetRawSourceRequest) (GetRawSourceResponse, error) {
	if strings.TrimSpace(req.Key) == "" {
		return GetRawSourceResponse{}, fmt.Errorf("belay/sonar/mcp: %s: Key is required", toolGetRawSource)
	}
	result, err := c.CallTool(ctx, toolGetRawSource, req)
	if err != nil {
		return GetRawSourceResponse{}, err
	}
	return GetRawSourceResponse{Source: result.Text(), Raw: result.Raw}, nil
}

// AnalyzeCodeSnippetRequest is the input to AnalyzeCodeSnippet. Every field
// is optional per the tool's own documentation, though a real analysis
// needs at least FileContent or CodeSnippet plus Language.
type AnalyzeCodeSnippetRequest struct {
	CodeSnippet string `json:"codeSnippet,omitempty"`
	FileContent string `json:"fileContent,omitempty"`
	FilePath    string `json:"filePath,omitempty"`
	Language    string `json:"language,omitempty"`
	ProjectKey  string `json:"projectKey,omitempty"`
	// Scope is "MAIN" or "TEST"; empty defaults to MAIN server-side.
	Scope string `json:"scope,omitempty"`
}

// AnalyzeCodeSnippetResponse is the decoded result of analyze_code_snippet.
type AnalyzeCodeSnippetResponse struct {
	Issues []SonarIssue `json:"issues"`
	// DeprecationNotice is the tool's documented deprecation notice field,
	// carried through verbatim when present; empty otherwise.
	DeprecationNotice string `json:"deprecationNotice"`
	// Raw is the exact bytes of the tools/call result.
	Raw json.RawMessage `json:"-"`
}

// AnalyzeCodeSnippet calls the analyze_code_snippet tool.
func (c *Client) AnalyzeCodeSnippet(ctx context.Context, req AnalyzeCodeSnippetRequest) (AnalyzeCodeSnippetResponse, error) {
	result, err := c.CallTool(ctx, toolAnalyzeCodeSnippet, req)
	if err != nil {
		return AnalyzeCodeSnippetResponse{}, err
	}
	var resp AnalyzeCodeSnippetResponse
	if err := result.JSON(&resp); err != nil {
		return AnalyzeCodeSnippetResponse{}, fmt.Errorf("belay/sonar/mcp: %s: %w", toolAnalyzeCodeSnippet, err)
	}
	resp.Issues = nonNilSlice(resp.Issues)
	resp.Raw = result.Raw
	return resp, nil
}

// GetProjectQualityGateStatusRequest is the input to
// GetProjectQualityGateStatus. ProjectKey (or ProjectID) identifies the
// project; Branch/PullRequest narrow to a specific analysis.
type GetProjectQualityGateStatusRequest struct {
	AnalysisID  string `json:"analysisId,omitempty"`
	Branch      string `json:"branch,omitempty"`
	ProjectID   string `json:"projectId,omitempty"`
	ProjectKey  string `json:"projectKey,omitempty"`
	PullRequest string `json:"pullRequest,omitempty"`
}

// QualityGateCondition is one condition of a project's quality gate
// evaluation, such as "new_reliability_rating must be better than A".
type QualityGateCondition struct {
	Status         string `json:"status"`
	MetricKey      string `json:"metricKey"`
	Comparator     string `json:"comparator"`
	ErrorThreshold string `json:"errorThreshold"`
	ActualValue    string `json:"actualValue"`
}

// ProjectStatus is the quality gate verdict for one project analysis.
type ProjectStatus struct {
	// Status is "OK", "ERROR", or "NONE". See BelayGate.
	Status            string                 `json:"status"`
	Conditions        []QualityGateCondition `json:"conditions"`
	IgnoredConditions bool                   `json:"ignoredConditions"`
}

// GetProjectQualityGateStatusResponse is the decoded result of
// get_project_quality_gate_status.
type GetProjectQualityGateStatusResponse struct {
	ProjectStatus ProjectStatus `json:"projectStatus"`
	// Raw is the exact bytes of the tools/call result.
	Raw json.RawMessage `json:"-"`
}

// GetProjectQualityGateStatus calls the get_project_quality_gate_status
// tool.
func (c *Client) GetProjectQualityGateStatus(ctx context.Context, req GetProjectQualityGateStatusRequest) (GetProjectQualityGateStatusResponse, error) {
	result, err := c.CallTool(ctx, toolGetProjectQualityGateStatus, req)
	if err != nil {
		return GetProjectQualityGateStatusResponse{}, err
	}
	var resp GetProjectQualityGateStatusResponse
	if err := result.JSON(&resp); err != nil {
		return GetProjectQualityGateStatusResponse{}, fmt.Errorf("belay/sonar/mcp: %s: %w", toolGetProjectQualityGateStatus, err)
	}
	resp.ProjectStatus.Conditions = nonNilSlice(resp.ProjectStatus.Conditions)
	resp.Raw = result.Raw
	return resp, nil
}

// SearchSonarIssuesInProjectsRequest is the input to
// SearchSonarIssuesInProjects. Every field is optional; an empty request
// searches every project the credentials can see.
type SearchSonarIssuesInProjectsRequest struct {
	IssueStatuses           []string `json:"issueStatuses,omitempty"`
	InNewCodePeriod         bool     `json:"inNewCodePeriod,omitempty"`
	IssueKey                string   `json:"issueKey,omitempty"`
	ImpactSoftwareQualities []string `json:"impactSoftwareQualities,omitempty"`
	// PageIndex is 1-based; omitted (zero) lets the server default to 1.
	PageIndex   int      `json:"pageIndex,omitempty"`
	ProjectKeys []string `json:"projectKeys,omitempty"`
	// PageSize must be in (0, 500]; omitted (zero) lets the server
	// default to 100.
	PageSize    int      `json:"pageSize,omitempty"`
	Branch      string   `json:"branch,omitempty"`
	PullRequest string   `json:"pullRequest,omitempty"`
	Severities  []string `json:"severities,omitempty"`
}

// SearchSonarIssuesInProjectsResponse is the decoded result of
// search_sonar_issues_in_projects.
type SearchSonarIssuesInProjectsResponse struct {
	Total     int          `json:"total"`
	PageIndex int          `json:"p"`
	PageSize  int          `json:"ps"`
	Issues    []SonarIssue `json:"issues"`
	// Raw is the exact bytes of the tools/call result.
	Raw json.RawMessage `json:"-"`
}

// SearchSonarIssuesInProjects calls the search_sonar_issues_in_projects
// tool.
func (c *Client) SearchSonarIssuesInProjects(ctx context.Context, req SearchSonarIssuesInProjectsRequest) (SearchSonarIssuesInProjectsResponse, error) {
	result, err := c.CallTool(ctx, toolSearchSonarIssuesInProjects, req)
	if err != nil {
		return SearchSonarIssuesInProjectsResponse{}, err
	}
	var resp SearchSonarIssuesInProjectsResponse
	if err := result.JSON(&resp); err != nil {
		return SearchSonarIssuesInProjectsResponse{}, fmt.Errorf("belay/sonar/mcp: %s: %w", toolSearchSonarIssuesInProjects, err)
	}
	resp.Issues = nonNilSlice(resp.Issues)
	resp.Raw = result.Raw
	return resp, nil
}
