//go:build unix

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// DefaultClientName and DefaultClientVersion are reported to the server
// during initialize when Client.Name / Client.ClientVersion are empty.
const (
	DefaultClientName    = "belay"
	DefaultClientVersion = "dev"
)

// ServerInfo names an MCP peer — either the server, in an initialize
// response, or this client, in an initialize request.
type ServerInfo struct {
	// Name is the peer's implementation name, such as "sonar-mcp-server".
	Name string `json:"name"`
	// Version is the peer's implementation version string.
	Version string `json:"version"`
}

// InitializeResult is the server's answer to the initialize handshake.
type InitializeResult struct {
	// ProtocolVersion is the MCP protocol version the server has agreed to
	// speak. Client.Initialize rejects any value not present in
	// SupportedProtocolVersions before returning.
	ProtocolVersion string `json:"protocolVersion"`
	// ServerInfo names the server implementation.
	ServerInfo ServerInfo `json:"serverInfo"`
	// Capabilities is the server's capabilities object, preserved
	// unparsed: this package only ever calls tools, so it has no need to
	// model every capability a server might advertise, and a future
	// server version adding one must not break decoding.
	Capabilities json.RawMessage `json:"capabilities"`
	// Raw is the exact bytes of the initialize result, for logging or a
	// caller that wants more than this package models.
	Raw json.RawMessage `json:"-"`
}

// initializeParams is the wire shape of an initialize request.
type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      ServerInfo     `json:"clientInfo"`
}

// ToolDescriptor is one entry of a tools/list response: the name Client.
// CallTool matches against, plus the description and input schema a caller
// may want for logging or validation. Client keeps only Name for its own
// "was this advertised" check; Description and InputSchema are carried
// through for a caller that wants them.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// listToolsResult is the wire shape of a tools/list response.
type listToolsResult struct {
	Tools []ToolDescriptor `json:"tools"`
}

// ContentBlock is one block of a tools/call result's content array. This
// package's five tools (tools.go) only ever produce "text" blocks; a block
// of any other MCP content type (image, audio, an embedded resource) is
// preserved with Type set and Text left empty, so a caller can detect and
// skip it rather than silently misreading an empty Text as an empty text
// block.
type ContentBlock struct {
	Type string
	Text string
}

// contentBlockWire is ContentBlock's wire shape.
type contentBlockWire struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// toolCallParams is the wire shape of a tools/call request.
type toolCallParams struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments,omitempty"`
}

// toolCallResultWire is the wire shape of a tools/call response.
type toolCallResultWire struct {
	Content           []contentBlockWire `json:"content"`
	IsError           bool               `json:"isError"`
	StructuredContent json.RawMessage    `json:"structuredContent,omitempty"`
}

// ToolResult is the decoded result of one tools/call, generic across all
// five tools this package wraps: each typed wrapper in tools.go decodes its
// own response shape out of the same envelope via JSON or Text.
type ToolResult struct {
	// Content is every content block the tool returned, most commonly a
	// single "text" block. Never nil.
	Content []ContentBlock
	// IsError reports the tool's own isError flag: the JSON-RPC call
	// succeeded, but the tool determined its own operation failed.
	IsError bool
	// StructuredContent is the tool's machine-readable result, when the
	// server sent one, verbatim.
	StructuredContent json.RawMessage
	// Raw is the exact bytes of the tools/call result.
	Raw json.RawMessage
}

// Text concatenates every "text" content block's Text, in order, joined by
// "\n". It is what GetRawSource uses to recover source code, which is not
// JSON and must not be parsed as such.
func (r ToolResult) Text() string {
	var parts []string
	for _, b := range r.Content {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// JSON decodes this result's structured payload into v. It prefers
// StructuredContent — the location the MCP spec (2025-06-18) defines for a
// tool's machine-readable output — and falls back to parsing the first
// non-empty "text" content block as JSON. The fallback matters in
// practice: the spec recommends that a tool with an output schema *also*
// emit its structured result serialized as a text block, for clients that
// predate structuredContent, and this package cannot assume every
// SonarQube MCP server deployment it talks to is new enough to send the
// former at all. It returns an error wrapping ErrNoContent if neither is
// present.
func (r ToolResult) JSON(v any) error {
	if len(r.StructuredContent) > 0 {
		return json.Unmarshal(r.StructuredContent, v)
	}
	for _, b := range r.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return json.Unmarshal([]byte(b.Text), v)
		}
	}
	return ErrNoContent
}

// Client speaks MCP's JSON-RPC 2.0 protocol over a Transport: it performs
// the initialize handshake, discovers the tools a server advertises via
// tools/list, and drives tools/call for the five SonarQube tools tools.go
// wraps. Client owns no transport-specific behavior — HTTPTransport and
// StdioTransport are fully interchangeable beneath it, which is what makes
// every Client behavior in this package testable against an in-memory fake
// with no subprocess and no network call; see helper_test.go's
// fakeTransport.
//
// # Concurrency
//
// A *Client is safe for concurrent use, but every JSON-RPC round trip —
// Initialize, ListTools, or one CallTool (and so one of the five typed
// wrappers in tools.go) — is fully serialized by an internal mutex. A
// second goroutine calling a method while one is in flight blocks until
// the first completes; it does not corrupt Client's state and does not
// race with it, it simply waits its turn.
//
// This is a deliberate simplification, stated rather than left for a
// caller to discover under load. Pipelining multiple in-flight JSON-RPC
// requests over one connection is possible in principle — each carries its
// own id, and mcp.go's decodeEnvelope already tolerates a response arriving
// out of order — but StdioTransport's single pipe makes concurrent writers
// a real risk of interleaved, corrupted frames, and doing that safely
// (per-request response channels, a background reader goroutine, careful
// notification fan-out to whichever caller is still waiting) buys
// throughput that belay's own use of this package does not need: an
// MCP-backed Reviewer issues one short, strictly sequential chain of calls
// per review (initialize once, tools/list once, then a handful of
// tools/call requests), not a high-throughput stream. A caller that wants
// several reviews running at once should give each its own Client — and,
// for MCPModeDocker, its own subprocess — rather than share one, exactly
// how belay's fanout already isolates one candidate's workspace, test run
// and review from its siblings.
type Client struct {
	// Transport is the underlying wire connection. Required.
	Transport Transport
	// Logger receives structured events. Nil uses slog.Default.
	Logger *slog.Logger
	// ClientName and ClientVersion are reported to the server during
	// initialize. Empty uses DefaultClientName / DefaultClientVersion.
	ClientName    string
	ClientVersion string

	mu          sync.Mutex // serializes every JSON-RPC round trip
	nextID      atomic.Int64
	initialized bool
	protocol    string
	serverInfo  ServerInfo
	tools       map[string]ToolDescriptor // nil until ListTools succeeds
}

// NewClient returns a Client speaking t, logging to logger (or
// slog.Default when nil).
func NewClient(t Transport, logger *slog.Logger) *Client {
	return &Client{Transport: t, Logger: logger}
}

func (c *Client) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c *Client) clientInfo() ServerInfo {
	name, version := c.ClientName, c.ClientVersion
	if name == "" {
		name = DefaultClientName
	}
	if version == "" {
		version = DefaultClientVersion
	}
	return ServerInfo{Name: name, Version: version}
}

// call sends one JSON-RPC request and blocks for its correlated response.
// It is the single place this package implements request/response
// correlation, malformed-frame recovery, and notification/unknown-id
// handling; Initialize, ListTools and CallTool all go through it.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID.Add(1)
	raw, err := json.Marshal(rpcRequest{JSONRPC: jsonRPCVersion, ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("belay/sonar/mcp: encode %s request: %w", method, err)
	}
	if err := c.Transport.Send(ctx, raw); err != nil {
		return nil, fmt.Errorf("belay/sonar/mcp: send %s: %w", method, err)
	}

	for {
		frame, err := c.Transport.Recv(ctx)
		if err != nil {
			return nil, fmt.Errorf("belay/sonar/mcp: recv reply to %s: %w", method, err)
		}
		env, err := decodeEnvelope(frame)
		if err != nil {
			c.logger().LogAttrs(ctx, slog.LevelWarn, "belay/sonar/mcp: dropping malformed frame while awaiting reply",
				slog.String("method", method), slog.String("error", err.Error()))
			continue
		}
		switch {
		case env.isNotification():
			c.logger().LogAttrs(ctx, slog.LevelDebug, "belay/sonar/mcp: received notification while awaiting reply",
				slog.String("awaited_method", method), slog.String("notification_method", env.Method))
			continue
		case env.isPeerRequest():
			c.logger().LogAttrs(ctx, slog.LevelWarn, "belay/sonar/mcp: dropping unsupported server-initiated request",
				slog.String("method", env.Method))
			continue
		case !env.matchID(id):
			c.logger().LogAttrs(ctx, slog.LevelWarn, "belay/sonar/mcp: dropping response with unrecognized id",
				slog.String("awaited_method", method), slog.Int64("want_id", id), slog.String("got_id", string(env.ID)))
			continue
		}
		if env.Error != nil {
			return nil, &RPCError{Code: env.Error.Code, Message: env.Error.Message, Data: env.Error.Data}
		}
		return env.Result, nil
	}
}

// notify sends a JSON-RPC notification: fire and forget, no reply awaited.
func (c *Client) notify(ctx context.Context, method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	raw, err := json.Marshal(rpcNotification{JSONRPC: jsonRPCVersion, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("belay/sonar/mcp: encode %s notification: %w", method, err)
	}
	if err := c.Transport.Send(ctx, raw); err != nil {
		return fmt.Errorf("belay/sonar/mcp: send %s: %w", method, err)
	}
	return nil
}

// Initialize performs the MCP handshake: it sends initialize, validates
// that the server's chosen protocol version is one Client supports, sends
// the notifications/initialized notification the spec requires before any
// further call, and only then marks Client ready for ListTools and
// CallTool. No tool is ever called before this method has returned nil.
func (c *Client) Initialize(ctx context.Context) (InitializeResult, error) {
	result, err := c.call(ctx, "initialize", initializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      c.clientInfo(),
	})
	if err != nil {
		return InitializeResult{}, err
	}

	var parsed InitializeResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		return InitializeResult{}, fmt.Errorf("%w: initialize result: %w", ErrMalformedFrame, err)
	}
	parsed.Raw = result

	if !slices.Contains(SupportedProtocolVersions, parsed.ProtocolVersion) {
		return parsed, fmt.Errorf("%w: server offered %q, client supports %v",
			ErrUnsupportedProtocolVersion, parsed.ProtocolVersion, SupportedProtocolVersions)
	}

	c.mu.Lock()
	c.initialized = true
	c.protocol = parsed.ProtocolVersion
	c.serverInfo = parsed.ServerInfo
	c.mu.Unlock()

	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return parsed, fmt.Errorf("belay/sonar/mcp: send initialized notification: %w", err)
	}

	c.logger().LogAttrs(ctx, slog.LevelInfo, "belay/sonar/mcp: initialized",
		slog.String("protocol_version", parsed.ProtocolVersion),
		slog.String("server_name", parsed.ServerInfo.Name),
		slog.String("server_version", parsed.ServerInfo.Version))
	return parsed, nil
}

func (c *Client) isInitialized() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initialized
}

// ListTools calls tools/list and records which tool names the server
// advertised, gating every later CallTool. It returns ErrNotInitialized if
// called before Initialize has completed successfully.
func (c *Client) ListTools(ctx context.Context) ([]ToolDescriptor, error) {
	if !c.isInitialized() {
		return nil, ErrNotInitialized
	}
	result, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var parsed listToolsResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, fmt.Errorf("%w: tools/list result: %w", ErrMalformedFrame, err)
	}
	tools := nonNilSlice(parsed.Tools)

	byName := make(map[string]ToolDescriptor, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	c.mu.Lock()
	c.tools = byName
	c.mu.Unlock()

	c.logger().LogAttrs(ctx, slog.LevelDebug, "belay/sonar/mcp: listed tools", slog.Int("count", len(tools)))
	return tools, nil
}

func (c *Client) hasTool(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tools == nil {
		return false
	}
	_, ok := c.tools[name]
	return ok
}

// CallTool invokes the tool named name with args as its arguments (marshaled
// as the request's JSON object) and returns its result.
//
// CallTool never sends a tools/call request for a tool ListTools has not
// most recently advertised: it returns an error wrapping
// ErrToolNotAdvertised without touching the Transport at all. This is a
// deliberate belt-and-braces check, not trust in server-side validation —
// a caller (or a future tools.go bug) asking for a tool a specific server
// deployment does not have should fail locally and immediately, not
// produce a confusing method-not-found *RPCError from the wire.
//
// It returns ErrNotInitialized if called before Initialize, and an error
// wrapping ErrToolExecutionFailed if the call reached the server and the
// tool reported its own failure via the result's isError flag — the
// ToolResult is still returned in that case, so a caller can inspect
// ToolResult.Text() for the tool's own explanation.
func (c *Client) CallTool(ctx context.Context, name string, args any) (ToolResult, error) {
	if !c.isInitialized() {
		return ToolResult{}, ErrNotInitialized
	}
	if !c.hasTool(name) {
		return ToolResult{}, fmt.Errorf("%w: %q", ErrToolNotAdvertised, name)
	}

	result, err := c.call(ctx, "tools/call", toolCallParams{Name: name, Arguments: args})
	if err != nil {
		return ToolResult{}, err
	}

	var wire toolCallResultWire
	if err := json.Unmarshal(result, &wire); err != nil {
		return ToolResult{}, fmt.Errorf("%w: tools/call result: %w", ErrMalformedFrame, err)
	}

	content := make([]ContentBlock, len(wire.Content))
	for i, b := range wire.Content {
		content[i] = ContentBlock(b)
	}
	out := ToolResult{
		Content:           nonNilSlice(content),
		IsError:           wire.IsError,
		StructuredContent: wire.StructuredContent,
		Raw:               result,
	}
	if out.IsError {
		return out, fmt.Errorf("%w: %s: %s", ErrToolExecutionFailed, name, out.Text())
	}
	return out, nil
}

// Close releases the underlying Transport.
func (c *Client) Close() error {
	if c.Transport == nil {
		return nil
	}
	return c.Transport.Close()
}
