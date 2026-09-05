# internal/runner test fixtures

Every file here is a captured, then hand-curated, sample of a real toolchain's
output. Nothing in this package's tests executes a toolchain: the fixtures are
replayed through the `Execer` seam, and `TestMain` empties `PATH` for the whole
test binary so an accidental real invocation cannot resolve a binary at all.

## Provenance

The samples were produced once, during development, by running the tool against
a throwaway project outside this repository, and then edited by hand for
stability and review:

| file | tool | curation applied |
| --- | --- | --- |
| `go-pass.jsonl` | `go test -json -count=1 ./...`, go1.26.3 | timestamps replaced with a fixed sequence |
| `go-fail.jsonl` | same, one failing test, one failing subtest, one passing panic-recovery test | timestamps replaced |
| `go-build-error.jsonl` | same, against a package that does not compile | timestamps replaced |
| `go-nested-fail.jsonl` | same, module with a failing test in `internal/sub` | timestamps replaced |
| `go-noisy-fail.jsonl` | `go-fail.jsonl` with non-JSON toolchain chatter interleaved | hand-written, see below |
| `jest-pass.json` / `jest-fail.json` | `jest --json --testLocationInResults`, jest 29 | pretty-printed and key-sorted; absolute paths rewritten to `/repo` |
| `vitest-pass.json` / `vitest-fail.json` | `vitest run --reporter=json --outputFile=/dev/stdout`, vitest 5.0.0 | absolute paths rewritten to `/repo`, otherwise byte-for-byte |
| `nodetest-tap-fail.txt` | `node --test --test-reporter=tap`, node v26.7.0, one file | absolute paths rewritten to `/repo` |
| `nodetest-tap-multifile-fail.txt` | same across two files | absolute paths rewritten to `/repo` |
| `pytest-pass.txt` / `pytest-fail.txt` | `pytest --tb=short -q -rfE`, pytest 9.1.1 | absolute paths rewritten to `/repo` |
| `pytest-collect-error.txt` | same, against a module that does not import | paths rewritten to `/repo` and `/usr` |
| `pytest-no-tests.txt` | same, against an empty directory | none |

## Two fixtures worth reading closely

`vitest-*.json` are kept unformatted on purpose. Since vitest 5 the json
reporter appends `JSON report written to /dev/stdout` **after** the document,
so the stream is not a bare JSON file. That trailing notice is the reason
`firstJSONObject` exists, and reformatting these fixtures would delete the
evidence.

`go-noisy-fail.jsonl` is the only hand-written fixture. `go test` interleaves
plain text on stdout in situations that are awkward to reproduce on demand
(module downloads, toolchain switches, `go: ...` notes), so the noise is
injected by hand into a real event stream. It exists to pin one behaviour: a
line that is not a JSON object is skipped, and the surrounding test events are
still counted.
