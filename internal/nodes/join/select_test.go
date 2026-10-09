package join_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/nodes/join"
	"github.com/dhaam-ai/belay/internal/state"
)

// cand is a terse candidate constructor: id, whether it passed, and its
// issue count are the only three fields any rule reads.
func cand(id string, passed bool, issues int) state.Candidate {
	return state.Candidate{ID: id, Dir: "/tmp/" + id, TestPassed: passed, IssueCount: issues}
}

// allStrategies is every strategy config can name. It is written out
// literally rather than derived from join.Strategies so that a row deleted
// from the table fails a test instead of quietly shrinking the coverage.
var allStrategies = []config.SelectStrategy{
	config.SelectFewestIssues,
	config.SelectFirstPass,
	config.SelectFastest,
}

// The table is the contract: every strategy config can name must have
// exactly one row, and every row must document its rule.
func TestStrategiesTable(t *testing.T) {
	table := join.Strategies()
	if len(table) != len(allStrategies) {
		t.Fatalf("Strategies() has %d rows, want %d", len(table), len(allStrategies))
	}

	for _, want := range allStrategies {
		idx := slices.IndexFunc(table, func(s join.Strategy) bool { return s.Name == want })
		if idx < 0 {
			t.Errorf("Strategies() has no row for %q", want)
		}
	}
	for _, s := range table {
		if strings.TrimSpace(s.Rule) == "" {
			t.Errorf("strategy %q has an empty Rule; a rule a reviewer cannot read is a rule nobody can disagree with", s.Name)
		}
		if got := s.Exact(); got != (s.Approximation == "") {
			t.Errorf("strategy %q: Exact() = %v with Approximation %q", s.Name, got, s.Approximation)
		}
	}
}

// fastest is the one row that may have to substitute for the data it is
// named after — a blackboard written before candidate runs recorded any
// duration — so the TABLE declares that substitution. Whether a given
// selection really made it is the Outcome's to say; see
// TestFastestIsExactWhenDurationsAreRecorded.
func TestFastestDeclaresItsApproximation(t *testing.T) {
	for _, s := range join.Strategies() {
		if s.Name != config.SelectFastest {
			if !s.Exact() {
				t.Errorf("strategy %q declares an approximation %q, but its rule is exact", s.Name, s.Approximation)
			}
			continue
		}
		if s.Exact() {
			t.Fatalf("strategy %q claims to be exact, but a blackboard may record no per-candidate duration", s.Name)
		}
	}
}

// The table returned to a caller is a copy: reordering it must not reorder
// the table Select applies.
func TestStrategiesReturnsACopy(t *testing.T) {
	first := join.Strategies()
	slices.Reverse(first)
	first[0].Name = "clobbered"

	second := join.Strategies()
	if second[0].Name != config.SelectFewestIssues {
		t.Fatalf("Strategies()[0].Name = %q after a caller mutated an earlier copy, want %q",
			second[0].Name, config.SelectFewestIssues)
	}
}

func TestSelect(t *testing.T) {
	tests := []struct {
		name       string
		strategy   config.SelectStrategy
		candidates []state.Candidate
		wantWinner string // empty means "no candidate passed"
		wantPassed int
	}{
		{
			name:       "fewest_issues picks the lowest issue count among passing",
			strategy:   config.SelectFewestIssues,
			candidates: []state.Candidate{cand("c1", true, 9), cand("c2", true, 2), cand("c3", true, 5)},
			wantWinner: "c2",
			wantPassed: 3,
		},
		{
			name:       "fewest_issues ignores a failing candidate with fewer issues",
			strategy:   config.SelectFewestIssues,
			candidates: []state.Candidate{cand("c1", false, 0), cand("c2", true, 7), cand("c3", true, 4)},
			wantWinner: "c3",
			wantPassed: 2,
		},
		{
			name:       "fewest_issues breaks a tie by the lowest ID",
			strategy:   config.SelectFewestIssues,
			candidates: []state.Candidate{cand("c3", true, 1), cand("c1", true, 1), cand("c2", true, 1)},
			wantWinner: "c1",
			wantPassed: 3,
		},
		{
			name:       "fewest_issues breaks a tie by ID, not by recorded order",
			strategy:   config.SelectFewestIssues,
			candidates: []state.Candidate{cand("b", true, 3), cand("a", true, 3)},
			wantWinner: "a",
			wantPassed: 2,
		},
		{
			name:       "first_pass picks the first passing candidate in recorded order",
			strategy:   config.SelectFirstPass,
			candidates: []state.Candidate{cand("c1", false, 0), cand("c2", true, 8), cand("c3", true, 1)},
			wantWinner: "c2",
			wantPassed: 2,
		},
		{
			name:       "first_pass ignores issue counts entirely",
			strategy:   config.SelectFirstPass,
			candidates: []state.Candidate{cand("c1", true, 100), cand("c2", true, 0)},
			wantWinner: "c1",
			wantPassed: 2,
		},
		{
			name:       "fastest degrades to the first passing candidate in recorded order",
			strategy:   config.SelectFastest,
			candidates: []state.Candidate{cand("c1", false, 0), cand("c2", true, 8), cand("c3", true, 1)},
			wantWinner: "c2",
			wantPassed: 2,
		},
		{
			name:       "fastest ignores issue counts, because it is not a quality rule",
			strategy:   config.SelectFastest,
			candidates: []state.Candidate{cand("c1", true, 100), cand("c2", true, 0)},
			wantWinner: "c1",
			wantPassed: 2,
		},
		{
			name:       "a single passing candidate wins under every rule",
			strategy:   config.SelectFewestIssues,
			candidates: []state.Candidate{cand("only", true, 42)},
			wantWinner: "only",
			wantPassed: 1,
		},
		{
			name:       "no candidate passed is an outcome, not an error",
			strategy:   config.SelectFewestIssues,
			candidates: []state.Candidate{cand("c1", false, 0), cand("c2", false, 1)},
			wantWinner: "",
			wantPassed: 0,
		},
		{
			name:       "no candidate passed under first_pass either",
			strategy:   config.SelectFirstPass,
			candidates: []state.Candidate{cand("c1", false, 0)},
			wantWinner: "",
			wantPassed: 0,
		},
		{
			name:       "no candidate passed under fastest either",
			strategy:   config.SelectFastest,
			candidates: []state.Candidate{cand("c1", false, 0)},
			wantWinner: "",
			wantPassed: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := join.Select(tt.candidates, tt.strategy)
			if err != nil {
				t.Fatalf("Select() error = %v, want nil", err)
			}
			if got.Winner.ID != tt.wantWinner {
				t.Errorf("Select() winner = %q, want %q", got.Winner.ID, tt.wantWinner)
			}
			if got.Selected() != (tt.wantWinner != "") {
				t.Errorf("Select() Selected() = %v, want %v", got.Selected(), tt.wantWinner != "")
			}
			if got.Passed != tt.wantPassed {
				t.Errorf("Select() Passed = %d, want %d", got.Passed, tt.wantPassed)
			}
			if got.Total != len(tt.candidates) {
				t.Errorf("Select() Total = %d, want %d", got.Total, len(tt.candidates))
			}
			if got.Strategy.Name != tt.strategy {
				t.Errorf("Select() Strategy.Name = %q, want %q", got.Strategy.Name, tt.strategy)
			}
			if !got.Selected() && !reflect.DeepEqual(got.Winner, state.Candidate{}) {
				t.Errorf("Select() carried winner %+v with nothing passing", got.Winner)
			}
		})
	}
}

// A non-deterministic best-of-N is unreproducible and unauditable: the same
// inputs must select the same winner every time, for every rule, including
// when every candidate ties.
func TestSelectIsDeterministic(t *testing.T) {
	const runs = 100

	inputs := map[string][]state.Candidate{
		"mixed": {cand("c3", true, 2), cand("c1", true, 5), cand("c2", false, 0), cand("c4", true, 2)},
		"all tied on issues": {
			cand("c9", true, 3), cand("c1", true, 3), cand("c5", true, 3), cand("c3", true, 3),
		},
		"tied and interleaved with failures": {
			cand("z", false, 0), cand("m", true, 1), cand("a", true, 1), cand("b", false, 9),
		},
	}

	for _, strategy := range allStrategies {
		for name, candidates := range inputs {
			t.Run(string(strategy)+"/"+name, func(t *testing.T) {
				first, err := join.Select(candidates, strategy)
				if err != nil {
					t.Fatalf("Select() error = %v", err)
				}
				for i := 1; i < runs; i++ {
					got, err := join.Select(candidates, strategy)
					if err != nil {
						t.Fatalf("Select() run %d error = %v", i, err)
					}
					if !reflect.DeepEqual(got.Winner, first.Winner) {
						t.Fatalf("Select() run %d winner = %+v, want %+v (selection is not deterministic)",
							i, got.Winner, first.Winner)
					}
				}
			})
		}
	}
}

// fewest_issues documents a stronger guarantee than the other two rules: it
// compares values only, so it is independent of the order candidates were
// recorded in, not merely repeatable for one fixed order.
func TestFewestIssuesIsOrderIndependent(t *testing.T) {
	candidates := []state.Candidate{cand("c1", true, 4), cand("c2", true, 1), cand("c3", true, 1), cand("c4", false, 0)}

	forward, err := join.Select(candidates, config.SelectFewestIssues)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	reversed := slices.Clone(candidates)
	slices.Reverse(reversed)
	backward, err := join.Select(reversed, config.SelectFewestIssues)
	if err != nil {
		t.Fatalf("Select() reversed error = %v", err)
	}

	if forward.Winner.ID != backward.Winner.ID {
		t.Fatalf("winner = %q forward and %q reversed, want the same candidate", forward.Winner.ID, backward.Winner.ID)
	}
	if forward.Winner.ID != "c2" {
		t.Fatalf("winner = %q, want %q (lowest ID among the issue-count tie)", forward.Winner.ID, "c2")
	}
}

// Select does not mutate what it is given: the candidates slice belongs to
// the caller's State snapshot.
func TestSelectDoesNotMutateInput(t *testing.T) {
	candidates := []state.Candidate{cand("c2", true, 5), cand("c1", true, 1), cand("c3", false, 0)}
	before := slices.Clone(candidates)

	if _, err := join.Select(candidates, config.SelectFewestIssues); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if !reflect.DeepEqual(candidates, before) {
		t.Fatalf("Select() mutated its input: got %+v, want %+v", candidates, before)
	}
}

func TestSelectErrors(t *testing.T) {
	tests := []struct {
		name       string
		strategy   config.SelectStrategy
		candidates []state.Candidate
		want       error
	}{
		{
			name:       "zero candidates is a wiring bug",
			strategy:   config.SelectFewestIssues,
			candidates: nil,
			want:       join.ErrNoCandidates,
		},
		{
			name:       "an empty candidate slice is the same wiring bug",
			strategy:   config.SelectFirstPass,
			candidates: []state.Candidate{},
			want:       join.ErrNoCandidates,
		},
		{
			name:       "a candidate with no ID cannot be recorded as the winner",
			strategy:   config.SelectFewestIssues,
			candidates: []state.Candidate{cand("c1", true, 1), {Dir: "/tmp/nameless", TestPassed: true}},
			want:       join.ErrInvalidCandidate,
		},
		{
			name:       "duplicate IDs make the winner ambiguous",
			strategy:   config.SelectFewestIssues,
			candidates: []state.Candidate{cand("c1", true, 1), cand("c1", true, 2)},
			want:       join.ErrInvalidCandidate,
		},
		{
			name:       "duplicate IDs among failing candidates are rejected too",
			strategy:   config.SelectFirstPass,
			candidates: []state.Candidate{cand("c1", true, 0), cand("dup", false, 0), cand("dup", false, 0)},
			want:       join.ErrInvalidCandidate,
		},
		{
			name:       "an unknown strategy is not silently defaulted",
			strategy:   config.SelectStrategy("cheapest"),
			candidates: []state.Candidate{cand("c1", true, 1)},
			want:       join.ErrUnknownStrategy,
		},
		{
			name:       "an unset strategy is not silently defaulted either",
			strategy:   "",
			candidates: []state.Candidate{cand("c1", true, 1)},
			want:       join.ErrUnknownStrategy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := join.Select(tt.candidates, tt.strategy)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Select() error = %v, want one wrapping %v", err, tt.want)
			}
			if got.Selected() {
				t.Errorf("Select() reported a winner %+v alongside an error", got.Winner)
			}
		})
	}
}

// An unknown strategy's error must name the strategies that do exist: the
// user's next action is to pick one of them.
func TestUnknownStrategyErrorNamesTheTable(t *testing.T) {
	_, err := join.Select([]state.Candidate{cand("c1", true, 0)}, config.SelectStrategy("cheapest"))
	if err == nil {
		t.Fatal("Select() error = nil, want an unknown-strategy error")
	}
	for _, s := range allStrategies {
		if !strings.Contains(err.Error(), string(s)) {
			t.Errorf("error %q does not name the valid strategy %q", err, s)
		}
	}
}

// ran is a candidate whose own candidate run reached the given end, in the
// vocabulary state.Candidate.Outcome uses.
func ran(id string, passed bool, issues int, outcome string) state.Candidate {
	c := cand(id, passed, issues)
	c.Outcome = outcome
	return c
}

// timed is a candidate whose own run completed and recorded how long its
// nodes took.
func timed(id string, ms int64) state.Candidate {
	c := ran(id, true, 0, "completed")
	c.DurationMS = ms
	return c
}

// Passing tests are necessary but not sufficient. A candidate whose tests
// passed and whose run then failed — its quality gate rejected the work and
// its fix loop gave up — must not win, because promoting it would ship code
// a single run would have refused.
func TestSelectRequiresACompletedCandidateRun(t *testing.T) {
	tests := []struct {
		name       string
		candidates []state.Candidate
		wantWinner string
		wantPassed int
	}{
		{
			name:       "a blackboard written before candidates really ran stays eligible",
			candidates: []state.Candidate{ran("c1", true, 3, ""), ran("c2", true, 1, "")},
			wantWinner: "c2",
			wantPassed: 2,
		},
		{
			name:       "a completed candidate run is eligible",
			candidates: []state.Candidate{ran("c1", true, 3, "completed"), ran("c2", true, 1, "completed")},
			wantWinner: "c2",
			wantPassed: 2,
		},
		{
			name:       "tests passed but the run failed is not eligible, however few issues it has",
			candidates: []state.Candidate{ran("c1", true, 0, "failed"), ran("c2", true, 4, "completed")},
			wantWinner: "c2",
			wantPassed: 1,
		},
		{
			name:       "a candidate that aborted on its envelope is not eligible",
			candidates: []state.Candidate{ran("c1", true, 0, "aborted"), ran("c2", true, 4, "completed")},
			wantWinner: "c2",
			wantPassed: 1,
		},
		{
			name:       "a candidate that never ran is not eligible",
			candidates: []state.Candidate{ran("c1", true, 0, "error"), ran("c2", true, 4, "completed")},
			wantWinner: "c2",
			wantPassed: 1,
		},
		{
			name:       "every candidate ran and failed its tests",
			candidates: []state.Candidate{ran("c1", false, 0, "completed"), ran("c2", false, 0, "completed")},
			wantWinner: "",
			wantPassed: 0,
		},
		{
			name:       "no candidate ever ran",
			candidates: []state.Candidate{ran("c1", false, 0, "error"), ran("c2", true, 0, "aborted")},
			wantWinner: "",
			wantPassed: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := join.Select(tt.candidates, config.SelectFewestIssues)
			if err != nil {
				t.Fatalf("Select() error = %v, want nil", err)
			}
			if got.Winner.ID != tt.wantWinner {
				t.Errorf("Select() winner = %q, want %q", got.Winner.ID, tt.wantWinner)
			}
			if got.Passed != tt.wantPassed {
				t.Errorf("Select() Passed = %d, want %d", got.Passed, tt.wantPassed)
			}
		})
	}
}

// Completed is how many candidates' own runs reached a verdict about the
// code at all, which is what tells "all of them failed their tests" apart
// from "none of them ever ran".
func TestSelectCountsCompletedRunsSeparatelyFromPassingOnes(t *testing.T) {
	tests := []struct {
		name          string
		candidates    []state.Candidate
		wantCompleted int
		wantPassed    int
	}{
		{
			name:          "legacy candidates count as completed, because nothing else is recorded",
			candidates:    []state.Candidate{cand("c1", true, 0), cand("c2", false, 0)},
			wantCompleted: 2,
			wantPassed:    1,
		},
		{
			name:          "a run that failed did not complete",
			candidates:    []state.Candidate{ran("c1", true, 0, "completed"), ran("c2", true, 0, "failed")},
			wantCompleted: 1,
			wantPassed:    1,
		},
		{
			name:          "nothing ran",
			candidates:    []state.Candidate{ran("c1", false, 0, "error"), ran("c2", false, 0, "error")},
			wantCompleted: 0,
			wantPassed:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := join.Select(tt.candidates, config.SelectFewestIssues)
			if err != nil {
				t.Fatalf("Select() error = %v, want nil", err)
			}
			if got.Completed != tt.wantCompleted {
				t.Errorf("Select() Completed = %d, want %d", got.Completed, tt.wantCompleted)
			}
			if got.Passed != tt.wantPassed {
				t.Errorf("Select() Passed = %d, want %d", got.Passed, tt.wantPassed)
			}
		})
	}
}

// Now that a candidate run records how long its own nodes took, fastest
// compares durations instead of substituting recorded order — the change
// select.go's own comment invited. Where the data is missing it still
// substitutes, and still says so.
func TestFastestIsExactWhenDurationsAreRecorded(t *testing.T) {
	tests := []struct {
		name       string
		candidates []state.Candidate
		wantWinner string
		wantExact  bool
	}{
		{
			name:       "the shortest total node time wins, whatever order it was recorded in",
			candidates: []state.Candidate{timed("c3", 900), timed("c2", 300), timed("c1", 300)},
			wantWinner: "c1",
			wantExact:  true,
		},
		{
			name:       "a tie breaks by the lowest id, so the winner does not depend on recorded order",
			candidates: []state.Candidate{timed("c2", 300), timed("c1", 300)},
			wantWinner: "c1",
			wantExact:  true,
		},
		{
			name:       "a slow candidate that was recorded first does not win",
			candidates: []state.Candidate{timed("c1", 5000), timed("c2", 10)},
			wantWinner: "c2",
			wantExact:  true,
		},
		{
			name:       "a failing candidate's duration is not compared",
			candidates: []state.Candidate{ran("c1", false, 0, "completed"), timed("c2", 800)},
			wantWinner: "c2",
			wantExact:  true,
		},
		{
			name:       "a blackboard with no durations at all keeps the substitution, and says so",
			candidates: []state.Candidate{cand("c1", true, 0), cand("c2", true, 0)},
			wantWinner: "c1",
			wantExact:  false,
		},
		{
			name:       "one eligible candidate without a duration is enough to make the comparison a substitution",
			candidates: []state.Candidate{timed("c1", 5000), ran("c2", true, 0, "completed")},
			wantWinner: "c1",
			wantExact:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := join.Select(tt.candidates, config.SelectFastest)
			if err != nil {
				t.Fatalf("Select() error = %v, want nil", err)
			}
			if got.Winner.ID != tt.wantWinner {
				t.Errorf("Select() winner = %q, want %q", got.Winner.ID, tt.wantWinner)
			}
			if got.Strategy.Exact() != tt.wantExact {
				t.Errorf("Select() Strategy.Exact() = %v (approximation %q), want %v",
					got.Strategy.Exact(), got.Strategy.Approximation, tt.wantExact)
			}
		})
	}
}

// An Outcome carrying a cleared approximation must not clear the table's.
// The row still describes the substitution it makes when the data is
// missing, because that is a property of the rule, not of one run.
func TestAnExactSelectionDoesNotRewriteTheTable(t *testing.T) {
	if _, err := join.Select([]state.Candidate{timed("c1", 10), timed("c2", 20)}, config.SelectFastest); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	for _, s := range join.Strategies() {
		if s.Name == config.SelectFastest && s.Exact() {
			t.Fatal("selecting with recorded durations cleared the fastest row's approximation in the table itself")
		}
	}
}

// guarded is a candidate whose own run completed with n policy violations
// outstanding.
func guarded(id string, violations int) state.Candidate {
	c := ran(id, true, 0, "completed")
	c.PolicyViolations = violations
	return c
}

// A candidate whose change breaks the repository's guardrails must not win,
// however good its tests and its review look. The whole point of a policy is
// that it holds whatever the tests say, and best-of-N would otherwise be the
// one way around it: run the change N times and promote whichever copy the
// gate happened not to stop.
//
// It reaches join at all because a repairable violation can survive a
// candidate's own fix loop giving up, which ends the candidate's run — and
// because a candidate run recorded by a belay that judged no policy carries
// zero, which is what keeps every existing blackboard selecting the winner
// it already selected.
func TestSelectRefusesACandidateThatBrokeThePolicy(t *testing.T) {
	tests := []struct {
		name       string
		candidates []state.Candidate
		wantWinner string
		wantPassed int
	}{
		{
			name:       "no violations anywhere",
			candidates: []state.Candidate{guarded("c1", 0), guarded("c2", 0)},
			wantWinner: "c1",
			wantPassed: 2,
		},
		{
			name:       "the candidate with violations loses to the one without",
			candidates: []state.Candidate{guarded("c1", 2), guarded("c2", 0)},
			wantWinner: "c2",
			wantPassed: 1,
		},
		{
			name:       "one violation is enough",
			candidates: []state.Candidate{guarded("c1", 1), guarded("c2", 0)},
			wantWinner: "c2",
			wantPassed: 1,
		},
		{
			name:       "every candidate broke the policy: there is no winner",
			candidates: []state.Candidate{guarded("c1", 1), guarded("c2", 3)},
			wantWinner: "",
			wantPassed: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := join.Select(tt.candidates, config.SelectFewestIssues)
			if err != nil {
				t.Fatalf("Select() error = %v, want nil", err)
			}
			if got.Winner.ID != tt.wantWinner {
				t.Errorf("Winner = %q, want %q", got.Winner.ID, tt.wantWinner)
			}
			if got.Passed != tt.wantPassed {
				t.Errorf("Passed = %d, want %d", got.Passed, tt.wantPassed)
			}
		})
	}
}
