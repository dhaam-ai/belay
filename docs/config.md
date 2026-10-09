# Belay Configuration

Belay is configured by a `belay.yaml` file in the repository it works on. This document describes every field and its effects.

## File Location

belay builds its configuration from up to three files, merged onto its built-in defaults. Each later file overrides the earlier ones:

1. `$XDG_CONFIG_HOME/belay/config.yaml`, or `~/.config/belay/config.yaml` when `XDG_CONFIG_HOME` is not set: your own settings, for every repository.
2. `belay.yaml` in the workspace (`--workspace`, `.` by default). For example, `belay run --workspace ../other-repo "your goal"` reads `../other-repo/belay.yaml`.
3. The file named by `--config`, if you pass one.

Merging is per field, not per file: a file that sets only `budget.max_usd` keeps every other setting from the layers below it, including the other `budget` settings. A file that is missing is skipped, except the one `--config` names, which must exist. With no files at all, belay runs on the defaults this document lists.

The [`policy`](#policy) section is the one exception. Its rules combine across the files instead of the later one winning, so that no file can remove a guardrail a file below it declared.

## Top-Level Fields

```yaml
version: 1
agent: {backend: claude-code, model: sonnet, max_turns: 30, mode: live}
graph: {give_up: 3, max_steps: 60, node_timeout: 15m, approval: true}
test: {runner: auto, custom_cmd: ""}
review: {mode: lint, fail_on: major}
budget: {max_usd: 5.00, max_tokens: 2000000, on_exceed: abort}
fanout: {enabled: false, candidates: 3, concurrency: 3, isolator: auto, select: fewest_issues}
output: {mode: auto}
plan: {archive: true, archive_dir: docs/plans}
```

## `version`

**Type**: Integer  
**Default**: `1`  
**Example**: `1`

The schema version of this file, not the belay release version. The only accepted value is `1`; anything else is rejected by name so a future schema change fails loudly rather than being half-applied.

## `agent`

The coding agent belay drives to write the plan, the code and the fixes.

### `agent.backend`

**Type**: String  
**Default**: `"claude-code"`  
**Shipped values**: `"claude-code"`

The agent backend to use. In v0.1 only `"claude-code"` exists; the `belay.AgentBackend` interface is exported so other CLIs can be added, but none is shipped or tested (see [ADR-0005](adr/0005-claude-code-only-backend-v0.md)). The loader does not yet reject unknown names, so a typo surfaces at run time rather than at load time.

### `agent.model`

**Type**: String  
**Default**: `"sonnet"`  
**Example**: `"sonnet"`, `"opus"`, `"claude-sonnet-5"`

The model identifier passed straight through to the backend CLI's `--model` flag, so any alias or full identifier that CLI accepts works here. It is also the key the cost estimator uses when the CLI does not report a price, so an unrecognised name makes the budget guard fall back to an error rather than a silent zero.

### `agent.max_turns`

**Type**: Integer  
**Default**: `30`  
**Minimum**: `1`

The most turns the agent may take in one step (plan, code or fix), where a turn is one model response and the tool calls it makes. belay passes it to Claude Code as `--max-turns`. It limits a single step, not the run: the number of trips around the fix loop is `graph.give_up`. An agent that runs out of turns stops, the step fails with the error the agent reported, and the run stops as failed.

### `agent.mode`

**Type**: String  
**Default**: `"live"`  
**Valid values**: `"live"`, `"record"`, `"replay"`

Whether belay calls the agent, and whether it keeps a recording (a cassette) of what the agent said:
- `"live"`: Call the agent for real. Nothing is recorded.
- `"record"`: Call the agent for real and save every exchange to the cassette, `<workspace>/.belay/cassette.json` unless `--cassette` names another file.
- `"replay"`: Answer every agent call from the cassette instead. No agent process is started and nothing is spent, so a recorded run can be repeated in CI for free (see [ADR-0009](adr/0009-record-replay-cassettes.md)). A call the cassette has no answer for is an error.

### `agent.models`

**Type**: Map of strings  
**Default**: `{}`  
**Example**: `{plan: opus, code: haiku}`

Per-node routing: the model each step is given, keyed by the node that asks
for it. A node with no entry here is sent `agent.model`, so leaving this out
is exactly belay's behaviour without it.

```yaml
agent:
  model: sonnet        # every node not named below
  models:
    plan: opus         # the plan is the decision the rest of the run executes
    code: haiku        # writing out an agreed plan is the mechanical part
    fix: opus          # a repair is the step that most needs the better model
```

The keys are the nodes that call a model, and only those: `plan`, `code` and
`fix`. `write`, `test`, `review` and `promote` never reach a model — they edit
files, run your test command, lint, and commit — and `approve` waits for you,
so belay refuses a key naming one of them rather than accept a setting that
would do nothing:

```
agent.models: "test" is not valid (want one of: plan, code, fix)
```

A misspelled node is refused the same way, which is the point of the closed
set. Silently ignoring `models.cod` would leave you reading a file that says
the code step is cheap while every step billed at `agent.model`.

The values are model identifiers, passed to the backend exactly as
`agent.model` is, so the same aliases and full names work. Every one of them
is checked against the backend before a run starts, not only `agent.model`:
`belay run --dry-run` prints one row per node with the model it would use and
that model's list rate, and a model the backend refuses stops the run there,
before anything is spent. The Claude Code backend refuses none: a model its
price table does not list, such as a dated identifier, an alias like
`sonnet[1m]` or `opusplan` it cannot resolve, or one released after this
version of belay, is priced at the highest rate belay knows, so the budget
ceiling errs towards stopping a run early, and belay warns once naming the
model. The CLI normally reports each call's cost itself; the table is only
used when it does not. The rate is per million tokens, and belay
states no total, because it cannot know how many tokens your goal will take
and a confident wrong figure is worse than none.

Routing is read from `belay.yaml` on every step, including after a resume. It
is not pinned in the run's manifest: changing a model between resumes changes
what the next step costs, and nothing already recorded. Spend is attributed to
the model that actually ran the step, so `belay timeline` and the budget
ledger show the cheap node cheap.

## `graph`

Limits on the graph of steps a run moves through.

### `graph.give_up`

**Type**: Integer  
**Default**: `3`

How many times the fix node may attempt a repair before belay stops and hands
the problem back to you. The fix loop is entered by a failing test suite and by
a failed quality gate alike; each pass through it consumes one attempt.

At the cap the run ends as failed, with a note naming the exhaustion — it does
not silently return a partial result. Set it higher for a task you expect to
take several rounds, lower to fail fast. It must be at least 1.

Not to be confused with the `give_up` field belay writes into a run's
`state.json`, which is a boolean recording whether the loop *has* exhausted
this budget.

### `graph.max_steps`

**Type**: Integer  
**Default**: `60`  
**Minimum**: `1`

The most steps one run may execute in total, counting every step each time it runs, including each pass through the fix loop. belay checks it before starting each step, and a run that reaches it is aborted with `graph: max steps exceeded`. It is a backstop for a graph that is not converging; `graph.give_up` is the limit meant to end a fix loop that is not making progress.

### `graph.node_timeout`

**Type**: String (duration)  
**Default**: `"15m"`  
**Example**: `"30m"`, `"1h30m"`

The longest one step may run, written as a Go duration. A step still running at the deadline is cancelled, and the run is aborted with `graph: node timeout exceeded`, whatever the step returns afterwards. It must be greater than zero.

### `graph.approval`

**Type**: Boolean  
**Default**: `true`

Whether belay stops after planning so a human can read the plan before any code
is written.

With `true`, the run pauses at the approve node and exits cleanly; the plan is
at `<run-dir>/artifacts/plan.md`, you may edit it, and `belay resume` continues
from there — reading the file again, so your edits are what the agent receives.
Because the pause is a clean exit rather than a blocked terminal, a run can
wait indefinitely and survive the session that started it. If you want a
different plan rather than an edited one, `belay plan --regenerate "<what to
change>"` sends the run back to the plan node with what you said; a plan you
emptied or saved as something that is not text pauses the gate again, with a
note saying so, rather than ending the run.

With `false`, planning routes straight to coding. That is the right setting for
an unattended or CI run, and the wrong one for the first time you point belay
at a repository you care about.

In a queue, `graph.approval` means the same thing. When a goal's plan is
written, `belay queue start` parks the goal for you to read the plan and moves
on to the next goal; `belay queue approve <goal>` releases it, and the next
start carries it on. The only way one queued goal skips the gate is `belay
queue add --no-approval`, which you type when you add it; belay never turns
the gate off by itself.

A few settings work differently for queued goals, and these are all of them:

- Each goal runs in its own copy of the workspace, and reads the `belay.yaml`
  in that copy, as the file stood when belay reached the goal.
- Every goal is held to `budget.max_usd` and `budget.max_tokens` on its own.
  `belay queue start` says the most the whole queue can spend before it spends
  anything, and refuses to start when `budget.on_exceed` is `"warn"`, because
  nobody is watching a queue to stop an overrun.
- A queued goal always edits its copy in place, whatever `output.mode` says,
  and never files its plan into the repository, whatever `plan.archive` says:
  the copy is not the repository, and a plan filed into it would be lost with
  it.

## `test`

How the test step proves the code works.

### `test.runner`

**Type**: String  
**Default**: `"auto"`  
**Valid values**: `"auto"`, `"go"`, `"node"`, `"python"`, `"custom"`

The test runner to invoke:
- `"auto"`: Use the first of Go, Node and Python that the workspace's root looks like, in that order: a `go.mod`, a `package.json`, or a Python project. A repository that is two of them gets the earlier one every time; set this to pick the other.
- `"go"`: `go test -json -count=1 ./...`
- `"node"`: The package's own `test` script, through the package manager the repository uses (npm, yarn, pnpm or bun), asking jest, vitest or `node:test` for machine-readable output.
- `"python"`: `pytest --tb=short -q -rfE`
- `"custom"`: The command in `test.custom_cmd`

### `test.custom_cmd`

**Type**: String  
**Default**: `""` (empty)  
**Example**: `"make test"`, `"./scripts/run-tests.sh"`

The command to run when `test.runner` is `"custom"`, which then requires it. Exit status 0 is a pass and anything else a failure; belay learns nothing else from it, so the fix step sees only the tail of the command's output. It is not run through a shell: pipes, redirection and globs do not work, so put them in a script and name the script here.

## `review`

The quality gate the review step applies to the run's change.

### `review.mode`

**Type**: String  
**Default**: `"lint"`  
**Valid values**: `"lint"`, `"sonar"`, `"ai"`

The review strategy:
- `"lint"`: Run local linters (golangci-lint, eslint, etc.). Fast, zero setup, no cost. In a git repository, golangci-lint and Ruff count only findings on lines that differ from `HEAD`, so findings committed before the run don't fail it. Where git can't tell, every finding counts. ESLint always counts every finding. For the limits, see [Troubleshooting](troubleshooting.md#quality-gate-fails-on-code-the-run-did-not-touch).
- `"sonar"`: Query SonarQube server for code quality. Requires infrastructure setup.
- `"ai"`: Ask SonarQube for its verdict through the Sonar MCP server (see `review.ai`). The loader accepts it, but `belay run` refuses it for now: this reviewer is not yet wired into the command.

### `review.fail_on`

**Type**: String  
**Default**: `"major"`  
**Valid values**: `"blocker"`, `"critical"`, `"major"`, `"minor"`, `"info"`

The least severe issue that fails the quality gate, on SonarQube's scale from worst to least: `"blocker"`, `"critical"`, `"major"`, `"minor"`, `"info"`. An issue at this severity or worse fails the gate; anything less severe is reported but passes. `"blocker"` is the most lenient setting and `"info"` the strictest.

It decides the gate under `review.mode` `"lint"` and `"ai"`. Under `"sonar"` the SonarQube server's own quality gate decides, because the scanner reports only the server's verdict and not the issues behind it.

### `review.sonar` (if `review.mode == "sonar"`)

Configuration for SonarQube reviewer.

#### `review.sonar.runner`

**Type**: String  
**Default**: `"docker"`  
**Shipped values**: `"docker"`, `"binary"`

How the SonarQube scanner is run. Either way it analyses the workspace and uploads the result to the SonarQube server named by the `SONAR_HOST_URL` environment variable, authenticating with `SONAR_TOKEN`:
- `"docker"`: Run the scanner in a container from `review.sonar.image`. Needs `docker` on `PATH`.
- `"binary"`: Run a `sonar-scanner` already installed on `PATH`.

The loader checks only that this is not empty; another name is refused when the review runs.

#### `review.sonar.image`

**Type**: String  
**Default**: `"sonarsource/sonar-scanner-cli"`

The scanner image to run when `review.sonar.runner` is `"docker"`. It is the scanner, not a SonarQube server: the server is wherever `SONAR_HOST_URL` points. Add a tag to pin the scanner version.

#### `review.sonar.quality_gate_wait`

**Type**: Boolean  
**Default**: `true`

Whether the scanner waits for the SonarQube server to compute the quality gate, passed to it as `sonar.qualitygate.wait`. The gate is computed on the server, after the scan uploads its report, so the verdict belay acts on exists only if the scanner waits for it. With `false`, the scan uploads and exits without a verdict, and belay reports the review as undecided rather than as a pass.

#### `review.sonar.quality_gate_timeout`

**Type**: Integer  
**Default**: `300`  
**Minimum**: `1`

How long, in seconds, the scanner waits for the server's verdict, passed to it as `sonar.qualitygate.timeout`. It bounds only that wait: indexing, analysis and the upload come first, and belay allows the whole scan five minutes more than this before it stops the scanner. A gate still not computed when it runs out is reported as undecided, not as a failure, so it does not send the agent into the fix loop.

### `review.ai` (if `review.mode == "ai"`)

Configuration for the `"ai"` reviewer, which asks SonarQube for its quality gate verdict and the issues behind it through the Sonar MCP server, authenticating with `SONAR_TOKEN`. `belay run` does not use it yet (see `review.mode`), but the loader still validates these settings.

#### `review.ai.mcp`

How the reviewer reaches the Sonar MCP server.

##### `review.ai.mcp.mode`

**Type**: String  
**Default**: `"cloud"`  
**Valid values**: `"cloud"`, `"server"`, `"docker"`

How to reach the MCP server:
- `"cloud"`: Over HTTP, to SonarQube Cloud's MCP endpoint at `review.ai.mcp.url`.
- `"server"`: Over HTTP, to the MCP endpoint of a SonarQube Server you run, at `review.ai.mcp.url`.
- `"docker"`: Start the server from `review.ai.mcp.image` as a local container and talk to it over its standard input and output. Needs `docker` on `PATH`.

##### `review.ai.mcp.url`

**Type**: String  
**Default**: `""` (empty)

The MCP endpoint's URL for the `"cloud"` and `"server"` modes; `"docker"` ignores it. While `review.mode` is `"ai"`, the loader requires it for both: belay has no built-in address for SonarQube Cloud's endpoint, so `"cloud"` needs it as much as `"server"` does. Under any other `review.mode` it may stay empty.

##### `review.ai.mcp.image`

**Type**: String  
**Default**: `"sonarsource/sonar-mcp-server"`

The MCP server image to run when `review.ai.mcp.mode` is `"docker"`, which then requires it.

## `budget`

Cost and token limits to prevent unexpected bills (see [ADR-0007](adr/0007-budget-ledger-day-one.md)).

### `budget.max_usd`

**Type**: Float  
**Default**: `5.00`  
**Example**: `2.5`, `20`

The most a run may spend, in US dollars, as estimated from the agent's reported usage. belay checks it before each step, so a run stops before paying for one more step rather than after. `0` means no dollar ceiling, and the loader accepts it only together with `budget.on_exceed: warn`.

### `budget.max_tokens`

**Type**: Integer  
**Default**: `2000000`

The most tokens, input and output together, a run may use across all its agent calls. It is checked alongside `budget.max_usd`, and whichever ceiling is reached first applies `budget.on_exceed`. `0` means no token ceiling.

### `budget.on_exceed`

**Type**: String  
**Default**: `"abort"`  
**Valid values**: `"abort"`, `"warn"`

What to do when budget is exceeded:
- `"abort"`: Stop the run before the next step. The run is paused, not ended: raise `budget.max_usd` or `budget.max_tokens` and run `belay resume <run-id>` to carry on with what it has already spent still counted. belay reads these limits again on every resume
- `"warn"`: Log a warning but continue (not recommended)

## `fanout`

Best-of-N: giving one goal several independent candidates at once, so the best can be kept.

A run with `fanout.enabled: true` takes a different route: plan → approve → fanout → join, and then promote when the run delivers its result on a branch. Each candidate is a **candidate run** with its own journal, blackboard, manifest and budget envelope, under `.belay/runs/<run-id>/candidate-runs/<candidate-id>/`, and its workspace beside it under `candidates/<candidate-id>/`. `belay timeline` shows one lane per candidate, and `belay timeline --candidate <id>` opens one candidate's own run. See [ADR-0019](adr/0019-executing-fanout-candidates-as-candidate-runs.md).

### `fanout.enabled`

**Type**: Boolean  
**Default**: `false`

Routes the run through the fanout and join nodes, so one goal gets `fanout.candidates` independent attempts and the best one is kept. It is decided once, when the run starts, and recorded in the run's `manifest.json`: `belay resume` finishes a run the way it started, whatever `belay.yaml` says by then, and warns when the two disagree.

It requires `budget.max_usd` above zero, because the fanout divides the run's remaining ceiling into one envelope per candidate and has nothing to divide otherwise. It is also refused with `review.mode: sonar` or `review.mode: ai`: both name the SonarQube project after the directory they review, and every run's candidates are called `cand-01`, `cand-02` and so on, so their quality gates would read one another's results.

A fanout can spend more than `budget.max_usd`: each candidate is capped at its own envelope, and a candidate is checked before a step rather than during one, so the overrun is bounded by one step per candidate. A step cut short by Ctrl-C, `graph.node_timeout` or a crash reports nothing of what it spent, so it is charged the costliest step recorded before it, marked as an estimate; that adds to the overrun only when the step that was cut cost more than any before it. `--dry-run` prints the envelope and that bound.

### `fanout.candidates`

**Type**: Integer  
**Default**: `3`  
**Minimum**: `2`

How many candidates the run fans out to, each in its own workspace and its own candidate run. Cost multiplies by about this factor, and so does disk: N workspaces plus N candidate-run directories. Like `fanout.enabled`, it is recorded in the run's manifest when the run starts, because it decides the candidate IDs and how the budget is divided; changing it mid-run would abandon candidates already claimed and paid for.

### `fanout.concurrency`

**Type**: Integer  
**Default**: `3`  
**Minimum**: `1`

How many candidates execute at the same time. `1` runs them one after another, which is what recording a cassette needs (`agent.mode: record`): concurrent candidates record in whatever order they finish. A value above `fanout.candidates` behaves as `fanout.candidates`.

Unlike the three settings above, this one is **not** recorded in the run's manifest: lowering it and resuming is how you back off a rate limit or a machine that cannot take the load. The cost of that is one thing — a duration measured while three candidates share a machine is not the duration of the same candidate running alone, so under `fanout.select: "fastest"` a resume at a different concurrency can pick a different winner. `"fewest_issues"` and `"first_pass"` are unaffected.

Above 1, several candidates can reach the review node at once. belay's `golangci-lint` gate passes `--allow-serial-runners` for exactly that reason: the tool takes a global file lock and, by default, exits with an error rather than waiting for it.

### `fanout.isolator`

**Type**: String  
**Default**: `"auto"`  
**Valid values**: `"auto"`, `"worktree"`, `"dircopy"`

Isolation strategy for candidates:
- `"auto"`: `worktree` when the run delivers its result on a branch, `dircopy` otherwise. This is the default, and it keeps the two settings in step: a candidate can only be merged if it is on a branch.
- `"worktree"`: Each candidate is a git worktree on its own branch, `belay/<run-id>-<candidate-id>`, and the winner is promoted with a merge commit. It needs the same git a branch run needs (2.38 or newer, at least one commit, no submodules).
- `"dircopy"`: Plain directory copies. Works on non-git targets and uses O(N × repo_size) disk — the trade [ADR-0004](adr/0004-directory-copies-for-fanout.md) accepted, now superseded by [ADR-0012](adr/0012-git-native-run-output.md) for git targets. Note that `.git` is **not** copied, so a candidate cannot run git commands, and its work cannot be merged.

A run that fans out is refused, before anything is spent, when the isolator cannot serve its output: `"dircopy"` in a run on a branch, because a copy has no branch to merge from, and `"worktree"` in a run that edits in place, because a worktree candidate branches from the run's own branch. With `fanout.enabled: false` this setting changes nothing.

### `fanout.select`

**Type**: String  
**Default**: `"fewest_issues"`  
**Valid values**: `"fewest_issues"`, `"first_pass"`, `"fastest"`

How the join node chooses the winning candidate. Only a candidate whose tests passed, whose own run completed, and that broke none of the guardrails set under `policy` can win:
- `"fewest_issues"`: The candidate with the fewest review issues. A tie goes to the lowest candidate ID.
- `"first_pass"`: The first candidate whose tests passed, in the order fanout recorded the candidates.
- `"fastest"`: The candidate whose own run spent the least time in its steps, which is not the same as the one that finished first: candidates running at once share the machine. A tie goes to the lowest candidate ID. Lowering `fanout.concurrency` and resuming can change which candidate this picks.

Like `fanout.enabled` and `fanout.candidates`, it is recorded in the run's manifest when the run starts, so a resumed run picks its winner the way it was going to.

## `output`

### `output.mode`

**Type**: String  
**Default**: `"auto"`  
**Valid values**: `"auto"`, `"branch"`, `"in_place"`

How a run delivers its result:
- `"branch"`: The run works in its own git worktree, under `.belay/runs/<run-id>/worktree`, on a new branch `belay/<run-id>` created from the commit `HEAD` was on when the run started. It never edits your checkout, and it commits once, after the quality gate passes, under your own git identity with a `Belay-Run: <run-id>` trailer.
- `"in_place"`: The run edits the workspace directly. This is what belay did before this setting existed.
- `"auto"`: `branch` when the workspace is inside a git work tree that has at least one commit and no submodules, and git is 2.38 or newer; `in_place` otherwise. belay prints which one it chose and why, both at the start of a run and under `--dry-run`.

Uncommitted changes in your checkout are not part of a branch run: the run starts from the commit, and belay warns when it finds any.

The mode is recorded in the run's `manifest.json` when the run starts. `belay resume` finishes a run the way it started, whatever `belay.yaml` says by then, and warns when the two disagree.

## `plan`

What becomes of a run's plan once the run is over.

### `plan.archive`

**Type**: Boolean  
**Default**: `true`

When a run finishes successfully, belay moves the plan it was approved with out of `.belay/runs/<run-id>/artifacts/plan.md` and into the repository, as `<plan.archive_dir>/<run-id>-<slug>.md`, where `<slug>` is your goal in lower-case words. The run directory is scratch, and deleting it is safe; the plan is the record of why the code looks the way it does, so it goes next to the code.

- A run that edits the workspace in place leaves the plan as a new file beside its change, for you to commit with the change or delete. belay does not stage or commit it.
- A run on a branch puts the plan in its one commit, so it is part of what the branch delivers, and of the pull request `--pr` opens.

A run that failed, was stopped or has not finished keeps its plan in the run directory. After a plan has moved, `belay plan <run-id>` shows it from where it now is, and `belay timeline <run-id>` still shows the run: the journal and the manifest stay in the run directory.

belay reads this setting when a run starts and records the answer in the run's manifest, so changing it does not change what a run already started does. Set `false` to leave every plan in `.belay`.

### `plan.archive_dir`

**Type**: String  
**Default**: `"docs/plans"`

Where finished plans go, relative to the workspace (`--workspace`), not to the directory belay was started from. Missing directories are created. It must stay inside the workspace: an absolute path, a `..`, and a path under `.belay` or `.git` are refused when the configuration loads, and a symbolic link that leads out of the workspace is refused when the plan is moved. belay never replaces a file that is already there; if one is, the plan stays in the run directory and belay says why. It is checked even when `plan.archive` is `false`.

## `policy`

Guardrails a run's change must obey, whatever its tests say. A change that passes every test can still edit the deployment pipeline, pull in a dependency nobody vetted, grow by four thousand lines, or leave a credential in a source file. These rules are how you write down what an unattended run's change may not do.

**What the gate judges.** Every file the workspace says the run changed, whether or not the agent reported it. The agent's own list of what it changed is a claim, and an agent that leaves an edit out of it, by mistake or on purpose, does not take the edit out of the gate's view; every such file is judged like the rest and named in the gate's verdict ("2 the agent did not report (belay.yaml, infra/main.tf), judged all the same"), in `belay timeline`, and in the commit. Paths are judged in the file system's own names, so a report spelled `Infra/main.tf` cannot dodge `infra/**`. Where the workspace's answer comes from depends on the run:

| The run | What the gate judges |
|---|---|
| On a branch (`output.mode: branch`, or `auto` in a git repository) | Every file git says differs from the commit the run's branch started at: committed since, staged, edited, deleted, renamed (both names), and every new file git does not ignore |
| In place, in a git checkout | The same, measured from the commit `HEAD` was on when the run started, apart from the files you had already changed by then: belay records those when the run starts, and judges one only if the run changes it again |
| In place, with no git (including a queued goal, which works in a copy belay makes without `.git`) | Every file that differs from a record of the workspace's files belay takes when the run starts, comparing their contents |
| A fanout candidate | As its run: a candidate of a branch run is a git worktree, measured from the run's base commit; a candidate of an in-place run is a directory copy, recorded when the copy is made |
| A fork (`belay fork`) | Only the files the agent reports. A fork's copy is taken part-way through its source run's change, so nothing in it can say where that change began; the gate says so in every verdict, and `belay fork --dry-run` says so first |

A branch run then commits exactly what was recorded and judged. Just before it commits, belay compares the run's worktree with what its steps recorded, and pauses, naming the files, if anything else changed there since: a file the test command wrote, an edit made by hand while the run was paused. It judges the change once more against the policy too, so a file the gate passed and something rewrote afterwards cannot ship. Both pauses are described in docs/troubleshooting.md.

**What it does not protect against.** belay does not sandbox the agent. Claude Code and your test command run as you, and nothing stops them writing outside the workspace, into another repository, your home directory, or belay's own `.belay` directory; the gate judges the workspace and sees none of that. In a git checkout, a file your `.gitignore` excludes is not part of the change (a commit would not carry it either, but an in-place run leaves it in your checkout); an edit to a `.gitignore` is part of it and is judged. In a workspace with no git there are no ignore rules, so a file the test command writes into the tree, such as a `__pycache__` or a coverage report, counts as changed on the next pass of the fix loop. If you need the agent confined, run belay in a container or a VM that holds only the repository.

Every rule is off by default, and a repository that declares no policy runs exactly as it did before this section existed: belay adds no step to the graph and reads no extra file.

When a policy is set, belay adds a `policy` gate to the run's route, between `write` and `test`, and judges the change there on every pass of the fix loop. Two things follow:

- **The rules are fixed when the run starts.** belay records them, and the dependencies your workspace declared, in the run's `manifest.json`, and `belay resume` judges the change against those and not against `belay.yaml` as it is by then. A run started with no policy never grows one: it has no gate, and no record of what its workspace looked like before the agent ran, so a policy added mid-run could only fail it for something it did not do. ADR-0014 records the decision.
- **A violation is one of two kinds.** `forbidden_paths`, `max_changed_files` and `max_changed_lines` **end the run**: a change that touched a path it may not touch is not something to ask an agent to repair. Everything else goes to the **fix loop**, which tells the agent what it broke and asks for a repair, bounded by `graph.give_up` like any other repair.
- **Every file's rules apply.** Unlike every other setting, a later file cannot replace a policy rule, only add to it: the lists (`forbidden_paths`, `required_tests`, `forbidden_content`) are joined, each rule once; each ceiling (`max_changed_files`, `max_changed_lines`) is the smallest one any file sets, and `0` in a later file does not lift a ceiling set below it; and `no_new_dependencies` holds if any file sets it. So the rules in your own `~/.config/belay/config.yaml` hold in every repository, and a repository's `belay.yaml`, which is a file the agent can edit, can tighten them but not loosen them. `belay run --dry-run` prints the combined rules. ADR-0014 records why this section breaks the per-field rule.

`review.fail_on` has nothing to do with this section. It is the threshold a linter's findings are judged against; a policy violation always counts.

Paths are matched with globs, described under `policy.forbidden_paths`.

Example:

```yaml
version: 1
policy:
  forbidden_paths:
    - ".github/workflows/**"
    - "infra/**"
    - "pyproject.toml"
  max_changed_files: 25
  max_changed_lines: 2000
  no_new_dependencies: true
  required_tests:
    - when: "**/*.go"
      require: "**/*_test.go"
  forbidden_content:
    - pattern: "AKIA[0-9A-Z]{16}"
      reason: "an AWS access key id"
```

### `policy.forbidden_paths`

**Type**: List of strings  
**Default**: `[]`

Paths the change may not touch. A run that changes one **ends**, naming the file and the pattern that forbade it.

A pattern is slash-separated, relative to the workspace, and anchored at its root:

| Pattern | Matches | Does not match |
|---|---|---|
| `go.mod` | `go.mod` | `api/go.mod` |
| `*.tf` | `main.tf` | `infra/main.tf` |
| `**/*.tf` | `main.tf`, `infra/aws/main.tf` | `main.tfvars` |
| `infra/**` | `infra`, `infra/aws/main.tf` | `infrastructure/main.tf` |
| `src/**/*_test.go` | `src/a/b/x_test.go` | `x_test.go` |

`*` and `?` and a character class such as `[0-9]` match inside one path segment. `**` is the only thing that crosses a `/`, and stands for zero or more whole segments. Unlike `.gitignore`, a pattern with no slash is **not** matched at every depth: write `**/secrets.yaml` for that. One rule that always reads the same way is worth two extra characters.

A pattern belay cannot match — an empty one, an absolute one, one containing `..`, or one gluing `**` to other characters such as `src**/x` — is refused when the configuration loads. A rule that silently matches nothing is worse than no rule, because somebody is relying on it.

Case is matched the way the run's disk names files. On a disk that treats `Infra` and `infra` as one directory, as macOS does by default, every pattern matches without regard to case, because there the two spellings are one file; belay records which kind of disk the run is on when it starts. On a disk that tells them apart, matching is exact.

### `policy.max_changed_files`

**Type**: Integer  
**Default**: `0`

The most files one run's change may touch. `0` means no ceiling. A change over it **ends** the run.

### `policy.max_changed_lines`

**Type**: Integer  
**Default**: `0`

The most lines the files a change touches may hold together. `0` means no ceiling. A change over it **ends** the run.

Read the name carefully: this is **not** a diffstat. belay keeps no copy of the tree from before the agent ran, so it cannot know which lines inside a file changed, and it does not pretend to — it counts the whole of every changed file as it now stands. A one-line edit to a 900-line file counts 900. That over-counts and never under-counts, which is the safe direction for a ceiling: it is a limit on how much of the repository one unattended run may rewrite, and how much you have to read afterwards.

Set it generously, or use `policy.max_changed_files` instead, which is exact. A file the change deleted is counted by `max_changed_files` but contributes no lines, because the lines it held are the one thing no longer there to count.

### `policy.no_new_dependencies`

**Type**: Boolean  
**Default**: `false`

Forbid declaring a dependency the workspace did not declare when the run started. A change that adds one goes to the **fix loop**: the agent is told which dependency in which manifest, and asked to do without it.

belay reads `go.mod` (its `require` directives), `package.json` (`dependencies`, `devDependencies`, `peerDependencies` and `optionalDependencies` alike — a build plugin runs with the same privileges as anything else) and `requirements.txt`, in the workspace and up to three directories below it, skipping the same directories `belay run` skips when it works out what kind of project you have (`node_modules`, `vendor`, `.git` and the rest). Names are compared, never versions: upgrading something you already trust is a different decision from taking on code nobody has looked at.

`pyproject.toml` is **not** read. Doing it properly means a TOML parser and PEP 621's several spellings of a dependency, and a parser that got it half right would report dependencies you do not have or miss ones you do. Until belay reads it properly, add it to `policy.forbidden_paths`, which stops the change belay cannot judge instead of waving it through.

A manifest belay cannot parse stops the run from starting, rather than being recorded as declaring nothing: a baseline missing what a file declares would make every one of those dependencies look new at the gate.

### `policy.required_tests`

**Type**: List of rules  
**Default**: `[]`

Make a change of one kind bring a change of another with it. Each rule has two path globs, written exactly as `policy.forbidden_paths` describes:

- `when`: the changed files the rule is about.
- `require`: what the change must also touch.

A change that touches a file matching `when` and nothing matching `require` goes to the **fix loop**, naming a file that triggered the rule. A change that touches nothing matching `when` is not what the rule is about and passes it.

```yaml
policy:
  required_tests:
    - when: "**/*.go"
      require: "**/*_test.go"
    - when: "internal/api/**"
      require: "docs/api/**"
```

The rule is satisfied by any changed file matching `require`, anywhere: belay does not try to work out which test covers which source file, because a wrong answer to that would either block correct changes or wave through untested ones.

### `policy.forbidden_content`

**Type**: List of rules  
**Default**: `[]`

Text a changed file may not contain. Each rule has:

- `pattern`: a [Go regular expression](https://pkg.go.dev/regexp/syntax), matched line by line so a violation can name the line it is on. Required.
- `paths`: path globs limiting the rule to some changed files. Empty means every changed file.
- `reason`: what you are told when the rule fires, in place of the pattern. A bare regular expression explains nothing to whoever has to act on it.

A match goes to the **fix loop**, once per rule per file: a pattern that matches forty lines of one file is one thing to fix.

```yaml
policy:
  forbidden_content:
    - pattern: "AKIA[0-9A-Z]{16}"
      reason: "an AWS access key id"
    - pattern: "(?i)//\\s*nolint"
      paths: ["**/*.go"]
      reason: "a lint suppression; fix the finding instead"
```

Only the changed files are read, never the whole repository: this is a check on what the run did, not an audit of what was already there. The changed files are the ones the workspace says changed, as described under [`policy`](#policy), not only the ones the agent reported. A changed file with a line longer than 1 MiB — a minified bundle, a generated blob — stops the gate with an error rather than being reported as clean, because "belay could not look" must never be recorded as "belay looked and it was fine". Exclude such a file with the rule's `paths`, or name it in `policy.forbidden_paths`.

## Environment Variables

The following environment variables are used by belay and are never read from `belay.yaml`:

### `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` (Claude backend)

The credential Claude Code authenticates with. Set one of them, or neither if Claude Code is already signed in on this machine. Whichever you set belongs in the shell; never include either in belay.yaml.

- `ANTHROPIC_API_KEY` is an Anthropic API key, billed per call. Claude Code prefers it when both are set.
- `CLAUDE_CODE_OAUTH_TOKEN` is the long-lived token `claude setup-token` issues to a Claude Pro, Max, Team or Enterprise subscription. Requires belay 0.1.1 or later.

belay passes both to the `claude` child process and redacts their values from its journal, logs and cassettes. It passes no other Anthropic, Bedrock, Vertex or proxy variable.

Example:
```bash
export ANTHROPIC_API_KEY="sk-ant-..."
# or
export CLAUDE_CODE_OAUTH_TOKEN="<token from claude setup-token>"
belay run "fix the bug"
```

### `SONAR_TOKEN` and `SONAR_HOST_URL` (SonarQube)

`SONAR_TOKEN` is your SonarQube authentication token and `SONAR_HOST_URL` the address of the SonarQube server. A scan under `review.mode: sonar` needs both, whichever `review.sonar.runner` it uses; the `"ai"` reviewer needs the token. Set them in the shell; never include either in belay.yaml.

Example:
```bash
export SONAR_TOKEN="squ_..."
export SONAR_HOST_URL="https://sonarqube.example.com"
belay run "fix the bug"
```

### `DOCKER_HOST` (optional)

Read by the `docker` CLI itself, not by belay. It therefore affects the SonarQube Docker runner (`review.sonar.runner: docker`) and the Docker-mode MCP bridge, because both shell out to `docker`. belay has no Docker setting of its own and never parses this value.

## Example Configuration

A whole `belay.yaml`. The values marked `# default` differ from belay's defaults on purpose, to show settings you might tighten; every other value is the default.

```yaml
version: 1

agent:
  backend: "claude-code"
  model: "sonnet"
  max_turns: 5          # default 30
  mode: "live"

graph:
  give_up: 3
  max_steps: 20         # default 60
  node_timeout: "10m"   # default 15m
  approval: true

test:
  runner: "auto"

review:
  mode: "lint"
  fail_on: "major"

budget:
  max_usd: 5.0
  max_tokens: 500000    # default 2000000
  on_exceed: "abort"

fanout:
  enabled: false
  candidates: 3
  concurrency: 3
  isolator: "auto"
  select: "fewest_issues"

output:
  mode: "auto"

plan:
  archive: true
  archive_dir: "docs/plans"
```

## Example Configuration with SonarQube

```yaml
version: 1

agent:
  backend: "claude-code"
  model: "sonnet"

review:
  mode: "sonar"
  fail_on: "blocker"
  sonar:
    runner: "docker"
    image: "sonarsource/sonar-scanner-cli"
    quality_gate_wait: true
    quality_gate_timeout: 600

budget:
  max_usd: 10.0
```

## Validation

belay checks the configuration before a run starts, and reports every problem it finds at once rather than stopping at the first:

- A key that names no setting is an error, not a warning, so a typo cannot be silently ignored. So is a key written twice.
- A value of the wrong type (e.g., `max_turns: "five"` instead of `5`) is an error naming the file, the line and the setting.
- A key that looks like a credential, such as `sonar_token` or `api_key`, is rejected wherever it appears. Secrets are read from the environment only.
- A closed-set setting must hold one of its **Valid values**, and numeric limits must respect their minimums.
- Durations must be Go durations (e.g., `"15m"`, `"1h30m"`).
- `version` may be left out, in which case it is `1`; any other value is rejected.

## Configuration Precedence

Settings come only from the files listed under [File Location](#file-location), in that order, on top of the built-in defaults. No environment variable or command-line flag overrides an individual setting; to change one for a single run, put it in a file and pass that file with `--config`:

```bash
# belay.yaml says max_usd: 10.0; this run gets 2.5 and keeps everything else
printf 'budget:\n  max_usd: 2.5\n' > /tmp/cheap.yaml
belay run --config /tmp/cheap.yaml "fix the bug"
```

## References

- [ADR-0007](adr/0007-budget-ledger-day-one.md): Budget ledger and cost caps
- [ADR-0008](adr/0008-local-linters-default.md): Linters as default review mode
