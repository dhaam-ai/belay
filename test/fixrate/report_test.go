//go:build fixrate

package fixrate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSummarize(t *testing.T) {
	t.Parallel()
	defects := []DefectOutcome{
		{ID: "B1", Class: "off-by-one", Status: StatusRepaired},
		{ID: "B2", Class: "off-by-one", Status: StatusNotRepaired},
		{ID: "B3", Class: "inverted-condition", Status: StatusRepaired},
		{ID: "B4", Class: "inverted-condition", Status: StatusInvalidRepair},
		{ID: "B5", Class: "inverted-condition", Status: StatusUnverified},
	}
	totals, breakdown := summarize(defects)

	if totals.Selected != 5 || totals.Repaired != 2 || totals.NotRepaired != 1 || totals.InvalidRepair != 1 || totals.Unverified != 1 {
		t.Fatalf("totals = %+v, want {5 2 1 1 1 ...}", totals)
	}
	if got, want := totals.FixRate, 2.0/5.0; got != want {
		t.Errorf("totals.FixRate = %v, want %v", got, want)
	}

	if len(breakdown) != 2 {
		t.Fatalf("breakdown has %d classes, want 2: %+v", len(breakdown), breakdown)
	}
	// Sorted alphabetically: "inverted-condition" before "off-by-one".
	if breakdown[0].Class != "inverted-condition" || breakdown[1].Class != "off-by-one" {
		t.Fatalf("breakdown classes = [%s, %s], want sorted order", breakdown[0].Class, breakdown[1].Class)
	}
	ic := breakdown[0]
	if ic.Selected != 3 || ic.Repaired != 1 || ic.InvalidRepair != 1 || ic.Unverified != 1 {
		t.Errorf("inverted-condition summary = %+v, want {3 1 0 1 1 ...}", ic)
	}
}

func TestSummarize_Empty(t *testing.T) {
	t.Parallel()
	totals, breakdown := summarize(nil)
	if totals.Selected != 0 || totals.FixRate != 0 {
		t.Errorf("totals for no defects = %+v, want the zero value", totals)
	}
	if len(breakdown) != 0 {
		t.Errorf("breakdown for no defects = %v, want empty", breakdown)
	}
}

func TestResult_MarshalIndentJSON_RoundTrips(t *testing.T) {
	t.Parallel()
	r := &Result{
		SchemaVersion: SchemaVersion,
		Mode:          ModeReplay,
		Provenance:    ModeReplay.provenance(),
		Selection:     SelectionInfo{Mode: "bugs", Bugs: []string{"B01"}},
		Totals:        Totals{Selected: 1, Repaired: 1, FixRate: 1},
		Defects:       []DefectOutcome{{ID: "B01", Status: StatusRepaired}},
	}
	data, err := r.MarshalIndentJSON()
	if err != nil {
		t.Fatalf("MarshalIndentJSON() error = %v", err)
	}
	if !strings.HasPrefix(string(data), "{\n") {
		t.Errorf("MarshalIndentJSON() is not indented: %q", string(data[:20]))
	}

	var back Result
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if back.Mode != ModeReplay || back.Totals.Repaired != 1 || len(back.Defects) != 1 {
		t.Errorf("round-tripped Result = %+v, want it to match the original", back)
	}
}

func TestResult_HumanSummary_LeadsWithProvenance(t *testing.T) {
	t.Parallel()
	r := &Result{
		Mode:          ModeReplay,
		Provenance:    ModeReplay.provenance(),
		LintAvailable: true,
		Totals:        Totals{Selected: 5, Repaired: 3, NotRepaired: 1, InvalidRepair: 1, FixRate: 0.6},
		Defects: []DefectOutcome{
			{ID: "B01", Class: "off-by-one", Status: StatusRepaired},
			{ID: "B17", Class: "early-return-before-required-work", File: "ledger.go", DetectedBy: "test",
				Status: StatusInvalidRepair, Reason: "deleted test"},
		},
	}
	summary := r.HumanSummary()

	if !strings.HasPrefix(summary, r.Provenance) {
		t.Errorf("HumanSummary() does not lead with the provenance line:\n%s", summary)
	}
	if !strings.Contains(summary, "3/5") {
		t.Errorf("HumanSummary() = %q, missing the fix-rate fraction", summary)
	}
	if !strings.Contains(summary, "B17") || !strings.Contains(summary, "deleted test") {
		t.Errorf("HumanSummary() does not detail the invalid_repair defect:\n%s", summary)
	}
	if strings.Contains(summary, "B01") {
		t.Errorf("HumanSummary() lists a repaired defect in the detail section, want only non-repaired ones:\n%s", summary)
	}
}
