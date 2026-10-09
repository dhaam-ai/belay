# Architecture Decision Records

This directory contains architecture decisions for belay, recorded to capture the reasoning behind non-obvious choices. Each ADR documents the context, decision, consequences, and alternatives considered.

## Index

| # | Title | Status | Summary |
|---|-------|--------|---------|
| [0001](0001-go-implementation-language.md) | Go as Implementation Language | Accepted | Single static binary, real concurrency for fanout, author depth. Cost: more JSON boilerplate, less mature MCP client story. |
| [0002](0002-dispatcher-owns-persistence.md) | Dispatcher Owns All Persistence; Nodes Are Pure | Accepted | Nodes return immutable results; dispatcher persists. Enables crash-resume and testable node functions. Cost: nodes cannot stream results. |
| [0003](0003-ndjson-journal-fsync.md) | Append-Only NDJSON Journal with Fsync Per Record | Accepted | Source of truth is an append-only journal; resume replays from journal. Crash-safe, auditable. Cost: slow reads on long runs, torn final line must be tolerated. |
| [0004](0004-directory-copies-for-fanout.md) | Directory Copies for Fanout Isolation, Behind Isolator Interface | Superseded by ADR-0012 for git targets | Plain `cp -r` per candidate, not git worktrees. Works on non-git targets, trivially understandable. AGAINST recommendation. Cost: O(N × repo size) disk, manual merge of winner. |
| [0005](0005-claude-code-only-backend-v0.md) | Claude Code as Only Shipped Backend in v0.1, with AgentBackend Exported | Accepted | Only Claude Code shipped in v0.1; interface exported for future backends. AGAINST recommendation. Cost: "CLI-agnostic" is design promise, not proven fact. |
| [0006](0006-single-quality-report-contract.md) | Single QualityReport Contract Shared by Deterministic and AI Reviewers | Accepted | Both linters and AI reviewers return the same type. Graph is reviewer-agnostic. Cost: lost semantic richness; Raw field is opaque JSON. |
| [0007](0007-budget-ledger-day-one.md) | Budget Ledger and Cost Caps on Day One | Accepted, amended 2026-09-19 | Budget controls track spend and abort if cap exceeded. Fanout is safe by default. Every step is charged what it spent, however it ended, a resume counts spend from the journal, and reaching the ceiling pauses the run instead of ending it. Cost: adds operational complexity; requires understanding budget fields. |
| [0008](0008-local-linters-default.md) | Local Linters as Default Quality Gate; SonarQube Opt-In | Accepted | Zero-setup linters are default; SonarQube requires infrastructure. Cost: two review paths to maintain; linters miss semantic bugs. |
| [0009](0009-record-replay-cassettes.md) | Record/Replay Cassettes for Zero-Cost CI | Accepted, amended 2026-09-06 | Tests record/replay MCP and Claude calls. CI costs zero. Cost: cassettes can drift; maintainer live-smoke test gates releases. |
| [0010](0010-cli-designed-for-how-people-think.md) | The CLI is designed against a cognitive model, not a feature list | Accepted | Commands are designed against the six cognitive systems in Whalen's *Designing for How People Think*, so parallel contributors produce one coherent tool. |
| [0011](0011-cross-version-safety-for-run-state.md) | Cross-version safety for run state | Accepted | Schema version 2 plus a `requires` list of capabilities, so a binary that cannot honour a run's control fields refuses it instead of silently dropping them. Cost: v0.1.x cannot resume a run a later belay has saved. |
| [0012](0012-git-native-run-output.md) | Git worktrees for isolation, and a run's result on its own branch | Accepted | A run in a git repository works in its own worktree on `belay/<run-id>`, never edits the user's checkout, and commits once after the quality gate passes; `--pr` opens a draft pull request. Fanout candidates are worktrees on sibling branches. Supersedes 0004 for git targets; dircopy stays as the non-git fallback. Cost: a full checkout of the tree per worktree, a git 2.38 floor, and two output modes to support. |
| 0013 | A second agent backend | Reserved (slice 5) | Will amend 0005. |
| [0014](0014-model-routing-and-the-policy-gate.md) | Per-node model routing, and the policy gate | Accepted | `agent.models` routes a node to a model of its own and is read live, because it changes no node and no edge. `policy.*` declares guardrails a change must obey whatever its tests say, and is pinned in the manifest with a survey of the workspace, because a gate needs a "before" and only the run that took it has one. Violations reuse the ADR-0006 quality report. Cost: a policy applies to the next run and not this one, `max_changed_lines` counts whole files, and limits/routing/shape are three categories of setting to keep straight. |
| [0015](0015-interactive-plan-viewer.md) | The interactive plan viewer runs outside the graph | Accepted | `belay plan` opens a full-screen viewer to read, edit, reorder, approve or replace a plan, on a terminal only. It runs between two dispatches, so the approve gate still never reads a terminal; editing goes through plandoc, saving through the gate's own rule and path, approving through the resume path. Cost: nineteen modules, all MIT or BSD-3. |
| [0016](0016-archiving-the-approved-plan-into-the-repository.md) | A finished run moves its plan into the repository | Accepted | A completed run's plan moves from `.belay/runs/<id>/artifacts/` to `docs/plans/<run-id>-<slug>.md`: into the commit for a branch run (written by `promote` before it commits), into the workspace for a run in place. Place, record, then remove, so a crash anywhere converges and never loses the plan. Cost: belay writes a file into the repository nobody named, and two settings change every config digest. |
| 0017 | Forking a run to re-run one node, and the goal queue | Reserved (slice 3) | |
| [0018](0018-swebench-corpus-and-the-control-arm.md) | SWE-bench Verified, scored by its own harness, against a control arm | Accepted | The official harness's verdict is the score and the ported rule a cross-check; a resumable corpus never re-runs or re-pays a finished cell; both arms work in the record's own image; aggregates only while the dataset's licence is unknown. Cost: the belay arm's test step is the harness's, the regression test is never scored, and the full run takes days. |
| 0019 | Executing fanout candidates | Reserved (slice 7) | |

Reserved numbers were assigned per slice up front, so parallel work never collides on a number. A reserved row gains a link, a title and a status when its ADR lands.

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
