package join_test

import (
	"errors"
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

// fastest is the one row that cannot compute what it is named after, so it
// must say so rather than pass its answer off as a duration comparison.
func TestFastestDeclaresItsApproximation(t *testing.T) {
	for _, s := range join.Strategies() {
		if s.Name != config.SelectFastest {
			if !s.Exact() {
				t.Errorf("strategy %q declares an approximation %q, but its rule is exact", s.Name, s.Approximation)
			}
			continue
		}
		if s.Exact() {
			t.Fatalf("strategy %q claims to be exact, but belay records no per-candidate duration", s.Name)
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
			if !got.Selected() && got.Winner != (state.Candidate{}) {
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
					if got.Winner != first.Winner {
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
	if !slices.Equal(candidates, before) {
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
