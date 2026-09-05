//go:build fixrate

package fixrate

import (
	"fmt"
	"strings"
)

// This file is T42's single most important correctness property: it
// decides whether a defect gets credited as repaired, and it is written
// so that a "fix" which makes a catalogued test go green by deleting or
// weakening that test — rather than by changing the code the test
// exercises — is caught and reported as StatusInvalidRepair, never
// StatusRepaired.
//
// # The rule, and why it needs no per-defect Find/Replace text
//
// A defect is credited as repaired only when POSITIVE evidence of a real
// fix exists: its own source file (InjectedDefect.File) has different
// bytes after the run than it had right after injection. That is checked
// alongside, not instead of, the catalogued signal itself (a test turning
// green, a lint finding going quiet) — both must hold.
//
// The two failure patterns this catches are symmetric:
//
//   - A catalogued test flips to pass, or vanishes outright (never runs
//     again — the literal effect of deleting it), while the defect's own
//     source file is untouched: something else made the check stop
//     firing, and the only other thing we saw change is a *_test.go file
//     — StatusInvalidRepair.
//   - A catalogued test flips to pass or a lint finding goes quiet AND
//     the source file changed: real, attributable evidence of a fix —
//     StatusRepaired.
//
// Requiring the source file specifically to have changed — not merely
// "something changed" — is what makes this catch a cheat that touches
// ONLY a test file, without also flagging a legitimate fix that happens
// to touch a test file too (a genuine source change is present either
// way, so the legitimate case still passes).
//
// # Why the test-tree scan is tree-wide, not defect-scoped
//
// evaluateDefect is handed a testTreeChanged bool computed once, from
// EVERY "*_test.go" file in the workspace (hash.go's anyTestFileChanged),
// not narrowed to whichever test file is guessed to "belong to" this
// defect. A narrower per-defect mapping would need to know which file
// declares which test name, which this package cannot derive without
// either importing the foreign fixture module (see doc.go) or parsing Go
// source with go/ast for a fixture-specific naming convention that need
// not hold for every repository this harness could someday point at. The
// coarser, tree-wide signal is deliberately the conservative direction:
// it can occasionally flag a legitimate repair that also touched an
// unrelated test file as suspicious, but it can never let a real cheat
// through by mis-attributing which file "counts."
type channelVerdict struct {
	status       Status
	reason       string
	stillFailing []string
	vanished     []string
}

// evaluateDefect classifies one InjectedDefect against before/after
// workspace snapshots and the harness's own post-run test/lint
// observations.
func evaluateDefect(d InjectedDefect, before, after snapshot, postTests map[string]string, lintOutput string, lintAvailable bool) DefectOutcome {
	out := DefectOutcome{
		ID:                   d.ID,
		Class:                d.Class,
		File:                 d.File,
		Symbol:               d.Symbol,
		DetectedBy:           d.DetectedBy,
		Linter:               d.Linter,
		ExpectedFailingTests: append([]string(nil), d.ExpectedFailingTests...),
		SourceChanged:        sourceChanged(before, after, d.File),
		TestTreeChanged:      anyTestFileChanged(before, after),
	}

	var verdicts []channelVerdict
	if d.detectsByTest() {
		v := evaluateTestChannel(d, out.SourceChanged, out.TestTreeChanged, postTests)
		verdicts = append(verdicts, v)
		out.StillFailing = v.stillFailing
		out.Vanished = v.vanished
	}
	if d.detectsByLint() {
		v, fires := evaluateLintChannel(d, out.SourceChanged, lintOutput, lintAvailable)
		verdicts = append(verdicts, v)
		if lintAvailable {
			b := fires
			out.LintStillFires = &b
		}
	}

	out.Status, out.Reason = combineVerdicts(verdicts)
	return out
}

// evaluateTestChannel classifies the test-detection signal for d.
func evaluateTestChannel(d InjectedDefect, srcChanged, testTreeChanged bool, post map[string]string) channelVerdict {
	var stillFailing, vanished []string
	for _, name := range d.ExpectedFailingTests {
		switch statusOf(post, name) {
		case statusFail:
			stillFailing = append(stillFailing, name)
		case statusAbsent:
			vanished = append(vanished, name)
		case statusPass:
			// nothing to record
		}
	}

	switch {
	case len(vanished) > 0 && !srcChanged:
		return channelVerdict{
			status: StatusInvalidRepair,
			reason: fmt.Sprintf("%d catalogued test(s) no longer exist in the suite (%s) and %s was never modified — this looks like a deleted test, not a repair",
				len(vanished), strings.Join(vanished, ", "), d.File),
			stillFailing: stillFailing, vanished: vanished,
		}
	case len(vanished) > 0:
		return channelVerdict{
			status: StatusNotRepaired,
			reason: fmt.Sprintf("%d catalogued test(s) no longer exist in the suite (%s); cannot confirm a repair",
				len(vanished), strings.Join(vanished, ", ")),
			stillFailing: stillFailing, vanished: vanished,
		}
	case len(stillFailing) > 0:
		return channelVerdict{
			status: StatusNotRepaired,
			reason: fmt.Sprintf("%d of %d catalogued test(s) still fail: %s",
				len(stillFailing), len(d.ExpectedFailingTests), strings.Join(stillFailing, ", ")),
			stillFailing: stillFailing,
		}
	case srcChanged:
		return channelVerdict{
			status: StatusRepaired,
			reason: fmt.Sprintf("all %d catalogued test(s) pass and %s changed", len(d.ExpectedFailingTests), d.File),
		}
	case testTreeChanged:
		return channelVerdict{
			status: StatusInvalidRepair,
			reason: fmt.Sprintf("all catalogued test(s) pass, but %s was never modified while some *_test.go file was — this looks like a weakened check, not a repair", d.File),
		}
	default:
		return channelVerdict{
			status: StatusUnverified,
			reason: "all catalogued test(s) pass, but neither the source file nor any test file changed at all; " +
				"most likely nondeterministic (see the catalogue's FlakyDetection concurrency defects), not attributable to a repair",
		}
	}
}

// evaluateLintChannel classifies the lint-detection signal for d. fires
// is meaningless (and unused by the caller) when lintAvailable is false.
func evaluateLintChannel(d InjectedDefect, srcChanged bool, lintOutput string, lintAvailable bool) (v channelVerdict, fires bool) {
	if !lintAvailable {
		return channelVerdict{
			status: StatusUnverified,
			reason: "golangci-lint was not available on PATH for post-run verification; the lint channel could not be checked",
		}, false
	}

	fires = lintStillFires(lintOutput, d)
	switch {
	case fires:
		return channelVerdict{
			status: StatusNotRepaired,
			reason: fmt.Sprintf("golangci-lint (%s) still flags %s", d.Linter, d.File),
		}, fires
	case srcChanged:
		return channelVerdict{
			status: StatusRepaired,
			reason: fmt.Sprintf("golangci-lint (%s) no longer flags %s, and the file changed", d.Linter, d.File),
		}, fires
	default:
		return channelVerdict{
			status: StatusInvalidRepair,
			reason: fmt.Sprintf("golangci-lint (%s) no longer flags %s, but the file was never modified — this looks like a suppression, not a repair", d.Linter, d.File),
		}, fires
	}
}

// verdictRank orders Status from least to most severe for
// combineVerdicts's precedence: a defect detected by both channels is
// only as good as its worst channel, and an invalid repair is a stronger,
// more specific claim than a plain not-repaired.
var verdictRank = map[Status]int{
	StatusRepaired:      0,
	StatusUnverified:    1,
	StatusNotRepaired:   2,
	StatusInvalidRepair: 3,
}

// combineVerdicts reduces one or two channel verdicts (a "both"-channel
// defect has exactly two) to the single Status and Reason DefectOutcome
// carries, taking the most severe verdict per verdictRank. With two
// verdicts, Reason concatenates both so a "both"-channel defect's report
// never hides what the OTHER channel said.
func combineVerdicts(vs []channelVerdict) (Status, string) {
	if len(vs) == 0 {
		return StatusUnverified, "defect has neither a test nor a lint detection channel recorded"
	}
	best := vs[0]
	for _, v := range vs[1:] {
		if verdictRank[v.status] > verdictRank[best.status] {
			best = v
		}
	}
	if len(vs) == 1 {
		return best.status, best.reason
	}
	reasons := make([]string, len(vs))
	for i, v := range vs {
		reasons[i] = string(v.status) + ": " + v.reason
	}
	return best.status, strings.Join(reasons, " | ")
}
