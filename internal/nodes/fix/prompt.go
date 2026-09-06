package fix

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Caps on how much failure detail reaches the agent.
//
// A prompt is not free: every failure and every issue costs input tokens on
// every remaining attempt, and a suite that breaks a shared helper can fail
// hundreds of tests with one root cause. Listing the first few in full is
// worth more than listing all of them in fragments, so the prompt is capped
// and says plainly how much it left out.
const (
	maxPromptFailures = 10
	maxPromptIssues   = 20
	maxPromptFiles    = 25
	maxMessageChars   = 1500
)

// cause is what routed the run into the fix node: a failing test suite or a
// failed quality gate. It exists because the fix node has two entry paths
// and one prompt builder, and building the prompt from the wrong one — a
// stale review from before the code changed, say — is the most likely way
// to waste a whole attempt.
type cause int

const (
	// causeNone means nothing on the blackboard is actually failing.
	causeNone cause = iota
	// causeTests means the recorded test run was not green.
	causeTests
	// causeReview means the recorded quality gate verdict was GateFail.
	causeReview
)

// String returns a short, stable, lowercase label suitable for a log field.
func (c cause) String() string {
	switch c {
	case causeNone:
		return "none"
	case causeTests:
		return "failing_tests"
	case causeReview:
		return "failed_review_gate"
	default:
		return fmt.Sprintf("cause(%d)", int(c))
	}
}

// detectCause decides which failure the prompt should be built from.
//
// Failing tests take precedence over a failed review gate, deliberately.
// The graph runs test before review, so when both look failing the review
// on the blackboard was computed against code whose tests were already red
// — it is at best stale and at worst about code the test fix is going to
// change anyway. Correctness first, then quality: repairing behaviour that
// does not work is never wasted, whereas polishing it might be.
//
// A GateError verdict is deliberately not a cause. It means the gate
// computation itself broke rather than that the code is bad (see
// belay.GateError), so there is nothing concrete for an agent to repair;
// that case belongs to a human, not to the fix loop.
func detectCause(st state.State) cause {
	if testsFailing(st.Test) {
		return causeTests
	}
	if st.Review.Gate == belay.GateFail {
		return causeReview
	}
	return causeNone
}

// testsFailing reports whether a test run has been recorded and was not
// green. It asks belay.TestReport.OK rather than reimplementing the rule,
// so "a suite that discovered zero tests is not passing" stays defined in
// exactly one place.
func testsFailing(t state.Test) bool {
	ran := t.ReportPath != "" || t.Total > 0 || t.Failed > 0 || len(t.Failures) > 0
	return ran && !t.Report().OK()
}

// promptInput is everything the fix prompt is built from — a plain value so
// the prompt, which is this node's real product, is testable without a
// RunContext, a filesystem or an agent.
type promptInput struct {
	// Goal is the run's objective.
	Goal string
	// Cause is which failure the prompt describes.
	Cause cause
	// Attempt is this fix attempt's 1-based number, and GiveUp the cap it
	// counts towards.
	Attempt int
	GiveUp  int
	// Test and Review are the blackboard's failure records.
	Test   state.Test
	Review state.Review
	// ChangedFiles is what the code node reported touching.
	ChangedFiles []string
}

// buildPrompt renders the repair instruction sent to the agent.
//
// It is deterministic: the same promptInput always renders byte-identical
// output, which is what makes a cassette replay of a fix loop reproducible.
func buildPrompt(in promptInput) string {
	var b strings.Builder

	b.WriteString("You are belay's fix step. A change was made to this repository to reach the goal ")
	b.WriteString("below, and it does not pass. Repair it.\n\n")

	if goal := strings.TrimSpace(in.Goal); goal != "" {
		fmt.Fprintf(&b, "## Goal\n\n%s\n\n", goal)
	}

	switch in.Cause {
	case causeTests:
		writeTestSection(&b, in.Test)
	case causeReview:
		writeReviewSection(&b, in.Review)
	case causeNone:
		// Unreachable: Run refuses to prompt without a cause, precisely so
		// that no agent is ever asked to fix nothing in particular.
	}

	writeChangedFiles(&b, in.ChangedFiles)
	writeInstructions(&b, in)

	return b.String()
}

// writeTestSection renders concrete failing tests: name, file, line and the
// runner's own message, which is the difference between an agent that can
// go straight to the broken line and one that has to re-run the suite to
// find out what this node already knows.
func writeTestSection(b *strings.Builder, t state.Test) {
	b.WriteString("## Failing tests\n\n")

	if t.Total == 0 && t.Failed == 0 {
		b.WriteString("The test suite ran but discovered no tests at all. That is a broken test setup, " +
			"not a green run: a missing or misnamed test file, a package that no longer compiles, or a " +
			"runner pointed at the wrong directory. Find out which and fix it.\n\n")
	} else {
		fmt.Fprintf(b, "%d of %d tests failed (%d passed).\n\n", t.Failed, t.Total, t.Passed)
	}

	if t.ReportPath != "" {
		fmt.Fprintf(b, "Full report: %s\n\n", t.ReportPath)
	}

	shown := t.Failures
	if len(shown) > maxPromptFailures {
		shown = shown[:maxPromptFailures]
	}
	for i, f := range shown {
		name := strings.TrimSpace(f.Name)
		if name == "" {
			name = "(unnamed test)"
		}
		fmt.Fprintf(b, "%d. %s\n", i+1, name)
		if loc := location(f.File, f.Line); loc != "" {
			fmt.Fprintf(b, "   %s\n", loc)
		}
		if msg := strings.TrimSpace(f.Message); msg != "" {
			b.WriteString(indent(truncate(msg, maxMessageChars), "   "))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if extra := len(t.Failures) - len(shown); extra > 0 {
		fmt.Fprintf(b, "...and %d more failing tests not listed here. Fix the ones above first; "+
			"they are frequently the same root cause.\n\n", extra)
	}
	if len(t.Failures) == 0 && t.Failed > 0 {
		fmt.Fprintf(b, "The runner reported %d failures but could not attribute them to individual "+
			"tests. Run the suite yourself to see its output.\n\n", t.Failed)
	}
}

// writeReviewSection renders the concrete gate issues, most severe first,
// so that a truncated list still contains the ones worth an attempt.
func writeReviewSection(b *strings.Builder, r state.Review) {
	b.WriteString("## Failed quality gate\n\n")

	source := strings.TrimSpace(r.Source)
	if source == "" {
		source = "The reviewer"
	}
	line := source + " failed the quality gate"
	if n := issueCount(r); n > 0 {
		line += fmt.Sprintf(" with %d issues", n)
		if bySeverity := countsSummary(r.Counts); bySeverity != "" {
			line += ": " + bySeverity
		}
	}
	fmt.Fprintf(b, "%s.\n", line)
	if summary := strings.TrimSpace(r.Summary); summary != "" {
		fmt.Fprintf(b, "Reviewer summary: %s\n", summary)
	}
	b.WriteString("\n")

	issues := slices.Clone(r.Issues)
	slices.SortStableFunc(issues, compareIssues)
	shown := issues
	if len(shown) > maxPromptIssues {
		shown = shown[:maxPromptIssues]
	}
	for i, is := range shown {
		rule := strings.TrimSpace(is.RuleID)
		if rule == "" {
			rule = "(no rule id)"
		}
		fmt.Fprintf(b, "%d. [%s] %s\n", i+1, is.Severity, rule)
		if loc := location(is.File, is.Line); loc != "" {
			fmt.Fprintf(b, "   %s\n", loc)
		}
		if msg := strings.TrimSpace(is.Message); msg != "" {
			b.WriteString(indent(truncate(msg, maxMessageChars), "   "))
			b.WriteString("\n")
		}
		if effort := strings.TrimSpace(is.Effort); effort != "" {
			fmt.Fprintf(b, "   estimated effort: %s\n", effort)
		}
		b.WriteString("\n")
	}

	if extra := len(issues) - len(shown); extra > 0 {
		fmt.Fprintf(b, "...and %d more issues not listed here.\n\n", extra)
	}
	if len(issues) == 0 {
		b.WriteString("The gate failed without listing individual issues. Work from the summary above, " +
			"and if it is not actionable, say so rather than guessing at changes.\n\n")
	}
}

// writeChangedFiles reminds the agent which files this run already touched.
// It matters most when the session could not be resumed: without it a cold
// agent has the failures but no idea which of its own edits caused them.
func writeChangedFiles(b *strings.Builder, files []string) {
	if len(files) == 0 {
		return
	}
	b.WriteString("## Files this run has already changed\n\n")
	shown := files
	if len(shown) > maxPromptFiles {
		shown = shown[:maxPromptFiles]
	}
	for _, f := range shown {
		fmt.Fprintf(b, "- %s\n", f)
	}
	if extra := len(files) - len(shown); extra > 0 {
		fmt.Fprintf(b, "- ...and %d more\n", extra)
	}
	b.WriteString("\n")
}

// writeInstructions renders the rules of the repair, which differ by cause:
// a failing test is repaired by changing code, whereas a failed gate can be
// "repaired" by suppressing the rule, and the prompt has to say that is not
// what repair means here.
func writeInstructions(b *strings.Builder, in promptInput) {
	b.WriteString("## What to do\n\n")

	if in.Cause == causeReview {
		b.WriteString("1. Fix the issues above, most severe first.\n")
		b.WriteString("2. Fix the underlying problem, not the report. Do not silence a finding with a " +
			"suppression comment, a lint exclusion, or a config change unless the finding is genuinely " +
			"wrong — and if it is, say so explicitly in your summary.\n")
		b.WriteString("3. Keep the existing tests passing: the suite runs again as soon as you finish.\n")
	} else {
		b.WriteString("1. Diagnose the root cause of each failure above before changing anything.\n")
		b.WriteString("2. Change the least code that makes those tests pass.\n")
		b.WriteString("3. Do not delete, skip, rename, or weaken a test to make the suite green. If a " +
			"test itself is wrong, fix the test and say so explicitly in your summary.\n")
	}
	b.WriteString("4. Do not touch code unrelated to what is listed above.\n")
	b.WriteString("5. Finish with a short summary: what was broken, what you changed, and why.\n\n")

	fmt.Fprintf(b, "This is fix attempt %d of %d. The suite runs again as soon as you finish; "+
		"after attempt %d the run stops and hands the problem to a human.\n", in.Attempt, in.GiveUp, in.GiveUp)
}

// compareIssues orders issues most severe first, then by file, line and
// rule, so the ordering is total and the rendered prompt is deterministic
// regardless of the order a reviewer happened to report issues in.
func compareIssues(x, y belay.Issue) int {
	if c := cmp.Compare(y.Severity, x.Severity); c != 0 {
		return c
	}
	if c := cmp.Compare(x.File, y.File); c != 0 {
		return c
	}
	if c := cmp.Compare(x.Line, y.Line); c != 0 {
		return c
	}
	return cmp.Compare(x.RuleID, y.RuleID)
}

// countsSummary renders the non-zero severity counts, most severe first,
// as "1 blocker, 3 major".
func countsSummary(c belay.Counts) string {
	pairs := []struct {
		n    int
		name string
	}{
		{c.Blocker, "blocker"},
		{c.Critical, "critical"},
		{c.Major, "major"},
		{c.Minor, "minor"},
		{c.Info, "info"},
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.name))
		}
	}
	return strings.Join(parts, ", ")
}

// issueCount reports how many issues a review found, preferring Counts and
// falling back to the issue list. A reviewer that populated Issues but not
// Counts must not make the prompt claim the gate failed over nothing.
func issueCount(r state.Review) int {
	if n := r.Counts.Total(); n > 0 {
		return n
	}
	return len(r.Issues)
}

// location renders a "file:line" reference from whichever half is known.
func location(file string, line int) string {
	file = strings.TrimSpace(file)
	switch {
	case file != "" && line > 0:
		return fmt.Sprintf("%s:%d", file, line)
	case file != "":
		return file
	case line > 0:
		return fmt.Sprintf("line %d", line)
	default:
		return ""
	}
}

// truncate cuts s to at most maxChars bytes on a rune boundary, marking
// that it did so. A single panic trace can be megabytes; sending all of it
// costs tokens on every remaining attempt and buries the assertion that
// actually explains the failure.
func truncate(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	cut := s[:maxChars]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n... (message truncated)"
}

// indent prefixes every non-blank line of s, so a multi-line failure
// message stays visibly attached to the failure it belongs to.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[i] = ""
			continue
		}
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
