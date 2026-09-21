# belay

belay is a durable, resumable orchestrator for autonomous coding agents. Instead of one unreliable AI call that crashes and loses work, it runs a persistent, multi-step state machine — plan → approve → code → write → test → fix → review — where every node's result is saved and journaled before the next node starts. When a run crashes, `belay resume` picks it up after the last completed node, not from the start.

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
```

In a git repository, belay works on its own branch — `belay/<run-id>`, in its
own worktree — and commits once after the quality gate passes. Your checkout
is never edited, so uncommitted work in it is safe; it is also not part of the
run, which starts from the commit you are on. Outside a git repository belay
edits the workspace directly. It says which it is doing before it spends
anything, and `belay --dry-run run` says it without creating anything.

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
- The most it could spend, and what reaching that ceiling does
- The configuration files it read, the agent it will call, and the tests and linter it found
- Which credentials are set, without their values
- The route through the graph, and when belay gives up
- Where it would record the run
- **But does not create anything or spend money.**

Example output:

```
This is a dry run. belay is only describing the work; none of it has happened.

what you asked for
  add a --json flag to the CLI output

where belay would work
  /path/to/your/project
  belay found: go

where its result would go
  belay is editing this workspace directly: /path/to/your/project is not inside a git repository

the most it could spend
  $5.00 or 2,000,000 tokens, whichever comes first
  belay stops the run before the next step

settings it would use
  read from    /path/to/your/project/belay.yaml
  agent        claude-code, model sonnet, calling the agent for real, up to 30 turns per step
  tests        go-test
  quality gate golangci-lint, failing the run on major issues or worse
  credentials  ANTHROPIC_API_KEY is set; CLAUDE_CODE_OAUTH_TOKEN is not set; SONAR_TOKEN is not set

the route it would take
  plan → approve → code → write → test → fix → review → done
  approve stops the run so you can read the plan before any code is written
  it gives up after 3 rounds of the test-and-fix loop, or 50 steps in all

what it would create
  /path/to/your/project/.belay/runs/<run-id>
  holding manifest.json, state.json, journal.ndjson and the plan

Nothing was created and nothing was spent. Run the same command without --dry-run to start.
```

### 5. Start a real run

```bash
belay run "add a --json flag to the CLI output"
```

belay writes a plan to `.belay/runs/<run-id>/artifacts/plan.md`, prints where it is, and stops, waiting for your approval. Read it.

```bash
# Look at the plan:
belay plan

# If you like it, continue:
belay resume
```

If something goes wrong, you can edit the plan before resuming, and belay will use your edited version.

If it is not what you wanted at all, have the agent write another one instead
of editing this one yourself:

```bash
belay plan --regenerate "fewer steps, and leave the parser alone"
```

belay keeps a copy of the plan it replaces beside it, writes the new plan with
your words in the prompt, and stops at the plan again so you can read it. That
is a step of the run like any other: it is journaled, it counts against the
budget, and `belay resume` picks it up if it is interrupted.

### 6. Watch the run unfold

```bash
# List all runs:
belay runs

# See how one unfolded, step by step:
belay timeline <run-id>
```

Example timeline output:

```
run     20260905T142530Z-1f2e3d4c5b6a
goal    add a --json flag to the CLI output
status  finished

   #  node       outcome   duration    cost
   1  plan       finished     12.3s  $0.040  █████
      plan archived to artifacts/plan.md (1843 bytes); next approve
   2  approve    paused        0.0s  $0.000  █
      Plan is waiting for your approval. Review or edit /home/you/app/.belay/runs/20260905T142530Z-1f2e3d4c5b6a/artifacts/plan.md, then continue with: belay resume 20260905T142530Z-1f2e3d4c5b6a
      waited 4m 12.0s here, then resumed
   3  approve    finished      0.0s  $0.000  █
      Plan approved; proceeding to code.
   4  code       finished     45.2s  $0.180  ██████████████████
      code: claude-code reported changes to 3 file(s) (new session)
   5  write      finished      0.1s  $0.000  █
      verified 3 changed file(s): 3 present, 0 deleted; recorded artifacts/diff-0005.patch
   6  test #1/3  finished      3.5s  $0.000  █
      go-test: 12 tests, 1 failed
   7  fix #1/2   finished     28.4s  $0.120  ███████████
      fix attempt 1 of 3: 1 of 12 tests failing
   8  test #2/3  finished      2.1s  $0.000  █
      go-test: 12 tests, 1 failed
   9  fix #2/2   finished     31.2s  $0.150  ████████████
      fix attempt 2 of 3: 1 of 12 tests failing
  10  test #3/3  finished      2.9s  $0.000  █
      go-test: 12 tests, 0 failed
  11  review     finished      6.4s  $0.000  ███
      gate pass: no issues at or above fail_on=major (source=golangci-lint, report=artifacts/review-11.json)

  loops       test ran 3 times, fix ran 2 times
  loop cost   steps 6-10 cost $0.270 and 1m 08.1s — 55% of the run's spend
  agent time  2m 12.1s across 11 nodes
  wall clock  6m 37.1s   2026-09-05 14:25:31Z → 14:32:08Z
  spend       $0.490 of the $5.00 ceiling

Files changed:
  cmd/cli.go
  go.sum
  handlers_test.go
```

### 7. The output is yours to keep

Everything belay records about a run lives in `.belay/runs/<run-id>`:
- `manifest.json` — what the run is and where it stands: its goal, status and current step, the settings it started with, and what it has spent
- `state.json` — what the steps have produced so far, which every step reads: the plan, the files changed, the latest test and review results, how many repairs were made
- `journal.ndjson` — an append-only record of every step's start and finish, from which `belay resume` works out where to pick up
- `artifacts/` — the plan the agent generated (`artifacts/plan.md`), the agent's summary of the code it wrote, the diff each write step recorded, and the test and quality-gate reports
- `nodes/` — for each step that called the agent, the prompt it sent and the reply it got

If you stopped the run with Ctrl-C, or it crashed mid-execution, your work is safe on disk. `belay resume` picks it up where it left off: the step that was interrupted runs again, and the steps that finished do not. A run that belay stopped on its own, at the budget ceiling, `graph.max_steps` or `graph.node_timeout`, is waiting too: raise that setting in belay.yaml and `belay resume` carries on with what the run has already spent still counted, rather than spending it again from the beginning.

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
│ │ write (belay)     → Record the changed files   │  │
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

For every step:
1. **The limits are checked** — the budget ceiling and `graph.max_steps` — before anything is spent
2. **The blackboard is read** from `state.json`, and the step's start is journaled
3. **The node runs** (agent, test runner, linter, etc.)
4. **Its result is checked**: it must have a status, and name the next node if it succeeded
5. **Its changes are saved** to `state.json`
6. **Its finish is journaled**, naming the next node — only after its changes are saved, so the journal never records a finished step whose changes are missing

If a crash occurs mid-execution, the journal file may have a half-written last line, but earlier lines are durable. `belay resume` discards the incomplete line and picks up from the last complete record, running again the step that was cut off. This is expected, not an error.

## The Four Pillars

### 1. Durable + Resumable Runs
Every step's result is saved, and its finish journaled, *before* the next step starts. If a run crashes, the next `belay resume` reads the journal and picks up after the last completed step, not from the beginning. Tokens spent on completed work are not wasted.

### 2. Approval Gates
The `approve` step pauses the run after the agent writes a plan. You can read it, edit it, and decide whether to proceed. This stops a runaway agent before it writes code. It is the only step that pauses a run for you. A failing quality gate does not pause: it sends the change to `fix`, like a failing test, and a run that `graph.give_up` repairs have not brought to passing tests and a passing gate ends as failed.

### 3. Best-of-N Candidates (Fanout) — not working yet
The aim is to give one goal N independent candidates, each in its own workspace, and keep the best, so that the variance between agent runs works for you. Two pieces of that exist: a `fanout` node that prepares one workspace per candidate, and a `join` node that picks a winner among the candidates whose tests passed. No run reaches either one, and nothing runs the candidates. See *Maturity & Limitations* below.

### 4. CLI-Agnostic Orchestration
The graph definition and all orchestration logic are independent of which agent backend you use. The `belay.AgentBackend` interface is public: you can implement Claude, Codex, Gemini, or your own LLM. The same belay workflow runs against any backend without code changes.

## Configuration

Every belay run reads `belay.yaml` from your project root. Your user config is read before it and a file you pass with `--config <path>` after it, merged setting by setting, so a setting in a later file wins. The file controls:
- **agent**: which backend and model, and the mode: `live`, `record` or `replay`
- **graph**: the approval gate, how many repairs before giving up, the step cap, and the timeout per step
- **test**: auto-detect or custom test command
- **review**: the quality gate: local linters (`lint`), SonarQube (`sonar`), or the AI reviewer (`ai`), which `belay run` does not use yet
- **budget**: spend ceiling in USD and tokens
- **fanout**: best-of-N candidates, which no run uses yet (see *Maturity & Limitations*)

See `docs/config.md` for the complete reference, including examples.

## Architecture & Design

belay is built on a few core ideas:

1. **State machine, not a list of tasks**: Each step (plan, approve, code, write, test, fix, review) is a node in a directed graph. Nodes can loop (test → fix → test) and branch (test success → review, test failure → fix). This makes the logic clear and testable.

2. **Dispatcher owns durability**: A single `Dispatcher` orchestrates the graph and writes the run's state as it goes: `state.json`, `manifest.json` and the journal. No node writes any of them: a node takes a context and returns a result, which the dispatcher saves. Nodes do touch the filesystem — they write files of their own under the run directory (the plan, diffs, reports, and each agent step's prompt and reply), the agent a node calls edits your working tree, and the test and review steps run tools in it — but none of that decides where a resumed run picks up. This separation makes nodes testable in isolation and makes the durability logic auditable.

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

4. **Fanout does not work yet**: The `fanout` and `join` nodes exist and are unit-tested, but no run reaches them. No step in belay's graph leads to `fanout`, so setting `fanout.enabled: true` changes nothing a run does. `belay run` builds the isolator `fanout` would use — a git worktree per candidate for a branch run, a directory copy otherwise — but nothing reaches it. The `fanout` node only prepares candidates: it creates one workspace per candidate and runs none of them, so no candidate ever has test results. `join` picks a winner among the candidates whose tests passed and routes it to `promote`, which merges that candidate's branch into the run's own (ADR-0012); with `fanout.isolator: dircopy`, or an in-place run, there is no branch to merge and promoting the winner is still manual (ADR-0004). Running candidates is planned, and ADR-0019 is reserved for it.

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

- [ ] Run fanout candidates (ADR-0019); `join` already selects one and `promote` already merges it
- [ ] Windows and macOS 11 support (process isolation)
- [ ] Codex and Gemini backends (implement `AgentBackend` for each)
- [ ] Published fix-rate benchmarks
- [ ] Stronger cost estimation (today we use fallback tables when APIs don't report costs)
- [ ] Multi-repository runs (one belay invocation orchestrates changes across repos)
- [ ] Integration with GitHub / GitLab (open PRs, request reviews, merge on approval)

## License

Licensed under the Apache License, Version 2.0. See `LICENSE` for details.

Copyright (c) The belay Authors.
