package fanout

import (
	"fmt"

	"github.com/dhaam-ai/belay/internal/budget"
	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/state"
)

// Envelopes returns what each of n candidates may spend: the run's
// remaining ceiling divided n ways, as the reservation a candidate run's
// own manifest persists.
//
// # Reserve once, at the claim
//
// The split happens exactly once per run, before any candidate is claimed,
// and nothing afterwards re-divides it. That is what bounds a fanout under
// concurrency. A pool that handed each candidate the parent's ceiling would
// let N candidates each spend it, and one that re-read the remaining
// ceiling as candidates finished would reward whichever candidate burned
// fastest and make the winner depend on goroutine timing — so a resumed run
// could starve a different candidate than the crashed one did, and
// best-of-N would become best-of-whoever-spent-first.
//
// A finished candidate's unspent envelope is therefore not given back to
// its siblings. That is the price of a reservation that converges on
// re-run, and ADR-0019 records it rather than hiding it.
//
// # Refusing is the point
//
// Every refusal comes back as the error internal/budget produced, not one
// composed here: a ceiling with nothing left to divide is a
// *belay.BudgetError, which the dispatcher turns into a pause naming
// budget.max_usd, so a person raises the ceiling and resumes rather than
// starting the run again and paying for it twice. A share that rounds down
// to zero is refused for a reason worth stating: budget.Ledger.Check reads
// a zero ceiling as no ceiling at all, so handing out the smallest possible
// envelope would hand out the largest.
//
// # Overshoot, stated honestly
//
// Ledger.Check is post-hoc — it compares what a run has spent, not what it
// is about to — so one node can cross the ceiling by its own cost. With C
// the most expensive single node execution, a fanout of N is bounded by
// max_usd + N*C ordinarily, and by max_usd + 2*N*C in the worst case where
// every candidate is killed part-way through its first paid node and the
// resume charges that attempt an estimate. The envelope is what makes the
// bound proportional to C at all; without it the bound is N * max_usd.
//
// The arithmetic lives in internal/budget, where micro-dollars already do,
// and returns plain scalars, so neither internal/state nor internal/budget
// has to import the other. This function is the thin caller that names the
// result for the manifest that stores it.
func Envelopes(remaining budget.Snapshot, cfg config.Budget, n int) (state.Envelope, error) {
	usd, tokens, err := budget.Partition(remaining, cfg, n)
	if err != nil {
		return state.Envelope{}, fmt.Errorf("fanout: dividing what is left of the budget between %d candidates: %w", n, err)
	}
	return state.Envelope{LimitUSD: usd, LimitTokens: tokens}, nil
}
