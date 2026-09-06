package state

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/pkg/belay"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestNewState_Defaults(t *testing.T) {
	st := NewState("do the thing")
	if st.SchemaVersion != StateSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", st.SchemaVersion, StateSchemaVersion)
	}
	if st.Goal != "do the thing" {
		t.Errorf("Goal = %q, want %q", st.Goal, "do the thing")
	}
	if st.Candidates == nil {
		t.Error("Candidates is nil, want a non-nil empty slice")
	}
	if st.History == nil {
		t.Error("History is nil, want a non-nil empty slice")
	}

	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !containsJSONArray(t, data, "candidates") || !containsJSONArray(t, data, "history") {
		t.Errorf("expected empty-array serialization for candidates/history, got %s", data)
	}
}

func containsJSONArray(t *testing.T, data []byte, key string) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, ok := m[key]
	if !ok {
		return false
	}
	return string(raw) == "[]"
}

func TestState_SaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	want := NewState("round trip me")
	want.Plan = Plan{Path: "artifacts/plan.md", Approved: true, Digest: "sha256:abc"}
	want.Code = Code{SessionID: "sess-1", LastDiff: "artifacts/diff-0001.patch", ChangedFiles: []string{"x.go"}}
	want.Test = NewTest(belay.TestReport{Total: 4, Passed: 3, Failed: 1}, "artifacts/test.json")
	want.Review = NewReview(belay.QualityReport{Source: "golangci-lint", Gate: belay.GatePass, Issues: []belay.Issue{}}, "")
	want.Fix = Fix{Attempts: 1, GiveUp: false}
	want.Candidates = []Candidate{{ID: "c1", Dir: "/tmp/c1", TestPassed: true, IssueCount: 0}}
	want.Winner = "c1"
	want.History = []HistoryEntry{{Seq: 1, Node: "plan", Attempt: 1, Status: "ok", Next: "approve"}}

	if err := SaveState(path, want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
}

func TestLoadState_MissingFile(t *testing.T) {
	_, err := LoadState(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestLoadState_UnknownSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":99,"goal":"x"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := LoadState(path)
	if err == nil {
		t.Fatal("expected an error for an unknown schema version")
	}
	if !errors.Is(err, ErrUnsupportedStateVersion) {
		t.Errorf("error does not wrap ErrUnsupportedStateVersion: %v", err)
	}
	var ve *VersionError
	if !errors.As(err, &ve) {
		t.Fatalf("error is not a *VersionError: %v", err)
	}
	if ve.Got != 99 {
		t.Errorf("Got = %d, want 99", ve.Got)
	}
}

func TestLoadState_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`not json`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

// TestReviewRoundTrip_PreservesGateCountsIssues is the acceptance test for
// "Round-trip QualityReport -> state.review -> QualityReport preserves
// Gate, Counts, Issues exactly." Source and Summary also round-trip since
// they are plain copies, but only Gate, Counts and Issues are the graph's
// actual contract with the review block (see the Review doc comment) and
// so are asserted independently below.
func TestReviewRoundTrip_PreservesGateCountsIssues(t *testing.T) {
	tests := []struct {
		name   string
		report belay.QualityReport
	}{
		{
			name:   "zero issues, non-nil slice",
			report: belay.QualityReport{Source: "golangci-lint", Gate: belay.GatePass, Issues: []belay.Issue{}, Summary: "clean"},
		},
		{
			name:   "nil issues normalizes to non-nil on the way back",
			report: belay.QualityReport{Source: "golangci-lint", Gate: belay.GatePass, Issues: nil, Summary: "clean"},
		},
		{
			name: "mixed severities and a gate failure",
			report: belay.QualityReport{
				Source: "sonar-mcp-ai-review",
				Gate:   belay.GateFail,
				Counts: belay.Counts{Blocker: 1, Critical: 2, Major: 3, Minor: 4, Info: 5},
				Issues: []belay.Issue{
					{RuleID: "govet:nilness", Severity: belay.SeverityCritical, File: "a.go", Line: 10, Message: "nil deref", Effort: "30min"},
					{RuleID: "sonar:S1234", Severity: belay.SeverityBlocker, File: "b.go", Line: 1, Message: "sql injection"},
				},
				Summary: "2 issues (1 blocker, 1 critical)",
			},
		},
		{
			name:   "gate error",
			report: belay.QualityReport{Source: "sonar-scan", Gate: belay.GateError, Issues: []belay.Issue{}, Summary: "timed out"},
		},
		{
			name: "raw is present on the input but is not part of the projection",
			report: belay.QualityReport{
				Source: "golangci-lint", Gate: belay.GatePass, Issues: []belay.Issue{}, Summary: "clean",
				Raw: json.RawMessage(`{"tool":"golangci-lint"}`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			review := NewReview(tt.report, "")
			got := review.QualityReport()

			if diff := cmp.Diff(tt.report.Gate, got.Gate); diff != "" {
				t.Errorf("Gate mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.report.Counts, got.Counts); diff != "" {
				t.Errorf("Counts mismatch (-want +got):\n%s", diff)
			}
			wantIssues := tt.report.Issues
			if wantIssues == nil {
				wantIssues = []belay.Issue{}
			}
			if diff := cmp.Diff(wantIssues, got.Issues); diff != "" {
				t.Errorf("Issues mismatch (-want +got):\n%s", diff)
			}
			if got.Issues == nil {
				t.Error("reconstructed QualityReport.Issues is nil, want non-nil per belay's contract")
			}
			if got.Source != tt.report.Source {
				t.Errorf("Source = %q, want %q", got.Source, tt.report.Source)
			}
			if got.Summary != tt.report.Summary {
				t.Errorf("Summary = %q, want %q", got.Summary, tt.report.Summary)
			}
			if got.Raw != nil {
				t.Errorf("Raw = %q, want nil (not part of the blackboard projection)", got.Raw)
			}
		})
	}
}

func TestNewReview_DoesNotAliasInputIssues(t *testing.T) {
	issues := []belay.Issue{{RuleID: "r1", Severity: belay.SeverityMinor}}
	report := belay.QualityReport{Issues: issues}
	review := NewReview(report, "")
	issues[0].RuleID = "mutated"
	if review.Issues[0].RuleID != "r1" {
		t.Errorf("Review.Issues aliases the input slice: got %q, want %q", review.Issues[0].RuleID, "r1")
	}
}

// TestNewReview_PreservesNonNilEmptyIssues guards against a real bug this
// package once had: projecting Issues with append([]belay.Issue(nil),
// q.Issues...) silently collapsed a non-nil-empty q.Issues to nil, because
// appending zero elements onto a nil destination never allocates. That
// would have violated belay.QualityReport.Issues's own "never nil"
// contract the very first time an adapter reported zero issues.
func TestNewReview_PreservesNonNilEmptyIssues(t *testing.T) {
	review := NewReview(belay.QualityReport{Issues: []belay.Issue{}}, "")
	if review.Issues == nil {
		t.Error("NewReview turned a non-nil empty Issues slice into nil")
	}
}

func TestNewTest_PreservesNonNilEmptyFailures(t *testing.T) {
	projected := NewTest(belay.TestReport{Failures: []belay.TestFailure{}}, "")
	if projected.Failures == nil {
		t.Error("NewTest turned a non-nil empty Failures slice into nil")
	}
}

func TestReview_JSONKeySetMatchesSpec(t *testing.T) {
	review := NewReview(belay.QualityReport{
		Source: "golangci-lint",
		Gate:   belay.GatePass,
		Issues: []belay.Issue{{RuleID: "r1", Severity: belay.SeverityMinor, File: "a.go", Line: 1, Message: "m"}},
		Counts: belay.Counts{Minor: 1},
	}, "")
	assertExactJSONKeys(t, review, []string{"source", "gate", "issues", "counts", "summary", "report_path"})
}

func TestTestRoundTrip_PreservesCounts(t *testing.T) {
	tests := []struct {
		name   string
		report belay.TestReport
	}{
		{"clean pass", belay.TestReport{Total: 10, Passed: 10, Failed: 0}},
		{"nil failures", belay.TestReport{Total: 0, Passed: 0, Failed: 0, Failures: nil}},
		{
			"with failures",
			belay.TestReport{
				Total: 5, Passed: 3, Failed: 2,
				Failures: []belay.TestFailure{
					{Name: "TestFoo", File: "foo_test.go", Line: 12, Message: "assertion failed"},
					{Name: "build", Message: "compile error"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projected := NewTest(tt.report, "artifacts/test.json")
			got := projected.Report()

			if got.Total != tt.report.Total || got.Passed != tt.report.Passed || got.Failed != tt.report.Failed {
				t.Errorf("counts mismatch: got %+v, want Total=%d Passed=%d Failed=%d",
					got, tt.report.Total, tt.report.Passed, tt.report.Failed)
			}
			wantFailures := tt.report.Failures
			if diff := cmp.Diff(wantFailures, got.Failures); diff != "" {
				t.Errorf("Failures mismatch (-want +got):\n%s", diff)
			}
			if projected.ReportPath != "artifacts/test.json" {
				t.Errorf("ReportPath = %q, want %q", projected.ReportPath, "artifacts/test.json")
			}
		})
	}
}

func TestNewTest_DoesNotAliasInputFailures(t *testing.T) {
	failures := []belay.TestFailure{{Name: "T1"}}
	report := belay.TestReport{Failures: failures}
	projected := NewTest(report, "")
	failures[0].Name = "mutated"
	if projected.Failures[0].Name != "T1" {
		t.Errorf("Test.Failures aliases the input slice: got %q, want %q", projected.Failures[0].Name, "T1")
	}
}

func TestTest_JSONKeySetMatchesSpec(t *testing.T) {
	projected := NewTest(belay.TestReport{Total: 1, Passed: 1}, "artifacts/test.json")
	assertExactJSONKeys(t, projected, []string{"passed", "total", "failed", "failures", "report_path"})
}

func TestPlan_JSONKeySetMatchesSpec(t *testing.T) {
	assertExactJSONKeys(t, Plan{}, []string{"path", "approved", "digest"})
}

func TestCode_JSONKeySetMatchesSpec(t *testing.T) {
	assertExactJSONKeys(t, Code{}, []string{"session_id", "last_diff", "changed_files"})
}

func TestFix_JSONKeySetMatchesSpec(t *testing.T) {
	assertExactJSONKeys(t, Fix{}, []string{"attempts", "give_up"})
}

func TestCandidate_JSONKeySetMatchesSpec(t *testing.T) {
	assertExactJSONKeys(t, Candidate{}, []string{"id", "dir", "test_passed", "issue_count"})
}

// assertExactJSONKeys marshals v and fails the test unless its top-level
// JSON object keys are exactly want, order aside. It exists to lock the
// on-disk field set the task spec fixes, and that T33 later relies on
// being identical across reviewer implementations.
func assertExactJSONKeys(t *testing.T, v any, want []string) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m) != len(want) {
		t.Fatalf("got %d keys %v, want %d keys %v", len(m), keysOf(m), len(want), want)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing expected key %q in %s", k, data)
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestStateGolden locks the exact on-disk shape of state.json for a
// representative, fully-populated State, so a future field addition or
// rename is a deliberate, reviewed change to this golden file rather than
// a silent drift. Run with -update to regenerate after an intentional
// schema change.
func TestStateGolden(t *testing.T) {
	st := NewState("ship the widget")
	st.Plan = Plan{Path: "artifacts/plan.md", Approved: true, Digest: "sha256:aaa"}
	st.Code = Code{SessionID: "sess-1", LastDiff: "artifacts/diff-0001.patch", ChangedFiles: []string{"a.go", "b.go"}}
	st.Test = NewTest(belay.TestReport{Total: 5, Passed: 5, Failed: 0, Failures: []belay.TestFailure{}}, "artifacts/test.json")
	st.Review = NewReview(belay.QualityReport{
		Source: "golangci-lint", Gate: belay.GatePass, Issues: []belay.Issue{}, Summary: "clean",
	}, "")
	st.Fix = Fix{Attempts: 0, GiveUp: false}
	st.Candidates = []Candidate{{ID: "c1", Dir: "/work/candidates/c1", TestPassed: true, IssueCount: 0}}
	st.Winner = "c1"
	st.History = []HistoryEntry{
		{Seq: 1, Node: "plan", Attempt: 1, Status: "ok", Next: "approve"},
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')

	goldenPath := filepath.Join("testdata", "state.golden.json")
	if *update {
		if err := os.WriteFile(goldenPath, data, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(goldenPath) //nolint:gosec // fixed testdata path
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if diff := cmp.Diff(string(want), string(data)); diff != "" {
		t.Errorf("state.json shape differs from %s (-golden +got):\n%s", goldenPath, diff)
	}
}
