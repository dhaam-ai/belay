//go:build e2e

package e2e

import (
	"fmt"
	"strings"

	"github.com/belay-dev/belay/pkg/belay"
)

// loopChangedFilesBlock renders the fenced "changed-files" block the code
// and fix prompts ask for (internal/nodes/code's changedFilesTag), one
// repository-relative path per line — the exact shape a real coding agent
// is instructed to reply with.
func loopChangedFilesBlock(paths ...string) string {
	var b strings.Builder
	b.WriteString("```changed-files\n")
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString("```\n")
	return b.String()
}

// loopPlanResponse is what a real coding agent's plan-node turn looks like:
// a complete Markdown document as Text — belay's plan node archives it
// verbatim as plan.md — with a small, non-zero Usage so the run's budget
// ledger has something real to accumulate.
func loopPlanResponse() belay.AgentResponse {
	return belay.AgentResponse{
		Text: "## Summary\n" +
			"Add an Add helper function to src/app.go.\n\n" +
			"## Context\n" +
			"src/app.go is the package's only source file today.\n\n" +
			"## Steps\n" +
			"1. Add an `Add(a, b int) int` function to src/app.go.\n\n" +
			"## Tests\n" +
			"Cover positive, negative and zero operands in src/app_test.go.\n\n" +
			"## Risks\n" +
			"None: this is a small, additive change.\n\n" +
			"## Out of scope\n" +
			"No other file changes.\n",
		Turns: 3,
		Usage: belay.Usage{InputTokens: 800, OutputTokens: 220, USD: 0.02},
	}
}

// loopCodeResponse is what a real coding agent's code-node turn looks like:
// a short prose summary followed by the changed-files block the write node
// verifies against the real filesystem. files should already exist in the
// workspace (see loopHarness.seedFile) so that verification finds them
// present, exactly as it would for a change a real agent actually made.
func loopCodeResponse(sessionID string, files ...string) belay.AgentResponse {
	return belay.AgentResponse{
		Text:      "Implemented the plan by adding the Add helper.\n\n" + loopChangedFilesBlock(files...),
		SessionID: sessionID,
		Turns:     4,
		Usage:     belay.Usage{InputTokens: 1500, OutputTokens: 400, USD: 0.05},
	}
}

// loopFixResponse is what a real coding agent's fix-node turn looks like.
// The fix node never parses its response for a changed-files block — it
// only archives the exchange as evidence — so this carries prose alone, the
// same way a real reply that made no further reported change still would.
func loopFixResponse(sessionID string, attempt int) belay.AgentResponse {
	return belay.AgentResponse{
		Text:      fmt.Sprintf("Fix attempt %d: adjusted the implementation to address the reported failure.", attempt),
		SessionID: sessionID,
		Turns:     2,
		Usage:     belay.Usage{InputTokens: 900, OutputTokens: 260, USD: 0.03},
	}
}

// loopPassingTestReport is a clean, real-looking belay.TestReport: total
// tests run, all passing, nothing failing.
func loopPassingTestReport(total int) belay.TestReport {
	return belay.TestReport{Total: total, Passed: total, Failed: 0}
}

// loopFailingTestReport is a belay.TestReport reporting failed of total
// tests failing, with one concrete belay.TestFailure a fix prompt can act
// on — the shape a real test runner produces for a real regression, not an
// empty or zero-value report.
func loopFailingTestReport(total, failed int, name, message string) belay.TestReport {
	return belay.TestReport{
		Total: total, Passed: total - failed, Failed: failed,
		Failures: []belay.TestFailure{{Name: name, File: "src/app_test.go", Line: 12, Message: message}},
	}
}

// loopPassingQualityReport is a clean belay.QualityReport: Gate is always
// set explicitly to belay.GatePass, never left at its zero value —
// belay.QualityReport's own contract requires a conforming Linter or
// Reviewer to always resolve to Pass, Fail or Error, and review.Node's
// resolveGate treats an unset Gate as GateError, not GatePass.
func loopPassingQualityReport(source string) belay.QualityReport {
	return belay.QualityReport{
		Source: source, Gate: belay.GatePass,
		Issues: []belay.Issue{}, Summary: "no issues found",
	}
}

// loopFailingQualityReport is a belay.QualityReport reporting one blocker
// issue, real enough to fail a "fail_on: major" gate and to give a fix
// prompt something concrete to act on.
func loopFailingQualityReport(source string) belay.QualityReport {
	return belay.QualityReport{
		Source: source, Gate: belay.GateFail,
		Counts: belay.Counts{Blocker: 1},
		Issues: []belay.Issue{{
			RuleID: "SEC001", Severity: belay.SeverityBlocker,
			File: "src/app.go", Line: 1, Message: "hardcoded credential",
		}},
		Summary: "1 blocker issue",
	}
}
