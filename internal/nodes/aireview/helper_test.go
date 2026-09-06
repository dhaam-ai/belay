//go:build unix

package aireview

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dhaam-ai/belay/internal/sonar/mcp"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// No test in this package contacts a SonarQube server, starts Docker, opens
// a socket, or reads a real SONAR_TOKEN.
//
//   - Every reviewer behavior is driven either against fakeSession below or
//     against a REAL *mcp.Client speaking to fakeTransport, an in-memory
//     mcp.Transport that starts no process and opens no connection. The
//     second form is preferred wherever the tool-response -> QualityReport
//     mapping is what is under test, because it exercises mcp's own
//     decoding rather than a mock of it.
//   - The one test that proves a missing container runtime surfaces as
//     belay.ErrToolchainMissing points WithDockerBinary at a path inside
//     t.TempDir() that does not exist. The failure happens in the kernel's
//     exec lookup, so no process is ever created and `docker` is never
//     consulted, whether or not it is installed on the host.
//   - Credentials are always injected through WithLookupEnv, so os.Getenv
//     is never consulted and a developer's real SONAR_TOKEN can never leak
//     into a test.
//
// See the acceptance report for how this was verified.

// quietLogger discards everything, so a failing test reports an assertion
// rather than a wall of structured output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// noEnv is a WithLookupEnv function for which every variable is unset.
func noEnv(string) (string, bool) { return "", false }

// loadFixture reads a hand-authored testdata frame. See testdata/README.md.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	// #nosec G304 -- name is a fixture filename this package's own tests
	// choose, not external input.
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// withID re-serializes a fixture frame (authored with a placeholder "id": 0)
// with its id replaced, so one fixture can answer whichever request the
// client actually sent, wherever it landed in a call sequence.
func withID(t *testing.T, frame []byte, id int64) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("withID: unmarshal fixture: %v", err)
	}
	idBytes, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("withID: marshal id: %v", err)
	}
	m["id"] = idBytes
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("withID: marshal fixture: %v", err)
	}
	return out
}

// sentRequest is one decoded outbound frame fakeTransport recorded.
type sentRequest struct {
	Method         string
	ID             int64
	IsNotification bool
	Params         json.RawMessage
}

// toolName decodes a tools/call request's tool name, which is how
// fakeTransport dispatches to a per-tool responder.
func (s sentRequest) toolName(t *testing.T) string {
	t.Helper()
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(s.Params, &p); err != nil {
		t.Fatalf("toolName: %v", err)
	}
	return p.Name
}

// toolArgs decodes a tools/call request's arguments into v, so a test can
// assert what this package actually asked SonarQube for.
func (s sentRequest) toolArgs(t *testing.T, v any) {
	t.Helper()
	var p struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(s.Params, &p); err != nil {
		t.Fatalf("toolArgs: %v", err)
	}
	if err := json.Unmarshal(p.Arguments, v); err != nil {
		t.Fatalf("toolArgs: arguments: %v", err)
	}
}

// responder builds a reply frame for one sent request, or returns a
// transport-level error instead of a reply.
type responder func(t *testing.T, req sentRequest) ([]byte, error)

// fixtureResponder answers with a testdata frame, its id rewritten to match
// the request.
func fixtureResponder(name string) responder {
	return func(t *testing.T, req sentRequest) ([]byte, error) {
		return withID(t, loadFixture(t, name), req.ID), nil
	}
}

// errResponder fails at the transport level — the shape of an unreachable
// server, as opposed to a server that answered with a JSON-RPC error object.
func errResponder(err error) responder {
	return func(*testing.T, sentRequest) ([]byte, error) { return nil, err }
}

// fakeTransport is the in-memory mcp.Transport every client-level test here
// uses. Send decodes and records the outbound frame, then — for a request,
// never a notification — looks up a responder by JSON-RPC method (and, for
// tools/call, by tool name) to synthesize the reply Recv then serves.
type fakeTransport struct {
	t *testing.T

	mu         sync.Mutex
	sent       []sentRequest
	queue      [][]byte
	closeCalls int

	byMethod map[string]responder
	byTool   map[string]responder
}

var _ mcp.Transport = (*fakeTransport)(nil)

// newFakeTransport returns a transport already scripted for a successful
// handshake and tool listing; a test overrides only what it cares about.
func newFakeTransport(t *testing.T) *fakeTransport {
	t.Helper()
	return &fakeTransport{
		t: t,
		byMethod: map[string]responder{
			"initialize": fixtureResponder("initialize_response.json"),
			"tools/list": fixtureResponder("tools_list_response.json"),
		},
		byTool: map[string]responder{},
	}
}

// onMethod registers r to answer every request for method.
func (f *fakeTransport) onMethod(method string, r responder) { f.byMethod[method] = r }

// onTool registers r to answer every tools/call request naming tool.
func (f *fakeTransport) onTool(tool string, r responder) { f.byTool[tool] = r }

// sent requests, for assertions about what was asked.
func (f *fakeTransport) toolCalls(t *testing.T, tool string) []sentRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentRequest
	for _, s := range f.sent {
		if s.Method == "tools/call" && s.toolName(t) == tool {
			out = append(out, s)
		}
	}
	return out
}

// Send implements mcp.Transport.
func (f *fakeTransport) Send(_ context.Context, frame []byte) error {
	var probe struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		f.t.Fatalf("fakeTransport.Send: outbound frame is not valid JSON: %v", err)
	}
	req := sentRequest{Method: probe.Method, Params: probe.Params, IsNotification: len(probe.ID) == 0}
	if !req.IsNotification {
		if err := json.Unmarshal(probe.ID, &req.ID); err != nil {
			f.t.Fatalf("fakeTransport.Send: outbound id is not an integer: %v", err)
		}
	}

	f.mu.Lock()
	f.sent = append(f.sent, req)
	f.mu.Unlock()

	if req.IsNotification {
		return nil // notifications are never answered
	}

	r := f.byMethod[req.Method]
	if req.Method == "tools/call" {
		r = f.byTool[req.toolName(f.t)]
	}
	if r == nil {
		f.t.Fatalf("fakeTransport.Send: no responder for %q", req.Method)
	}
	reply, err := r(f.t, req)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.queue = append(f.queue, reply)
	f.mu.Unlock()
	return nil
}

// Recv implements mcp.Transport.
func (f *fakeTransport) Recv(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) == 0 {
		return nil, mcp.ErrNoFrame
	}
	frame := f.queue[0]
	f.queue = f.queue[1:]
	return frame, nil
}

// Close implements mcp.Transport.
func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return nil
}

// dialerFor returns a Dialer that hands out a real *mcp.Client speaking to
// tr, already through initialize and tools/list — the same two steps the
// production dialer performs, so a test differs from production only in
// which bytes come back.
func dialerFor(t *testing.T, tr *fakeTransport) Dialer {
	t.Helper()
	return func(ctx context.Context) (Session, error) {
		c := mcp.NewClient(tr, quietLogger())
		if _, err := c.Initialize(ctx); err != nil {
			return nil, err
		}
		if _, err := c.ListTools(ctx); err != nil {
			return nil, err
		}
		return c, nil
	}
}

// fakeSession is a scripted Session, for the handful of tests that are about
// the reviewer's own control flow rather than about decoding MCP frames.
type fakeSession struct {
	status     mcp.ProjectStatus
	statusRaw  json.RawMessage
	statusErr  error
	pages      []mcp.SearchSonarIssuesInProjectsResponse
	searchErr  error
	closeErr   error
	closeCalls int
	searchN    int
}

var _ Session = (*fakeSession)(nil)

func (s *fakeSession) GetProjectQualityGateStatus(ctx context.Context, _ mcp.GetProjectQualityGateStatusRequest) (mcp.GetProjectQualityGateStatusResponse, error) {
	if err := ctx.Err(); err != nil {
		return mcp.GetProjectQualityGateStatusResponse{}, err
	}
	if s.statusErr != nil {
		return mcp.GetProjectQualityGateStatusResponse{}, s.statusErr
	}
	return mcp.GetProjectQualityGateStatusResponse{ProjectStatus: s.status, Raw: s.statusRaw}, nil
}

func (s *fakeSession) SearchSonarIssuesInProjects(ctx context.Context, _ mcp.SearchSonarIssuesInProjectsRequest) (mcp.SearchSonarIssuesInProjectsResponse, error) {
	if err := ctx.Err(); err != nil {
		return mcp.SearchSonarIssuesInProjectsResponse{}, err
	}
	if s.searchErr != nil {
		return mcp.SearchSonarIssuesInProjectsResponse{}, s.searchErr
	}
	n := s.searchN
	s.searchN++
	if n >= len(s.pages) {
		return mcp.SearchSonarIssuesInProjectsResponse{Issues: []mcp.SonarIssue{}}, nil
	}
	return s.pages[n], nil
}

func (s *fakeSession) Close() error {
	s.closeCalls++
	return s.closeErr
}

// okRequest is the request every test uses unless it is testing validation.
func okRequest(t *testing.T, failOn belay.Severity) belay.ReviewRequest {
	t.Helper()
	return belay.ReviewRequest{
		WorkDir:      t.TempDir(),
		ChangedFiles: []string{"internal/store/pg.go"},
		ProjectKey:   "belay-demo",
		FailOn:       failOn,
	}
}

// assertReportRules asserts every field-population rule finalize promises,
// on whatever report it is handed. Acceptance check 4 calls it on every
// path this package can produce.
func assertReportRules(t *testing.T, q belay.QualityReport) {
	t.Helper()
	if q.Source != Source {
		t.Errorf("Source = %q, want %q", q.Source, Source)
	}
	if q.Issues == nil {
		t.Error("Issues is nil; belay.QualityReport requires a non-nil slice on every path")
	}
	if got, want := countIssues(q.Issues), q.Counts; got != want {
		t.Errorf("Counts = %+v, want the histogram of Issues %+v", want, got)
	}
	if q.Counts.Total() != len(q.Issues) {
		t.Errorf("Counts.Total() = %d, want len(Issues) = %d", q.Counts.Total(), len(q.Issues))
	}
	if q.Summary == "" {
		t.Error("Summary is empty")
	}
	if q.Gate == belay.GateUnknown {
		t.Error("Gate is GateUnknown, which no path may produce")
	}
	if len(q.Raw) == 0 {
		t.Fatal("Raw is empty; it must always be valid JSON")
	}
	if !json.Valid(q.Raw) {
		t.Fatalf("Raw is not valid JSON: %s", q.Raw)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(q.Raw, &obj); err != nil {
		t.Fatalf("Raw is not a JSON object: %v", err)
	}
	// The nil-slice discipline has to hold inside Raw too, or an artifact
	// grows a "changed_files": null nobody expected.
	for _, key := range []string{"changed_files", "conditions", "issues"} {
		v, ok := obj[key]
		if !ok {
			t.Errorf("Raw has no %q key", key)
			continue
		}
		if string(v) == "null" {
			t.Errorf("Raw[%q] is null; it must be an empty array instead", key)
		}
	}
	// The belay.Reviewer invariant: Gate is GatePass iff nothing reaches
	// the threshold. GateError is the documented third state and is exempt.
	if q.Gate != belay.GateError {
		atOrAbove := q.Counts.AtOrAbove(reportFailOn(t, q))
		if (q.Gate == belay.GatePass) != (atOrAbove == 0) {
			t.Errorf("Gate = %s with %d issue(s) at or above fail_on; belay.Reviewer requires GatePass iff that count is 0",
				q.Gate, atOrAbove)
		}
	}
}

// reportFailOn recovers the fail_on the report was built against out of Raw,
// so assertReportRules can check the belay.Reviewer invariant without every
// caller threading the threshold through by hand. Raw does not record it
// directly, so the summary's own rendering is parsed back; a report whose
// summary does not name one is checked against the strictest threshold,
// which is the safe direction.
func reportFailOn(t *testing.T, q belay.QualityReport) belay.Severity {
	t.Helper()
	for _, sev := range []belay.Severity{
		belay.SeverityBlocker, belay.SeverityCritical, belay.SeverityMajor,
		belay.SeverityMinor, belay.SeverityInfo,
	} {
		if strings.Contains(q.Summary, "fail_on="+sev.String()) {
			return sev
		}
	}
	return belay.SeverityInfo
}

// errIs is errors.Is with a nicer failure message.
func errIs(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("error = %v, want one satisfying errors.Is(err, %v)", got, want)
	}
}
