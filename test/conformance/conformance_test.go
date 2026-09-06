//go:build unix

package conformance

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/nodes/aireview"
	"github.com/dhaam-ai/belay/internal/sonar/scanner"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// --- JSON shape helpers ---
//
// "Shape" in this file always means: the top-level key set, and, key by
// key, the JSON kind of the value (object / array / string / number / bool
// / null) — never the value's content. Content is allowed, and in
// gate_pass's case expected, to differ between the two adapters; kind is
// not.

// jsonObject marshals v and re-decodes it as a map of raw top-level
// values, so its keys and per-key JSON kinds can be inspected without
// hard-coding v's Go field list.
func jsonObject(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %T as a JSON object: %v", v, err)
	}
	return m
}

// jsonKind classifies raw's top-level JSON kind. It is deliberately naive
// (a prefix check, not a parse) because the values here are already known
// to be valid JSON — they came out of encoding/json — and a naive
// classifier keeps the failure a reader sees closest to "the bytes were
// literally null" rather than routed through another decode step.
func jsonKind(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	switch {
	case s == "":
		return "absent"
	case s == "null":
		return "null"
	case strings.HasPrefix(s, "["):
		return "array"
	case strings.HasPrefix(s, "{"):
		return "object"
	case strings.HasPrefix(s, `"`):
		return "string"
	case s == "true", s == "false":
		return "bool"
	default:
		return "number"
	}
}

// sortedKeys returns m's keys in sorted order, for a stable diff.
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// assertSameShape is requirement 1: an identical top-level key set between
// a and b, and, key by key, an identical JSON kind. Key-set equality is
// guaranteed here by Go's type system alone — both belay.QualityReport and
// state.Review are single shared struct types with no omitempty tag, so
// any two values of either type always marshal to the same key set
// regardless of content. It is asserted anyway, explicitly, because that
// guarantee is exactly the property T33 exists to keep true by
// construction rather than by every future field addition remembering it.
// The per-key JSON-kind check below is what actually has teeth: it is what
// tells "issues": [] apart from "issues": null, which the same struct
// definition does NOT protect against — that distinction is made by
// whether the Go slice behind it was nil, and cloneIssues/cloneSlice can
// each get that wrong independently of the struct's shape.
func assertSameShape(t *testing.T, label string, a, b map[string]json.RawMessage) {
	t.Helper()
	ka, kb := sortedKeys(a), sortedKeys(b)
	if !slices.Equal(ka, kb) {
		t.Fatalf("%s: top-level key sets differ:\n  scanner:  %v\n  aireview: %v", label, ka, kb)
	}
	for _, k := range ka {
		gk, wk := jsonKind(a[k]), jsonKind(b[k])
		if gk != wk {
			t.Errorf("%s: key %q: JSON kind differs: scanner=%s aireview=%s (raw: scanner=%s aireview=%s)",
				label, k, gk, wk, a[k], b[k])
		}
	}
}

// --- per-report structural invariants ---
//
// These hold for EVERY conforming belay.Reviewer on EVERY path, not just
// pairwise between these two — they are pkg/belay/quality.go's own
// contract. Running them against both adapters, across all five
// scenarios, is what requirement 2 ("assert non-nil explicitly on every
// path") asks for.

// assertIssuesNeverNil is requirement 2. It checks both the Go value (the
// contract belay.QualityReport.Issues documents) and the JSON it produces
// (the shape a fixture or a downstream tool actually sees). The two checks
// happen to agree for the append([]belay.Issue(nil), src...) bug this
// suite exists to catch — that mistake collapses a non-nil-empty slice
// back to nil, which fails both at once — but the JSON check is the one
// that matters: it is the surface a real fixture comparison exercises,
// not the Go value a caller never sees directly.
func assertIssuesNeverNil(t *testing.T, label string, report belay.QualityReport) {
	t.Helper()
	if report.Issues == nil {
		t.Errorf("%s: Issues is nil; belay.QualityReport.Issues must never be nil, even when empty", label)
	}
	obj := jsonObject(t, report)
	if kind := jsonKind(obj["issues"]); kind != "array" {
		t.Errorf(`%s: "issues" serialized as %s, want array — a nil slice serializes as null, `+
			`which is the exact regression this suite exists to catch`, label, kind)
	}
}

// assertCountsIsHistogram checks that report.Counts is exactly the
// severity histogram of report.Issues, per belay.QualityReport.Counts's
// own doc comment, and that Counts.Total() always agrees with
// len(Issues).
func assertCountsIsHistogram(t *testing.T, label string, report belay.QualityReport) {
	t.Helper()
	var want belay.Counts
	for _, iss := range report.Issues {
		switch iss.Severity {
		case belay.SeverityBlocker:
			want.Blocker++
		case belay.SeverityCritical:
			want.Critical++
		case belay.SeverityMajor:
			want.Major++
		case belay.SeverityMinor:
			want.Minor++
		default:
			want.Info++
		}
	}
	if report.Counts != want {
		t.Errorf("%s: Counts %+v is not the severity histogram of Issues (want %+v)", label, report.Counts, want)
	}
	if got, wantTotal := report.Counts.Total(), len(report.Issues); got != wantTotal {
		t.Errorf("%s: Counts.Total()=%d but len(Issues)=%d; these must always agree", label, got, wantTotal)
	}
}

// assertRawIsValidJSONObject checks that Raw is always present and is a
// JSON object — never nil, never a bare scalar, never absent — per
// belay.QualityReport.Raw's doc comment. Raw's INTERNAL shape is exempt
// from any cross-adapter comparison (RawScan and RawReview are
// deliberately different structs); only "is it a non-nil JSON object at
// all" is checked here.
func assertRawIsValidJSONObject(t *testing.T, label string, report belay.QualityReport) {
	t.Helper()
	if len(report.Raw) == 0 {
		t.Errorf("%s: Raw is empty, want a non-nil JSON object", label)
		return
	}
	if !json.Valid(report.Raw) {
		t.Errorf("%s: Raw is not valid JSON: %s", label, report.Raw)
		return
	}
	if kind := jsonKind(report.Raw); kind != "object" {
		t.Errorf("%s: Raw is a JSON %s, want object", label, kind)
	}
}

// assertSummaryNonEmpty checks belay.QualityReport.Summary's "never empty"
// contract.
func assertSummaryNonEmpty(t *testing.T, label string, report belay.QualityReport) {
	t.Helper()
	if strings.TrimSpace(report.Summary) == "" {
		t.Errorf("%s: Summary is empty, want a non-empty one-line synopsis", label)
	}
}

// assertGateResolved checks that Gate is one of the three terminal values a
// completed Review call is required to reach — never the zero value,
// GateUnknown.
func assertGateResolved(t *testing.T, label string, report belay.QualityReport) {
	t.Helper()
	switch report.Gate {
	case belay.GatePass, belay.GateFail, belay.GateError:
	default:
		t.Errorf("%s: Gate = %v, want GatePass, GateFail or GateError (never GateUnknown)", label, report.Gate)
	}
}

// assertGateCountsInvariant checks belay.Reviewer's documented invariant:
// Gate == GatePass if and only if Counts.AtOrAbove(failOn) == 0.
// GateError is the documented exception (both here and in
// pkg/belay/quality.go's own Reviewer doc comment) — it is the absence of
// a verdict, not a verdict about issues, so it is exempt.
func assertGateCountsInvariant(t *testing.T, label string, report belay.QualityReport, failOn belay.Severity) {
	t.Helper()
	if report.Gate == belay.GateError {
		return
	}
	atOrAbove := report.Counts.AtOrAbove(failOn)
	wantPass := atOrAbove == 0
	gotPass := report.Gate == belay.GatePass
	if gotPass != wantPass {
		t.Errorf("%s: Gate=%v but Counts.AtOrAbove(%v)=%d — the invariant "+
			"Gate==GatePass iff Counts.AtOrAbove(failOn)==0 does not hold",
			label, report.Gate, failOn, atOrAbove)
	}
}

// --- reviewer roster ---

// TestReviewersUnderTestIsComplete pins the size of reviewersUnderTest.
//
// This is requirement 5. The roster is meant to enumerate EVERY
// belay.Reviewer implementation belay ships; if one is added (or removed)
// without a matching update here, this test fails the build rather than
// letting the new adapter go unchecked by every other test in this
// package. Growing the roster is exactly two changes made together: add
// the entry to reviewersUnderTest in scenarios_test.go, and update
// wantReviewerCount below to match — never one without the other.
func TestReviewersUnderTestIsComplete(t *testing.T) {
	// internal/sonar/scanner (the deterministic SonarQube-CLI reviewer)
	// and internal/nodes/aireview (the AI-augmented MCP reviewer) are the
	// only two belay.Reviewer implementations in this codebase as of T33.
	const wantReviewerCount = 2
	if got := len(reviewersUnderTest); got != wantReviewerCount {
		t.Fatalf("reviewersUnderTest has %d entries, want %d — a belay.Reviewer implementation "+
			"was added or removed without updating this conformance suite to match; see this "+
			"test's doc comment", got, wantReviewerCount)
	}
}

// TestQualityReportConformance is requirements 1, 2 and 4: it drives every
// entry in reviewersUnderTest through every scenario, checks each report's
// own structural invariants, and — pairwise, since there are exactly two
// reviewers — asserts their belay.QualityReport values share an identical
// shape with Source the only permitted difference in value.
func TestQualityReportConformance(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			req := sc.request(t)

			reports := make(map[string]belay.QualityReport, len(reviewersUnderTest))
			for _, rc := range reviewersUnderTest {
				reviewer := rc.build(t, sc)
				report, err := reviewer.Review(context.Background(), req)

				if wantErr, ok := sc.wantErr[rc.name]; ok {
					if gotErr := err != nil; gotErr != wantErr {
						t.Errorf("%s: err != nil = %v, want %v (err=%v)", rc.name, gotErr, wantErr, err)
					}
				} else {
					t.Fatalf("scenario %q has no wantErr entry for reviewer %q", sc.name, rc.name)
				}

				if report.Gate != sc.wantGate {
					t.Errorf("%s: Gate = %v, want %v", rc.name, report.Gate, sc.wantGate)
				}

				assertIssuesNeverNil(t, rc.name, report)
				assertCountsIsHistogram(t, rc.name, report)
				assertRawIsValidJSONObject(t, rc.name, report)
				assertSummaryNonEmpty(t, rc.name, report)
				assertGateResolved(t, rc.name, report)
				assertGateCountsInvariant(t, rc.name, report, req.FailOn)

				reports[rc.name] = report
			}

			scan, ai := reports[nameScanner], reports[nameAIReview]

			// Source is the one field belay.QualityReport documents as
			// permitted to differ in value between conforming adapters —
			// checked here as a semantic assertion, distinct from the
			// shape check below, which treats "source" like any other
			// key (same JSON kind: string, on both sides).
			if scan.Source == "" || ai.Source == "" {
				t.Errorf("Source must never be empty: scanner=%q aireview=%q", scan.Source, ai.Source)
			}
			if scan.Source == ai.Source {
				t.Errorf("scanner and aireview reported identical Source %q; Source is documented "+
					"as the one field expected to differ between them", scan.Source)
			}
			if scan.Source != scanner.Source {
				t.Errorf("scanner report Source = %q, want the scanner.Source constant %q", scan.Source, scanner.Source)
			}
			if ai.Source != aireview.Source {
				t.Errorf("aireview report Source = %q, want the aireview.Source constant %q", ai.Source, aireview.Source)
			}

			assertSameShape(t, "belay.QualityReport", jsonObject(t, scan), jsonObject(t, ai))
		})
	}
}

// TestBlackboardProjectionConformance is requirement 3: it projects both
// reviewers' reports through state.NewReview — the function that actually
// puts a review result into state.json — and asserts the resulting
// state.Review values share an identical shape, the same way the raw
// reports do. This is the structure the challenge cares about: state.json
// is what a resumed run reads, not the archived report artifact.
func TestBlackboardProjectionConformance(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			req := sc.request(t)

			projections := make(map[string]state.Review, len(reviewersUnderTest))
			for _, rc := range reviewersUnderTest {
				reviewer := rc.build(t, sc)
				report, _ := reviewer.Review(context.Background(), req)

				// A fixed, adapter-agnostic archive path: state.Review's
				// own doc comment describes ReportPath as "where the
				// dispatcher archived the full report", which is
				// dispatcher-chosen and identical regardless of which
				// reviewer produced the report.
				projection := state.NewReview(report, "review-1.json")

				if projection.Issues == nil {
					t.Errorf("%s: state.Review.Issues is nil after projecting a report whose own "+
						"Issues was non-nil; state.NewReview's cloneSlice must preserve non-nil-empty, "+
						"not just nil", rc.name)
				}
				obj := jsonObject(t, projection)
				if kind := jsonKind(obj["issues"]); kind != "array" {
					t.Errorf(`%s: state.Review "issues" serialized as %s, want array`, rc.name, kind)
				}

				projections[rc.name] = projection
			}

			assertSameShape(t, "state.Review",
				jsonObject(t, projections[nameScanner]), jsonObject(t, projections[nameAIReview]))
		})
	}
}

// TestGateFailSyntheticIssueSharesRuleID checks a content-level, not merely
// shape-level, agreement: on the gate_fail scenario (chosen so that
// neither adapter's fetched issues explain the failure on their own — see
// its doc comment in scenarios_test.go), both packages are documented to
// synthesize exactly one belay.SeverityBlocker issue carrying the same
// RuleID, GateRuleID = "belay:sonar-quality-gate", shared verbatim between
// internal/sonar/scanner and internal/nodes/aireview. This is what lets a
// human or a downstream tool filter that marker out without knowing which
// reviewer produced the report.
func TestGateFailSyntheticIssueSharesRuleID(t *testing.T) {
	var sc scenario
	for _, s := range scenarios {
		if s.name == "gate_fail" {
			sc = s
			break
		}
	}
	if sc.name == "" {
		t.Fatal(`scenario "gate_fail" not found`)
	}

	req := sc.request(t)
	scanReport, err := sc.newScanner(t).Review(context.Background(), req)
	if err != nil {
		t.Fatalf("scanner: unexpected error: %v", err)
	}
	aiReport, err := sc.newAIReview(t).Review(context.Background(), req)
	if err != nil {
		t.Fatalf("aireview: unexpected error: %v", err)
	}

	if len(scanReport.Issues) != 1 {
		t.Fatalf("scanner: len(Issues) = %d, want exactly 1 synthetic gate issue", len(scanReport.Issues))
	}
	if len(aiReport.Issues) != 1 {
		t.Fatalf("aireview: len(Issues) = %d, want exactly 1 synthetic gate issue", len(aiReport.Issues))
	}

	if scanReport.Issues[0].RuleID != scanner.GateRuleID {
		t.Errorf("scanner: synthetic issue RuleID = %q, want scanner.GateRuleID %q",
			scanReport.Issues[0].RuleID, scanner.GateRuleID)
	}
	if aiReport.Issues[0].RuleID != aireview.GateRuleID {
		t.Errorf("aireview: synthetic issue RuleID = %q, want aireview.GateRuleID %q",
			aiReport.Issues[0].RuleID, aireview.GateRuleID)
	}
	if scanReport.Issues[0].RuleID != aiReport.Issues[0].RuleID {
		t.Errorf("synthetic gate issue RuleID differs between adapters: scanner=%q aireview=%q",
			scanReport.Issues[0].RuleID, aiReport.Issues[0].RuleID)
	}
	if scanReport.Issues[0].Severity != belay.SeverityBlocker {
		t.Errorf("scanner: synthetic issue Severity = %v, want SeverityBlocker", scanReport.Issues[0].Severity)
	}
	if aiReport.Issues[0].Severity != belay.SeverityBlocker {
		t.Errorf("aireview: synthetic issue Severity = %v, want SeverityBlocker", aiReport.Issues[0].Severity)
	}
}
