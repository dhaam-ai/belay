//go:build unix

// Package mcp is a minimal Model Context Protocol (MCP) client, built to let
// belay's AI-augmented review node (review.mode: ai, ADR-0008) drive a
// SonarQube MCP server instead of shelling out to sonar-scanner directly.
//
// It wraps exactly five tools a SonarQube MCP server exposes —
// list_quality_gates, get_raw_source, analyze_code_snippet,
// get_project_quality_gate_status and search_sonar_issues_in_projects (see
// tools.go) — behind Go-typed requests and responses, so the reviewer that
// consumes this package (T32) never hand-builds JSON-RPC. This package does
// not implement belay.Reviewer itself; it is the client T32 builds one on
// top of.
//
// # No SDK
//
// There is no MCP SDK in this module, and this package does not add one:
// MCP's wire protocol is JSON-RPC 2.0 framed either as one HTTP request per
// message or as newline-delimited JSON over a subprocess's stdio, and both
// are small enough to implement directly against encoding/json and net/http.
//
// # Three deployments, one seam
//
// config.Review.AI.MCP.Mode selects how belay reaches the server:
//
//   - "cloud" and "server" talk HTTP to an already-running endpoint
//     (SonarQube Cloud's embedded MCP endpoint, or SonarQube Server
//     2026.3+'s native <url>/mcp) — see HTTPTransport.
//   - "docker" starts sonarsource/sonar-mcp-server as a local container and
//     speaks JSON-RPC over its stdin/stdout — see StdioTransport.
//
// [Client] knows about neither. It is built from a [Transport] — an
// interface with exactly Send, Recv and Close — and does the same
// initialize handshake, tools/list gate and tools/call dance regardless of
// which one it was given. That seam is the entire point of this package:
// every behavioral test in this package drives Client against an in-memory
// fake transport (see helper_test.go's fakeTransport) and starts no
// subprocess and makes no network call; the two real Transport
// implementations are exercised separately, StdioTransport against a
// harmless re-exec of this test binary (see helper_test.go's TestMain, the
// same technique internal/exec and internal/agent/claude use) and
// HTTPTransport against an httptest.Server bound to loopback — neither
// touches Docker or a real network zone. See the acceptance report for how
// this was verified.
//
// # JSON-RPC correctness
//
// Every request Client sends carries a monotonically increasing integer id
// (Client.nextID); [decodeEnvelope] classifies every inbound frame as a
// response, a notification, or a request from the peer (which this client,
// having no capabilities to serve, only logs and drops) before Client.call
// acts on it, so a notification arriving while a call is in flight is never
// mistaken for that call's answer, and a response whose id does not match
// what was asked for is dropped with a log line rather than corrupting the
// waiting call — see client.go's call and mcp_test.go's table tests for
// malformed frames, unrecognized ids, and JSON-RPC error objects. A JSON-RPC
// error object comes back as *[RPCError] (errors.Is ErrServerError),
// distinct from every transport-level failure (a broken pipe, a non-2xx
// HTTP status, a cancelled context), which Client never wraps as an
// RPCError.
//
// # The initialize gate
//
// No tool is ever called before Client.Initialize has completed
// successfully and negotiated a protocol version this client supports, and
// no tool is ever called unless Client.ListTools has advertised it by name
// first — see Client.CallTool. Both are enforced in Go, not left as a
// documented precondition a caller might skip.
//
// # Credentials
//
// SONAR_TOKEN is read from the process environment only, by whichever
// caller constructs a Transport (never by this package reaching into
// os.Environ itself), and is passed to HTTPTransport as an Authorization
// header value and to StdioTransport as an explicit child environment
// variable — never as a command-line argument, and never logged. Both
// transports additionally seed an internal/exec.Redactor with the token's
// value, so even a server or child process that echoes it back cannot make
// it appear in an error string or a log line. See internal/exec/redact.go.
//
// # Concurrency
//
// See the doc comment on Client: calls are serialized by design.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// jsonRPCVersion is the only "jsonrpc" value this package sends or accepts,
// per the JSON-RPC 2.0 spec and MCP's requirement that every message carry
// it verbatim.
const jsonRPCVersion = "2.0"

// ProtocolVersion is the MCP protocol version Client offers first during
// the initialize handshake.
const ProtocolVersion = "2025-06-18"

// SupportedProtocolVersions are every protocol version Client can speak,
// preferred first. A server's initialize response naming any other version
// is rejected with ErrUnsupportedProtocolVersion: per the MCP spec, the
// client must not proceed using a version it does not understand.
var SupportedProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Sentinel errors. Match them with errors.Is; several have a companion type
// recoverable with errors.As where structured detail matters (*RPCError,
// *HTTPStatusError).
var (
	// ErrMalformedFrame reports that a byte sequence read from a Transport
	// was not a well-formed JSON-RPC 2.0 frame: invalid JSON, a JSON value
	// that is not the required message shape (an object carrying exactly
	// one of a method or a result/error, plus an id iff it is a request or
	// response), or a "jsonrpc" field that is not exactly "2.0". A single
	// malformed frame does not fail an in-flight call outright — Client
	// logs it and keeps waiting, bounded by the caller's context — but it
	// always means the bytes in question could not become a decoded
	// message.
	ErrMalformedFrame = errors.New("belay/sonar/mcp: malformed jsonrpc frame")

	// ErrTransportClosed reports that a Transport can no longer send or
	// receive: its subprocess exited (StdioTransport) or Close was called.
	ErrTransportClosed = errors.New("belay/sonar/mcp: transport closed")

	// ErrNoFrame reports that a Transport was asked to Recv with nothing
	// available to return and no way to produce more without another Send
	// — the shape of HTTPTransport, where one Send performs a full HTTP
	// round trip and Recv only drains what it produced.
	ErrNoFrame = errors.New("belay/sonar/mcp: no frame available")

	// ErrNotInitialized reports that ListTools or CallTool was called
	// before Client.Initialize completed successfully.
	ErrNotInitialized = errors.New("belay/sonar/mcp: client is not initialized")

	// ErrUnsupportedProtocolVersion reports that a server's initialize
	// response named a protocol version not present in
	// SupportedProtocolVersions.
	ErrUnsupportedProtocolVersion = errors.New("belay/sonar/mcp: server offered an unsupported protocol version")

	// ErrToolNotAdvertised reports that CallTool (or one of the five typed
	// wrappers in tools.go) was asked to call a tool name that the most
	// recent ListTools did not advertise. No tools/call request is ever
	// sent for it: this is a local refusal, not a server error.
	ErrToolNotAdvertised = errors.New("belay/sonar/mcp: tool was not advertised by tools/list")

	// ErrToolExecutionFailed reports that a tools/call round trip
	// succeeded at the JSON-RPC layer but the tool itself reported failure
	// via the result's isError field — for example, an analyze_code_snippet
	// call given a language SonarQube's analyzer does not recognize. It is
	// deliberately distinct from *RPCError: the server processed the
	// request and the *tool* failed, rather than the protocol call itself
	// being rejected.
	ErrToolExecutionFailed = errors.New("belay/sonar/mcp: tool reported an execution error")

	// ErrNoContent reports that a tools/call result carried neither
	// structuredContent nor a parsable text content block for
	// ToolResult.JSON to decode.
	ErrNoContent = errors.New("belay/sonar/mcp: tool result carried no parsable content")

	// ErrServerError is what every *RPCError unwraps to, so a caller can
	// test errors.Is(err, ErrServerError) without recovering the code and
	// message errors.As would give access to.
	ErrServerError = errors.New("belay/sonar/mcp: server returned a jsonrpc error object")
)

// RPCError reports a JSON-RPC 2.0 error object the server returned in
// answer to a request: the request reached the server and the server
// refused or failed to process it. It is deliberately distinct from a
// transport failure (a broken connection, a non-2xx HTTP status, a
// cancelled context) — those never become an *RPCError, because the server
// never got the chance to answer at all.
type RPCError struct {
	// Code is the JSON-RPC error code, such as -32601 (method not found)
	// or -32602 (invalid params).
	Code int
	// Message is the server's one-line description of the error.
	Message string
	// Data is the server's optional structured detail, verbatim.
	Data json.RawMessage
}

// Error implements error.
func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("belay/sonar/mcp: jsonrpc error %d: %s (data: %s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("belay/sonar/mcp: jsonrpc error %d: %s", e.Code, e.Message)
}

// Unwrap reports ErrServerError, so errors.Is(err, ErrServerError) holds
// for any *RPCError regardless of its code.
func (e *RPCError) Unwrap() error { return ErrServerError }

// rpcRequest is the wire shape of an outbound JSON-RPC request: a message
// that expects a correlated response.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcNotification is the wire shape of an outbound JSON-RPC notification: a
// message with no id, and so no correlated response — sent and forgotten.
type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcErrorObject is the wire shape of a JSON-RPC error object, nested
// inside a response envelope's "error" field.
type rpcErrorObject struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// wireEnvelope decodes any inbound JSON-RPC 2.0 message — a response, a
// notification, or (unsupported by this client, but not allowed to panic
// on) a request from the peer — into one shape, deferring classification to
// hasID/isNotification/isPeerRequest/matchID. Fields it does not recognize
// are ignored by encoding/json, which is what lets a server add a field in
// a later spec revision (MCP's own "_meta", for instance) without breaking
// this client.
type wireEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErrorObject `json:"error,omitempty"`
}

// hasID reports whether e carries a meaningful id: the "id" key is present
// and its value is not the JSON literal null. MCP notifications omit the
// key entirely; treating an explicit null the same way as an absent key
// matches every server this client has been written against and keeps a
// stray null from being misread as a response awaiting correlation.
func (e wireEnvelope) hasID() bool {
	trimmed := bytes.TrimSpace(e.ID)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// isNotification reports whether e is a notification: a method call with
// no id, and so no reply Client should ever wait for.
func (e wireEnvelope) isNotification() bool {
	return !e.hasID() && e.Method != ""
}

// isPeerRequest reports whether e is a request originating from the server
// rather than a response to one of ours: it carries both an id and a
// method. This client advertises no server-callable capabilities (it never
// sends "sampling" or "roots" capabilities during initialize), so a
// conformant server should never send one; isPeerRequest exists so that if
// one arrives anyway, Client.call can log and drop it instead of
// misreading it as an answer to the request it is waiting on.
func (e wireEnvelope) isPeerRequest() bool {
	return e.hasID() && e.Method != ""
}

// matchID reports whether e is a response correlated to the request that
// used id. A response whose id cannot be parsed as an integer, or that
// names a different integer, reports false — either the id is one this
// client never issued (a stray or duplicated frame) or the server echoed a
// non-numeric id back, which this client never sends and so never expects.
func (e wireEnvelope) matchID(want int64) bool {
	if !e.hasID() {
		return false
	}
	var got int64
	if err := json.Unmarshal(e.ID, &got); err != nil {
		return false
	}
	return got == want
}

// decodeEnvelope parses raw as one JSON-RPC 2.0 message and validates its
// shape against the spec's three message kinds (request, notification,
// response), returning an error wrapping ErrMalformedFrame for anything
// else: invalid JSON, a JSON value that decodes but is not an object (a
// bare string, number, or array — MCP does not support batched array
// frames, and a lone scalar is never a valid message), a "jsonrpc" field
// other than exactly "2.0", a frame that is neither a request/notification
// (has a method) nor a response (has a result or an error) and so satisfies
// none of the three shapes, or a frame that impossibly satisfies more than
// one of them at once (both a method and a result/error, or both a result
// and an error).
func decodeEnvelope(raw []byte) (wireEnvelope, error) {
	var env wireEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return wireEnvelope{}, fmt.Errorf("%w: %w", ErrMalformedFrame, err)
	}
	if env.JSONRPC != jsonRPCVersion {
		return wireEnvelope{}, fmt.Errorf("%w: jsonrpc field is %q, want %q", ErrMalformedFrame, env.JSONRPC, jsonRPCVersion)
	}
	if env.Result != nil && env.Error != nil {
		return wireEnvelope{}, fmt.Errorf("%w: frame carries both a result and an error", ErrMalformedFrame)
	}
	hasMethod := env.Method != ""
	hasPayload := env.Result != nil || env.Error != nil
	switch {
	case hasMethod && hasPayload:
		return wireEnvelope{}, fmt.Errorf("%w: frame carries both a method and a result/error", ErrMalformedFrame)
	case env.hasID() && !hasMethod && !hasPayload:
		return wireEnvelope{}, fmt.Errorf("%w: frame has an id but no method, result, or error", ErrMalformedFrame)
	case !env.hasID() && !hasMethod:
		return wireEnvelope{}, fmt.Errorf("%w: frame has neither an id nor a method", ErrMalformedFrame)
	}
	return env, nil
}

// nonNilSlice returns s, or a non-nil empty slice when s is nil.
//
// The landed internal/state package hit a real bug where copying a slice
// through append([]T(nil), src...) collapses a non-nil, empty src back to
// nil, silently turning "measured and found nothing" into "not measured at
// all" once it reaches JSON. Every slice this package decodes from a tool
// response is normalized through nonNilSlice before being handed to a
// caller, deliberately not via that append idiom, so an empty result
// serializes as [] and a *belay.QualityReport a Reviewer builds from it can
// keep belay.QualityReport.Issues's own "never nil" guarantee.
func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
