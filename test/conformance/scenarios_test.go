//go:build unix

package conformance

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/internal/nodes/aireview"
	"github.com/dhaam-ai/belay/internal/sonar/mcp"
	"github.com/dhaam-ai/belay/internal/sonar/scanner"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// errFixtureExecutableNotFound stands in for the *exec.Error a real
// exec.LookPath("docker") would return when the binary is not installed.
// It is never passed to a real lookup — see stubExecer — so this fixture
// exists only to give *exec.ToolchainError a plausible Err to wrap.
var errFixtureExecutableNotFound = errors.New(`exec: "docker": executable file not found in $PATH`)

// Names used both as reviewersUnderTest entries' identity and as the keys
// of scenario.wantErr. Declared once so a typo in one cannot silently
// desync it from the other.
const (
	nameScanner  = "internal/sonar/scanner"
	nameAIReview = "internal/nodes/aireview"
)

// Fixed values every scenario's golden request shares, unless a scenario
// deliberately needs a different one.
const (
	testProjectKey = "belay-conformance"
	testHostURL    = "https://sonar.example.test"
	// testToken is shaped like a real SonarQube user token so it would be
	// recognizable if it ever leaked, but it is synthetic: there is no
	// such SonarQube instance.
	//nolint:gosec // synthetic fixture value, not a real credential.
	testToken = "squ_conformance0123456789abcdef0123456789"
)

// goldenRequest builds the belay.ReviewRequest a scenario hands to both
// reviewers. WorkDir is a fresh, real, empty directory — both adapters
// require one to exist, and internal/sonar/scanner refuses a WorkDir that
// is not an existing directory before it ever reaches the stub Execer.
func goldenRequest(t *testing.T, failOn belay.Severity) belay.ReviewRequest {
	t.Helper()
	return belay.ReviewRequest{
		WorkDir:      t.TempDir(),
		ChangedFiles: []string{"pkg/foo.go", "pkg/bar.go"},
		ProjectKey:   testProjectKey,
		FailOn:       failOn,
	}
}

// scannerEnv reports SONAR_TOKEN and SONAR_HOST_URL as set, and everything
// else as unset — enough for scanner.Scanner.validateRequest to pass so a
// scenario's outcome is decided by the stub Execer, not by an environment
// check.
func scannerEnv() func(string) (string, bool) {
	return func(name string) (string, bool) {
		switch name {
		case scanner.EnvToken:
			return testToken, true
		case scanner.EnvHostURL:
			return testHostURL, true
		default:
			return "", false
		}
	}
}

// newScannerReviewer wires a *scanner.Scanner to e, the test environment,
// and a silent logger. Every scenario's newScanner closure is built on
// this.
func newScannerReviewer(t *testing.T, e scanner.Execer) belay.Reviewer {
	t.Helper()
	return scanner.New(
		scanner.WithExecer(e),
		scanner.WithLogger(quietLogger()),
		scanner.WithLookupEnv(scannerEnv()),
	)
}

// newAIReviewReviewer wires an *aireview.Reviewer to s, bypassing the MCP
// dial entirely (aireview.WithSession). Every scenario's newAIReview
// closure that is not about a missing toolchain is built on this.
func newAIReviewReviewer(t *testing.T, s aireview.Session) belay.Reviewer {
	t.Helper()
	return aireview.New(
		config.MCP{Mode: config.MCPModeDocker, Image: "sonarsource/sonar-mcp-server"},
		aireview.WithSession(s),
		aireview.WithLogger(quietLogger()),
		aireview.WithLookupEnv(noEnv),
	)
}

// newAIReviewToolchainMissing wires an *aireview.Reviewer to a docker
// binary path that provably does not exist, so its dial fails in the
// kernel's exec lookup — no container, no process — exactly the pattern
// internal/nodes/aireview's own tests use to prove the same translation.
func newAIReviewToolchainMissing(t *testing.T) belay.Reviewer {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "docker-does-not-exist")
	return aireview.New(
		config.MCP{Mode: config.MCPModeDocker, Image: "sonarsource/sonar-mcp-server"},
		aireview.WithDockerBinary(missing),
		aireview.WithLogger(quietLogger()),
		aireview.WithLookupEnv(noEnv),
	)
}

// scenario is one shared input this suite drives every registered reviewer
// through: a single belay.ReviewRequest — identical for every reviewer,
// which is what "golden input" means here — plus, per reviewer, the
// scripted backend that makes its own underlying tool answer the way this
// scenario's name promises.
type scenario struct {
	// name identifies the scenario as a t.Run subtest name. The five
	// entries in the scenarios slice below are, by name, the five
	// categories T33's acceptance criteria require: gate pass, gate fail,
	// no verdict reached, an explicitly empty issue list, and an adapter
	// that could not run at all.
	name string

	// request builds the belay.ReviewRequest both reviewers receive.
	// Called once per scenario run so both reviewers see byte-identical
	// input, never two independently constructed requests that merely
	// look alike.
	request func(t *testing.T) belay.ReviewRequest

	// newScanner and newAIReview build this scenario's two belay.Reviewer
	// instances, keyed in reviewersUnderTest by nameScanner/nameAIReview.
	newScanner  func(t *testing.T) belay.Reviewer
	newAIReview func(t *testing.T) belay.Reviewer

	// wantGate is the belay.GateStatus both reviewers are expected to
	// reach. It is shared: this suite drives both adapters through the
	// same conceptual outcome even though a CLI exit status and an MCP
	// tool response get there by entirely different means.
	wantGate belay.GateStatus

	// wantErr says, per reviewer name, whether Review is expected to
	// return a non-nil error for this scenario. This is the one place
	// the two reviewers are allowed to disagree — see the
	// "gate_error_no_verdict" scenario's comment for the documented,
	// deliberate reason.
	wantErr map[string]bool
}

// scenarios is every input this suite drives reviewersUnderTest through.
//
// Every non-nil-Issues assertion below runs against every entry here, so
// together they are what makes requirement 2 ("assert non-nil explicitly
// on every path — pass, fail, and error") true: gate_pass and
// empty_issue_list cover the pass path, gate_fail covers fail, and
// gate_error_no_verdict / adapter_could_not_run cover error.
var scenarios = []scenario{
	{
		// A clean pass, with real content behind it for the reviewer that
		// can produce content on a pass at all. internal/sonar/scanner
		// cannot: the scanner CLI never prints per-issue findings, only
		// the server's one-word verdict (see its package doc), so its
		// Issues is always empty here. internal/nodes/aireview fetches
		// SonarQube's issue list independently of the verdict, so it can
		// legitimately report a real (sub-threshold) issue on a pass.
		// This is the one intentional, documented CONTENT divergence this
		// suite exercises rather than hides — see the acceptance report.
		// It is not a SHAPE divergence: both still serialize "issues" as
		// a non-nil JSON array.
		name:    "gate_pass",
		request: func(t *testing.T) belay.ReviewRequest { return goldenRequest(t, belay.SeverityMajor) },
		newScanner: func(t *testing.T) belay.Reviewer {
			return newScannerReviewer(t, &stubExecer{
				stdout:   "QUALITY GATE STATUS: OK - View details on https://sonar.example.test/dashboard?id=belay-conformance",
				exitCode: 0,
			})
		},
		newAIReview: func(t *testing.T) belay.Reviewer {
			return newAIReviewReviewer(t, &fakeSession{
				status: mcp.ProjectStatus{Status: "OK"},
				issues: []mcp.SonarIssue{{
					Key: "i1", Rule: "go:S1005", Severity: "MINOR",
					Component: testProjectKey + ":pkg/foo.go", Line: 12,
					Message: "consider using a named constant here", Status: "OPEN",
				}},
			})
		},
		wantGate: belay.GatePass,
		wantErr:  map[string]bool{nameScanner: false, nameAIReview: false},
	},
	{
		// A failing gate where NEITHER adapter's fetched issues (none, for
		// aireview; the CLI reports none at all, for scanner) reach
		// fail_on on their own — the gate failed on a condition like
		// coverage or duplication that produces no discrete issue. Both
		// packages handle this identically: they synthesize exactly one
		// belay.SeverityBlocker issue carrying the SAME RuleID,
		// GateRuleID = "belay:sonar-quality-gate", shared verbatim between
		// the two packages for exactly this reason.
		// TestGateFailSyntheticIssueSharesRuleID pins that byte-for-byte
		// agreement.
		name:    "gate_fail",
		request: func(t *testing.T) belay.ReviewRequest { return goldenRequest(t, belay.SeverityMajor) },
		newScanner: func(t *testing.T) belay.Reviewer {
			return newScannerReviewer(t, &stubExecer{
				stdout:   "QUALITY GATE STATUS: FAILED - View details on https://sonar.example.test/dashboard?id=belay-conformance",
				exitCode: 1,
			})
		},
		newAIReview: func(t *testing.T) belay.Reviewer {
			return newAIReviewReviewer(t, &fakeSession{
				status: mcp.ProjectStatus{Status: "ERROR", Conditions: []mcp.QualityGateCondition{
					{Status: "ERROR", MetricKey: "new_coverage", Comparator: "LT", ErrorThreshold: "80", ActualValue: "42.3"},
				}},
				// No issues at all: the gate failed on coverage, which is
				// exactly the condition the synthetic GateRuleID issue
				// exists to represent on both sides.
			})
		},
		wantGate: belay.GateFail,
		wantErr:  map[string]bool{nameScanner: false, nameAIReview: false},
	},
	{
		// The tool ran and printed something, but nothing that names a
		// gate verdict at all: sonar.qualitygate.wait=false for the
		// scanner CLI, SonarQube's own "NONE" status word (no analysis
		// has run yet) for the MCP server. Both resolve to GateError.
		//
		// Where they deliberately differ: belay.Reviewer's own doc
		// comment draws a line inside GateError between "the tool could
		// not complete the review at all" (a non-nil error) and "the
		// tools ran fine but SonarQube had no verdict to give" (GateError
		// with a NIL error). internal/sonar/scanner's classify always
		// attaches an error to a GateError outcome — the CLI itself has
		// no equivalent of SonarQube's "NONE" that lets it distinguish
		// the two. internal/nodes/aireview does distinguish them, and
		// "NONE" is exactly the case its package doc calls out: "GateError
		// with a nil error means the tools ran and SonarQube simply had
		// no verdict to give." wantErr below encodes that: TRUE for
		// scanner, FALSE for aireview, for the identical conceptual
		// outcome. This is a genuine, intentional divergence at the
		// Go-error-return level ONLY — it does not touch
		// belay.QualityReport's JSON shape, which the pairwise shape
		// assertion below still holds both reviewers to.
		name:    "gate_error_no_verdict",
		request: func(t *testing.T) belay.ReviewRequest { return goldenRequest(t, belay.SeverityMajor) },
		newScanner: func(t *testing.T) belay.Reviewer {
			return newScannerReviewer(t, &stubExecer{
				stdout:   "INFO: Analysis report uploaded, waiting was disabled\n",
				exitCode: 0,
			})
		},
		newAIReview: func(t *testing.T) belay.Reviewer {
			return newAIReviewReviewer(t, &fakeSession{status: mcp.ProjectStatus{Status: "NONE"}})
		},
		wantGate: belay.GateError,
		wantErr:  map[string]bool{nameScanner: true, nameAIReview: false},
	},
	{
		// A clean pass with a literally, explicitly empty issue list on
		// both sides: exactly the shape most likely to be hand-compared
		// against a fixture, and exactly the shape append([]T(nil), ...)
		// silently gets wrong. This is the scenario
		// TestIssuesSerializeAsEmptyArrayNeverNull exists for.
		name:    "empty_issue_list",
		request: func(t *testing.T) belay.ReviewRequest { return goldenRequest(t, belay.SeverityMajor) },
		newScanner: func(t *testing.T) belay.Reviewer {
			return newScannerReviewer(t, &stubExecer{
				stdout:   "QUALITY GATE STATUS: OK - View details on https://sonar.example.test/dashboard?id=belay-conformance",
				exitCode: 0,
			})
		},
		newAIReview: func(t *testing.T) belay.Reviewer {
			return newAIReviewReviewer(t, &fakeSession{status: mcp.ProjectStatus{Status: "OK"}})
		},
		wantGate: belay.GatePass,
		wantErr:  map[string]bool{nameScanner: false, nameAIReview: false},
	},
	{
		// The adapter could not run at all: its underlying toolchain is
		// missing. Both translate this to the same sentinel,
		// belay.ErrToolchainMissing, and both are required to still hand
		// back a structurally valid, GateError QualityReport rather than
		// the zero value — belay.Reviewer's doc comment requires exactly
		// that ("Returning the zero QualityReport would serialize Issues
		// as null and break it").
		name:    "adapter_could_not_run",
		request: func(t *testing.T) belay.ReviewRequest { return goldenRequest(t, belay.SeverityMajor) },
		newScanner: func(t *testing.T) belay.Reviewer {
			return newScannerReviewer(t, &stubExecer{
				exitCode: -1,
				err: &exec.ToolchainError{
					Tool:   "docker",
					Err:    errFixtureExecutableNotFound,
					Detail: errFixtureExecutableNotFound.Error(),
				},
			})
		},
		newAIReview: newAIReviewToolchainMissing,
		wantGate: belay.GateError,
		wantErr:  map[string]bool{nameScanner: true, nameAIReview: true},
	},
}

// reviewerUnderTest names one belay.Reviewer implementation this suite
// holds to the shared belay.QualityReport / state.Review shape contract,
// plus how to build it for a given scenario.
type reviewerUnderTest struct {
	name  string
	build func(t *testing.T, sc scenario) belay.Reviewer
}

// reviewersUnderTest is the fixed, enumerated roster of belay.Reviewer
// implementations this conformance suite proves interchangeable.
//
// Every test in this package drives its comparisons from this list rather
// than naming the two packages ad hoc, and TestReviewersUnderTestIsComplete
// pins its length at 2. belay.Reviewer (pkg/belay/quality.go) exists
// precisely so a deterministic tool and an AI-driven one are
// interchangeable to the graph; a third belay.Reviewer added to the
// codebase without a matching entry here would let a non-conforming
// adapter ship unnoticed, which defeats the entire point of T33. If this
// ever needs to grow, add the new adapter's entry above and update
// TestReviewersUnderTestIsComplete's wantReviewerCount in the same change
// — never one without the other.
var reviewersUnderTest = []reviewerUnderTest{
	{
		name:  nameScanner,
		build: func(t *testing.T, sc scenario) belay.Reviewer { return sc.newScanner(t) },
	},
	{
		name:  nameAIReview,
		build: func(t *testing.T, sc scenario) belay.Reviewer { return sc.newAIReview(t) },
	},
}
