//go:build fixrate

package fixrate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is Result's JSON schema version, bumped whenever a field
// is added, removed or reinterpreted incompatibly.
const SchemaVersion = 1

// Status is the outcome the diff-against-injected.json + cheat-detector
// pipeline (evaluate.go) assigns to one defect. It is never derived from
// a single signal: see evaluateDefect.
type Status string

const (
	// StatusRepaired means every catalogued signal for this defect
	// (failing tests turned green, a flagged lint finding gone silent)
	// agrees, AND the defect's own source file changed — the positive
	// evidence a real fix leaves behind. Only StatusRepaired counts
	// toward Totals.FixRate.
	StatusRepaired Status = "repaired"

	// StatusNotRepaired means at least one catalogued signal for this
	// defect still fires: a test still fails, a lint finding still
	// appears.
	StatusNotRepaired Status = "not_repaired"

	// StatusInvalidRepair means a catalogued signal went quiet WITHOUT
	// the defect's own source file changing — the cheat-detector firing.
	// It is never counted as a repair, and is reported distinctly from
	// StatusNotRepaired because it is a different, more serious claim:
	// not "still broken" but "the check was gamed, not the code fixed."
	// See evaluate.go's doc comment for the exact rule.
	StatusInvalidRepair Status = "invalid_repair"

	// StatusUnverified means this package could not attribute the
	// outcome either way: a test result changed with no file (source or
	// test) changing at all — most plausibly one of the catalogue's
	// FlakyDetection concurrency defects, whose failure is inherently
	// probabilistic (see fixtures/seeded-bug/README.md) — or the lint
	// channel could not be checked because golangci-lint was not on
	// PATH. Not counted as a repair.
	StatusUnverified Status = "unverified"
)

// DefectOutcome is one defect's diff result: what the catalogue expected,
// what this package observed, and the Status the cheat-detector-aware
// classifier assigned.
type DefectOutcome struct {
	ID                   string   `json:"id"`
	Class                string   `json:"class"`
	File                 string   `json:"file"`
	Symbol               string   `json:"symbol"`
	DetectedBy           string   `json:"detected_by"`
	Linter               string   `json:"linter,omitempty"`
	Status               Status   `json:"status"`
	Reason               string   `json:"reason"`
	ExpectedFailingTests []string `json:"expected_failing_tests,omitempty"`
	// StillFailing lists ExpectedFailingTests entries that still report a
	// leaf "fail" after the run.
	StillFailing []string `json:"still_failing,omitempty"`
	// Vanished lists ExpectedFailingTests entries with no result at all
	// after the run — never ran, most plausibly because the test (or its
	// whole file) was deleted. See the cheat detector.
	Vanished []string `json:"vanished,omitempty"`
	// SourceChanged reports whether File's own bytes differ between right
	// after injection and the end of the run — the cheat detector's
	// central positive-evidence signal.
	SourceChanged bool `json:"source_changed"`
	// TestTreeChanged reports whether any "*_test.go" file anywhere in
	// the workspace differs between right after injection and the end of
	// the run.
	TestTreeChanged bool `json:"test_tree_changed"`
	// LintStillFires is non-nil only for a defect whose DetectedBy
	// includes "lint": true if golangci-lint still names this defect's
	// (File, Linter) pair after the run, false if it does not, nil if the
	// lint channel could not be checked (golangci-lint unavailable — see
	// LintAvailable on the enclosing Result).
	LintStillFires *bool `json:"lint_still_fires,omitempty"`
}

// ClassSummary aggregates DefectOutcomes by bug class ("off-by-one",
// "concurrency", ...), because an aggregate fix rate hides exactly the
// kind of unevenness — "belay repairs 90% of off-by-ones but 20% of
// concurrency bugs" — that makes the number useful to a reader deciding
// whether to trust belay with a given kind of bug.
type ClassSummary struct {
	Class         string  `json:"class"`
	Selected      int     `json:"selected"`
	Repaired      int     `json:"repaired"`
	NotRepaired   int     `json:"not_repaired"`
	InvalidRepair int     `json:"invalid_repair"`
	Unverified    int     `json:"unverified"`
	FixRate       float64 `json:"fix_rate"`
}

// Totals is the run-wide rollup of every DefectOutcome.
type Totals struct {
	Selected      int     `json:"selected"`
	Repaired      int     `json:"repaired"`
	NotRepaired   int     `json:"not_repaired"`
	InvalidRepair int     `json:"invalid_repair"`
	Unverified    int     `json:"unverified"`
	FixRate       float64 `json:"fix_rate"`
}

// SelectionInfo records, in the Result itself, exactly which defects were
// requested and how — the same information injected.json's own
// "selection" object carries, copied forward so a Result is
// self-describing without a reader also needing the injected.json it came
// from.
type SelectionInfo struct {
	Mode   string   `json:"mode"` // "bugs" or "random"
	Bugs   []string `json:"bugs,omitempty"`
	Random int      `json:"random,omitempty"`
	Seed   *int64   `json:"seed,omitempty"`
}

// Result is one Run call's complete, machine-readable output.
//
// Provenance and Mode are placed first among the meaningful fields
// (both here and in the JSON encoding, since Go's encoding/json preserves
// struct field order) precisely so that reading only the first few lines
// of a Result — piped to `head`, previewed in a PR comment, whatever —
// still surfaces which of "measures the graph" or "measures a real agent"
// this number is. See Mode's doc comment.
type Result struct {
	SchemaVersion int    `json:"schema_version"`
	Mode          Mode   `json:"mode"`
	Provenance    string `json:"provenance"`

	GeneratedAt time.Time     `json:"generated_at"`
	FixtureSrc  string        `json:"fixture_src"`
	Selection   SelectionInfo `json:"selection"`

	// BelayStatus is the run's terminal state.RunStatus, as a string
	// (state.RunStatus.String()) — "completed", "failed", "aborted", or
	// "paused" (which should never appear: Run disables graph.approval —
	// see belayConfig — so nothing should ever pause it; a "paused"
	// Result is itself worth investigating).
	BelayStatus string `json:"belay_status"`
	// BelaySteps is the number of graph node executions the run
	// performed.
	BelaySteps int `json:"belay_steps"`
	// BelayNote is the dispatcher's own short, human-readable account of
	// how the run ended.
	BelayNote string `json:"belay_note"`

	// UsageUSD is the run's cumulative belay.Usage.USD across every
	// AgentBackend.Invoke call. In ModeReplay this is always exactly 0 —
	// Run refuses to report a Result otherwise; see Run's doc comment.
	UsageUSD float64 `json:"usage_usd"`
	// UsageEstimated mirrors belay.Usage.Estimated: whether UsageUSD came
	// from a reported cost figure or a fallback estimate. Always false
	// for the built-in ScriptedAgent, which reports a literal, non-
	// estimated 0.
	UsageEstimated bool `json:"usage_estimated"`

	// LintAvailable reports whether golangci-lint was found on PATH for
	// this package's own POST-run verification. When false, every
	// lint/both-channel defect's LintStillFires is nil and its Status is
	// StatusUnverified for that channel.
	LintAvailable bool `json:"lint_available"`

	Totals         Totals          `json:"totals"`
	ClassBreakdown []ClassSummary  `json:"class_breakdown"`
	Defects        []DefectOutcome `json:"defects"`

	// Warnings lists conditions worth a human's attention that did not
	// rise to a hard error — for example golangci-lint being unavailable
	// for post-run verification.
	Warnings []string `json:"warnings,omitempty"`
}

// MarshalIndentJSON renders r as indented JSON, matching the style
// fixtures/seeded-bug/inject.go uses for injected.json (2-space indent,
// HTML escaping off — this is a report a human may read directly, not
// HTML output).
func (r *Result) MarshalIndentJSON() ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// HumanSummary renders r as a short, human-readable report: the
// mandatory provenance line first, then totals, the per-class breakdown,
// and finally every non-repaired defect (a run that repaired everything
// prints no per-defect detail beyond the totals — there is nothing more
// useful to say about it).
func (r *Result) HumanSummary() string {
	var b strings.Builder

	fmt.Fprintln(&b, r.Provenance)
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "fix rate: %d/%d (%.0f%%)  [not_repaired=%d invalid_repair=%d unverified=%d]\n",
		r.Totals.Repaired, r.Totals.Selected, r.Totals.FixRate*100,
		r.Totals.NotRepaired, r.Totals.InvalidRepair, r.Totals.Unverified)
	fmt.Fprintf(&b, "belay run: %s in %d step(s) — %s\n", r.BelayStatus, r.BelaySteps, r.BelayNote)
	fmt.Fprintf(&b, "usage: $%.4f (estimated=%v)\n", r.UsageUSD, r.UsageEstimated)
	if !r.LintAvailable {
		fmt.Fprintln(&b, "note: golangci-lint was not available for post-run verification; lint-channel defects are unverified")
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "by class:")
	for _, c := range r.ClassBreakdown {
		fmt.Fprintf(&b, "  %-40s %d/%d (%.0f%%)  [not_repaired=%d invalid_repair=%d unverified=%d]\n",
			c.Class, c.Repaired, c.Selected, c.FixRate*100, c.NotRepaired, c.InvalidRepair, c.Unverified)
	}

	nonRepaired := make([]DefectOutcome, 0, len(r.Defects))
	for _, d := range r.Defects {
		if d.Status != StatusRepaired {
			nonRepaired = append(nonRepaired, d)
		}
	}
	if len(nonRepaired) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "not repaired / invalid / unverified:")
		for _, d := range nonRepaired {
			fmt.Fprintf(&b, "  %-4s [%-10s] %-40s (%s, %s): %s\n", d.ID, d.Status, d.Class, d.File, d.DetectedBy, d.Reason)
		}
	}

	for _, w := range r.Warnings {
		fmt.Fprintln(&b, "warning:", w)
	}

	return b.String()
}

// summarize computes Totals and ClassBreakdown from defects, sorted by
// class name for deterministic output.
func summarize(defects []DefectOutcome) (Totals, []ClassSummary) {
	var totals Totals
	byClass := make(map[string]*ClassSummary)

	tally := func(c *ClassSummary, s Status) {
		switch s {
		case StatusRepaired:
			c.Repaired++
		case StatusNotRepaired:
			c.NotRepaired++
		case StatusInvalidRepair:
			c.InvalidRepair++
		case StatusUnverified:
			c.Unverified++
		}
	}

	for _, d := range defects {
		totals.Selected++
		switch d.Status {
		case StatusRepaired:
			totals.Repaired++
		case StatusNotRepaired:
			totals.NotRepaired++
		case StatusInvalidRepair:
			totals.InvalidRepair++
		case StatusUnverified:
			totals.Unverified++
		}

		c, ok := byClass[d.Class]
		if !ok {
			c = &ClassSummary{Class: d.Class}
			byClass[d.Class] = c
		}
		c.Selected++
		tally(c, d.Status)
	}
	if totals.Selected > 0 {
		totals.FixRate = float64(totals.Repaired) / float64(totals.Selected)
	}

	classes := make([]string, 0, len(byClass))
	for c := range byClass {
		classes = append(classes, c)
	}
	sort.Strings(classes)

	breakdown := make([]ClassSummary, 0, len(classes))
	for _, name := range classes {
		c := byClass[name]
		if c.Selected > 0 {
			c.FixRate = float64(c.Repaired) / float64(c.Selected)
		}
		breakdown = append(breakdown, *c)
	}
	return totals, breakdown
}
