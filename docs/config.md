# Belay Configuration

Belay is configured via a `belay.yaml` file in the project root. This document describes every field and its effects.

## File Location

Belay looks for `belay.yaml` in the workspace directory (specified by `--workspace`, defaulting to `.`). For example, `belay run --workspace ../other-repo "your goal"` reads `../other-repo/belay.yaml`. You can also specify an alternate path with `belay run --config /path/to/config.yaml`, which overrides the automatic lookup.

## Top-Level Fields

```yaml
version: 1
agent: {backend: claude-code, model: sonnet, max_turns: 30, mode: live}
graph: {give_up: 3, max_steps: 60, node_timeout: 15m, approval: true}
test: {runner: auto, custom_cmd: ""}
review: {mode: lint, fail_on: major}
budget: {max_usd: 5.00, max_tokens: 2000000, on_exceed: abort}
fanout: {enabled: false, candidates: 3, isolator: dircopy, select: fewest_issues}
```

## `version`

**Type**: Integer  
**Default**: `1`  
**Example**: `1`

The schema version of this file, not the belay release version. The only accepted value is `1`; anything else is rejected by name so a future schema change fails loudly rather than being half-applied.

## `agent`

Configuration for the LLM backend that generates code, plans, and fixes.

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
**Default**: `5`  
**Range**: `1` to `100`

The maximum number of back-and-forth turns in a single phase (e.g., "code → fix → code → fix → done"). If max_turns is reached, the phase aborts with "max_turns_exceeded" status.

### `agent.mode`

**Type**: String  
**Default**: `"auto"`  
**Valid values**: `"auto"`, `"streaming"`, `"batch"`

How the agent returns results:
- `"auto"`: Agent chooses based on backend capabilities
- `"streaming"`: Agent streams tokens as they arrive (faster feedback, less latency for large responses)
- `"batch"`: Agent waits until generation is complete and returns the full result

## `graph`

Configuration for the execution graph (DAG phases, ordering, timeouts).

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
**Default**: `20`  
**Range**: `1` to `1000`

The maximum number of total steps (phases × retries) before aborting. If a phase fails and retries, each attempt counts as a step.

### `graph.node_timeout`

**Type**: String (duration)  
**Default**: `"10m"`  
**Example**: `"30m"`, `"1h"`

The timeout for a single node (phase) to complete. If a phase takes longer, it is cancelled and marked failed.

### `graph.approval`

**Type**: Boolean  
**Default**: `true`

Whether belay stops after planning so a human can read the plan before any code
is written.

With `true`, the run pauses at the approve node and exits cleanly; the plan is
at `<run-dir>/artifacts/plan.md`, you may edit it, and `belay resume` continues
from there — reading the file again, so your edits are what the agent receives.
Because the pause is a clean exit rather than a blocked terminal, a run can
wait indefinitely and survive the session that started it.

With `false`, planning routes straight to coding. That is the right setting for
an unattended or CI run, and the wrong one for the first time you point belay
at a repository you care about.

## `test`

Configuration for test execution (phase 5).

### `test.runner`

**Type**: String  
**Default**: `"auto"`  
**Valid values**: `"auto"`, `"go"`, `"npm"`, `"python"`, `"custom"`

The test runner to invoke:
- `"auto"`: Detect from file extensions (*.go → go test, package.json → npm test)
- `"go"`: `go test ./... -cover`
- `"npm"`: `npm test`
- `"python"`: `pytest .`
- `"custom"`: Use the command specified in `test.custom_cmd`

### `test.custom_cmd`

**Type**: String  
**Default**: (empty)  
**Example**: `"make test"`, `"./scripts/run-tests.sh"`

When `test.runner` is `"custom"`, the exact command to run. Must exit with status 0 for pass, non-zero for fail.

## `review`

Configuration for code quality review gates (phases 6 and 7).

### `review.mode`

**Type**: String  
**Default**: `"lint"`  
**Valid values**: `"lint"`, `"sonar"`, `"ai"`

The review strategy:
- `"lint"`: Run local linters (golangci-lint, eslint, etc.). Fast, zero setup, no cost.
- `"sonar"`: Query SonarQube server for code quality. Requires infrastructure setup.
- `"ai"`: Run Claude Code as a reviewer (catches semantic bugs, style issues). Costs tokens.

### `review.fail_on`

**Type**: String  
**Default**: `"blocker"`  
**Valid values**: `"error"`, `"warning"`, `"blocker"`, `"any"`

The severity threshold for failing the review:
- `"error"`: Only error-level issues cause failure
- `"warning"`: Warning and error issues cause failure
- `"blocker"`: Only blocker-level issues cause failure (least strict)
- `"any"`: Any issue (including notes) causes failure (most strict)

### `review.sonar` (if `review.mode == "sonar"`)

Configuration for SonarQube reviewer.

#### `review.sonar.runner`

**Type**: String  
**Default**: `"docker"`  
**Valid values**: `"docker"`, `"server"`

How to access SonarQube:
- `"docker"`: Spawn SonarQube in Docker container (requires Docker, `docker` binary in PATH)
- `"server"`: Connect to existing SonarQube server (requires `review.sonar.server_url` and SONAR_TOKEN env var)

#### `review.sonar.image`

**Type**: String  
**Default**: `"sonarsource/sonar-scanner-cli"`

Docker image to use when `review.sonar.runner == "docker"`. Example: `"sonarqube:11.0"`.

#### `review.sonar.quality_gate_wait`

**Type**: String (duration)  
**Default**: `"30s"`

Time to wait for SonarQube to compute quality gate status after submitting analysis. If the gate is not ready after this timeout, analysis is marked failed.

#### `review.sonar.quality_gate_timeout`

**Type**: String (duration)  
**Default**: `"10m"`

Total time to wait for SonarQube analysis and quality gate computation before giving up.

### `review.ai` (if `review.mode == "ai"`)

Configuration for AI (Claude Code) reviewer.

#### `review.ai.mcp`

MCP connection settings for the AI reviewer.

##### `review.ai.mcp.mode`

**Type**: String  
**Default**: `"auto"`  
**Valid values**: `"auto"`, `"stdio"`, `"docker"`

How to connect to the MCP server:
- `"auto"`: Auto-detect (stdio if available, otherwise Docker)
- `"stdio"`: Connect via stdin/stdout (requires agent binary in PATH)
- `"docker"`: Run agent in Docker container

##### `review.ai.mcp.url`

**Type**: String  
**Default**: (empty; only used for remote MCP servers)

URL to a remote MCP server (e.g., `"http://mcp-server:3000"`). Leave empty for local MCP via stdio or Docker.

##### `review.ai.mcp.image`

**Type**: String  
**Default**: `"anthropic/claude-code:latest"`

Docker image to use when `review.ai.mcp.mode == "docker"`.

## `budget`

Cost and token limits to prevent unexpected bills (see [ADR-0007](adr/0007-budget-ledger-day-one.md)).

### `budget.max_usd`

**Type**: Float  
**Default**: `10.0`  
**Example**: `"5.5"`, `"100.0"`

Maximum spend (in USD) before belay aborts. Set to `0` to disable cost limits (not recommended in production).

### `budget.max_tokens`

**Type**: Integer  
**Default**: `1000000`

Maximum token count (input + output) before belay aborts. Set to `0` for unlimited.

### `budget.on_exceed`

**Type**: String  
**Default**: `"abort"`  
**Valid values**: `"abort"`, `"warn"`

What to do when budget is exceeded:
- `"abort"`: Stop immediately and fail the run
- `"warn"`: Log a warning but continue (not recommended)

## `fanout`

Configuration for parallel candidate generation (phase 4).

### `fanout.enabled`

**Type**: Boolean  
**Default**: `false`

Whether to enable fanout (parallel code generation with multiple candidates).

### `fanout.candidates`

**Type**: Integer  
**Default**: `3`  
**Minimum**: `2`

Number of parallel candidates to generate. Each candidate runs in isolation. Cost multiplies by this factor (3 candidates = 3× budget usage).

### `fanout.isolator`

**Type**: String  
**Default**: `"dircopy"`  
**Valid values**: `"dircopy"`

Isolation strategy for candidates:
- `"dircopy"`: Plain directory copies. Works on non-git targets and uses O(N × repo_size) disk — the trade [ADR-0004](adr/0004-directory-copies-for-fanout.md) accepted deliberately. Note that `.git` is **not** copied, so a candidate cannot run git commands.

Git worktrees and containers are anticipated by the `belay.Isolator` interface but are not implemented; `dircopy` is the only value the loader accepts today.

### `fanout.select`

**Type**: String  
**Default**: `"fewest_issues"`  
**Valid values**: `"fewest_issues"`, `"first_pass"`, `"fastest"`

How to choose the winning candidate after fanout:
- `"fewest_issues"`: The passing candidate with the fewest review issues
- `"first_pass"`: The first candidate that clears the quality gate
- `"fastest"`: The candidate that finished soonest

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

### `SONAR_TOKEN` (required if using SonarQube server)

Your SonarQube authentication token. Required if `review.sonar.runner == "server"`. Must be set in the shell; never include it in belay.yaml.

Example:
```bash
export SONAR_TOKEN="squ_..."
belay run
```

### `DOCKER_HOST` (optional)

Read by the `docker` CLI itself, not by belay. It therefore affects the SonarQube Docker runner (`review.sonar.runner: docker`) and the Docker-mode MCP bridge, because both shell out to `docker`. belay has no Docker setting of its own and never parses this value.

## Example Configuration

```yaml
version: 1

agent:
  backend: "claude-code"
  model: "sonnet"
  max_turns: 5
  mode: "live"

graph:
  give_up: 3
  max_steps: 20
  node_timeout: "10m"
  approval: true

test:
  runner: "auto"

review:
  mode: "lint"
  fail_on: "major"

budget:
  max_usd: 5.0
  max_tokens: 500000
  on_exceed: "abort"

fanout:
  enabled: true
  candidates: 3
  isolator: "dircopy"
  select: "fewest_issues"
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

Belay validates the configuration on startup:

- Missing required fields (e.g., `version`) are errors
- Invalid field types (e.g., `max_turns: "five"` instead of `5`) are errors
- Unknown fields are warnings (allows forward compatibility)
- Timeout and duration fields must be valid Go durations (e.g., `"10m"`, `"1h30m"`)

If validation fails, belay exits with a clear error message and suggests fixes.

## Configuration Precedence

1. belay.yaml (file)
2. Environment variables (override file)
3. CLI flags (override both)

Example:
```bash
# belay.yaml says max_usd: 10.0
# Override via CLI flag:
belay run --budget-max-usd 5.0
```

## References

- [ADR-0007](adr/0007-budget-ledger-day-one.md): Budget ledger and cost caps
- [ADR-0008](adr/0008-local-linters-default.md): Linters as default review mode
