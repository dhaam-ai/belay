package join

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/state"
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
	//
	// On a row returned by Strategies it is the approximation the rule MAY
	// have to make; on the row inside an Outcome it is the one this
	// selection really made, cleared when the candidates carried everything
	// the rule wanted. A rule whose data only sometimes exists would
	// otherwise have to choose between crying wolf on every run and never
	// warning at all.
	Approximation string

	// pick chooses among eligible, which Select guarantees is non-empty and
	// in the order fanout recorded the candidates.
	pick func(eligible []state.Candidate) state.Candidate

	// approximates reports whether this row had to substitute for data it
	// wanted, given the candidates it was offered. Nil means the row's
	// Approximation is fixed: exact rules have none, and a rule whose data
	// can never exist always declares one.
	approximates func(eligible []state.Candidate) bool
}

// Exact reports whether this row's rule is computed from the data it
// actually wants, rather than from a documented substitute.
func (s Strategy) Exact() bool { return s.Approximation == "" }

// strategies is belay's complete candidate selection table, in a stable
// order so error messages and tests do not depend on map iteration.
//
// # Only eligible candidates are offered to a row
//
// Every row selects among the candidates eligible reports, and Select — not
// the rows — enforces that. A "best of N" that can return a candidate whose
// tests fail is not selecting a best, it is selecting a survivor: the whole
// point of running N attempts is that at least one of them works.
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
		Rule: "among the candidates that passed, the one whose own candidate run spent the least time in its nodes; " +
			"ties break by the lowest candidate ID, so the result does not depend on the order candidates were recorded in. " +
			"A blackboard that records no duration for some eligible candidate falls back to the first that passed in recorded order.",
		// The duration compared is the sum of the candidate run's own
		// node_finished durations, not wall-clock time, so the downtime
		// between a crash and the resume that healed it is not held against
		// a candidate nobody was running.
		//
		// This row used to substitute recorded order unconditionally, and
		// said why: "no per-candidate duration is recorded anywhere a node
		// can read [...] The day the dispatcher runs candidates
		// concurrently and records per-candidate timing, this is the one
		// row that changes." That day is this slice. The substitution
		// survives only for a blackboard written before candidates really
		// ran, where there is still nothing to compare — and it says so,
		// through approximates, rather than warning on every run.
		//
		// Reading "fastest" as "took the least time" rather than "finished
		// soonest" is deliberate now that candidates run CONCURRENTLY.
		// Finishing soonest under contention is partly a fact about the
		// pool's scheduling; time spent is a fact about the candidate.
		Approximation: "some eligible candidate records no duration, so this run picked the first that passed in recorded order instead; " +
			"that is not a duration comparison",
		pick:         fastest,
		approximates: func(eligible []state.Candidate) bool { return !allTimed(eligible) },
	},
}

// firstInOrder returns the first candidate, which is the earliest one fanout
// recorded. eligible is never empty; Select checks that before calling a row.
func firstInOrder(eligible []state.Candidate) state.Candidate { return eligible[0] }

// fastest returns the candidate whose own run spent the least time in its
// nodes, breaking ties by the lowest ID.
//
// It falls back to recorded order when any eligible candidate records no
// duration. Treating a missing duration as zero would make the candidate
// belay knows least about win every time, which is the opposite of what the
// rule is for; and comparing only the timed ones would silently narrow the
// field a person asked to choose from.
func fastest(eligible []state.Candidate) state.Candidate {
	if !allTimed(eligible) {
		return firstInOrder(eligible)
	}
	best := eligible[0]
	for _, c := range eligible[1:] {
		if c.DurationMS < best.DurationMS || (c.DurationMS == best.DurationMS && c.ID < best.ID) {
			best = c
		}
	}
	return best
}

// allTimed reports whether every candidate carries a duration to compare. A
// zero is "not recorded" rather than "instantaneous": no candidate run that
// executed a node took no time at all.
func allTimed(eligible []state.Candidate) bool {
	for _, c := range eligible {
		if c.DurationMS <= 0 {
			return false
		}
	}
	return true
}

// fewestIssues returns the candidate with the lowest IssueCount, breaking
// ties by the lowest ID.
//
// The comparison is a strict total order over (IssueCount, ID), and IDs are
// unique by the time a row runs (see validate), so exactly one candidate is
// the minimum. That makes this row independent of the order candidates
// arrive in, not merely repeatable for one fixed order.
func fewestIssues(eligible []state.Candidate) state.Candidate {
	best := eligible[0]
	for _, c := range eligible[1:] {
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
	// Completed is how many candidates' own runs reached a verdict about
	// the code: the run completed, or the blackboard predates candidate
	// runs and records no outcome at all.
	//
	// It is what tells "all of them failed their tests" apart from "none of
	// them ever ran" — two results a person would act on very differently,
	// and which a single "no winner" would report identically.
	Completed int
	// Passed is how many candidates were eligible: their tests passed AND
	// their own run completed. Only these were offered to the rule.
	Passed int
}

// Selected reports whether Outcome names a winner.
func (o Outcome) Selected() bool { return o.Passed > 0 }

// eligible reports whether a candidate may be offered to a selection rule:
// its tests passed AND its own candidate run completed.
//
// The second half is not redundant. A candidate whose tests passed and whose
// quality gate then rejected the work, until its fix loop gave up, ends
// run_failed — and letting it win would promote code a single run would
// never have promoted. So would a candidate that aborted part-way through
// its envelope with a green test result from an earlier round, or one whose
// workspace was refused and which therefore has no result at all.
//
// An empty Outcome stays eligible. That is every candidate on a blackboard
// written before candidate runs existed, where nothing recorded how a
// candidate ended, and reading "unrecorded" as "did not complete" would make
// every one of those runs unable to select a winner it had already selected.
//
// It is one function rather than a term inside each row so that a rule
// cannot opt out of it, and so that the next thing that makes a candidate
// ineligible — a policy violation, say — is added in one place.
func eligible(c state.Candidate) bool {
	return c.TestPassed && completedRun(c)
}

// completedRun reports whether a candidate's own run reached a verdict about
// the code, whatever that verdict was.
func completedRun(c state.Candidate) bool {
	return c.Outcome == "" || c.Outcome == state.RunStatusCompleted.String()
}

// Select applies the strategy named by name to candidates and reports the
// winner.
//
// Eligibility, tie-breaking and validation live here rather than in the
// table rows, so that every rule inherits the same invariants and a new row
// cannot quietly opt out of them: only eligible candidates are offered to a
// rule, candidates are offered in the order fanout recorded them, and the
// result is deterministic for a given input.
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

	offered := make([]state.Candidate, 0, len(candidates))
	completed := 0
	for _, c := range candidates {
		if completedRun(c) {
			completed++
		}
		if eligible(c) {
			offered = append(offered, c)
		}
	}

	// strategy is this function's own copy of the row, so clearing the
	// approximation it did not have to make says what THIS selection did
	// without rewriting the table for every later caller.
	if strategy.approximates != nil && !strategy.approximates(offered) {
		strategy.Approximation = ""
	}

	out := Outcome{Strategy: strategy, Total: len(candidates), Completed: completed, Passed: len(offered)}
	if len(offered) == 0 {
		return out, nil
	}
	out.Winner = strategy.pick(offered)
	return out, nil
}

// validate rejects candidates that cannot be named as a winner. It runs over
// every candidate, not only the eligible ones: a duplicate ID among the
// ineligible candidates still means the blackboard is describing two different
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
