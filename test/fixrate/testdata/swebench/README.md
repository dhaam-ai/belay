# testdata/swebench

**Every record in this directory is synthetic. None of it is SWE-bench
dataset content.**

The repositories (`example/widget`, `example/gadget`), the instance IDs,
the problem statements, the patches and the test names were all written by
hand for this package's tests. They exist to exercise `swebench.go`'s
decoder and resolution rule offline, with no network and no dataset
download.

This is not a stylistic preference. The SWE-bench **code** is MIT licensed
(https://raw.githubusercontent.com/SWE-bench/SWE-bench/main/LICENSE), but
the **dataset** declares no licence at all: as of 2026-09-30 none of
`SWE-bench/SWE-bench_Verified`, `princeton-nlp/SWE-bench_Verified` or
`princeton-nlp/SWE-bench` carries a `license` field or a `license:*` tag in
its Hugging Face metadata. Until that is established, no dataset record may
be committed to this repository. See `swebench.go`'s "Licence" section.

## What each file is for

- `instances-array.json` — a JSON array, with `FAIL_TO_PASS` and
  `PASS_TO_PASS` as native JSON arrays. This is the encoding the pinned
  revision uses.
- `instances-jsonl.jsonl` — one record per line, with `FAIL_TO_PASS` and
  `PASS_TO_PASS` as JSON-encoded **strings**. This is the encoding the
  older `princeton-nlp/*` copies use, and the one their dataset card
  documents. Both must decode.
- `instance-no-fail-to-pass.json` — a record with an empty `FAIL_TO_PASS`,
  which this adapter refuses. SWE-bench's own `compute_fail_to_pass`
  returns 1 when the set is empty, so such an instance resolves trivially;
  importing it would inflate both arms' resolve rates identically and for
  no reason.
