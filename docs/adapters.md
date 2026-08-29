# Adapters & Extension Points

Belay is built around pluggable adapters that interact with external services. This document describes each adapter interface and how to implement custom adapters.

## Overview

Adapters are injected into the dispatcher at runtime. They abstract away differences between services (Claude, Codex, linters, etc.), allowing the graph to remain backend-agnostic.

The main adapter interfaces are:

1. **AgentBackend**: LLM-based code generation (Claude Code, Codex, Gemini)
2. **Reviewer**: Code quality assessment (linters, SonarQube, AI)
3. **TestRunner**: Test execution and reporting
4. **CodeFormatter**: Code formatting and cleanup

## AgentBackend Interface

<!-- T46: Document AgentBackend interface methods, MCP session lifecycle, streaming behavior -->

**Purpose**: Generate code, plans, fixes in response to natural language or failing tests.

**Key Methods**:
- `Plan(ctx, prompt, repo) → PlanResult`
- `Generate(ctx, prompt, context, history) → GenerateResult`
- `Review(ctx, code, context) → ReviewResult`

**MCP Integration**: Adapters use MCP to communicate with agents. The adapter manages the MCP session lifecycle: open, send RPC calls, close.

**Error Handling**:

<!-- T46: Document error types (TokenLimitExceeded, RateLimited, InvalidRequest, etc.), recovery behavior -->

Adapters return typed errors; the dispatcher handles them based on error category (transient vs. permanent).

## Reviewer Interfaces

### LocalLinterReviewer

<!-- T46: Document linter detection, runner invocation, result parsing -->

**Purpose**: Run static analysis tools (golangci-lint, eslint, bandit, sqlcheck) on generated code.

**Linter Detection**: Auto-detects linters based on file extensions and language (Go → golangci-lint, JS → eslint).

**Result**: Returns QualityReport{Gate: "pass"/"fail", Counts, Issues}.

### SonarQubeReviewer

<!-- T46: Document SonarQube integration, authentication, quality gate logic, server vs. Docker modes -->

**Purpose**: Query SonarQube for code quality metrics and semantic analysis.

**Modes**:
- **Server mode**: Connect to existing SonarQube server (require SONAR_TOKEN)
- **Docker mode**: Spawn SonarQube in Docker (require docker binary, more setup)

**Quality Gate**: Checks SonarQube quality gate status (blocker issues, coverage thresholds). Fails review if gate is not passed.

## TestRunner Interface

<!-- T46: Document TestRunner methods, language detection, coverage reporting, failure detection -->

**Purpose**: Execute tests and report results and coverage metrics.

**Implementations**:
- Go: `go test ./...` with -cover flag
- JavaScript: `npm test` (or custom script)
- Python: `pytest` (or unittest)

**Coverage**: Extracts line/branch coverage from test output.

**Failure Detection**: Parses test output to identify which tests failed and why.

## CodeFormatter Interface

<!-- T46: Document CodeFormatter methods, language detection, idempotency guarantees -->

**Purpose**: Auto-format generated code to match project style.

**Idempotency**: Formatters must be idempotent (running twice produces the same result).

**Implementations**:
- Go: `gofmt` or `goimports`
- JavaScript: `prettier`
- Python: `black` or `autopep8`

## Custom Adapter Implementation

### Example: Gemini Backend

<!-- T46: Provide a template/skeleton for implementing a new AgentBackend (Gemini) -->

To implement a new agent backend:

1. Create `pkg/agent/backends/gemini/adapter.go`
2. Implement the AgentBackend interface
3. Manage the MCP session (open → RPC calls → close)
4. Return results conforming to PlanResult, GenerateResult, etc.
5. Add integration tests using cassettes (see docs/adr/0009-record-replay-cassettes.md)

### Example: Property-Based Reviewer

<!-- T46: Provide a template/skeleton for implementing a new Reviewer -->

To implement a custom reviewer:

1. Create `pkg/review/property_based/runner.go`
2. Implement the Reviewer interface
3. Execute property-based tests (e.g., Hypothesis, QuickCheck)
4. Return QualityReport{Gate: pass/fail, Issues}

## Adapter Registration

<!-- T46: Document how adapters are registered in the dispatcher, dependency injection pattern -->

Adapters are registered at dispatcher creation time via a Config struct:

```go
cfg := &DispatcherConfig{
  Agent:     &claude.Adapter{...},
  Reviewer:  &linters.LocalReviewer{...},
  TestRunner: &golang.Runner{...},
}
d := NewDispatcher(cfg)
```

The dispatcher holds references to all adapters and passes them to node functions.

## Testing Adapters

### Cassette Recording

<!-- T46: Document how to record and replay cassettes for adapter tests -->

Adapter tests use cassettes (recorded HTTP request/response pairs):

1. Run tests locally with `belay test --record` (live API calls)
2. Cassettes are saved to `testdata/cassettes/`
3. CI replays cassettes without making live calls

See docs/adr/0009-record-replay-cassettes.md for details.

### Conformance Suite

<!-- T46: Document the conformance suite structure and what it validates -->

A conformance test suite exercises all adapter implementations and asserts they produce compatible results. For example:

- All AgentBackend implementations must return valid GenerateResult
- All Reviewers must return valid QualityReport with consistent Counts

## References

- [ADR-0005](adr/0005-claude-code-only-backend-v0.md): Claude Code as only shipped backend
- [ADR-0006](adr/0006-single-quality-report-contract.md): Single QualityReport contract
- [ADR-0009](adr/0009-record-replay-cassettes.md): Cassette recording strategy
