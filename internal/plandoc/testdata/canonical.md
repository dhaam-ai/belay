## Summary

The CLI gains a `--dry-run` flag that prints the work it would do and exits 0.

## Context

`internal/cli/run.go` holds the run command's flags today; `startRun` is where a
run is created.

## Steps

1. Add a `dryRun` field to `runOptions` in `internal/cli/run.go`.
2. Bind `--dry-run` to it in `newRunCmd`, with help text a first-time reader can act on.
3. Return from `startRun` before `createRun` when the flag is set, after printing the resolved graph.

## Tests

`internal/cli/run_test.go` covers the flag set and unset, and pins the exit code at 0.

## Risks

The flag is additive and defaults to off, so an existing invocation cannot change behaviour.

## Out of scope

`belay resume` is not given a dry run.
