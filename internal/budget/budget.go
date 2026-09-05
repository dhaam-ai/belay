// Package budget implements belay's cost ledger: the guard standing
// between a looping fix node, or an N-way fanout, and an unbounded bill.
//
// # Why this exists
//
// Fanout multiplies cost by however many candidates run in parallel, and a
// dispatcher retrying a failing node can call an AgentBackend far more
// times than a human would ever approve by hand. Without an enforced
// ceiling, both "best-of-N" and "keep retrying until it passes" are ways
// to spend money with no upper bound. docs/adr/0007 makes the fuller
// argument for why this shipped on day one rather than as a "going
// further" extra; this package is its implementation.
//
// # Ledger
//
// A *Ledger accumulates belay.Usage — input/output tokens, USD, and
// whether any contribution was estimated — per node and in total, across
// however many AgentBackend.Invoke calls a run makes. Record adds one
// call's usage; Total and Breakdown read the cumulative picture back.
//
// # Enforcement
//
// Check and CanAfford are the two points a dispatcher calls into this
// package. Check runs before every node, to stop — or, under
// config.OnExceedWarn, merely log — a run that has already crossed its
// ceiling. CanAfford runs before a fanout launches its N candidates, to
// refuse the whole batch up front when its projected cost would not fit
// in what remains, rather than discovering that partway through actually
// spending it.
//
// # Money
//
// USD is accumulated internally as integer micro-dollars, never as
// float64, specifically so that many nodes' worth of additions cannot
// drift the way repeated float64 += does. See the "Float discipline"
// section of ledger.go's doc comment for the precise bound this gives.
//
// # Estimation
//
// belay.Usage.Estimated is sticky at both the per-node and the run-total
// level: once any single contribution was estimated (see price.go's
// Estimate), that node's and the run's Estimated flag reads true forever
// after, even once every later contribution is exact. A user must never
// see a confident total that was quietly built in part from a guess.
//
// # Resumption
//
// internal/state owns manifest.json; this package never imports it. A
// dispatcher persists a Ledger's Snapshot into the manifest's budget block
// after every node and, on resume, calls Restore instead of New — see
// Snapshot's and Restore's doc comments for why skipping this turns a hard
// budget cap into a suggestion that resets on every crash.
package budget

import (
	"log/slog"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/pkg/belay"
)

// Check reports whether l's cumulative total has crossed cfg's ceiling,
// honouring cfg.OnExceed. A dispatcher calls Check before every graph
// node, so a run stops (or logs) as soon as it crosses its ceiling rather
// than only after the node that finally pushes it over has already run to
// completion.
//
// MaxUSD and MaxTokens are independent ceilings; either, both, or neither
// may be active. A value of zero or less disables that dimension's cap —
// config.Config.Validate already requires MaxUSD > 0 unless OnExceed is
// config.OnExceedWarn, but Check treats zero as "uncapped" unconditionally
// so it never has to trust that validation ran first.
//
// Crossing either ceiling under config.OnExceedAbort — or under any
// OnExceed value this package does not recognize, which fails safe as
// abort rather than silently permissive — returns a *belay.BudgetError
// populated with the run's current spend and cfg.MaxUSD. Because
// *belay.BudgetError.Unwrap returns belay.ErrBudgetExceeded, both
// errors.Is(err, belay.ErrBudgetExceeded) and errors.As(err, &budgetErr)
// succeed on the result. A token-ceiling crossing is reported the same
// way: belay.BudgetError has no token fields by the landed pkg/belay
// contract, so SpentUSD/LimitUSD carry the current USD figures regardless
// of which ceiling actually triggered — the slog line on the warn path
// below is where the token figures are visible when that matters.
//
// Crossing either ceiling under config.OnExceedWarn logs the overrun via
// slog.Warn and returns nil: the run continues over budget by design,
// because a user who chose "warn" wants visibility, not a stop.
func (l *Ledger) Check(cfg config.Budget) error {
	l.mu.Lock()
	spentMicros := l.total.micros
	tokens := l.total.inputTokens + l.total.outputTokens
	l.mu.Unlock()

	limitMicros := usdToMicros(cfg.MaxUSD)
	overUSD := limitMicros > 0 && spentMicros > limitMicros
	overTokens := cfg.MaxTokens > 0 && tokens > cfg.MaxTokens
	if !overUSD && !overTokens {
		return nil
	}

	spentUSD := microsToUSD(spentMicros)
	if cfg.OnExceed == config.OnExceedWarn {
		slog.Warn("belay/budget: budget ceiling crossed, continuing (on_exceed=warn)",
			"over_usd", overUSD,
			"over_tokens", overTokens,
			"spent_usd", spentUSD,
			"max_usd", cfg.MaxUSD,
			"tokens", tokens,
			"max_tokens", cfg.MaxTokens,
		)
		return nil
	}

	return &belay.BudgetError{SpentUSD: spentUSD, LimitUSD: cfg.MaxUSD}
}

// CanAfford reports whether launching n more candidates, each projected to
// cost approximately projected, would fit within cfg's remaining budget —
// without spending anything. A fanout node calls this before starting any
// candidate, so a run that cannot afford N parallel attempts fails loudly
// up front instead of discovering it partway through, after 1..N-1
// candidates have already spent real money.
//
// Comparisons are done in the same integer micro-dollars a Ledger
// accumulates internally (see ledger.go's "Float discipline" section), so
// a projection that lands exactly on the remaining budget is affordable
// and one that lands even a single cent over it is not — free of the
// float64 boundary error a naive dollars-and-cents comparison could
// introduce right at that edge.
//
// n <= 0 always succeeds: there is nothing to afford. A zero or negative
// cfg.MaxUSD or cfg.MaxTokens disables that dimension's cap, exactly as in
// Check.
func (l *Ledger) CanAfford(n int, projected belay.Usage, cfg config.Budget) error {
	if n <= 0 {
		return nil
	}

	l.mu.Lock()
	spentMicros := l.total.micros
	tokens := l.total.inputTokens + l.total.outputTokens
	l.mu.Unlock()

	projMicros := usdToMicros(projected.USD) * int64(n)
	projTokens := (projected.InputTokens + projected.OutputTokens) * int64(n)

	limitMicros := usdToMicros(cfg.MaxUSD)
	if limitMicros > 0 && spentMicros+projMicros > limitMicros {
		return &belay.BudgetError{
			SpentUSD: microsToUSD(spentMicros + projMicros),
			LimitUSD: cfg.MaxUSD,
		}
	}
	if cfg.MaxTokens > 0 && tokens+projTokens > cfg.MaxTokens {
		return &belay.BudgetError{
			SpentUSD: microsToUSD(spentMicros),
			LimitUSD: cfg.MaxUSD,
		}
	}
	return nil
}
