//go:build fixrate

package fixrate

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// pairedFixture is a deterministic ten-task comparison: 3 tasks both arms
// resolved, 5 only belay resolved, 0 only the control arm resolved, and 2
// neither resolved. Spend is arranged so belay costs more in total ($5.00
// against $3.00) but less per resolved task ($0.625 against $1.00) — the
// case the report has to get right, because it is the case a
// total-spend-only report reads backwards.
//
// Every dollar figure here is an exact binary fraction (halves, quarters),
// and the totals divide exactly by each arm's resolved count. That is
// deliberate: the assertions below compare formatted JSON, so a fixture
// built from values like 0.4 would have them turn on IEEE rounding of an
// accumulated sum rather than on this package's arithmetic.
func pairedFixture() []PairedTask {
	pair := func(id string, belayOK, controlOK bool, belayUSD, controlUSD float64) PairedTask { //nolint:unparam // belayUSD is uniform across this fixture by design; naming it keeps the table readable
		return PairedTask{
			TaskID: id,
			Belay: TaskOutcome{
				TaskID: id, Arm: ArmBelay, Resolved: belayOK,
				UsageUSD: belayUSD, Turns: 9, WallClockSeconds: 30,
			},
			Control: TaskOutcome{
				TaskID: id, Arm: ArmControl, Resolved: controlOK,
				UsageUSD: controlUSD, Turns: 4, WallClockSeconds: 10,
			},
		}
	}
	return []PairedTask{
		pair("t01", true, true, 0.5, 0.25),
		pair("t02", true, true, 0.5, 0.25),
		pair("t03", true, true, 0.5, 0.25),
		pair("t04", true, false, 0.5, 0.25),
		pair("t05", true, false, 0.5, 0.25),
		pair("t06", true, false, 0.5, 0.25),
		pair("t07", true, false, 0.5, 0.25),
		pair("t08", true, false, 0.5, 0.25),
		pair("t09", false, false, 0.5, 0.25),
		pair("t10", false, false, 0.5, 0.75),
	}
}

func TestSummarizePaired_Rates(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := summarizePaired(ModeReplay, at, pairedFixture())
	if err != nil {
		t.Fatalf("summarizePaired() error = %v", err)
	}

	if got.SchemaVersion != PairedSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, PairedSchemaVersion)
	}
	if got.Mode != ModeReplay {
		t.Errorf("Mode = %q, want %q", got.Mode, ModeReplay)
	}
	if got.Provenance != ModeReplay.provenance() {
		t.Errorf("Provenance = %q, want the mode's own disclosure", got.Provenance)
	}
	if !got.GeneratedAt.Equal(at) {
		t.Errorf("GeneratedAt = %v, want %v", got.GeneratedAt, at)
	}

	arms := []struct {
		name        string
		got         ArmSummary
		wantArm     Arm
		wantTasks   int
		wantResolve int
		wantRate    float64
		wantLo      float64
		wantHi      float64
		wantUSD     float64
		wantPerFix  float64
		wantWall    float64
	}{
		{"belay", got.Belay, ArmBelay, 10, 8, 0.8, 0.490162471537, 0.943317848546, 5.0, 0.625, 300},
		{"control", got.Control, ArmControl, 10, 3, 0.3, 0.107791267406, 0.603221852539, 3.0, 1.0, 100},
	}
	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			switch {
			case a.got.Arm != a.wantArm:
				t.Errorf("Arm = %q, want %q", a.got.Arm, a.wantArm)
			case a.got.Provenance != a.wantArm.provenance():
				t.Errorf("Provenance = %q, want the arm's own disclosure", a.got.Provenance)
			case a.got.Tasks != a.wantTasks:
				t.Errorf("Tasks = %d, want %d", a.got.Tasks, a.wantTasks)
			case a.got.Resolved != a.wantResolve:
				t.Errorf("Resolved = %d, want %d", a.got.Resolved, a.wantResolve)
			}
			if math.Abs(a.got.ResolveRate-a.wantRate) > tol {
				t.Errorf("ResolveRate = %v, want %v", a.got.ResolveRate, a.wantRate)
			}
			if math.Abs(a.got.ResolveRateCI.Lo-a.wantLo) > 1e-9 || math.Abs(a.got.ResolveRateCI.Hi-a.wantHi) > 1e-9 {
				t.Errorf("ResolveRateCI = [%.12f, %.12f], want [%.12f, %.12f]",
					a.got.ResolveRateCI.Lo, a.got.ResolveRateCI.Hi, a.wantLo, a.wantHi)
			}
			if math.Abs(a.got.UsageUSD-a.wantUSD) > 1e-9 {
				t.Errorf("UsageUSD = %v, want %v", a.got.UsageUSD, a.wantUSD)
			}
			if a.got.USDPerResolvedTask == nil {
				t.Fatalf("USDPerResolvedTask = nil, want %v", a.wantPerFix)
			}
			if math.Abs(*a.got.USDPerResolvedTask-a.wantPerFix) > 1e-9 {
				t.Errorf("USDPerResolvedTask = %v, want %v", *a.got.USDPerResolvedTask, a.wantPerFix)
			}
			if math.Abs(a.got.WallClockSeconds-a.wantWall) > 1e-9 {
				t.Errorf("WallClockSeconds = %v, want %v", a.got.WallClockSeconds, a.wantWall)
			}
		})
	}
}

func TestSummarizePaired_Comparison(t *testing.T) {
	t.Parallel()

	got, err := summarizePaired(ModeReplay, time.Now(), pairedFixture())
	if err != nil {
		t.Fatalf("summarizePaired() error = %v", err)
	}
	c := got.Comparison
	switch {
	case c.BothResolved != 3:
		t.Errorf("BothResolved = %d, want 3", c.BothResolved)
	case c.BelayOnly != 5:
		t.Errorf("BelayOnly = %d, want 5", c.BelayOnly)
	case c.ControlOnly != 0:
		t.Errorf("ControlOnly = %d, want 0", c.ControlOnly)
	case c.NeitherResolved != 2:
		t.Errorf("NeitherResolved = %d, want 2", c.NeitherResolved)
	}
	if math.Abs(c.ResolveRateDelta-0.5) > tol {
		t.Errorf("ResolveRateDelta = %v, want 0.5 (0.8 - 0.3)", c.ResolveRateDelta)
	}
	// b=5, c=0 -> 2 * (1/32) exactly.
	if math.Abs(c.McNemarExactP-0.0625) > 1e-12 {
		t.Errorf("McNemarExactP = %.15f, want 0.0625 for 5 discordant pairs all one way", c.McNemarExactP)
	}
	// The concordant counts must add up: every task is in exactly one cell.
	if total := c.BothResolved + c.BelayOnly + c.ControlOnly + c.NeitherResolved; total != len(got.Tasks) {
		t.Errorf("the four cells sum to %d, want %d tasks", total, len(got.Tasks))
	}
}

func TestSummarizePaired_NothingResolvedHasNoCostPerFix(t *testing.T) {
	t.Parallel()

	pairs := []PairedTask{{
		TaskID:  "t01",
		Belay:   TaskOutcome{TaskID: "t01", Arm: ArmBelay, Resolved: true, UsageUSD: 2},
		Control: TaskOutcome{TaskID: "t01", Arm: ArmControl, Resolved: false, UsageUSD: 1.25},
	}}
	got, err := summarizePaired(ModeReplay, time.Now(), pairs)
	if err != nil {
		t.Fatalf("summarizePaired() error = %v", err)
	}
	if got.Control.USDPerResolvedTask != nil {
		t.Errorf("Control.USDPerResolvedTask = %v, want nil: an arm that spent $1.25 and resolved nothing "+
			"has no cost per resolved task to quote", *got.Control.USDPerResolvedTask)
	}
	if got.Control.UsageUSD != 1.25 {
		t.Errorf("Control.UsageUSD = %v, want 1.25 — the spend still happened", got.Control.UsageUSD)
	}
	if got.Belay.USDPerResolvedTask == nil || *got.Belay.USDPerResolvedTask != 2 {
		t.Errorf("Belay.USDPerResolvedTask = %v, want 2", got.Belay.USDPerResolvedTask)
	}
}

func TestSummarizePaired_Refusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mode    Mode
		pairs   []PairedTask
		wantErr error
	}{
		{
			name:    "unknown mode",
			mode:    "bogus",
			pairs:   pairedFixture(),
			wantErr: ErrUnknownMode,
		},
		{
			name: "arms name different tasks",
			mode: ModeReplay,
			pairs: []PairedTask{{
				TaskID:  "t01",
				Belay:   TaskOutcome{TaskID: "t01", Arm: ArmBelay},
				Control: TaskOutcome{TaskID: "t99", Arm: ArmControl},
			}},
			wantErr: ErrUnpairedTask,
		},
		{
			name: "an outcome is labelled with the wrong arm",
			mode: ModeReplay,
			pairs: []PairedTask{{
				TaskID:  "t01",
				Belay:   TaskOutcome{TaskID: "t01", Arm: ArmControl},
				Control: TaskOutcome{TaskID: "t01", Arm: ArmControl},
			}},
			wantErr: ErrUnpairedTask,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := summarizePaired(tt.mode, time.Now(), tt.pairs)
			if err == nil {
				t.Fatal("summarizePaired() = nil error, want one")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("summarizePaired() error = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

func TestSummarizePaired_NoTasksWarnsRatherThanClaims(t *testing.T) {
	t.Parallel()

	got, err := summarizePaired(ModeReplay, time.Now(), nil)
	if err != nil {
		t.Fatalf("summarizePaired() error = %v", err)
	}
	if got.Belay.ResolveRateCI != (Interval{Lo: 0, Hi: 1}) {
		t.Errorf("Belay.ResolveRateCI = %+v, want the whole range for zero observations", got.Belay.ResolveRateCI)
	}
	if got.Comparison.McNemarExactP != 1 {
		t.Errorf("McNemarExactP = %v, want 1 with no discordant pairs", got.Comparison.McNemarExactP)
	}
	if len(got.Warnings) == 0 {
		t.Error("Warnings is empty, want one saying no tasks were compared")
	}
}

// TestPairedResult_HumanSummary_LeadsWithProvenanceAndCost pins the two
// things the report must not bury: which arm and which mode produced each
// number, and the dollar cost of one resolved task.
func TestPairedResult_HumanSummary_LeadsWithProvenanceAndCost(t *testing.T) {
	t.Parallel()

	got, err := summarizePaired(ModeReplay, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), pairedFixture())
	if err != nil {
		t.Fatalf("summarizePaired() error = %v", err)
	}
	summary := got.HumanSummary()

	first := strings.SplitN(summary, "\n", 2)[0]
	if first != ModeReplay.provenance() {
		t.Errorf("HumanSummary's first line = %q, want the mode provenance %q", first, ModeReplay.provenance())
	}
	for _, want := range []string{
		"usd per resolved task",
		"$0.6250", // belay: 5.00 / 8
		"$1.0000", // control: 3.00 / 3
		"mcnemar",
		"0.0625",
		ArmBelay.provenance(),
		ArmControl.provenance(),
	} {
		if !strings.Contains(strings.ToLower(summary), strings.ToLower(want)) {
			t.Errorf("HumanSummary() is missing %q:\n%s", want, summary)
		}
	}
}

func TestPairedResult_MarshalIndentJSON(t *testing.T) {
	t.Parallel()

	got, err := summarizePaired(ModeReplay, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), pairedFixture())
	if err != nil {
		t.Fatalf("summarizePaired() error = %v", err)
	}
	data, err := got.MarshalIndentJSON()
	if err != nil {
		t.Fatalf("MarshalIndentJSON() error = %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`"schema_version": 1`,
		`"mode": "replay"`,
		`"usd_per_resolved_task": 0.625`,
		`"mcnemar_exact_p": 0.0625`,
		`"belay_only": 5`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("MarshalIndentJSON() is missing %s:\n%s", want, text)
		}
	}
	// Provenance must be impossible to miss: it precedes every number.
	if i, j := strings.Index(text, `"provenance"`), strings.Index(text, `"resolve_rate"`); i < 0 || i > j {
		t.Errorf("provenance appears at %d and resolve_rate at %d; provenance must come first", i, j)
	}
}

func TestBelayTaskOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		totals       Totals
		wantResolved bool
	}{
		{"every defect repaired", Totals{Selected: 3, Repaired: 3}, true},
		{"one left broken", Totals{Selected: 3, Repaired: 2, NotRepaired: 1}, false},
		// An invalid repair is a gamed check, and must never make a task
		// resolve however green the suite looks.
		{"one gamed", Totals{Selected: 3, Repaired: 2, InvalidRepair: 1}, false},
		{"one unverified", Totals{Selected: 3, Repaired: 2, Unverified: 1}, false},
		// No defects selected is not a resolved task; it is an empty one.
		{"nothing selected", Totals{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &Result{
				Mode:        ModeReplay,
				Totals:      tt.totals,
				UsageUSD:    1.5,
				BelayStatus: "completed",
				BelayNote:   "all gates green",
			}
			got := belayTaskOutcome("t01", r, 42*time.Second)
			switch {
			case got.TaskID != "t01":
				t.Errorf("TaskID = %q, want %q", got.TaskID, "t01")
			case got.Arm != ArmBelay:
				t.Errorf("Arm = %q, want %q", got.Arm, ArmBelay)
			case got.Resolved != tt.wantResolved:
				t.Errorf("Resolved = %v, want %v for %+v", got.Resolved, tt.wantResolved, tt.totals)
			case got.UsageUSD != 1.5:
				t.Errorf("UsageUSD = %v, want 1.5", got.UsageUSD)
			case got.WallClockSeconds != 42:
				t.Errorf("WallClockSeconds = %v, want 42", got.WallClockSeconds)
			case got.Reason == "":
				t.Error("Reason is empty; every outcome must say why it landed where it did")
			}
		})
	}
}

func TestControlTaskOutcome(t *testing.T) {
	t.Parallel()

	res := controlRunResult{
		Usage:  belayUsageForTest(0.75, true),
		Turns:  17,
		Model:  "sonnet",
		RunErr: "backend hit its own ceiling",
	}
	got := controlTaskOutcome("t01", res, false, "2 catalogued test(s) still fail", 11*time.Second)
	switch {
	case got.Arm != ArmControl:
		t.Errorf("Arm = %q, want %q", got.Arm, ArmControl)
	case got.Resolved:
		t.Error("Resolved = true, want false")
	case got.UsageUSD != 0.75:
		t.Errorf("UsageUSD = %v, want 0.75", got.UsageUSD)
	case !got.UsageEstimated:
		t.Error("UsageEstimated = false, want the backend's own true carried through")
	case got.Turns != 17:
		t.Errorf("Turns = %d, want 17", got.Turns)
	case got.WallClockSeconds != 11:
		t.Errorf("WallClockSeconds = %v, want 11", got.WallClockSeconds)
	case got.Note != "backend hit its own ceiling":
		t.Errorf("Note = %q, want the backend's error recorded", got.Note)
	case got.Reason != "2 catalogued test(s) still fail":
		t.Errorf("Reason = %q, want the caller's verdict", got.Reason)
	}
}

// TestSummarizePaired_ReportsTheControlArmsAllowance closes the question a
// reader of a losing control arm asks first: was it starved? The allowance
// is derived (controlAllowance), so the report has to show it rather than
// leave the reader to recompute it from a configuration they do not have.
func TestSummarizePaired_ReportsTheControlArmsAllowance(t *testing.T) {
	t.Parallel()

	cfg := belayConfig()
	res := controlRunResult{Model: "sonnet", Allowance: controlAllowance(cfg)}
	pairs := []PairedTask{{
		TaskID:  "t01",
		Belay:   belayTaskOutcome("t01", &Result{Totals: Totals{Selected: 1, Repaired: 1}}, time.Second),
		Control: controlTaskOutcome("t01", res, false, "still failing", time.Second),
	}}

	got, err := summarizePaired(ModeReplay, time.Now(), pairs)
	if err != nil {
		t.Fatalf("summarizePaired() error = %v", err)
	}
	if got.Control.Allowance == nil {
		t.Fatal("Control.Allowance = nil, want the derived allowance so a reader can check the arm was not starved")
	}
	if want := controlAllowance(cfg); *got.Control.Allowance != want {
		t.Errorf("Control.Allowance = %+v, want %+v", *got.Control.Allowance, want)
	}
	// belay's ceilings are its own configuration, not a derived figure,
	// so there is nothing to audit and nothing to print.
	if got.Belay.Allowance != nil {
		t.Errorf("Belay.Allowance = %+v, want nil", *got.Belay.Allowance)
	}
	if got.Control.Model != "sonnet" {
		t.Errorf("Control.Model = %q, want %q", got.Control.Model, "sonnet")
	}

	summary := got.HumanSummary()
	for _, want := range []string{"150 turn", "1h15m0s"} {
		if !strings.Contains(summary, want) {
			t.Errorf("HumanSummary() is missing the allowance detail %q:\n%s", want, summary)
		}
	}
}
