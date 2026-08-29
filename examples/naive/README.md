# The Naive One-Shot Example

This directory contains an intentionally minimal, obviously fragile example of how a developer might use an agentic coding CLI in isolation: invoke it once, capture the output, done. It is not production code. Its entire purpose is to motivate why `belay` exists.

## Running the Example

**Warning: This example invokes the real Claude API and charges real tokens to your account.**

```sh
cd examples/naive
./run.sh
```

If `claude` is not on PATH, the script exits cleanly with an error message. The script outputs to `out.json`.

## How It Breaks

The naive script demonstrates **eight failure modes**. Each row below shows a concrete failure of the one-shot approach and how `belay` solves it:

| Failure | Symptom | Belay Solution |
|---------|---------|---|
| **Process dies mid-run** | If the agent is killed, crashes, or hits a timeout, all work is lost. Restarting from zero repeats the entire inference. | [`belay resume`](../README.md#journaled-state-and-resume) restarts at the last completed node. Every node transition is journaled to the state file before execution, so a crash or timeout costs only the current node's tokens. |
| **No visibility into what happened** | The script exits with exit code and a JSON blob. Was the agent stuck? Looping? Refactoring correctly? No timeline. | [`belay timeline`](../README.md#timeline) renders every node transition with its duration, cost, input prompt, agent response, and completion reason. A skeptic can audit every step. |
| **Tests fail → you re-prompt by hand, forever** | If a generated function has a bug, the script stops. There is no loop; you run it again with a different prompt, maybe it works, maybe it doesn't. No cap on iterations. | [`test` and `fix` nodes](../README.md#test-and-fix-loops) automatically re-prompt the agent if tests fail, up to a `give_up_after` limit. The run fails loudly rather than looping indefinitely. |
| **No quality bar beyond "it compiles"** | The script does no lint, no style check, no security scan. If the generated code violates your org's standards, nothing stops it. | [`review` node](../README.md#review) runs golangci-lint, eslint, ruff, or integrates with SonarQube (with `sonar.qualitygate.wait=true`). Failures automatically route back to `fix`. |
| **One attempt, take whatever you get** | If the agent generates code the first time, you have it. There is no way to compare alternatives or select the best by a real metric. | [`fanout` and `join` nodes](../README.md#fanout-and-join) run N isolated candidates in parallel with different prompts or models. The `join` node ranks them by quality (test pass rate, coverage, size, or a custom scorer) and returns the best. |
| **Unbounded spend** | The script has no budget tracking and no cutoff. A loop in the prompt, a runaway model, or an accident can consume unlimited tokens. | [Per-node ledger with budget cap](../README.md#budget-tracking) tracks tokens and USD spent by each node. Every node checks the remaining budget before executing. If a node would exceed the cap, the run stops with a clear message. |
| **No approval point** | The agent runs to completion with no human in the loop. If the plan is wrong or the output needs editing before commit, there is no pause. | [`approve` node](../README.md#approval-gates) pauses after a planning or design phase so a human can review, edit, and approve the plan before the agent writes code. The run resumes from the approval point on `belay resume`. |
| **Locked to one vendor's CLI** | The script is hardcoded to `claude -p`. If you want to run the same graph against OpenAI, Anthropic self-hosted, or a local model, you rewrite the shell script. | [`AgentBackend` interface](../README.md#agent-backend-abstraction) decouples the graph definition from the CLI. Each node specifies a backend by name; the executor loads the matching backend implementation. The same graph runs against Claude, OpenAI, or your own inference service without code changes. |

## Expected Output

When run successfully, you will see:

```
Invoking claude with one-shot prompt...
Prompt: Write a function in Go that validates an email address. Include a docstring explaining the logic.

Done.
Session ID: <session-id>
Turns: 1
Duration: <milliseconds>ms
Cost: $<amount>

Full output written to out.json
```

The full response—including the generated code—is in `out.json`.

## Real-World Example: Why This Matters

Imagine you ask the one-shot script to build a feature (write a handler, add tests, open a PR). Here is what happens:

1. **60 seconds in:** The agent writes initial code. Your home internet drops.
2. **Restart:** You run the script again. The agent re-generates the code from scratch. Another $0.50 wasted.
3. **120 seconds in:** Tests fail. The agent suggests a fix, but the fix is wrong.
4. **No loop:** You manually edit the prompt and run the script again. It re-generates everything from scratch, wasting tokens on the parts that were already correct.
5. **3 minutes later:** You now have something that compiles. Does it pass linting? No. Does it violate your org's code review policy? You do not know until a human looks at it.
6. **4 minutes later:** A human points out a flaw in the algorithm. You edit the prompt again, run the script again, waste more tokens.
7. **Total cost:** $3.00 for work a 46-task graph with memoized state, retries, and approval gates could do in one coherent run for $0.40, with visibility into every step.

`belay` is the difference between "I invoked an agent" and "the agent reliably shipped code."

## Next Steps

See the main [README](../README.md) for the full belay spec, including:
- Graph definition syntax
- Node types and their configuration
- State persistence and recovery
- Cost tracking and budget limits
- API for custom backends and scorers
