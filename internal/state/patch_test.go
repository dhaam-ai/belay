package state

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// cloneCandidates, cloneHistory and cloneStrings each return an
// independent copy of their argument, preserving nil vs. non-nil-empty
// exactly. append([]T(nil), src...) is not good enough for that: appending
// zero elements onto a nil destination returns nil regardless of whether
// src itself was nil or non-nil-empty, silently losing the distinction
// this test relies on. make+copy does not have that problem.
func cloneCandidates(c []Candidate) []Candidate {
	if c == nil {
		return nil
	}
	out := make([]Candidate, len(c))
	copy(out, c)
	return out
}

func cloneHistory(h []HistoryEntry) []HistoryEntry {
	if h == nil {
		return nil
	}
	out := make([]HistoryEntry, len(h))
	copy(out, h)
	return out
}

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func baseTestState() State {
	st := NewState("ship the widget")
	st.Plan = Plan{Path: "artifacts/plan.md", Approved: true, Digest: "sha256:aaa"}
	st.Code = Code{SessionID: "sess-1", LastDiff: "artifacts/diff-0001.patch", ChangedFiles: []string{"a.go"}}
	return st
}

// TestPatch_Apply_Idempotent is the acceptance test for the core contract
// of Patch: applying the same Patch value a second time, on top of the
// State the first application already produced, must be a no-op. This is
// exactly the crash-resume scenario ADR-0002 exists to make provably
// correct — the dispatcher may not be able to tell whether a Patch it
// built was already applied before a crash, so applying it again must be
// safe.
func TestPatch_Apply_Idempotent(t *testing.T) {
	fixTwo := 2
	fixThree := 3
	goal := "new goal"
	winner := "candidate-a"
	plan := Plan{Path: "artifacts/plan.md", Approved: true, Digest: "sha256:bbb"}
	code := Code{SessionID: "sess-2", LastDiff: "artifacts/diff-0002.patch", ChangedFiles: []string{"a.go", "b.go"}}
	testResult := Test{Passed: 10, Total: 10, Failed: 0, ReportPath: "artifacts/test.json"}
	review := Review{Source: "golangci-lint", Summary: "clean"}
	candidates := []Candidate{{ID: "c1", Dir: "/tmp/c1", TestPassed: true, IssueCount: 0}}

	tests := []struct {
		name  string
		patch Patch
	}{
		{"goal", Patch{Goal: &goal}},
		{"plan", Patch{Plan: &plan}},
		{"code", Patch{Code: &code}},
		{"test", Patch{Test: &testResult}},
		{"review", Patch{Review: &review}},
		{"fix attempts absolute set", Patch{Fix: &Fix{Attempts: 3, GiveUp: false}}},
		{"candidates", Patch{Candidates: &candidates}},
		{"winner", Patch{Winner: &winner}},
		{
			"history append",
			Patch{History: &HistoryEntry{Seq: 7, Node: "code", Attempt: 1, Status: "ok", Next: "test", Time: time.Unix(0, 0).UTC()}},
		},
		{
			"combined patch mirroring a real node result",
			Patch{
				Fix:  &Fix{Attempts: fixTwo, GiveUp: false},
				Test: &testResult,
				History: &HistoryEntry{
					Seq: 11, Node: "test", Attempt: 1, Status: "ok", Next: "review", Time: time.Unix(100, 0).UTC(),
				},
			},
		},
		{
			"fix attempts then give up, still absolute",
			Patch{Fix: &Fix{Attempts: fixThree, GiveUp: true}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			once := baseTestState()
			if err := tt.patch.Apply(&once); err != nil {
				t.Fatalf("first Apply: %v", err)
			}

			// twice starts as an independent deep copy of once, so the
			// second Apply cannot mutate slices the first Apply already
			// produced out from under the comparison below. Cloning must
			// preserve nil-vs-empty exactly (append(nil, none...) would
			// silently collapse an empty-but-non-nil slice to nil, which
			// is not what a real second read of state.json would ever
			// do), so nil is special-cased rather than run through append.
			twice := once
			twice.Candidates = cloneCandidates(once.Candidates)
			twice.History = cloneHistory(once.History)
			twice.Code.ChangedFiles = cloneStrings(once.Code.ChangedFiles)
			if err := tt.patch.Apply(&twice); err != nil {
				t.Fatalf("second Apply: %v", err)
			}

			if diff := cmp.Diff(once, twice); diff != "" {
				t.Errorf("applying the same patch twice changed the result (-once +twice):\n%s", diff)
			}
		})
	}
}

func TestPatch_Apply_FixAttemptsIsAbsoluteNotADelta(t *testing.T) {
	st := baseTestState()
	patch := Patch{Fix: &Fix{Attempts: 3}}

	if err := patch.Apply(&st); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if st.Fix.Attempts != 3 {
		t.Fatalf("after first Apply: Attempts = %d, want 3", st.Fix.Attempts)
	}

	if err := patch.Apply(&st); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if st.Fix.Attempts != 3 {
		t.Fatalf("after second Apply: Attempts = %d, want 3 (not accumulated to 6)", st.Fix.Attempts)
	}
}

func TestPatch_Apply_HistoryUpsertReplacesSameSeq(t *testing.T) {
	st := baseTestState()

	first := Patch{History: &HistoryEntry{Seq: 5, Node: "fix", Attempt: 1, Status: "retrying", Next: "test"}}
	if err := first.Apply(&st); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if len(st.History) != 1 {
		t.Fatalf("len(History) = %d, want 1", len(st.History))
	}

	// The dispatcher recomputes and reapplies a patch for the same
	// journal record (same Seq) with updated content — this must replace
	// the entry, not append a second one.
	updated := Patch{History: &HistoryEntry{Seq: 5, Node: "fix", Attempt: 1, Status: "ok", Next: "test", Summary: "resolved"}}
	if err := updated.Apply(&st); err != nil {
		t.Fatalf("updated Apply: %v", err)
	}
	if len(st.History) != 1 {
		t.Fatalf("len(History) = %d, want 1 after upsert", len(st.History))
	}
	if st.History[0].Status != "ok" || st.History[0].Summary != "resolved" {
		t.Errorf("History[0] = %+v, want the updated entry", st.History[0])
	}
}

func TestPatch_Apply_HistoryAppendsDistinctSeqs(t *testing.T) {
	st := baseTestState()
	entries := []HistoryEntry{
		{Seq: 1, Node: "plan", Status: "ok", Attempt: 1},
		{Seq: 2, Node: "approve", Status: "ok", Attempt: 1},
		{Seq: 3, Node: "code", Status: "ok", Attempt: 1},
	}
	for _, e := range entries {
		e := e
		if err := (Patch{History: &e}).Apply(&st); err != nil {
			t.Fatalf("Apply seq %d: %v", e.Seq, err)
		}
	}
	if len(st.History) != 3 {
		t.Fatalf("len(History) = %d, want 3", len(st.History))
	}
	for i, e := range entries {
		if st.History[i].Seq != e.Seq {
			t.Errorf("History[%d].Seq = %d, want %d", i, st.History[i].Seq, e.Seq)
		}
	}
}

func TestPatch_Apply_RejectsZeroSeqHistory(t *testing.T) {
	st := baseTestState()
	patch := Patch{History: &HistoryEntry{Seq: 0, Node: "plan", Status: "ok"}}
	err := patch.Apply(&st)
	if err == nil {
		t.Fatal("expected an error for a zero-Seq history entry")
	}
	if !errors.Is(err, ErrInvalidPatch) {
		t.Errorf("error does not wrap ErrInvalidPatch: %v", err)
	}
	if len(st.History) != 0 {
		t.Errorf("state was mutated despite a rejected patch: History = %+v", st.History)
	}
}

func TestPatch_Apply_EmptyPatchIsANoOp(t *testing.T) {
	st := baseTestState()
	before := st
	before.Code.ChangedFiles = append([]string(nil), st.Code.ChangedFiles...)

	if err := (Patch{}).Apply(&st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if diff := cmp.Diff(before, st); diff != "" {
		t.Errorf("empty patch changed state (-before +after):\n%s", diff)
	}
}

func TestPatch_Apply_CandidatesDoesNotAliasCallerSlice(t *testing.T) {
	st := baseTestState()
	callerSlice := []Candidate{{ID: "c1"}}
	patch := Patch{Candidates: &callerSlice}

	if err := patch.Apply(&st); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	callerSlice[0].ID = "mutated-after-apply"

	if st.Candidates[0].ID != "c1" {
		t.Errorf("State.Candidates aliases the caller's slice: got %q, want %q", st.Candidates[0].ID, "c1")
	}
}

// TestPatch_Apply_CandidatesPreservesNonNilEmpty guards against a real bug
// this package once had: cloning a slice with append([]T(nil), src...)
// silently collapses a non-nil-empty src to nil, because appending zero
// elements onto a nil destination never allocates. That would have undone
// NewState's "candidates: [] rather than null" guarantee the first time
// any Patch touched Candidates with an empty-but-explicit slice.
func TestPatch_Apply_CandidatesPreservesNonNilEmpty(t *testing.T) {
	st := baseTestState()
	empty := []Candidate{}
	if err := (Patch{Candidates: &empty}).Apply(&st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if st.Candidates == nil {
		t.Error("Patch.Apply turned a non-nil empty Candidates slice into nil")
	}
}

func TestPatch_Apply_NilFieldsLeaveThoseCorrespondingStateFieldsUntouched(t *testing.T) {
	st := baseTestState()
	original := st
	original.Code.ChangedFiles = append([]string(nil), st.Code.ChangedFiles...)

	goal := "only the goal changes"
	if err := (Patch{Goal: &goal}).Apply(&st); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if st.Goal != goal {
		t.Errorf("Goal = %q, want %q", st.Goal, goal)
	}
	if diff := cmp.Diff(original.Plan, st.Plan); diff != "" {
		t.Errorf("Plan changed despite a nil Patch.Plan (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(original.Code, st.Code); diff != "" {
		t.Errorf("Code changed despite a nil Patch.Code (-want +got):\n%s", diff)
	}
}
