# belay

belay is a durable, resumable orchestrator for autonomous coding agents. Instead of one unreliable AI call that crashes and loses work, it runs a persistent, multi-phase state machine — plan → approve → code → write → test → fix → review — where state is journaled after every node. When a run crashes, the next invocation resumes from the last completed node, not from the start.

**The Problem It Solves**: When you invoke a code-generating agent once, you get a response. If it crashes, you lose everything and re-run from zero, wasting tokens on work already done. If tests fail, you re-prompt by hand with no loop. If there is no budget cap, a prompt bug can burn money. If the generated code is never reviewed, it may break your standards. belay builds all the logic that wraps the agent — the crash recovery, the retry loops, the approval gates, the cost tracking, the quality gates — so you can point it at a repository and get reliable software generation without operator intervention.

```bash
# Run belay once. It writes a plan to disk and stops for your review.
belay run "add error handling to the JSON parser"

# Approve the plan and continue from where it left off.
belay resume

# Watch every step: timing, cost, test results, quality gate, agent output.
belay timeline
```

## Install

### macOS — Homebrew

```bash
brew tap dhaam-ai/belay
brew install belay
belay version
```

Requires the tap to be published; until then use one of the options below.

### Go 1.26+

```bash
go install github.com/dhaam-ai/belay/cmd/belay@latest
```

### A release binary

Download the archive for your platform from the
[releases page](https://github.com/dhaam-ai/belay/releases), verify it, and put
it on your PATH:

```bash
tar -xzf belay_0.1.0_darwin_arm64.tar.gz
shasum -a 256 -c checksums.txt --ignore-missing
sudo mv belay /usr/local/bin/
```

### From source

```bash
git clone https://github.com/dhaam-ai/belay.git
cd belay
make build          # -> ./bin/belay
```

### Windows — use WSL2

belay does not run natively on Windows and no Windows binary is published.
`internal/exec`, `internal/runner` and `internal/linter` are `//go:build unix`,
because the two controls that make belay safe to point at a repository — the
process-group kill that stops a runaway agent, and the `os.Root` containment
that stops it writing outside the workspace — have no direct Windows
equivalent. Shipping a build without them would be shipping something weaker
while calling it the same tool.

Inside WSL2 belay is simply a Linux program, and everything works:

```powershell
wsl --install                      # once, then reboot
```

```bash
# inside the WSL2 shell
sudo apt update && sudo apt install -y golang-go git
git clone https://github.com/dhaam-ai/belay.git
cd belay && make build && ./bin/belay --help
```

Install Claude Code inside WSL too, and keep your repositories on the Linux
filesystem (`~/code/...`) rather than under `/mnt/c/`. Windows-mounted paths go
through a translation layer that is slow enough to change how a test-and-fix
loop feels, and their permission model does not match what belay's containment
checks expect.

belay runs natively on **macOS and Linux**, amd64 and arm64.

### You also need an agent CLI

belay orchestrates an agent; it is not one. Install
[Claude Code](https://claude.com/product/claude-code) and sign in. belay
inherits that login — you do not need to set `ANTHROPIC_API_KEY`, though it is
honoured if present.

To run on a Claude Pro, Max, Team or Enterprise subscription without relying
on that stored login, create a long-lived token and export it. belay passes it
to Claude Code and redacts it from everything it records, exactly as it does an
API key:

```bash
claude setup-token                          # prints the token once
export CLAUDE_CODE_OAUTH_TOKEN="<that token>"
```

belay 0.1.0 dropped this variable, so `claude` started signed out and exited 1
in about a second, having spent nothing. Upgrade to 0.1.1 or later.

```bash
claude --version     # belay shells out to this
```

From 0.1.4, belay needs Claude Code 2.1.186 or later, and is tested with
2.1.274. It limits each step to its own tools with `--tools` and
`--strict-mcp-config`. Claude Code fixed how `--tools` handles search tools in
2.1.162, and feature-gated tools in 2.1.186.

belay does not replace Claude Code — it drives it. The relationship is worth
being precise about, because it decides when you would reach for which:

| | Claude Code | belay |
|---|---|---|
| You are | at the keyboard | not at the keyboard |
| Failure | you see it and redirect | routed back to a repair step, up to `give_up` |
| Cost | you notice | capped, and printed before it spends |
| Interruption | the session is gone | the run is on disk; `belay resume` continues |
| Quality bar | your judgement | a gate that must pass before the run can end |

Use Claude Code for work you want to steer. Use belay for work you want to
hand over and check later — and for anything you want a record of, since every
run leaves a journal, a plan, a diff, and a per-step cost you can read back
with `belay timeline`.

belay uses whatever Claude Code login you already have, and the same model
selection: `agent.model` in `belay.yaml` is passed straight through as
`--model`.

## Five-Minute Quickstart

### 1. Prepare your repository

belay works on any Go, Node, or Python project with tests. It will auto-detect your toolchain. If you have a custom test command, you can configure that too.

```bash
cd /path/to/your/project
# Make sure `git status` shows no uncommitted changes,
# because belay will write files to disk if you approve the plan.
```

### 2. Create a belay.yaml

The simplest config: paste this into your project root.

```yaml
version: 1
agent:
  backend: "claude-code"
  model: "sonnet"
graph:
  approval: true          # Pause after planning so you can review
  give_up: 3              # Retry failing tests up to 3 times
  max_steps: 50           # Stop after 50 total node executions
review:
  mode: "lint"            # Use local linters (golangci-lint, eslint, ruff)
  fail_on: "major"        # Fail if linter finds major issues or worse
budget:
  max_usd: 5.0            # Spend at most $5, then stop
  on_exceed: "abort"      # Abort if budget is hit
```

### 3. Set a credential

Pick one. An API key is billed per call:

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
```

A Claude subscription uses a token from `claude setup-token`:

```bash
export CLAUDE_CODE_OAUTH_TOKEN="<token from claude setup-token>"
```

belay calls the Claude Code CLI (`claude` binary) and passes it whichever of
these is set. With neither, belay relies on the login Claude Code already has.

### 4. See what belay would do (without spending anything)

```bash
belay run --dry-run "add a --json flag to the CLI output"
```

This shows:
- The goal you entered
- Your budget and cost ceiling
- What tools belay found (tests, linters)
- What agent backend it will call
- How many steps the graph will take
- Where it would write files
- **But does not create anything or spend money.**

Example output:

```
This is a dry run. belay is only describing the work; none of it has happened.

what you asked for
  add a --json flag to the CLI output

where belay would work
  /path/to/your/project
  belay found: Go project with tests

the most it could spend
  $5.00 or 1,000,000 tokens, whichever comes first

settings it would use
  read from    /path/to/your/project/belay.yaml
  agent        claude-code, model sonnet, calling the agent for real, up to 5 turns per step
  tests        go test -json -count=1 ./... (auto-detected)
  quality gate golangci-lint (auto-detected), failing on major issues or worse
  credentials  ANTHROPIC_API_KEY is set; CLAUDE_CODE_OAUTH_TOKEN is not set; SONAR_TOKEN is not set

the route it would take
  plan → approve → code → write → test → fix → review → done
  graph.approval is true, so belay will stop after planning and wait for you

what it would create
  /path/to/your/project/.belay/runs/<run-id>
  holding manifest.json, state.json, journal.ndjson and the plan

belay would proceed with this run.

Nothing was created and nothing was spent.
```

### 5. Start a real run

```bash
belay run "add a --json flag to the CLI output"
```

belay writes a plan to `.belay/runs/<run-id>/plan.md` and stops, waiting for your approval. Read it.

```bash
# Look at the plan:
cat .belay/runs/*/plan.md

# If you like it, continue:
belay resume
```

If something goes wrong, you can edit the plan before resuming, and belay will use your edited version.

### 6. Watch the run unfold

```bash
# List all runs:
belay runs

# Watch one in real-time:
belay timeline <run-id>
```

Example timeline output:

```
20260905T153301Z-1f2e3d4c5b6a: "add a --json flag to the CLI output"

Phase           Outcome    Time      Cost
────────────────────────────────────────────
plan            ok         12.3s     $0.04
approve         ok (auto)  0.0s      $0.00
code            ok         45.2s     $0.18
write           ok         2.1s      $0.00
test            failed     3.5s      $0.00
  (1 test failing: handlers_test.go::TestJSONFlag)
fix #1/3        ok         28.4s     $0.12
test #2         failed     2.1s      $0.00
  (1 test failing: handlers_test.go::TestJSONFlag)
fix #2/3        ok         31.2s     $0.15
test #3         ok         2.9s      $0.00
review          ok         18.7s     $0.08
────────────────────────────────────────────
DONE            ok         146.4s    $0.57 ≤ $5.00

Files changed:
  cmd/cli.go (new function JSONOutput)
  handlers_test.go (new tests for JSON mode)
  go.sum (1 new dependency)
```

### 7. The output is yours to keep

All state lives in `.belay/runs/<run-id>`:
- `manifest.json` — the run's complete state (goal, changes, budget tracking)
- `journal.ndjson` — append-only log of every state transition
- `state.json` — current accumulated state (read by `belay resume`)
- `plan.md` — the plan the agent generated

If the run crashed mid-execution, your work is safe on disk. Restart with `belay resume` and it picks up where it left off.

If you want to delete a run, `rm -rf .belay/runs/<run-id>` is safe.

## How It Works (in One Picture)

```
┌──────────────────────────────────────────────────────┐
│ belay run "your goal"                                │
│  ↓                                                   │
│ ┌────────────────────────────────────────────────┐  │
│ │ plan (Agent)      → Generate a plan            │  │
│ │  ↓                                              │  │
│ │ approve (Human)   → Read plan, edit if needed  │  │
│ │  ↓                                              │  │
│ │ code (Agent)      → Generate code              │  │
│ │  ↓                                              │  │
│ │ write (Agent)     → Save files, format         │  │
│ │  ↓                                              │  │
│ │ test (Runner)     → Run test suite             │  │
│ │  ├─ pass? → review (next)                      │  │
│ │  └─ fail? → fix (loop)                         │  │
│ │      ↓                                          │  │
│ │ fix (Agent) × up to N retries                  │  │
│ │      ↓                                          │  │
│ │ review (Linter)   → Check quality gate         │  │
│ │  ├─ pass? → DONE                               │  │
│ │  └─ fail? → fix (loop again)                   │  │
│ └────────────────────────────────────────────────┘  │
│  Each step's result is journaled to disk              │
│  If belay crashes, the next invocation resumes here   │
└──────────────────────────────────────────────────────┘
```

For every phase:
1. **State** is read from disk (on resume, or initialized fresh)
2. **The phase runs** (agent, test runner, linter, etc.)
3. **Result is validated** (e.g., generated code is well-formed)
4. **Result is persisted to journal** (so a crash does not lose it)
5. **State is updated** with the result
6. **Next phase is decided** (automatically or by human gate)

If a crash occurs mid-execution, the journal file may have a half-written last line, but earlier lines are durable. `belay resume` detects the incomplete line, discards it, and resumes from the last fully-written state. This is not an error; it is expected. durability.

## The Four Pillars

### 1. Durable + Resumable Runs
Every phase's result is persisted to an append-only journal *before* the next phase starts. If a run crashes, the next `belay resume` reads the journal and picks up from the last completed phase, not from the beginning. Tokens spent on completed work are not wasted.

### 2. Approval Gates
The `approve` phase pauses the run after the agent writes a plan. You can read it, edit it, and decide whether to proceed. This stops a runaway agent before it writes code. Runs also pause if quality gates fail and manual review is needed.

### 3. Best-of-N Candidates (Fanout)
Enable `fanout` in belay.yaml to generate N code candidates in parallel, each in its own isolated workspace. The `join` phase ranks them by test pass rate, coverage, and cost, and returns the best one. This exploits agent variance to get higher quality output. (Currently disabled; joining is manual; see *Maturity & Limitations* below.)

### 4. CLI-Agnostic Orchestration
The graph definition and all orchestration logic are independent of which agent backend you use. The `belay.AgentBackend` interface is public: you can implement Claude, Codex, Gemini, or your own LLM. The same belay workflow runs against any backend without code changes.

## Configuration

Every belay run reads `belay.yaml` from your project root (or via `--config <path>`). The file controls:
- **agent**: which LLM backend, model, and mode (live, streaming, batch)
- **graph**: approval gates, max retries, timeout per phase
- **test**: auto-detect or custom test command
- **review**: linter mode (local, SonarQube) or AI review
- **budget**: spend ceiling in USD and tokens
- **fanout**: enable parallel candidates

See `docs/config.md` for the complete reference, including examples.

## Architecture & Design

belay is built on a few core ideas:

1. **State machine, not a list of tasks**: Each phase (plan, code, test, fix, review) is a node in a directed graph. Nodes can loop (test → fix → test) and branch (test success → review, test failure → fix). This makes the logic clear and testable.

2. **Dispatcher owns durability**: A single `Dispatcher` orchestrates the graph, and it is responsible for persisting state. Phases themselves are pure functions: they take context, return a result, and never touch the filesystem. This separation makes phases testable in isolation and makes the durability logic auditable.

3. **Append-only journal**: All state transitions are written to an NDJSON journal file with `O_APPEND` and explicit fsync. This means a crash mid-write corrupts only the current record, not past ones. Resume logic detects and heals torn tails.

4. **Pluggable adapters**: The graph talks to the outside world (agent backend, test runner, linter, etc.) only through interfaces. Each is implemented once (Claude Code for agents, golangci-lint for linters) and plugged in at runtime. This makes it easy to add new backends or swap them out.

For the full design, see `docs/architecture.md`.

## Maturity & Limitations

belay is **early but real**. Use it for:
- Side projects and learning
- Teams that can afford 1-2 hours of downtime if something breaks
- Code that is reviewed before merge anyway

### Known Limitations

1. **Only Claude Code is shipped**: v0.1 includes the Claude Code agent backend. The `AgentBackend` interface is exported so Codex and Gemini can be added, but only Claude Code is tested and supported. See ADR-0005.

2. **Module path is a placeholder**: `github.com/dhaam-ai/belay` will change when the real repo is public. Use `go get github.com/dhaam-ai/belay@latest` or clone from the URL when it exists.

3. **Unix only**: belay's process runner, linter launcher, and workspace isolation use unix-specific system calls. Windows and macOS 11 (Monterey) are not supported. (Contributions welcome.)

4. **Fanout is disabled by default**: Parallel candidate generation works, but joining (choosing the best candidate and merging it back) is currently manual. Set `fanout.enabled: true` in belay.yaml to spawn candidates, then inspect `.belay/runs/<run-id>/candidates/` and manually copy the best one back. The automatic join phase is unimplemented. See ADR-0004.

5. **No published fix rate**: belay has a test harness (`test/fixrate`) that measures "what percentage of runs result in code that passes tests and linting?" but no published number yet. We are working on benchmarks. Do not rely on any claimed success rate.

6. **SonarQube integration is optional**: The challenge names SonarQube as a quality gate. belay ships with local linters (golangci-lint, eslint, ruff) as the default. SonarQube is available via `review.mode: sonar` if you have Docker or a server. See ADR-0008.

7. **No credentials management**: belay reads ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN from your shell, or relies on Claude Code's own login. It does not manage secrets, store tokens, or integrate with secret vaults.

## Contributing

belay welcomes contributions. Read `CONTRIBUTING.md` for commit message conventions.

### Development

```bash
git clone https://github.com/dhaam-ai/belay.git
cd belay
make build
./bin/belay --help
make test
```

**Tests use recorded cassettes**: External interactions (HTTP calls, agent responses, tool output) are recorded in `testdata/` and replayed in CI. To add a new test:
1. Write a test that calls belay's code
2. Run `make test-record` locally to capture real interactions
3. Commit the cassette
4. CI replays the cassette, so no API credentials are needed in GitHub Actions

See `docs/adr/0009-record-replay-cassettes.md` for the cassette format.

### Roadmap

- [ ] Implement the join phase (automatic best-candidate selection and merge-back)
- [ ] Windows and macOS 11 support (process isolation)
- [ ] Codex and Gemini backends (implement `AgentBackend` for each)
- [ ] Published fix-rate benchmarks
- [ ] Stronger cost estimation (today we use fallback tables when APIs don't report costs)
- [ ] Multi-repository runs (one belay invocation orchestrates changes across repos)
- [ ] Integration with GitHub / GitLab (open PRs, request reviews, merge on approval)

## License

Licensed under the Apache License, Version 2.0. See `LICENSE` for details.

Copyright (c) The belay Authors.
