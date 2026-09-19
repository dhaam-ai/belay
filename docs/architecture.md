# Belay Architecture

Belay is a durable, resumable orchestrator for autonomous coding agents. This document describes the high-level design, component interactions, and key abstractions.

## Overview

Belay runs a graph of coding phases, in this order:

```
plan -> approve -> code -> write -> test -> fix -> review
```

Failed tests and failed quality gates both route back through fix, so the graph has cycles, and a run whose tests and gate pass never runs fix. Two more nodes, fanout and join, exist for best-of-N candidates, but no node routes to fanout, so no run reaches either one (see Fanout/Join below).

Each node in the graph is a pure function that returns a result; the dispatcher persists the result to an append-only journal. If belay crashes, the next invocation resumes from the last completed node.

The four product pillars are:

1. **Durable + Resumable**: Crash-resume from the last completed node, not from the beginning.
2. **Quality-Gated**: Code must pass linters (phase 6) or AI review (phase 7) before being marked done.
3. **Best-of-N Candidates** (not working yet): The aim is for fanout to give one goal N candidates and keep the best. The fanout and join nodes exist, but no run reaches them and nothing runs a candidate (see Fanout/Join).
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
- A node that started and never finished (a crash mid-node), or a `node_finished` that closed an interrupted attempt (status `aborted`, `next` naming the node itself): re-run that node at the next attempt
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
- `Isolator belay.Isolator`: workspace isolation, used only by the fanout node; the CLI injects none

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

**Fanout/Join**: The fanout and join nodes exist and are unit-tested (`internal/nodes/fanout`, `internal/nodes/join`), but no run reaches them:
1. No node returns Next="fanout". `nodes.Default` registers fanout and join only when `fanout.enabled` is true, and even then `nodes.Route` leaves them off the route, so turning fanout on changes nothing a run does.
2. The CLI passes the dispatcher no Isolator, and the fanout node refuses to run without one (`fanout.ErrNoIsolator`).
3. Called directly, as its tests call it, the fanout node checks that the budget can cover N candidates, creates one workspace per candidate through the Isolator, one after another, records each as a `state.Candidate`, and routes to join. It runs no candidate: no code, test or review step happens in a candidate's workspace, so every candidate's `TestPassed` stays false.
4. The join node picks a winner among the candidates whose tests passed, by the rule `fanout.select` names, records it in `state.Winner`, and ends the run. It copies nothing back: promotion is manual (ADR-0004). Given the candidates fanout records, it finds none that passed and reports no winner.

Running candidates is planned, and ADR-0019 is reserved for it.

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
- `fanout`: best-of-N candidates, including `fanout.isolator`, whose only value is `dircopy`. No run reaches the fanout node, so none of these settings changes what a run does

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
  ├→ NodeFunc (code)  → Result
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
- **code** (Agent): Uses the agent to implement the plan. Branches to write.
- **write** (Agent): Commits code changes (writes files, formats). Branches to test.
- **test** (TestRunner): Executes the test suite. If all tests pass, branches to review; if any fail, branches to fix.
- **fix** (Agent): Uses the agent to fix failing tests, and adds the files the repair reports changing to the run's list of changed files (state.Code.ChangedFiles). Branches back to test (bounded by attempt counter graph.give_up to prevent infinite loops).
- **review** (Linter or Reviewer): Runs static analysis or AI review. If gate passes, branches to END; if gate fails, branches to fix.
- **fanout** (Isolator): Creates one isolated workspace per candidate, one after another, and records them. It runs none of them. Branches to join. No node branches to fanout, so no run reaches it.
- **join**: Picks a winner among the candidates whose tests passed, by `fanout.select`, and records it in state.Winner. It copies nothing back; promotion is manual (ADR-0004). Branches to END, or fails the run when no candidate passed. Only fanout branches to join, so no run reaches it either.

**Terminal States**:
- StatusOK with Next="END": run completed successfully
- StatusFailed: a node returned an error or returned StatusFailed
- StatusAborted: budget exceeded, max steps reached, a node timeout, or the caller's context cancelled. `belay run` and `belay resume` set the dispatcher's `SuspendOnCancel`, so for them a cancellation (Ctrl-C, SIGTERM) closes the interrupted attempt as aborted, naming the node itself as `next`, and pauses the run; `belay resume` re-runs that node
- StatusPaused: human approval gate not granted; use `belay resume` to continue

If a node returns an error or an invalid Result, the dispatcher stops the run and records the failure. The user inspects the journal with `belay timeline`, fixes the underlying issue, and starts a new run: `belay resume` refuses a run that has failed.

## Isolation & Fanout

**Isolator Interface** (see `pkg/belay/isolate.go`): Abstracts workspace isolation so the graph is implementation-agnostic. Two methods:
- `Create(ctx, src, id string) (Workspace, error)`: Copy or prepare a workspace from src, identified by id. Returns a Workspace{ID, Dir, Ephemeral}.
- `Destroy(ctx, Workspace) error`: Clean up the workspace. Must idempotently handle a Workspace decoded from the journal after a process restart.

**Current Implementation** (ADR-0004): Plain directory copies (see `internal/isolate/dircopy`), which leave out `.git`, `node_modules`, `vendor` and similar directories (`dircopy.DefaultSkip`). It is unit-tested, but nothing in belay constructs it yet, so no run creates a candidate workspace. Future implementations could use git worktrees or containers.

**Promotion**: There is no merge-back code in belay. The join node records a winner and copies nothing back, because ADR-0004 accepted a manual merge as the price of directory copies. The `internal/nodes/join` package doc lists the steps a person would take.

## Adapters & Extension Points

Five pluggable interfaces in `pkg/belay` allow third-party implementations:

- **AgentBackend** (see `pkg/belay/agent.go`): LLM code generation. One method: Invoke(ctx, AgentRequest) → AgentResponse. The request carries the prompt, working directory, MCP config, and optional session ID for resumption. The response carries generated text, usage, and session ID for continuation. Implemented by internal/agent/claude (v0.1) and future Codex, Gemini implementations.

- **TestRunner** (see `pkg/belay/runner.go`): Test execution auto-detection and running. Two methods: Detect(dir) → bool (is this a project this runner can test?), Test(ctx, dir) → TestReport. Returns test pass/fail counts, failure details, and coverage. Implemented by internal/runner for Go, Node, Python. A missing toolchain returns an error wrapping belay.ErrToolchainMissing (not a TestReport with a failure).

- **Linter** (see `pkg/belay/linter.go`): Static analysis auto-detection and running. Two methods: Detect(dir) → bool, Lint(ctx, dir) → QualityReport. Like TestRunner, a missing toolchain is an error, not a failed gate. Implemented by internal/linter for golangci-lint, eslint, ruff.

- **Reviewer** (see `pkg/belay/quality.go`): Holistic quality assessment against a caller-supplied threshold. One method: Review(ctx, ReviewRequest) → QualityReport. Takes file list and FailOn severity. Implemented by internal/review for SonarQube (server or Docker mode) and AI review.

- **Isolator** (see `pkg/belay/isolate.go`): Workspace isolation. Two methods: Create(ctx, src, id) → Workspace, Destroy(ctx, ws) → error. Implemented by internal/isolate/dircopy for directory copies, which nothing in belay constructs yet.

## Error Handling & Recovery

**Error Categories** (see `pkg/belay/errors.go`):

1. **ToolchainMissing** (ErrToolchainMissing): A required binary is not installed (go, pytest, golangci-lint, sonar-scanner). The remedy is to install software, not to edit code. The graph routes this to a human, not to the fix loop.

2. **BudgetExceeded** (ErrBudgetExceeded): Run cost crossed its configured ceiling. Not retryable; the run must abort.

3. **GateFailed** (ErrGateFailed): A quality gate returned GateFail. This is not an error to the gate itself (it worked, returned a verdict), but a graph-level error for callers that want to exit non-zero. The node returns (QualityReport{Gate: GateFail}, nil); the graph branches to fix, not to error handling.

4. **Unsupported** (ErrUnsupported): A call was valid but the adapter cannot do it (e.g., resuming a session on a backend with no session support). Permanent, not retryable.

5. **Other errors**: Network timeouts, context cancellation, parse failures, missing files, etc. Most are permanent and end the run. Context cancellation under `belay run` or `belay resume` pauses the run instead, and `belay resume` continues it.

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

Nothing runs candidates, in parallel or otherwise. The fanout node creates candidate workspaces one after another in one goroutine and runs none of them; its package doc explains why running candidates concurrently belongs to the dispatcher, not to a node.

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
