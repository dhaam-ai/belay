package belay_test

import (
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/pkg/belay"
)

var allSeverities = []belay.Severity{
	belay.SeverityInfo, belay.SeverityMinor, belay.SeverityMajor,
	belay.SeverityCritical, belay.SeverityBlocker,
}

var allGateStatuses = []belay.GateStatus{
	belay.GateUnknown, belay.GatePass, belay.GateFail, belay.GateError,
}

// TestSeverityOrdering is the acceptance-critical test: Severity's numeric
// ordering, not just its five names, is part of the contract, because
// ReviewRequest.FailOn threshold comparisons (see
// TestSeverityFailOnThreshold) are implemented as a plain >= on this
// ordering everywhere in belay.
func TestSeverityOrdering(t *testing.T) {
	t.Parallel()

	for i := 1; i < len(allSeverities); i++ {
		lower, higher := allSeverities[i-1], allSeverities[i]
		if lower >= higher {
			t.Fatalf("%v is not less than %v; Severity's documented ordering (info < minor < major < critical < blocker) is broken", lower, higher)
		}
	}
}

// TestSeverityFailOnThreshold table-tests the exact comparison
// ReviewRequest.FailOn is documented to use: an issue fails the gate iff
// issue.Severity >= req.FailOn.
func TestSeverityFailOnThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		issue      belay.Severity
		failOn     belay.Severity
		wantBlocks bool
	}{
		{"info issue against fail_on info blocks", belay.SeverityInfo, belay.SeverityInfo, true},
		{"info issue against fail_on minor does not block", belay.SeverityInfo, belay.SeverityMinor, false},
		{"minor issue against fail_on major does not block", belay.SeverityMinor, belay.SeverityMajor, false},
		{"major issue against fail_on major blocks", belay.SeverityMajor, belay.SeverityMajor, true},
		{"critical issue against fail_on major blocks", belay.SeverityCritical, belay.SeverityMajor, true},
		{"blocker issue against fail_on major blocks", belay.SeverityBlocker, belay.SeverityMajor, true},
		{"blocker issue against fail_on blocker blocks", belay.SeverityBlocker, belay.SeverityBlocker, true},
		{"critical issue against fail_on blocker does not block", belay.SeverityCritical, belay.SeverityBlocker, false},
		{"minor issue against fail_on info blocks", belay.SeverityMinor, belay.SeverityInfo, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.issue >= tt.failOn
			if got != tt.wantBlocks {
				t.Errorf("Severity %v >= FailOn %v = %v, want %v", tt.issue, tt.failOn, got, tt.wantBlocks)
			}
		})
	}
}

func TestSeverityRoundTrip(t *testing.T) {
	t.Parallel()

	for _, want := range allSeverities {
		t.Run(want.String(), func(t *testing.T) {
			t.Parallel()

			got, err := belay.ParseSeverity(want.String())
			if err != nil {
				t.Fatalf("ParseSeverity(%q) returned error: %v", want.String(), err)
			}
			if got != want {
				t.Errorf("ParseSeverity(%q) = %v, want %v", want.String(), got, want)
			}
		})
	}
}

func TestParseSeverity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    belay.Severity
		wantErr bool
	}{
		{name: "exact", input: "major", want: belay.SeverityMajor},
		{name: "uppercase", input: "MAJOR", want: belay.SeverityMajor},
		{name: "mixed case", input: "BlOcKeR", want: belay.SeverityBlocker},
		{name: "surrounding whitespace", input: "  info\t", want: belay.SeverityInfo},
		{name: "empty", input: "", wantErr: true},
		{name: "unknown word", input: "catastrophic", wantErr: true},
		{name: "near miss", input: "majors", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := belay.ParseSeverity(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseSeverity(%q) = %v, want error", tt.input, got)
				}
				if !errors.Is(err, belay.ErrUnknownSeverity) {
					t.Errorf("ParseSeverity(%q) error = %v, want it to wrap ErrUnknownSeverity", tt.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSeverity(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseSeverity(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSeverityOutOfRangeDoesNotPanic(t *testing.T) {
	t.Parallel()

	for _, s := range []belay.Severity{-1, 99} {
		if got := s.String(); got == "" {
			t.Errorf("Severity(%d).String() = empty, want a placeholder", int(s))
		}
		if _, err := s.MarshalText(); err == nil {
			t.Errorf("Severity(%d).MarshalText() = nil error, want error", int(s))
		}
	}
}

func TestSeverityJSONRoundTrip(t *testing.T) {
	t.Parallel()

	for _, want := range allSeverities {
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("json.Marshal(%v) returned error: %v", want, err)
		}
		var got belay.Severity
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
		}
		if got != want {
			t.Errorf("round trip of %v through %s = %v", want, encoded, got)
		}
	}
}

func TestGateStatusRoundTrip(t *testing.T) {
	t.Parallel()

	for _, want := range allGateStatuses {
		t.Run(want.String(), func(t *testing.T) {
			t.Parallel()

			got, err := belay.ParseGateStatus(want.String())
			if err != nil {
				t.Fatalf("ParseGateStatus(%q) returned error: %v", want.String(), err)
			}
			if got != want {
				t.Errorf("ParseGateStatus(%q) = %v, want %v", want.String(), got, want)
			}
		})
	}
}

func TestParseGateStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    belay.GateStatus
		wantErr bool
	}{
		{name: "exact", input: "fail", want: belay.GateFail},
		{name: "uppercase", input: "PASS", want: belay.GatePass},
		{name: "surrounding whitespace", input: " error \n", want: belay.GateError},
		{name: "unknown word", input: "maybe", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := belay.ParseGateStatus(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseGateStatus(%q) = %v, want error", tt.input, got)
				}
				if !errors.Is(err, belay.ErrUnknownGateStatus) {
					t.Errorf("ParseGateStatus(%q) error = %v, want it to wrap ErrUnknownGateStatus", tt.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGateStatus(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseGateStatus(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestGateStatusOutOfRangeDoesNotPanic(t *testing.T) {
	t.Parallel()

	for _, g := range []belay.GateStatus{-1, 99} {
		if got := g.String(); got == "" {
			t.Errorf("GateStatus(%d).String() = empty, want a placeholder", int(g))
		}
		if _, err := g.MarshalText(); err == nil {
			t.Errorf("GateStatus(%d).MarshalText() = nil error, want error", int(g))
		}
	}
}

func TestCountsTotal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		counts belay.Counts
		want   int
	}{
		"zero value":  {belay.Counts{}, 0},
		"mixed":       {belay.Counts{Blocker: 1, Critical: 2, Major: 3, Minor: 4, Info: 5}, 15},
		"single kind": {belay.Counts{Major: 7}, 7},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := tt.counts.Total(); got != tt.want {
				t.Errorf("Counts%+v.Total() = %d, want %d", tt.counts, got, tt.want)
			}
		})
	}
}

func TestCountsAtOrAbove(t *testing.T) {
	t.Parallel()

	counts := belay.Counts{Blocker: 1, Critical: 2, Major: 3, Minor: 4, Info: 5}

	tests := []struct {
		min  belay.Severity
		want int
	}{
		{belay.SeverityInfo, 15},    // everything
		{belay.SeverityMinor, 10},   // 1+2+3+4
		{belay.SeverityMajor, 6},    // 1+2+3
		{belay.SeverityCritical, 3}, // 1+2
		{belay.SeverityBlocker, 1},  // 1
	}

	for _, tt := range tests {
		t.Run(tt.min.String(), func(t *testing.T) {
			t.Parallel()

			if got := counts.AtOrAbove(tt.min); got != tt.want {
				t.Errorf("Counts%+v.AtOrAbove(%v) = %d, want %d", counts, tt.min, got, tt.want)
			}
		})
	}
}

// TestReviewerGateInvariant pins the invariant documented on
// Reviewer.Review: Gate is GatePass if and only if
// Counts.AtOrAbove(req.FailOn) == 0. Any Reviewer implementation is
// expected to satisfy this; this test exercises the arithmetic a
// conforming implementation relies on.
func TestReviewerGateInvariant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		counts   belay.Counts
		failOn   belay.Severity
		wantPass bool
	}{
		{"no issues at all", belay.Counts{}, belay.SeverityMajor, true},
		{"only minor issues, fail_on major", belay.Counts{Minor: 3}, belay.SeverityMajor, true},
		{"one major issue, fail_on major", belay.Counts{Major: 1}, belay.SeverityMajor, false},
		{"only info issues, fail_on info", belay.Counts{Info: 1}, belay.SeverityInfo, false},
		{"blocker present, fail_on blocker", belay.Counts{Blocker: 1}, belay.SeverityBlocker, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotPass := tt.counts.AtOrAbove(tt.failOn) == 0
			if gotPass != tt.wantPass {
				t.Errorf("Counts%+v.AtOrAbove(%v) == 0 is %v, want %v", tt.counts, tt.failOn, gotPass, tt.wantPass)
			}
		})
	}
}

// TestQualityReportJSONKeySetIsAdapterIndependent rehearses the central
// promise documented on QualityReport: two structurally different
// adapters — here, a stand-in for a deterministic linter and a stand-in
// for an AI reviewer — must serialize to an identical top-level JSON key
// set, and their Issues must too, even though Source, Gate, Counts, Issues
// content and Raw all legitimately differ.
func TestQualityReportJSONKeySetIsAdapterIndependent(t *testing.T) {
	t.Parallel()

	deterministic := belay.QualityReport{
		Source: "golangci-lint",
		Gate:   belay.GateFail,
		Counts: belay.Counts{Major: 1, Minor: 2},
		Issues: []belay.Issue{
			{RuleID: "govet:nilness", Severity: belay.SeverityMajor, File: "main.go", Line: 42, Message: "possible nil dereference"},
		},
		Summary: "3 issues (1 major, 2 minor)",
		Raw:     json.RawMessage(`{"Issues":[{"FromLinter":"govet"}]}`),
	}

	ai := belay.QualityReport{
		Source: "sonar-mcp-ai-review",
		Gate:   belay.GatePass,
		Counts: belay.Counts{Info: 1},
		Issues: []belay.Issue{
			{RuleID: "sonar:S1234", Severity: belay.SeverityInfo, File: "handler.py", Line: 10, Message: "consider renaming", Effort: "5min"},
		},
		Summary: "1 informational suggestion",
		Raw:     json.RawMessage(`{"projectStatus":{"status":"OK"}}`),
	}

	if diff := cmp.Diff(jsonKeys(t, deterministic), jsonKeys(t, ai)); diff != "" {
		t.Fatalf("QualityReport top-level JSON key set differs between adapters (-deterministic +ai):\n%s", diff)
	}
	if diff := cmp.Diff(jsonKeys(t, deterministic.Issues[0]), jsonKeys(t, ai.Issues[0])); diff != "" {
		t.Fatalf("Issue JSON key set differs between adapters (-deterministic +ai):\n%s", diff)
	}

	// Source is documented as the only field two otherwise-identical
	// reports may differ on: with Source normalized out, two reports that
	// are identical apart from Source must serialize identically.
	base := belay.QualityReport{
		Gate:    belay.GatePass,
		Counts:  belay.Counts{},
		Issues:  []belay.Issue{},
		Summary: "",
		Raw:     nil,
	}
	a, b := base, base
	a.Source, b.Source = "adapter-a", "adapter-b"

	if diff := cmp.Diff(marshalWithoutSource(t, a), marshalWithoutSource(t, b)); diff != "" {
		t.Fatalf("two reports differing only in Source produced different JSON once source is removed (-a +b):\n%s", diff)
	}
}

func TestQualityReportIssuesNeverSerializesAsNull(t *testing.T) {
	t.Parallel()

	report := belay.QualityReport{Issues: []belay.Issue{}}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
	}
	if got := string(asMap["issues"]); got != "[]" {
		t.Errorf(`QualityReport{Issues: []belay.Issue{}} serialized "issues" as %s, want "[]"`, got)
	}
}

func TestQualityReportJSONRoundTrip(t *testing.T) {
	t.Parallel()

	want := belay.QualityReport{
		Source: "golangci-lint",
		Gate:   belay.GateFail,
		Counts: belay.Counts{Major: 1},
		Issues: []belay.Issue{
			{RuleID: "govet:nilness", Severity: belay.SeverityMajor, File: "main.go", Line: 42, Message: "nil deref", Effort: "10min"},
		},
		Summary: "1 major issue",
		Raw:     json.RawMessage(`{"ok":true}`),
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	var got belay.QualityReport
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
	}
	if diff := cmp.Diff(want, got, rawMessageComparer); diff != "" {
		t.Errorf("round trip through %s changed the value (-want +got):\n%s", encoded, diff)
	}
}

// jsonKeys marshals v and returns its top-level JSON object keys, sorted.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()

	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%#v) returned error: %v", v, err)
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
	}
	keys := make([]string, 0, len(asMap))
	for k := range asMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// marshalWithoutSource marshals r, deletes the top-level "source" key, and
// returns the result as a string for comparison.
func marshalWithoutSource(t *testing.T, r belay.QualityReport) string {
	t.Helper()

	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
	}
	delete(asMap, "source")
	out, err := json.Marshal(asMap)
	if err != nil {
		t.Fatalf("re-marshal returned error: %v", err)
	}
	return string(out)
}
