# belay

belay runs an autonomous coding agent against a repository as a durable,
resumable graph of steps, so that a crash, a failing test or a spent budget is a
recorded outcome rather than lost work.

## Runs and the graph

**Run**:
One attempt to achieve one goal against one repository, from first step to end,
identified by a run ID and recorded under `.belay/runs/<run-id>/`.
_Avoid_: job, session, task, execution

**Goal**:
The natural-language request a run exists to satisfy, exactly as the user typed it.
_Avoid_: prompt, task, instruction

**Graph**:
The fixed set of nodes a run moves through and the edges that route between them.
_Avoid_: pipeline, workflow, DAG (the graph has cycles: test → fix → test)

**Node**:
One named step of the graph — plan, approve, code, write, test, fix, review, fanout
or join — that reads the blackboard and returns a result.
_Avoid_: phase, stage, step (in code; "step" is fine in prose), task

**Dispatcher**:
The one component that executes nodes in order and makes the run durable; nodes
never persist anything themselves.
_Avoid_: engine, runner, scheduler, orchestrator

**Result**:
A node's verdict on the work it was given — including "the tests failed" — as
opposed to an error, which means the node could not reach a verdict at all.
_Avoid_: output, response, return value

**Blackboard**:
The run's shared, accumulated record that every node reads and the dispatcher
updates; persisted as `state.json`.
_Avoid_: context, memory, store, state (ambiguous with the package name)

**Patch**:
The set of blackboard fields a node's result assigns; every field it names is set
outright, never merged or appended.
_Avoid_: diff, delta, update, mutation

**Journal**:
The append-only record of every node start and finish in a run, from which the run
can be resumed and audited.
_Avoid_: log, history, event stream

**Resume**:
Continuing an interrupted run from its last finished node, re-running any node that
started but did not finish.
_Avoid_: restart, retry, recover

**Fork**:
A new run, with its own ID, that re-runs one node of an existing run from the
blackboard as it stood before that node, and never writes to the source run. The
command is `belay fork <run-id> <node>`, which is not built yet (ADR-0017 is
reserved for it). Unrelated to forking belay's own repository, which is what
"fork" means in ADR-0009.
_Avoid_: replay, node re-run

## Gates and loops

**Approve gate**:
The point after planning where a run halts until a human accepts, edits or rejects
the plan.
_Avoid_: approval step, checkpoint, review (review is a different gate)

**Plan**:
The agent's written intent for achieving the goal, which a human may edit before
any code is written.
_Avoid_: spec, design, proposal

**Archived plan**:
A plan moved out of the run's scratch directory into the repository's `docs/` once
the run completes, so it survives as a record beside the code it produced.
_Avoid_: saved plan, exported plan

**Quality gate**:
The review node's pass/fail judgement on the run's change, from linters, SonarQube
or an AI reviewer, expressed as one quality report.
_Avoid_: lint check, code review

**Fix loop**:
The cycle in which a failed test or quality gate is sent back to the agent for
repair, bounded by `give_up`.
_Avoid_: retry loop, repair cycle

**Policy**:
Rules declared in `belay.yaml` that a run's change must obey — paths it may not
touch, limits on its size — independent of whether its tests pass.
_Avoid_: guardrail (in code), rule set, constraint

**Violation**:
One way a change breaks a policy.
_Avoid_: policy error, breach

## Best-of-N

**Fanout**:
Giving one goal N independent attempts at once so the best can be kept.
_Avoid_: parallel runs, best-of-N (fine in prose, not as an identifier)

**Candidate**:
One of the N attempts a fanout creates, each in its own workspace.
_Avoid_: attempt, variant, branch (a candidate may have a branch; it is not one)

**Winner**:
The candidate join selects; there is no winner when every candidate's tests failed.
_Avoid_: best candidate, result

**Promotion**:
Making the winner's change part of the run's own output.
_Avoid_: merge (merge is one mechanism for promotion, not the concept), apply, adopt

**Workspace**:
An isolated working copy one run or candidate works in, created and destroyed by an
isolator and described entirely by plain data so it can be destroyed after a restart.
_Avoid_: sandbox, checkout, worktree (a worktree is one kind of workspace)

**Isolator**:
Whatever creates and destroys workspaces — a directory copy, a git worktree or a
container.
_Avoid_: sandbox, provisioner

## Agents and cost

**Backend**:
The coding agent CLI belay drives, behind the `AgentBackend` contract; Claude Code
is the only one today.
_Avoid_: agent (ambiguous), model, provider, LLM

**Model**:
The specific model a backend is told to use for a node, e.g. `sonnet`.
_Avoid_: backend, engine

**Routing**:
Assigning a different model to different nodes of the same run.
_Avoid_: model selection, per-node models

**Budget**:
A run's spend ceiling in dollars and tokens, checked before spending, never after.
_Avoid_: quota, limit, cap (fine in prose)

**Queue**:
An ordered list of goals belay works through unattended, one run per goal.
_Avoid_: batch, backlog, job list

## Recorded agents

**Cassette**:
A recording, made with `agent.mode: record`, of the requests belay sent the backend
and the responses it got back. `agent.mode: replay` answers from it instead of calling
the backend, so the recorded run repeats without spending anything (ADR-0009). It holds
the conversation, not the workspace: the edits the agent made are not in it. "Replay"
means playing a cassette, and nothing else.
_Avoid_: fixture, mock, recording (fine in prose)

**Scripted mode**:
A fix-rate benchmark run driven by a zero-cost scripted agent, which measures belay's
graph and not any agent's skill. Named `ModeReplay` in code.
_Avoid_: replay mode (collides with cassette)

## Measurement

**Fix rate**:
The share of seeded or real defects a run repairs so that tests and the quality gate
pass.
_Avoid_: success rate, accuracy, score

**Provenance**:
The statement, attached to every measured number, of which mode and which backend
produced it, so a scripted-mode number can never be quoted as an agent's skill.
_Avoid_: source, metadata

**Control arm**:
The same backend on the same corpus and budget with no graph — one call per task —
against which belay's fix rate is compared.
_Avoid_: baseline (overloaded), bare mode, vanilla
