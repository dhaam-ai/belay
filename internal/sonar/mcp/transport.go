//go:build unix

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	osexec "os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// SecretEnvName is the environment variable SONAR_TOKEN is read from and,
// for StdioTransport, the name it is set under in the child's environment.
// It is never placed on argv.
const SecretEnvName = "SONAR_TOKEN"

// maxFrameBytes caps a single JSON-RPC frame this package will read, from
// either transport: a generous 16 MiB holds a full source file or a large
// issue-search page, and bounds what a misbehaving peer can make this
// process buffer.
const maxFrameBytes = 16 << 20

// maxStderrBytes caps the redacted tail of a stdio child's stderr kept for
// diagnostics, mirroring the spirit (not the exact size) of
// internal/exec's own output cap: enough to explain why the process died,
// never enough to matter for memory.
const maxStderrBytes = 4 << 10

// Transport is the seam between Client's JSON-RPC logic and the wire.
//
// Client never assumes which Transport it holds. Send transmits one
// complete, already-serialized JSON-RPC frame (a request or a
// notification); Recv blocks for the next inbound frame — a response or a
// notification — until one arrives, the context passed to it is done, or
// the transport is closed. Implementations do not need to support
// concurrent Send/Recv calls: Client serializes every round trip (see its
// doc comment), so at most one Send and one Recv are outstanding on a given
// Transport at a time in normal use.
//
// Close releases whatever the Transport owns — for StdioTransport, the
// child process and its pipes; for HTTPTransport, nothing persistent — and
// must unblock a Recv already in progress. Close is safe to call more than
// once and safe to call concurrently with an in-flight Send or Recv.
type Transport interface {
	// Send transmits frame to the peer. frame is a single, complete
	// JSON-RPC 2.0 message with no trailing delimiter; the Transport adds
	// whatever framing its wire format requires (a newline for stdio, an
	// HTTP request for HTTP).
	Send(ctx context.Context, frame []byte) error

	// Recv returns the next inbound frame: a response to a request this
	// Transport has sent, or a notification. It returns an error wrapping
	// ctx.Err() if ctx is done first, and an error wrapping
	// ErrTransportClosed if the peer is gone and nothing more will ever
	// arrive.
	Recv(ctx context.Context) ([]byte, error)

	// Close releases the Transport's resources. It is safe to call more
	// than once.
	Close() error
}

// ---------------------------------------------------------------------
// HTTPTransport: MCPModeCloud, MCPModeServer.
// ---------------------------------------------------------------------

// HTTPTransport speaks MCP's streamable-HTTP transport: each JSON-RPC
// message is POSTed to a single endpoint URL, and the response — either one
// JSON object or a text/event-stream of them — is queued for Recv. It is
// used for config.Review.AI.MCP.Mode "cloud" (SonarQube Cloud's embedded
// endpoint) and "server" (SonarQube Server 2026.3+'s native <url>/mcp).
//
// Because Client serializes calls (see Client's doc comment), Send always
// performs one full HTTP round trip — request out, response fully read —
// before returning, and whatever frame(s) that round trip produced are
// already queued by the time Recv is called. HTTPTransport therefore has no
// background goroutine and no persistent connection to release; Close is a
// no-op.
//
// The zero value is not usable: URL is required. Client (or Client's
// constructor at the config-integration layer T32 adds) is responsible for
// reading SONAR_TOKEN from the environment and setting it here — this type
// never reads the environment itself.
type HTTPTransport struct {
	// Client performs the HTTP round trip. Nil uses http.DefaultClient.
	Client *http.Client
	// URL is the MCP endpoint to POST every message to. Required.
	URL string
	// Token is sent as "Authorization: Bearer <Token>" on every request
	// when non-empty. Never logged; see redactor.
	Token string
	// Logger receives structured events. Nil uses slog.Default.
	Logger *slog.Logger

	initOnce sync.Once
	red      *exec.Redactor

	mu        sync.Mutex
	sessionID string
	queue     [][]byte
}

var _ Transport = (*HTTPTransport)(nil)

func (t *HTTPTransport) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}

func (t *HTTPTransport) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return http.DefaultClient
}

// redactor lazily builds the Redactor seeded with Token, so a value that
// never carries a token (a test double, a server with auth disabled) never
// pays for one.
func (t *HTTPTransport) redactor() *exec.Redactor {
	t.initOnce.Do(func() {
		var secrets []exec.Secret
		if t.Token != "" {
			secrets = append(secrets, exec.Secret{Label: SecretEnvName, Value: t.Token})
		}
		t.red = exec.NewRedactor(secrets...)
	})
	return t.red
}

// HTTPStatusError reports that the MCP endpoint answered with a non-2xx
// HTTP status. It is a transport-level failure, distinct from *RPCError:
// the request never reached a point where the server could return a
// JSON-RPC error object about it.
type HTTPStatusError struct {
	// StatusCode is the HTTP response status.
	StatusCode int
	// Body is the redacted, truncated response body, if any.
	Body string
}

// Error implements error.
func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("belay/sonar/mcp: http transport: unexpected status %d: %s", e.StatusCode, truncate(e.Body, 500))
}

// Send implements Transport.
func (t *HTTPTransport) Send(ctx context.Context, frame []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(frame))
	if err != nil {
		return fmt.Errorf("belay/sonar/mcp: http transport: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if t.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.Token)
	}
	if sid := t.currentSessionID(); sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := t.client().Do(req)
	if err != nil {
		return fmt.Errorf("belay/sonar/mcp: http transport: %s", t.redactor().Redact(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFrameBytes))
	if err != nil {
		return fmt.Errorf("belay/sonar/mcp: http transport: read response: %s", t.redactor().Redact(err.Error()))
	}

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}

	trimmed := bytes.TrimSpace(body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPStatusError{StatusCode: resp.StatusCode, Body: t.redactor().Redact(string(trimmed))}
	}
	if len(trimmed) == 0 {
		// A notification's POST is acknowledged with an empty body (202
		// Accepted, typically): nothing to queue, and correctly so — a
		// notification has no id for a caller to correlate a reply to.
		return nil
	}

	frames, err := splitHTTPFrames(resp.Header.Get("Content-Type"), trimmed)
	if err != nil {
		return fmt.Errorf("belay/sonar/mcp: http transport: %w", err)
	}
	t.mu.Lock()
	t.queue = append(t.queue, frames...)
	t.mu.Unlock()
	t.logger().LogAttrs(ctx, slog.LevelDebug, "belay/sonar/mcp: http transport sent",
		slog.Int("status", resp.StatusCode), slog.Int("queued_frames", len(frames)))
	return nil
}

// Recv implements Transport.
func (t *HTTPTransport) Recv(_ context.Context) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.queue) == 0 {
		return nil, fmt.Errorf("%w: last request produced no reply", ErrNoFrame)
	}
	frame := t.queue[0]
	t.queue = t.queue[1:]
	return frame, nil
}

// Close implements Transport. HTTPTransport holds no persistent connection,
// so Close is a no-op.
func (t *HTTPTransport) Close() error { return nil }

func (t *HTTPTransport) currentSessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// splitHTTPFrames turns one HTTP response body into the JSON-RPC frames it
// carries: a single frame for "application/json", or one frame per
// server-sent event for "text/event-stream" — streamable-HTTP MCP's two
// documented response shapes. Any other content type is treated as a
// single JSON frame, on the theory that a body byte-identical to a valid
// JSON-RPC message should not be rejected merely for an unexpected
// Content-Type header.
func splitHTTPFrames(contentType string, body []byte) ([][]byte, error) {
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSE(body), nil
	}
	return [][]byte{bytes.Clone(body)}, nil
}

// parseSSE extracts the "data:" payload of every event in a server-sent
// event stream, joining a multi-line event's data fields with "\n" per the
// SSE spec. Non-data fields (event, id, retry, comments) are not needed to
// recover a JSON-RPC payload and are ignored.
func parseSSE(body []byte) [][]byte {
	var frames [][]byte
	var data [][]byte
	flush := func() {
		if len(data) > 0 {
			frames = append(frames, bytes.Join(data, []byte("\n")))
			data = nil
		}
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		switch {
		case len(line) == 0:
			flush()
		case bytes.HasPrefix(line, []byte("data:")):
			d := bytes.TrimPrefix(line, []byte("data:"))
			d = bytes.TrimPrefix(d, []byte(" "))
			data = append(data, d)
		}
	}
	flush()
	return frames
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// ---------------------------------------------------------------------
// StdioTransport: MCPModeDocker.
// ---------------------------------------------------------------------

// StdioCommand describes the child process a StdioTransport starts.
//
// It deliberately does not embed or reuse internal/exec.Command: that type
// describes one bounded, request/response run (it carries a fixed Stdin, a
// Timeout, and a MaxOutput for a single captured result), and none of those
// have a sensible meaning for a process that stays alive across many
// separate tools/call round trips whose number is not known up front. See
// StdioTransport's doc comment for the full reasoning.
type StdioCommand struct {
	// Path is the executable to run — typically "docker". Required.
	Path string
	// Args are the arguments after the program name, for example
	// ["run", "-i", "--rm", "sonarsource/sonar-mcp-server"].
	Args []string
	// Dir is the child's working directory. Empty means this process's.
	Dir string
	// EnvAllow names additional parent environment variables to pass
	// through, beyond exec.BaseEnvNames(). Everything else is dropped.
	EnvAllow []string
	// Token is SONAR_TOKEN's value, set into the child's environment as
	// SecretEnvName and never placed on argv. Empty means no token is
	// passed (an anonymous or trust-the-network deployment).
	Token string
	// Grace is the delay between SIGTERM and SIGKILL when the process
	// group must be terminated. Zero uses exec.DefaultGrace.
	Grace time.Duration
}

// StdioTransport speaks MCP's stdio transport: newline-delimited JSON-RPC
// messages over a long-lived child process's stdin and stdout. It is used
// for config.Review.AI.MCP.Mode "docker".
//
// # Why not internal/exec.Runner
//
// internal/exec.Runner.Run is request/response: it starts a process, feeds
// it one predetermined Stdin, blocks until the process exits, and returns
// everything captured. That shape fits a linter or a test runner perfectly
// — one input, one output, done — but it cannot express an MCP stdio
// session. initialize, tools/list, and an a-priori-unknown number of
// tools/call round trips all share one live process across a Client's
// entire lifetime; the next request is not known until the caller decides
// to make it, which is long after Run would already have to have been
// given every byte of Stdin and already be blocked waiting for exit.
// There is no way to hand Run a growing conversation, because Run does not
// return control to the caller until the child is already dead.
//
// StdioTransport therefore starts and owns its own *os/exec.Cmd rather
// than going through Runner. It deliberately keeps Runner's two
// non-negotiable safety properties rather than treating "not using
// Runner" as license to drop them:
//
//   - Process-group termination. Like Runner, the child is started with
//     Setpgid, so a cancelled call or Close can kill the whole group with
//     SIGTERM, then SIGKILL after a grace period (see terminate). The
//     child here is a `docker run` invocation that itself forks and
//     supervises a container; killing only the direct process would leave
//     the container running as an orphaned cost and, on a shared host, a
//     stray listener.
//   - Deny-by-default environment plus redaction. The child receives only
//     exec.BaseEnvNames() plus StdioCommand.EnvAllow, built directly from
//     this process's own environment lookups rather than by forwarding
//     os.Environ(); SONAR_TOKEN is added explicitly as
//     StdioCommand.Token, by name, never inherited implicitly and never
//     placed on argv. Every error and log line this type produces is
//     passed through an internal/exec.Redactor seeded with that token's
//     value first, the same primitive package exec redacts subprocess
//     output with.
//
// What this type does not reproduce from Runner, deliberately: an overall
// output byte cap (a stdio session is framed message-by-message with its
// own generous per-frame cap, maxFrameBytes, rather than one capped
// lifetime blob) and a fixed run Timeout (a StdioTransport's process
// outlives many calls with independent deadlines; each Send/Recv instead
// honors whatever context its own caller supplies, terminating the shared
// process if that specific call's deadline elapses — see Send and Recv).
type StdioTransport struct {
	// Logger receives structured events. Nil uses slog.Default.
	Logger *slog.Logger

	red   *exec.Redactor
	grace time.Duration

	cmd      *osexec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	stderr   *stderrCapture
	stderrW  *exec.RedactWriter
	waitDone chan struct{}

	writeMu sync.Mutex
	readMu  sync.Mutex

	closed   atomic.Bool
	killOnce sync.Once
}

var _ Transport = (*StdioTransport)(nil)

// NewStdioTransport returns a StdioTransport that logs to logger, or to
// slog.Default when nil. Call Start before Send or Recv.
func NewStdioTransport(logger *slog.Logger) *StdioTransport {
	return &StdioTransport{Logger: logger}
}

func (t *StdioTransport) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}

// Start launches cmd and prepares the transport to Send and Recv against
// it. It returns *belay.ToolchainError (errors.Is belay.ErrToolchainMissing)
// if cmd.Path cannot be found or executed at all — a missing `docker`
// binary is an environment problem a human fixes, not something a review
// retries — and a plain error for anything else that prevents starting.
func (t *StdioTransport) Start(ctx context.Context, cmd StdioCommand) error {
	if strings.TrimSpace(cmd.Path) == "" {
		return errors.New("belay/sonar/mcp: stdio transport: empty command path")
	}
	t.grace = cmd.Grace
	if t.grace <= 0 {
		t.grace = exec.DefaultGrace
	}

	var secrets []exec.Secret
	if cmd.Token != "" {
		secrets = append(secrets, exec.Secret{Label: SecretEnvName, Value: cmd.Token})
	}
	t.red = exec.NewRedactor(secrets...)

	// #nosec G204 -- cmd.Path/cmd.Args are belay's own configuration
	// (config.Review.AI.MCP.Image, resolved by the caller), never data
	// from a reviewed repository, and are never interpreted by a shell:
	// os/exec.Command execs argv directly.
	c := osexec.Command(cmd.Path, cmd.Args...)
	c.Dir = cmd.Dir
	c.Env = buildChildEnv(cmd.EnvAllow, cmd.Token)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := c.StdinPipe()
	if err != nil {
		return fmt.Errorf("belay/sonar/mcp: stdio transport: stdin pipe: %w", err)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return fmt.Errorf("belay/sonar/mcp: stdio transport: stdout pipe: %w", err)
	}
	stderrCap := newStderrCapture(maxStderrBytes)
	stderrW := t.red.Writer(stderrCap)
	c.Stderr = stderrW

	if err := c.Start(); err != nil {
		return classifyStdioStart(t.red, cmd.Path, err)
	}

	t.cmd = c
	t.stdin = stdin
	t.stdout = bufio.NewReaderSize(stdout, 4096)
	t.stderr = stderrCap
	t.stderrW = stderrW
	t.waitDone = make(chan struct{})

	go func() {
		_ = c.Wait()
		_ = stderrW.Close()
		close(t.waitDone)
	}()

	t.logger().LogAttrs(ctx, slog.LevelDebug, "belay/sonar/mcp: stdio transport started",
		slog.String("path", cmd.Path), slog.Int("pid", c.Process.Pid))
	return nil
}

// classifyStdioStart maps a failure to start the child into
// *belay.ToolchainError for a missing/unexecutable binary, or a plain
// redacted error otherwise.
func classifyStdioStart(red *exec.Redactor, tool string, err error) error {
	if errors.Is(err, osexec.ErrNotFound) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return &belay.ToolchainError{Tool: tool, Err: err}
	}
	return fmt.Errorf("belay/sonar/mcp: stdio transport: cannot start %q: %s", tool, red.Redact(err.Error()))
}

// buildChildEnv resolves the child's environment directly from this
// process's own variables — never by forwarding os.Environ() wholesale —
// so the deny-by-default property is enforced by construction rather than
// by remembering to filter.
func buildChildEnv(allow []string, token string) []string {
	names := exec.BaseEnvNames()
	names = append(names, allow...)
	seen := make(map[string]bool, len(names))
	env := make([]string, 0, len(names)+1)
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	if token != "" {
		env = append(env, SecretEnvName+"="+token)
	}
	return env
}

// Send implements Transport: it writes frame followed by a newline to the
// child's stdin. If ctx is done before the write completes, the child's
// whole process group is terminated (see terminate) and ctx.Err() is
// returned — a stuck write almost always means the child itself is stuck,
// and this transport has no way to abandon one write without risking the
// stream's framing for every call after it.
func (t *StdioTransport) Send(ctx context.Context, frame []byte) error {
	if t.closed.Load() {
		return ErrTransportClosed
	}
	line := make([]byte, 0, len(frame)+1)
	line = append(line, frame...)
	line = append(line, '\n')

	done := make(chan error, 1)
	go func() {
		t.writeMu.Lock()
		defer t.writeMu.Unlock()
		_, err := t.stdin.Write(line)
		done <- err
	}()

	select {
	case <-ctx.Done():
		t.terminate(ctx.Err())
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return t.classifyIOErr(err)
		}
		return nil
	}
}

// Recv implements Transport: it reads the next newline-delimited frame
// from the child's stdout. If ctx is done before a full frame arrives, the
// child's whole process group is terminated and ctx.Err() is returned, for
// the same reason as Send: there is no safe way to un-block a pending read
// without ending the session it belongs to.
func (t *StdioTransport) Recv(ctx context.Context) ([]byte, error) {
	if t.closed.Load() {
		return nil, ErrTransportClosed
	}
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		t.readMu.Lock()
		defer t.readMu.Unlock()
		line, err := readFrame(t.stdout)
		ch <- result{line, err}
	}()

	select {
	case <-ctx.Done():
		t.terminate(ctx.Err())
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, t.classifyIOErr(r.err)
		}
		return r.line, nil
	}
}

// classifyIOErr turns a raw pipe error into ErrTransportClosed (wrapping
// io.EOF and, when available, the child's redacted stderr tail) or a plain
// redacted error for anything else.
func (t *StdioTransport) classifyIOErr(err error) error {
	if errors.Is(err, io.EOF) {
		// Stdout hitting EOF and the child's exit being fully reaped are
		// two different pipes racing each other: the redacted stderr tail
		// is only guaranteed complete once Start's goroutine has called
		// c.Wait() (os/exec waits for its internal stderr-copying
		// goroutine too) and then closed the RedactWriter, which is what
		// flushes a short tail sitting inside the writer's overlap
		// hold-back buffer. Wait briefly for that before reading it,
		// rather than risking an empty or truncated tail read out from
		// under a Close that has not run yet.
		select {
		case <-t.waitDone:
		case <-time.After(2 * time.Second):
		}
		if tail := t.stderr.String(); tail != "" {
			return fmt.Errorf("%w: mcp server exited: %s", ErrTransportClosed, tail)
		}
		return fmt.Errorf("%w: %w", ErrTransportClosed, err)
	}
	return fmt.Errorf("belay/sonar/mcp: stdio transport: %s", t.red.Redact(err.Error()))
}

// Close implements Transport: it terminates the child's process group (if
// still running) and closes stdin. Close is safe to call more than once
// and unblocks any Send or Recv in progress by way of terminate closing the
// pipes out from under them.
func (t *StdioTransport) Close() error {
	if t.closed.Swap(true) {
		return nil
	}
	if t.cmd != nil && t.cmd.Process != nil {
		t.terminate(ErrTransportClosed)
	}
	if t.stdin != nil {
		_ = t.stdin.Close()
	}
	return nil
}

// terminate escalates SIGTERM to SIGKILL across the child's whole process
// group, mirroring internal/exec's terminateGroup: the negative pid
// targets the group Setpgid created, so a `docker run` process's own
// supervised container dies with it rather than being orphaned. It runs at
// most once per StdioTransport.
func (t *StdioTransport) terminate(reason error) {
	t.killOnce.Do(func() {
		if t.cmd == nil || t.cmd.Process == nil {
			return
		}
		pid := t.cmd.Process.Pid
		signalGroup(pid, syscall.SIGTERM)
		select {
		case <-t.waitDone:
			return
		case <-time.After(t.grace):
		}
		signalGroup(pid, syscall.SIGKILL)
		<-t.waitDone
		msg := "context canceled"
		if reason != nil {
			msg = t.red.Redact(reason.Error())
		}
		t.logger().LogAttrs(context.Background(), slog.LevelWarn,
			"belay/sonar/mcp: terminated mcp server process group",
			slog.Int("pgid", pid), slog.String("reason", msg))
	})
}

func signalGroup(pid int, sig syscall.Signal) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, sig); err == nil {
		return
	}
	_ = syscall.Kill(pid, sig)
}

// readFrame reads one newline-delimited JSON-RPC frame from r, trimming
// the trailing "\r\n" or "\n". It bounds accumulated bytes at
// maxFrameBytes so a peer that never sends a newline cannot grow memory
// without limit.
func readFrame(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == nil {
			return bytes.TrimRight(buf, "\r\n"), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(buf) > maxFrameBytes {
				return nil, fmt.Errorf("%w: frame exceeds %d bytes", ErrMalformedFrame, maxFrameBytes)
			}
			continue
		}
		return nil, err
	}
}

// stderrCapture accumulates a bounded, already-redacted tail of a stdio
// child's standard error for diagnostics. It is deliberately much simpler
// than internal/exec's capture: a stdio MCP server's stderr is a debug log,
// not a result to preserve byte-for-byte, so silently stopping at the cap
// (no drop counter, no truncation marker) is an acceptable loss.
type stderrCapture struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func newStderrCapture(limit int) *stderrCapture {
	return &stderrCapture{limit: limit}
}

// Write implements io.Writer. It always reports success and never blocks
// past filling its bounded buffer, so a chatty child can never be stalled
// by this capture refusing bytes.
func (c *stderrCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.limit - len(c.buf); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
	}
	return len(p), nil
}

// String returns the captured tail, whitespace-trimmed.
func (c *stderrCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(string(c.buf))
}
