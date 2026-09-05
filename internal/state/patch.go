package state

import (
	"errors"
	"fmt"
)

// ErrInvalidPatch reports a Patch that cannot be applied as given.
var ErrInvalidPatch = errors.New("state: invalid patch")

// Patch is a typed, composable mutation to a State blackboard, returned by
// a graph node's Result (see ADR-0002: nodes are pure and never touch
// state.json themselves) and applied only by the dispatcher, normally
// through Store.ApplyPatch.
//
// Every field is a pointer, or a pointer to a slice: nil means "leave this
// part of State unchanged." Every field except History sets an absolute
// value rather than a delta — Fix's Attempts is the new total attempt
// count, not "add one" — and that is what makes Apply idempotent. A crash
// can happen after the dispatcher applies a Patch but before it durably
// records having done so; resuming may then apply the very same Patch a
// second time. Because every field but History is an absolute set,
// applying it twice lands on exactly the same State as applying it once.
// History needs its own mechanism because "append" is not naturally
// idempotent — see the History field and Patch.Apply.
type Patch struct {
	// Goal replaces State.Goal.
	Goal *string
	// Plan replaces State.Plan wholesale.
	Plan *Plan
	// Code replaces State.Code wholesale.
	Code *Code
	// Test replaces State.Test wholesale.
	Test *Test
	// Review replaces State.Review wholesale.
	Review *Review
	// Fix replaces State.Fix wholesale — including Attempts, which a
	// caller must therefore compute as the new total, never an increment.
	Fix *Fix
	// Candidates replaces State.Candidates wholesale.
	Candidates *[]Candidate
	// Winner replaces State.Winner.
	Winner *string
	// History appends one HistoryEntry to State.History, or — if an entry
	// with the same Seq is already present — replaces it in place. Seq is
	// the dedup key that makes a repeated Apply of the same Patch
	// idempotent for History specifically; every other field is already
	// idempotent by virtue of being an absolute set.
	History *HistoryEntry
}

// Apply mutates s to reflect p.
//
// Its only error case is a History entry with a zero Seq: Seq is History's
// idempotency key, and zero cannot serve as one — it is indistinguishable
// from "no entry recorded yet," so a second, unrelated zero-Seq entry
// would silently overwrite the first instead of appending alongside it.
func (p Patch) Apply(s *State) error {
	if p.History != nil && p.History.Seq == 0 {
		return fmt.Errorf("%w: history entry has seq 0, which cannot be deduplicated", ErrInvalidPatch)
	}

	if p.Goal != nil {
		s.Goal = *p.Goal
	}
	if p.Plan != nil {
		s.Plan = *p.Plan
	}
	if p.Code != nil {
		s.Code = *p.Code
	}
	if p.Test != nil {
		s.Test = *p.Test
	}
	if p.Review != nil {
		s.Review = *p.Review
	}
	if p.Fix != nil {
		s.Fix = *p.Fix
	}
	if p.Candidates != nil {
		// Copied rather than aliased, so a caller mutating its own slice
		// after building the Patch cannot reach back into State.
		s.Candidates = cloneSlice(*p.Candidates)
	}
	if p.Winner != nil {
		s.Winner = *p.Winner
	}
	if p.History != nil {
		upsertHistory(&s.History, *p.History)
	}
	return nil
}

// upsertHistory appends entry to *history, or, if an entry with the same
// Seq is already present, replaces it in place. Keying on Seq is what
// makes repeated Apply calls carrying the same HistoryEntry idempotent:
// the second call finds its own previous effect and overwrites it with an
// identical value instead of appending a duplicate.
func upsertHistory(history *[]HistoryEntry, entry HistoryEntry) {
	for i := range *history {
		if (*history)[i].Seq == entry.Seq {
			(*history)[i] = entry
			return
		}
	}
	*history = append(*history, entry)
}
