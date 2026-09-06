//go:build unix

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// ---------------------------------------------------------------------
// HTTPTransport: exercised against httptest.Server, which binds to
// loopback (127.0.0.1) and never leaves the machine — no real network call
// and no dependency on any actual SonarQube endpoint.
// ---------------------------------------------------------------------

// TestHTTPTransport_SendsBearerTokenAndDecodesJSON proves the basic
// request shape: the token as a Bearer Authorization header, and a plain
// application/json response decoded into exactly one queued frame.
func TestHTTPTransport_SendsBearerTokenAndDecodesJSON(t *testing.T) {
	t.Parallel()

	const token = "squ_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" //nolint:gosec // test fixture value, not a real credential.
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer srv.Close()

	tr := &HTTPTransport{URL: srv.URL, Token: token, Logger: quietLogger()}
	if err := tr.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if want := "Bearer " + token; gotAuth != want {
		t.Errorf("Authorization header = %q, want %q", gotAuth, want)
	}

	frame, err := tr.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if !bytes.Contains(frame, []byte(`"ok":true`)) {
		t.Errorf("frame = %s, want it to carry the response result", frame)
	}

	if _, err := tr.Recv(context.Background()); !errors.Is(err, ErrNoFrame) {
		t.Errorf("second Recv with an empty queue: err = %v, want ErrNoFrame", err)
	}
}

// TestHTTPTransport_SSEResponse proves a text/event-stream response is
// split into one frame per "data:" event and returned from Recv in order —
// the shape a streamable-HTTP MCP server uses when a request's response
// carries more than one message (a progress notification, then the
// result).
func TestHTTPTransport_SSEResponse(t *testing.T) {
	t.Parallel()

	body := "event: message\n" +
		"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"pct\":50}}\n\n" +
		"event: message\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"done\":true}}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	tr := &HTTPTransport{URL: srv.URL, Logger: quietLogger()}
	if err := tr.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call"}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	first, err := tr.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv 1: %v", err)
	}
	if !bytes.Contains(first, []byte("notifications/progress")) {
		t.Errorf("first frame = %s, want the progress notification first", first)
	}
	second, err := tr.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv 2: %v", err)
	}
	if !bytes.Contains(second, []byte(`"done":true`)) {
		t.Errorf("second frame = %s, want the result second", second)
	}
}

// TestHTTPTransport_NotificationGetsNoQueuedFrame proves that posting a
// notification (which this package never expects a reply to) and getting
// back an empty-bodied acknowledgement queues nothing for Recv.
func TestHTTPTransport_NotificationGetsNoQueuedFrame(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	tr := &HTTPTransport{URL: srv.URL, Logger: quietLogger()}
	if err := tr.Send(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := tr.Recv(context.Background()); !errors.Is(err, ErrNoFrame) {
		t.Errorf("Recv after a notification: err = %v, want ErrNoFrame", err)
	}
}

// TestHTTPTransport_SessionIDPropagated proves a server-assigned
// Mcp-Session-Id is captured and echoed back on the next request.
func TestHTTPTransport_SessionIDPropagated(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Mcp-Session-Id", "sess-123")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		if got := r.Header.Get("Mcp-Session-Id"); got != "sess-123" {
			t.Errorf("second request Mcp-Session-Id = %q, want sess-123", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{}}`))
	}))
	defer srv.Close()

	tr := &HTTPTransport{URL: srv.URL, Logger: quietLogger()}
	if err := tr.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if _, err := tr.Recv(context.Background()); err != nil {
		t.Fatalf("Recv 1: %v", err)
	}
	if err := tr.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("server saw %d calls, want 2", calls)
	}
}

// TestHTTPTransport_ErrorStatusRedactsToken proves that if a broken or
// misconfigured MCP endpoint ever echoes something containing the token
// back in an error body, the token is redacted before it can reach an
// error message a log statement or a returned error might carry.
func TestHTTPTransport_ErrorStatusRedactsToken(t *testing.T) {
	t.Parallel()

	const token = "squ_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" //nolint:gosec // test fixture value, not a real credential.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, "invalid credentials: token %s was rejected", token)
	}))
	defer srv.Close()

	tr := &HTTPTransport{URL: srv.URL, Token: token, Logger: quietLogger()}
	err := tr.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err == nil {
		t.Fatal("Send returned nil error for a 401 response, want an error")
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v (%T), want *HTTPStatusError", err, err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(statusErr.Body, token) {
		t.Fatalf("token leaked into error: %v", err)
	}
	if !strings.Contains(statusErr.Body, "REDACTED") {
		t.Errorf("Body = %q, want a redaction marker in place of the token", statusErr.Body)
	}
}

// ---------------------------------------------------------------------
// StdioTransport: exercised against a re-exec of this test binary (see
// helper_test.go's TestMain/helperMain), never Docker and never a real
// SonarQube MCP server.
// ---------------------------------------------------------------------

// startEchoTransport starts a StdioTransport whose child is this test
// binary re-executed in "echo-line" mode, using StdioCommand.EnvAllow to
// pass the dispatch variable through belay's own deny-by-default
// environment path (T.Setenv makes it visible to buildChildEnv exactly
// like a real deployment's SONAR_TOKEN would be).
func startEchoTransport(t *testing.T, mode string, grace time.Duration) (*StdioTransport, int) {
	t.Helper()
	t.Setenv(helperModeEnv, mode)

	tr := NewStdioTransport(quietLogger())
	if err := tr.Start(context.Background(), StdioCommand{
		Path:     testExe(t),
		EnvAllow: []string{helperModeEnv},
		Grace:    grace,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return tr, tr.cmd.Process.Pid
}

func TestStdioTransport_EchoRoundTrip(t *testing.T) {

	tr, _ := startEchoTransport(t, "echo-line", 200*time.Millisecond)
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := tr.Send(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	frame, err := tr.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if string(frame) != `{"jsonrpc":"2.0","id":1,"method":"ping"}` {
		t.Errorf("frame = %s, want the echoed request", frame)
	}
}

// TestStdioTransport_CancellationReapsSubprocess proves the acceptance
// requirement head-on: a Recv whose context is cancelled mid-call
// terminates the child's whole process group promptly, even when the
// child ignores SIGTERM and only SIGKILL can end it.
func TestStdioTransport_CancellationReapsSubprocess(t *testing.T) {

	tr, pid := startEchoTransport(t, "stall", 100*time.Millisecond)
	t.Cleanup(func() { _ = tr.Close() })

	if !processAlive(pid) {
		t.Fatal("child process did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := tr.Recv(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Recv err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("Recv took %s to return, want prompt return", elapsed)
	}
	if !waitGone(pid, 2*time.Second) {
		t.Fatalf("child pid %d survived cancellation: process group was not reaped", pid)
	}
}

// TestStdioTransport_CloseReapsSubprocess proves Close alone (no pending
// call) also terminates a still-running child.
func TestStdioTransport_CloseReapsSubprocess(t *testing.T) {

	tr, pid := startEchoTransport(t, "hang", 200*time.Millisecond)
	if !processAlive(pid) {
		t.Fatal("child process did not start")
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !waitGone(pid, 2*time.Second) {
		t.Fatalf("child pid %d survived Close", pid)
	}
	// Close is safe to call more than once.
	if err := tr.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestStdioTransport_TokenRedactedFromStderr proves requirement 5 for the
// stdio path end to end: a child that carelessly logs SONAR_TOKEN to
// stderr and exits must never let that value reach the error this
// transport returns.
func TestStdioTransport_TokenRedactedFromStderr(t *testing.T) {

	const token = "squ_ccccccccccccccccccccccccccccccccccccccccc" //nolint:gosec // test fixture value, not a real credential.
	t.Setenv(helperModeEnv, "leak-token")

	tr := NewStdioTransport(quietLogger())
	if err := tr.Start(context.Background(), StdioCommand{
		Path:     testExe(t),
		EnvAllow: []string{helperModeEnv},
		Token:    token,
		Grace:    200 * time.Millisecond,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	// The helper prints the token to stderr and exits immediately, closing
	// stdout; Recv (reading stdout) observes that as EOF and surfaces the
	// captured, redacted stderr tail via classifyIOErr.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := tr.Recv(ctx)
	if err == nil {
		t.Fatal("Recv returned nil error after the child exited, want an error")
	}
	if !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("err = %v, want it to wrap ErrTransportClosed", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Errorf("err = %v, want a redaction marker where the token was", err)
	}
}

// TestStdioTransport_MissingBinary proves a nonexistent program path is
// reported as a toolchain problem (errors.Is belay.ErrToolchainMissing),
// not a generic start failure — the same distinction internal/linter and
// internal/agent/claude make at their own subprocess boundary, so a
// Reviewer built on this package can route "install docker" to a human and
// everything else to a retry.
func TestStdioTransport_MissingBinary(t *testing.T) {
	t.Parallel()

	tr := NewStdioTransport(quietLogger())
	err := tr.Start(context.Background(), StdioCommand{Path: "belay-mcp-definitely-does-not-exist"})
	if err == nil {
		t.Fatal("Start returned nil error for a missing binary")
	}
	if !errors.Is(err, belay.ErrToolchainMissing) {
		t.Fatalf("err = %v, want errors.Is(err, belay.ErrToolchainMissing)", err)
	}
	var toolErr *belay.ToolchainError
	if !errors.As(err, &toolErr) {
		t.Fatalf("err = %v (%T), want *belay.ToolchainError", err, err)
	}
	if toolErr.Tool != "belay-mcp-definitely-does-not-exist" {
		t.Errorf("ToolchainError.Tool = %q, want the missing path", toolErr.Tool)
	}
}

// ---------------------------------------------------------------------
// readFrame: the stdio line-framing helper, tested directly against the
// hand-authored truncated-stream fixture.
// ---------------------------------------------------------------------

// TestReadFrame_TruncatedStream proves a stream that ends mid-message
// (the child died or the pipe closed before a newline arrived) surfaces a
// clean io.EOF rather than fabricating a frame out of the partial bytes or
// hanging.
func TestReadFrame_TruncatedStream(t *testing.T) {
	t.Parallel()

	raw := loadFixture(t, "truncated_stream.txt")
	r := bufio.NewReader(bytes.NewReader(raw))
	_, err := readFrame(r)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("readFrame(truncated stream) err = %v, want io.EOF", err)
	}
}

// TestReadFrame_WellFormedLine proves the ordinary case: exactly one
// trailing newline is stripped and nothing else is disturbed.
func TestReadFrame_WellFormedLine(t *testing.T) {
	t.Parallel()

	r := bufio.NewReader(strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n"))
	frame, err := readFrame(r)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(frame) != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Errorf("frame = %s, want the line without its trailing newline", frame)
	}
}
