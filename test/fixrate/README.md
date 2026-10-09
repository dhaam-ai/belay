# test/fixrate

The fix-rate measurement harness (T42): the instrument behind belay's
result-driven pillar's headline claim, "belay repairs N/M seeded defects."

If this harness were not rigorous, that claim would be marketing fiction.
This document explains what it measures, how, and what it deliberately
refuses to claim.

## Build tag

This package is gated behind the `fixrate` build tag, so it is invisible to
the root module's default `go build ./...` / `go test ./...`:

```
go build -tags fixrate ./test/fixrate/...
go vet   -tags fixrate ./test/fixrate/...
go test  -tags fixrate ./test/fixrate/... -race
golangci-lint run --build-tags=fixrate ./test/fixrate/...
```

Three of the tests in this package (`TestRun_EndToEnd_ReplayMode_MixedOutcomes`,
`TestRun_Determinism_SameSeedTwice`, `TestRunControl_EndToEnd_PairsWithTheBelayArm`)
drive belay's real dispatcher graph end to end — injecting defects, running
`go build`, `go test -race` (more than once, through the fix loop) and
`golangci-lint` — and two more (`TestRun_ReplayMode_RefusesNonZeroUsage`,
`TestRunControl_ReplayMode_RefusesNonZeroUsage`) inject defects before
refusing. Budget roughly 30-120 seconds for the package's test run, not
milliseconds. Everything else is well under a second.

## What this measures, and what it does not

A fix-rate number is only meaningful if its provenance is unambiguous. This
package has exactly two modes, and every `Result` it produces says which one
made it, in words, as the very first thing in both the JSON and the human
summary:

- **`ModeReplay`** (the default, free): drives belay's graph with a
  zero-cost `belay.AgentBackend` — either this package's own scripted
  repair/cheat/noop agent (`NewScriptedAgent`), or a pre-recorded
  `internal/agent/replay` cassette. **This measures belay's graph — the
  dispatcher, the fix loop, the write node's verification, the review
  gate — not any coding agent's actual skill.** A number produced this way
  is useful for catching a regression in belay's own state machine at zero
  cost; it is not a claim about what a real coding agent can fix, and
  `Result.Provenance` says so explicitly so it can never be quoted as one
  by accident.
- **`ModeLive`**: drives the same graph with a real agent backend the
  caller constructs and supplies. Spends real money, measures real agent
  capability. `Run` refuses `ModeLive` unless `RunOptions.Confirm` exactly
  equals `ConfirmLiveSpend` — a deliberate tripwire against a stray default
  flipping a CI job from free to billed.

**This package never imports an agent-CLI adapter (`internal/agent/claude`
or similar) and constructs no live backend itself.** There is no code path
inside `test/fixrate` capable of starting one; a live run is entirely the
caller's own wiring, which is how T42's "do not invoke the `claude` CLI or
spend anything in this task" requirement is satisfied structurally, not by
promise. Every acceptance run below is `ModeReplay`.

### The zero-cost guarantee is enforced, not just documented

`Run`, for `ModeReplay`, sums `belay.Usage` across the whole belay run
(`graph.Outcome.Usage`, the dispatcher's own cumulative budget ledger) and
returns `ErrReplayModeSpentMoney` instead of a `Result` if that sum is
non-zero. This is a runtime assertion that fires on every call, not a code
comment: an `Agent` that is not actually free makes `Run` fail loudly rather
than silently hand back a result that would misrepresent paid work as free.
See `TestRun_ReplayMode_RefusesNonZeroUsage`.

## Two arms: belay, and the same backend without it

A fix rate measured against nothing cannot be a claim about belay. "belay
repairs 18 of 25" does not say which part of the 18 belongs to belay's
dispatcher, fix loop and quality gate, and which part belongs to the coding
agent underneath, which might have repaired them if it had simply been
pointed at the repository. The figure worth publishing is a **delta**: the
same backend, the same corpus, the same budget, with and without belay's
orchestration. So this package has two arms.

- **`Run`** is the belay arm: belay's real graph, as described below.
- **`RunControl`** is the control arm: exactly **one** raw
  `belay.AgentBackend.Invoke` per task. No plan, no approve gate, no write
  verification, no test node, no fix loop, no quality gate, no second
  attempt.

**What is held identical.** Both arms go through one pipeline (`runArm` in
`run.go`), and only the step in the middle differs. The same subprocess
injects the defects, the same snapshots bracket the work, and the same
post-run tests, lint and cheat-aware classifier judge it. They share one
check because they share the code, not because two copies are kept in step.
The control arm's single call also gets:

- the belay arm's goal text, verbatim;
- the model belay's code node uses (`config.Agent.ModelFor("code")`, read
  from the belay arm's own configuration);
- the code node's tool allow-list (`Read Grep Glob Edit Write`, and no
  shell). The list is replicated, because the code node's is unexported, so
  `TestControlTools_MatchesTheCodeNode` drives the real code node and fails
  if the two ever differ;
- the largest turn and time allowance belay's graph could have spent on an
  agent: belay invokes one at plan, code and up to `graph.give_up` fixes, so
  the control call gets `2 + give_up` times `agent.max_turns` and
  `graph.node_timeout`. On shipped defaults that is **150 turns and 75
  minutes** for one call. The plan node's share is included even though the
  control arm has no planning step, so a control arm that loses cannot have
  lost for want of room. The allowance is printed in every report.

**What is deliberately different, and said so.** Each of these is something
belay adds, which is what is being measured:

1. The prompt is not the code node's prompt, because that one cites a plan
   document no control run ever wrote. The control arm gets the goal plus
   the smallest framing one call needs (`controlPromptTemplate`).
2. There is no system prompt. belay's is part of belay's contribution.
3. There is no session continuation, because there is no second call.
4. Its failures are final. Nothing re-runs the tests and nothing asks
   again.

Both arms drive Claude Code. The second backend was dropped, so this is
not a comparison between backends, and the design's "bare-native" third arm
was dropped with it. Holding the backend fixed is what isolates
orchestration.

### The paired report (`paired.go`)

Both arms run the same tasks, so each task is a matched pair, and the
pairing is the evidence. A task belay resolved and the control arm did not
says something about orchestration; a task both resolved says something
about the backend. `PairedResult` reports, per arm:

- the resolve rate with a 95% **Wilson** interval. The textbook
  approximation collapses to a zero-width interval at 0/n, which is a real
  outcome for a control arm;
- total spend and **USD per resolved task**, in the same block as the rate
  and not below the fold. belay may call an agent five times where the
  control arm calls it once, so it is *expected* to cost more in total, and
  a total-spend-only report reads the interesting case backwards. Spend is
  counted on failed calls too, so a control arm's failures do not flatter its
  cost per fix. An arm that resolved nothing has no cost per resolved task:
  the report prints its absence, not `+Inf`;
- wall clock, meaning the arm's own work, excluding the identical injection
  and evaluation.

And over the pairs: the four cells of the 2x2 table, the resolve-rate
delta, and an **exact** McNemar test on the two discordant cells (the
binomial form, not the chi-squared approximation, which needs far more
disagreements than a corpus this size produces). There is deliberately no
unpaired difference-of-proportions interval for a reader to pick up by
accident.

**What a delta can claim, and what it cannot:**

- In **`ModeReplay`**, a delta is a difference between two of belay's own
  code paths and nothing else. `TestRunControl_EndToEnd_PairsWithTheBelayArm`
  shows `belay_only=1` using a scripted agent that repairs only on its
  second editing call. That proves the pipeline can register a difference.
  It says nothing about any agent, and the mode's provenance line on every
  report says so.
- In **`ModeLive`**, a delta is the effect of belay's orchestration *for
  that backend, that model, that corpus and that configuration*. It does not
  carry to another backend, another model or another corpus.
- A task resolves, in either arm, only when **every** selected defect came
  back `repaired`. One `invalid_repair` leaves the task unresolved, so the
  cheat detector is not undone at the rollup.
- A p-value describes one corpus. No discordant pairs is an *absence of
  evidence* for a difference. It is not evidence that the arms are
  equivalent, and the report's warnings say so in words when it happens.

Both entry points keep every guarantee below. `ModeLive` is refused without
`ConfirmLiveSpend`, and a `ModeReplay` agent that reports spend is refused
after the fact, because `runArm` makes those checks once for both arms. Neither
arm constructs an agent.

## SWE-bench, offline (`swebench.go`)

`swebench.go` turns SWE-bench records into `Task`s both arms can be handed,
and ports SWE-bench's own rule for deciding whether a task resolved. **It
runs nothing.** It builds no container, clones no repository, applies no
patch and touches no network; the rest of the pipeline, which does, is
described under "SWE-bench, for real" below. Every claim below is cited in
the file's doc comment, and anything that could not be confirmed is marked
UNKNOWN there.

- **The pin.** `SWE-bench/SWE-bench_Verified` at
  `78f471bf655a3137b2e8a75af1501690ec009ec3`, which was that dataset
  repository's HEAD on 2026-09-30: one `test` split of 500 instances.
- **The schema.** 17 fields at the pin, taken verbatim from the Hugging Face
  datasets-server feature list. `TestInstance_FieldNamesMatchThePinnedSchema`
  guards them against a misspelt tag. `FAIL_TO_PASS`/`PASS_TO_PASS` are JSON
  arrays at the pin but JSON-encoded *strings* in the `princeton-nlp`
  copies, and the decoder reads both. Anything else is an error, never a
  silent empty list, because an empty `FAIL_TO_PASS` resolves for free in
  SWE-bench's own grading.
- **The grading** is ported from `swebench/harness/grading.py` at
  `ca6e4e0d252f32f8762625b73575d5dee49d0a5a`. It is not exact-name lookup.
  `PASS_TO_PASS` treats `SKIPPED` as maintained where `FAIL_TO_PASS` treats
  it as a failure. A parametrized test id truncated mid-parameter (676 of
  them in Verified, per upstream's own docstring) is prefix-matched when the
  matches agree. Only `pass_and_fail` grading is ported, which is every
  instance at the pin; `fail_only` is refused rather than graded by the
  wrong rule. "Resolved" means SWE-bench's `RESOLVED_FULL`.
- **Three departures**, each closing a way to resolve without fixing
  anything, and none able to turn an unresolved task into a resolved one:
  an empty `FAIL_TO_PASS` (upstream scores it 1); a status outside the
  `TestStatus` enum (upstream drops it from the denominator); and the one
  order-dependent corner of upstream's prefix matching, where the
  conservative verdict is taken.
- **The goal** carries the issue verbatim and asks both arms identically
  for a regression test. It never carries the gold patch, the test patch or
  `hints_text`.

**Licence.** The SWE-bench *code* is MIT licensed. The *dataset* declares
no licence: none of its three Hugging Face repositories has a licence field
or tag, so its licence is **UNKNOWN**. Until that changes, this repository
commits **no dataset content**. Every fixture under `testdata/swebench` is
synthetic, `TestSWEBenchFixturesAreSynthetic` refuses a record file naming
a project SWE-bench draws from, and only aggregate results may ever be
published.

**What a number produced this way can claim, and what it cannot.** A
resolve rate over these tasks can claim one thing: the share of the corpus
whose dataset-supplied tests went from failing to passing, without breaking
the tests that passed before, judged by SWE-bench's own `pass_and_fail`
rule. It **cannot** claim:

- that a fix is right. The tests are the dataset's, and a change can pass
  them and still be wrong, narrow, or unacceptable to the project;
- comparability with a published SWE-bench leaderboard figure, unless the
  test statuses came from the official harness's own Docker containers. Even
  then the goal text here differs from any given submission's prompt;
- anything at all in `ModeReplay`, where no agent reads the issue;
- that the records came from the pinned revision, on its own. A `Source`
  is the caller's declaration, refused unless it names a full commit sha;
  `Download` (below) is what verifies the records against the pin.

UNKNOWN, and stated as such in `swebench.go`: the dataset's licence. No
longer unknown: the official evaluation resets the test files an
instance's `test_patch` touches before applying it (cited in `swebench.go`),
so the regression test the goal asks for is not part of what is scored,
in either arm.

## SWE-bench, for real

Everything above about SWE-bench is offline. This is the part that runs.

| File | What it does |
| --- | --- |
| `dataset.go` | Downloads the pinned revision's one data file and refuses it unless it is byte for byte the pinned file: size 6,304,616 and the sha256 the Hugging Face tree API lists for that revision, and not a different revision by the server's own `X-Repo-Commit`. Converts it to records with the official harness's own loader, then checks every record against the pinned schema, the count (500) and for duplicates. |
| `container.go` | Gives each cell a workspace: `/testbed` copied out of the record's own instance image, which is the tree the official evaluation applies a patch to. A belay cell also gets a container of that image for its tests. |
| `harness.go` | Scores every cell with `python -m swebench.harness.run_evaluation`. **Its verdict is the score.** The port in `swebench.go` re-grades the same test statuses as a cross-check; a disagreement never changes the score and is counted, loudly, in the report. |
| `corpus.go`, `cell.go` | Runs both arms over the corpus, one cell at a time, resumably, under a total spend cap. |
| `corpusreport.go` | Writes the only output that may be published: aggregates, nothing of any instance. |
| `cmd/swebench` | The command. It is the one place a paid agent is constructed, and it is not in this package. |

A **cell** is one task, in one arm, at one replicate. The full corpus is 500
tasks in two arms: 1,000 cells.

### What the belay arm is, and is not

The belay arm is belay's real graph with two adapters in it, and every
report names them:

- **Its test step is the harness's, not belay's.** belay's shipped pytest
  runner runs a whole suite, which is the wrong tool for django's and
  sympy's own runners and hours of work on the rest. The harness runs only
  the test files the run changed, with the repository's own test command,
  inside the instance's own container. It never runs the dataset's tests:
  those are the answer key, and the control arm gets no such help.
- **Its quality gate is shipped belay**: the ruff adapter, judging only the
  lines the run changed. Most SWE-bench repositories configure no ruff, and
  belay as shipped refuses to start in such a repository; the harness runs
  ruff anyway, with its default rules, which is what adding an empty
  `[tool.ruff]` to `pyproject.toml` does. The report counts how many tasks
  belay as shipped would have refused (`shipped_gate_refused`).

### Resuming is the point

A full run takes days, and it will be interrupted. Run the same command
again and it continues: a cell that finished is never run, paid for or
scored again, and a belay cell cut off mid-way resumes from belay's own
journal, re-running only the step that was cut off. A control call cut off
mid-flight is made again from a fresh workspace (a call cannot be resumed);
one that had returned is kept, and never made again. Something the harness
failed at -- Docker went away, the API key ran out of credit -- stops the
corpus with nothing recorded for that cell, so mending it and re-running
loses nothing.

## Runbook: running SWE-bench Verified

This is for whoever runs it, and assumes you have not read the code.

### What you need

- **An x86_64 machine.** The official instance images are built for
  `linux/amd64`; the live command refuses any other host. Linux is best;
  Docker Desktop on an Intel Mac works.
- **Docker, running, with room.** At least 120GB free disk and 16GB of RAM,
  per SWE-bench's own Docker guide. The command removes each instance image
  once its task is scored (`-keep-images` to keep them).
- **Python 3.10 or newer with the official harness, at exactly 5.0.2**, which
  is what the scoring was written and tested against:

  ```
  python3 -m venv ~/swebench-venv
  ~/swebench-venv/bin/pip install swebench==5.0.2
  ```

- **ruff**, the belay arm's lint gate: `pipx install ruff`.
- **git**, **Go 1.26**, and this repository checked out.
- **Claude Code**, the backend both arms drive:
  `npm install -g @anthropic-ai/claude-code`.
- **A dedicated API key** in `ANTHROPIC_API_KEY`, so the spend is billed per
  call, reported per call, and nobody else's.
- **A belay configuration for the run**, for example
  `belay-swebench.yaml`:

  ```yaml
  version: 1
  agent:
    model: claude-sonnet-5
  budget:
    max_usd: 3
  ```

  `agent.model` is the model both arms run on. `budget.max_usd` is each
  cell's ceiling: belay enforces it on its own runs, and the corpus reserves
  it for every cell, belay or control, before starting it. Only this file is
  read; no other belay configuration is merged in.

The command checks all of this before downloading a byte or spending a cent,
and says what to install when something is missing.

### Step 1: the free check (costs nothing)

```
go run -tags fixrate ./test/fixrate/cmd/swebench \
  -mode replay -pilot \
  -config belay-swebench.yaml -python ~/swebench-venv/bin/python \
  -out ~/belay-swebench/check
```

This runs the whole pipeline on the 50 pilot tasks with a stand-in that
applies each task's own known-good patch and spends nothing. **Every cell
should resolve.** One that does not is a fault in the pipeline -- an image
that will not start, a test command that does not run in it, a harness that
cannot grade -- and finding it now is the point. The belay arm's runs end
"failed" here, which is expected: the stand-in writes no regression test,
so belay's own test step never passes, but the patch it leaves is the
known-good one and the official harness resolves it.

If the belay arm stops with "the tests could not run in container ...
(exit 127)", the instance images set up their environment differently from
SWE-bench's older ones; pass a working `-prologue` (the default is
`source /opt/miniconda3/bin/activate && conda activate testbed && cd /testbed`).

### Step 2: the pilot (about $300)

```
go run -tags fixrate ./test/fixrate/cmd/swebench \
  -mode live -pilot \
  -config belay-swebench.yaml -python ~/swebench-venv/bin/python \
  -max-usd 350 \
  -confirm "I understand this spends real money" \
  -out ~/belay-swebench/pilot
```

50 tasks, chosen by repository so every repository is represented (seed 1;
`-seed` and `-pilot-size` change that), each run **twice in each arm**: 200
cells. The second run of each arm is the A/A replicate, the run-to-run noise
any difference between the arms must be read against. It runs on the pilot
only.

### Step 3: the full run (about $1,500)

```
go run -tags fixrate ./test/fixrate/cmd/swebench \
  -mode live \
  -config belay-swebench.yaml -python ~/swebench-venv/bin/python \
  -max-usd 1500 \
  -confirm "I understand this spends real money" \
  -out ~/belay-swebench/full
```

All 500 tasks, each arm once: 1,000 cells, one at a time, which takes
days.

### What it costs, and what stops it

The dollar figures are estimates: the $1,500 the full run was budgeted at,
which is $1.50 a cell, and the pilot's 200 cells at the same rate. They are
not measurements. **`-max-usd` is what enforces a limit**: before each cell
the corpus adds what it has spent to the most the cell may spend
(`budget.max_usd`) and starts the cell only if that fits under the cap. It
can go over only by what one cell spends past its own ceiling. An attempt
cut off mid-call is charged its whole ceiling, because nothing reported
what it spent. When the cap stops the run, raise `-max-usd` and run the same
command again.

### Stopping and resuming

Ctrl-C at any time, or lose the machine. Run **the same command** again,
with the same `-out`, to continue. The command refuses to resume a corpus
with different settings (another model, other tasks, another
configuration), because that would quote two measurements as one.

Exit status: 0 the corpus is complete; 3 it stopped part-way and running it
again resumes it (interrupted, at the cap, or the infrastructure failed --
the message says which); 2 it refused before spending anything; 1 anything
else.

### What the output means

In the `-out` directory:

- **`report.txt` and `report.json` are the result, and the only files that
  may be published.** They hold aggregates only, because the dataset's
  licence is unknown and nothing derived from an instance may be
  published; a test enforces it.
- Everything else is private: `cells/` (each cell's patch, its belay
  journal, the control call's reply), `evaluation/` (the official harness's
  logs), `dataset/`.

`report.txt` reads top to bottom:

1. **What produced the numbers.** The mode (a `replay` report measures this
   harness, never an agent), the corpus and its pin, how the tasks were
   chosen, how they were scored, and the belay arm's adapters.
2. **Each arm's resolve rate**, with a 95% Wilson interval, its total spend,
   and **its cost per resolved task**, which is the number to compare:
   belay calls the agent several times where the control arm calls it once,
   so it is expected to cost more in total.
3. **The paired comparison.** Both arms ran the same tasks, so each task is
   a pair: `belay_only` tasks are what orchestration added, `control_only`
   what it lost, and the exact McNemar p-value says how surprising that
   split would be if the arms were equally good. No disagreements is an
   absence of evidence, not proof the arms are the same.
4. **A/A** (pilot only): each arm against its own second run. If an arm
   disagrees with itself about as often as the two arms disagree with each
   other, the delta is noise.
5. **Evaluation counts** per arm and replicate, and **warnings**. Read these
   before quoting anything. Above all: a nonzero `disagreement` count means
   the official harness and this repository's port of its grading
   disagreed on those cells. The official verdict is the score, but each
   disagreement is in its cell's `verdict.json` (`port.disagrees`) and must
   be understood first. `not graded` cells are ones the harness could not
   grade (a patch that did not apply, a timeout); they count as unresolved.

### What a number from this can claim

In live mode: the share of the chosen SWE-bench Verified tasks whose own
tests went from failing to passing without breaking the ones that passed
before, as SWE-bench's official harness judged it, for each arm; and the
difference between belay's orchestration and the same backend called once,
for **this** backend, **this** model and **this** configuration. It cannot
claim that a fix is right beyond what the tests check, comparability with a
leaderboard figure (the goal text differs from any submission's prompt, and
the belay arm's test step is the harness's), or anything about another
model or another corpus. ADR-0018 records the decisions behind it.

## What "driving belay" means here

`Run` constructs and runs a real, unmodified `internal/graph.Dispatcher`
over `internal/nodes.Default`'s registry — `plan -> approve -> code ->
write -> test -> fix -> review`, exactly the graph `belay run` uses — with:

- `Agent`: the caller-supplied `belay.AgentBackend` (the only swappable
  part — see above);
- `Runner`: `internal/runner.NewGo`, real `go test`, no network;
- `Linter`: `internal/linter.NewGolangCI`, real `golangci-lint`, no
  network;
- `Reviewer`/`Isolator`: unused (`review.mode` is `lint`; fanout is
  disabled) — belay's own shipped defaults, deliberately, since measuring
  belay "as it ships" is the point.

Nothing in this package reimplements belay's fix loop, its budget guard, or
its crash-recovery journal. The only thing this package writes to disk on
belay's behalf is the goal string every run receives (`runbelay.go`) —
everything else, including every retry decision, is the real dispatcher.

## The catalogued-defect contract: a subprocess, never an import

`fixtures/seeded-bug` is its own Go module
(`belay.dev/fixtures/seededbug`), deliberately outside this module's
dependency graph. This package is forbidden from adding a module
dependency or replace directive to reach it (out of T42's scope), and
would not want to even if it could — `injected.json` is that fixture's
public, documented contract for exactly this purpose. So this package
talks to it the way any external tool would:

```
go run ./inject.go --bugs=B01,B05,B15 --out=<dir>
go run ./inject.go --random=5 --seed=42 --out=<dir>
```

as a child process (`inject.go` in this package, not to be confused with
the fixture's own file of the same name), followed by reading and parsing
`injected.json` into this package's own mirrored schema (`catalogue.go`).
A schema drift between the two shows up as a JSON decode error the first
time a real `injected.json` is parsed — loud, not silent.

`Run` refuses to measure a fix rate against a copy that did not behave
exactly as its selected defects are catalogued to (`InjectedVerify.OK`,
i.e. `injected.json`'s own `missing_failing_tests` /
`unexpected_failing_tests` / `lint_unmatched_defects` are all empty) — see
`fixtures/seeded-bug/README.md`'s notes on composing a panic-causing defect
with others, and on the three `FlakyDetection` concurrency defects, for the
two ways a defect combination can legitimately fail this check through no
fault of this harness.

## The demonstration agent (`agent.go`)

Because this package never imports the fixture's `bugs` package, it cannot
read a `Defect`'s exact `Find`/`Replace` text (that only exists inside the
foreign module) to build a "reverse the byte range" fake. It works at the
file level instead, via `ScriptedPlan` / `Strategy`:

- **`StrategyRepair`**: overwrites the defect's target file with the
  byte-identical, known-good version from `fixtures/seeded-bug/src` — the
  strongest possible stand-in for "an agent correctly diagnosed and fixed
  this," with no foreknowledge of the exact edit needed.
- **`StrategyCheat`**: simulates exactly the failure mode the cheat
  detector exists to catch — instead of touching the source at all, it
  empties the corresponding `*_test.go` file down to a bare package
  clause, deleting every test it contained. The source bug is left exactly
  as injected.
- **`StrategyNoop`**: leaves the file untouched — what an agent that could
  not find or would not attempt the fix looks like.

`NewScriptedAgent` returns a `*belaytest.FakeAgent` (belay's own
test-support fake) with `Func` set to a handler that edits files on disk as
its only side effect, exactly how belay's real code/fix nodes expect an
agent to behave (`internal/nodes/code`'s "the agent edits the workspace
directly" design), and always reports zero `belay.Usage`. It distinguishes
belay's read-only planning call from its editing calls purely by
`AgentRequest.AllowedTools` (the plan node's tool set has no `Edit`; the
code and fix nodes' do, or omit `AllowedTools` entirely) — never by
matching against any unexported prompt text belonging to
`internal/nodes`.

## The cheat detector — the single most important correctness property

A "fix" that makes a catalogued test go green by deleting or weakening
that test, rather than by changing the code the test exercises, must never
be counted as a repair. `evaluate.go` implements the rule:

> A defect is credited as **repaired** only when POSITIVE evidence of a
> real fix exists — its own source file has different bytes after the run
> than right after injection — alongside the catalogued signal itself (a
> test turning green, a lint finding going quiet). A signal going quiet
> **without** the source file changing is **`invalid_repair`**, not
> `repaired`, regardless of how green the test suite looks.

Concretely, per defect, this package tracks (via `hash.go`'s sha256
snapshots, taken right after injection and again once belay's run
finishes):

- whether the defect's own target file's bytes changed (`source_changed`);
- whether **any** `*_test.go` file anywhere in the workspace changed
  (`test_tree_changed` — deliberately tree-wide, not narrowed to a guessed
  "owning" test file; see `evaluate.go`'s doc comment for why a coarser,
  more conservative signal is the correct trade here).

A catalogued test that flips to pass, or **vanishes outright** (the literal
effect of deleting it — reported separately in `DefectOutcome.Vanished`),
while the source file is untouched is `invalid_repair`. The four outcome
statuses (`Status` in `report.go`) are `repaired`, `not_repaired`,
`invalid_repair`, and `unverified` (an inconclusive case — most plausibly
one of the catalogue's `FlakyDetection` concurrency defects, or
`golangci-lint` being unavailable for the lint channel).

See `evaluate_test.go` for the classifier exercised in isolation (no
filesystem, no dispatcher — every branch of the decision tree above, in
milliseconds) and `TestRun_EndToEnd_ReplayMode_MixedOutcomes` for the same
logic exercised through the real dispatcher end to end, including the
literal "delete the failing test" cheat.

## Output

A `PairedResult` (`paired.go`, its own `schema_version`) is the two-arm
report described above. Everything below is about one arm's `Result`.

`Result` (`report.go`) is the machine-readable JSON
(`Result.MarshalIndentJSON`) and `Result.HumanSummary()` the human one.
Both report, in order: the mandatory mode/provenance disclosure and,
since schema version 2, the arm's own (`arm`, `arm_provenance`), then run-wide
totals and fix rate, a **per-class breakdown** (`ClassBreakdown`) — because
"belay repairs 90% of off-by-ones but 20% of concurrency bugs" is the
useful claim a single aggregate hides — and then every individual
`DefectOutcome`, including its detectability channel (`detected_by`:
`test`, `lint`, or `both`) and, for a non-repaired defect, exactly why.

## Determinism

Same seed, same defect set, same result: `TestRun_Determinism_SameSeedTwice`
runs `Selection{Random: N, Seed: S}` through `Run` twice and asserts
byte-identical JSON (modulo nothing — `RunOptions.Now` is pinned so even
`generated_at` matches exactly). Determinism here rests on three layers,
each already independently guaranteed below it:

1. `fixtures/seeded-bug/inject.go`'s own `--random` selection is a seeded
   permutation (`math/rand.New(rand.NewSource(seed))`, never the global
   source) — see that package's own `TestRandomDefects` and
   `TestRandomSelection_CLIEndToEndDeterminism`.
2. The scripted agent's file-level strategies are pure functions of the
   defect set (no time, no randomness).
3. belay's own dispatcher is deterministic given a deterministic clock and
   a deterministic agent (which this package pins via `RunOptions.Now` and
   provides via the scripted agent).

## A subtlety this harness had to work around: golangci-lint's cache

`fixtures/seeded-bug/inject.go` writes the **same hardcoded module path**
(`belay.dev/fixtures/seededbug/injected`) into every injected copy's
`go.mod`, regardless of how many separate temporary directories `Run`
creates across however many calls. Empirically, `golangci-lint`'s own
result cache can leak a stale absolute file path from an **earlier** call's
already-deleted temp directory into a **later** call's lint output — belay's
own review node then fails its gate forever on an issue attributed to a
file that no longer exists, which neither a real agent nor this package's
scripted one can ever resolve, and the run only stops when
`graph.max_steps` aborts it. That is a false negative for a defect set that
was, in fact, correctly repaired.

`golangcicache.go` fixes this the only way available without editing
`internal/linter` (out of scope): every `Run` call points
`GOLANGCI_LINT_CACHE` at a fresh directory under its own temp work
directory before belay's review node (or this package's own post-run lint
check) ever invokes `golangci-lint`, and restores the previous value
before returning. Because this is a process-wide environment variable,
concurrent `Run` calls in one process serialize around it
(`golangciCacheMu`) — a deliberate trade, documented at the mutex's
definition, given `Run` is already a multi-second-to-multi-minute
operation.

## Acceptance

Run these from the repository root (`export PATH=".../bin:$PATH"` for Go
and golangci-lint first, per this repo's own setup notes):

```
go build -tags fixrate ./test/fixrate/...
go vet   -tags fixrate ./test/fixrate/...
golangci-lint run --build-tags=fixrate ./test/fixrate/...
go test  -tags fixrate ./test/fixrate/... -race
```

For the end-to-end demonstration's JSON and human summary specifically:

```
go test -tags fixrate ./test/fixrate/... \
  -run TestRun_EndToEnd_ReplayMode_MixedOutcomes -v
```

or, to save both to files instead of scrolling through `-v` output:

```
FIXRATE_ACCEPTANCE_OUT=/tmp/fixrate-acceptance \
  go test -tags fixrate ./test/fixrate/... -run TestRun_EndToEnd_ReplayMode_MixedOutcomes -v
cat /tmp/fixrate-acceptance/result.json
cat /tmp/fixrate-acceptance/summary.txt
```

For the SWE-bench corpus runner end to end in scripted mode -- both arms,
belay's real graph, scored through the official harness's exec path with a
stand-in for Python -- and its resume guarantees:

```
go test -tags fixrate ./test/fixrate/... -run 'TestRunCorpus' -v
```

The tests that need Docker, or the real swebench (`FIXRATE_PYTHON` naming a
Python with `swebench==5.0.2`), skip without them and say how to install
them.

For the two arms paired over one task, with the paired report in the log:

```
go test -tags fixrate ./test/fixrate/... \
  -run TestRunControl_EndToEnd_PairsWithTheBelayArm -v
```

The single-arm run's demonstration defect set (`demoSelection`/`demoPlan` in
`run_test.go`) is five defects across five different bug classes and every
detectability channel this small a set can exercise: `B01` (off-by-one),
`B05` (inverted-condition) and `B15` (error-swallowed) are genuinely
repaired; `B16` (error-swallowed, both channels) is left broken
(`StrategyNoop`); `B17` (early-return-before-required-work) is "fixed" by
deleting its failing test (`StrategyCheat`) and must be — and is — reported
as `invalid_repair`, not `repaired`.

Zero token spend is guaranteed as described above: the scripted agent
reports zero `belay.Usage` on every call, and `Run` and `RunControl`, through
the pipeline they share, independently verify the sum is exactly zero
before either will return a `ModeReplay` result at all.
