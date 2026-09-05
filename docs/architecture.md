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

**State**: The run's persistent state lives in `.belay/runs/<run-id>/manifest.json`, a JSON file written after every node completes (see `internal/state/manifest.go`). It contains the goal, the current state (accumulated patches from all completed nodes), configuration, and budget tracking. The dispatcher reads it on resume to reconstruct what happened.

**Journal**: An append-only NDJSON file (`.belay/runs/<run-id>/belay.journal`) records every state transition. Each line is a Record (see `internal/journal/record.go`):
```
{"seq":1,"event":"run_started","ts":"2026-08-29T10:31:00Z"}
{"seq":2,"attempt":1,"node":"plan","event":"node_started","ts":"2026-08-29T10:31:00.4Z"}
{"seq":3,"attempt":1,"node":"plan","event":"node_finished","status":"ok","next":"approve","usage":{"input_tokens":0,"output_tokens":0,"usd":0,"estimated":false}}
```
Records are written with O_APPEND|O_WRONLY, synced after every append, and never rewritten. A crash during one record can corrupt only that record; earlier records remain durable. The journal is the sole source of truth: it records what *completed*, not what was attempted (see ADR-0003).

**Resume**: On startup (or `belay resume`), the dispatcher calls `journal.ResolveStart(records)` to read the journal and compute the next node to run (see `internal/journal/journal.go`). The algorithm uses a decision table that handles:
- A normal run ending in `node_finished`: resume at that node's `next` field
- A paused run (ending in `run_paused`): refuse unless explicitly resumed with `belay resume`
- A run ending in a terminal state (`run_completed`, `run_failed`, `run_aborted`): refuse to resume (it is finished)
- A half-written final record (crash mid-append): discard and heal the journal, resuming at the prior node's `next`

The manifest is recomputed by replaying all journal records up to the resume point, so a crash cannot leave stale state behind.

### Layer 2: Node Execution

**Pure Functions**: Each graph node (plan, code, review, etc.) is a Node interface that takes a RunContext → Result (see `internal/graph/node.go`). Nodes never touch the filesystem, never write to the journal, and never modify state directly. A node is entirely deterministic given frozen inputs and mocked adapters, making it table-testable.

**Result Contract**: Every node returns `Result{Next string, Status journal.Status, Patch state.Patch, Usage belay.Usage, Note string}` (see `internal/graph/dispatcher.go`):
- `Next`: the name of the node to run after this one (e.g., "approve" after "plan"), or "END" to finish the run
- `Status`: one of journal.StatusOK, StatusFailed, StatusPaused, StatusAborted
- `Patch`: an absolute set of state mutations (not a delta); applied by the dispatcher after verification
- `Usage`: token and cost accounting for this node's invocation(s)
- `Note`: a short, human-readable summary of what happened, persisted to the journal

**Adapters**: Nodes access external services via interfaces injected into RunContext:
- `Agent belay.AgentBackend`: LLM code generation (Claude Code, etc.)
- `Runner belay.TestRunner`: test execution
- `Linter belay.Linter`: static analysis (golangci-lint, eslint, etc.)
- `Reviewer belay.Reviewer`: quality gates (SonarQube, AI review, etc.)
- `Isolator belay.Isolator`: workspace isolation

All adapters are injected at dispatcher creation time, making the entire graph backend-agnostic (ADR-0005).

### Layer 3: Dispatcher & Graph

**Dispatcher.Run()**: The main orchestration loop (see `internal/graph/dispatcher.go`) for each step:
1. Resolve the next node using `journal.ResolveStart`, skipping nodes already persisted
2. Set a timeout on the node (from config.Graph.NodeTimeout, usually 15 minutes)
3. Call the node's Run(ctx, rc) method
4. Validate the Result (check Patch is well-formed, Next names a valid node, Status is known)
5. **Apply the Patch to state**, then **write node_finished to the journal** (this order is critical; see below)
6. Update Usage tracking and check budget constraints
7. Return control to step 1 for the next node

**Why Patch-Then-Journal Matters (ADR-0002)**: The dispatcher applies patches and journals *after*, not before. If it journalled first:
- A crash between journal and patch means a durable record exists with a Next node but no state to reach it
- Resume would skip that node (the patch never landed) and downstream nodes would read a State missing the skipped node's output
- The run returns wrong results, silently — not a failure the user notices

By applying first, a crash can leave an applied patch with no journal record (resume runs the node twice, idempotent because Patch is absolute), but a crash can never leave a journal record whose patch did not land.

**Graph Definition**: A directed graph defined in config, with nodes like plan, approve, code, write, test, fix, review and terminal node END. Edges enforce ordering (e.g., code must precede test). The dispatcher consults this to validate that a node's Next is reachable (see `internal/graph` node implementations).

**Fanout/Join**: When a node returns Next="fanout":
1. The dispatcher creates N isolated Workspaces (via Isolator.Create) for N candidates
2. Each candidate runs its own subgraph in parallel (starting from code), independent
3. All candidates complete, and their Results are collected
4. A join node picks the best candidate (by gate status, then coverage, then cost)
5. The winning candidate's state is merged back into the main tree, and execution continues

Fanout is controlled by config.Fanout (currently disabled; see ADR-0004 for directory-copy implementation).

### Layer 4: Quality Gates & Budget

**QualityReport** (see `pkg/belay/quality.go`): The contract for linters and reviewers. Every call returns a report containing:
- `Gate`: GatePass, GateFail, or GateError — the quality verdict
- `Counts`: a histogram of issues by Severity (Info, Minor, Major, Critical, Blocker)
- `Issues`: the list of individual problems found (never nil; empty slice for no findings)
- `Source`: which tool produced the report (e.g., "golangci-lint", "sonarqube")
- `Summary`: a human-readable synopsis
- `Raw`: the tool's unparsed output (graph never reads it)

The critical invariant: two conforming adapters produce reports that serialize to identical JSON key sets, regardless of content. This makes reports comparable and testable across implementations.

The graph acts only on `Gate`: if GateFail, the run branches to the `fix` node (bounded by a loop counter); if GatePass, the run proceeds to the next phase; if GateError, the run aborts (the tool could not reach a verdict, not "the code is bad").

**Budget Ledger** (see `internal/budget/budget.go`): Day-one constraint (ADR-0007). Every node's Usage is added to a cumulative budget. When total USD crosses the configured limit (belay.yaml review.budget_limit_usd), the dispatcher returns an error wrapping belay.ErrBudgetExceeded and aborts the run.

Usage.Estimated reports whether USD was computed from actual backend costs or estimated from a fallback table (belay.Usage.Estimated); callers can log differently for estimated costs.

### Layer 5: CLI & Configuration

**Config** (see `internal/config/config.go` and docs/config.md): belay.yaml defines:
- `agent`: which backend to use (e.g., "claude-code")
- `review`: quality-gate mode ("lint", "sonar-server", "sonar-docker"), severity thresholds (fail_on), budget limits
- `runner`: test toolchain (auto-detected or explicit)
- `graph`: node timeout, max retry attempts, max total steps
- `fanout`: whether to enable best-of-N parallelism (currently disabled)
- `isolator`: isolation mode ("copy" or "none")

**CLI** (in cmd/ — not in scope for this task but referenced via internal/cli):
- `belay run --config belay.yaml <repo>`: start a new run
- `belay resume <run-id>`: continue a paused or crashed run
- `belay timeline <run-id>`: inspect the journal and manifest
- Config is validated on load; invalid settings abort before any work starts.

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

The run progresses through a directed graph of nodes (see `internal/graph/node.go` for each). Each node:
1. **Receives** RunContext with current state, config, adapters, and run metadata
2. **Executes** work (calls adapters) and returns a Result
3. **Persists** the Result (dispatcher writes to journal and applies Patch)
4. **Advances** to the next node named in Result.Next, or END if terminal

**Phase Descriptions**:

- **plan** (Agent): Uses the agent to propose a plan from the goal. Returns a plan summary and branches to approve.
- **approve** (Human Gate): Awaits human approval before proceeding. Returns paused if not approved, or continues to code.
- **code** (Agent): Uses the agent to implement the plan. If fanout is enabled, branches to fanout; else proceeds to write.
- **write** (Agent): Commits code changes (writes files, formats). Branches to test.
- **test** (TestRunner): Executes the test suite. If all tests pass, branches to review; if any fail, branches to fix.
- **fix** (Agent): Uses the agent to fix failing tests. Branches back to test (bounded by attempt counter graph.give_up to prevent infinite loops).
- **review** (Linter or Reviewer): Runs static analysis or AI review. If gate passes, branches to END; if gate fails, branches to fix.
- **fanout** (Isolator): Creates N isolated workspaces and runs each from code onward in parallel. Branches to join.
- **join** (Merge): Selects the best candidate (by gate, coverage, cost) and merges back to main. Branches to review. **Note**: join is not implemented; fanout is disabled (ADR-0004).

**Terminal States**:
- StatusOK with Next="END": run completed successfully
- StatusFailed: a node returned an error or returned StatusFailed
- StatusAborted: budget exceeded, max steps reached, or context cancelled
- StatusPaused: human approval gate not granted; use `belay resume` to continue

If a node returns an error or an invalid Result, the dispatcher stops the run and records the failure. The user inspects the journal with `belay timeline` and may retry with `belay resume` after fixing the underlying issue.

## Isolation & Fanout

**Isolator Interface** (see `pkg/belay/isolate.go`): Abstracts workspace isolation so the graph is implementation-agnostic. Two methods:
- `Create(ctx, src, id string) (Workspace, error)`: Copy or prepare a workspace from src, identified by id. Returns a Workspace{ID, Dir, Ephemeral}.
- `Destroy(ctx, Workspace) error`: Clean up the workspace. Must idempotently handle a Workspace decoded from the journal after a process restart.

**Current Implementation** (ADR-0004): Plain directory copies via `cp -r` (see `internal/isolate/copy.go`). Each candidate gets its own directory under `.belay/runs/<run-id>/candidates/<candidate-id>/`. Fanout is currently disabled (config.Fanout.Enabled = false); future implementations could use git worktrees or containers.

**Merge Back Algorithm**: After all candidates finish, the join node (not yet implemented) would:
1. Score each candidate (gate status, coverage, cost)
2. Select the best one
3. Copy its modified files back to the main repository
4. Continue execution from the main tree

Because the current implementation is disabled, this logic is a placeholder (see `internal/graph` for merge-back stub).

## Adapters & Extension Points

Five pluggable interfaces in `pkg/belay` allow third-party implementations:

- **AgentBackend** (see `pkg/belay/agent.go`): LLM code generation. One method: Invoke(ctx, AgentRequest) → AgentResponse. The request carries the prompt, working directory, MCP config, and optional session ID for resumption. The response carries generated text, usage, and session ID for continuation. Implemented by internal/agent/claude (v0.1) and future Codex, Gemini implementations.

- **TestRunner** (see `pkg/belay/runner.go`): Test execution auto-detection and running. Two methods: Detect(dir) → bool (is this a project this runner can test?), Test(ctx, dir) → TestReport. Returns test pass/fail counts, failure details, and coverage. Implemented by internal/runner for Go, Node, Python. A missing toolchain returns an error wrapping belay.ErrToolchainMissing (not a TestReport with a failure).

- **Linter** (see `pkg/belay/linter.go`): Static analysis auto-detection and running. Two methods: Detect(dir) → bool, Lint(ctx, dir) → QualityReport. Like TestRunner, a missing toolchain is an error, not a failed gate. Implemented by internal/linter for golangci-lint, eslint, ruff.

- **Reviewer** (see `pkg/belay/quality.go`): Holistic quality assessment against a caller-supplied threshold. One method: Review(ctx, ReviewRequest) → QualityReport. Takes file list and FailOn severity. Implemented by internal/review for SonarQube (server or Docker mode) and AI review.

- **Isolator** (see `pkg/belay/isolate.go`): Workspace isolation. Two methods: Create(ctx, src, id) → Workspace, Destroy(ctx, ws) → error. Implemented by internal/isolate for directory copies.

## Error Handling & Recovery

**Error Categories** (see `pkg/belay/errors.go`):

1. **ToolchainMissing** (ErrToolchainMissing): A required binary is not installed (go, pytest, golangci-lint, sonar-scanner). The remedy is to install software, not to edit code. The graph routes this to a human, not to the fix loop.

2. **BudgetExceeded** (ErrBudgetExceeded): Run cost crossed its configured ceiling. Not retryable; the run must abort.

3. **GateFailed** (ErrGateFailed): A quality gate returned GateFail. This is not an error to the gate itself (it worked, returned a verdict), but a graph-level error for callers that want to exit non-zero. The node returns (QualityReport{Gate: GateFail}, nil); the graph branches to fix, not to error handling.

4. **Unsupported** (ErrUnsupported): A call was valid but the adapter cannot do it (e.g., resuming a session on a backend with no session support). Permanent, not retryable.

5. **Other errors**: Network timeouts, context cancellation, parse failures, missing files, etc. Most are permanent and end the run; context cancellation is resumable (the same command can be retried).

**Recovery on Crash**: If belay crashes mid-run:
1. The journal file may have a half-written final record (torn tail)
2. On restart, `journal.ResolveStart` detects the incomplete record, discards it, and heals the file atomically
3. The dispatcher resumes at the node whose completion was the last durable record
4. Node patches are idempotent (absolute sets, not deltas), so re-running a completed node is safe

**User Recovery**: Users can:
- `belay resume <run-id>`: continue a paused or crashed run (refusing runs already in terminal states)
- `belay timeline <run-id>`: inspect the journal and manifest to diagnose failures
- Manually edit the working directory and retry if the failure was external (e.g., a network issue that is now resolved)

## Performance & Concurrency

Fanout uses goroutines for parallel candidate execution. Each candidate runs independently in its isolated directory; there is no synchronization until join.

The dispatcher is single-threaded; it processes phases sequentially. This simplifies state management and makes the replay-resume model easy to reason about.

## Testing & Conformance

**Test Layout**: Each package has its own test suite:
- Node tests (internal/graph/*_test.go): table-driven tests of node logic against mocked adapters (pkg/belay/belaytest fakes)
- Adapter tests (internal/linter/*_test.go, internal/runner/*_test.go, etc.): verify adapters parse tool output correctly, using fixtures instead of invoking real tools
- Dispatcher tests (internal/graph/dispatcher_test.go): full end-to-end runs with recorded cassettes (see ADR-0009)

**Cassette Recording** (ADR-0009): External interactions (HTTP calls, agent invocations, tool output) are recorded as cassettes (YAML files in testdata/) and replayed in CI. Run locally with `belay test --record` to capture; CI replays without network. This makes tests fast, deterministic, and network-independent.

**Conformance Suite**: All linter implementations must:
- Detect the right project type (Detect(dir) → bool)
- Return a QualityReport with identical JSON key structure (even empty reports)
- Map their severity vocabulary to belay.Severity correctly (see the severity tables in internal/linter/)

All test runner implementations must:
- Detect the right project type
- Return a TestReport with Total > 0 iff tests ran (Total == 0 is not "passed")
- Parse test failures and return them as TestFailure objects

See docs/adr/0009-record-replay-cassettes.md for the cassette format and recording workflow.

## References

- [ADR-0001](adr/0001-go-implementation-language.md): Go implementation language
- [ADR-0002](adr/0002-dispatcher-owns-persistence.md): Dispatcher owns persistence
- [ADR-0003](adr/0003-ndjson-journal-fsync.md): NDJSON journal
- [ADR-0004](adr/0004-directory-copies-for-fanout.md): Directory copy fanout isolation
