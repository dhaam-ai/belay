//go:build unix

package aireview

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/internal/sonar/mcp"
	"github.com/belay-dev/belay/internal/sonar/scanner"
	"github.com/belay-dev/belay/pkg/belay"
)

// issue is a compact belay.Issue for the tables below.
func issue(sev belay.Severity) belay.Issue {
	return belay.Issue{RuleID: "go:S1", Severity: sev, File: "a.go", Line: 1, Message: "m"}
}

// TestFinalizeFieldPopulationRules is acceptance check 4: every rule
// belay.QualityReport and internal/sonar/scanner hold themselves to must
// hold for every report this package can produce, including the ones built
// on an error path where it is easiest to forget.
func TestFinalizeFieldPopulationRules(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		issues   []belay.Issue
		cause    error
		failOn   belay.Severity
		wantGate belay.GateStatus
	}{
		{
			name: "pass with no issues", status: "OK", failOn: belay.SeverityMajor,
			wantGate: belay.GatePass,
		},
		{
			name: "pass with issues below the threshold", status: "OK",
			issues: []belay.Issue{issue(belay.SeverityMinor), issue(belay.SeverityInfo)},
			failOn: belay.SeverityMajor, wantGate: belay.GatePass,
		},
		{
			name: "fail on the server with matching issues", status: "ERROR",
			issues: []belay.Issue{issue(belay.SeverityBlocker), issue(belay.SeverityMinor)},
			failOn: belay.SeverityMajor, wantGate: belay.GateFail,
		},
		{
			name: "fail on the server with nothing at the threshold", status: "ERROR",
			issues: []belay.Issue{issue(belay.SeverityMinor)},
			failOn: belay.SeverityMajor, wantGate: belay.GateFail,
		},
		{
			name: "fail on belay's stricter threshold while the server passed", status: "OK",
			issues: []belay.Issue{issue(belay.SeverityMinor)},
			failOn: belay.SeverityMinor, wantGate: belay.GateFail,
		},
		{
			name: "no analysis has run", status: "NONE", failOn: belay.SeverityMajor,
			wantGate: belay.GateError,
		},
		{
			name: "unrecognized server status", status: "WAT", failOn: belay.SeverityMajor,
			wantGate: belay.GateError,
		},
		{
			name: "empty server status", status: "", failOn: belay.SeverityMajor,
			wantGate: belay.GateError,
		},
		{
			name: "transport failure before any status", status: "",
			cause: errors.New("dial tcp: connection refused"), failOn: belay.SeverityMajor,
			wantGate: belay.GateError,
		},
		{
			name: "failure after a passing status still reaches no verdict", status: "OK",
			issues: []belay.Issue{issue(belay.SeverityBlocker)},
			cause:  errors.New("search failed"), failOn: belay.SeverityMajor,
			wantGate: belay.GateError,
		},
		{
			name: "fail_on info gates on everything", status: "OK",
			issues: []belay.Issue{issue(belay.SeverityInfo)},
			failOn: belay.SeverityInfo, wantGate: belay.GateFail,
		},
		{
			name: "fail_on blocker ignores everything below it", status: "OK",
			issues: []belay.Issue{issue(belay.SeverityCritical)},
			failOn: belay.SeverityBlocker, wantGate: belay.GatePass,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := finalize(tc.status, tc.issues, RawReview{ProjectKey: "demo"}, tc.failOn, tc.cause)
			if got.Gate != tc.wantGate {
				t.Errorf("Gate = %s, want %s", got.Gate, tc.wantGate)
			}
			assertReportRules(t, got)
		})
	}
}

// TestFinalizeNeverAliasesTheCallersSlice proves the make+copy discipline:
// a caller that keeps writing to its own slice cannot reach into a report
// that has already been handed back.
func TestFinalizeNeverAliasesTheCallersSlice(t *testing.T) {
	src := []belay.Issue{issue(belay.SeverityMajor)}
	got := finalize("OK", src, RawReview{}, belay.SeverityMajor, nil)
	src[0].Message = "mutated after the fact"
	if got.Issues[0].Message == "mutated after the fact" {
		t.Fatal("report Issues aliases the caller's slice")
	}
}

// TestFinalizeEmptyIssuesIsNonNilSlice is the specific bug the whole
// discipline exists to prevent: an empty report serializing as null.
func TestFinalizeEmptyIssuesIsNonNilSlice(t *testing.T) {
	got := finalize("OK", nil, RawReview{}, belay.SeverityMajor, nil)
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"issues":[]`) {
		t.Fatalf("empty report serialized as %s, want \"issues\":[]", b)
	}
}

// TestFinalizeSynthesizesGateIssueOnlyWhenNeeded pins the one place this
// package invents an issue, and proves it does not do so anywhere else.
func TestFinalizeSynthesizesGateIssueOnlyWhenNeeded(t *testing.T) {
	tests := []struct {
		name   string
		status string
		issues []belay.Issue
		want   bool
	}{
		{"server failed with nothing at the threshold", "ERROR", nil, true},
		{"server failed with something at the threshold", "ERROR", []belay.Issue{issue(belay.SeverityBlocker)}, false},
		{"server passed", "OK", nil, false},
		{"no verdict", "NONE", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := finalize(tc.status, tc.issues, RawReview{ProjectKey: "demo"}, belay.SeverityMajor, nil)
			var found bool
			for _, i := range got.Issues {
				if i.RuleID == GateRuleID {
					found = true
					if i.Severity != belay.SeverityBlocker {
						t.Errorf("synthesized issue severity = %s, want blocker", i.Severity)
					}
					if i.File != "" || i.Line != 0 {
						t.Errorf("synthesized issue points at %s:%d; a project-level gate is not attributable to a line", i.File, i.Line)
					}
				}
			}
			if found != tc.want {
				t.Errorf("synthesized gate issue = %v, want %v", found, tc.want)
			}
			var raw RawReview
			if err := json.Unmarshal(got.Raw, &raw); err != nil {
				t.Fatalf("unmarshal Raw: %v", err)
			}
			if raw.SyntheticGateIssue != tc.want {
				t.Errorf("Raw.synthetic_gate_issue = %v, want %v", raw.SyntheticGateIssue, tc.want)
			}
			assertReportRules(t, got)
		})
	}
}

// TestCountIssuesIsTheHistogram covers the arm a report can never reach
// through finalize but that Counts.Total()==len(Issues) depends on.
func TestCountIssuesIsTheHistogram(t *testing.T) {
	issues := []belay.Issue{
		issue(belay.SeverityBlocker), issue(belay.SeverityBlocker),
		issue(belay.SeverityCritical), issue(belay.SeverityMajor),
		issue(belay.SeverityMinor), issue(belay.SeverityInfo),
		{RuleID: "x", Severity: belay.Severity(99)},
	}
	want := belay.Counts{Blocker: 2, Critical: 1, Major: 1, Minor: 1, Info: 2}
	if got := countIssues(issues); got != want {
		t.Errorf("countIssues() = %+v, want %+v", got, want)
	}
	if got := countIssues(issues); got.Total() != len(issues) {
		t.Errorf("Total() = %d, want %d: an out-of-range severity must be counted, not dropped", got.Total(), len(issues))
	}
}

// TestMapIssue pins the tool-response -> belay.Issue mapping
// internal/sonar/mcp's tools.go documents for this package.
func TestMapIssue(t *testing.T) {
	tests := []struct {
		name       string
		in         mcp.SonarIssue
		projectKey string
		want       belay.Issue
	}{
		{
			name: "clean code impacts win over legacy severity",
			in: mcp.SonarIssue{
				Rule: "go:S2259", Severity: "MINOR",
				Impacts:   []mcp.SonarImpact{{SoftwareQuality: "RELIABILITY", Severity: "HIGH"}, {SoftwareQuality: "MAINTAINABILITY", Severity: "MEDIUM"}},
				Component: "demo:internal/a.go", Line: 12, Message: "boom", Effort: "20min",
			},
			projectKey: "demo",
			want:       belay.Issue{RuleID: "go:S2259", Severity: belay.SeverityCritical, File: "internal/a.go", Line: 12, Message: "boom", Effort: "20min"},
		},
		{
			name:       "legacy severity when there are no impacts",
			in:         mcp.SonarIssue{Rule: "go:S1192", Severity: "MAJOR", Component: "demo:main.go", Line: 3, Message: "dup"},
			projectKey: "demo",
			want:       belay.Issue{RuleID: "go:S1192", Severity: belay.SeverityMajor, File: "main.go", Line: 3, Message: "dup"},
		},
		{
			name:       "unrecognized severity falls back to info",
			in:         mcp.SonarIssue{Rule: "go:S9", Severity: "NOPE", Component: "demo:x.go", Message: "?"},
			projectKey: "demo",
			want:       belay.Issue{RuleID: "go:S9", Severity: belay.SeverityInfo, File: "x.go", Message: "?"},
		},
		{
			name:       "a component from another project keeps its key verbatim",
			in:         mcp.SonarIssue{Rule: "go:S9", Severity: "INFO", Component: "other:x.go", Message: "?"},
			projectKey: "demo",
			want:       belay.Issue{RuleID: "go:S9", Severity: belay.SeverityInfo, File: "other:x.go", Message: "?"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, mapIssue(tc.in, tc.projectKey)); diff != "" {
				t.Errorf("mapIssue() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSummaryNeverEmpty walks every arm of summarize, including the ones
// with nothing to say.
func TestSummaryNeverEmpty(t *testing.T) {
	for _, gate := range []belay.GateStatus{belay.GatePass, belay.GateFail, belay.GateError, belay.GateUnknown} {
		for _, cause := range []error{nil, errors.New("boom")} {
			got := summarize(gate, "", belay.Counts{}, belay.SeverityMajor, cause)
			if got == "" {
				t.Fatalf("summarize(%s, cause=%v) is empty", gate, cause)
			}
			if strings.Contains(got, "quality gate ;") {
				t.Errorf("summarize(%s) has an empty status word: %q", gate, got)
			}
		}
	}
}

// TestMarshalRawNormalizesNilSlices proves the nil discipline reaches inside
// Raw, one level below the field belay.QualityReport documents.
func TestMarshalRawNormalizesNilSlices(t *testing.T) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(marshalRaw(RawReview{}), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, want := range map[string]string{
		"changed_files": "[]",
		"conditions":    "[]",
		"issues":        "[]",
		"quality_gate":  "null",
	} {
		if got := string(obj[key]); got != want {
			t.Errorf("Raw[%q] = %s, want %s", key, got, want)
		}
	}
}

// TestReportKeySetMatchesTheDeterministicScanner is the conformance claim
// challenge step 7 rests on: this package's report and
// internal/sonar/scanner's serialize to one top-level key set, with Source
// the only permitted difference in meaning.
//
// The scanner report is produced without Docker, a network or a SonarQube
// server: a request with no ProjectKey is rejected by its own validation,
// which returns its fully populated error report before any process starts.
func TestReportKeySetMatchesTheDeterministicScanner(t *testing.T) {
	det := scanner.New(scanner.WithLogger(quietLogger()), scanner.WithLookupEnv(noEnv))
	detReport, detErr := det.Review(t.Context(), belay.ReviewRequest{FailOn: belay.SeverityMajor})
	if detErr == nil {
		t.Fatal("scanner accepted a request with no project key; the fixture assumption no longer holds")
	}

	cases := map[string]belay.QualityReport{
		"pass":  finalize("OK", nil, RawReview{}, belay.SeverityMajor, nil),
		"fail":  finalize("ERROR", []belay.Issue{issue(belay.SeverityBlocker)}, RawReview{}, belay.SeverityMajor, nil),
		"error": finalize("", nil, RawReview{}, belay.SeverityMajor, errors.New("unreachable")),
	}
	wantKeys := topLevelKeys(t, detReport)
	for name, ai := range cases {
		t.Run(name, func(t *testing.T) {
			if diff := cmp.Diff(wantKeys, topLevelKeys(t, ai)); diff != "" {
				t.Errorf("top-level key set differs from the deterministic scanner (-scanner +ai):\n%s", diff)
			}
			if ai.Source == detReport.Source {
				t.Errorf("both reports claim Source %q; Source is the one field they must differ on", ai.Source)
			}
			// Same key set is not enough: the value shapes the graph reads
			// must match too, which is where a null issues list would show.
			var aiIssues, detIssues json.RawMessage
			aiIssues, detIssues = fieldOf(t, ai, "issues"), fieldOf(t, detReport, "issues")
			if string(aiIssues) == "null" || string(detIssues) == "null" {
				t.Errorf("issues serialized as null (ai=%s scanner=%s)", aiIssues, detIssues)
			}
			assertReportRules(t, ai)
		})
	}
}

// topLevelKeys returns q's serialized top-level JSON keys, sorted.
func topLevelKeys(t *testing.T, q belay.QualityReport) []string {
	t.Helper()
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// fieldOf returns one serialized top-level field of q.
func fieldOf(t *testing.T, q belay.QualityReport, key string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return obj[key]
}
