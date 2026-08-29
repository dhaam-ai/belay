# ADR-0005: Claude Code as the Only Shipped Backend in v0.1, with AgentBackend Exported

## Status
Accepted

## Context
Belay targets three agent backends in the product vision: Claude Code (Anthropic), Codex (GitHub), and Gemini (Google). The challenge allows any backend. But v0.1 ships a product, not a research prototype, so a choice had to be made: ship all three, ship one, or ship one with infrastructure for others?

The team debated:

1. **Ship all three backends**: Validate the "CLI-agnostic" pillar immediately. Downsides: requires credentials for all three, harder to test locally.

2. **Ship Claude Code only; export AgentBackend interface for others**: Validates one backend thoroughly. Codex and Gemini can be implemented later by the team or contributors. Downsides: "CLI-agnostic" becomes a design promise, not a proven fact. Marketing must be honest about this.

The lead engineer recommended shipping all three (or at least Codex + Claude Code) to validate the interface boundary. The team chose Claude Code only, accepting the risk.

## Decision
v0.1 ships Claude Code as the only agent backend. The `AgentBackend` interface is exported in `pkg/agent/`, documented, and remains stable for future implementations. Codex and Gemini CLIs are not shipped until a maintainer runs a conformance suite against them and confirms they work.

## Consequences

### Positive
- **Focus on quality**: One backend means detailed integration testing, clear error messages, predictable behavior. No "this works with Claude but not Gemini" bugs in v0.1.
- **Lower onboarding friction**: Users don't need Codex (not generally available) or Gemini (less popular in agentic use cases). Claude Code is the natural first choice.
- **Simpler CI/CD**: Tests need only ANTHROPIC_API_KEY or recorded cassettes. No matrix of backend credentials.
- **Clear growth path**: AgentBackend interface is public. Contributors can implement Codex or Gemini without waiting for maintainer action.

### Negative
- **"CLI-agnostic" is a design promise, not a proven fact**: The product roadmap promises multiple backends. v0.1 proves one. If Codex or Gemini implementations fail, the promise is broken.
- **Locked into Claude Code for v0.1 users**: If a user wants to switch to Gemini later, they must wait for an implementation or write their own (and maintain it).
- **Harder to test the interface contract**: Without two implementations, it's unclear whether AgentBackend is sufficient for other backends. Codex might need a new field or method.
- **Interface changes before other backends land**: If AgentBackend needs a breaking change between v0.1 and when Codex lands, it's a painful retrofit.

### Follow-ups
- Document the AgentBackend interface in `pkg/agent/backend.go` with clear examples of what a Codex implementation would need to do.
- Add a section to README: "Supported Backends in v0.1" with a clear statement: "Only Claude Code is shipped and tested. Other backends (Codex, Gemini) are design-verified but not implemented."
- Create an example Codex stub in `pkg/agent/backends/codex/stub.go` that compiles but returns ErrNotImplemented (useful for contributors to start from).
- Track adoption and feedback: if many users request Gemini support, deprioritize other features to land it sooner.

## Alternatives Considered
- **Ship all three backends**: Would prove the "CLI-agnostic" pillar. Not chosen because: (1) Codex is not available on all development machines, (2) Gemini integration is less mature, (3) testing would require three separate API keys and three times the CI cost, (4) first release would be unfocused.

## Revisit If
- A second backend (Codex or Gemini) passes a full conformance test run (suggesting AgentBackend is sufficient and no breaking changes are needed).
- A user survey shows >30% of requests are for non-Claude-Code backends (reprioritize implementation).
- AgentBackend needs a breaking change after v0.1 ships (would be a strong signal that the interface was premature).

## References
- Challenge #134 phase 2: "Choose Backend"
- pkg/agent/backend.go (T46: AgentBackend interface)
- pkg/agent/claude/adapter.go (T46: Claude Code implementation)
- README.md: "Supported Backends" section (user-facing)
