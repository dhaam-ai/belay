# Belay Architecture

Belay is a durable, resumable orchestrator for autonomous coding agents. This document describes the high-level design, component interactions, and key abstractions.

## Overview

Belay runs a directed acyclic graph (DAG) of coding phases:

```
plan -> approve -> code -> write -> test -> fix -> review
  |
  +-- (fanout with multiple candidates)
  |
  +-- (join: merge winning candidate)
```

Each node in the graph is a pure function that returns a result; the dispatcher persists the result to an append-only journal. If belay crashes, the next invocation resumes from the last completed node.

The four product pillars are:

1. **Durable + Resumable**: Crash-resume from the last completed node, not from the beginning.
2. **Quality-Gated**: Code must pass linters (phase 6) or AI review (phase 7) before being marked done.
3. **Best-of-N Candidates**: Fanout spawns N parallel code candidates; the graph selects the best one.
4. **CLI-Agnostic**: Agent backend is pluggable (Claude Code in v0.1; Codex and Gemini via AgentBackend interface).

## Architecture Layers

### Layer 1: State & Persistence

<!-- T46: Document the state schema (RunState, Manifest fields), journal format, resume algorithm -->

**State**: A RunState struct holds the current run metadata and phase results. Persisted to `manifest.json` after every phase.

**Journal**: An append-only NDJSON file (`belay.journal`) records every state change. Each line is a JSON object: `{phase: "code", timestamp: "...", result: {...}}`.

**Resume**: On startup, belay reads the journal from the beginning, replays all records to reconstruct RunState, and skips already-completed phases.

### Layer 2: Node Execution

<!-- T46: Document NodeFunc signature, Result{Next, Status, Patch, Usage}, adapter interface -->

**Pure Functions**: Each graph node (plan, code, review, etc.) is a NodeFunc that takes (RunState, adapters) → Result. Nodes never touch the filesystem or journal.

**Result Contract**: Every node returns Result{Next string, Status string, Patch, Usage{InputTokens, OutputTokens, CostUSD}}.

**Adapters**: Nodes interact with external services (Claude Code, linters, test runners) via adapter interfaces. Adapters are injected at graph creation time.

### Layer 3: Dispatcher & Graph

<!-- T46: Document Dispatcher.Run() flow, phase ordering, error handling, retry logic -->

**Dispatcher**: Orchestrates the graph. For each phase: (1) check if completed in journal, (2) call the node function, (3) validate result, (4) persist to journal, (5) apply patch to RunState.

**Graph Definition**: A directed graph defining edges (e.g., code → write → test). The dispatcher respects the graph topology.

**Fanout/Join**: When a phase returns Next="fanout", the dispatcher spawns N candidates (each in isolated directories). After all finish, "join" merges the winning candidate back into the main tree.

### Layer 4: Quality Gates & Budget

<!-- T46: Document QualityReport contract, ReviewerFunc interface, budget ledger, cost tracking -->

**QualityReport**: Phases 6 and 7 return a QualityReport{Gate (pass/fail), Counts, Issues, Raw}. The graph stops if Gate == "fail".

**Budget**: A ledger tracks cumulative spend (USD, token count). Phases abort if spend exceeds belay.yaml limits.

### Layer 5: CLI & Configuration

<!-- T46: Document belay.yaml schema, CLI flags, config validation -->

**Config**: belay.yaml defines agent backend, graph phases, budget limits, review mode, fanout settings, test runner.

**CLI**: `belay run` / `belay resume` commands load config and invoke the dispatcher.

## Component Interaction

```
CLI (belay run)
  ↓
Config Loader (belay.yaml)
  ↓
Dispatcher (orchestrates graph)
  ├→ NodeFunc (plan)  → Result
  ├→ NodeFunc (code)  → Result + Fanout
  │  └→ Isolator (spawn N candidates)
  │     └→ NodeFunc (code) × N  → Results
  ├→ Merge (join candidates)
  ├→ NodeFunc (review)  → QualityReport
  ├→ BudgetLedger (track cost)
  └→ Journal (persist all records)
```

## State Machine & Phases

<!-- T46: Document each phase (plan, approve, code, write, test, fix, review) with entry/exit conditions -->

The run progresses through phases in order. Each phase has:
- Entry condition (e.g., "previous phase completed")
- Execution (call node function via adapter)
- Exit condition (e.g., "result is valid")
- Persistence (write to journal)

If a phase fails the exit condition (invalid result, budget exceeded, etc.), the dispatcher stops and returns an error. The user can retry with `belay resume`.

## Isolation & Fanout

<!-- T46: Document Isolator interface, directory copy strategy, merge back algorithm -->

When fanout is enabled, belay creates an isolated workspace for each candidate. Currently, isolation uses plain directory copies (`cp -r`). The Isolator interface allows future implementations (git worktrees, containers).

After all candidates complete, the dispatcher merges the winning candidate's changes back into the original tree (manual copy-back; no automatic merge).

## Adapters & Extension Points

<!-- T46: Document adapter interfaces: Agent, Reviewer, TestRunner, CodeFormatter -->

Adapters are injected into the dispatcher and called by node functions:

- **AgentBackend**: LLM backend (Claude Code, Codex, Gemini). Implements streaming code generation via MCP.
- **LocalLinterReviewer**: Runs linters (golangci-lint, eslint). Returns QualityReport.
- **SonarQubeReviewer**: Queries SonarQube server. Returns QualityReport.
- **TestRunner**: Executes tests (go test, npm test). Returns test results and coverage.
- **CodeFormatter**: Formats code (gofmt, prettier). Idempotent.

## Error Handling & Recovery

<!-- T46: Document error types, retry logic, recovery conditions -->

Errors fall into two categories:

1. **Transient** (e.g., network timeout): Dispatcher retries up to N times, with exponential backoff.
2. **Permanent** (e.g., invalid config, syntax error in generated code): Dispatcher stops and returns error to user.

Users can inspect the journal (`belay journal show`) to see what completed and what failed.

## Performance & Concurrency

Fanout uses goroutines for parallel candidate execution. Each candidate runs independently in its isolated directory; there is no synchronization until join.

The dispatcher is single-threaded; it processes phases sequentially. This simplifies state management and makes the replay-resume model easy to reason about.

## Testing & Conformance

<!-- T46: Document conformance suite, cassette recording, test layout -->

The test suite exercises the full graph with recorded cassettes (zero-cost CI). A conformance suite ensures all adapter implementations (LocalLinterReviewer, SonarQubeReviewer, etc.) produce compatible results.

See docs/adr/0009-record-replay-cassettes.md for cassette strategy.

## References

- [ADR-0001](adr/0001-go-implementation-language.md): Go implementation language
- [ADR-0002](adr/0002-dispatcher-owns-persistence.md): Dispatcher owns persistence
- [ADR-0003](adr/0003-ndjson-journal-fsync.md): NDJSON journal
- [ADR-0004](adr/0004-directory-copies-for-fanout.md): Directory copy fanout isolation
