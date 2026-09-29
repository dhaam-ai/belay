package fix

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/dhaam-ai/belay/internal/nodes/code"
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

// cause is what routed the run into the fix node: a failing test suite, a
// failed quality gate, or a policy violation. It exists because the fix node
// has three entry paths and one prompt builder, and building the prompt from
// the wrong one — a stale review from before the code changed, say — is the
// most likely way to waste a whole attempt.
type cause int

const (
	// causeNone means nothing on the blackboard is actually failing.
	causeNone cause = iota
	// causeTests means the recorded test run was not green.
	causeTests
	// causeReview means the recorded quality gate verdict was GateFail.
	causeReview
	// causePolicy means the recorded policy gate verdict was GateFail: the
	// change broke a guardrail the agent can be asked to put right.
	causePolicy
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
	case causePolicy:
		return "policy_violation"
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
// A policy violation outranks both, and for the same reason rather than a
// different one. The policy gate runs between write and test, so a run whose
// gate failed never reached the tests on this pass: whatever test result the
// blackboard holds was computed against the code as it stood before the
// change the gate has just judged, and the quality gate's is older still. A
// failing policy verdict is therefore proof the run arrived here from the
// gate, and it is the only cause that can be established with certainty --
// the gate routes to fix on a repairable violation and ends the run on any
// other kind, so test and review are never reached with one outstanding.
//
// A GateError verdict is deliberately not a cause. It means the gate
// computation itself broke rather than that the code is bad (see
// belay.GateError), so there is nothing concrete for an agent to repair;
// that case belongs to a human, not to the fix loop. The policy gate never
// records GateError at all: a policy belay could not evaluate is no verdict,
// and it stops the node rather than the blackboard.
func detectCause(st state.State) cause {
	if st.Policy.Gate == belay.GateFail {
		return causePolicy
	}
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
	// Test, Review and Policy are the blackboard's failure records.
	Test   state.Test
	Review state.Review
	Policy state.Policy
	// ChangedFiles is what the run has recorded as changed: the files the
	// code node reported, and those every earlier repair reported.
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
	case causePolicy:
		writePolicySection(&b, in.Policy)
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

// writePolicySection renders the guardrails the change broke.
//
// It reads exactly like the quality-gate section, because a violation is a
// belay.Issue like any other finding and the agent has already been taught to
// act on that shape. What differs is the framing above the list: a policy
// violation is not a defect in the code, it is the change being outside what
// this repository allows, and an agent told "fix these issues" would
// reasonably try to make the checker happy. Saying where the rules come from
// is what makes the difference legible.
func writePolicySection(b *strings.Builder, p state.Policy) {
	b.WriteString("## Policy violations\n\n")
	b.WriteString("This repository declares rules in belay.yaml that a change must obey whatever its tests " +
		"say, and this change breaks the ones below.\n")
	if summary := strings.TrimSpace(p.Summary); summary != "" {
		fmt.Fprintf(b, "Gate summary: %s\n", summary)
	}
	if p.ReportPath != "" {
		fmt.Fprintf(b, "Full report: %s\n", p.ReportPath)
	}
	b.WriteString("\n")

	violations := slices.Clone(p.Violations)
	slices.SortStableFunc(violations, compareIssues)
	shown := violations
	if len(shown) > maxPromptIssues {
		shown = shown[:maxPromptIssues]
	}
	for i, v := range shown {
		rule := strings.TrimSpace(v.RuleID)
		if rule == "" {
			rule = "(no rule id)"
		}
		fmt.Fprintf(b, "%d. [%s] %s\n", i+1, rule, location(v.File, v.Line))
		if msg := strings.TrimSpace(v.Message); msg != "" {
			b.WriteString(indent(truncate(msg, maxMessageChars), "   "))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if extra := len(violations) - len(shown); extra > 0 {
		fmt.Fprintf(b, "...and %d more violations not listed here.\n\n", extra)
	}
	if len(violations) == 0 {
		b.WriteString("The gate failed without listing individual violations. Work from the summary above, " +
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

// writeInstructions renders the rules of the repair, which differ by cause.
//
// A failing test is repaired by changing code. A failed quality gate can be
// "repaired" by suppressing the finding, and a policy violation by rewriting
// the policy, so each of those has to say in as many words that this is not
// what repair means. The policy case is the sharpest of the three: the rules
// are the repository's rather than the task's, and belay judges the run
// against the rules it started with whatever belay.yaml says by the end, so
// an agent that edited the file would have spent an attempt achieving
// nothing.
//
// The list is numbered as it is written rather than in literals, because the
// cause-specific part is three items for some causes and four for others,
// and a hard-coded tail leaves a gap in the numbering for the rest.
func writeInstructions(b *strings.Builder, in promptInput) {
	b.WriteString("## What to do\n\n")

	n := 0
	item := func(text string) {
		n++
		fmt.Fprintf(b, "%d. %s\n", n, text)
	}

	switch in.Cause {
	case causePolicy:
		item("Bring the change within the rules above.")
		item("Do NOT edit belay.yaml, and do not add, widen or remove a policy rule. The rules are this " +
			"repository's, not this task's; belay judges this run against the rules it started with " +
			"whatever the file says by the end, so rewriting the gate spends an attempt and changes nothing.")
		item("If a rule genuinely cannot be satisfied for this goal, do not work around it: say so " +
			"explicitly in your summary and change nothing, so a person can decide.")
		item("Keep the existing tests passing: the suite runs as soon as the change is within the rules.")
	case causeReview:
		item("Fix the issues above, most severe first.")
		item("Fix the underlying problem, not the report. Do not silence a finding with a suppression " +
			"comment, a lint exclusion, or a config change unless the finding is genuinely wrong — and if " +
			"it is, say so explicitly in your summary.")
		item("Keep the existing tests passing: the suite runs again as soon as you finish.")
	case causeTests, causeNone:
		item("Diagnose the root cause of each failure above before changing anything.")
		item("Change the least code that makes those tests pass.")
		item("Do not delete, skip, rename, or weaken a test to make the suite green. If a test itself is " +
			"wrong, fix the test and say so explicitly in your summary.")
	}
	item("Do not touch code unrelated to what is listed above.")
	item("Finish with a short summary: what was broken, what you changed, and why. Then list the files " +
		"you changed, as below.")
	b.WriteString("\n")

	writeReportRequest(b, in.ChangedFiles)

	fmt.Fprintf(b, "This is fix attempt %d of %d. The suite runs again as soon as you finish; "+
		"after attempt %d the run stops and hands the problem to a human.\n", in.Attempt, in.GiveUp, in.GiveUp)
}

// writeReportRequest asks for the changed-files block Run reads back. It is
// the code node's block, by the code node's tag (code.ChangedFilesTag), so a
// session that was told once how to report its files is asked the same way
// again, and code.ParseChangedFilesBlock holds the reply to the same rules.
//
// The block is also the only thing Run reads: the code node's parser would
// salvage paths from a diff, and this prompt quotes the failure it wants
// repaired, which is routinely a diff. So the instruction below has to say,
// in as many words, that a file the agent does not list is recorded nowhere.
//
// It asks for the files this repair touched, not every file the run has
// changed. Run keeps the union, so a file recorded earlier stays recorded
// either way and a reply that repeats one costs nothing. Asking for the
// cumulative set, as the code node's resume prompt must because its list
// replaces the record, would give the agent more to get wrong for no gain.
//
// The sentence about files already recorded appears only when the prompt
// lists some, so it never points at a section that is not there.
func writeReportRequest(b *strings.Builder, recorded []string) {
	b.WriteString("## Files you changed\n\n")
	fmt.Fprintf(b, "After the summary, emit exactly one fenced block tagged %q, holding the repository-relative "+
		"path of every file this repair created, modified or deleted, one path per line and nothing else:\n\n",
		code.ChangedFilesTag)
	fmt.Fprintf(b, "```%s\ninternal/calc/calc.go\ninternal/calc/calc_test.go\n```\n\n", code.ChangedFilesTag)
	b.WriteString("No absolute paths, no \"..\" segments, no quotes, no bullets: a path outside the repository " +
		"fails the run. ")
	if len(recorded) > 0 {
		b.WriteString("The files under \"Files this run has already changed\" stay recorded whether or not " +
			"you list them again. ")
	}
	b.WriteString("A file you change but do not list is recorded nowhere, and nothing later in the run will " +
		"know it changed. If this repair changed no file, emit no block.\n\n")
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
