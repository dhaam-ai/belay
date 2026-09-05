# Architecture Decision Records

This directory contains architecture decisions for belay, recorded to capture the reasoning behind non-obvious choices. Each ADR documents the context, decision, consequences, and alternatives considered.

## Index

| # | Title | Status | Summary |
|---|-------|--------|---------|
| [0001](0001-go-implementation-language.md) | Go as Implementation Language | Accepted | Single static binary, real concurrency for fanout, author depth. Cost: more JSON boilerplate, less mature MCP client story. |
| [0002](0002-dispatcher-owns-persistence.md) | Dispatcher Owns All Persistence; Nodes Are Pure | Accepted | Nodes return immutable results; dispatcher persists. Enables crash-resume and testable node functions. Cost: nodes cannot stream results. |
| [0003](0003-ndjson-journal-fsync.md) | Append-Only NDJSON Journal with Fsync Per Record | Accepted | Source of truth is an append-only journal; resume replays from journal. Crash-safe, auditable. Cost: slow reads on long runs, torn final line must be tolerated. |
| [0004](0004-directory-copies-for-fanout.md) | Directory Copies for Fanout Isolation, Behind Isolator Interface | Accepted | Plain `cp -r` per candidate, not git worktrees. Works on non-git targets, trivially understandable. AGAINST recommendation. Cost: O(N × repo size) disk, manual merge of winner. |
| [0005](0005-claude-code-only-backend-v0.md) | Claude Code as Only Shipped Backend in v0.1, with AgentBackend Exported | Accepted | Only Claude Code shipped in v0.1; interface exported for future backends. AGAINST recommendation. Cost: "CLI-agnostic" is design promise, not proven fact. |
| [0006](0006-single-quality-report-contract.md) | Single QualityReport Contract Shared by Deterministic and AI Reviewers | Accepted | Both linters and AI reviewers return the same type. Graph is reviewer-agnostic. Cost: lost semantic richness; Raw field is opaque JSON. |
| [0007](0007-budget-ledger-day-one.md) | Budget Ledger and Cost Caps on Day One | Accepted | Budget controls track spend and abort if cap exceeded. Fanout is safe by default. Cost: adds operational complexity; requires understanding budget fields. |
| [0008](0008-local-linters-default.md) | Local Linters as Default Quality Gate; SonarQube Opt-In | Accepted | Zero-setup linters are default; SonarQube requires infrastructure. Cost: two review paths to maintain; linters miss semantic bugs. |
| [0009](0009-record-replay-cassettes.md) | Record/Replay Cassettes for Zero-Cost CI | Accepted | Tests record/replay MCP and Claude calls. CI costs zero. Cost: cassettes can drift; maintainer live-smoke test gates releases. |
| [0010](0010-cli-designed-for-how-people-think.md) | The CLI is designed against a cognitive model, not a feature list | Accepted | Commands are designed against the six cognitive systems in Whalen's *Designing for How People Think*, so parallel contributors produce one coherent tool. |

## Reading ADRs

Each ADR follows a standard template:

- **Status**: Proposed, Accepted, Deprecated, or Superseded
- **Context**: What forced the decision; alternatives considered
- **Decision**: What was chosen
- **Consequences**: Positive, negative, follow-ups
- **Alternatives Considered**: Real options evaluated and why they lost
- **Revisit If**: Concrete signals that would justify reconsideration

ADRs document the *why*, not just the *what*. Some decisions were made *against* recommendations (see ADRs 0004 and 0005); those are recorded explicitly.

## Updating ADRs

When an ADR is superseded by a later decision:

1. Change its Status to `Superseded by ADR-NNNN`
2. Add a one-paragraph note at the top explaining the change
3. Create a new ADR with the fresh decision and context

Never delete or rewrite an ADR after it is accepted. Git history preserves old decisions; future maintainers may need to understand why a choice was made, then changed.
