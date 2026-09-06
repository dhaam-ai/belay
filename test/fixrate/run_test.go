//go:build fixrate

package fixrate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// fixedClock returns a deterministic Now func for tests that assert exact
// timestamps or need every belay run they drive to agree on the wall
// clock (a resumed-run scenario is out of this harness's scope, so this
// only needs to be a fixed, valid time.Time).
func fixedClock(t *testing.T) func() time.Time {
	t.Helper()
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return fixed }
}

func TestRun_Validation(t *testing.T) {
	t.Parallel()
	validAgent := &belaytest.FakeAgent{}
	validSelection := Selection{Bugs: []string{"B01"}}

	tests := []struct {
		name    string
		opts    RunOptions
		wantErr error // checked with errors.Is when non-nil
	}{
		{
			name:    "unknown mode",
			opts:    RunOptions{Mode: "bogus", Agent: validAgent, Selection: validSelection},
			wantErr: ErrUnknownMode,
		},
		{
			name:    "no agent",
			opts:    RunOptions{Mode: ModeReplay, Selection: validSelection},
			wantErr: ErrNoAgent,
		},
		{
			name:    "live mode without confirmation",
			opts:    RunOptions{Mode: ModeLive, Agent: validAgent, Selection: validSelection},
			wantErr: ErrLiveConfirmationRequired,
		},
		{
			name:    "live mode with wrong confirmation text",
			opts:    RunOptions{Mode: ModeLive, Agent: validAgent, Selection: validSelection, Confirm: "yes"},
			wantErr: ErrLiveConfirmationRequired,
		},
		{
			name:    "invalid selection",
			opts:    RunOptions{Mode: ModeReplay, Agent: validAgent, Selection: Selection{}},
			wantErr: ErrInvalidSelection,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Run(t.Context(), tt.opts)
			if err == nil {
				t.Fatal("Run() = nil error, want one")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Run() error = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

// demoPlan is the fixed, hand-picked 5-defect set used by the end-to-end
// tests below: 3 files the scripted agent genuinely repairs, one it
// leaves untouched (StrategyNoop), and one where it "fixes" the failing
// test by deleting it instead of the bug (StrategyCheat) — every outcome
// category Result can report, in one deterministic run. See
// evaluate_test.go for focused, filesystem-free tests of the
// classification logic itself; this is the same logic exercised through
// the real dispatcher end to end.
//
// Choosing distinct files per defect (delay.go, policy.go, errors.go,
// ledger.go, orchestrate.go — every source file fixtures/seeded-bug/src
// has) is deliberate: it keeps each defect's outcome attributable to
// exactly one file-level strategy, with no risk of one defect's
// StrategyRepair also silently repairing a different, same-file
// StrategyNoop defect. B04 (also policy.go) was passed over for B05
// specifically because B04's own catalogued BreaksTests overlaps
// orchestrate.go's TestRun suite (see fixtures/seeded-bug/bugs/
// b04_inverted_condition.go), which would have entangled its outcome
// with B16's; B05 has no such overlap.
func demoSelection() Selection {
	return Selection{Bugs: []string{"B01", "B05", "B15", "B16", "B17"}}
}

func demoPlan() ScriptedPlan {
	return ScriptedPlan{FileStrategy: map[string]Strategy{
		"delay.go":       StrategyRepair, // B01 off-by-one
		"policy.go":      StrategyRepair, // B05 inverted-condition
		"errors.go":      StrategyRepair, // B15 error-swallowed
		"orchestrate.go": StrategyNoop,   // B16 error-swallowed (both channels) — left broken
		"ledger.go":      StrategyCheat,  // B17 early-return-before-required-work — test deleted, not fixed
	}}
}

// TestRun_EndToEnd_ReplayMode_MixedOutcomes is T42 acceptance check #2 (an
// end-to-end run in replay/fake mode over >=5 catalogued defects) and,
// via its B17 assertion, acceptance check #4 (the cheat detector) end to
// end through the real dispatcher rather than evaluate.go's unit-level
// harness.
func TestRun_EndToEnd_ReplayMode_MixedOutcomes(t *testing.T) {
	fixtureDir, err := DefaultFixtureDir()
	if err != nil {
		t.Fatalf("DefaultFixtureDir() error = %v", err)
	}
	agent := NewScriptedAgent(filepath.Join(fixtureDir, "src"), demoPlan())

	result, err := Run(t.Context(), RunOptions{
		Mode:        ModeReplay,
		FixtureDir:  fixtureDir,
		Selection:   demoSelection(),
		Agent:       agent,
		Now:         fixedClock(t),
		KeepWorkDir: testing.Verbose(), // leave evidence on disk for -v inspection
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// --- The mandatory provenance disclosure (requirement 2) ---
	if result.Mode != ModeReplay {
		t.Errorf("Result.Mode = %q, want %q", result.Mode, ModeReplay)
	}
	if result.Provenance == "" || result.Provenance != ModeReplay.provenance() {
		t.Errorf("Result.Provenance = %q, want the standard replay-mode disclosure", result.Provenance)
	}

	// --- Zero token spend (requirement 6) ---
	if result.UsageUSD != 0 {
		t.Errorf("Result.UsageUSD = %v, want exactly 0 in replay mode", result.UsageUSD)
	}

	// --- Per-defect outcomes, by ID ---
	byID := make(map[string]DefectOutcome, len(result.Defects))
	for _, d := range result.Defects {
		byID[d.ID] = d
	}
	if len(byID) != 5 {
		t.Fatalf("Result.Defects has %d entries, want 5: %+v", len(byID), result.Defects)
	}

	wantStatus := map[string]Status{
		"B01": StatusRepaired,
		"B05": StatusRepaired,
		"B15": StatusRepaired,
		"B16": StatusNotRepaired,
		"B17": StatusInvalidRepair,
	}
	for id, want := range wantStatus {
		got, ok := byID[id]
		if !ok {
			t.Errorf("defect %s missing from Result.Defects", id)
			continue
		}
		if got.Status != want {
			t.Errorf("defect %s Status = %q, want %q (reason: %s)", id, got.Status, want, got.Reason)
		}
	}

	// B17 specifically: the cheat must be visible as a vanished test, not
	// merely an aggregate status.
	if b17 := byID["B17"]; len(b17.Vanished) == 0 {
		t.Errorf("B17.Vanished = %v, want the deleted test named", b17.Vanished)
	} else if b17.Vanished[0] != "TestClassifyAndCount/records_even_when_error_is_nil" {
		t.Errorf("B17.Vanished = %v, want [TestClassifyAndCount/records_even_when_error_is_nil]", b17.Vanished)
	}
	if byID["B17"].SourceChanged {
		t.Error("B17.SourceChanged = true, want false: the cheat strategy must never touch ledger.go")
	}

	// --- Totals and per-class breakdown (requirement 4) ---
	if result.Totals.Selected != 5 || result.Totals.Repaired != 3 {
		t.Errorf("Totals = %+v, want Selected=5 Repaired=3", result.Totals)
	}
	if len(result.ClassBreakdown) == 0 {
		t.Error("ClassBreakdown is empty, want one entry per bug class")
	}
	// "error-swallowed" appears twice (B15 repaired, B16 not), which is
	// exactly the nuance a bare aggregate would hide.
	var errorSwallowed *ClassSummary
	for i := range result.ClassBreakdown {
		if result.ClassBreakdown[i].Class == "error-swallowed" {
			errorSwallowed = &result.ClassBreakdown[i]
		}
	}
	if errorSwallowed == nil {
		t.Fatal("ClassBreakdown has no error-swallowed entry")
	}
	if errorSwallowed.Selected != 2 || errorSwallowed.Repaired != 1 || errorSwallowed.NotRepaired != 1 {
		t.Errorf("error-swallowed class summary = %+v, want {Selected:2 Repaired:1 NotRepaired:1}", *errorSwallowed)
	}

	// --- Both machine and human output, pasted into the acceptance report ---
	jsonBytes, err := result.MarshalIndentJSON()
	if err != nil {
		t.Fatalf("MarshalIndentJSON() error = %v", err)
	}
	t.Logf("--- JSON result ---\n%s", jsonBytes)
	t.Logf("--- human summary ---\n%s", result.HumanSummary())

	// FIXRATE_ACCEPTANCE_OUT is a debug-only escape hatch, set by a human
	// running this test locally to paste the result into an acceptance
	// report — never by belay's own code, CI config, or anything an
	// agent's prompt could influence.
	if dir := os.Getenv("FIXRATE_ACCEPTANCE_OUT"); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // dir is an operator-supplied debug output path, not attacker-controlled input
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "result.json"), jsonBytes, 0o600); err != nil { //nolint:gosec // dir is an operator-supplied debug output path, not attacker-controlled input
			t.Fatalf("WriteFile(result.json) error = %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "summary.txt"), []byte(result.HumanSummary()), 0o600); err != nil { //nolint:gosec // dir is an operator-supplied debug output path, not attacker-controlled input
			t.Fatalf("WriteFile(summary.txt) error = %v", err)
		}
	}
}

// TestRun_Determinism_SameSeedTwice is T42 acceptance check #3: the same
// seed and defect set must produce an identical result. It uses --random
// rather than the demo's explicit --bugs list specifically to prove
// determinism holds through the seeded-permutation path too, and
// AllRepair so the plan needs no foreknowledge of which files a given
// seed selects.
func TestRun_Determinism_SameSeedTwice(t *testing.T) {
	fixtureDir, err := DefaultFixtureDir()
	if err != nil {
		t.Fatalf("DefaultFixtureDir() error = %v", err)
	}
	pristineSrc := filepath.Join(fixtureDir, "src")
	// Seed 6 with count 3 is pinned deliberately (rather than derived from
	// e.g. today's date) because it is known, empirically, to select a
	// composable trio (B01, B05, B25 at time of writing) with no panic-
	// causing defect (B12/B13/B14) and no FlakyDetection concurrency
	// defect (B20-B23) — see fixtures/seeded-bug/README.md's "On composing
	// a panic-causing defect with others" and "flaky by nature" sections.
	// This test is about proving Run's OWN determinism, not about
	// re-litigating the fixture's own documented composition caveats, so
	// it deliberately avoids seeds that would exercise them.
	sel := Selection{Random: 3, Seed: 6}

	run := func() *Result {
		t.Helper()
		// The agent needs to know which files a --random selection
		// picked before it can build a repair plan for them; inject once
		// ourselves (into a throwaway directory) purely to learn the
		// file set, then let Run perform the real, measured injection.
		probe, err := injectDefects(t.Context(), fixtureDir, filepath.Join(t.TempDir(), "probe"), sel)
		if err != nil {
			t.Fatalf("injectDefects() (probe) error = %v", err)
		}
		agent := NewScriptedAgent(pristineSrc, AllRepair(defectFiles(probe.Defects)))
		result, err := Run(t.Context(), RunOptions{
			Mode: ModeReplay, FixtureDir: fixtureDir, Selection: sel, Agent: agent, Now: fixedClock(t),
		})
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		return result
	}

	first := run()
	second := run()

	if len(first.Defects) == 0 {
		t.Fatal("first run selected no defects")
	}
	firstIDs := defectIDs(first.Defects)
	secondIDs := defectIDs(second.Defects)
	if !equalStrings(firstIDs, secondIDs) {
		t.Fatalf("same seed selected different defects across two runs: %v vs %v", firstIDs, secondIDs)
	}

	firstJSON, err := first.MarshalIndentJSON()
	if err != nil {
		t.Fatalf("MarshalIndentJSON() error = %v", err)
	}
	secondJSON, err := second.MarshalIndentJSON()
	if err != nil {
		t.Fatalf("MarshalIndentJSON() error = %v", err)
	}
	// GeneratedAt legitimately differs run to run in general use, but
	// fixedClock pins Now for both calls here, so a byte-identical
	// comparison (rather than field-by-field) is the strongest available
	// determinism check.
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("Run() with the same seed produced different results:\n--- first ---\n%s\n--- second ---\n%s", firstJSON, secondJSON)
	}
}

func defectIDs(defects []DefectOutcome) []string {
	ids := make([]string, len(defects))
	for i, d := range defects {
		ids[i] = d.ID
	}
	return ids
}

// TestRun_ReplayMode_RefusesNonZeroUsage proves the zero-cost guarantee is
// enforced, not merely documented: an Agent that (mis)reports spend in
// ModeReplay must make Run fail loudly rather than hand back a Result
// that would misrepresent free measurement as free when it was not.
func TestRun_ReplayMode_RefusesNonZeroUsage(t *testing.T) {
	t.Parallel()
	fixtureDir, err := DefaultFixtureDir()
	if err != nil {
		t.Fatalf("DefaultFixtureDir() error = %v", err)
	}

	// A FakeAgent that reports non-zero spend on every call, standing in
	// for a caller who mistakenly wired a live (or estimated-cost)
	// backend into a ModeReplay run.
	spendy := &belaytest.FakeAgent{Responses: []belay.AgentResponse{{Text: "ok", Usage: belay.Usage{USD: 0.01}}}}

	_, err = Run(t.Context(), RunOptions{
		Mode:       ModeReplay,
		FixtureDir: fixtureDir,
		Selection:  Selection{Bugs: []string{"B25"}}, // a cheap, single, non-panicking wrong-constant defect
		Agent:      spendy,
		Now:        fixedClock(t),
	})
	if !errors.Is(err, ErrReplayModeSpentMoney) {
		t.Fatalf("Run() error = %v, want it to wrap ErrReplayModeSpentMoney", err)
	}
}
