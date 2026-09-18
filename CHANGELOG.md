# Changelog

All notable changes to belay are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and belay follows
[semantic versioning](https://semver.org/spec/v2.0.0.html) — with the caveat
that only `pkg/belay` carries a compatibility promise. Everything under
`internal/` may change in any release.

## v0.1.4 — 2026-09-17

### Fixed

- **The lint gate no longer passes code in a directory whose new files git
  ignores.** v0.1.3 checked only that HEAD holds something in the workspace.
  With a rule such as `ws/*` plus `!ws/.gitkeep`, HEAD holds `.gitkeep`, but
  git ignores every file a run adds there. Their findings were hidden and the
  gate passed. belay now also asks git whether a new file in the workspace
  would be ignored, and counts every finding if so.
- The docs now also list what else scoping by line misses: changes inside a
  nested repository or submodule, and new files git ignores by name, such as
  under a `*_gen.go` rule. The troubleshooting steps now show both
  golangci-lint commands belay can run.

### Security

- **Each step's agent now gets only the tools belay allows it.** belay passed
  a step's tool list to Claude Code as `--allowedTools`, which only
  pre-approves those tools. Every other built-in tool stayed available, and
  Claude Code's own settings could approve it: `defaultMode: acceptEdits`,
  for example, approves edits. So the plan step, meant to be read-only,
  could edit files, and the code and fix steps could run shell commands.
  MCP servers from the user's or the repository's Claude Code configuration
  also added their tools. belay now also passes `--tools`, which removes
  every other built-in tool. It also passes `--strict-mcp-config`, which
  loads no MCP servers except the ones belay configures itself. The code and
  fix steps therefore no longer run commands such as `go test` themselves;
  belay's test step runs the tests. This needs Claude Code 2.1.186 or later.

## v0.1.3 — 2026-09-17

### Fixed

- **The lint gate no longer passes code it cannot scope.** v0.1.2 always
  passed `--new-from-rev=HEAD`. In a directory the enclosing repository
  ignores, that hid every finding and the gate passed. A fanout candidate
  under `.belay` is such a directory, though belay doesn't lint candidates
  yet. belay now asks git first. If HEAD doesn't hold the
  directory, or there is no repository, commit or `git`, every finding counts.
  belay says so in a warning, shown with `--verbose`, and in the gate's
  summary, which the fix step passes to the agent.
- **A repository's `.golangci.yml` can no longer change what the gate
  counts.** belay now overrides the `issues.new`, `new-from-rev`,
  `new-from-merge-base`, `new-from-patch` and `whole-files` settings. Left in
  place, `new-from-merge-base` and `whole-files` counted findings committed
  before the run, and `new-from-patch` could hide the run's own findings.
- The docs now list what scoping by line misses:
  - findings a change causes on lines it didn't touch, such as unchecked
    errors where a function that now returns one is called;
  - files git ignores;
  - commits made during a run;
  - findings golangci-lint's cache takes from another checkout with identical
    code.

  They also correct two v0.1.2 claims: that a fanout candidate would have
  every finding counted, and that the warning golangci-lint prints when it
  can't scope reaches belay's output.

## v0.1.2 — 2026-09-17

### Fixed

- **The lint gate now judges the run's change, not the repository's
  history.** With `review.mode: lint`, belay ran golangci-lint over every
  package and counted every finding. In a repository with existing lint
  debt, every run failed the gate whatever it changed, and the fix step then
  asked the agent to repair findings in files the run never touched. belay
  now passes `--new-from-rev=HEAD`, golangci-lint's own option for reporting
  only findings on lines that differ from the last commit. New files count
  as changed. A package that does not compile still fails the gate, changed
  or not.

  The scoping needs git on `PATH` and a repository with at least one commit.
  Without them, golangci-lint prints a warning and reports every finding, as
  before. Uncommitted edits made before the run count as part of its change.
  ESLint and Ruff still report every finding.

## v0.1.1 — 2026-09-17

### Fixed

- **Claude subscription tokens now reach Claude Code.** belay passed
  `ANTHROPIC_API_KEY` to the `claude` child process but dropped
  `CLAUDE_CODE_OAUTH_TOKEN`, the token `claude setup-token` issues to a Claude
  Pro, Max, Team or Enterprise subscription. A user signed in that way saw
  every run fail at the plan step: `claude` exited 1 in about a second, at no
  cost and with no error text. belay now passes the token and redacts its
  value from the journal, logs and recorded cassettes, as it already did for
  the API key.
- **A failed `claude` run now says why.** When the CLI cannot start a run, for
  example because it is signed out or its credential was rejected, it prints
  the reason as an `is_error` result on stdout, writes nothing to stderr, and
  exits 1. belay reported only the exit code and the command line. It now
  reports the CLI's reason, such as `Not logged in · Please run /login`,
  redacted like everything else it records.
- A dry run reports whether `CLAUDE_CODE_OAUTH_TOKEN` is set, and the
  "not signed in" warning no longer appears when it is.

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
