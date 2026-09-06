//go:build unix

package aireview

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/sonar/mcp"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// newReviewer builds a Reviewer over a scripted fake transport, dialing a
// REAL *mcp.Client so the decode path under test is mcp's own.
func newReviewer(t *testing.T, tr *fakeTransport) *Reviewer {
	t.Helper()
	return New(config.MCP{Mode: config.MCPModeServer, URL: "http://127.0.0.1:0/unused"},
		WithDialer(dialerFor(t, tr)), WithLogger(quietLogger()), WithLookupEnv(noEnv))
}

// TestReviewGateOutcomes is acceptance check 2 at the Reviewer layer: the
// three server verdicts map to the three belay.GateStatus values, and only
// the third is accompanied by "no verdict".
func TestReviewGateOutcomes(t *testing.T) {
	tests := []struct {
		name         string
		gateFixture  string
		issueFixture string
		failOn       belay.Severity
		wantGate     belay.GateStatus
		wantIssues   int
		wantErr      bool
	}{
		{
			name: "OK is a pass", gateFixture: "gate_pass.json", issueFixture: "issues_none.json",
			failOn: belay.SeverityMajor, wantGate: belay.GatePass, wantIssues: 0,
		},
		{
			name: "ERROR is a fail, not an error", gateFixture: "gate_fail.json", issueFixture: "issues_mixed.json",
			failOn: belay.SeverityMajor, wantGate: belay.GateFail, wantIssues: 5,
		},
		{
			name: "NONE reaches no verdict", gateFixture: "gate_none.json", issueFixture: "issues_none.json",
			failOn: belay.SeverityMajor, wantGate: belay.GateError, wantIssues: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newFakeTransport(t)
			tr.onTool("get_project_quality_gate_status", fixtureResponder(tc.gateFixture))
			tr.onTool("search_sonar_issues_in_projects", fixtureResponder(tc.issueFixture))

			got, err := newReviewer(t, tr).Review(t.Context(), okRequest(t, tc.failOn))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tc.wantErr)
			}
			if got.Gate != tc.wantGate {
				t.Errorf("Gate = %s, want %s", got.Gate, tc.wantGate)
			}
			if len(got.Issues) != tc.wantIssues {
				t.Errorf("len(Issues) = %d, want %d", len(got.Issues), tc.wantIssues)
			}
			assertReportRules(t, got)
		})
	}
}

// TestReviewMapsIssuesFromTheServer pins the whole decode-and-map chain
// against the hand-authored mixed fixture: two severity vocabularies, a
// multi-impact issue, an unrecognized severity, line-less findings, and a
// component belonging to a different project.
func TestReviewMapsIssuesFromTheServer(t *testing.T) {
	tr := newFakeTransport(t)
	tr.onTool("get_project_quality_gate_status", fixtureResponder("gate_pass.json"))
	tr.onTool("search_sonar_issues_in_projects", fixtureResponder("issues_mixed.json"))

	got, err := newReviewer(t, tr).Review(t.Context(), okRequest(t, belay.SeverityMajor))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	want := []belay.Issue{
		{RuleID: "go:S2068", Severity: belay.SeverityBlocker, File: "internal/auth/token.go", Line: 31,
			Message: "Revoke and change this password, as it is compromised.", Effort: "30min"},
		{RuleID: "go:S2259", Severity: belay.SeverityCritical, File: "internal/store/pg.go", Line: 118,
			Message: "A null pointer could be dereferenced here.", Effort: "20min"},
		{RuleID: "go:S1192", Severity: belay.SeverityMajor, File: "cmd/demo/main.go", Line: 7,
			Message: "Define a constant instead of duplicating this literal.", Effort: "10min"},
		{RuleID: "go:S9999", Severity: belay.SeverityInfo, File: "internal/store/pg.go", Line: 0,
			Message: "A finding whose severity vocabulary this belay build does not recognize."},
		{RuleID: "common-go:InsufficientCommentDensity", Severity: belay.SeverityInfo,
			File: "another-project:internal/other.go", Line: 0, Message: "Insufficient comment density."},
	}
	if diff := cmp.Diff(want, got.Issues); diff != "" {
		t.Errorf("Issues mismatch (-want +got):\n%s", diff)
	}
	wantCounts := belay.Counts{Blocker: 1, Critical: 1, Major: 1, Info: 2}
	if got.Counts != wantCounts {
		t.Errorf("Counts = %+v, want %+v", got.Counts, wantCounts)
	}
	// The server passed its own gate; belay's fail_on=major did not.
	if got.Gate != belay.GateFail {
		t.Errorf("Gate = %s, want fail: three issues are at or above fail_on=major", got.Gate)
	}
	assertReportRules(t, got)
}

// TestReviewAsksForWhatItNeeds pins the outbound requests. A search that
// forgot its project key would silently review the whole organization.
func TestReviewAsksForWhatItNeeds(t *testing.T) {
	tr := newFakeTransport(t)
	tr.onTool("get_project_quality_gate_status", fixtureResponder("gate_pass.json"))
	tr.onTool("search_sonar_issues_in_projects", fixtureResponder("issues_none.json"))

	if _, err := newReviewer(t, tr).Review(t.Context(), okRequest(t, belay.SeverityMajor)); err != nil {
		t.Fatalf("Review: %v", err)
	}

	gateCalls := tr.toolCalls(t, "get_project_quality_gate_status")
	if len(gateCalls) != 1 {
		t.Fatalf("get_project_quality_gate_status called %d times, want 1", len(gateCalls))
	}
	var gateArgs mcp.GetProjectQualityGateStatusRequest
	gateCalls[0].toolArgs(t, &gateArgs)
	if gateArgs.ProjectKey != "belay-demo" {
		t.Errorf("gate projectKey = %q, want %q", gateArgs.ProjectKey, "belay-demo")
	}

	searchCalls := tr.toolCalls(t, "search_sonar_issues_in_projects")
	if len(searchCalls) != 1 {
		t.Fatalf("search_sonar_issues_in_projects called %d times, want 1", len(searchCalls))
	}
	var searchArgs mcp.SearchSonarIssuesInProjectsRequest
	searchCalls[0].toolArgs(t, &searchArgs)
	if diff := cmp.Diff([]string{"belay-demo"}, searchArgs.ProjectKeys); diff != "" {
		t.Errorf("search projectKeys mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(openIssueStatuses, searchArgs.IssueStatuses); diff != "" {
		t.Errorf("search issueStatuses mismatch (-want +got):\n%s", diff)
	}
	if searchArgs.PageSize != issuePageSize || searchArgs.PageIndex != 1 {
		t.Errorf("search paging = index %d size %d, want index 1 size %d",
			searchArgs.PageIndex, searchArgs.PageSize, issuePageSize)
	}

	// analyze_code_snippet is advertised by the fixture and deliberately
	// never called; a responder for it was never registered, so calling it
	// would have failed the test in Send.
	if n := len(tr.toolCalls(t, "analyze_code_snippet")); n != 0 {
		t.Errorf("analyze_code_snippet called %d times, want 0", n)
	}
}

// TestReviewSkipsTheIssueSearchWithoutAVerdict proves a server that has run
// no analysis is not then asked for the issues that analysis would have
// produced.
func TestReviewSkipsTheIssueSearchWithoutAVerdict(t *testing.T) {
	tr := newFakeTransport(t)
	tr.onTool("get_project_quality_gate_status", fixtureResponder("gate_none.json"))

	got, err := newReviewer(t, tr).Review(t.Context(), okRequest(t, belay.SeverityMajor))
	if err != nil {
		t.Fatalf("Review returned an error for a NONE status; belay.Reviewer documents that as GateError with a nil error: %v", err)
	}
	if got.Gate != belay.GateError {
		t.Errorf("Gate = %s, want error", got.Gate)
	}
	if n := len(tr.toolCalls(t, "search_sonar_issues_in_projects")); n != 0 {
		t.Errorf("search_sonar_issues_in_projects called %d times, want 0", n)
	}
	assertReportRules(t, got)
}

// TestReviewToolNotAdvertised covers the deployment whose MCP server does
// not expose the gate tool at all. mcp.Client refuses locally, without
// sending anything.
func TestReviewToolNotAdvertised(t *testing.T) {
	tr := newFakeTransport(t)
	tr.onMethod("tools/list", fixtureResponder("tools_list_missing_gate.json"))

	got, err := newReviewer(t, tr).Review(t.Context(), okRequest(t, belay.SeverityMajor))
	errIs(t, err, mcp.ErrToolNotAdvertised)
	if got.Gate != belay.GateError {
		t.Errorf("Gate = %s, want error", got.Gate)
	}
	if n := len(tr.toolCalls(t, "get_project_quality_gate_status")); n != 0 {
		t.Errorf("a tools/call was sent for an unadvertised tool (%d times)", n)
	}
	assertReportRules(t, got)
}

// TestReviewServerError covers a server that answered with a JSON-RPC error
// object: the call reached it and it refused.
func TestReviewServerError(t *testing.T) {
	tr := newFakeTransport(t)
	tr.onTool("get_project_quality_gate_status", fixtureResponder("server_error.json"))

	got, err := newReviewer(t, tr).Review(t.Context(), okRequest(t, belay.SeverityMajor))
	errIs(t, err, mcp.ErrServerError)
	var rpcErr *mcp.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want an *mcp.RPCError recoverable with errors.As", err)
	}
	if rpcErr.Code != -32603 {
		t.Errorf("RPCError.Code = %d, want -32603", rpcErr.Code)
	}
	if got.Gate != belay.GateError {
		t.Errorf("Gate = %s, want error", got.Gate)
	}
	assertReportRules(t, got)
}

// TestReviewTransportFailure covers an unreachable server: nothing ever
// answered, so no *mcp.RPCError exists and none is invented.
func TestReviewTransportFailure(t *testing.T) {
	wire := errors.New("dial tcp 10.0.0.1:9000: connect: connection refused")
	tr := newFakeTransport(t)
	tr.onTool("get_project_quality_gate_status", errResponder(wire))

	got, err := newReviewer(t, tr).Review(t.Context(), okRequest(t, belay.SeverityMajor))
	errIs(t, err, wire)
	if errors.Is(err, mcp.ErrServerError) {
		t.Error("a transport failure was reported as a server error")
	}
	if got.Gate != belay.GateError {
		t.Errorf("Gate = %s, want error", got.Gate)
	}
	assertReportRules(t, got)
}

// TestMissingContainerRuntimeIsToolchainMissing is acceptance check 3.
//
// It points the docker deployment at an absolute path inside t.TempDir()
// that does not exist, so os/exec fails in the kernel's exec lookup: no
// process is created, no container is started, and the result does not
// depend on whether the host has Docker installed at all.
func TestMissingContainerRuntimeIsToolchainMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-a-container-runtime")

	r := New(config.MCP{Mode: config.MCPModeDocker, Image: "sonarsource/sonar-mcp-server"},
		WithLogger(quietLogger()), WithLookupEnv(noEnv), WithDockerBinary(missing))

	got, err := r.Review(t.Context(), okRequest(t, belay.SeverityMajor))
	errIs(t, err, belay.ErrToolchainMissing)

	var te *belay.ToolchainError
	if !errors.As(err, &te) {
		t.Fatalf("error = %v, want a *belay.ToolchainError recoverable with errors.As", err)
	}
	if te.Tool != missing {
		t.Errorf("ToolchainError.Tool = %q, want %q", te.Tool, missing)
	}
	if got.Gate != belay.GateError {
		t.Errorf("Gate = %s, want error", got.Gate)
	}
	assertReportRules(t, got)
}

// TestDialConfigurationErrors covers every deployment this package refuses
// to dial, before it touches a socket or a process.
func TestDialConfigurationErrors(t *testing.T) {
	tests := []struct {
		name string
		mcp  config.MCP
		want error
	}{
		{"cloud with no url", config.MCP{Mode: config.MCPModeCloud}, ErrMissingURL},
		{"server with no url", config.MCP{Mode: config.MCPModeServer}, ErrMissingURL},
		{"server with a blank url", config.MCP{Mode: config.MCPModeServer, URL: "   "}, ErrMissingURL},
		{"docker with no image", config.MCP{Mode: config.MCPModeDocker}, ErrMissingImage},
		{"an unknown mode", config.MCP{Mode: config.MCPMode("carrier-pigeon")}, ErrUnknownMode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New(tc.mcp, WithLogger(quietLogger()), WithLookupEnv(noEnv))
			got, err := r.Review(t.Context(), okRequest(t, belay.SeverityMajor))
			errIs(t, err, tc.want)
			if got.Gate != belay.GateError {
				t.Errorf("Gate = %s, want error", got.Gate)
			}
			assertReportRules(t, got)
		})
	}
}

// TestReviewValidation covers requests this package cannot act on.
func TestReviewValidation(t *testing.T) {
	tests := []struct {
		name string
		req  belay.ReviewRequest
		want error
	}{
		{"no work dir", belay.ReviewRequest{ProjectKey: "demo"}, ErrMissingWorkDir},
		{"no project key", belay.ReviewRequest{WorkDir: "/tmp"}, ErrMissingProjectKey},
		{"blank project key", belay.ReviewRequest{WorkDir: "/tmp", ProjectKey: "  "}, ErrMissingProjectKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess := &fakeSession{}
			r := New(config.MCP{}, WithSession(sess), WithLogger(quietLogger()), WithLookupEnv(noEnv))
			got, err := r.Review(t.Context(), tc.req)
			errIs(t, err, tc.want)
			if sess.closeCalls != 0 {
				t.Error("a session was opened for a request that could never be served")
			}
			if got.Gate != belay.GateError {
				t.Errorf("Gate = %s, want error", got.Gate)
			}
			assertReportRules(t, got)
		})
	}
}

// TestReviewRespectsContextCancellation covers requirement 7 at both the
// entry check and the mid-conversation check.
func TestReviewRespectsContextCancellation(t *testing.T) {
	t.Run("cancelled before the first call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		sess := &fakeSession{status: mcp.ProjectStatus{Status: "OK"}}
		r := New(config.MCP{}, WithSession(sess), WithLogger(quietLogger()), WithLookupEnv(noEnv))
		got, err := r.Review(ctx, okRequest(t, belay.SeverityMajor))
		errIs(t, err, context.Canceled)
		if got.Gate != belay.GateError {
			t.Errorf("Gate = %s, want error", got.Gate)
		}
		assertReportRules(t, got)
	})

	t.Run("cancelled between the gate and the search", func(t *testing.T) {
		tr := newFakeTransport(t)
		ctx, cancel := context.WithCancel(t.Context())
		tr.onTool("get_project_quality_gate_status", func(t *testing.T, req sentRequest) ([]byte, error) {
			cancel()
			return withID(t, loadFixture(t, "gate_pass.json"), req.ID), nil
		})
		tr.onTool("search_sonar_issues_in_projects", fixtureResponder("issues_none.json"))

		got, err := newReviewer(t, tr).Review(ctx, okRequest(t, belay.SeverityMajor))
		errIs(t, err, context.Canceled)
		if got.Gate != belay.GateError {
			t.Errorf("Gate = %s, want error; a cancelled review reached no verdict", got.Gate)
		}
		assertReportRules(t, got)
	})
}

// TestReviewClosesItsSession proves a review does not leak a container or a
// connection, including on the paths that failed.
func TestReviewClosesItsSession(t *testing.T) {
	tests := []struct {
		name string
		sess *fakeSession
	}{
		{"success", &fakeSession{status: mcp.ProjectStatus{Status: "OK"}}},
		{"gate call failed", &fakeSession{statusErr: errors.New("boom")}},
		{"search failed", &fakeSession{status: mcp.ProjectStatus{Status: "OK"}, searchErr: errors.New("boom")}},
		{"close itself failed", &fakeSession{status: mcp.ProjectStatus{Status: "OK"}, closeErr: errors.New("already gone")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New(config.MCP{}, WithSession(tc.sess), WithLogger(quietLogger()), WithLookupEnv(noEnv))
			got, _ := r.Review(t.Context(), okRequest(t, belay.SeverityMajor))
			if tc.sess.closeCalls != 1 {
				t.Errorf("Close called %d times, want 1", tc.sess.closeCalls)
			}
			assertReportRules(t, got)
		})
	}
}

// TestReviewPagesTheIssueSearch proves a project with more issues than one
// page keeps asking, and stops on a short page.
func TestReviewPagesTheIssueSearch(t *testing.T) {
	full := make([]mcp.SonarIssue, issuePageSize)
	for i := range full {
		full[i] = mcp.SonarIssue{Rule: "go:S1", Severity: "MINOR", Component: "belay-demo:a.go", Line: i + 1}
	}
	sess := &fakeSession{
		status: mcp.ProjectStatus{Status: "OK"},
		pages: []mcp.SearchSonarIssuesInProjectsResponse{
			{Total: issuePageSize + 2, Issues: full},
			{Total: issuePageSize + 2, Issues: []mcp.SonarIssue{
				{Rule: "go:S2", Severity: "BLOCKER", Component: "belay-demo:b.go", Line: 1},
				{Rule: "go:S3", Severity: "MINOR", Component: "belay-demo:c.go", Line: 2},
			}},
		},
	}
	r := New(config.MCP{}, WithSession(sess), WithLogger(quietLogger()), WithLookupEnv(noEnv))
	got, err := r.Review(t.Context(), okRequest(t, belay.SeverityMajor))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if sess.searchN != 2 {
		t.Errorf("search called %d times, want 2", sess.searchN)
	}
	if len(got.Issues) != issuePageSize+2 {
		t.Errorf("len(Issues) = %d, want %d", len(got.Issues), issuePageSize+2)
	}
	if got.Gate != belay.GateFail {
		t.Errorf("Gate = %s, want fail: the blocker on page two is above fail_on=major", got.Gate)
	}
	assertReportRules(t, got)
}

// TestReviewPagingCapIsRecorded proves the bound is honoured and, more
// importantly, that hitting it is written down rather than hidden.
func TestReviewPagingCapIsRecorded(t *testing.T) {
	full := make([]mcp.SonarIssue, issuePageSize)
	for i := range full {
		full[i] = mcp.SonarIssue{Rule: "go:S1", Severity: "INFO", Component: "belay-demo:a.go", Line: i + 1}
	}
	pages := make([]mcp.SearchSonarIssuesInProjectsResponse, maxIssuePages)
	for i := range pages {
		pages[i] = mcp.SearchSonarIssuesInProjectsResponse{Total: 1 << 20, Issues: full}
	}
	sess := &fakeSession{status: mcp.ProjectStatus{Status: "OK"}, pages: pages}
	r := New(config.MCP{}, WithSession(sess), WithLogger(quietLogger()), WithLookupEnv(noEnv))
	got, err := r.Review(t.Context(), okRequest(t, belay.SeverityMajor))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if sess.searchN != maxIssuePages {
		t.Errorf("search called %d times, want the cap %d", sess.searchN, maxIssuePages)
	}
	var raw RawReview
	if err := json.Unmarshal(got.Raw, &raw); err != nil {
		t.Fatalf("unmarshal Raw: %v", err)
	}
	if !raw.Truncated {
		t.Error("Raw.truncated is false after the page cap was reached")
	}
	if raw.Pages != maxIssuePages {
		t.Errorf("Raw.pages = %d, want %d", raw.Pages, maxIssuePages)
	}
	assertReportRules(t, got)
}

// TestRawRecordsTheDeployment proves Raw carries what a human debugging a
// run needs, and carries no credential.
func TestRawRecordsTheDeployment(t *testing.T) {
	tr := newFakeTransport(t)
	tr.onTool("get_project_quality_gate_status", fixtureResponder("gate_fail.json"))
	tr.onTool("search_sonar_issues_in_projects", fixtureResponder("issues_none.json"))

	r := New(config.MCP{Mode: config.MCPModeServer, URL: "https://sonar.example/mcp"},
		WithDialer(dialerFor(t, tr)), WithLogger(quietLogger()),
		WithLookupEnv(func(string) (string, bool) { return "super-secret-token", true }))

	got, err := r.Review(t.Context(), okRequest(t, belay.SeverityMajor))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	var raw RawReview
	if err := json.Unmarshal(got.Raw, &raw); err != nil {
		t.Fatalf("unmarshal Raw: %v", err)
	}
	if raw.Mode != string(config.MCPModeServer) || raw.Endpoint != "https://sonar.example/mcp" {
		t.Errorf("Raw deployment = %q/%q, want server/https://sonar.example/mcp", raw.Mode, raw.Endpoint)
	}
	if raw.ServerStatus != "ERROR" || raw.ServerGate != belay.GateFail.String() {
		t.Errorf("Raw server verdict = %q/%q, want ERROR/fail", raw.ServerStatus, raw.ServerGate)
	}
	if len(raw.Conditions) != 2 {
		t.Errorf("Raw.conditions has %d entries, want the 2 the fixture reports", len(raw.Conditions))
	}
	if diff := cmp.Diff([]string{"internal/store/pg.go"}, raw.ChangedFiles); diff != "" {
		t.Errorf("Raw.changed_files mismatch (-want +got):\n%s", diff)
	}
	if strings.Contains(string(got.Raw), "super-secret-token") {
		t.Fatal("Raw carries the SONAR_TOKEN value")
	}
	if strings.Contains(got.Summary, "super-secret-token") {
		t.Fatal("Summary carries the SONAR_TOKEN value")
	}
	assertReportRules(t, got)
}

// TestDockerArgsKeepCredentialsOffArgv pins the one construction this
// package cannot verify against a real container: the argv is asserted to
// forward credentials BY NAME, never as -e NAME=value, which would put a
// token in every process listing on the host.
func TestDockerArgsKeepCredentialsOffArgv(t *testing.T) {
	args := dockerArgs("sonarsource/sonar-mcp-server")
	want := []string{"run", "--rm", "-i",
		"-e", mcp.SecretEnvName, "-e", "SONAR_HOST_URL", "-e", "SONAR_ORGANIZATION",
		"sonarsource/sonar-mcp-server"}
	if diff := cmp.Diff(want, args); diff != "" {
		t.Errorf("dockerArgs() mismatch (-want +got):\n%s", diff)
	}
	for _, a := range args {
		if strings.Contains(a, "=") {
			t.Errorf("argv element %q carries a value; every -e must forward by name only", a)
		}
	}
}

// TestReviewerIsSafeToReuse proves the documented reuse property: two
// reviews against one Reviewer each dial, use and close their own session.
func TestReviewerIsSafeToReuse(t *testing.T) {
	dials := 0
	var last *fakeSession
	r := New(config.MCP{}, WithLogger(quietLogger()), WithLookupEnv(noEnv),
		WithDialer(func(context.Context) (Session, error) {
			dials++
			last = &fakeSession{status: mcp.ProjectStatus{Status: "OK"}}
			return last, nil
		}))
	for range 2 {
		if _, err := r.Review(t.Context(), okRequest(t, belay.SeverityMajor)); err != nil {
			t.Fatalf("Review: %v", err)
		}
	}
	if dials != 2 {
		t.Errorf("dialed %d times for 2 reviews, want 2", dials)
	}
	if last.closeCalls != 1 {
		t.Errorf("the second session was closed %d times, want 1", last.closeCalls)
	}
}
