# Changelog

All notable changes to belay are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and belay follows
[semantic versioning](https://semver.org/spec/v2.0.0.html) — with the caveat
that only `pkg/belay` carries a compatibility promise. Everything under
`internal/` may change in any release.

## Unreleased

### Changed

- **belay v0.1.x can no longer resume a run this version has saved.** belay
  ignores fields it does not know in a run's state.json and manifest.json,
  and saves the run without them. Once runs carry settings that change what
  they do, an older belay resuming one would drop those settings without a
  word. Both files now carry a `requires` list naming what a run depends on,
  and a belay that lacks something on the list refuses the run by name and
  says to upgrade. Nothing adds to the list yet. v0.1.x never reads it, so
  both files also move to schema version 2, which v0.1.x refuses: its
  `belay runs` shows such a run as "unsupported manifest version 2", and its
  `belay resume` does not offer it. Runs started by v0.1.x still resume, and
  are saved at version 2 from then on. With two belay installs, resume runs
  with the newer one. ADR-0011 records the decision.

### Fixed

- **`--config` help no longer says the file replaces belay.yaml.** It said
  belay reads the file "instead of" the belay.yaml it would find, but belay
  reads your user config and the workspace's belay.yaml as well, and merges
  the `--config` file on top of them, setting by setting. A setting you
  left out of that file still came from the others. The help, and the
  README's configuration section, now say so.
- **A run a newer belay wrote now says it needs a newer belay.** A run
  whose manifest.json requires something this belay does not have, or is
  at a schema version above the one it reads, is not damaged, but
  `belay runs` listed it as "manifest.json is damaged" or "unsupported
  manifest version", and `belay resume` acted as if it were not there:
  named, it said there was no such run, and left to choose, it continued
  an older run instead. `belay runs` now lists it as "needs a newer belay",
  with what it requires. `belay resume` refuses it with an error that says
  what it requires and to upgrade belay, and while such a run is in the
  workspace, `belay resume` with no run id will not choose a run for you;
  name the run instead. A version older than any belay reads keeps its
  "unsupported" wording.
- **Files changed in the fix loop are now recorded with the rest of the
  run's change.** belay keeps a list of the files a run changed, taken from
  the files the agent reports after writing the code. When a test or the
  quality gate failed and the agent repaired it, the files that repair
  touched were never added, so the list belay passes to a reviewer, and
  shows the agent on its next repair, left them out. belay now asks the
  agent which files each repair changed and adds them to the list, each
  file once. A repair that reports a path outside the repository fails the
  run, as the first change already did.
- **Ctrl-C now leaves a run that `belay resume` continues.** `belay run
  --help` promised this, but Ctrl-C, or a SIGTERM, ended the run as
  aborted: belay printed `belay resume <id>`, and `belay resume` then
  refused the run as "already stopped for good". belay now pauses the run
  at the step it was in. `belay resume` runs that step again from its
  start, and not the steps that had finished. If Ctrl-C lands while belay
  is at the approval gate, resuming asks for approval again rather than
  approving the plan for you. An interrupted `belay run` still exits with
  code 3. A second Ctrl-C still quits at once, which leaves the run as a
  crash does, and `belay resume` continues that too.
- **belay no longer tells you to resume a run it cannot resume.** After
  `graph.max_steps`, `graph.node_timeout`, or the budget ceiling with
  `budget.on_exceed: abort`, the closing report offered `belay resume
  <id>`, which refuses such a run. It now says the run has ended. After
  the step cap or a timeout it names the setting to raise and the command
  that starts a new run; otherwise it points at `belay timeline`.
  docs/troubleshooting.md and docs/architecture.md also suggested `belay
  resume` for runs that had completed or failed, and no longer do.
- **A crash while belay recovers from a crash no longer ends the run.**
  After a crash belay closes the step that was cut off and runs it again.
  If belay died again between the two, the next attempt took the run for
  aborted and refused it. It now runs the step again, and numbers it as
  the next attempt instead of reusing the number of the one it replaces.
- **A `belay run` that fails while creating its run no longer leaves a run
  that `belay resume` picks up.** belay wrote the run's manifest.json before
  its state.json. If writing state.json failed, or belay was killed between
  the two, the manifest said the run was running: `belay resume` chose it
  and then failed to load it, and `belay runs` listed it as resumable. belay
  now writes state.json first and the manifest last, so a run exists only
  once both files do. A run whose creation was cut short shows in
  `belay runs` as unreadable, with no manifest.json, and `belay resume`
  never offers it.
- **The configuration reference now states the defaults and values belay
  actually uses.** docs/config.md had drifted on nine defaults, among them
  `agent.max_turns` (30, not 5), `graph.max_steps` (60, not 20),
  `graph.node_timeout` (15m, not 10m) and `budget.max_usd` (5, not 10). It
  listed values the loader rejects for `agent.mode`, `test.runner`,
  `review.fail_on` and `review.ai.mcp.mode`, and documented
  `review.sonar.quality_gate_wait` and `quality_gate_timeout` as durations
  when they are a boolean and a number of seconds. It also said unknown keys
  are only warnings, when they are errors, and that environment variables
  and flags override settings, when only a `--config` file does. Tests now
  check every documented setting's type, default and valid values against
  the code, and fail when a setting has no entry.
- **The docs no longer say fanout works.** The README said parallel
  candidate generation works and only merging the winner back was manual,
  and docs/architecture.md said the fanout node runs each candidate from the
  code step onward. Neither was true. The fanout and join nodes exist and
  are unit-tested, but no step in belay's graph leads to fanout, so no run
  reaches either one, and `fanout.enabled: true` changes nothing a run does.
  The fanout node only prepares a workspace per candidate; nothing runs a
  candidate. The README, docs/architecture.md, docs/troubleshooting.md,
  docs/config.md, docs/adapters.md and examples/naive now say so. Running
  candidates is planned, and ADR-0019 is reserved for it.

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
