//go:build unix

package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/belay-dev/belay/internal/exec"
	"github.com/belay-dev/belay/pkg/belay"
)

// gateStatusRe matches the one line the scanner prints naming the server's
// quality-gate verdict, and the dashboard URL that follows it.
//
// The verified shape, from a scanner console log, is:
//
//	09:03:06.004 INFO: EXECUTION FAILURE
//	09:03:06.057 ERROR: Error during SonarScanner execution
//	QUALITY GATE STATUS: FAILED - View details on https://sonar.example/dashboard?id=house
//
// The line's prefix is deliberately not matched. It varies with the scanner's
// log level and with the verdict — a passing gate arrives under INFO:, and a
// failing one is folded into the execution-failure message and arrives with no
// level prefix at all — while "QUALITY GATE STATUS:" itself is stable. Keying
// off the prefix is how a parser silently stops recognizing a passing gate
// after a scanner upgrade.
var gateStatusRe = regexp.MustCompile(`(?i)QUALITY GATE STATUS:\s*([A-Za-z_]+)(?:\s*-\s*View details on\s+(\S+))?`)

// gateWaitTimeoutMarker is what the scanner prints when the server has not
// finished computing the gate within sonar.qualitygate.timeout.
const gateWaitTimeoutMarker = "quality gate check timeout exceeded"

// dockerUnavailableMarkers identify a docker CLI that started but could not
// reach a daemon. Matching is case-insensitive substring.
//
// These are not scanner failures at all — nothing was analyzed — and the
// remedy is a human starting or fixing Docker, so they translate to
// belay.ErrToolchainMissing exactly like an absent binary.
var dockerUnavailableMarkers = []string{
	"cannot connect to the docker daemon",
	"is the docker daemon running",
	"permission denied while trying to connect to the docker daemon",
}

// errVerdictExitMismatch reports a passing verdict contradicted by a non-zero
// exit status.
//
// The scanner exits zero after printing a passing gate, so this combination
// means something failed after the gate was evaluated and belay cannot tell
// what. It is resolved to GateError rather than GatePass because for a quality
// tool a false pass is the one failure with no downstream check behind it.
var errVerdictExitMismatch = errors.New("belay/sonar: quality gate passed but the scanner exited non-zero")

// verdicts maps the scanner's own gate vocabulary to belay's.
//
// The mapping is a strict allowlist: a word that is not listed yields no
// verdict at all, and therefore GateError, rather than being guessed at.
//
//	PASSED, OK      -> GatePass
//	FAILED, ERROR   -> GateFail
//	anything else   -> no verdict (GateError, ErrNoGateVerdict)
//
// ERROR mapping to GateFail is the trap belay.GateStatus documents against:
// SonarQube's project quality-gate status uses the string "ERROR" to mean the
// gate was evaluated and failed. That is belay's GateFail. belay's GateError
// means the gate computation itself broke, which has no SonarQube equivalent,
// and matching these two by name is the single most likely bug in a
// Sonar-backed adapter.
//
// Deliberately absent: WARN, which SonarQube removed in 7.6 and which never
// failed a build, and NONE, which means no quality gate is assigned to the
// project. Both are real answers belay cannot act on, so both are GateError.
var verdicts = map[string]belay.GateStatus{
	"PASSED": belay.GatePass,
	"OK":     belay.GatePass,
	"FAILED": belay.GateFail,
	"ERROR":  belay.GateFail,
}

// RawScan is the shape of QualityReport.Raw for every report this package
// produces.
//
// belay.QualityReport documents Raw as the adapter's own tool-specific output,
// which the graph never reads; it exists for logs, debugging and human
// forensics. Everything needed to reconstruct why a verdict was reached is
// here, because for this adapter that reconstruction is the only way to tell a
// genuinely passing gate from one that passed because it scanned an empty
// directory.
//
// Every string field has already passed through the exec redactor.
type RawScan struct {
	// Runner is "docker" or "binary".
	Runner string `json:"runner"`
	// Tool is the executable belay started.
	Tool string `json:"tool"`
	// Args is the redacted argv, program first.
	Args []string `json:"args"`
	// ExitCode is the process exit status, or -1 if it never ran.
	ExitCode int `json:"exit_code"`
	// Verdict is the scanner's own gate word, such as "PASSED", or empty
	// when the scanner printed none.
	Verdict string `json:"verdict"`
	// DashboardURL is the project dashboard link the scanner printed beside
	// the verdict, or empty.
	DashboardURL string `json:"dashboard_url"`
	// Gate is the belay.GateStatus the verdict resolved to.
	Gate string `json:"gate"`
	// DurationMS is the scan's wall-clock time in milliseconds.
	DurationMS int64 `json:"duration_ms"`
	// TimedOut reports that belay's own deadline elapsed and the process
	// group was killed. It is distinct from the scanner's own
	// quality-gate wait expiring, which shows up in Stdout instead.
	TimedOut bool `json:"timed_out"`
	// Truncated reports that captured output hit the capture limit. A
	// truncated capture loses the tail, which is where the verdict is, so a
	// true here explains an otherwise puzzling ErrNoGateVerdict.
	Truncated bool `json:"truncated"`
	// Stdout is the redacted, capped standard output.
	Stdout string `json:"stdout"`
	// Stderr is the redacted, capped standard error.
	Stderr string `json:"stderr"`
	// Error is the rendered error, or empty on a pass or a failed gate.
	Error string `json:"error"`
}

// outcome is the classified result of one scan.
type outcome struct {
	gate         belay.GateStatus
	verdict      string
	dashboardURL string
	err          error
}

// Review implements belay.Reviewer.
//
// It runs one scan and reports the SonarQube server's quality-gate verdict. A
// failed gate is a successful call — (Gate: GateFail, nil) — because the graph
// routes it to the fix node exactly like a failing test. Only a scan that
// never reached a verdict returns an error, and then the report carries
// GateError.
//
// The returned report is always fully populated, including on every error
// path, so a caller can journal it before inspecting the error and so its
// serialized key set never depends on the outcome. See the package doc for the
// exact rules.
func (s *Scanner) Review(ctx context.Context, req belay.ReviewRequest) (belay.QualityReport, error) {
	workDir, err := s.validateRequest(req)
	if err != nil {
		return errorReport(RawScan{
			Runner: string(s.runner), Tool: s.tool(), ExitCode: -1,
			Args: []string{}, Error: err.Error(),
		}), err
	}

	cmd, err := s.command(workDir, req.ProjectKey)
	if err != nil {
		return errorReport(RawScan{
			Runner: string(s.runner), Tool: s.tool(), ExitCode: -1,
			Args: []string{}, Error: err.Error(),
		}), err
	}

	res, runErr := s.execer.Run(ctx, cmd)
	out := s.classify(res, runErr)

	raw := RawScan{
		Runner:       string(s.runner),
		Tool:         s.tool(),
		Args:         copyStrings(res.Args),
		ExitCode:     res.ExitCode,
		Verdict:      out.verdict,
		DashboardURL: out.dashboardURL,
		Gate:         out.gate.String(),
		DurationMS:   res.Duration.Milliseconds(),
		TimedOut:     res.TimedOut,
		Truncated:    res.Truncated,
		Stdout:       res.Stdout,
		Stderr:       res.Stderr,
	}
	if out.err != nil {
		raw.Error = out.err.Error()
	}

	report := newReport(out, raw, req.FailOn)
	s.logger.LogAttrs(ctx, slog.LevelDebug, "belay/sonar: scan finished",
		slog.String("runner", string(s.runner)),
		slog.String("gate", report.Gate.String()),
		slog.String("verdict", out.verdict),
		slog.Int("exit_code", res.ExitCode),
		slog.Bool("timed_out", res.TimedOut),
		slog.Duration("duration", res.Duration))
	return report, out.err
}

// classify turns one exec result into a gate verdict, and is where the
// three-outcome contract in the package doc actually lives.
//
// The order of the checks is the contract. A missing toolchain is decided
// before anything is read out of the output, because there is no output; a
// gate-wait timeout is decided before the verdict, because a timed-out scan
// has no verdict to find; and the verdict is decided before the exit status,
// because the exit status cannot distinguish a failed gate from a failed scan
// and the verdict line can.
func (s *Scanner) classify(res exec.Result, runErr error) outcome {
	// TRAP: internal/exec and pkg/belay declare separate ErrToolchainMissing
	// sentinels. Handing exec's straight back would make
	// errors.Is(err, belay.ErrToolchainMissing) false at the graph, which
	// decides "a human installs software" versus "the agent edits code" on
	// exactly that test. Translate here.
	if errors.Is(runErr, exec.ErrToolchainMissing) {
		return outcome{gate: belay.GateError, err: &belay.ToolchainError{Tool: s.tool(), Err: runErr}}
	}

	// A deadline, a cancellation, or an unusable working directory: the
	// process never reached a verdict. Sentinels (exec.ErrTimeout,
	// context.Canceled) are preserved through ScanError.Unwrap.
	//
	// belay's own deadline elapsing is GateError, not GateFail, for the same
	// reason the scanner's gate-wait timeout is: nothing was learned.
	if runErr != nil && !ranToCompletion(runErr) {
		return outcome{gate: belay.GateError, err: s.scanError(res, runErr)}
	}

	if s.runner == RunnerDocker {
		if tool, unavailable := dockerUnavailable(res); unavailable {
			return outcome{
				gate: belay.GateError,
				err:  &belay.ToolchainError{Tool: s.pick(tool), Err: s.scanError(res, nil)},
			}
		}
	}

	combined := res.Stdout + "\n" + res.Stderr

	if containsFold(combined, gateWaitTimeoutMarker) {
		return outcome{gate: belay.GateError, err: s.scanError(res, ErrGateWaitTimeout)}
	}

	verdict, dashboardURL := parseVerdict(combined)
	gate, known := verdicts[verdict]
	switch {
	case !known:
		return outcome{
			gate: belay.GateError, verdict: verdict, dashboardURL: dashboardURL,
			err: s.scanError(res, ErrNoGateVerdict),
		}
	case gate == belay.GateFail:
		// The one non-zero exit that is not an error. This is challenge
		// step 6: the gate failing routes back to fix exactly like a
		// failing test, so it must not surface as a Go error.
		return outcome{gate: belay.GateFail, verdict: verdict, dashboardURL: dashboardURL}
	case res.ExitCode != 0:
		return outcome{
			gate: belay.GateError, verdict: verdict, dashboardURL: dashboardURL,
			err: s.scanError(res, errVerdictExitMismatch),
		}
	default:
		return outcome{gate: belay.GatePass, verdict: verdict, dashboardURL: dashboardURL}
	}
}

// scanError wraps a failure with the diagnostics that explain it.
func (s *Scanner) scanError(res exec.Result, cause error) *ScanError {
	return &ScanError{
		Runner:   s.runner,
		Tool:     s.tool(),
		ExitCode: res.ExitCode,
		Stderr:   res.Stderr,
		Err:      cause,
	}
}

// pick returns tool, or the docker executable name when tool is empty.
func (s *Scanner) pick(tool string) string {
	if tool == "" {
		return s.tool()
	}
	return tool
}

// ranToCompletion reports whether err still means the process ran and printed
// what it had to say. A non-zero exit is the normal outcome for a failed
// quality gate, so *exec.ExitError is not a failure to run.
func ranToCompletion(err error) bool {
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// dockerUnavailable reports whether the docker CLI ran but could not obtain or
// start the scanner container, and names the thing a human has to fix.
//
// Docker's CLI reference defines the exit statuses this reads: "Exit code 125
// indicates that the error is with Docker daemon itself", "126 indicates that
// the specified contained command can't be invoked", "127 indicates that the
// contained command can't be found", and "Any exit code other than 125, 126,
// and 127 represent the exit code of the provided container command". So those
// three, and only those three, are docker failing rather than the scanner
// speaking — which is what makes an unpullable image distinguishable from a
// failed quality gate, given both are non-zero exits.
//
// The daemon markers are matched separately because a daemon that is not
// running makes the docker CLI exit 1, which the exit-status rule above
// classifies as the container's own status.
func dockerUnavailable(res exec.Result) (tool string, unavailable bool) {
	for _, marker := range dockerUnavailableMarkers {
		if containsFold(res.Stderr, marker) {
			return "", true
		}
	}
	switch res.ExitCode {
	case 125, 126, 127:
		// The image is what could not be obtained or started, and it is
		// what an operator has to act on, so it is the name reported
		// rather than "docker".
		return imageFromArgs(res.Args), true
	default:
		return "", false
	}
}

// imageFromArgs recovers the image name from a redacted argv, which is the
// argument immediately before the first -D property.
func imageFromArgs(args []string) string {
	for i, a := range args {
		if strings.HasPrefix(a, "-Dsonar.") && i > 0 {
			return args[i-1]
		}
	}
	return ""
}

// parseVerdict extracts the scanner's gate word and dashboard URL from
// captured output.
//
// The last match wins. The scanner prints the verdict once, but a failing scan
// echoes its error summary at the end of the run, and reading the final
// occurrence keeps the parse correct either way.
func parseVerdict(output string) (verdict, dashboardURL string) {
	matches := gateStatusRe.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return "", ""
	}
	last := matches[len(matches)-1]
	return strings.ToUpper(last[1]), last[2]
}

// newReport assembles the QualityReport for a classified outcome.
//
// failOn is accepted and used only for the summary line. It is not a threshold
// here: a CLI scan reports the server's project-level gate, and the server
// evaluated its own conditions long before belay saw a verdict. The package
// doc records that limitation, and the belay.Reviewer invariant
// Gate == GatePass ⟺ Counts.AtOrAbove(failOn) == 0 still holds for every
// failOn because a failed gate synthesizes one belay.SeverityBlocker issue and
// a passing one synthesizes none.
func newReport(out outcome, raw RawScan, failOn belay.Severity) belay.QualityReport {
	issues := make([]belay.Issue, 0, 1)
	if out.gate == belay.GateFail {
		issues = append(issues, gateIssue(out))
	}
	counts := countIssues(issues)
	return belay.QualityReport{
		Source:  Source,
		Gate:    out.gate,
		Counts:  counts,
		Issues:  issues,
		Summary: summarize(out, counts, failOn),
		Raw:     marshalRaw(raw),
	}
}

// errorReport is the report for a scan that never started.
//
// It is shaped exactly like every other report — non-nil Issues, non-nil Raw —
// because belay.QualityReport guarantees that two conforming adapters
// serialize to an identical key set, and the emptiest report is the one most
// likely to be compared against a fixture.
func errorReport(raw RawScan) belay.QualityReport {
	out := outcome{gate: belay.GateError}
	return newReport(out, raw, belay.SeverityMajor)
}

// gateIssue synthesizes the single issue that represents a failed server-side
// quality gate.
//
// The scanner CLI never prints the findings behind a verdict — the gate is
// evaluated server-side against conditions belay does not see — so this is a
// representation of the verdict, not an invented finding. Every part of it
// says so: RuleID is belay's own GateRuleID rather than a SonarQube rule key,
// the message states plainly where the detail lives, and File and Line are
// left empty and zero because a project-level gate is not attributable to a
// line.
//
// Severity is always belay.SeverityBlocker. See the package doc for why that,
// rather than a severity derived from ReviewRequest.FailOn.
func gateIssue(out outcome) belay.Issue {
	msg := "SonarQube quality gate failed. The scanner CLI reports the gate verdict only; " +
		"the failing conditions and the issues behind them are on the SonarQube server"
	if out.dashboardURL != "" {
		msg += " at " + out.dashboardURL
	}
	return belay.Issue{
		RuleID:   GateRuleID,
		Severity: belay.SeverityBlocker,
		Message:  msg + ".",
	}
}

// countIssues builds the severity histogram.
func countIssues(issues []belay.Issue) belay.Counts {
	var c belay.Counts
	for _, issue := range issues {
		switch issue.Severity {
		case belay.SeverityBlocker:
			c.Blocker++
		case belay.SeverityCritical:
			c.Critical++
		case belay.SeverityMajor:
			c.Major++
		case belay.SeverityMinor:
			c.Minor++
		case belay.SeverityInfo:
			c.Info++
		default:
			c.Info++
		}
	}
	return c
}

// summarize renders the one-line human synopsis carried in
// QualityReport.Summary.
func summarize(out outcome, c belay.Counts, failOn belay.Severity) string {
	switch out.gate {
	case belay.GatePass:
		return fmt.Sprintf("%s: quality gate %s; gate pass at fail_on=%s",
			Source, strings.ToLower(orUnknown(out.verdict)), failOn)
	case belay.GateFail:
		return fmt.Sprintf("%s: quality gate %s on the server (%d blocker); gate fail at fail_on=%s",
			Source, strings.ToLower(orUnknown(out.verdict)), c.Blocker, failOn)
	default:
		reason := "no verdict"
		if out.err != nil {
			reason = out.err.Error()
		}
		return Source + ": could not produce a quality gate verdict; gate error: " + reason
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// marshalRaw renders raw as QualityReport.Raw.
//
// Raw is documented as always present, so a marshal failure falls back to a
// valid JSON object carrying the marshal error rather than to nil, which would
// serialize the field as null and break the identical-shape guarantee that
// belay.QualityReport makes across adapters.
func marshalRaw(raw RawScan) json.RawMessage {
	if raw.Args == nil {
		raw.Args = []string{}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return json.RawMessage(`{"error":` + strconv.Quote("belay/sonar: raw output could not be encoded: "+err.Error()) + `}`)
	}
	return b
}

// copyStrings returns a non-nil copy of src.
//
// make-and-copy rather than append([]string(nil), src...), which collapses a
// non-nil empty slice back to nil and would serialize "args" as null for a
// command that never ran. internal/state shipped exactly that bug.
func copyStrings(src []string) []string {
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// containsFold reports whether s contains lit, ignoring case. lit must already
// be lowercase.
func containsFold(s, lit string) bool {
	return strings.Contains(strings.ToLower(s), lit)
}
