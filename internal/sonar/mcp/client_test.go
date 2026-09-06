//go:build unix

package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestClient_FullFlow drives the complete, intended sequence — initialize,
// tools/list, then a tools/call for each of the five SonarQube tools this
// package wraps — entirely against fakeTransport, and checks every typed
// response field a Reviewer (T32) will read.
func TestClient_FullFlow(t *testing.T) {
	t.Parallel()

	ft := newFakeTransport(t)
	ft.onMethod("initialize", fixtureResponder("initialize_response.json"))
	ft.onMethod("tools/list", fixtureResponder("tools_list_response.json"))
	ft.onTool(toolListQualityGates, fixtureResponder("call_list_quality_gates.json"))
	ft.onTool(toolGetRawSource, fixtureResponder("call_get_raw_source.json"))
	ft.onTool(toolAnalyzeCodeSnippet, fixtureResponder("call_analyze_code_snippet.json"))
	ft.onTool(toolGetProjectQualityGateStatus, fixtureResponder("call_get_project_quality_gate_status.json"))
	ft.onTool(toolSearchSonarIssuesInProjects, fixtureResponder("call_search_sonar_issues_in_projects.json"))

	c := NewClient(ft, quietLogger())
	ctx := context.Background()

	initResult, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if initResult.ProtocolVersion != "2025-06-18" {
		t.Errorf("ProtocolVersion = %q, want 2025-06-18", initResult.ProtocolVersion)
	}
	if initResult.ServerInfo.Name != "sonar-mcp-server" {
		t.Errorf("ServerInfo.Name = %q, want sonar-mcp-server", initResult.ServerInfo.Name)
	}
	// The spec-required notifications/initialized must have gone out as a
	// notification (no id), not a request.
	last := ft.lastSent()
	if last.Method != "notifications/initialized" || !last.IsNotification {
		t.Errorf("last sent frame = %+v, want a notifications/initialized notification", last)
	}

	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 6 {
		t.Fatalf("ListTools returned %d tools, want 6 (5 wrapped + 1 unused)", len(tools))
	}

	t.Run(toolListQualityGates, func(t *testing.T) {
		resp, err := c.ListQualityGates(ctx, ListQualityGatesRequest{})
		if err != nil {
			t.Fatalf("ListQualityGates: %v", err)
		}
		if len(resp.QualityGates) != 2 {
			t.Fatalf("QualityGates = %+v, want 2 entries", resp.QualityGates)
		}
		if !resp.QualityGates[0].IsDefault || resp.QualityGates[0].Name != "Sonar way" {
			t.Errorf("QualityGates[0] = %+v, want the default Sonar way gate", resp.QualityGates[0])
		}
		if len(resp.Raw) == 0 {
			t.Error("Raw is empty, want the tools/call result bytes")
		}
	})

	t.Run(toolGetRawSource, func(t *testing.T) {
		resp, err := c.GetRawSource(ctx, GetRawSourceRequest{Key: "proj:main.go"})
		if err != nil {
			t.Fatalf("GetRawSource: %v", err)
		}
		if !strings.Contains(resp.Source, "func Greet") {
			t.Errorf("Source = %q, want it to contain the fixture's function", resp.Source)
		}
	})

	t.Run(toolAnalyzeCodeSnippet, func(t *testing.T) {
		resp, err := c.AnalyzeCodeSnippet(ctx, AnalyzeCodeSnippetRequest{
			FileContent: "x = 1\n", Language: "python",
		})
		if err != nil {
			t.Fatalf("AnalyzeCodeSnippet: %v", err)
		}
		if len(resp.Issues) != 1 {
			t.Fatalf("Issues = %+v, want 1", resp.Issues)
		}
		if resp.Issues[0].BelaySeverity() != belay.SeverityMinor {
			t.Errorf("Issues[0].BelaySeverity() = %v, want SeverityMinor (LOW impact)", resp.Issues[0].BelaySeverity())
		}
		if resp.DeprecationNotice == "" {
			t.Error("DeprecationNotice is empty, want the fixture's notice")
		}
	})

	t.Run(toolGetProjectQualityGateStatus, func(t *testing.T) {
		resp, err := c.GetProjectQualityGateStatus(ctx, GetProjectQualityGateStatusRequest{ProjectKey: "belay-dev_belay"})
		if err != nil {
			t.Fatalf("GetProjectQualityGateStatus: %v", err)
		}
		if BelayGate(resp.ProjectStatus.Status) != belay.GateFail {
			t.Errorf("BelayGate(%q) = %v, want GateFail", resp.ProjectStatus.Status, BelayGate(resp.ProjectStatus.Status))
		}
		if len(resp.ProjectStatus.Conditions) != 2 {
			t.Fatalf("Conditions = %+v, want 2", resp.ProjectStatus.Conditions)
		}
	})

	t.Run(toolSearchSonarIssuesInProjects, func(t *testing.T) {
		resp, err := c.SearchSonarIssuesInProjects(ctx, SearchSonarIssuesInProjectsRequest{
			ProjectKeys: []string{"belay-dev_belay"},
		})
		if err != nil {
			t.Fatalf("SearchSonarIssuesInProjects: %v", err)
		}
		if resp.Total != 2 || len(resp.Issues) != 2 {
			t.Fatalf("resp = %+v, want Total=2 and 2 Issues", resp)
		}
		// AZ2 carries impacts HIGH and MEDIUM; BelaySeverity must take the
		// worst (HIGH -> Critical), not the first or the legacy MAJOR.
		if got := resp.Issues[1].BelaySeverity(); got != belay.SeverityCritical {
			t.Errorf("Issues[1].BelaySeverity() = %v, want SeverityCritical (worst of HIGH/MEDIUM impacts)", got)
		}
		wantFile := "internal/sonar/mcp/transport.go"
		if got := RelativeFile(resp.Issues[1].Component, "belay-dev_belay"); got != wantFile {
			t.Errorf("RelativeFile(%q, ...) = %q, want %q", resp.Issues[1].Component, got, wantFile)
		}
	})

	// Every request Client sent, including the initialize/tools/list
	// handshake, must carry a strictly increasing id.
	seen := map[int64]bool{}
	prev := int64(0)
	count := 0
	for _, s := range ft.sent {
		if s.IsNotification {
			continue
		}
		count++
		if seen[s.ID] {
			t.Errorf("id %d was reused", s.ID)
		}
		seen[s.ID] = true
		if s.ID <= prev {
			t.Errorf("id sequence not monotonic: %d did not increase past %d", s.ID, prev)
		}
		prev = s.ID
	}
	if count == 0 {
		t.Fatal("no requests were recorded")
	}
}

// TestClient_JSONRPCEdgeCases table-tests the correlation logic in
// Client.call directly: a malformed frame, a response with an id nobody
// asked for, and a notification, must each be silently skipped while
// Client keeps waiting for the real reply — and a genuine JSON-RPC error
// object must come back as *RPCError, never confused with a transport
// failure.
func TestClient_JSONRPCEdgeCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		inject func(t *testing.T, ft *fakeTransport, id int64)
		check  func(t *testing.T, resp GetProjectQualityGateStatusResponse, err error)
	}{
		{
			name: "malformed frame is skipped, real reply still wins",
			inject: func(t *testing.T, ft *fakeTransport, id int64) {
				ft.push(loadFixture(t, "malformed_frame.txt"))
				ft.push(withID(t, loadFixture(t, "call_get_project_quality_gate_status.json"), id))
			},
			check: func(t *testing.T, resp GetProjectQualityGateStatusResponse, err error) {
				if err != nil {
					t.Fatalf("CallTool: %v", err)
				}
				if resp.ProjectStatus.Status != "ERROR" {
					t.Errorf("Status = %q, want ERROR", resp.ProjectStatus.Status)
				}
			},
		},
		{
			name: "response with an unrecognized id is skipped",
			inject: func(t *testing.T, ft *fakeTransport, id int64) {
				stray := withID(t, loadFixture(t, "call_get_project_quality_gate_status.json"), id+1000)
				ft.push(stray)
				ft.push(withID(t, loadFixture(t, "call_get_project_quality_gate_status.json"), id))
			},
			check: func(t *testing.T, resp GetProjectQualityGateStatusResponse, err error) {
				if err != nil {
					t.Fatalf("CallTool: %v", err)
				}
				if resp.ProjectStatus.Status != "ERROR" {
					t.Errorf("Status = %q, want ERROR", resp.ProjectStatus.Status)
				}
			},
		},
		{
			name: "notification interleaved while awaiting a reply is not mistaken for it",
			inject: func(t *testing.T, ft *fakeTransport, id int64) {
				ft.push([]byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"scanning"}}`))
				ft.push(withID(t, loadFixture(t, "call_get_project_quality_gate_status.json"), id))
			},
			check: func(t *testing.T, resp GetProjectQualityGateStatusResponse, err error) {
				if err != nil {
					t.Fatalf("CallTool: %v", err)
				}
				if resp.ProjectStatus.Status != "ERROR" {
					t.Errorf("Status = %q, want ERROR", resp.ProjectStatus.Status)
				}
			},
		},
		{
			name: "jsonrpc error object becomes *RPCError, not a transport failure",
			inject: func(t *testing.T, ft *fakeTransport, id int64) {
				ft.push(withID(t, loadFixture(t, "error_response.json"), id))
			},
			check: func(t *testing.T, _ GetProjectQualityGateStatusResponse, err error) {
				var rpcErr *RPCError
				if !errors.As(err, &rpcErr) {
					t.Fatalf("error = %v, want it to be an *RPCError", err)
				}
				if rpcErr.Code != -32602 {
					t.Errorf("Code = %d, want -32602", rpcErr.Code)
				}
				if !errors.Is(err, ErrServerError) {
					t.Error("errors.Is(err, ErrServerError) = false, want true")
				}
				if errors.Is(err, ErrTransportClosed) {
					t.Error("a jsonrpc error object must not also satisfy a transport-failure sentinel")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c, ft := newHandshakenClient(t)
			ft.onTool(toolGetProjectQualityGateStatus, func(t *testing.T, req sentRequest) ([]byte, error) {
				tt.inject(t, ft, req.ID)
				return nil, nil // reply already pushed by inject; do not also auto-queue one
			})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resp, err := c.GetProjectQualityGateStatus(ctx, GetProjectQualityGateStatusRequest{ProjectKey: "p"})
			tt.check(t, resp, err)
		})
	}
}

// TestClient_UnadvertisedToolRefused proves the never-call-an-unadvertised
// -tool rule: ListTools deliberately omits analyze_code_snippet, and
// calling it must fail locally, with zero tools/call requests reaching the
// Transport.
func TestClient_UnadvertisedToolRefused(t *testing.T) {
	t.Parallel()

	ft := newFakeTransport(t)
	ft.onMethod("initialize", fixtureResponder("initialize_response.json"))
	ft.onMethod("tools/list", fixtureResponder("tools_list_missing_analyze.json"))
	// If CallTool ever sent this, the test would hang instead of failing
	// fast, since no responder for it is registered — but the assertion
	// on sentCount below is what actually proves it was never sent.

	c := NewClient(ft, quietLogger())
	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	_, err := c.AnalyzeCodeSnippet(ctx, AnalyzeCodeSnippetRequest{Language: "go", FileContent: "package x"})
	if !errors.Is(err, ErrToolNotAdvertised) {
		t.Fatalf("err = %v, want ErrToolNotAdvertised", err)
	}
	if n := ft.sentCount("tools/call"); n != 0 {
		t.Errorf("Transport recorded %d tools/call requests, want 0 — the refusal must happen before Send", n)
	}

	// A tool that *was* advertised must still work on the same Client.
	ft.onTool(toolGetProjectQualityGateStatus, fixtureResponder("call_get_project_quality_gate_status.json"))
	if _, err := c.GetProjectQualityGateStatus(ctx, GetProjectQualityGateStatusRequest{ProjectKey: "p"}); err != nil {
		t.Fatalf("GetProjectQualityGateStatus (advertised) failed: %v", err)
	}
}

// TestClient_NotInitializedRefusesCalls proves ListTools and CallTool both
// refuse to run before Initialize has completed, without touching the
// Transport.
func TestClient_NotInitializedRefusesCalls(t *testing.T) {
	t.Parallel()

	ft := newFakeTransport(t)
	c := NewClient(ft, quietLogger())

	if _, err := c.ListTools(context.Background()); !errors.Is(err, ErrNotInitialized) {
		t.Errorf("ListTools before Initialize: err = %v, want ErrNotInitialized", err)
	}
	if _, err := c.CallTool(context.Background(), toolListQualityGates, struct{}{}); !errors.Is(err, ErrNotInitialized) {
		t.Errorf("CallTool before Initialize: err = %v, want ErrNotInitialized", err)
	}
	if n := ft.sentCount(""); n != 0 {
		t.Errorf("Transport recorded %d requests, want 0", n)
	}
}

// TestClient_InitializeRejectsUnsupportedProtocolVersion proves a server
// naming a protocol version this client does not support is refused rather
// than silently accepted.
func TestClient_InitializeRejectsUnsupportedProtocolVersion(t *testing.T) {
	t.Parallel()

	ft := newFakeTransport(t)
	ft.onMethod("initialize", func(t *testing.T, req sentRequest) ([]byte, error) {
		return withID(t, []byte(`{"jsonrpc":"2.0","id":0,"result":{
			"protocolVersion":"1999-01-01",
			"serverInfo":{"name":"ancient-server","version":"0.0.1"},
			"capabilities":{}
		}}`), req.ID), nil
	})

	c := NewClient(ft, quietLogger())
	_, err := c.Initialize(context.Background())
	if !errors.Is(err, ErrUnsupportedProtocolVersion) {
		t.Fatalf("err = %v, want ErrUnsupportedProtocolVersion", err)
	}
	if c.isInitialized() {
		t.Error("isInitialized() = true after a rejected handshake, want false")
	}
}

// TestClient_CancellationMidCall proves a call blocked waiting for a reply
// that never arrives returns promptly once its context is done, rather
// than hanging for the life of the process. StdioTransport's own
// obligation to reap the underlying subprocess on the same event is
// covered separately in transport_test.go, against a real child process.
func TestClient_CancellationMidCall(t *testing.T) {
	t.Parallel()

	c, _ := newHandshakenClient(t)
	// Deliberately no responder for get_project_quality_gate_status: the
	// fake will record the Send and then Recv will simply have nothing to
	// return, exactly like a hung server.

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.GetProjectQualityGateStatus(ctx, GetProjectQualityGateStatusRequest{ProjectKey: "p"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("call took %s to return after a 50ms deadline, want prompt return", elapsed)
	}
}

// TestClient_ConcurrentCallsAreSerialized proves the documented concurrency
// contract: many goroutines may call the same Client at once, but at most
// one JSON-RPC round trip is ever in flight, so a responder can safely
// assume it is never invoked concurrently with itself.
func TestClient_ConcurrentCallsAreSerialized(t *testing.T) {
	t.Parallel()

	c, ft := newHandshakenClient(t)

	var inFlight atomic.Int32
	var sawConcurrency atomic.Bool
	ft.onTool(toolGetProjectQualityGateStatus, func(t *testing.T, req sentRequest) ([]byte, error) {
		if inFlight.Add(1) > 1 {
			sawConcurrency.Store(true)
		}
		defer inFlight.Add(-1)
		time.Sleep(2 * time.Millisecond) // widen the window a race would need
		return withID(t, loadFixture(t, "call_get_project_quality_gate_status.json"), req.ID), nil
	})

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := c.GetProjectQualityGateStatus(context.Background(), GetProjectQualityGateStatusRequest{ProjectKey: "p"})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	if sawConcurrency.Load() {
		t.Error("two calls were in flight at once; Client is documented to serialize every round trip")
	}

	// Every one of the n calls plus the two handshake calls got its own,
	// still-unique id even under concurrent submission.
	ids := map[int64]bool{}
	for _, s := range ft.sent {
		if s.IsNotification {
			continue
		}
		if ids[s.ID] {
			t.Fatalf("id %d reused under concurrent calls", s.ID)
		}
		ids[s.ID] = true
	}
	if len(ids) != n+2 {
		t.Errorf("recorded %d distinct request ids, want %d (2 handshake + %d calls)", len(ids), n+2, n)
	}
}

// TestToolResult_TextVsJSON proves ToolResult picks the right decoding
// strategy per caller: Text() never attempts JSON parsing (GetRawSource's
// payload is source code, which may not even be valid JSON), and JSON()
// prefers structuredContent over the text fallback when both are present.
func TestToolResult_TextVsJSON(t *testing.T) {
	t.Parallel()

	t.Run("Text ignores JSON-shaped content", func(t *testing.T) {
		t.Parallel()
		r := ToolResult{Content: []ContentBlock{{Type: "text", Text: `{"not": "parsed"}`}}}
		if got := r.Text(); got != `{"not": "parsed"}` {
			t.Errorf("Text() = %q, want the raw block content unparsed", got)
		}
	})

	t.Run("JSON prefers structuredContent over text", func(t *testing.T) {
		t.Parallel()
		r := ToolResult{
			Content:           []ContentBlock{{Type: "text", Text: `{"total":999}`}},
			StructuredContent: []byte(`{"total":1}`),
		}
		var out struct {
			Total int `json:"total"`
		}
		if err := r.JSON(&out); err != nil {
			t.Fatalf("JSON: %v", err)
		}
		if out.Total != 1 {
			t.Errorf("Total = %d, want 1 (from structuredContent, not the text fallback)", out.Total)
		}
	})

	t.Run("JSON falls back to text when structuredContent absent", func(t *testing.T) {
		t.Parallel()
		r := ToolResult{Content: []ContentBlock{{Type: "text", Text: `{"total":7}`}}}
		var out struct {
			Total int `json:"total"`
		}
		if err := r.JSON(&out); err != nil {
			t.Fatalf("JSON: %v", err)
		}
		if out.Total != 7 {
			t.Errorf("Total = %d, want 7", out.Total)
		}
	})

	t.Run("JSON reports ErrNoContent with neither", func(t *testing.T) {
		t.Parallel()
		var out struct{}
		err := ToolResult{}.JSON(&out)
		if !errors.Is(err, ErrNoContent) {
			t.Errorf("err = %v, want ErrNoContent", err)
		}
	})
}

// TestClient_ToolExecutionFailure proves a tool that runs but reports its
// own isError:true surfaces as ErrToolExecutionFailed, distinct from both
// *RPCError and a transport failure, while still returning the ToolResult
// for inspection.
func TestClient_ToolExecutionFailure(t *testing.T) {
	t.Parallel()

	c, ft := newHandshakenClient(t)
	ft.onTool(toolAnalyzeCodeSnippet, func(_ *testing.T, req sentRequest) ([]byte, error) {
		frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{
			"content":[{"type":"text","text":"unsupported language: cobol"}],
			"isError":true
		}}`, req.ID)
		return []byte(frame), nil
	})

	result, err := c.CallTool(context.Background(), toolAnalyzeCodeSnippet, AnalyzeCodeSnippetRequest{Language: "cobol"})
	if !errors.Is(err, ErrToolExecutionFailed) {
		t.Fatalf("err = %v, want ErrToolExecutionFailed", err)
	}
	if !result.IsError {
		t.Error("result.IsError = false, want true")
	}
	if !strings.Contains(result.Text(), "cobol") {
		t.Errorf("result.Text() = %q, want the tool's own explanation", result.Text())
	}
}
