# Belay Configuration

<!--
Insertion markers. Each "slice:NAME" comment below marks where the work in
flight for that slice adds its text: insert directly above the marker and
leave the marker in place, so changes built in parallel land on different
lines. Delete a marker once its slice has merged.
-->

Belay is configured by a `belay.yaml` file in the repository it works on. This document describes every field and its effects.

## File Location

belay builds its configuration from up to three files, merged onto its built-in defaults. Each later file overrides the earlier ones:

1. `$XDG_CONFIG_HOME/belay/config.yaml`, or `~/.config/belay/config.yaml` when `XDG_CONFIG_HOME` is not set: your own settings, for every repository.
2. `belay.yaml` in the workspace (`--workspace`, `.` by default). For example, `belay run --workspace ../other-repo "your goal"` reads `../other-repo/belay.yaml`.
3. The file named by `--config`, if you pass one.

Merging is per field, not per file: a file that sets only `budget.max_usd` keeps every other setting from the layers below it, including the other `budget` settings. A file that is missing is skipped, except the one `--config` names, which must exist. With no files at all, belay runs on the defaults this document lists.

## Top-Level Fields

```yaml
version: 1
agent: {backend: claude-code, model: sonnet, max_turns: 30, mode: live}
graph: {give_up: 3, max_steps: 60, node_timeout: 15m, approval: true}
test: {runner: auto, custom_cmd: ""}
review: {mode: lint, fail_on: major}
budget: {max_usd: 5.00, max_tokens: 2000000, on_exceed: abort}
fanout: {enabled: false, candidates: 3, isolator: auto, select: fewest_issues}
output: {mode: auto}
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

<!-- slice:routing -->

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

<!-- slice:queue -->

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
- `"lint"`: Run local linters (golangci-lint, eslint, etc.). Fast, zero setup, no cost. In a git repository, golangci-lint counts only findings on lines that differ from `HEAD`, so findings committed before the run don't fail it. Where git can't tell, every finding counts. ESLint and Ruff always count every finding. For the limits, see [Troubleshooting](troubleshooting.md#quality-gate-fails-on-code-the-run-did-not-touch).
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

No run uses these settings yet. No step in belay's graph leads to the fanout node, so a run never reaches it, and nothing runs a candidate: the fanout node only prepares a workspace for each. Running candidates is planned, and ADR-0019 is reserved for it.

### `fanout.enabled`

**Type**: Boolean  
**Default**: `false`

Registers the fanout and join nodes. No node routes to them, so this changes nothing a run does; `true` only makes the loader require a `fanout.candidates` of at least 2.

### `fanout.candidates`

**Type**: Integer  
**Default**: `3`  
**Minimum**: `2`

Number of candidates the fanout node prepares, each in its own workspace. Nothing runs them yet; once something does, cost multiplies by about this factor.

### `fanout.isolator`

**Type**: String  
**Default**: `"auto"`  
**Valid values**: `"auto"`, `"worktree"`, `"dircopy"`

Isolation strategy for candidates:
- `"auto"`: `worktree` when the run delivers its result on a branch, `dircopy` otherwise. This is the default, and it keeps the two settings in step: a candidate can only be merged if it is on a branch.
- `"worktree"`: Each candidate is a git worktree on its own branch, `belay/<run-id>-<candidate-id>`, and the winner is promoted with a merge commit. It needs the same git a branch run needs (2.38 or newer, at least one commit, no submodules).
- `"dircopy"`: Plain directory copies. Works on non-git targets and uses O(N × repo_size) disk — the trade [ADR-0004](adr/0004-directory-copies-for-fanout.md) accepted, now superseded by [ADR-0012](adr/0012-git-native-run-output.md) for git targets. Note that `.git` is **not** copied, so a candidate cannot run git commands, and its work cannot be merged.

`"dircopy"` together with `output.mode: "branch"` refuses the run: a copy has no branch to merge from.

### `fanout.select`

**Type**: String  
**Default**: `"fewest_issues"`  
**Valid values**: `"fewest_issues"`, `"first_pass"`, `"fastest"`

How the join node chooses the winning candidate. Only a candidate whose tests passed can win:
- `"fewest_issues"`: The candidate with the fewest review issues. A tie goes to the lowest candidate ID.
- `"first_pass"`: The first candidate whose tests passed, in the order fanout recorded the candidates.
- `"fastest"`: Meant to be the candidate that finished soonest, but no candidate's duration is recorded, so join picks the first candidate whose tests passed in recorded order, as `"first_pass"` does, and logs a warning that the choice is approximate.

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

<!-- slice:output -->

<!-- slice:plan -->

<!-- slice:policy -->

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

<!-- slice:backend -->

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
  isolator: "auto"
  select: "fewest_issues"

output:
  mode: "auto"
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
