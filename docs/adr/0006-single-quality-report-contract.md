# ADR-0006: Single QualityReport Contract Shared by Deterministic and AI Reviewers

## Status
Accepted

## Context
Belay supports two review modes in the quality gate (phase 6):

1. **Deterministic review** (linters, static analysis): LocalLinterReviewer runs golangci-lint, sqlcheck, etc. Fast, reproducible, no cost.

2. **AI review**: Claude Code reviews the code via MCP, looking for logic errors, edge cases, style issues. Slower, costs tokens, but catches semantic bugs.

Also phase 7 can run AI review as a fallback if phase 6 rejected the code. Should these phases share a QualityReport type or have separate types with a translation layer?

Two approaches:

1. **Single shared QualityReport**: Gate, Counts (total issues, severity distribution), Issues (list of {file, line, message}), Raw (opaque adapter-specific detail). Both reviewers return the same type; the graph treats them identically.

2. **Separate types per reviewer**: LocalLintReport has {violations, count_by_severity}. AIReviewReport has {findings, reasoning, fix_suggestions}. A translation layer converts AIReviewReport → QualityReport so the graph sees a unified interface.

Single type means no reviewer-specific branching in the graph. Separate types offer more semantic detail but couple the graph to reviewer specifics.

## Decision
A single QualityReport type is shared by deterministic and AI reviewers. The report contains: Gate (pass/fail), Counts (total, by severity), Issues (slice of issues), and Raw (opaque JSON for adapter use). Both reviewers populate these fields; the graph reads only Gate and Counts and treats both reviews identically.

## Consequences

### Positive
- **Graph is reviewer-agnostic**: Phase 6 and phase 7 share identical logic. Swapping reviewers is a config change, not a code change.
- **Enforced by conformance suite**: A test can assert that LocalLinterReviewer and AIReviewer return compatible QualityReports, catching future drift.
- **Simpler state machine**: No branches for "if phase6 is AI" or "if phase7 is deterministic." Same decision logic everywhere.
- **Adapter detail is isolated**: Raw field is opaque JSON; adapters can store anything (reasoning, suggestions, fix code) without polluting the graph contract.

### Negative
- **Lost semantic richness**: AIReviewer might generate fix suggestions or reasoning that deterministic linters don't. Hiding this in Raw means the graph can't make smarter decisions (e.g., "auto-fix if confidence > 95%").
- **Lossy translation**: If AIReviewer returns {findings: [...], reasoning: "...", fix_suggestions: [...]}, mapping to Issues requires dropping detail into Raw, possibly losing structure.
- **Type-safety trade-off**: The Raw field is untyped JSON. A maintainer adding code to read Raw must manually decode and validate it; there's no compile-time check that the structure matches.
- **Less flexibility for future reviewers**: A third reviewer type (property-based testing, formal verification) might need different fields entirely. Single contract makes adding them awkward.

### Follow-ups
- Define QualityReport schema in `pkg/review/report.go` with clear documentation of what each field means and when it's populated.
- Create a conformance suite test in `pkg/review/conformance_test.go` that asserts both LocalLinterReviewer and AIReviewer produce valid QualityReports (same Gate, non-zero Counts, non-empty Issues for failures).
- Document the Raw field: "Adapter-specific detail. Graph ignores this; intended for CLI output, logs, or future review enhancements."

## Alternatives Considered
- **Separate types per reviewer**: LocalLintReport, AIReviewReport, FormalVerifyReport, etc. Would allow richer semantic detail. Not chosen because: (1) graph becomes reviewer-specific, (2) translation layer adds maintenance burden, (3) enforcing conformance becomes harder (must define translation + assert correctness).

## Revisit If
- A user requests auto-fix based on AI reasoning (currently impossible; would need AIReviewReport.fix_suggestions in the graph).
- A second reviewer type lands (formal verification, property-based testing) and cannot map to QualityReport sensibly.
- Conformance tests show repeated failures due to Raw field mismatches (suggests the contract is too loose).

## References
- Challenge #134 phase 6: "Quality Gate (Deterministic)"
- Challenge #134 phase 7: "Quality Gate (AI Fallback)"
- pkg/review/report.go (T46: QualityReport type)
- pkg/review/conformance_test.go (T46: suite)
