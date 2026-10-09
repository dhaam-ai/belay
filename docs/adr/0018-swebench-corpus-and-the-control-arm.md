# ADR-0018: SWE-bench Verified, scored by its own harness, against a control arm

## Status
Accepted

## Context

belay's headline claim is a fix rate. Measured on belay's own seeded-bug fixture, it says something about belay's plumbing and nothing anyone else can check. The roadmap settled on SWE-bench Verified as the corpus worth publishing a number on, pinned at `SWE-bench/SWE-bench_Verified@78f471bf655a3137b2e8a75af1501690ec009ec3`, with `claude-sonnet-5` as the model, and on the shape of the claim: not "belay resolves N%", which cannot separate belay from the agent underneath it, but a **delta** against a control arm -- the same backend, the same goal, the same model and tools, called once with no graph. Slices 6a and 6b built the control arm, the paired report (resolve rate with a Wilson interval, cost per resolved task first, an exact McNemar test) and an offline adapter that ports SWE-bench's grading rule. This record covers slice 6c, which makes it run, and records what a number from it can and cannot claim.

Five facts shaped it.

**The official harness is a pipeline, not a rule.** SWE-bench 5.0.2 evaluates a prediction by starting the record's own instance image, applying the patch the way every submission's patch is applied, resetting the test files the dataset's test patch touches, applying that patch, running the repository's tests and parsing the log with the repository's own parser (`swebench/harness/run_evaluation.py`, `grading.py`). A port of the last step is not the same instrument.

**A run of this size will be interrupted.** 500 tasks in two arms is 1,000 cells at minutes of agent time each: days. belay's thesis is that interruption loses nothing; a benchmark of belay that could not resume would contradict it.

**belay as shipped cannot run on most of the corpus.** belay's lint gate needs a linter the repository configures, and most SWE-bench repositories configure no ruff; belay refuses to start there. Its pytest runner runs a whole suite, which is wrong for django's and sympy's own runners and too slow on the rest. And until this slice, ruff counted every finding in the repository, so a repository with old debt failed every run.

**The dataset's licence is unknown.** None of its Hugging Face repositories declares one. The harness code is MIT; the records are not covered by it.

**Claude does not run it.** No x86_64 host or dedicated key is available to the agents that built it; the owner triggers the live run.

## Decision

The SWE-bench corpus is scored by SWE-bench's official harness, run as `python -m swebench.harness.run_evaluation` on each cell's prediction, and **its per-instance verdict is the score**. The ported rule re-grades the same raw test statuses -- read from the harness's own log reader -- as a cross-check, and a disagreement is counted and reported, never averaged and never allowed to change the score.

The corpus runner works one cell at a time and is resumable by construction: a cell's state is three atomically written files (result, verdict, attempts), a finished cell is never run, paid for or scored again, a belay cell resumes from belay's own journal, and a control call's reply is kept the moment it returns. A cell the harness failed -- a container that died, a key out of credit -- is not recorded as the arm's outcome. A total spend cap is checked before every cell against what was spent plus the cell's own ceiling (`budget.max_usd`); an attempt cut off mid-call is charged that whole ceiling.

Both arms work in `/testbed` copied out of the record's own instance image. The belay arm is belay's real graph with one harness adapter, its test step (the test files the run changed, run with the repository's own command inside the instance's container, never the dataset's tests), and shipped belay's ruff gate, which this slice scopes to the lines a run changed, as golangci-lint already was. Every result names the adapters.

The dataset is downloaded only at the pin and kept only if it is byte for byte the pinned file. The published report holds aggregates only, enforced by a test. The live command refuses without its confirm token and a cap, and before anything is spent, refuses a host that is not x86_64 or lacks Docker, the pinned harness, the tools or a key. The pilot is about 50 tasks stratified by repository, with an A/A replicate of each arm; the full run is every task once.

## Consequences

### Positive
- A resolve rate from this harness is SWE-bench's own, from SWE-bench's own containers, and the cross-check shows whether this repository's understanding of the rule still matches it.
- An interruption costs at most the cell in flight, and for a belay cell only the step in flight. The tests prove it by interrupting a corpus mid-step and counting agent calls on resume, and fail if finished cells are not skipped or belay cells restart instead of resuming.
- The two product changes the numbers waited on are product changes: ruff judges only the run's change, and belay says why its lint gate has nothing to run instead of claiming not to recognise a Python project.
- The cost of one resolved task, in each arm, is first-class, next to a paired test that says how much the difference means, and an A/A run that says how much an arm disagrees with itself.

### Negative
- The belay arm is not pure shipped belay: its test step is the harness's. A belay user on a SWE-bench repository would have to configure `test.runner: custom` and a ruff table to get close. The report says so, and counts how many tasks shipped belay would have refused.
- The regression test the shared goal asks for is never scored: the official evaluation resets or ignores it, in both arms alike.
- One cell at a time makes the full run slow -- days.
- An interrupted control call is charged its whole ceiling though it may have spent less: the cap can stop a run early, never late. That charge is reported apart from the arms' own spend, which is what cost per resolved task uses.
- Nothing containerised has run where this was built: no Docker. The environment prologue is taken from SWE-bench's older eval scripts. The free gold-patch check exists to find a mistake there before money is spent.
- A number from a corpus is a property of that backend, that model, that configuration and that goal text. It is not a leaderboard figure: the goal differs from any submission's prompt.

### Follow-ups
- Apply the three-line `internal/cli` change that makes the lint blocker use `linter.Explain`; it was held back while other lanes were editing that package.
- Run cells concurrently, once the cap's reservation accounting is extended to cells in flight.
- Read the dataset's licence again before publishing anything beyond aggregates.
- Port `fail_only` grading if a later pin contains such instances.
- ESLint is still unscoped; scope it the way ruff now is.

## Alternatives Considered
- **Score with the port alone.** It needs no Docker. It is not the instrument anyone else's number comes from, and it would have hidden every way the port drifts. It is the cross-check instead.
- **Run belay inside the instance containers.** The most faithful environment, but it needs Node and Claude Code installed into every image, and belay's scripted mode could not exercise the same path. The agent runs on the host against the copied `/testbed`, and only the tests run in the container.
- **Use belay's shipped pytest runner.** It runs whole suites and cannot run django's or sympy's tests; the belay arm would have been measured on a broken tool.
- **Let the belay arm's test step run the dataset's FAIL_TO_PASS tests.** That is the answer key. The control arm has no such help, and the delta would measure the leak.
- **Restart an interrupted belay cell.** Simpler, and it would pay twice for every step that had finished, which is what belay exists to prevent.
- **A bare-native third arm.** Dropped with the second backend: both arms drive Claude Code, which is the comparison that isolates orchestration.

## Revisit If
- The cross-check reports disagreements on more than a handful of cells: the port has drifted, or the rule has changed, and the report should not be published until it is understood.
- The dataset gains a licence that permits redistribution, which would allow per-instance results.
- A pilot's A/A disagreement is as large as the arms' disagreement: the corpus is too small, or the run too noisy, for the delta to mean anything.
- SWE-bench moves the internals the cross-check calls, or changes where reports are written; the harness asks for the latter at run time, but 5.0.2 is the version pinned.

## References
- `test/fixrate/README.md`, "SWE-bench, for real" and the runbook.
- `test/fixrate/harness.go`, `corpus.go`, `cell.go`, `container.go`, `dataset.go`, `corpusreport.go`, `cmd/swebench`.
- ADR-0007 (budget ledger; the charge for an attempt a crash left open), ADR-0008 (local linters as the default gate), ADR-0009 (scripted mode vs cassette replay).
- SWE-bench: https://github.com/SWE-bench/SWE-bench (5.0.2; grading.py@ca6e4e0d252f32f8762625b73575d5dee49d0a5a; eval scripts at v3.0.17, 3f01bd622c0a22c00406139f69a234ef08225f22).
- Dataset: https://huggingface.co/datasets/SWE-bench/SWE-bench_Verified at 78f471bf655a3137b2e8a75af1501690ec009ec3.
- Jimenez et al., "SWE-bench: Can Language Models Resolve Real-world GitHub Issues?", ICLR 2024.
