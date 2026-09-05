package join

import (
	"errors"
	"fmt"
	"strings"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/state"
)

// Sentinel errors returned by Select. Callers detect them with errors.Is.
//
// Every one of them reports a wiring bug rather than a verdict about the
// run's code — see the package doc's "Errors versus results". "No candidate
// passed" is deliberately not among them: that is a real outcome, and Select
// reports it as an Outcome with Passed == 0.
var (
	// ErrNoCandidates reports that Select was asked to choose a winner
	// from an empty candidate list. Reaching join at all means the graph
	// routed here, and the only node that routes to join is fanout, which
	// records every candidate it creates; so an empty list means either
	// fanout never ran (join was wired into a graph without it) or its
	// Patch was lost. Neither is something a selection rule can paper
	// over.
	ErrNoCandidates = errors.New("join: no candidates to select a winner from")

	// ErrInvalidCandidate reports a candidate that cannot take part in a
	// selection: one with no ID, or one whose ID another candidate already
	// used. state.Patch.Winner names the winner by ID and nothing else, so
	// an empty or duplicated ID makes the recorded winner either
	// unnameable or ambiguous — and an ambiguous winner is worse than no
	// winner, because a human would promote whichever directory they
	// guessed.
	ErrInvalidCandidate = errors.New("join: candidate cannot take part in a selection")

	// ErrUnknownStrategy reports a fanout.select value with no row in the
	// selection table. config.Config.Validate already rejects one; Select
	// re-checks rather than trusting that validation ran, because the
	// alternative is silently defaulting to a rule the user did not ask
	// for after N candidates have already been paid for.
	ErrUnknownStrategy = errors.New("join: unknown candidate selection strategy")
)

// Strategy is one row of the selection table: a named, documented rule for
// picking the winner out of the candidates that passed.
//
// A Strategy value is only meaningful when it came from Strategies or from
// an Outcome — the rule itself is unexported, so the table is closed. That
// is deliberate: which rules exist is a configuration question
// (config.SelectStrategy is a fixed enum that the config validator polices),
// not an extension point, and a pluggable table would let a caller install a
// rule that config could not name.
type Strategy struct {
	// Name is the fanout.select value that chooses this row.
	Name config.SelectStrategy

	// Rule states in one sentence exactly what this row picks, including
	// how it breaks ties. It is written to be disagreed with: a reviewer
	// who thinks the rule is wrong should be able to say so from this
	// sentence alone, without reading the function that implements it.
	Rule string

	// Approximation is empty when the rule is exact. When it is not, it
	// names the data the rule does not have and says what it does instead,
	// and the join node reports it to the user rather than presenting an
	// approximate winner as an exact one.
	Approximation string

	// pick chooses among passing, which Select guarantees is non-empty and
	// in the order fanout recorded the candidates.
	pick func(passing []state.Candidate) state.Candidate
}

// Exact reports whether this row's rule is computed from the data it
// actually wants, rather than from a documented substitute.
func (s Strategy) Exact() bool { return s.Approximation == "" }

// strategies is belay's complete candidate selection table, in a stable
// order so error messages and tests do not depend on map iteration.
//
// # Only passing candidates are eligible
//
// Every row selects among candidates whose TestPassed is true, and Select —
// not the rows — enforces that. A "best of N" that can return a candidate
// whose tests fail is not selecting a best, it is selecting a survivor: the
// whole point of running N attempts is that at least one of them works.
//
// # Every row is deterministic
//
// The same candidates in the same order always produce the same winner.
// A best-of-N that picks differently on a re-run cannot be reproduced or
// audited, and it would break the dispatcher's re-run-after-crash contract:
// re-executing join must land on the winner the crashed attempt chose, or
// resume would silently promote a different candidate than the one the run
// had already reported.
var strategies = []Strategy{
	{
		Name: config.SelectFewestIssues,
		Rule: "among the candidates that passed, the one with the lowest IssueCount; " +
			"ties break by the lowest candidate ID, so the result does not depend on the order candidates were recorded in.",
		pick: fewestIssues,
	},
	{
		Name: config.SelectFirstPass,
		Rule: "the first candidate that passed, in the order fanout recorded them; " +
			"no tie is possible, because two candidates cannot share a position, and Select rejects duplicate IDs.",
		pick: firstInOrder,
	},
	{
		Name: config.SelectFastest,
		Rule: "the candidate that finished soonest, approximated by the first candidate that passed in recorded order; " +
			"no tie is possible, for the same reason as first_pass.",
		// The honest version of "fastest" would compare durations, and
		// belay records none that this node can see. state.Candidate has
		// no duration field (and inventing one is not this package's
		// call); state.HistoryEntry records a finish Time but not which
		// candidate it belongs to; and the journal, besides being the
		// dispatcher's file rather than a node's, carries no per-candidate
		// records at all today, because v0.1's fanout node prepares
		// candidate workspaces without running anything inside them.
		//
		// So this row substitutes recorded order, which under v0.1 is the
		// closest true statement available: fanout prepares candidates
		// SEQUENTIALLY, in candidate order, in one goroutine, so candidate
		// 1 finishes before candidate 2 finishes before candidate 3.
		// Earliest-finishing is therefore exactly first-in-recorded-order,
		// and the approximation is in reading "fastest" as "finished
		// soonest" (which is what config.SelectFastest's own doc comment
		// says) rather than "took the least time" — the two coincide only
		// while candidates start together, which sequential fanout does
		// not do.
		//
		// The day the dispatcher runs candidates concurrently and records
		// per-candidate timing, this is the one row that changes, and the
		// Approximation below is how a user finds out that today it has
		// not changed yet.
		Approximation: "no per-candidate duration is recorded anywhere a node can read, " +
			"so this run picked the first candidate that passed in recorded order instead; " +
			"under v0.1's sequential fanout that is the candidate that finished soonest, but it is not a duration comparison",
		pick: firstInOrder,
	},
}

// firstInOrder returns the first candidate, which is the earliest one fanout
// recorded. passing is never empty; Select checks that before calling a row.
func firstInOrder(passing []state.Candidate) state.Candidate { return passing[0] }

// fewestIssues returns the candidate with the lowest IssueCount, breaking
// ties by the lowest ID.
//
// The comparison is a strict total order over (IssueCount, ID), and IDs are
// unique by the time a row runs (see validate), so exactly one candidate is
// the minimum. That makes this row independent of the order candidates
// arrive in, not merely repeatable for one fixed order.
func fewestIssues(passing []state.Candidate) state.Candidate {
	best := passing[0]
	for _, c := range passing[1:] {
		if c.IssueCount < best.IssueCount || (c.IssueCount == best.IssueCount && c.ID < best.ID) {
			best = c
		}
	}
	return best
}

// Strategies returns belay's selection table, in the table's own stable
// order. The returned slice is a copy: a caller that reorders or truncates
// it cannot change what Select does.
func Strategies() []Strategy {
	out := make([]Strategy, len(strategies))
	copy(out, strategies)
	return out
}

// lookup finds name's row in the table.
func lookup(name config.SelectStrategy) (Strategy, error) {
	for _, s := range strategies {
		if s.Name == name {
			return s, nil
		}
	}
	return Strategy{}, fmt.Errorf("%w: %q is not one of %s", ErrUnknownStrategy, name, strategyNames())
}

// strategyNames lists the table's names for an error message.
func strategyNames() string {
	names := make([]string, 0, len(strategies))
	for _, s := range strategies {
		names = append(names, string(s.Name))
	}
	return strings.Join(names, ", ")
}

// Outcome is what Select decided, and everything the join node needs to
// explain that decision to a human.
//
// An Outcome with Passed == 0 is a complete, valid answer: it says every one
// of Total candidates failed, and it carries a zero Winner because there is
// no honest one to carry.
type Outcome struct {
	// Strategy is the table row that was applied.
	Strategy Strategy
	// Winner is the selected candidate, or the zero Candidate when no
	// candidate passed. Check Selected rather than testing Winner.ID.
	Winner state.Candidate
	// Total is how many candidates were considered.
	Total int
	// Passed is how many of them were eligible, that is, had TestPassed
	// set. Only these were offered to the rule.
	Passed int
}

// Selected reports whether Outcome names a winner.
func (o Outcome) Selected() bool { return o.Passed > 0 }

// Select applies the strategy named by name to candidates and reports the
// winner.
//
// Eligibility, tie-breaking and validation live here rather than in the
// table rows, so that every rule inherits the same invariants and a new row
// cannot quietly opt out of them: only candidates with TestPassed are
// offered to a rule, candidates are offered in the order fanout recorded
// them, and the result is deterministic for a given input.
//
// It returns an error only for the wiring bugs the sentinels above describe.
// "No candidate passed" is not one: that comes back as an Outcome with
// Passed == 0, because it is a true and useful answer about the run rather
// than a failure to reach one.
func Select(candidates []state.Candidate, name config.SelectStrategy) (Outcome, error) {
	strategy, err := lookup(name)
	if err != nil {
		return Outcome{}, err
	}
	if len(candidates) == 0 {
		return Outcome{}, fmt.Errorf("%w: fanout must run before join and record at least one", ErrNoCandidates)
	}
	if err := validate(candidates); err != nil {
		return Outcome{}, err
	}

	passing := make([]state.Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.TestPassed {
			passing = append(passing, c)
		}
	}

	out := Outcome{Strategy: strategy, Total: len(candidates), Passed: len(passing)}
	if len(passing) == 0 {
		return out, nil
	}
	out.Winner = strategy.pick(passing)
	return out, nil
}

// validate rejects candidates that cannot be named as a winner. It runs over
// every candidate, not only the passing ones: a duplicate ID among the
// failing candidates still means the blackboard is describing two different
// workspaces under one name, and the next run of this graph might select
// one of them.
func validate(candidates []state.Candidate) error {
	seen := make(map[string]int, len(candidates))
	for i, c := range candidates {
		if c.ID == "" {
			return fmt.Errorf("%w: the candidate at index %d has no ID, so nothing could record it as the winner", ErrInvalidCandidate, i)
		}
		if first, dup := seen[c.ID]; dup {
			return fmt.Errorf("%w: the candidates at index %d and %d share the ID %q, so a winner named by ID would be ambiguous",
				ErrInvalidCandidate, first, i, c.ID)
		}
		seen[c.ID] = i
	}
	return nil
}
