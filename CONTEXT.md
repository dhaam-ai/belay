# belay

belay runs an autonomous coding agent against a repository as a durable,
resumable graph of steps, so that a crash, a failing test or a spent budget is a
recorded outcome rather than lost work.

## Runs and the graph

**Run**:
One effort to achieve one goal against one repository, from first step to end,
identified by a run ID and recorded under `.belay/runs/<run-id>/`.
_Avoid_: job, session, task, execution

**Goal**:
The natural-language request a run exists to satisfy, exactly as the user typed it.
_Avoid_: prompt, task, instruction

**Graph**:
The fixed set of nodes a run moves through and the edges that route between them.
_Avoid_: pipeline, workflow, DAG (the graph has cycles: test → fix → test)

**Node**:
One named step of the graph — plan, approve, code, write, policy, test, fix,
review, promote, fanout or join — that reads the blackboard and returns a result.
_Avoid_: phase, stage, step (in code; "step" is fine in prose), task

**Dispatcher**:
The one component that executes nodes in order and makes the run durable; no node
writes the blackboard, the manifest or the journal itself.
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

**Attempt**:
One execution of a node within a run. A node cut short by a crash or Ctrl-C is
re-run as the next attempt of the same visit; a later visit to the node, such as
the test node's next round of the fix loop, starts again at attempt 1. The fix
loop's count of repairs (`state.Fix.Attempts`, "fix attempt 2 of 3") is a
different number.
_Avoid_: retry (a retry sounds like a new request; an attempt re-runs the same visit)

**Visit**:
One pass of a run through a node, from the execution that begins it to the last
execution that takes it up again. A gate that paused for a person and ran again when
they resumed, or an attempt a crash cut short and its re-run, is one visit; the test
node's next round of the fix loop is a second. `belay timeline` numbers visits
("test #2/3"), and `belay fork --visit` takes the same numbers.
_Avoid_: execution (one run of a node; a visit can hold several)

**Snapshot**:
A write-once copy of the blackboard as it stood before a node execution, kept under
`snapshots/` in the run directory. A node re-run after a crash is given it instead of
`state.json`, and a fork starts from the snapshot of the visit it re-runs.
_Avoid_: checkpoint, backup, config snapshot (the manifest's record of the settings a
run started with)

**Fork**:
A new run, with its own ID, that re-runs one node of an existing run from the
blackboard as it stood before that node, and never writes to the source run. It
works in a copy of the workspace under `.belay/forks/<fork-id>`, runs that one node
and pauses, or finishes when that node was the last; `belay fork <run-id> <node>`
makes one (ADR-0017). Unrelated to forking
belay's own repository, which is what "fork" means in ADR-0009.
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
The review node's pass/fail judgement on the run's change, from a linter or a
SonarQube scan, expressed as one quality report.
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

**Change**:
What a run did to its workspace, as the workspace itself records it — what git
says differs from the run's starting commit, or what differs from a record of
the workspace's files taken when the run began — whatever its agent reports.
The policy gate judges it, and a branch run commits exactly it. A file in it
the agent did not report is an unreported change.
_Avoid_: diff (the artifact), patch (a blackboard update), change set (fine in prose)

## Best-of-N

**Fanout**:
Working on one goal in N independent ways at once so the best result can be kept.
_Avoid_: parallel runs, best-of-N (fine in prose, not as an identifier)

**Candidate**:
One of the N independent tries at a goal that a fanout sets up, each in its own
workspace.
_Avoid_: attempt (an attempt is one execution of a node), variant, branch (a
candidate may have a branch; it is not one)

**Candidate run**:
The run one candidate executes — its own journal, blackboard, manifest and
budget envelope — nested under its parent run in `candidate-runs/<id>/`, and
resumed only through it.
_Avoid_: child run (in code), subrun

**Envelope**:
The share of the parent run's remaining budget one candidate run may spend,
fixed when the candidate is claimed and never lowered.
_Avoid_: allowance, sub-budget, quota

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
Whatever creates and destroys workspaces — a directory copy or a git worktree.
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
An ordered list of goals belay works through unattended, one run per goal, each
in its own copy of the workspace. A goal whose plan stops at the approve gate is
parked, and the queue moves on to the next; only a person approving it, or
having added it with `--no-approval`, takes it past the gate.
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

**Cell**:
One task of a measurement corpus, in one arm, at one replicate — the unit a
corpus run finishes, pays for and scores once, and never again when it is
resumed. A full SWE-bench Verified run is 500 tasks in two arms: 1,000 cells.
_Avoid_: trial, sample, job
