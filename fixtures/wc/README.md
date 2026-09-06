# fixtures/wc — belay's demo fixture

This directory is a coding task for an autonomous agent, used two ways:

1. As the subject of belay's README demo — the task a reader watches
   belay solve, end to end, through the plan → code → test → fix → review
   graph.
2. As the source of the record/replay cassettes checked in under
   `testdata/cassettes/` elsewhere in the repo (see ADR-0009), so CI can
   run the whole graph against this task at zero API cost.

It is a real, honest task: implement a working clone of the Unix `wc`
utility in Go. `GOAL.md` is the exact prompt belay feeds the agent.
`wc_test.go` is the executable specification the agent's implementation
must satisfy. Everything else here supports those two files.

`fixtures/wc/go.mod` makes this its own Go module
(`belay.dev/fixtures/wc`), separate from the `github.com/dhaam-ai/belay`
root module. That is deliberate: `go build ./...` and `go test ./...` run
from the repo root skip nested modules automatically, so this fixture
(and the tokens spent solving it) never leaks into the root module's own
build or test loop.

## Running it standalone

```
cd fixtures/wc
go test ./...
```

No flags, no setup, no network, no Docker — just the Go toolchain.
`go vet ./...` is clean.

## Expected initial state

Right now, every test fails. That is intentional — see "Stub vs. no
implementation" below. What matters is *how* they fail: `go build .`
succeeds, and `go test ./...` runs all 35 cases and fails each one on an
assertion (wrong stdout, wrong exit code), never on a compile error. You
can confirm this yourself:

```
$ cd fixtures/wc && go test ./...
--- FAIL: TestRun/tier1_bytes_only/empty_file (0.00s)
    wc_test.go:270: run([-c testdata/empty.txt]) exit code = 1, want 0 (stderr: "wc: not implemented\n")
    ...
FAIL
FAIL	belay.dev/fixtures/wc	0.3s
```

This matters for belay's graph: a *test* failure drives the test → fix
loop the way it's meant to be exercised. A *build* failure would instead
exercise error-recovery in the code/write phase — a different, less
interesting path for a demo whose point is showing the quality-gated
loop converge.

## Difficulty ramp

`wc_test.go` groups its 35 cases into eight tiers, named directly in
each subtest (`go test -v` shows the ramp). Earlier tiers are reachable
with a much simpler implementation than later ones — the point is that
belay's test → fix loop should see steady, partial progress across
iterations rather than an all-or-nothing suite that gives it nothing to
converge on:

| Tier | What it needs | Why it's harder than the last |
|---|---|---|
| 1. `tier1_bytes_only` | `-c`: raw byte counting | Baseline — read input, report `len()`. No parsing at all. |
| 2. `tier2_lines_only` | `-l`: count `\n` bytes | One more byte comparison; includes the "no trailing newline → 0 lines" gotcha. |
| 3. `tier3_words_only` | `-w`: whitespace tokenizing | Needs real state (are we inside a word?), not just a counter. |
| 4. `tier4_default_no_flags` | Combined output, fixed column order/width, filename | First tier where exact formatting has to be right, and where flag order must NOT affect output order. |
| 5. `tier5_chars_multibyte` | `-m`: UTF-8-aware character counting | Requires decoding, not just counting bytes — the first genuinely different algorithm. |
| 6. `tier6_nul_byte_via_stdin` | NUL-safety, stdin | Byte 0 must not be mistaken for a string terminator or whitespace. |
| 7. `tier7_stdin_input` | Standard input as a first-class source | Input has no filename, so the "no filename" output branch must be exercised, combined with harder flags. |
| 8. `tier8_large_file_integration` | Everything, at ~300 KB | Integration: correctness doesn't get to assume tiny input. |

Every scenario required by this fixture's design brief — empty input, a
file with no trailing newline, a file with one, CRLF line endings,
runs of spaces and tabs, leading/trailing whitespace, a multi-byte UTF-8
file, a NUL byte, a large generated file, and stdin — appears in the
table above, most of them in more than one tier.

## Determinism

This fixture has to produce the same result on any machine, every time —
recorded cassettes (ADR-0009) replay this task's tool calls verbatim, so
if the environment behaved differently between the recording machine and
CI, the replay would be meaningless. Concretely:

- All committed `testdata/` files were generated with exact byte control
  (`printf`, not an editor) and every count in `wc_test.go` was verified
  independently with a Python one-liner before being hard-coded into the
  test table — the test's expectations are never derived from the same
  logic they're checking.
- The large (~300 KB) file is **not** committed. `wc_test.go` regenerates
  it byte-for-byte on every run (`largeFixturePattern` repeated
  `largeFixtureRepeat` times) to a fixed, `.gitignore`d path
  (`testdata/generated/`), and removes it afterward via `t.Cleanup`. A
  fixed path is used deliberately, instead of `t.TempDir()`, because a
  temp directory's path is different on every run — had it leaked into a
  test's expected-output string, two runs of this suite could produce
  different output even with an identical implementation, which is
  exactly what this fixture must never do.
- The NUL-byte case is **not** committed as a testdata file either, even
  though it's small. A raw NUL byte in a text-oriented git history is
  friction-prone (some tooling treats the file as binary, diffs render
  oddly), and Go string literals can express `\x00` directly in source.
  `wc_test.go` builds it as `[]byte("foo\x00bar baz\n")` and feeds it
  through stdin instead.
- No test depends on the wall clock, the local timezone, or OS locale
  settings. `-m` is defined here as "count of Unicode code points" —
  exactly what decoding UTF-8 gives you — never "locale-aware character
  count," which varies by platform.

Verified: running `go test -v ./...` twice and diffing the two outputs
produces exactly one difference — the package's own trailing elapsed-time
line that `go test` always prints (e.g. `0.32s` vs `0.29s`). Every
subtest name, ordering, pass/fail result, and failure message —
including the ones built from the freshly-regenerated large file — is
byte-identical between runs.

## Stub vs. no implementation

The task instructions for this fixture call out a choice: ship
`wc_test.go` alongside either no implementation file, or a stub that
compiles and fails every test. This fixture ships a stub (`main.go`): a
`run` function with the right signature that always returns a failure
without doing any real work.

The reasoning: with no implementation file at all, `wc_test.go`'s calls
to `run(...)` wouldn't resolve, and `go test ./...` would fail with a
compile error before a single test ran — the graph's code/write phase
would need to *create* a new symbol from scratch, and the first thing
belay would see is a build failure, not a test failure. With a stub,
`go build .` and `go vet ./...` are clean from the start, and `go test
./...` fails 35 assertions for the right reason. That puts belay's
test → fix loop in exactly the state it's designed for: a green build,
a red test suite, and a spec (`GOAL.md` plus the tests themselves) to
converge against.

The stub is intentionally *only* a stub — it doesn't attempt partial
credit. Its `run` ignores its arguments and always returns exit code 1
after writing "not implemented" to stderr, which is enough to make every
test case fail (every expected output is a non-empty, specifically
formatted string; the stub produces none of them). The implementation is
free to rewrite `main.go` entirely, add more files, and use any internal
structure it likes — the only thing that has to survive is a `run`
function with this exact signature, since `wc_test.go` calls it
directly and must not be modified.

## Cost warning

Running belay against this fixture calls a real coding-agent backend and
spends real tokens/API credits per run. Running `go test ./...` directly,
as shown above, costs nothing — do that first to confirm the fixture
itself is healthy before pointing belay at it.
