//go:build unix

package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// No test in this package contacts a real SonarQube server, starts Docker,
// or makes a network call.
//
//   - Every Client behavior (Initialize, ListTools, CallTool, JSON-RPC
//     correlation, cancellation) is driven against fakeTransport below, an
//     in-memory Transport that starts no process and opens no socket.
//   - StdioTransport's real-subprocess behavior (process-group kill,
//     stderr capture and redaction) is exercised by re-executing this test
//     binary as the child, exactly the technique internal/exec and
//     internal/agent/claude use for the same reason: a real process and a
//     real pipe, with no external toolchain and no cost.
//   - HTTPTransport's real-HTTP behavior is exercised against an
//     httptest.Server, which binds to loopback (127.0.0.1) and never
//     leaves the machine.
//
// See the acceptance report for how this was verified (a grep for osexec/
// http.Get-style calls to non-loopback, non-self-reexec targets across the
// test files, plus the fact that `docker` and a real SonarQube URL are
// simply never referenced).
const helperModeEnv = "BELAY_MCP_HELPER"

// TestMain dispatches to helperMain whenever this binary is re-executed as
// a StdioTransport test's child.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnv); mode != "" {
		helperMain(mode)
		return
	}
	os.Exit(m.Run())
}

// helperMain impersonates an MCP stdio server for the tests that need a
// real child process rather than fakeTransport.
func helperMain(mode string) {
	switch mode {
	case "echo-line":
		// Reads one newline-delimited line and writes it straight back,
		// repeatedly: proves Send/Recv frame over a real pipe correctly,
		// independent of any JSON-RPC semantics.
		r := bufio.NewReader(os.Stdin)
		for {
			line, err := r.ReadString('\n')
			if len(line) > 0 {
				fmt.Print(line)
				if !stringsHasSuffix(line, "\n") {
					fmt.Println()
				}
			}
			if err != nil {
				return
			}
		}
	case "hang":
		// Never writes anything back; used to prove a cancelled Recv
		// terminates the process group promptly under ordinary signal
		// handling (SIGTERM is enough).
		select {}
	case "stall":
		// Ignores SIGTERM/SIGINT, so only SIGKILL can end it: proves the
		// grace-period escalation in StdioTransport.terminate.
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		time.Sleep(10 * time.Minute)
	case "leak-token":
		// Simulates a bridge or container that carelessly logs a
		// credential from its own environment to stderr, then exits
		// immediately (closing stdout), which is what drives the parent's
		// Recv to io.EOF and into classifyIOErr's stderr-tail path.
		fmt.Fprintf(os.Stderr, "debug: SONAR_TOKEN=%s\n", os.Getenv(SecretEnvName))
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

func stringsHasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// testExe returns the absolute path of this test binary, used as the
// StdioTransport child in transport_test.go.
func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// quietLogger discards everything, so a failing test reports an assertion
// rather than a wall of structured output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// processAlive reports whether pid still exists.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// waitGone polls until pid disappears or the deadline passes, and reports
// whether it is gone.
func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !processAlive(pid)
}

// loadFixture reads a hand-authored testdata file.
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

// withID re-serializes a hand-authored fixture frame (authored with a
// placeholder "id":0) with id replaced by id, so one fixture can answer
// whichever request a test's Client actually sent, regardless of exactly
// where in a call sequence it landed.
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
	Raw            []byte
}

// toolCallArgs decodes the "name" field of a tools/call request's params,
// which is how fakeTransport's per-tool responders dispatch.
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

// responder builds a reply frame for one sent request, or returns a
// transport-level error instead of a reply.
type responder func(t *testing.T, req sentRequest) (reply []byte, err error)

// fixtureResponder returns a responder that answers with fixture, its id
// rewritten to match the request.
func fixtureResponder(name string) responder {
	return func(t *testing.T, req sentRequest) ([]byte, error) {
		return withID(t, loadFixture(t, name), req.ID), nil
	}
}

// fakeTransport is the in-memory Transport every Client-level test in this
// package uses. It starts no process and makes no network call: Send
// decodes and records the outbound frame, then — for a request, never a
// notification — looks up a responder by JSON-RPC method (and, for
// "tools/call", by tool name) to synthesize the reply, which Recv then
// serves. A raw frame can also be injected directly via push, for the
// malformed-frame / unknown-id / interleaved-notification table tests that
// are not about what a well-behaved server sends back.
type fakeTransport struct {
	t *testing.T

	mu     sync.Mutex
	sent   []sentRequest
	queue  [][]byte
	closed bool

	// byMethod answers non-"tools/call" requests, keyed by method.
	byMethod map[string]responder
	// byTool answers "tools/call" requests, keyed by the tool name inside
	// params.
	byTool map[string]responder
}

func newFakeTransport(t *testing.T) *fakeTransport {
	t.Helper()
	return &fakeTransport{
		t:        t,
		byMethod: map[string]responder{},
		byTool:   map[string]responder{},
	}
}

// onMethod registers r to answer every request for method.
func (f *fakeTransport) onMethod(method string, r responder) {
	f.byMethod[method] = r
}

// onTool registers r to answer every tools/call request naming tool.
func (f *fakeTransport) onTool(tool string, r responder) {
	f.byTool[tool] = r
}

// push injects a raw frame directly into the inbox, bypassing every
// responder — for tests that need to hand Recv a specific malformed,
// unrecognized-id, or notification frame regardless of what was sent.
func (f *fakeTransport) push(frame []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, frame)
}

// sentCount reports how many requests (excluding notifications) fakeTransport
// has recorded for method, or every method if method is "".
func (f *fakeTransport) sentCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if method == "" {
		return len(f.sent)
	}
	n := 0
	for _, s := range f.sent {
		if s.Method == method {
			n++
		}
	}
	return n
}

// lastSent returns the most recently recorded outbound frame.
func (f *fakeTransport) lastSent() sentRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		f.t.Fatal("fakeTransport: no request has been sent yet")
	}
	return f.sent[len(f.sent)-1]
}

// Send implements Transport.
func (f *fakeTransport) Send(_ context.Context, frame []byte) error {
	var probe struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		f.t.Fatalf("fakeTransport.Send: outbound frame is not valid JSON-RPC: %v", err)
	}
	req := sentRequest{Method: probe.Method, Params: probe.Params, Raw: frame, IsNotification: len(probe.ID) == 0}
	if !req.IsNotification {
		if err := json.Unmarshal(probe.ID, &req.ID); err != nil {
			f.t.Fatalf("fakeTransport.Send: outbound id is not an integer: %v", err)
		}
	}

	f.mu.Lock()
	f.sent = append(f.sent, req)
	f.mu.Unlock()

	if req.IsNotification {
		return nil
	}

	var r responder
	var ok bool
	if req.Method == "tools/call" {
		r, ok = f.byTool[req.toolName(f.t)]
	} else {
		r, ok = f.byMethod[req.Method]
	}
	if !ok {
		return nil // no scripted reply; Recv will block on ctx (cancellation tests)
	}
	reply, err := r(f.t, req)
	if err != nil {
		return err
	}
	if reply != nil {
		f.push(reply)
	}
	return nil
}

// Recv implements Transport.
func (f *fakeTransport) Recv(ctx context.Context) ([]byte, error) {
	for {
		f.mu.Lock()
		if len(f.queue) > 0 {
			frame := f.queue[0]
			f.queue = f.queue[1:]
			f.mu.Unlock()
			return frame, nil
		}
		closed := f.closed
		f.mu.Unlock()
		if closed {
			return nil, ErrTransportClosed
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
			// Poll briefly rather than blocking on a dedicated wake
			// channel: simple, and every test's queue is either
			// already populated by the time Recv is first called
			// (Send is synchronous above) or deliberately left empty
			// to exercise cancellation.
		}
	}
}

// Close implements Transport.
func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// newHandshakenClient returns a Client wired to a fresh fakeTransport with
// a default, all-five-tools initialize+tools/list already scripted, and
// performs both calls so the returned Client is ready for CallTool. It is
// the shared setup for every test that does not care about the handshake
// itself.
func newHandshakenClient(t *testing.T) (*Client, *fakeTransport) {
	t.Helper()
	ft := newFakeTransport(t)
	ft.onMethod("initialize", fixtureResponder("initialize_response.json"))
	ft.onMethod("tools/list", fixtureResponder("tools_list_response.json"))

	c := NewClient(ft, quietLogger())
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	return c, ft
}
