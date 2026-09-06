//go:build unix

package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestSonarIssue_BelaySeverity table-tests the mapping T32 depends on:
// Impacts take priority over the legacy field, the worst of several
// Impacts wins, case is ignored, and an unrecognized value never escalates
// to something that would stop a run.
func TestSonarIssue_BelaySeverity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		issue SonarIssue
		want  belay.Severity
	}{
		{"no severity signal at all defaults to info", SonarIssue{}, belay.SeverityInfo},
		{"legacy blocker, no impacts", SonarIssue{Severity: "BLOCKER"}, belay.SeverityBlocker},
		{"legacy is case-insensitive", SonarIssue{Severity: "major"}, belay.SeverityMajor},
		{"legacy unrecognized falls back to info", SonarIssue{Severity: "WEIRD"}, belay.SeverityInfo},
		{
			"single impact overrides legacy",
			SonarIssue{Severity: "MAJOR", Impacts: []SonarImpact{{SoftwareQuality: "SECURITY", Severity: "BLOCKER"}}},
			belay.SeverityBlocker,
		},
		{
			"worst of several impacts wins",
			SonarIssue{Impacts: []SonarImpact{
				{SoftwareQuality: "MAINTAINABILITY", Severity: "LOW"},
				{SoftwareQuality: "RELIABILITY", Severity: "HIGH"},
				{SoftwareQuality: "SECURITY", Severity: "MEDIUM"},
			}},
			belay.SeverityCritical, // HIGH
		},
		{
			"impact severity is case-insensitive",
			SonarIssue{Impacts: []SonarImpact{{SoftwareQuality: "SECURITY", Severity: "blocker"}}},
			belay.SeverityBlocker,
		},
		{
			"unrecognized impact severity does not count toward worst",
			SonarIssue{Impacts: []SonarImpact{{SoftwareQuality: "SECURITY", Severity: "WEIRD"}}},
			belay.SeverityInfo,
		},
		{"cleanCode INFO", SonarIssue{Impacts: []SonarImpact{{Severity: "INFO"}}}, belay.SeverityInfo},
		{"cleanCode LOW", SonarIssue{Impacts: []SonarImpact{{Severity: "LOW"}}}, belay.SeverityMinor},
		{"cleanCode MEDIUM", SonarIssue{Impacts: []SonarImpact{{Severity: "MEDIUM"}}}, belay.SeverityMajor},
		{"cleanCode HIGH", SonarIssue{Impacts: []SonarImpact{{Severity: "HIGH"}}}, belay.SeverityCritical},
		{"cleanCode BLOCKER", SonarIssue{Impacts: []SonarImpact{{Severity: "BLOCKER"}}}, belay.SeverityBlocker},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.issue.BelaySeverity(); got != tt.want {
				t.Errorf("BelaySeverity() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBelayGate proves the exact trap belay.GateError's own doc comment
// warns about: Sonar's "ERROR" string must map to GateFail, never
// GateError.
func TestBelayGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status string
		want   belay.GateStatus
	}{
		{"OK", belay.GatePass},
		{"ok", belay.GatePass},
		{"  OK  ", belay.GatePass},
		{"ERROR", belay.GateFail},
		{"error", belay.GateFail},
		{"NONE", belay.GateError},
		{"", belay.GateError},
		{"WEIRD", belay.GateError},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			t.Parallel()
			if got := BelayGate(tt.status); got != tt.want {
				t.Errorf("BelayGate(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestRelativeFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		component  string
		projectKey string
		want       string
	}{
		{"matching prefix stripped", "belay-dev_belay:internal/sonar/mcp/client.go", "belay-dev_belay", "internal/sonar/mcp/client.go"},
		{"non-matching prefix left alone", "other-project:main.go", "belay-dev_belay", "other-project:main.go"},
		{"empty project key leaves component alone", "belay-dev_belay:main.go", "", "belay-dev_belay:main.go"},
		{"component with no colon at all", "main.go", "belay-dev_belay", "main.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RelativeFile(tt.component, tt.projectKey); got != tt.want {
				t.Errorf("RelativeFile(%q, %q) = %q, want %q", tt.component, tt.projectKey, got, tt.want)
			}
		})
	}
}

// TestResponses_SlicesAreNeverNil proves the requirement 8 guarantee: every
// slice field this package decodes is non-nil even when the server's JSON
// omits the key entirely or sends an explicit empty array — the two shapes
// a real server can plausibly use for "nothing to report", both of which
// must round-trip to a non-nil empty slice so a Reviewer's assembled
// belay.QualityReport.Issues keeps its own documented "never nil"
// guarantee.
func TestResponses_SlicesAreNeverNil(t *testing.T) {
	t.Parallel()

	t.Run("ListQualityGates: key omitted", func(t *testing.T) {
		t.Parallel()
		c, ft := newHandshakenClient(t)
		ft.onTool(toolListQualityGates, rawResponder(`{"content":[{"type":"text","text":"{}"}],"isError":false}`))
		resp, err := c.ListQualityGates(context.Background(), ListQualityGatesRequest{})
		if err != nil {
			t.Fatalf("ListQualityGates: %v", err)
		}
		assertNonNilEmpty(t, "QualityGates", resp.QualityGates)
	})

	t.Run("AnalyzeCodeSnippet: issues explicitly empty", func(t *testing.T) {
		t.Parallel()
		c, ft := newHandshakenClient(t)
		ft.onTool(toolAnalyzeCodeSnippet, rawResponder(`{"content":[{"type":"text","text":"{\"issues\":[]}"}],"isError":false}`))
		resp, err := c.AnalyzeCodeSnippet(context.Background(), AnalyzeCodeSnippetRequest{})
		if err != nil {
			t.Fatalf("AnalyzeCodeSnippet: %v", err)
		}
		assertNonNilEmpty(t, "Issues", resp.Issues)
	})

	t.Run("SearchSonarIssuesInProjects: issues key omitted", func(t *testing.T) {
		t.Parallel()
		c, ft := newHandshakenClient(t)
		ft.onTool(toolSearchSonarIssuesInProjects, rawResponder(`{"content":[{"type":"text","text":"{\"total\":0}"}],"isError":false}`))
		resp, err := c.SearchSonarIssuesInProjects(context.Background(), SearchSonarIssuesInProjectsRequest{})
		if err != nil {
			t.Fatalf("SearchSonarIssuesInProjects: %v", err)
		}
		assertNonNilEmpty(t, "Issues", resp.Issues)
	})

	t.Run("GetProjectQualityGateStatus: conditions explicitly empty", func(t *testing.T) {
		t.Parallel()
		c, ft := newHandshakenClient(t)
		ft.onTool(toolGetProjectQualityGateStatus, rawResponder(
			`{"content":[{"type":"text","text":"{\"projectStatus\":{\"status\":\"OK\",\"conditions\":[]}}"}],"isError":false}`))
		resp, err := c.GetProjectQualityGateStatus(context.Background(), GetProjectQualityGateStatusRequest{})
		if err != nil {
			t.Fatalf("GetProjectQualityGateStatus: %v", err)
		}
		assertNonNilEmpty(t, "Conditions", resp.ProjectStatus.Conditions)
	})

	t.Run("ListTools result itself is never nil", func(t *testing.T) {
		t.Parallel()
		ft := newFakeTransport(t)
		ft.onMethod("initialize", fixtureResponder("initialize_response.json"))
		ft.onMethod("tools/list", rawResponder(`{"tools":[]}`))
		c := NewClient(ft, quietLogger())
		if _, err := c.Initialize(context.Background()); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		tools, err := c.ListTools(context.Background())
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		assertNonNilEmpty(t, "tools", tools)
	})
}

// assertNonNilEmpty fails t unless s is both empty and non-nil.
func assertNonNilEmpty[T any](t *testing.T, field string, s []T) {
	t.Helper()
	if s == nil {
		t.Fatalf("%s is nil, want a non-nil empty slice", field)
	}
	if len(s) != 0 {
		t.Fatalf("%s has %d elements, want 0 for this fixture", field, len(s))
	}
}

// rawResponder answers with a hand-built "result" object (not a
// pre-authored testdata fixture), useful for the small edge-case payloads
// this file needs.
func rawResponder(result string) responder {
	return func(t *testing.T, req sentRequest) ([]byte, error) {
		frame := `{"jsonrpc":"2.0","id":0,"result":` + result + `}`
		return withID(t, []byte(frame), req.ID), nil
	}
}

// TestGetRawSource_RequiresKey proves the one required parameter is
// validated locally before any tools/call is sent.
func TestGetRawSource_RequiresKey(t *testing.T) {
	t.Parallel()

	c, ft := newHandshakenClient(t)
	_, err := c.GetRawSource(context.Background(), GetRawSourceRequest{})
	if err == nil {
		t.Fatal("GetRawSource with an empty Key returned nil error")
	}
	if n := ft.sentCount("tools/call"); n != 0 {
		t.Errorf("Transport recorded %d tools/call requests for an invalid request, want 0", n)
	}
}

// TestRequestMarshaling_OmitsUnsetOptionalFields proves the request
// wrappers only send parameters the caller actually set, so a server's own
// defaults (page 1, page size 100, scope MAIN) are never silently
// overridden by a Go zero value that looks like a deliberate choice.
func TestRequestMarshaling_OmitsUnsetOptionalFields(t *testing.T) {
	t.Parallel()

	c, ft := newHandshakenClient(t)
	ft.onTool(toolSearchSonarIssuesInProjects, fixtureResponder("call_search_sonar_issues_in_projects.json"))

	if _, err := c.SearchSonarIssuesInProjects(context.Background(), SearchSonarIssuesInProjectsRequest{
		ProjectKeys: []string{"belay-dev_belay"},
	}); err != nil {
		t.Fatalf("SearchSonarIssuesInProjects: %v", err)
	}

	sent := ft.lastSent()
	var params struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(sent.Params, &params); err != nil {
		t.Fatalf("unmarshal sent params: %v", err)
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(params.Arguments, &args); err != nil {
		t.Fatalf("unmarshal arguments: %v", err)
	}

	for _, unset := range []string{"pageIndex", "pageSize", "inNewCodePeriod", "issueKey", "branch", "pullRequest", "severities", "issueStatuses", "impactSoftwareQualities"} {
		if _, ok := args[unset]; ok {
			t.Errorf("arguments carries unset field %q = %s, want it omitted", unset, args[unset])
		}
	}
	if _, ok := args["projectKeys"]; !ok {
		t.Error("arguments is missing projectKeys, which was set")
	}
}
