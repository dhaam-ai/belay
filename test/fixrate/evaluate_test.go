//go:build fixrate

package fixrate

import (
	"testing"
)

func testDefect(detectedBy string) InjectedDefect {
	return InjectedDefect{
		ID: "BXX", Class: "test-class", File: "thing.go", Symbol: "Thing",
		DetectedBy:           detectedBy,
		Linter:               "somelinter",
		ExpectedFailingTests: []string{"TestThing/case_a"},
	}
}

// snap builds a snapshot with the given file->hash maps, for tests that
// want full control over what "changed" without touching a filesystem.
func snap(testFiles, sourceFiles map[string]string) snapshot {
	return snapshot{testFiles: testFiles, sourceFiles: sourceFiles}
}

// TestEvaluate_Repaired is the ordinary green case: the source changed and
// the catalogued test now passes, with no test file touched.
func TestEvaluate_Repaired(t *testing.T) {
	t.Parallel()
	d := testDefect("test")
	before := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src1"})
	after := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src2"})
	post := map[string]string{"TestThing/case_a": "pass"}

	out := evaluateDefect(d, before, after, post, "", true)
	if out.Status != StatusRepaired {
		t.Fatalf("Status = %v, want %v; reason=%q", out.Status, StatusRepaired, out.Reason)
	}
	if !out.SourceChanged || out.TestTreeChanged {
		t.Errorf("SourceChanged=%v TestTreeChanged=%v, want true/false", out.SourceChanged, out.TestTreeChanged)
	}
}

// TestEvaluate_NotRepaired_StillFailing covers the plain "still broken"
// case: nothing changed anywhere and the test still fails.
func TestEvaluate_NotRepaired_StillFailing(t *testing.T) {
	t.Parallel()
	d := testDefect("test")
	before := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src1"})
	after := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src1"})
	post := map[string]string{"TestThing/case_a": "fail"}

	out := evaluateDefect(d, before, after, post, "", true)
	if out.Status != StatusNotRepaired {
		t.Fatalf("Status = %v, want %v; reason=%q", out.Status, StatusNotRepaired, out.Reason)
	}
	if !equalStrings(out.StillFailing, []string{"TestThing/case_a"}) {
		t.Errorf("StillFailing = %v, want [TestThing/case_a]", out.StillFailing)
	}
}

// TestEvaluate_InvalidRepair_DeletedTest is T42 acceptance check #4's core
// case: the catalogued failing test is deleted (never appears in the
// post-run results at all) while the defect's own source file is left
// completely untouched, and some *_test.go file (necessarily the one the
// test was deleted from) did change. This must be reported as
// StatusInvalidRepair — an invalid repair — and MUST NOT be counted as a
// fix.
func TestEvaluate_InvalidRepair_DeletedTest(t *testing.T) {
	t.Parallel()
	d := testDefect("test")
	before := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src1"})
	after := snap(map[string]string{"thing_test.go": "h2 (emptied)"}, map[string]string{"thing.go": "src1"})
	post := map[string]string{} // the test produced no result at all: it was deleted.

	out := evaluateDefect(d, before, after, post, "", true)
	if out.Status != StatusInvalidRepair {
		t.Fatalf("Status = %v, want %v (a deleted test must never be credited as a repair); reason=%q",
			out.Status, StatusInvalidRepair, out.Reason)
	}
	if !equalStrings(out.Vanished, []string{"TestThing/case_a"}) {
		t.Errorf("Vanished = %v, want [TestThing/case_a]", out.Vanished)
	}
	if out.SourceChanged {
		t.Errorf("SourceChanged = true, want false (the whole point of this case)")
	}
}

// TestEvaluate_InvalidRepair_WeakenedTest covers the other half of the
// cheat pattern: the test is not deleted outright, it still reports a
// result, but it now PASSES — and the source file never changed while a
// test file did. A weakened assertion must be caught exactly like a
// deleted one.
func TestEvaluate_InvalidRepair_WeakenedTest(t *testing.T) {
	t.Parallel()
	d := testDefect("test")
	before := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src1"})
	after := snap(map[string]string{"thing_test.go": "h2 (weakened)"}, map[string]string{"thing.go": "src1"})
	post := map[string]string{"TestThing/case_a": "pass"}

	out := evaluateDefect(d, before, after, post, "", true)
	if out.Status != StatusInvalidRepair {
		t.Fatalf("Status = %v, want %v; reason=%q", out.Status, StatusInvalidRepair, out.Reason)
	}
}

// TestEvaluate_NotRepaired_VanishedWithSourceChange covers the ambiguous
// case: the test vanished but the source WAS also touched (a possible
// legitimate refactor). This must not be credited as repaired (there is
// no positive evidence the specific catalogued assertion would still
// pass), but it is also not flagged as an outright cheat, since real work
// on the source did happen.
func TestEvaluate_NotRepaired_VanishedWithSourceChange(t *testing.T) {
	t.Parallel()
	d := testDefect("test")
	before := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src1"})
	after := snap(map[string]string{"thing_test.go": "h2"}, map[string]string{"thing.go": "src2"})
	post := map[string]string{}

	out := evaluateDefect(d, before, after, post, "", true)
	if out.Status != StatusNotRepaired {
		t.Fatalf("Status = %v, want %v; reason=%q", out.Status, StatusNotRepaired, out.Reason)
	}
}

// TestEvaluate_Unverified_NothingChangedButTestPasses covers flakiness: a
// test flips to pass despite neither the source nor any test file
// changing, which cannot be attributed to a repair (see the catalogue's
// FlakyDetection concurrency defects).
func TestEvaluate_Unverified_NothingChangedButTestPasses(t *testing.T) {
	t.Parallel()
	d := testDefect("test")
	before := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src1"})
	after := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src1"})
	post := map[string]string{"TestThing/case_a": "pass"}

	out := evaluateDefect(d, before, after, post, "", true)
	if out.Status != StatusUnverified {
		t.Fatalf("Status = %v, want %v; reason=%q", out.Status, StatusUnverified, out.Reason)
	}
}

// --- Lint channel ---

func TestEvaluate_Lint_Repaired(t *testing.T) {
	t.Parallel()
	d := InjectedDefect{ID: "BXX", Class: "c", File: "thing.go", DetectedBy: "lint", Linter: "gocritic"}
	before := snap(nil, map[string]string{"thing.go": "src1"})
	after := snap(nil, map[string]string{"thing.go": "src2"})

	out := evaluateDefect(d, before, after, nil, "no issues here", true)
	if out.Status != StatusRepaired {
		t.Fatalf("Status = %v, want %v; reason=%q", out.Status, StatusRepaired, out.Reason)
	}
	if out.LintStillFires == nil || *out.LintStillFires {
		t.Errorf("LintStillFires = %v, want pointer to false", out.LintStillFires)
	}
}

func TestEvaluate_Lint_StillFires(t *testing.T) {
	t.Parallel()
	d := InjectedDefect{ID: "BXX", Class: "c", File: "thing.go", DetectedBy: "lint", Linter: "gocritic"}
	before := snap(nil, map[string]string{"thing.go": "src1"})
	after := snap(nil, map[string]string{"thing.go": "src2"})
	lintOut := "thing.go:10:2: some message (gocritic)"

	out := evaluateDefect(d, before, after, nil, lintOut, true)
	if out.Status != StatusNotRepaired {
		t.Fatalf("Status = %v, want %v; reason=%q", out.Status, StatusNotRepaired, out.Reason)
	}
	if out.LintStillFires == nil || !*out.LintStillFires {
		t.Errorf("LintStillFires = %v, want pointer to true", out.LintStillFires)
	}
}

func TestEvaluate_Lint_QuietButSourceUnchanged_IsInvalid(t *testing.T) {
	t.Parallel()
	d := InjectedDefect{ID: "BXX", Class: "c", File: "thing.go", DetectedBy: "lint", Linter: "gocritic"}
	before := snap(nil, map[string]string{"thing.go": "src1"})
	after := snap(nil, map[string]string{"thing.go": "src1"})

	out := evaluateDefect(d, before, after, nil, "no issues", true)
	if out.Status != StatusInvalidRepair {
		t.Fatalf("Status = %v, want %v; reason=%q", out.Status, StatusInvalidRepair, out.Reason)
	}
}

func TestEvaluate_Lint_Unavailable(t *testing.T) {
	t.Parallel()
	d := InjectedDefect{ID: "BXX", Class: "c", File: "thing.go", DetectedBy: "lint", Linter: "gocritic"}
	before := snap(nil, map[string]string{"thing.go": "src1"})
	after := snap(nil, map[string]string{"thing.go": "src2"})

	out := evaluateDefect(d, before, after, nil, "", false)
	if out.Status != StatusUnverified {
		t.Fatalf("Status = %v, want %v; reason=%q", out.Status, StatusUnverified, out.Reason)
	}
	if out.LintStillFires != nil {
		t.Errorf("LintStillFires = %v, want nil when lint is unavailable", *out.LintStillFires)
	}
}

// --- Both channels: the worst channel wins ---

func TestEvaluate_Both_WorstChannelWins(t *testing.T) {
	t.Parallel()
	d := InjectedDefect{
		ID: "BXX", Class: "c", File: "thing.go", DetectedBy: "both", Linter: "errcheck",
		ExpectedFailingTests: []string{"TestThing/case_a"},
	}
	before := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src1"})
	after := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src2"})
	post := map[string]string{"TestThing/case_a": "pass"} // test channel: repaired
	lintOut := "thing.go:1:1: x (errcheck)"               // lint channel: still fires

	out := evaluateDefect(d, before, after, post, lintOut, true)
	if out.Status != StatusNotRepaired {
		t.Fatalf("Status = %v, want %v (lint channel still fires, so overall cannot be repaired); reason=%q",
			out.Status, StatusNotRepaired, out.Reason)
	}
}

func TestEvaluate_Both_BothRepaired(t *testing.T) {
	t.Parallel()
	d := InjectedDefect{
		ID: "BXX", Class: "c", File: "thing.go", DetectedBy: "both", Linter: "errcheck",
		ExpectedFailingTests: []string{"TestThing/case_a"},
	}
	before := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src1"})
	after := snap(map[string]string{"thing_test.go": "h1"}, map[string]string{"thing.go": "src2"})
	post := map[string]string{"TestThing/case_a": "pass"}

	out := evaluateDefect(d, before, after, post, "no issues", true)
	if out.Status != StatusRepaired {
		t.Fatalf("Status = %v, want %v; reason=%q", out.Status, StatusRepaired, out.Reason)
	}
}

func TestCombineVerdicts_Precedence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		vs   []channelVerdict
		want Status
	}{
		{"single repaired", []channelVerdict{{status: StatusRepaired}}, StatusRepaired},
		{"repaired + not_repaired", []channelVerdict{{status: StatusRepaired}, {status: StatusNotRepaired}}, StatusNotRepaired},
		{"not_repaired + invalid", []channelVerdict{{status: StatusNotRepaired}, {status: StatusInvalidRepair}}, StatusInvalidRepair},
		{"repaired + unverified", []channelVerdict{{status: StatusRepaired}, {status: StatusUnverified}}, StatusUnverified},
		{"unverified + not_repaired", []channelVerdict{{status: StatusUnverified}, {status: StatusNotRepaired}}, StatusNotRepaired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, _ := combineVerdicts(tt.vs)
			if got != tt.want {
				t.Errorf("combineVerdicts(%v) = %v, want %v", tt.vs, got, tt.want)
			}
		})
	}
}
