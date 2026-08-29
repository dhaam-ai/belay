# ADR-0009: Record/Replay Cassettes for Zero-Cost CI

## Status
Accepted

## Context
CI runs must be affordable. Belay's tests exercise the full graph: plan, code, test, review, etc. Each phase calls Claude Code, costing ~$0.50 per run. With 10 tests, that's $5 per CI run. Over a week: $50. Over a year: $2600 for a small team.

Cassettes (recorded HTTP request/response pairs) are a standard solution: record real Claude API calls once, replay them in CI. No API calls, zero cost, deterministic tests.

Downsides: cassettes can drift from real behavior (Claude's output changes, API behavior drifts). Without a periodic live-run check, tests pass in CI but fail in production.

## Decision
The test suite records cassettes of MCP and Claude API calls. CI replays cassettes at zero cost. Maintainers run a live smoke test before every release tag; if the smoke test fails, the tag is blocked until cassettes are re-recorded.

## Consequences

### Positive
- **Zero-cost CI**: No API credits spent. External contributors can fork and run the suite without paying.
- **Deterministic tests**: Cassettes are fixed; tests pass or fail the same way every time. No flaky "network was slow" failures.
- **CI is fast**: Replaying from disk is orders of magnitude faster than calling Claude.
- **Auditable**: Cassettes are checked in to git; diff shows what API calls changed between versions.

### Negative
- **Cassettes drift from real behavior**: If Claude's response format changes, or if a bug is fixed on the server, cassettes silently hide it. Tests pass in CI but fail in production.
- **Manual smoke test gate required**: Maintainers must run a live test before every release. This is a process requirement, not automatic. If skipped once, cassettes could be stale.
- **Cassette update process is manual**: When Claude changes behavior, cassettes must be re-recorded. This requires credits and manual review to ensure the new cassettes are correct (not corrupted by a transient API issue).
- **Hard to test new backends**: When Codex or Gemini support lands, cassettes must be recorded for those too. Adds friction to onboarding new adapters.

### Follow-ups
- Implement cassette recording/replay in `pkg/cassette/` using go-vcr or similar. Record on first run (or via `belay test --record`), replay in CI.
- Store cassettes in `testdata/cassettes/` with clear naming: `phase_<phase>_backend_<backend>.yaml`.
- Add a `belay test --live` flag that forces live API calls (for smoke testing). Update CI to run this before release tags.
- Document in CONTRIBUTING.md: "To record a new cassette: run `belay test --record` locally (requires ANTHROPIC_API_KEY and $1 test credits). Review the cassette diff before committing."
- Add a pre-release checklist: "Before tagging, run `belay test --live` and confirm all phases pass. If any fail, re-record cassettes with the latest Claude output."

## Alternatives Considered
- **No cassettes; pay for every CI run**: Simpler implementation, but unsustainable at scale. Every fork, every PR would cost credits.
- **Snapshot testing (compare code output without cassettes)**: Would require mocking Claude responses inline in test code, which is harder to maintain and doesn't catch API drift.

## Revisit If
- A user reports a production failure that cassettes should have caught (indicates drift is too high; increase smoke-test frequency).
- Cassettes become too large to check in to git (> 100 MB; may need external storage like S3 or a separate cassette repository).
- Maintainers consistently forget to run smoke tests before tags (automate via release CI, or use a GitHub Action that blocks merges without a smoke-test signature).

## References
- Challenge #134 test requirements: conformance suite
- go-vcr library: https://github.com/dnaeon/go-vcr
- VCR (Ruby original): https://github.com/vcr/vcr
- pkg/cassette/ (T46)
- testdata/cassettes/ (T46: cassette storage)
- CONTRIBUTING.md: cassette recording process (T46)
