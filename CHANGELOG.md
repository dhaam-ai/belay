# Changelog

All notable changes to belay are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and belay follows
[semantic versioning](https://semver.org/spec/v2.0.0.html) — with the caveat
that only `pkg/belay` carries a compatibility promise. Everything under
`internal/` may change in any release.

## v0.1.0 — 2026-09-06

First release. belay runs an autonomous coding agent as a state-machine graph
whose state is journaled after every step, so a crashed run resumes from the
last completed node instead of restarting.

### Added

- **`belay run`** — plan, approve, code, write, test, fix, review. Stops at the
  plan for a human to read, and prints its budget ceiling before it spends
  anything. `--dry-run` resolves everything and creates nothing.
- **`belay resume`** — continues a paused or crashed run. With no run id it
  picks the most recent resumable one and says which it picked.
- **`belay runs`** — lists this workspace's runs, newest first, with status,
  age, goal text and cost. A damaged run directory is listed as `unreadable`
  rather than silently omitted.
- **`belay timeline`** — one run's execution order, timing and per-node cost,
  with loop iterations numbered (`test #2/3`) so a fix loop is visible rather
  than inferred.
- **Durable, resumable runs.** An append-only NDJSON journal, fsynced per
  record. The dispatcher applies a node's state patch *before* journaling that
  it finished, so a crash re-runs a node rather than skipping one whose effects
  never landed.
- **Quality-gated loop.** A failing test suite and a failed quality gate both
  route back to `fix`, bounded by `graph.give_up`. golangci-lint, ESLint and
  ruff ship as gates; SonarQube is available via CLI or its MCP server.
- **Budget ceilings.** Token and USD spend is tracked per node and checked
  before each one. Estimated costs are marked as estimates.
- **Polyglot targets.** Go, Node and Python test runners and linters are
  detected automatically; any command can be configured explicitly.
- **Best-of-N.** `fanout` prepares N isolated candidate workspaces and `join`
  selects a winner by a configurable rule. Off by default.

### Known limitations

- **One agent backend.** Only Claude Code ships. `belay.AgentBackend` is
  exported and stable so others can be written, but none is implemented or
  tested (ADR 0005).
- **Unix only.** `internal/exec`, `internal/runner` and `internal/linter` are
  `//go:build unix`. Windows is not supported and no Windows artifact is built.
- **Promoting a fanout winner is manual.** Candidates are directory copies, so
  belay records which one won and leaves merging it to you (ADR 0004).
- **A cassette replays the agent, not the workspace.** Replay serves recorded
  responses; it does not reproduce the file edits the agent made, so a replayed
  run diverges as soon as a real test runner reads the workspace (ADR 0009,
  amended).
- **No published fix-rate figure.** `test/fixrate` is the instrument that would
  produce one against the seeded-defect catalogue; no number is claimed here.
