# Belay Architecture

Belay is a durable, resumable orchestrator for autonomous coding agents. This document describes the high-level design, component interactions, and key abstractions.

## Overview

Belay runs a graph of nodes, in this order:

```
plan -> approve -> code -> write -> test -> fix -> review
```

Failed tests and failed quality gates both route back through fix, so the graph has cycles, and a run whose tests and gate pass never runs fix. Two more nodes, fanout and join, exist for best-of-N candidates, but no node routes to fanout, so no run reaches either one (see Fanout/Join below).

Each node returns a result, and the dispatcher persists it: the result's changes to state.json, then the node's completion to an append-only journal. If belay crashes, `belay resume` continues after the last completed node, running again the one that was cut off.

The four product pillars are:

1. **Durable + Resumable**: Crash-resume from the last completed node, not from the beginning.
2. **Quality-Gated**: Code must pass the quality gate, a linter or a SonarQube scan, before the run can finish.
3. **Best-of-N Candidates** (not working yet): The aim is for fanout to give one goal N candidates and keep the best. The fanout and join nodes exist, but no run reaches them and nothing runs a candidate (see Fanout/Join).
4. **CLI-Agnostic**: Agent backend is pluggable (Claude Code in v0.1; Codex and Gemini via AgentBackend interface).

## Architecture Layers

### Layer 1: State & Persistence

**State**: A run keeps two JSON documents in `.belay/runs/<run-id>/`. `state.json` is the blackboard every node reads: the accumulated patches of the nodes that have finished, such as the plan, the files changed, the latest test and review results and the fix loop's count (see `internal/state/state.go`). `manifest.json` is the run's identity and lifecycle: its goal, status, current node and step count, the configuration it started with, and the budget spent so far (see `internal/state/manifest.go`). The dispatcher saves state.json when it accepts a node's result and manifest.json after the node's completion is journaled, and reads both on resume. Before each node execution it also saves the blackboard it is about to hand the node as a write-once snapshot, `snapshots/<seq>-<node>.json`, keyed on the execution's `node_started` record, with a fingerprint of the workspace beside it (see `internal/state/snapshot.go`). A node re-run after a crash is handed that snapshot, and a fork will start from one.

**Journal**: An append-only NDJSON file (`.belay/runs/<run-id>/journal.ndjson`) records every state transition. Each line is a Record (see `internal/journal/record.go`):
```
{"seq":1,"event":"run_started","ts":"2026-08-29T10:31:00Z"}
{"seq":2,"attempt":1,"node":"plan","event":"node_started","ts":"2026-08-29T10:31:00.4Z"}
{"seq":3,"attempt":1,"node":"plan","event":"node_finished","status":"ok","next":"approve","usage":{"input_tokens":0,"output_tokens":0,"usd":0,"estimated":false}}
```
Records are written with O_APPEND|O_WRONLY, synced after every append, and never rewritten. A crash during one record can corrupt only that record; earlier records remain durable. The journal is the sole source of truth for where a run is: it records every node's start and finish, and a node counts as done only once its `node_finished` record is written (see ADR-0003).

**Resume**: On startup (or `belay resume`), the dispatcher calls `journal.ResolveStart(records)` to read the journal and compute the next node to run (see `internal/journal/journal.go`). The algorithm uses a decision table that handles:
- A normal run ending in `node_finished`: resume at that node's `next` field
- A node that started and never finished (a crash mid-node), or a `node_finished` that closed an interrupted attempt (status `aborted`, `next` naming the node itself): re-run that node at the next attempt
- A paused run (ending in `run_paused`): refuse unless explicitly resumed with `belay resume`
- A run ending in a terminal state (`run_completed`, `run_failed`, `run_aborted`): refuse to resume (it is finished)
- A half-written final record (crash mid-append): `journal.Open` discards it and heals the file before `ResolveStart` reads the journal, so the decision is made from the last complete record

Only the budget ledger is recomputed from the journal. On resume the dispatcher reads state.json and manifest.json as they were last saved, and the journal decides where to resume. The one exception is a node run again at a later attempt: it is handed the snapshot taken before its first attempt instead of state.json, which may already hold the patch of the attempt that crashed. The write order is what keeps the blackboard right: a node's changes reach state.json before its completion reaches the journal (see *Why Patch-Then-Journal Matters* below), so the journal never records a finished node whose changes are missing. The manifest has no such protection. It is saved after the journal record, so a crash between the two leaves it one node behind, its step count and budget snapshot missing that node. The step count is left that way. The budget is not: the dispatcher rebuilds the ledger from the usage on every `node_finished` record in the journal, and uses the manifest's snapshot only as a floor the total never drops below, so a resumed run cannot spend that node's cost a second time.

### Layer 2: Node Execution

**Nodes**: Each graph node (plan, code, review, etc.) implements the Node interface, which takes a RunContext and returns a Result (see `internal/graph/node.go`). A node never writes state.json, manifest.json or the journal: every change it makes to the blackboard travels in its Result's Patch. It is not kept off the filesystem, though. Through its RunContext it writes artifacts and per-step evidence under the run directory (the plan, diffs, reports, the prompt it sent and the reply it got); the agent a node calls edits the workspace; and the write, test and review nodes read the workspace or run tools in it. Given a frozen RunContext and fake adapters, a node is table-testable.

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
- `VCS vcs.Repository`: commits and merges, used only by the promote node of a run whose output is a branch; the CLI injects none yet, because nothing selects branch output. It is an `internal/vcs` interface, not a `pkg/belay` one, because git is its only implementation.

Beside the adapters, RunContext carries `Output`, how the run delivers its result. It is read from the run's manifest, where it was pinned when the run was created, never from live configuration.

All adapters are injected at dispatcher creation time, making the entire graph backend-agnostic (ADR-0005).

### Layer 3: Dispatcher & Graph

**Dispatcher.Run()**: The main orchestration loop (see `internal/graph/dispatcher.go`). It asks `journal.ResolveStart` once, on entry, where to start (see Resume above); after that, the next node is the one the last Result named. For each step:
1. Check the budget ceiling and `graph.max_steps`, before anything is spent
2. Run the node with a timeout (`graph.node_timeout`, 15 minutes by default)
3. Validate the Result: its Status is set, and a successful Result names its Next
4. **Apply the Patch to state**, then **write node_finished to the journal** (this order is critical; see below)
5. Save the manifest: the step count, the next node, and the budget ledger with this node's Usage added
6. Continue with the node the Result named, where a name no node has fails the run; a Result that pauses, fails or finishes the run ends the loop instead

**Why Patch-Then-Journal Matters (ADR-0002)**: The dispatcher applies patches and journals *after*, not before. If it journalled first:
- A crash between journal and patch means a durable record exists with a Next node but no state to reach it
- Resume would skip that node (the patch never landed) and downstream nodes would read a State missing the skipped node's output
- The run returns wrong results, silently — not a failure the user notices

By applying first, a crash can leave an applied patch with no journal record, but a crash can never leave a journal record whose patch did not land. Resume then runs the node again, handed the blackboard snapshot its crashed attempt was handed, so it computes its patch from the same input rather than on top of its own output — the fix node would otherwise count one repair twice — and landing that patch a second time is harmless because every Patch field is an absolute set.

**Graph Definition**: The graph is built in code, not read from config. `nodes.Default` (`internal/nodes/wire.go`) registers plan, approve, code, write, test, fix and review, and `nodes.Route` gives the order a run walks them in, leaving approve off when `graph.approval` is false. No edge list exists: each node names its successor in its Result's `Next`, and the dispatcher looks that name up in the registry. A name no node has fails the run with `graph.ErrUnknownNode`, and `graph.End` finishes it. belay.yaml changes the graph in two ways only: `graph.approval` decides whether plan routes to approve or straight to code (approve stays registered either way, so a run started with the gate can still be resumed at it), and `fanout.enabled` registers fanout and join, off the route.

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

The graph acts only on `Gate`: if GateFail, the run branches to the `fix` node (bounded by a loop counter); if GatePass, the run finishes; if GateError, the run fails (the tool could not reach a verdict, not "the code is bad").

**Budget Ledger** (see `internal/budget/budget.go`): Day-one constraint (ADR-0007). Every node's Usage is added to a cumulative budget. Before each node the dispatcher checks the total against `budget.max_usd` and `budget.max_tokens` in belay.yaml. Once either is exceeded, `budget.on_exceed` decides: `abort`, the default, stops the run with an error wrapping belay.ErrBudgetExceeded; `warn` logs the overrun and carries on. `abort` pauses the run rather than ending it, so raising the ceiling and running `belay resume` continues it with the spend it has already made still counted; the limits are read from belay.yaml on every dispatch, unlike the shape of the graph, which is pinned when the run is created.

Usage.Estimated reports whether USD was computed from actual backend costs or estimated from a fallback table (belay.Usage.Estimated); callers can log differently for estimated costs.

### Layer 5: CLI & Configuration

**Config** (see `internal/config/config.go` and docs/config.md): belay.yaml defines:
- `agent`: which backend to use (e.g., "claude-code")
- `review`: quality-gate mode ("lint", "sonar", "ai"), the severity that fails the gate (fail_on), and the settings for the SonarQube scanner and the AI reviewer
- `test`: test toolchain (auto-detected or explicit)
- `graph`: the approval gate, fix-loop limit (give_up), max total steps, node timeout
- `budget`: spend ceilings in USD and tokens, and what to do on reaching one
- `fanout`: best-of-N candidates, including `fanout.isolator`, whose only value is `dircopy`. No run reaches the fanout node, so none of these settings changes what a run does

**CLI** (`cmd/belay`, implemented in `internal/cli`):
- `belay run "<what you want built>"`: start a new run. `--workspace` names the repository, and `--config` adds a settings file on top of belay.yaml
- `belay resume <run-id>`: continue a paused or crashed run
- `belay plan [run-id]`: read the plan a run stopped at; `--regenerate "<guidance>"` records a request for a new one and continues the run, so the plan node writes it
- `belay runs`: list the workspace's runs
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

## State Machine & Nodes

The run progresses through a directed graph of nodes (see `internal/graph/node.go` for each). Each node:
1. **Receives** RunContext with current state, config, adapters, and run metadata
2. **Executes** work (calls adapters) and returns a Result
3. **Persists** the Result (dispatcher writes to journal and applies Patch)
4. **Advances** to the next node named in Result.Next, or END if terminal

**Node Descriptions**:

- **plan** (Agent): Uses the agent to propose a plan from the goal. Returns a plan summary and branches to approve, or straight to code when `graph.approval` is false.
- **approve** (Human Gate): Awaits human approval before proceeding. Returns paused if not approved, or continues to code.
- **code** (Agent): Uses the agent to implement the plan. Branches to write.
- **write**: Checks the files the agent reported changing against the workspace, refusing any path outside it, and records them in state.Code.ChangedFiles with a diff artifact. It never modifies the workspace: the agent has already edited the files in place. Branches to test.
- **test** (TestRunner): Executes the test suite. If all tests pass, branches to review; if any fail, branches to fix.
- **fix** (Agent): Uses the agent to fix failing tests, and adds the files the repair reports changing to the run's list of changed files (state.Code.ChangedFiles). Branches back to test. Reached again after `graph.give_up` repairs, it fails the run instead of repairing, so the loop cannot run forever.
- **review** (Linter or Reviewer): Runs the quality gate, a linter or a SonarQube scan. If gate passes, branches to END; if gate fails, branches to fix.
- **fanout** (Isolator): Creates one isolated workspace per candidate, one after another, and records them. It runs none of them. Branches to join. No node branches to fanout, so no run reaches it.
- **join**: Picks a winner among the candidates whose tests passed, by `fanout.select`, and records it in state.Winner. It copies nothing back; promotion is manual (ADR-0004). Branches to END, or fails the run when no candidate passed. Only fanout branches to join, so no run reaches it either.

**Terminal States**:
- StatusOK with Next="END": run completed successfully
- StatusFailed: a node returned an error or returned StatusFailed
- StatusAborted: a node returned StatusAborted, ending the run
- StatusPaused: the run is waiting and `belay resume` continues it. Four things pause a run: the human approval gate; the caller's context being cancelled, for which `belay run` and `belay resume` set the dispatcher's `PauseOnCancel`, so a Ctrl-C or SIGTERM closes the interrupted attempt as aborted, naming the node itself as `next`, and `belay resume` re-runs that node; and belay's own limits — the budget ceiling, `graph.max_steps` and `graph.node_timeout` — each of which records in its note the setting to raise. The invocation still exits 3 for a pause that was not the graph's own choice, so a script can tell the two apart

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

2. **BudgetExceeded** (ErrBudgetExceeded): Run cost crossed its configured ceiling. Retrying it unchanged only meets the ceiling again; the run is paused, and raising `budget.max_usd` or `budget.max_tokens` and resuming continues it.

3. **GateFailed** (ErrGateFailed): A quality gate returned GateFail. This is not an error to the gate itself (it worked, returned a verdict), but a graph-level error for callers that want to exit non-zero. The node returns (QualityReport{Gate: GateFail}, nil); the graph branches to fix, not to error handling.

4. **Unsupported** (ErrUnsupported): A call was valid but the adapter cannot do it (e.g., resuming a session on a backend with no session support). Permanent, not retryable.

5. **Other errors**: Network timeouts, context cancellation, parse failures, missing files, etc. Most are permanent and end the run. Context cancellation under `belay run` or `belay resume` pauses the run instead, and `belay resume` continues it.

**Recovery on Crash**: If belay crashes mid-run:
1. The journal file may have a half-written final record (torn tail)
2. On restart, `journal.Open` discards the incomplete record and rewrites the file atomically without it
3. `journal.ResolveStart` names where to resume: the `next` of the last `node_finished` record, or, when the last node started and never finished (or its attempt was closed as interrupted), that node again at the next attempt
4. Node patches are absolute sets, not deltas, so re-running a node whose patch landed before the crash but whose completion was never journaled is safe

**User Recovery**: Users can:
- `belay resume <run-id>`: continue a paused or crashed run (refusing runs already in terminal states)
- `belay plan [run-id]`: read the plan a run stopped at, or ask for a different one with `--regenerate`
- `belay timeline <run-id>`: inspect the journal and manifest to diagnose failures
- `belay run` again once an external cause is fixed (e.g., a network outage): a run that failed cannot be continued, so the retry is a new run, and it starts from the working directory as the failed run left it

## Performance & Concurrency

Nothing runs candidates, in parallel or otherwise. The fanout node creates candidate workspaces one after another in one goroutine and runs none of them; its package doc explains why running candidates concurrently belongs to the dispatcher, not to a node.

The dispatcher is single-threaded; it runs one node at a time. This simplifies state management and makes resuming easy to reason about.

## Testing & Conformance

**Test Layout**: Each package has its own test suite:
- Node tests (internal/nodes/*/*_test.go): table-driven tests of each node's logic against mocked adapters (pkg/belay/belaytest fakes)
- Adapter tests (internal/linter/*_test.go, internal/runner/*_test.go, etc.): verify adapters parse tool output correctly, using fixtures instead of invoking real tools
- Dispatcher tests (internal/graph/*_test.go): the dispatcher and registry, driven by scripted fake nodes
- End-to-end tests (test/e2e, build tag `e2e`): the real dispatcher and the nodes `nodes.Default` wires, on a real run directory, with belaytest fakes for the agent, test runner and linter

**Cassette Recording** (ADR-0009): Agent calls can be recorded to a cassette and replayed. With `agent.mode: record`, belay calls the agent for real and saves every exchange to `<workspace>/.belay/cassette.json`, or the file `--cassette` names; with `agent.mode: replay` it answers every agent call from the cassette, starting no agent process and spending nothing. `internal/agent/replay` implements this as a decorator over `belay.AgentBackend`, so only agent calls are recorded, not test or linter runs.

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
