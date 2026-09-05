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

Two of the tests in this package (`TestRun_EndToEnd_ReplayMode_MixedOutcomes`,
`TestRun_Determinism_SameSeedTwice`) each drive belay's real dispatcher
graph end to end — injecting defects, running `go build`, `go test -race`
(more than once, through the fix loop) and `golangci-lint` — so budget
roughly 15-90 seconds for the package's test run, not milliseconds.
Everything else is well under a second.

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

`Result` (`report.go`) is the machine-readable JSON
(`Result.MarshalIndentJSON`) and `Result.HumanSummary()` the human one.
Both report, in order: the mandatory mode/provenance disclosure, run-wide
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

That run's demonstration defect set (`demoSelection`/`demoPlan` in
`run_test.go`) is five defects across five different bug classes and every
detectability channel this small a set can exercise: `B01` (off-by-one),
`B05` (inverted-condition) and `B15` (error-swallowed) are genuinely
repaired; `B16` (error-swallowed, both channels) is left broken
(`StrategyNoop`); `B17` (early-return-before-required-work) is "fixed" by
deleting its failing test (`StrategyCheat`) and must be — and is — reported
as `invalid_repair`, not `repaired`.

Zero token spend is guaranteed as described above: the scripted agent
reports zero `belay.Usage` on every call, and `Run` independently verifies
the run-wide sum is exactly zero before it will return a `ModeReplay`
result at all.
