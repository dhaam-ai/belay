# ADR-0002: Dispatcher Owns All Persistence; Nodes Are Pure

## Status
Accepted

## Context
Early design debated where state mutation happens. Should nodes (plan, code, test, review) write directly to the journal and manifest, or should they return immutable results that the dispatcher persists?

This choice couples with crash-resume correctness. If a node crashes mid-write, how do we know whether its work is committed? If nodes own their own persistence, recovery logic lives in every node. If the dispatcher owns it, recovery is centralized and provable.

## Decision
Nodes are pure functions of (state, adapters) that return Result{Next, Status, Patch, Usage} and never touch state.json, manifest.json, or the journal. The dispatcher receives the result, applies the patch, writes to the journal, and fsync's.

## Consequences

### Positive
- **Crash-resume is provably correct**: The journal is the single source of truth. If a node crashes, its result never made it to the journal, so resume starts before that node's execution. No recovery logic per node.
- **Every node is table-testable**: Given frozen state and mocked adapters, call a node function and assert Result. No filesystem interaction, no hidden I/O.
- **Easier to reason about state**: The dispatcher is the only place where state changes. Bugs in node code cannot corrupt the journal.
- **Fanout isolation is trivial**: Each fanout candidate is independent; the dispatcher merges results only after all candidates finish.

### Negative
- **Nodes cannot stream results**: A node cannot incrementally write progress; it must buffer everything and return at the end. This means large file writes happen in memory first.
- **Dispatcher has two responsibilities**: Orchestration AND persistence. This couples concerns, though the interface isolation mitigates it.
- **Backward compatibility is harder**: If the Result type changes, old nodes cannot be upgraded in place; the dispatcher must know both schemas.

### Follow-ups
- Define Result as a stable public type in `pkg/graph/`, with documentation of backward-compatibility rules.
- Require every node to declare its Result schema and validate at dispatch time (defer to T46).
- Document that streaming large files requires pushing them to an external service (e.g., S3) or breaking the work into smaller Result returns.

## Alternatives Considered
- **Nodes own their persistence**: Each node writes directly to the journal and manifest. Simpler for node code. Lost because: (1) recovery logic duplicated per node, (2) impossible to reason about crash state — was the result written or not?, (3) fanout merges become complex (which candidate's state do we trust?).

## Revisit If
- Users report memory pressure from large Result buffers (> 100 MB). Streaming from nodes might become necessary.
- A conformance test fails due to dispatcher incorrectly applying patches (suggests the split is the wrong place).

## References
- Challenge #134 phase 5: "Merge Fanout Results"
- pkg/graph/result.go (T46)
