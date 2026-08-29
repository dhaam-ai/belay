# fixtures/seeded-bug

This is the instrument behind belay's result-driven pillar: the fixture
that produces a real, defensible "fix rate: N/26 seeded defects" number.

It is a separate module (`belay.dev/fixtures/seededbug`), excluded from
the root build (`go list ./...` from the repo root lists no package under
here). It has two parts:

- **`src/`** — a small, fully-tested, correct Go library (package
  `backoff`).
- **`bugs/` + `inject.go`** — a catalogue of 26 hand-verified defects and
  a CLI that stamps a chosen subset of them onto a fresh copy of `src/`.

`fixtures/wc/` (owned by a different task) is belay's other fixture: an
honest build-from-spec demo. This one is different on purpose — it is a
*working* codebase into which known, catalogued defects are injected, so
a harness (T42) can measure exactly how many of them an agent repairs.

## Why `backoff`

The brief for `src/` asked for something with real logic density — a
codebase that admits a *variety* of realistic bug classes, not just
typos — and named candidates including an LRU cache, an expression
evaluator, a semver comparator, an interval merger, and a
retry-with-backoff calculator.

`backoff` (a retry/backoff policy calculator) was chosen because it is
the one candidate that naturally hosts all twelve target bug classes
without contrivance, and because it is thematically honest for this repo
— belay's own dispatcher retries agent calls and tracks a budget ledger
(ADR-0007), so a retry-policy calculator is exactly the kind of code
belay itself contains:

- **Off-by-one / boundary**: attempt counting, loop bounds on doubling.
- **Inverted condition**: retryable-error checks, validation range
  checks (`&&` vs `||`).
- **Wrong operator**: exponential-growth arithmetic, jitter scaling,
  sentinel comparisons.
- **Swapped arguments**: a same-typed `Clamp(d, lo, hi)` call site.
- **Nil/empty handling**: an empty-slice guard, an optional-nil-pointer
  guard.
- **Error swallowed**: a classification result silently dropped; an
  unchecked error return.
- **Early return before required work**: bookkeeping that must run
  unconditionally, reordered behind a guard.
- **Integer overflow**: naive exponential-backoff doubling is a
  textbook real-world overflow bug.
- **Incorrect default**: a documented zero-means-unlimited sentinel
  field, exactly the kind of convention real backoff libraries have
  gotten wrong before.
- **Concurrency**: a shared attempt ledger with a real `sync.Mutex` to
  drop, plus a classic mutex-by-value (`govet` copylocks) mistake.
- **Resource leak**: a per-iteration timer/ticker whose `Stop()` calls
  get "helpfully" moved to `defer` inside a loop.
- **Wrong constant**: default threshold and unit mistakes
  (`time.Millisecond` vs `time.Second` is one of the most common
  real-world constant bugs there is).

No I/O, no network, no filesystem access anywhere in `src/` — every
package it imports is standard-library `context`, `errors`, `fmt`,
`math`, `math/rand`, `sync`, or `time`.

## Layout

```
fixtures/seeded-bug/
├── go.mod              module belay.dev/fixtures/seededbug, stdlib-only
├── README.md            this file
├── .gitignore
├── inject.go             the injector CLI (package main, single file —
│                         `go run ./inject.go` only compiles the file(s)
│                         named on its command line, so every byte of the
│                         injector lives in this one file by design)
├── inject_test.go        the injector's own test suite, including the
│                         per-defect self-verification harness
├── src/                  the correct backoff library
│   ├── policy.go          Policy, Validate, NextDelay, ShouldRetry,
│   │                     DefaultPolicy + its constants
│   ├── errors.go          ErrTransient, Retryable, IsRetryable, Classify
│   ├── delay.go           Clamp, ExponentialDelay, MinDelay
│   ├── ledger.go          Ledger, ClassifyAndCount, WatchTotal,
│   │                     WaitForEach
│   ├── orchestrate.go     Run (ties Policy + Ledger + a sleep func into
│   │                     a retry loop)
│   └── *_test.go          table-driven tests for all of the above
└── bugs/                 the defect catalogue
    ├── catalogue.go        the Defect type, the self-registering
    │                     registry, and Defect.Validate()/ValidateAll()
    ├── catalogue_test.go   tests for the catalogue machinery itself
    └── b01_*.go … b26_*.go one file per defect, each registering exactly
                          one Defect via init()
```

## Quality baseline

```
cd fixtures/seeded-bug/src
go test ./... -race            # 100% green
go test ./... -cover           # 99.2% of statements
golangci-lint run ./...        # 0 issues
```

99.2% is Go's standard **statement** coverage metric (`go test -cover`);
Go's toolchain has no separate branch-coverage mode, so this is the most
precise number the stdlib toolchain can report. The two uncovered
branches are both genuinely unreachable in practice, not untested logic:
`wrapAttempt`'s nil-guard (get a direct unit test instead —
`TestWrapAttempt/nil_error_stays_nil` — since `Run` never actually calls
it with a nil error) and `Run`'s `NextDelay` error branch (`NextDelay`
only ever errors when `attempt < 1`, which `Run`'s own loop, starting at
1 and only incrementing, can never produce).

`golangci-lint run ./...` reports zero issues on a clean `src/` using
this repo's own root `.golangci.yml` (errcheck, govet, staticcheck,
ineffassign, unused, gosec, revive, bodyclose, errorlint, gocritic,
misspell) — so any lint finding on an injected copy is attributable to a
defect, never to pre-existing noise. `inject.go` and `bugs/` are held to
the same bar (also 0 issues); it just is not one of the acceptance
checks.

## The defect catalogue

26 defects (the floor was 20), spanning 12 bug classes and both
detectability channels — 22 are caught only by the target package's own
tests, 2 only by a linter, and 2 by both:

| ID | Class | File | Symbol | Transformation | Detected by |
|----|-------|------|--------|-----------------|-------------|
| B01 | off-by-one | delay.go | `ExponentialDelay` | loop bound `i < attempt` → `i <= attempt` (one extra doubling) | test |
| B02 | off-by-one | policy.go | `Policy.ShouldRetry` | `attempt >= p.MaxAttempts` → `attempt > p.MaxAttempts` | test |
| B03 | off-by-one | policy.go | `Policy.Validate` | `p.MaxDelay < p.BaseDelay` → `<=` (rejects the valid equal case) | test |
| B04 | inverted-condition | policy.go | `Policy.ShouldRetry` | `if !IsRetryable(err)` → drops the `!` | test |
| B05 | inverted-condition | policy.go | `Policy.Validate` | Jitter range check `\|\|` → `&&` (never rejects anything) | test |
| B06 | inverted-condition | errors.go | `Classify` | `if predicate(err)` → `if !predicate(err)` | test |
| B07 | wrong-operator | delay.go | `ExponentialDelay` | `delay <<= 1` → `delay += 1` (exponential → linear) | test |
| B08 | wrong-operator | policy.go | `Policy.NextDelay` | `spread := capped * p.Jitter` → `capped + p.Jitter` | test |
| B09 | wrong-operator | policy.go | `Policy.NextDelay` | `capped - spread + …` → `capped + spread + …` (can exceed MaxDelay) | test |
| B10 | wrong-operator | ledger.go | `Ledger.Record` | `if op == ""` → `if op != ""` | test |
| B11 | wrong-operator | errors.go | `IsRetryable` | `errors.Is(err, ErrTransient)` → `err == ErrTransient` | both (errorlint) |
| B12 | swapped-arguments | policy.go | `Policy.NextDelay` | `Clamp(raw, base, max)` → `Clamp(base, raw, max)` | test |
| B13 | nil-empty-handling | delay.go | `MinDelay` | removes the `len(delays) == 0` guard (panics on empty input) | test |
| B14 | nil-empty-handling | orchestrate.go | `Run` | removes the `if ledger != nil` guard (panics with a nil ledger) | test |
| B15 | error-swallowed | errors.go | `Classify` | final `return err` → `return nil` | test |
| B16 | error-swallowed | orchestrate.go | `Run` | drops the `sleep` error check entirely (also unchecked-error) | both (errcheck) |
| B17 | early-return-before-required-work | ledger.go | `ClassifyAndCount` | moves the nil-error early return ahead of `ledger.Record` | test |
| B18 | integer-overflow-truncation | delay.go | `ExponentialDelay` | removes the pre-shift overflow guard | test |
| B19 | incorrect-default | policy.go | `Policy.ShouldRetry` | drops the `MaxElapsed != 0` sentinel check | test |
| B20 | concurrency | ledger.go | `Ledger.Record` | drops `mu.Lock`/`Unlock` (data race, `-race`-only) | test (flaky-by-nature, see below) |
| B21 | concurrency | ledger.go | `Ledger.Total` | drops `mu.Lock`/`Unlock` on the read path | test (flaky-by-nature) |
| B22 | concurrency | ledger.go | `Ledger.Reset` | drops `mu.Lock`/`Unlock` | test (flaky-by-nature) |
| B23 | concurrency | ledger.go | `Ledger.Count` | pointer receiver → value receiver (copies the embedded mutex) | lint (govet/copylocks) |
| B24 | resource-leak | ledger.go | `WaitForEach` | per-iteration `timer.Stop()`/`ticker.Stop()` → `defer` (pins resources for every earlier name until the whole call returns) | lint (gocritic/deferInLoop) |
| B25 | wrong-constant | policy.go | `DefaultMaxAttempts` | `5` → `50` (wrong threshold) | test |
| B26 | wrong-constant | policy.go | `DefaultBaseDelay` | `100 * time.Millisecond` → `100 * time.Second` (wrong unit) | test |

Full per-defect detail — including every test each one is catalogued to
break — lives in `bugs/bNN_*.go`; `bugs.All()` / `bugs.ByID` /
`bugs.Classes()` give programmatic access, and `Defect.Validate()` /
`bugs.ValidateAll()` check a defect's own internal consistency (unique
ID, non-empty fields, `Find != Replace`, and `DetectedBy` matching which
of `BreaksTests`/`Linter` are set).

### On B20–B22 and "flaky by nature"

`-race` only reports a data race when it actually observes the two
conflicting accesses' interleaving — that is inherent to how the race
detector works, not a defect in these three catalogue entries. In
isolation, B20's own dedicated stress test caught it in well over a
dozen back-to-back manual runs; but the moment it runs as part of the
*whole* package's `go test ./...` (which is what real verification always
does), heap-address reuse across sequential tests can carry a real race
in `Record` into `TestLedger_ConcurrentReset`/`TestLedger_ConcurrentTotal`
too, and that is probabilistic rather than guaranteed on any single run.
`bugs.Defect.FlakyDetection` marks exactly these three; `inject_test.go`
verifies them by retrying (up to 5 attempts) rather than treating one
clean-looking run as proof the catalogue entry is wrong. A harness
consuming this fixture for a fix-rate number should either run the
target tests more than once for these three IDs, or accept that they are
inherently probabilistic signal — which is itself an accurate reflection
of what data races are actually like.

### On composing a panic-causing defect with others

B12, B13, and B14 each fail by panicking (an out-of-range index, a nil
pointer, or `Clamp`'s own `lo > hi` precondition). An unrecovered panic
in a Go test terminates the whole test *binary*, not just the one
subtest — so if one of these three is injected together with other
defects whose tests are declared in files that run later (`go test`
runs top-level `Test` functions in file-then-declaration order), the
panic can prevent those later tests from running at all. That is not
those other defects "not behaving as catalogued" — each one is
independently, individually verified (see below) — it is a consequence
of Go's test runner, and it is why `injected.json`'s
`missing_failing_tests` and `unexpected_failing_tests` are computed from
tests that actually reported a result, never by assuming an absent test
must have passed.

## The injector

```
go run ./inject.go --bugs=B03,B07 --out=/tmp/seeded-01
go run ./inject.go --random=5 --seed=42 --out=/tmp/seeded-02
```

Flags: `--bugs` (comma-separated IDs) and `--random` (a count) are
mutually exclusive selection modes; `--random` requires `--seed` (there
is no silent default — an unseeded random selection would not be
reproducible, so it is refused outright). `--out` is required. `--src`
defaults to `src` (relative to the working directory, i.e. this
directory) and can be overridden, mainly so tests can point it at a
throwaway copy.

What one run does, in order:

1. **Selects** the defect set (explicit IDs, or a seeded random
   permutation of the whole catalogue).
2. **Refuses** if `--out` is `--src` itself, nested inside it, or an
   ancestor of it (which clearing `--out` would otherwise delete `src/`
   through) — `checkSafeDestination`, covered by
   `TestCheckSafeDestination` and, end-to-end through the real CLI
   entry point, `TestInjectorRefusesToWriteIntoSrc`.
3. **Copies** `--src` to a clean `--out` (`os.RemoveAll` then a fresh
   walk-and-copy), and writes a standalone `go.mod` plus a
   `.golangci.yml` into it — see below — so the copy builds, tests, and
   lints on its own regardless of where `--out` points, even somewhere
   entirely outside this repo.
4. **Locates** every defect's `Find` text in its target file, requiring
   it to appear *exactly once* (a stale or ambiguous catalogue entry is
   a hard error, not a silent no-op), then **checks every pair of edits
   in the same file for an overlapping byte range** and refuses to
   compose two that overlap.
5. **Applies** every edit in one left-to-right pass per file, so earlier
   edits never invalidate later ones' byte offsets.
6. **Verifies its own work**: `go build ./...` in the copy (a defect
   that fails to compile is a bad defect, full stop); `go test -race
   -json ./...`, diffed against the union of the injected defects'
   `BreaksTests`; and, if `golangci-lint` is on `PATH`,
   `golangci-lint run ./...` in the copy, checked against every
   lint-detected defect's `Linter`.
7. **Writes `injected.json`** — always, even when step 6 finds a
   mismatch, so the evidence for *why* is on disk — and prints a summary
   to stdout. It exits non-zero exactly when step 6 found a mismatch.

### Determinism

`--random=N --seed=S` always selects the same N defect IDs for a given
catalogue: the full 26-entry list is sorted by ID first, then
`math/rand` seeded from `S` (via `rand.New(rand.NewSource(S))`, never
the global source) permutes it and takes the first N, so neither the
`bugs/` package's file-load order nor anything outside the seed can
change the result. `TestRandomDefects/same_seed_and_n_selects_the_same_set_across_100_calls`
checks 100 in-process calls to the selection function directly;
`TestRandomSelection_CLIEndToEndDeterminism` additionally shells out to
the real `go run ./inject.go --random=5 --seed=42` CLI twice and
compares `injected.json`'s selections, to prove the determinism holds
through flag parsing and process boundaries too, not just inside the
Go function.

### Composability

`TestComposability` injects four defects that touch four different files
(B01, B15, B19, B23 — including one lint-only entry) in a single run and
checks the result matches the union of what each is individually
catalogued to do, with nothing extra. The general safety net is
`planEdits`'s overlap check, exercised directly and in isolation (with
synthetic, non-catalogue `Defect` values so the test doesn't depend on
any two real defects happening to collide) by `TestPlanEdits`.

## Verifying every defect individually

`TestEveryDefect_IndividuallyVerified` is requirement #4's automation:
for every one of the 26 catalogued defects, in its own subtest, it

1. injects that one defect alone into a fresh temp directory through the
   real `run()` entry point (i.e. exactly what the CLI does),
2. asserts the copy still compiles,
3. asserts every test named in that defect's `BreaksTests` actually
   failed (falling back to a bounded retry only for the three
   `FlakyDetection` entries), and
4. asserts **no other test failed** — the leaf-level failure set (after
   collapsing `go test`'s parent/child result aggregation, so a failing
   subtest's container isn't double-counted) must equal exactly the
   catalogued set, and any lint-detected defect's linter must have
   actually fired.

Run it from this directory:

```
go test . -run TestEveryDefect_IndividuallyVerified -v
```

The summary subtest logs a table and a final `N/26 defects individually
verified exactly as catalogued` line. Current state: **26/26**. Every
`BreaksTests` list in `bugs/` was set from this harness's own observed
output, not hand-guessed — several defects turned out to have a wider
(or narrower) blast radius than a first read of the diff would suggest,
which is exactly why this self-check exists.

This test (and `TestComposability`, `TestRandomSelection_CLIEndToEndDeterminism`)
builds and race-tests a fresh copy per case, so plan on roughly 3 minutes
for the full run; everything else in the package runs in under a second.
All of it is `go test`, `go build`, and (opportunistically)
`golangci-lint` — no Docker, no network, and no other external
toolchain, matching this wave's constraints.

## `injected.json` schema

Written into `--out` on every run, success or failure. This is real,
unedited output from `go run ./inject.go --bugs=B03,B07 --out=<dir>`:

```json
{
  "generated_at": "2026-08-29T13:12:28Z",
  "src": "/absolute/path/to/fixtures/seeded-bug/src",
  "out": "/absolute/path/to/out",
  "selection": {
    "mode": "bugs",
    "bugs": ["B03", "B07"]
  },
  "defects": [
    {
      "id": "B03",
      "class": "off-by-one",
      "file": "policy.go",
      "symbol": "Policy.Validate",
      "description": "Boundary comparison off-by-one: <= in place of < wrongly rejects the valid edge case where MaxDelay equals BaseDelay.",
      "line": 76,
      "detected_by": "test",
      "expected_failing_tests": [
        "TestPolicy_Validate/max_delay_equal_to_base_delay_is_valid"
      ]
    },
    {
      "id": "B07",
      "class": "wrong-operator",
      "file": "delay.go",
      "symbol": "ExponentialDelay",
      "description": "Wrong operator: left-shift-by-one (double) replaced with add-one, turning exponential growth into linear growth.",
      "line": 39,
      "detected_by": "test",
      "expected_failing_tests": [
        "TestExponentialDelay/doubles_each_attempt",
        "TestExponentialDelay/caps_at_max"
      ]
    }
  ],
  "verification": {
    "build_ok": true,
    "expected_failing_tests": [
      "TestExponentialDelay/caps_at_max",
      "TestExponentialDelay/doubles_each_attempt",
      "TestPolicy_Validate/max_delay_equal_to_base_delay_is_valid"
    ],
    "actual_failing_tests": [
      "TestExponentialDelay/caps_at_max",
      "TestExponentialDelay/doubles_each_attempt",
      "TestPolicy_Validate/max_delay_equal_to_base_delay_is_valid"
    ],
    "missing_failing_tests": [],
    "unexpected_failing_tests": [],
    "lint_available": true
  }
}
```

(`selection.random`/`selection.seed`, every `defects[].linter`, and the
three `verification.lint_*` fields are all `omitempty`; a `--bugs` run
like this one omits the first two, a defect with no `Linter` set omits
the third, and this run had no lint-detected defects selected so the
last three are absent too — not `null`, not `0`, just not there.)

Field notes:

- **`selection.mode`** is `"bugs"` or `"random"`; the fields that don't
  apply to the mode used are omitted (`encoding/json`'s `omitempty`), not
  zero-valued-and-present — `random`/`seed` are absent for `--bugs` runs
  and vice versa.
- **`defects[].line`** is the 1-indexed line of the *start* of the
  edit, computed from the injected copy, not `src/` (harmless either way
  since the copy is byte-identical to `src/` before edits are applied).
- **`defects[].expected_failing_tests`** is exactly `bugs.Defect.BreaksTests`
  — empty for a lint-only defect.
- **`verification.actual_failing_tests`** is the full leaf-level failing
  set from the real `go test -race -json ./...` run against the copy —
  a T42-style harness computing its own fix-rate number after handing
  the copy to an agent should re-run this same command and compare
  against this same field, not re-derive it from scratch.
- **`missing_failing_tests`** (catalogued to fail, but didn't — a
  catalogue bug if this is ever non-empty for a single-defect injection)
  and **`unexpected_failing_tests`** (failed, but no defect claims it —
  collateral damage, or two panic-adjacent defects interacting per the
  section above) are what `inject.go`'s own exit code is driven by.
- **`lint_available`** is `false`, with every `lint_*` field omitted,
  when `golangci-lint` wasn't found on `PATH` — lint verification is
  best-effort, never a hard requirement, since `go build`/`go test` are
  the two checks every environment can run.

## What `.golangci.yml` gets written into `--out`

Every injected copy gets its own `.golangci.yml`, matching the linter
set this catalogue was authored and verified against (the same list as
this repo's root config, plus explicitly enabling gocritic's
`deferInLoop` check, which golangci-lint does not enable by default even
when `gocritic` itself is on). Without this, a copy placed outside this
repository entirely — which `--out` fully permits — would fall back to
`golangci-lint`'s much smaller built-in default set (errcheck, govet,
ineffassign, staticcheck, unused) and silently under-report B11, B16's
errcheck/errorlint angle, and both of B23/B24, since none of those are
in that default set.

## Reproducing the numbers in this README from scratch

```
cd fixtures/seeded-bug/src
go test ./... -race -cover -covermode=atomic     # 100% green, 99.2%
golangci-lint run ./...                          # 0 issues

cd ..
go test ./bugs/... -race                         # catalogue-machinery tests
go test . -run TestEveryDefect_IndividuallyVerified -v   # 26/26
go test . -run TestComposability -v
go test . -run TestRandomSelection_CLIEndToEndDeterminism -v
go test .                                        # everything else, < 1s
```
