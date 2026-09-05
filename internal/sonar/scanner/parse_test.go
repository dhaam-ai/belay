//go:build unix

package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/internal/exec"
	"github.com/belay-dev/belay/pkg/belay"
)

// TestReviewOutcomes is the three-way exit-code table, and the most important
// test in the package.
//
// The distinction it pins is the one challenge step 6 depends on: a failed
// quality gate exits non-zero and is NOT a Go error, because the graph routes
// it back to the fix node exactly like a failing test. A scan that could not
// reach a verdict also exits non-zero and IS an error. Conflating them either
// way breaks the graph — one direction sends an agent to fix findings that do
// not exist, the other silently swallows a real quality failure.
func TestReviewOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		stdout      string
		stderr      string
		exitCode    int
		opts        []Option
		wantGate    belay.GateStatus
		wantErr     bool
		wantErrIs   error
		wantIssues  int
		wantVerdict string
	}{
		{
			name:        "gate passed",
			stdout:      readFixture(t, "gate-pass.txt"),
			exitCode:    0,
			wantGate:    belay.GatePass,
			wantErr:     false,
			wantIssues:  0,
			wantVerdict: "PASSED",
		},
		{
			// THE crux: non-zero exit, and error must be nil.
			name:        "gate failed",
			stdout:      readFixture(t, "gate-fail.txt"),
			exitCode:    1,
			wantGate:    belay.GateFail,
			wantErr:     false,
			wantIssues:  1,
			wantVerdict: "FAILED",
		},
		{
			name:      "gate wait timed out",
			stdout:    readFixture(t, "gate-wait-timeout.txt"),
			exitCode:  1,
			wantGate:  belay.GateError,
			wantErr:   true,
			wantErrIs: ErrGateWaitTimeout,
		},
		{
			name:      "authentication rejected",
			stdout:    readFixture(t, "auth-failure.txt"),
			stderr:    readFixture(t, "auth-failure.stderr.txt"),
			exitCode:  1,
			wantGate:  belay.GateError,
			wantErr:   true,
			wantErrIs: ErrNoGateVerdict,
		},
		{
			name:      "host unreachable",
			stdout:    readFixture(t, "unreachable-host.txt"),
			exitCode:  1,
			wantGate:  belay.GateError,
			wantErr:   true,
			wantErrIs: ErrNoGateVerdict,
		},
		{
			// Exit zero is not a pass. The scanner uploaded its report and
			// left without waiting, so belay has no verdict to act on.
			name:      "gate wait disabled yields no verdict",
			stdout:    readFixture(t, "no-verdict.txt"),
			exitCode:  0,
			opts:      []Option{WithQualityGateWait(false)},
			wantGate:  belay.GateError,
			wantErr:   true,
			wantErrIs: ErrNoGateVerdict,
		},
		{
			// A passing verdict contradicted by a non-zero exit resolves
			// to GateError, never to GatePass. A false pass is the one
			// failure with nothing downstream to catch it.
			name:        "passing verdict with a non-zero exit",
			stdout:      readFixture(t, "gate-pass.txt"),
			exitCode:    2,
			wantGate:    belay.GateError,
			wantErr:     true,
			wantErrIs:   errVerdictExitMismatch,
			wantVerdict: "PASSED",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubExecer{stdout: tc.stdout, stderr: tc.stderr, exitCode: tc.exitCode}
			s := newTestScanner(stub, tc.opts...)

			report, err := s.Review(context.Background(), request(t))

			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("error = nil, want an error (gate %s)", report.Gate)
			case !tc.wantErr && err != nil:
				t.Fatalf("error = %v, want nil; a failed gate is a normal branch, not a Go error", err)
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Errorf("errors.Is(err, %v) = false; err = %v", tc.wantErrIs, err)
			}
			if report.Gate != tc.wantGate {
				t.Errorf("Gate = %s, want %s", report.Gate, tc.wantGate)
			}
			if len(report.Issues) != tc.wantIssues {
				t.Errorf("len(Issues) = %d, want %d", len(report.Issues), tc.wantIssues)
			}
			// A failed gate must never satisfy the "the tool is missing"
			// test, which is what decides install-software vs edit-code.
			if errors.Is(err, belay.ErrToolchainMissing) {
				t.Error("a scan that ran reported ErrToolchainMissing")
			}
			assertReportShape(t, report)

			var raw RawScan
			if uerr := json.Unmarshal(report.Raw, &raw); uerr != nil {
				t.Fatalf("unmarshal Raw: %v", uerr)
			}
			if raw.Verdict != tc.wantVerdict {
				t.Errorf("Raw.Verdict = %q, want %q", raw.Verdict, tc.wantVerdict)
			}
			if raw.Gate != tc.wantGate.String() {
				t.Errorf("Raw.Gate = %q, want %q", raw.Gate, tc.wantGate.String())
			}
			if raw.ExitCode != tc.exitCode {
				t.Errorf("Raw.ExitCode = %d, want %d", raw.ExitCode, tc.exitCode)
			}
		})
	}
}

// TestToolchainMissingTranslation is the trap test.
//
// internal/exec and pkg/belay declare separate ErrToolchainMissing sentinels.
// Returning exec's straight out of a belay.Reviewer makes
// errors.Is(err, belay.ErrToolchainMissing) false, which silently sends the
// graph down the "the agent should edit code" branch when the real remedy is a
// human installing software. Every path that means "belay could not get the
// scanner running" is checked here.
func TestToolchainMissingTranslation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		stub     *stubExecer
		opts     []Option
		wantTool string
	}{
		{
			name:     "docker is not installed",
			stub:     missingRunner("docker"),
			wantTool: "docker",
		},
		{
			name:     "sonar-scanner is not installed",
			stub:     missingRunner("sonar-scanner"),
			opts:     []Option{WithRunner(RunnerBinary)},
			wantTool: "sonar-scanner",
		},
		{
			// docker ran; the image could not be obtained. Exit 125 is
			// documented as "the error is with Docker daemon itself",
			// which is what makes this distinguishable from a failed
			// quality gate given both are non-zero exits.
			name:     "scanner image cannot be pulled",
			stub:     &stubExecer{stderr: readFixture(t, "docker-image-missing.txt"), exitCode: 125},
			opts:     []Option{WithImage("sonarsource/sonar-scanner-cli:no-such-tag")},
			wantTool: "sonarsource/sonar-scanner-cli:no-such-tag",
		},
		{
			// A daemon that is not running exits 1, which the exit-status
			// rule would read as the container's own status, so this one
			// is matched by message instead.
			name:     "docker daemon is not running",
			stub:     &stubExecer{stderr: readFixture(t, "docker-daemon-down.txt"), exitCode: 1},
			wantTool: "docker",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestScanner(tc.stub, tc.opts...)

			report, err := s.Review(context.Background(), request(t))

			if !errors.Is(err, belay.ErrToolchainMissing) {
				t.Fatalf("errors.Is(err, belay.ErrToolchainMissing) = false; err = %v (%T)", err, err)
			}
			var te *belay.ToolchainError
			if !errors.As(err, &te) {
				t.Fatalf("errors.As(err, **belay.ToolchainError) = false; err = %v (%T)", err, err)
			}
			if te.Tool != tc.wantTool {
				t.Errorf("ToolchainError.Tool = %q, want %q", te.Tool, tc.wantTool)
			}
			if report.Gate != belay.GateError {
				t.Errorf("Gate = %s, want %s", report.Gate, belay.GateError)
			}
			assertReportShape(t, report)
		})
	}
}

// TestExecErrorsAreNotToolchainErrors is the other half of the trap.
//
// exec's own sentinel must not reach a caller as belay's, and belay's must not
// be claimed for a scanner that ran and disagreed.
func TestExecErrorsAreNotToolchainErrors(t *testing.T) {
	t.Parallel()
	stub := scanRunner(readFixture(t, "gate-fail.txt"), 1)
	s := newTestScanner(stub)
	_, err := s.Review(context.Background(), request(t))
	if err != nil {
		t.Fatalf("a failed gate returned an error: %v", err)
	}

	stub = missingRunner("docker")
	s = newTestScanner(stub)
	_, err = s.Review(context.Background(), request(t))
	if !errors.Is(err, belay.ErrToolchainMissing) {
		t.Fatalf("belay.ErrToolchainMissing not reported: %v", err)
	}
	// The underlying exec error is still reachable, so a caller that wants
	// the low-level cause can still get at it.
	if !errors.Is(err, exec.ErrToolchainMissing) {
		t.Error("the underlying exec.ErrToolchainMissing was discarded by the translation")
	}
}

// TestTimeoutIsGateErrorNotGateFail pins requirement 6 for both deadlines.
//
// "We never learned the answer" is not "the answer was no". Reporting either
// timeout as GateFail would send an agent into a fix loop with nothing to fix,
// and would burn the graph's give_up budget on a server that was merely slow.
func TestTimeoutIsGateErrorNotGateFail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		stub      *stubExecer
		wantErrIs error
	}{
		{
			// The scanner's own sonar.qualitygate.timeout expired: it ran
			// to completion and said so.
			name:      "scanner gate wait expired",
			stub:      scanRunner(readFixture(t, "gate-wait-timeout.txt"), 1),
			wantErrIs: ErrGateWaitTimeout,
		},
		{
			// belay's own deadline expired and exec killed the process
			// group mid-scan.
			name:      "belay deadline expired",
			stub:      timeoutRunner(readFixture(t, "no-verdict.txt")),
			wantErrIs: exec.ErrTimeout,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestScanner(tc.stub)

			report, err := s.Review(context.Background(), request(t))

			if report.Gate == belay.GateFail {
				t.Fatal("a timeout was reported as GateFail; the fix node would be handed a diagnosis that does not exist")
			}
			if report.Gate != belay.GateError {
				t.Errorf("Gate = %s, want %s", report.Gate, belay.GateError)
			}
			if !errors.Is(err, tc.wantErrIs) {
				t.Errorf("errors.Is(err, %v) = false; err = %v", tc.wantErrIs, err)
			}
			if len(report.Issues) != 0 {
				t.Errorf("len(Issues) = %d, want 0; nothing was measured", len(report.Issues))
			}
			assertReportShape(t, report)
		})
	}
}

// TestGateInvariantHoldsForEveryFailOn checks the belay.Reviewer contract that
// Gate == GatePass if and only if Counts.AtOrAbove(FailOn) == 0.
//
// A CLI scan cannot honor FailOn as a threshold — the server evaluated its own
// gate conditions long before belay saw a verdict — so the invariant is
// preserved structurally instead: a failed gate synthesizes exactly one
// belay.SeverityBlocker issue, which is at or above every threshold, and a
// passing gate synthesizes none.
func TestGateInvariantHoldsForEveryFailOn(t *testing.T) {
	t.Parallel()
	severities := []belay.Severity{
		belay.SeverityInfo, belay.SeverityMinor, belay.SeverityMajor,
		belay.SeverityCritical, belay.SeverityBlocker,
	}
	outcomes := []struct {
		name     string
		fixture  string
		exitCode int
		wantGate belay.GateStatus
	}{
		{"pass", "gate-pass.txt", 0, belay.GatePass},
		{"fail", "gate-fail.txt", 1, belay.GateFail},
	}
	for _, oc := range outcomes {
		for _, failOn := range severities {
			t.Run(oc.name+"/fail_on="+failOn.String(), func(t *testing.T) {
				t.Parallel()
				stub := scanRunner(readFixture(t, oc.fixture), oc.exitCode)
				s := newTestScanner(stub)
				req := request(t)
				req.FailOn = failOn

				report, err := s.Review(context.Background(), req)
				if err != nil {
					t.Fatalf("Review: %v", err)
				}
				if report.Gate != oc.wantGate {
					t.Fatalf("Gate = %s, want %s", report.Gate, oc.wantGate)
				}
				atOrAbove := report.Counts.AtOrAbove(failOn)
				wantPass := report.Gate == belay.GatePass
				if wantPass != (atOrAbove == 0) {
					t.Errorf("invariant broken: Gate = %s but Counts.AtOrAbove(%s) = %d",
						report.Gate, failOn, atOrAbove)
				}
			})
		}
	}
}

// TestGateFailIssue pins what the synthesized issue says.
//
// It is a representation of the server's verdict, not a fabricated finding,
// and every field has to say so: belay's own rule id rather than a SonarQube
// rule key, no file, no line, and a message that states plainly that the
// detail lives on the server.
func TestGateFailIssue(t *testing.T) {
	t.Parallel()
	stub := scanRunner(readFixture(t, "gate-fail.txt"), 1)
	s := newTestScanner(stub)

	report, err := s.Review(context.Background(), request(t))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(report.Issues) != 1 {
		t.Fatalf("len(Issues) = %d, want 1", len(report.Issues))
	}
	got := report.Issues[0]

	if got.RuleID != GateRuleID {
		t.Errorf("RuleID = %q, want %q", got.RuleID, GateRuleID)
	}
	if strings.Contains(got.RuleID, ":S") {
		t.Errorf("RuleID %q is shaped like a SonarQube rule key; it must not be mistakable for one", got.RuleID)
	}
	if got.Severity != belay.SeverityBlocker {
		t.Errorf("Severity = %s, want %s", got.Severity, belay.SeverityBlocker)
	}
	if got.File != "" || got.Line != 0 {
		t.Errorf("File/Line = %q/%d, want empty/0; a project-level gate is not attributable to a line", got.File, got.Line)
	}
	if !strings.Contains(got.Message, "https://sonar.example.test/dashboard?id=belay-demo") {
		t.Errorf("Message does not carry the dashboard URL, which is the only actionable detail available: %q", got.Message)
	}
	if !strings.Contains(got.Message, "SonarQube server") {
		t.Errorf("Message does not say where the findings actually live: %q", got.Message)
	}
	want := belay.Counts{Blocker: 1}
	if diff := cmp.Diff(want, report.Counts); diff != "" {
		t.Errorf("Counts mismatch (-want +got):\n%s", diff)
	}
}

// TestReportKeySetIsStableAcrossOutcomes is the T33 guard.
//
// belay.QualityReport promises two conforming adapters serialize to an
// identical key set regardless of content, with Source the only field expected
// to differ. That promise breaks first for the emptiest report, so every
// outcome this package can produce — including the ones where nothing ran — is
// marshalled and compared key for key.
func TestReportKeySetIsStableAcrossOutcomes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		stub *stubExecer
		opts []Option
		bad  bool
	}{
		{name: "pass", stub: scanRunner(readFixture(t, "gate-pass.txt"), 0)},
		{name: "fail", stub: scanRunner(readFixture(t, "gate-fail.txt"), 1)},
		{name: "no verdict", stub: scanRunner(readFixture(t, "no-verdict.txt"), 0)},
		{name: "toolchain missing", stub: missingRunner("docker")},
		{name: "belay timeout", stub: timeoutRunner("")},
		{name: "never started", stub: &stubExecer{}, bad: true},
	}
	var want []string
	for _, tc := range cases {
		s := newTestScanner(tc.stub, tc.opts...)
		req := request(t)
		if tc.bad {
			req.ProjectKey = ""
		}
		report, _ := s.Review(context.Background(), req)

		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.name, err)
		}
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		slices.Sort(keys)

		if want == nil {
			want = keys
		} else if diff := cmp.Diff(want, keys); diff != "" {
			t.Errorf("%s: key set differs from the first outcome (-first +%s):\n%s", tc.name, tc.name, diff)
		}
		if string(fields["issues"]) == "null" {
			t.Errorf("%s: issues serialized as null; it must be [] so an empty report matches a fixture", tc.name)
		}
		if string(fields["raw"]) == "null" {
			t.Errorf("%s: raw serialized as null", tc.name)
		}
		if string(fields["source"]) != `"`+Source+`"` {
			t.Errorf("%s: source = %s, want %q", tc.name, fields["source"], Source)
		}
	}
}

// TestParseVerdict covers the verdict vocabulary directly, including the
// mapping belay.GateStatus documents as the likeliest bug in a Sonar adapter.
func TestParseVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		output      string
		wantVerdict string
		wantURL     string
		wantGate    belay.GateStatus
		wantKnown   bool
	}{
		{
			name:        "passed under an INFO prefix",
			output:      "INFO: QUALITY GATE STATUS: PASSED - View details on https://s.test/dashboard?id=k\nINFO: EXECUTION SUCCESS",
			wantVerdict: "PASSED",
			wantURL:     "https://s.test/dashboard?id=k",
			wantGate:    belay.GatePass,
			wantKnown:   true,
		},
		{
			name:        "failed with no level prefix at all",
			output:      "ERROR: Error during SonarScanner execution\nQUALITY GATE STATUS: FAILED - View details on https://s.test/dashboard?id=k",
			wantVerdict: "FAILED",
			wantURL:     "https://s.test/dashboard?id=k",
			wantGate:    belay.GateFail,
			wantKnown:   true,
		},
		{
			// SonarQube's project quality-gate status spells a failed gate
			// "ERROR". That is belay's GateFail. Mapping it onto
			// belay.GateError by name is the bug belay.GateStatus warns
			// about at length.
			name:        "sonar ERROR means the gate was evaluated and failed",
			output:      "QUALITY GATE STATUS: ERROR - View details on https://s.test/dashboard?id=k",
			wantVerdict: "ERROR",
			wantURL:     "https://s.test/dashboard?id=k",
			wantGate:    belay.GateFail,
			wantKnown:   true,
		},
		{
			name:        "OK is a pass",
			output:      "QUALITY GATE STATUS: OK",
			wantVerdict: "OK",
			wantGate:    belay.GatePass,
			wantKnown:   true,
		},
		{
			name:        "lowercase is accepted and normalized",
			output:      "quality gate status: passed - View details on https://s.test/d",
			wantVerdict: "PASSED",
			wantURL:     "https://s.test/d",
			wantGate:    belay.GatePass,
			wantKnown:   true,
		},
		{
			// Removed in SonarQube 7.6 and never a build failure. belay
			// does not guess at it.
			name:        "WARN is not a verdict belay acts on",
			output:      "QUALITY GATE STATUS: WARN",
			wantVerdict: "WARN",
			wantKnown:   false,
		},
		{
			// The project has no quality gate assigned. A real answer,
			// but not one belay can gate on.
			name:        "NONE is not a verdict belay acts on",
			output:      "QUALITY GATE STATUS: NONE",
			wantVerdict: "NONE",
			wantKnown:   false,
		},
		{
			name:      "no verdict at all",
			output:    "INFO: ANALYSIS SUCCESSFUL\nINFO: EXECUTION SUCCESS",
			wantKnown: false,
		},
		{
			name:        "the last verdict wins",
			output:      "QUALITY GATE STATUS: PASSED\nQUALITY GATE STATUS: FAILED - View details on https://s.test/d",
			wantVerdict: "FAILED",
			wantURL:     "https://s.test/d",
			wantGate:    belay.GateFail,
			wantKnown:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verdict, url := parseVerdict(tc.output)
			if verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q", verdict, tc.wantVerdict)
			}
			if url != tc.wantURL {
				t.Errorf("dashboard URL = %q, want %q", url, tc.wantURL)
			}
			gate, known := verdicts[verdict]
			if known != tc.wantKnown {
				t.Fatalf("known = %v, want %v", known, tc.wantKnown)
			}
			if known && gate != tc.wantGate {
				t.Errorf("gate = %s, want %s", gate, tc.wantGate)
			}
		})
	}
}

// TestVerdictIsReadFromEitherStream proves the parser reads stdout and stderr
// together.
//
// Which stream the scanner logs a level to is not something belay controls,
// and a verdict found on only one of them is a parser that stops working after
// a scanner upgrade.
func TestVerdictIsReadFromEitherStream(t *testing.T) {
	t.Parallel()
	verdictLine := "QUALITY GATE STATUS: FAILED - View details on https://sonar.example.test/dashboard?id=belay-demo"
	tests := []struct {
		name   string
		stdout string
		stderr string
	}{
		{"on stdout", verdictLine, "ERROR: Error during SonarScanner execution"},
		{"on stderr", "INFO: EXECUTION FAILURE", "ERROR: Error during SonarScanner execution\n" + verdictLine},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubExecer{stdout: tc.stdout, stderr: tc.stderr, exitCode: 1}
			s := newTestScanner(stub)
			report, err := s.Review(context.Background(), request(t))
			if err != nil {
				t.Fatalf("Review: %v", err)
			}
			if report.Gate != belay.GateFail {
				t.Errorf("Gate = %s, want %s", report.Gate, belay.GateFail)
			}
		})
	}
}

// TestTruncatedOutputFailsClosed checks the one case where losing output could
// invent a pass.
//
// exec's capture keeps the head of a stream and drops the tail, and the verdict
// is the last thing the scanner prints. A truncated capture therefore looks
// exactly like a scan with no verdict, which must resolve to GateError.
func TestTruncatedOutputFailsClosed(t *testing.T) {
	t.Parallel()
	head := strings.SplitN(readFixture(t, "gate-fail.txt"), "INFO: ------------- Check Quality Gate status", 2)[0]
	stub := &stubExecer{stdout: head, exitCode: 1, truncated: true}
	s := newTestScanner(stub)

	report, err := s.Review(context.Background(), request(t))

	if report.Gate != belay.GateError {
		t.Errorf("Gate = %s, want %s", report.Gate, belay.GateError)
	}
	if !errors.Is(err, ErrNoGateVerdict) {
		t.Errorf("errors.Is(err, ErrNoGateVerdict) = false; err = %v", err)
	}
	var raw RawScan
	if uerr := json.Unmarshal(report.Raw, &raw); uerr != nil {
		t.Fatalf("unmarshal Raw: %v", uerr)
	}
	if !raw.Truncated {
		t.Error("Raw.Truncated is false; a reader has no way to tell why the verdict was missing")
	}
}

// TestChangedFilesAreNotNarrowedTo documents an intentional omission.
//
// Mapping ReviewRequest.ChangedFiles onto sonar.sources would make the server
// treat every unlisted file as deleted, corrupting the project's stored state
// and the new-code delta of every later scan.
func TestChangedFilesAreNotNarrowedTo(t *testing.T) {
	t.Parallel()
	stub := scanRunner(readFixture(t, "gate-pass.txt"), 0)
	s := newTestScanner(stub)
	req := request(t)
	req.ChangedFiles = []string{"internal/a.go", "internal/b.go"}

	if _, err := s.Review(context.Background(), req); err != nil {
		t.Fatalf("Review: %v", err)
	}
	joined := strings.Join(stub.lastCall(t).Args, " ")
	if strings.Contains(joined, "sonar.sources") {
		t.Errorf("sonar.sources was set; it must default to the base directory or the repository's own configuration: %s", joined)
	}
	for _, f := range req.ChangedFiles {
		if strings.Contains(joined, f) {
			t.Errorf("changed file %q reached the scanner argv: %s", f, joined)
		}
	}
}

// TestNewDefaults pins the zero-option Scanner against the shipped
// review.sonar defaults in internal/config.
//
// This package deliberately does not import internal/config, so nothing but
// this test stops the two from drifting apart — and a drifted default is
// invisible until someone's scan uses an image or a timeout they never
// configured.
func TestNewDefaults(t *testing.T) {
	t.Parallel()
	// New() satisfies belay.Reviewer by construction; scanner.go carries the
	// compile-time assertion. This exercises the value New actually returns.
	var r belay.Reviewer = New()
	s, ok := r.(*Scanner)
	if !ok {
		t.Fatalf("New returned %T, want *Scanner", r)
	}
	if s.runner != RunnerDocker {
		t.Errorf("runner = %q, want %q (config default review.sonar.runner)", s.runner, RunnerDocker)
	}
	if s.image != "sonarsource/sonar-scanner-cli" {
		t.Errorf("image = %q, want the config default review.sonar.image", s.image)
	}
	if !s.gateWait {
		t.Error("gateWait = false, want true (config default review.sonar.quality_gate_wait)")
	}
	if s.gateTimeout != 300*time.Second {
		t.Errorf("gateTimeout = %s, want 300s (config default review.sonar.quality_gate_timeout)", s.gateTimeout)
	}
	if s.execer == nil || s.logger == nil || s.lookupEnv == nil {
		t.Error("New left a collaborator nil")
	}
}

// assertReportShape checks the invariants every report from this package must
// satisfy, on every path.
func assertReportShape(t *testing.T, report belay.QualityReport) {
	t.Helper()
	if report.Source != Source {
		t.Errorf("Source = %q, want %q", report.Source, Source)
	}
	if report.Issues == nil {
		t.Error("Issues is nil; it must be an empty non-nil slice so it serializes as []")
	}
	if report.Raw == nil {
		t.Error("Raw is nil")
	} else if !json.Valid(report.Raw) {
		t.Errorf("Raw is not valid JSON: %s", report.Raw)
	}
	if report.Summary == "" {
		t.Error("Summary is empty")
	}
	if report.Gate == belay.GateUnknown {
		t.Error("Gate is GateUnknown; a completed call must resolve to pass, fail or error")
	}
	if got := countIssues(report.Issues); got != report.Counts {
		t.Errorf("Counts %+v is not the histogram of Issues %+v", report.Counts, got)
	}
}
