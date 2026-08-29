# ADR-0007: Budget Ledger and Cost Caps on Day One, Not a "Going Further" Extra

## Status
Accepted

## Context
Fanout multiplies cost by N candidates. An AI review phase can exceed $10 per run. A user who misconfigures belay or runs it in a tight loop could silently spend hundreds of dollars before noticing.

Early drafts treated cost tracking as a future nice-to-have, not part of v0.1. The team reconsidered: a tool that can spend unbounded money without warning is not ready for daily use. Budget controls were moved to day-one requirements.

## Decision
A budget ledger tracks cumulative spend (USD and token counts) per run and per day (if the config specifies it). When spend reaches a configured cap, belay aborts with a clear error. The cap is mandatory (no silent override). Cost is parsed from `claude -p --output-format json` (fields: total_cost_usd, usage.input_tokens, usage.output_tokens). If those fields disappear in a future claude version, a fallback uses a static price table marked Estimated:true.

## Consequences

### Positive
- **Prevents surprise bills**: Users can set max_usd:5 and know a misconfiguration won't cost $500. Abort is loud and clear.
- **Fanout is safe**: With budget caps, fanout_candidates can be high without risk. The cap automatically gates cost.
- **Transparency**: Every phase logs its cost. Users understand where money is going (usually review phases).
- **Graceful degradation**: If claude's JSON output format changes, the estimator kicks in, maintaining safety even if accuracy drops.

### Negative
- **Adds operational complexity**: Users must understand budget fields and set reasonable caps. Too low and runs abort unexpectedly. Too high and they defeat the purpose.
- **Cost accuracy depends on claude version**: If --output-format json is removed in a future claude release, estimated costs might be off by 10-20%. The ledger must track both actual and estimated, and maintainers must monitor for drift.
- **Fanout cost is multiplicative**: With N candidates, cost is N× higher. A user might set max_usd:50 thinking 5 candidates × $10 each = $50, but not account for retry loops or approval phases. The cap prevents disasters, but users must still plan.
- **Requires environment variable for API key**: ANTHROPIC_API_KEY must be set for cost parsing to work (if parsing fails, no fallback is available; run aborts with "cannot determine cost").

### Follow-ups
- Implement BudgetLedger in `pkg/budget/ledger.go` with methods: RecordPhase(phase string, cost float64), Check() error.
- Document belay.yaml budget fields in docs/config.md (max_usd, max_tokens, on_exceed: abort|warn).
- Parse cost from `claude -p` output in `pkg/adapter/claude/cost.go` with a price table fallback for when JSON output is unavailable.
- Add a CLI flag: `belay run --budget-dry-run` that executes and reports estimated cost without actually running (helps users calibrate caps).
- Track estimated vs. actual cost in logs; expose a metric (future: Prometheus) for cost accuracy drift.

## Alternatives Considered
- **No budget controls in v0.1**: Cost tracking as a future enhancement. Not chosen because: (1) fanout is too powerful without caps, (2) users would silently run high-cost operations, (3) would require a breaking change to add later.

## Revisit If
- Users report that estimated costs are frequently >10% off from actual costs (re-calibrate the price table).
- A significant use case emerges where budgets are too restrictive (e.g., large fanout runs on monorepos always need > $100).
- claude-p JSON output is deprecated and no replacement is announced (would need to query billing API, if available).

## References
- Challenge #134 phase 1: "Initialize Budget"
- Claude API docs: https://docs.anthropic.com/claude/reference/usage
- pkg/budget/ledger.go (T46)
- pkg/adapter/claude/cost.go (T46)
- docs/config.md (budget fields)
