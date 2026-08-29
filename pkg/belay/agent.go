package belay

import (
	"context"
	"encoding/json"
)

// AgentBackend adapts a coding-agent CLI or API to the graph runner.
//
// An implementation wraps one specific tool — the Claude Code CLI, the
// Codex CLI, an internal HTTP-based agent service, whatever a user has
// installed — behind a single call: Invoke. The runner never knows which
// backend it is talking to; it only knows AgentRequest in, AgentResponse
// out. This is the entire surface of belay's CLI-agnostic orchestration
// pillar: swapping backends never touches graph logic.
//
// Invoke's concurrency requirements are the caller's, not the interface's:
// the runner invokes at most one AgentBackend call per graph node, but a
// best-of-N fanout may call the same AgentBackend concurrently from
// multiple nodes. An implementation that cannot safely serve concurrent
// Invoke calls must serialize internally (or document that it cannot be
// used with fanout) rather than silently corrupt state.
type AgentBackend interface {
	// Name returns a short, stable, lowercase identifier for the backend,
	// such as "claude-code" or "codex-cli". Name appears in run journals,
	// logs, and best-of-N candidate labels, so it must not change between
	// versions of the same backend — treat it like a wire value, not a
	// display string.
	Name() string

	// Invoke runs the backend once against req and returns its response.
	//
	// Invoke must respect ctx: a canceled or expired context should abort
	// the underlying process or request promptly and return an error
	// wrapping ctx.Err(). A non-nil error means the node failed outright —
	// the caller discards AgentResponse and does not inspect it. A backend
	// that produced partial output before hitting a problem it can
	// identify (for example its own budget guard) should still return a
	// populated AgentResponse together with an error wrapping
	// ErrBudgetExceeded, rather than silently discarding the output; a
	// caller that wants the partial work must be able to get at it via the
	// error path too (for example with errors.As against a backend-defined
	// type carrying the response), which is why Invoke's error case does
	// not forbid a meaningful AgentResponse — it just marks the call as
	// failed.
	Invoke(ctx context.Context, req AgentRequest) (AgentResponse, error)
}

// AgentRequest is the input to one AgentBackend.Invoke call: everything a
// backend needs to run one turn, or one bounded multi-turn session, of an
// autonomous coding agent.
type AgentRequest struct {
	// Prompt is the task-specific instruction for this invocation, such as
	// "implement the Login handler described in the plan below." Required.
	Prompt string `json:"prompt"`

	// SystemPrompt, if non-empty, overrides or augments the backend's
	// default system/instructions prompt. A backend with no separate
	// system-prompt concept may prepend it to Prompt instead; callers must
	// not depend on which strategy a given backend uses.
	SystemPrompt string `json:"system_prompt"`

	// WorkDir is the absolute path the backend must treat as its working
	// directory and repository root — typically an isolated
	// Workspace.Dir. Required.
	WorkDir string `json:"work_dir"`

	// AllowedTools restricts which tools or capabilities the backend may
	// use during this invocation, such as "Read", "Write", "Bash". Empty
	// (nil or zero-length) means "use the backend's default tool set", not
	// "no tools". A backend that supports denying all tools must document
	// its own explicit spelling for that (for example a single reserved
	// entry) rather than overload the zero value, which every backend
	// already reserves for "default".
	AllowedTools []string `json:"allowed_tools"`

	// MaxTurns caps the number of agent turns (tool-call/response cycles)
	// the backend may take before it must return. Zero means "use the
	// backend's default", not "zero turns". A backend with no turn concept
	// ignores it.
	MaxTurns int `json:"max_turns"`

	// Model selects a backend-specific model identifier, such as
	// "claude-opus-4" or "gpt-5-codex". Empty means "use the backend's
	// default model". belay does not validate model names: an unknown
	// value is the backend's error to report.
	Model string `json:"model"`

	// SessionID, if non-empty, asks the backend to resume a prior
	// conversation instead of starting a new one — typically the
	// SessionID a previous AgentResponse returned. A backend that does not
	// support resumption must return an error wrapping ErrUnsupported
	// rather than silently starting a fresh session under the requested
	// prompt: silently ignoring SessionID would make a "continue this
	// session" call indistinguishable from "start fresh", which is exactly
	// the kind of implicit behavior Hyrum's Law turns into a contract
	// nobody agreed to.
	SessionID string `json:"session_id"`

	// MCPConfig is an opaque, backend-specific MCP server configuration —
	// typically the JSON document a CLI's own --mcp-config flag expects.
	// belay never parses it. Nil or empty means no MCP servers are
	// configured for this call; a backend with no MCP support ignores it.
	MCPConfig json.RawMessage `json:"mcp_config"`
}

// AgentResponse is the result of one AgentBackend.Invoke call.
type AgentResponse struct {
	// Text is the backend's final textual output: its answer, summary, or
	// description of what it changed. A backend that applies changes
	// directly to WorkDir (rather than returning a diff in Text) still
	// populates Text with a human-readable summary of what was done — the
	// graph and any downstream logging depend on Text never being empty
	// after a successful call.
	Text string `json:"text"`

	// SessionID identifies the conversation this response belongs to.
	// Passing it back as the next AgentRequest.SessionID continues the
	// same session, if the backend supports resumption. Empty if the
	// backend has no session concept.
	SessionID string `json:"session_id"`

	// Usage reports token counts and cost for this single invocation.
	Usage Usage `json:"usage"`

	// Turns is the number of agent turns the backend actually took. Zero
	// if the backend does not report turn counts — not necessarily "zero
	// turns happened".
	Turns int `json:"turns"`

	// Raw is the backend's unparsed output — for example the raw JSON line
	// `claude -p --output-format json` prints — preserved for debugging
	// and adapter-specific downstream processing. The graph runner never
	// reads Raw; only the originating backend and its own tests should.
	Raw json.RawMessage `json:"raw"`
}

// Usage reports the token and dollar cost of one AgentBackend.Invoke call.
//
// Usage is the budget guard's only input: a run tracks cumulative USD
// across invocations and stops — returning an error wrapping
// ErrBudgetExceeded — once a configured ceiling is crossed. USD must
// therefore always be populated, even when a backend does not report cost
// directly; Estimated records how it was obtained.
type Usage struct {
	// InputTokens is the number of tokens consumed as input (prompt plus
	// context) for this invocation.
	InputTokens int64 `json:"input_tokens"`

	// OutputTokens is the number of tokens the backend generated.
	OutputTokens int64 `json:"output_tokens"`

	// USD is the dollar cost of this invocation.
	USD float64 `json:"usd"`

	// Estimated reports how USD was obtained.
	//
	// false means the backend's own output reported a dollar figure
	// directly — for example, `claude -p --output-format json`'s
	// total_cost_usd field — and USD is that figure verbatim.
	//
	// true means no such figure was available and USD was computed from a
	// fallback price table keyed on Model and InputTokens/OutputTokens.
	// Callers enforcing a budget must treat an Estimated USD as
	// lower-confidence, not silently equivalent to a reported one: this
	// field exists specifically so that a future CLI version dropping its
	// cost field degrades the budget guard's precision instead of making
	// it silently vanish — the guard should still fire on an estimate, it
	// should just log that it did.
	Estimated bool `json:"estimated"`
}
