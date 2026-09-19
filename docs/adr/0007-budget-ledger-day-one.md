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

---

## Amendment (2026-09-19): every step is charged, and a resume counts from the journal

The ceiling this decision promises did not hold across a crash or a failed
step. Three holes let a run spend past `budget.max_usd`:

- **A rejected step was free.** When a step returned an error, ran past
  `graph.node_timeout`, returned a result the dispatcher could not use, or was
  stopped with Ctrl-C, the dispatcher journalled it with zero usage and never
  added anything to the ledger. The Claude backend answers an `is_error` run
  with its usage and an error together, so real, reported spend was thrown
  away. Once Ctrl-C paused a run instead of ending it, a resumed run could
  spend its whole ceiling a second time.
- **A resume restored the ledger from a snapshot that could be stale.** The
  dispatcher journals a step's completion, usage included, and only then saves
  the manifest's budget block. A crash between the two left a manifest one
  step behind, and the ledger was restored from it.
- **A step belay never saw finish was free.** After a kill -9, the dispatcher
  closes the attempt the crash left open, and charged it nothing.

### Decision

1. **Spend is not a verdict.** A step's `Result.Usage` is charged and
   journalled however the dispatcher disposes of the rest of the Result. A
   node whose backend reported usage carries it on its error return too. Only
   the Patch of a rejected Result is thrown away.
2. **The journal is the ledger's source of truth.** On every dispatch the
   ledger is rebuilt from the usage on every `node_finished` record, the same
   record everything else a resume decides is read from. The manifest's
   snapshot is kept as a floor the total never drops below, so rebuilding can
   only ever find more spend than before, never less.
3. **An attempt a crash left open is charged the costliest step the run has
   recorded**, in each dimension (tokens in, tokens out, dollars), and the
   charge is marked estimated. The flag is sticky, so every total that
   includes the charge says it is partly a guess. When the run has recorded
   no spend yet, the charge is zero.

The third point is a choice about which way to be wrong, because the real
cost is unknowable: the process that would have reported it is dead, and an
agent may have spent any amount before the kill. Charging nothing is wrong in
the direction that breaks this ADR's promise. Charging too much only stops a
run early. So the charge leans high, and it comes from the run's own evidence
of what one of its steps costs, not from a second source of truth.

### Consequences

- A run killed during a paid step, then resumed, is charged the estimate for
  the attempt that died and then the real cost of the attempt that runs it
  again. Its total reads "(estimated)" from then on.
- A run can still pass its ceiling in one case: killed during its first paid
  step, before anything has been recorded, when there is no evidence to charge
  from. The overshoot is at most what that one step cost.
- A kill -9 during a step that spends nothing, such as the test step, is
  charged as if it were the costliest step. That is the conservative choice
  working as intended, and the price of it is a run that stops early.

### Alternatives considered

- **Charge the whole remaining budget.** Safe, but it stops every run that
  crashed, including one killed while its tests were running, and makes a
  resume after any crash need a raised ceiling.
- **Estimate from the prompt size and a price table.** The output tokens and
  the number of turns are exactly what is unknown, and a price table in the
  dispatcher is a second source of truth for what a backend costs, which the
  ledger's design already rejects.
- **Charge the most this same node has cost before.** Nothing at all for the
  commonest case, a crash during the first `code` or `fix` attempt.

### Revisit if

- A backend reports usage as it goes, so that the dispatcher could journal
  partial spend before a crash instead of estimating it afterwards.
- Estimated charges stop runs early often enough that people raise their
  ceilings by reflex.
