# Belay Configuration

Belay is configured via a `belay.yaml` file in the project root. This document describes every field and its effects.

## File Location

Belay looks for `belay.yaml` in the current directory. You can also specify an alternate path with `belay run --config /path/to/config.yaml`.

## Top-Level Fields

```yaml
version: "0.1"
agent: {...}
graph: {...}
test: {...}
review: {...}
budget: {...}
fanout: {...}
```

## `version`

**Type**: String  
**Default**: (required; no default)  
**Example**: `"0.1"`

The belay configuration version. Used to detect breaking changes in the YAML schema. Must match the running belay version (or be compatible).

## `agent`

Configuration for the LLM backend that generates code, plans, and fixes.

### `agent.backend`

**Type**: String  
**Default**: `"claude"`  
**Valid values**: `"claude"`, `"codex"`, `"gemini"`

The agent backend to use. In v0.1, only `"claude"` is shipped and tested. Other backends are design-verified but not implemented (see [ADR-0005](adr/0005-claude-code-only-backend-v0.md)).

### `agent.model`

**Type**: String  
**Default**: `"claude-opus-4-1"`  
**Example**: `"claude-3-5-sonnet"`

The specific model identifier. Used when creating MCP sessions with the agent backend.

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

**Type**: Boolean  
**Default**: `false`

If `true`, the graph stops and returns success if any phase fails. Useful for MVP/exploratory runs where partial results are acceptable.

If `false` (default), the graph retries failed phases up to `graph.max_steps` times.

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

**Type**: String  
**Default**: `"manual"`  
**Valid values**: `"manual"`, `"auto"`

Whether the approval phase requires manual user input:
- `"manual"`: Pause at the approval phase and wait for user to approve or reject
- `"auto"`: Skip manual approval (useful for CI/testing)

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
**Default**: `"sonarqube:10-lts"`

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
**Range**: `2` to `10`

Number of parallel candidates to generate. Each candidate runs in isolation. Cost multiplies by this factor (3 candidates = 3× budget usage).

### `fanout.isolator`

**Type**: String  
**Default**: `"dir"`  
**Valid values**: `"dir"`, `"worktree"`, `"container"`

Isolation strategy for candidates:
- `"dir"`: Plain directory copies (cp -r). Works on non-git targets. Uses O(N × repo_size) disk.
- `"worktree"`: Git worktrees (not yet implemented; see [ADR-0004](adr/0004-directory-copies-for-fanout.md))
- `"container"`: Run each candidate in a Docker container (not yet implemented)

### `fanout.select`

**Type**: String  
**Default**: `"best"`  
**Valid values**: `"best"`, `"first"`, `"all"`

How to choose the winning candidate after fanout:
- `"best"`: Select the candidate with the highest quality score (test pass rate + review gate pass)
- `"first"`: Use the first candidate that passes the quality gate
- `"all"`: Merge all candidates (experimental; not recommended)

## Environment Variables

The following environment variables are used by belay and are never read from `belay.yaml`:

### `ANTHROPIC_API_KEY` (required if using Claude backend)

Your Anthropic API key. Required for agent backend `"claude"` and review mode `"ai"`. Must be set in the shell; never include it in belay.yaml.

Example:
```bash
export ANTHROPIC_API_KEY="sk-ant-..."
belay run --plan "fix the bug"
```

### `SONAR_TOKEN` (required if using SonarQube server)

Your SonarQube authentication token. Required if `review.sonar.runner == "server"`. Must be set in the shell; never include it in belay.yaml.

Example:
```bash
export SONAR_TOKEN="squ_..."
belay run
```

### `DOCKER_HOST` (optional)

Docker daemon socket URL. Used by SonarQube Docker runner and container isolator. Defaults to Unix socket (`unix:///var/run/docker.sock` on macOS/Linux).

## Example Configuration

```yaml
version: "0.1"

agent:
  backend: "claude"
  model: "claude-3-5-sonnet"
  max_turns: 5
  mode: "auto"

graph:
  give_up: false
  max_steps: 20
  node_timeout: "10m"
  approval: "manual"

test:
  runner: "auto"

review:
  mode: "lint"
  fail_on: "error"

budget:
  max_usd: 5.0
  max_tokens: 500000
  on_exceed: "abort"

fanout:
  enabled: true
  candidates: 3
  isolator: "dir"
  select: "best"
```

## Example Configuration with SonarQube

```yaml
version: "0.1"

agent:
  backend: "claude"
  model: "claude-3-5-sonnet"

review:
  mode: "sonar"
  fail_on: "blocker"
  sonar:
    runner: "docker"
    image: "sonarqube:10-lts"
    quality_gate_wait: "30s"
    quality_gate_timeout: "10m"

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
