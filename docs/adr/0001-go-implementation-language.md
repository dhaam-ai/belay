# ADR-0001: Go as Implementation Language

## Status
Accepted

## Context
Coding Challenge #134 ("Agentic Engineering Graph") is language-agnostic, but an implementation must choose. Three contenders emerged in the design phase:

1. **Python**: Fastest path to MVP. LangGraph is the reference mental model; Python's MCP client libraries are mature (anthropic-sdk, langchain). Fit felt natural to the challenge author's context.

2. **Node/TypeScript**: The official MCP SDK is TypeScript-first; npx distribution would allow zero-install runs for end users. Strong async/await primitives for orchestration.

3. **Go**: Single static binary, real OS-level concurrency (goroutines, channels) for fanout isolation, and author depth — the lead engineer has production Go experience and owns the architecture.

The trade-off centered on deployability, concurrency model, and iteration speed. No language makes all three easy.

## Decision
Go was chosen as the implementation language. The project will be a single static binary deployable anywhere, using goroutines for fanout parallelism and channels for state synchronization.

## Consequences

### Positive
- **Single binary**: No runtime, no dependency hell, no container required. A single `belay` binary distributed via GitHub releases works on macOS, Linux, Windows.
- **Real concurrency**: Goroutines scale to hundreds of fanout candidates without OS thread overhead. Channels enforce message passing discipline.
- **Author depth**: Lead engineer owns the codebase without friction; code reviews and maintenance are faster.
- **Production maturity**: Go's standard library (encoding/json, os, sync) is stable and well-understood for CLI tools.

### Negative
- **JSON boilerplate**: Marshaling/unmarshaling state to/from ndjson requires manual field tags and error handling; no native dataclass equivalent.
- **MCP client story**: No official Go MCP SDK (only Python and TypeScript). The project must define its own MCP client or vendor one — adds ~200 lines of glue per session type.
- **LLM library ecosystem**: Fewer mature Go libraries for LLM integration (no langchain-go, no LangGraph equivalent). Must hand-craft orchestration logic.
- **Slower iteration on new adapter backends**: Adding Gemini or Codex support requires writing new JSON request/response types instead of inheriting from a Python base.

### Follow-ups
- Define a lightweight MCP client interface in `pkg/mcp/` so future adapters can implement it without duplicating session logic.
- Document JSON field tags and state schema in code comments to avoid serialization bugs.
- Maintain a living list of recommended Go LLM libraries in docs (anthropic-sdk for Claude, etc.).

## Alternatives Considered
- **Python**: Would have reduced time to first working graph and enabled direct reuse of LangGraph patterns. Lost because: (1) requires a Python runtime in production, (2) goroutines are hard to replicate with asyncio, (3) author depth was lower.
- **Node/TypeScript**: Official MCP SDK and npx distribution were attractive. Lost because: (1) single binary not feasible (requires Node runtime or complex bundling), (2) Go's goroutines beat async/await for fanout safety, (3) author chose Go.

## Revisit If
- Users request easy installation on Windows and npm packages emerge as the dominant distribution method (would favor Node).
- Fanout rarely exceeds 3 candidates in practice (goroutine overhead becomes moot; Python+asyncio becomes viable).
- An official Go MCP SDK lands with production adoption and reduces boilerplate cost below 200 lines.

## References
- Challenge #134: "Agentic Engineering Graph"
- MPC Spec: https://spec.modelcontextprotocol.io/
- Go sync primitives: https://golang.org/pkg/sync/
