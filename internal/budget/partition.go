package budget

import (
	"errors"
	"fmt"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// ErrNoCeiling reports a budget with no ceiling to divide. A fanout whose
// candidates each got an uncapped envelope would be N uncapped runs, which
// is why budget.max_usd above zero is a condition of fanning out at all.
var ErrNoCeiling = errors.New("budget: no budget.max_usd to divide")

// ErrNoCandidates reports a partition into no shares, which is a caller's
// mistake rather than anything about the money.
var ErrNoCandidates = errors.New("budget: no candidates to divide a budget among")

// Partition divides what a run may still spend into n equal shares: what
// each of n fanout candidates may spend, in dollars and in tokens.
//
// The division is done in whole micro-dollars, the unit this package
// accumulates money in, and the remainder of fewer than n micro-dollars is
// left unallocated. That is what makes the shares sum to no more than the
// run had left, by construction rather than by a float64 division that
// lands a fraction of a cent the wrong side of the ceiling. Tokens divide
// the same way, from what is left of the input and output counts together,
// which is the total Ledger.Check compares against config.Budget.MaxTokens.
//
// It returns plain numbers rather than a state.Envelope so that
// internal/state and this package still need not import each other — the
// same documented agreement state.Budget and Snapshot already keep.
//
// Three things are refused rather than approximated. No ceiling is
// ErrNoCeiling and no candidates is ErrNoCandidates, both mistakes to fix
// before anything runs. A ceiling with nothing left to divide — including
// one where the share would round down to zero, which Ledger.Check reads as
// uncapped and which would therefore be the opposite of a small envelope —
// is a *belay.BudgetError, so a fanout's refusal carries the same error a
// ledger produces and the dispatcher can turn it into a pause a person
// resumes after raising budget.max_usd.
func Partition(spent Snapshot, cfg config.Budget, n int) (float64, int64, error) {
	if n <= 0 {
		return 0, 0, fmt.Errorf("%w: %d", ErrNoCandidates, n)
	}
	limitMicros := usdToMicros(cfg.MaxUSD)
	if limitMicros <= 0 {
		return 0, 0, fmt.Errorf("%w: max_usd is %v", ErrNoCeiling, cfg.MaxUSD)
	}

	exhausted := &belay.BudgetError{SpentUSD: spent.SpentUSD, LimitUSD: cfg.MaxUSD}
	shareMicros := (limitMicros - usdToMicros(spent.SpentUSD)) / int64(n)
	if shareMicros <= 0 {
		return 0, 0, exhausted
	}

	var shareTokens int64
	if cfg.MaxTokens > 0 {
		shareTokens = (cfg.MaxTokens - spent.TokensIn - spent.TokensOut) / int64(n)
		if shareTokens <= 0 {
			return 0, 0, exhausted
		}
	}
	return microsToUSD(shareMicros), shareTokens, nil
}

// PartitionAtLeast returns the envelope a candidate that was claimed earlier
// continues with: the larger of what its manifest already promised it and
// the share Partition would hand out now.
//
// An envelope is never lowered. Part of it may already be spent, so taking
// it back would not return any money; and a candidate that aborted on its
// own envelope has to be continuable by raising budget.max_usd and resuming,
// which is the settled rule for every limit belay enforces. The cost, which
// ADR-0019 states, is that lowering max_usd mid-fanout does not claw back
// what was already reserved.
//
// It is a pure function of the persisted value, the live ceiling, the
// run's spend before the fanout and n — never of what any sibling actually
// spent or of the order goroutines ran in — so two resumes of the same run
// with the same configuration produce the same envelope, and the fanout a
// crash interrupted converges rather than drifting. Because the share for a
// given ceiling never exceeds the headroom divided by n, raising to it keeps
// the sum of the envelopes inside the ceiling as well.
//
// A ceiling that has nothing left to divide, or none at all, leaves the
// envelope exactly as it was: that is not a refusal here, because the
// candidate has already been promised what it holds.
//
// Zero means uncapped in both dimensions, as everywhere else in this
// package, so it is treated as the largest value rather than the smallest —
// a candidate claimed while max_tokens was unset is not retroactively capped
// by one set afterwards.
func PartitionAtLeast(persistedUSD float64, persistedTokens int64, spent Snapshot, cfg config.Budget, n int) (float64, int64) {
	shareUSD, shareTokens, err := Partition(spent, cfg, n)
	if err != nil {
		return persistedUSD, persistedTokens
	}
	usd := microsToUSD(raise(usdToMicros(persistedUSD), usdToMicros(shareUSD)))
	return usd, raise(persistedTokens, shareTokens)
}

// raise returns the higher of two ceilings, reading zero or less as no
// ceiling at all and therefore as the higher of the two.
func raise(persisted, share int64) int64 {
	if persisted <= 0 || share <= 0 {
		return 0
	}
	return max(persisted, share)
}
