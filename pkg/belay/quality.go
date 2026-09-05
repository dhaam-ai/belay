package belay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownSeverity reports a string that does not name a Severity.
var ErrUnknownSeverity = errors.New("belay: unknown severity")

// ErrUnknownGateStatus reports a string that does not name a GateStatus.
var ErrUnknownGateStatus = errors.New("belay: unknown gate status")

// Severity ranks how serious a quality issue is.
//
// Severity is ordered from least to most severe:
//
//	SeverityInfo < SeverityMinor < SeverityMajor < SeverityCritical < SeverityBlocker
//
// mirroring log/slog.Level — the zero value is the mildest severity, and a
// threshold check is a single comparison. ReviewRequest.FailOn of
// SeverityMajor means "fail the gate if any issue is major, critical, or
// blocker"; that test is exactly:
//
//	issue.Severity >= req.FailOn
//
// This ordering, not just the five names, is part of the contract: it is
// what makes a fail_on: major configuration value expressible and testable
// across every Linter and Reviewer implementation.
type Severity int

const (
	// SeverityInfo is an informational note — a style nit or suggestion
	// that carries no risk. The zero value.
	SeverityInfo Severity = iota
	// SeverityMinor is a minor issue: worth fixing, low risk if shipped.
	SeverityMinor
	// SeverityMajor is a major issue: a real defect or design problem.
	SeverityMajor
	// SeverityCritical is a critical issue: high risk of a production
	// incident, such as a likely nil dereference on a hot path.
	SeverityCritical
	// SeverityBlocker must not ship — a security vulnerability, a
	// data-loss bug, or a broken build.
	SeverityBlocker
)

var severityNames = [...]string{
	SeverityInfo:     "info",
	SeverityMinor:    "minor",
	SeverityMajor:    "major",
	SeverityCritical: "critical",
	SeverityBlocker:  "blocker",
}

// String returns the canonical lowercase spelling of s, such as "major". An
// out-of-range Severity renders as Severity(n) rather than panicking.
func (s Severity) String() string {
	if !s.valid() {
		return fmt.Sprintf("Severity(%d)", int(s))
	}
	return severityNames[s]
}

func (s Severity) valid() bool { return s >= 0 && int(s) < len(severityNames) }

// ParseSeverity is the inverse of Severity.String. It accepts "info",
// "minor", "major", "critical" and "blocker", ignoring surrounding
// whitespace and letter case, and returns an error wrapping
// ErrUnknownSeverity for any other input.
func ParseSeverity(s string) (Severity, error) {
	normalized := strings.ToLower(strings.TrimSpace(s))
	for i, name := range severityNames {
		if name == normalized {
			return Severity(i), nil
		}
	}
	return 0, fmt.Errorf("%w: %q", ErrUnknownSeverity, s)
}

// MarshalText implements encoding.TextMarshaler so a Severity round-trips
// through configuration and JSON as its canonical spelling.
func (s Severity) MarshalText() ([]byte, error) {
	if !s.valid() {
		return nil, fmt.Errorf("%w: %d", ErrUnknownSeverity, int(s))
	}
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Severity) UnmarshalText(text []byte) error {
	parsed, err := ParseSeverity(string(text))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// GateStatus is the pass/fail/error outcome of a quality gate.
type GateStatus int

const (
	// GateUnknown means no gate verdict is available. It is the zero
	// value, and a conforming Linter or Reviewer never returns it as the
	// final Gate of a QualityReport it hands back successfully — every
	// completed call resolves to GatePass, GateFail or GateError. Its only
	// legitimate appearance is in a value nobody has populated yet (a
	// freshly constructed QualityReport in a test fixture, for instance).
	GateUnknown GateStatus = iota
	// GatePass means the report's issues do not violate the configured
	// threshold.
	GatePass
	// GateFail means the report's issues violate the configured threshold
	// (see ReviewRequest.FailOn) — a normal, expected outcome the "fix"
	// node acts on, not an error.
	GateFail
	// GateError means the tool ran but could not reach a verdict at all —
	// it crashed mid-analysis, timed out, or returned output the adapter
	// could not parse. GateError is distinct from GateFail because the
	// graph should retry or escalate to a human rather than hand
	// diagnostics to a fix loop that has nothing concrete to fix.
	//
	// Adapters wrapping SonarQube specifically: Sonar's own project
	// quality-gate status uses the string "ERROR" to mean the gate was
	// evaluated and FAILED — that is belay's GateFail, not GateError.
	// Mapping Sonar's "ERROR" string onto this constant by name-matching
	// alone is the single most likely bug in a Sonar-backed adapter;
	// GateError is reserved for belay's own "the gate computation itself
	// broke" case, which has no direct Sonar equivalent.
	GateError
)

var gateStatusNames = [...]string{
	GateUnknown: "unknown",
	GatePass:    "pass",
	GateFail:    "fail",
	GateError:   "error",
}

// String returns the canonical lowercase spelling of g, such as "fail". An
// out-of-range GateStatus renders as GateStatus(n) rather than panicking.
func (g GateStatus) String() string {
	if !g.valid() {
		return fmt.Sprintf("GateStatus(%d)", int(g))
	}
	return gateStatusNames[g]
}

func (g GateStatus) valid() bool { return g >= 0 && int(g) < len(gateStatusNames) }

// ParseGateStatus is the inverse of GateStatus.String. It accepts
// "unknown", "pass", "fail" and "error", ignoring surrounding whitespace
// and letter case, and returns an error wrapping ErrUnknownGateStatus for
// any other input.
func ParseGateStatus(s string) (GateStatus, error) {
	normalized := strings.ToLower(strings.TrimSpace(s))
	for i, name := range gateStatusNames {
		if name == normalized {
			return GateStatus(i), nil
		}
	}
	return GateUnknown, fmt.Errorf("%w: %q", ErrUnknownGateStatus, s)
}

// MarshalText implements encoding.TextMarshaler.
func (g GateStatus) MarshalText() ([]byte, error) {
	if !g.valid() {
		return nil, fmt.Errorf("%w: %d", ErrUnknownGateStatus, int(g))
	}
	return []byte(g.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (g *GateStatus) UnmarshalText(text []byte) error {
	parsed, err := ParseGateStatus(string(text))
	if err != nil {
		return err
	}
	*g = parsed
	return nil
}

// Counts summarizes a QualityReport's Issues by severity: it is the
// histogram of Issues[i].Severity. Every field is always present in JSON
// output (none uses omitempty) so two reports with different issue mixes
// still serialize Counts to the same key set — part of the guarantee
// documented on QualityReport.
type Counts struct {
	Blocker  int `json:"blocker"`
	Critical int `json:"critical"`
	Major    int `json:"major"`
	Minor    int `json:"minor"`
	Info     int `json:"info"`
}

// Total returns the sum of every severity count.
func (c Counts) Total() int {
	return c.Blocker + c.Critical + c.Major + c.Minor + c.Info
}

// AtOrAbove returns how many issues have severity threshold or higher under
// Severity's ordering. AtOrAbove(SeverityMajor) is exactly the count a
// `fail_on: major` gate acts on.
func (c Counts) AtOrAbove(threshold Severity) int {
	n := 0
	if SeverityBlocker >= threshold {
		n += c.Blocker
	}
	if SeverityCritical >= threshold {
		n += c.Critical
	}
	if SeverityMajor >= threshold {
		n += c.Major
	}
	if SeverityMinor >= threshold {
		n += c.Minor
	}
	if SeverityInfo >= threshold {
		n += c.Info
	}
	return n
}

// Issue is one quality problem found by a Linter or Reviewer.
//
// Like QualityReport, Issue has no omitempty tags: every field is always
// present in JSON output, so an Issue from a deterministic linter and an
// Issue from an AI reviewer serialize to the same key set.
type Issue struct {
	// RuleID identifies the rule or check that fired, in the underlying
	// tool's own namespace — for example "govet:nilness" or a SonarQube
	// rule key. Opaque to belay; never parsed.
	RuleID string `json:"rule_id"`

	// Severity is this issue's severity. See Severity for the ordering
	// that makes ReviewRequest.FailOn comparisons well-defined.
	Severity Severity `json:"severity"`

	// File is the path to the affected file, relative to the directory or
	// WorkDir the check ran against.
	File string `json:"file"`

	// Line is the 1-indexed line the issue was reported at. Zero if the
	// issue is not attributable to one line (a whole-file or whole-project
	// finding).
	Line int `json:"line"`

	// Message is the human-readable description of the issue.
	Message string `json:"message"`

	// Effort is an opaque, tool-native remediation-effort estimate, such
	// as SonarQube's "5min" or "1h30min". belay never parses it — it is
	// carried through for display only. Empty if the tool does not
	// estimate effort.
	Effort string `json:"effort"`
}

// QualityReport is the result of a Linter.Lint or Reviewer.Review call.
//
// QualityReport is the seam that makes a deterministic tool (golangci-lint,
// eslint, ruff, a sonar-scanner run with sonar.qualitygate.wait=true) and an
// AI-driven reviewer (one that calls a SonarQube MCP server's
// list_quality_gates, get_raw_source, analyze_code_snippet,
// get_project_quality_gate_status and search_sonar_issues_in_projects
// tools, say) fully interchangeable to the graph: the graph reads ONLY
// Gate, Counts and Issues, and never Raw.
//
// Every field below is always present in JSON output — none uses
// `omitempty` — specifically so two conforming adapters serialize a report
// to an identical set of JSON keys regardless of content; Source is the
// only field permitted to differ between them (and Raw's internal shape,
// which is exempt because the graph never reads it). Do not add omitempty
// to this struct or to Issue: doing so would let a report's key set depend
// on its content, and it would break first for the emptiest report — zero
// issues — which is exactly the report most likely to be compared against
// a fixture in a conformance test.
type QualityReport struct {
	// Source identifies which Linter or Reviewer produced this report,
	// equal to that adapter's Name() (or, for a Reviewer, a caller- or
	// adapter-chosen identifier such as "sonar-mcp-ai-review"). This is
	// the one field two otherwise-conformant reports are expected to
	// differ on.
	Source string `json:"source"`

	// Gate is the pass/fail/error verdict. The graph acts on Gate alone to
	// decide whether a quality-gated node succeeded; it never inspects Raw
	// to derive its own verdict.
	Gate GateStatus `json:"gate"`

	// Counts summarizes Issues by severity.
	Counts Counts `json:"counts"`

	// Issues is every issue found, in no guaranteed order. Never nil: an
	// adapter with nothing to report returns an empty, non-nil slice, so
	// this serializes as "issues": [] rather than "issues": null — another
	// instance of the identical-key-set-and-shape guarantee above applying
	// to value, not just key presence.
	Issues []Issue `json:"issues"`

	// Summary is a short, human-readable synopsis suitable for a run log
	// or a PR comment — for example "3 issues (1 blocker, 2 major)" or a
	// one-line AI reviewer verdict. Never parsed by the graph.
	Summary string `json:"summary"`

	// Raw is the adapter's unparsed, tool-specific output — golangci-lint's
	// JSON, a SonarQube API response, an AI reviewer's full transcript.
	// The graph never reads Raw; it exists for logs, debugging and
	// adapter-specific downstream tooling. Raw legitimately differs in
	// shape between two conforming adapters — only Gate, Counts and Issues
	// are required to mean the same thing across adapters.
	Raw json.RawMessage `json:"raw"`
}

// Reviewer performs a holistic quality review against a caller-supplied
// threshold and returns a gate verdict — the AI-review and full-Sonar-
// quality-gate counterpart to Linter. Unlike Linter, a Reviewer is not
// auto-detected: the graph is configured with exactly one Reviewer for its
// quality-gated review node and calls it directly.
type Reviewer interface {
	// Review evaluates req and returns a QualityReport.
	//
	// Review must honor req.FailOn when computing QualityReport.Gate: Gate
	// is GatePass if and only if Counts.AtOrAbove(req.FailOn) == 0.
	//
	// If Review cannot reach or run its underlying tool at all — network
	// down, MCP server unreachable, sonar-scanner not installed — it
	// returns an error: one wrapping ErrToolchainMissing for a missing
	// local binary, a plain error otherwise. A non-nil error is
	// authoritative and the caller must not read the returned report as a
	// verdict.
	//
	// The report accompanying that error must still be structurally valid
	// — Gate set to GateError and Issues non-nil — because QualityReport's
	// identical-key-set guarantee holds on every path, including failures.
	// Returning the zero QualityReport would serialize Issues as null and
	// break it. This is a serialization requirement, not a verdict: it
	// does not collapse the distinction below.
	//
	// GateError with a nil error means something different and narrower:
	// the tool ran and analyzed the change, but the gate computation could
	// not reach a verdict. "We never learned the answer" is not "the
	// answer was no", so neither case may be reported as GateFail.
	Review(ctx context.Context, req ReviewRequest) (QualityReport, error)
}

// ReviewRequest is the input to one Reviewer.Review call.
type ReviewRequest struct {
	// WorkDir is the absolute path to the repository or workspace being
	// reviewed. Required.
	WorkDir string `json:"work_dir"`

	// ChangedFiles lists the paths, relative to WorkDir, the review should
	// focus on. A Reviewer that only supports whole-project analysis may
	// ignore it and review everything; a Reviewer that supports scoped
	// analysis should prefer it over scanning WorkDir in full. Empty means
	// "review everything".
	ChangedFiles []string `json:"changed_files"`

	// ProjectKey identifies the project to a project-keyed backend such as
	// SonarQube. A Reviewer with no such concept ignores it.
	ProjectKey string `json:"project_key"`

	// FailOn is the minimum Severity that causes QualityReport.Gate to be
	// GateFail. Required — and a Reviewer receiving the zero value
	// (SeverityInfo) must treat it as a literal, deliberately strict
	// threshold (fail on anything at all), not as "unset". SeverityInfo is
	// a valid severity a caller can genuinely want to gate on, so it
	// cannot double as a sentinel for "the caller forgot to set this". A
	// caller that wants no gating at all should not call Review in the
	// first place, not pass an out-of-range Severity to mean "off".
	FailOn Severity `json:"fail_on"`
}
