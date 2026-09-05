package join_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/nodes/join"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
)

// newRunContext builds the only context join reads: the blackboard's
// candidates and the configured strategy.
//
// It deliberately sets no Layout, no adapters and no workspace. The join
// node compares numbers other nodes already recorded — it opens no file and
// calls no adapter — and a test that needs a temp directory to exercise it
// would be evidence that it had started doing one of those things.
func newRunContext(t *testing.T, strategy config.SelectStrategy, candidates []state.Candidate) *graph.RunContext {
	t.Helper()
	cfg := config.Default()
	cfg.Fanout.Enabled = true
	cfg.Fanout.Candidates = len(candidates)
	cfg.Fanout.Select = strategy

	st := state.NewState("add a feature")
	st.Candidates = candidates

	return &graph.RunContext{
		Goal:     st.Goal,
		State:    st,
		Config:   cfg,
		RunID:    "20260101T000000Z-abcdef123456",
		NodeName: graph.NodeJoin,
		Step:     8,
		Attempt:  1,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestNodeName(t *testing.T) {
	if got := join.New().Name(); got != graph.NodeJoin {
		t.Fatalf("Name() = %q, want %q", got, graph.NodeJoin)
	}
}

// A selected winner ends the run, and the winner's ID is what lands in the
// Patch — the whole output of this node.
func TestRunSelectsWinner(t *testing.T) {
	tests := []struct {
		name       string
		strategy   config.SelectStrategy
		candidates []state.Candidate
		wantWinner string
	}{
		{
			name:       "fewest_issues",
			strategy:   config.SelectFewestIssues,
			candidates: []state.Candidate{cand("c1", true, 6), cand("c2", true, 2), cand("c3", false, 0)},
			wantWinner: "c2",
		},
		{
			name:       "fewest_issues with a tie broken by ID",
			strategy:   config.SelectFewestIssues,
			candidates: []state.Candidate{cand("c3", true, 1), cand("c1", true, 1)},
			wantWinner: "c1",
		},
		{
			name:       "first_pass",
			strategy:   config.SelectFirstPass,
			candidates: []state.Candidate{cand("c1", false, 0), cand("c2", true, 9), cand("c3", true, 0)},
			wantWinner: "c2",
		},
		{
			name:       "fastest",
			strategy:   config.SelectFastest,
			candidates: []state.Candidate{cand("c1", false, 0), cand("c2", true, 9), cand("c3", true, 0)},
			wantWinner: "c2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := newRunContext(t, tt.strategy, tt.candidates)

			got, err := join.New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("Run() returned a Result the dispatcher cannot act on: %v", err)
			}
			if got.Next != graph.End {
				t.Errorf("Run() Next = %q, want %q", got.Next, graph.End)
			}
			if got.Status != journal.StatusOK {
				t.Errorf("Run() Status = %v, want %v", got.Status, journal.StatusOK)
			}
			if !got.Done() {
				t.Errorf("Run() Done() = false, want true")
			}
			if got.Patch.Winner == nil {
				t.Fatalf("Run() Patch.Winner = nil, want %q", tt.wantWinner)
			}
			if *got.Patch.Winner != tt.wantWinner {
				t.Errorf("Run() Patch.Winner = %q, want %q", *got.Patch.Winner, tt.wantWinner)
			}
			if got.Usage != (belay.Usage{}) {
				t.Errorf("Run() Usage = %+v, want zero: join calls no agent", got.Usage)
			}
			if got.Patch.Candidates != nil {
				t.Errorf("Run() Patch.Candidates = %+v, want nil: join does not rewrite the candidate list", *got.Patch.Candidates)
			}
		})
	}
}

// The winner's Patch must actually land on the blackboard, and land there
// without disturbing the candidate list fanout recorded.
func TestRunPatchAppliesToState(t *testing.T) {
	candidates := []state.Candidate{cand("c1", true, 4), cand("c2", true, 1)}
	rc := newRunContext(t, config.SelectFewestIssues, candidates)

	got, err := join.New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	st := state.NewState("add a feature")
	st.Candidates = candidates
	if err := got.Patch.Apply(&st); err != nil {
		t.Fatalf("Patch.Apply() error = %v", err)
	}
	if st.Winner != "c2" {
		t.Errorf("state.Winner = %q after applying the patch, want %q", st.Winner, "c2")
	}
	if len(st.Candidates) != len(candidates) {
		t.Errorf("state.Candidates has %d entries after applying the patch, want %d", len(st.Candidates), len(candidates))
	}
}

// Every candidate failing is a real outcome, not an error: the run fails,
// says why, and records no winner rather than promoting a loser.
func TestRunNoCandidatePassed(t *testing.T) {
	for _, strategy := range allStrategies {
		t.Run(string(strategy), func(t *testing.T) {
			candidates := []state.Candidate{cand("c1", false, 0), cand("c2", false, 3), cand("c3", false, 1)}
			rc := newRunContext(t, strategy, candidates)

			got, err := join.New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run() error = %v, want nil: every candidate failing is an outcome, not an error", err)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("Run() returned a Result the dispatcher cannot act on: %v", err)
			}
			if got.Status != journal.StatusFailed {
				t.Errorf("Run() Status = %v, want %v", got.Status, journal.StatusFailed)
			}
			if got.Done() {
				t.Errorf("Run() Done() = true, want false: a run with no winner did not succeed")
			}
			if got.Patch.Winner == nil {
				t.Fatalf("Run() Patch.Winner = nil, want an explicitly cleared winner")
			}
			if *got.Patch.Winner != "" {
				t.Errorf("Run() Patch.Winner = %q, want empty: no candidate may be promoted", *got.Patch.Winner)
			}
			if !strings.Contains(got.Note, "3") || !strings.Contains(got.Note, "failed") {
				t.Errorf("Run() Note = %q, want it to say how many candidates failed", got.Note)
			}
		})
	}
}

// A join that finds no winner must clear a winner an earlier attempt left
// behind, not let it stand.
func TestRunNoWinnerClearsAStaleWinner(t *testing.T) {
	candidates := []state.Candidate{cand("c1", false, 0)}
	rc := newRunContext(t, config.SelectFirstPass, candidates)

	got, err := join.New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	st := state.NewState("add a feature")
	st.Candidates = candidates
	st.Winner = "c1-from-a-previous-attempt"
	if err := got.Patch.Apply(&st); err != nil {
		t.Fatalf("Patch.Apply() error = %v", err)
	}
	if st.Winner != "" {
		t.Fatalf("state.Winner = %q, want it cleared", st.Winner)
	}
}

// Wiring bugs are errors, not results: they say nothing about the run's
// code, and no selection rule can paper over them.
func TestRunWiringErrors(t *testing.T) {
	tests := []struct {
		name       string
		strategy   config.SelectStrategy
		candidates []state.Candidate
		want       error
	}{
		{
			name:       "join reached with no candidates",
			strategy:   config.SelectFewestIssues,
			candidates: nil,
			want:       join.ErrNoCandidates,
		},
		{
			name:       "fanout produced an empty candidate list",
			strategy:   config.SelectFewestIssues,
			candidates: []state.Candidate{},
			want:       join.ErrNoCandidates,
		},
		{
			name:       "a candidate has no ID",
			strategy:   config.SelectFirstPass,
			candidates: []state.Candidate{{Dir: "/tmp/nameless", TestPassed: true}},
			want:       join.ErrInvalidCandidate,
		},
		{
			name:       "two candidates share an ID",
			strategy:   config.SelectFirstPass,
			candidates: []state.Candidate{cand("c1", true, 0), cand("c1", true, 1)},
			want:       join.ErrInvalidCandidate,
		},
		{
			name:       "fanout.select names no rule",
			strategy:   config.SelectStrategy("cheapest"),
			candidates: []state.Candidate{cand("c1", true, 0)},
			want:       join.ErrUnknownStrategy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := newRunContext(t, tt.strategy, tt.candidates)

			got, err := join.New().Run(context.Background(), rc)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Run() error = %v, want one wrapping %v", err, tt.want)
			}
			if got.Patch.Winner != nil {
				t.Errorf("Run() recorded winner %q alongside an error", *got.Patch.Winner)
			}
			if got.Status == journal.StatusOK {
				t.Errorf("Run() Status = %v alongside an error", got.Status)
			}
		})
	}
}

func TestRunCancelledContext(t *testing.T) {
	rc := newRunContext(t, config.SelectFewestIssues, []state.Candidate{cand("c1", true, 0)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := join.New().Run(ctx, rc)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want one wrapping context.Canceled", err)
	}
	if got.Status != journal.StatusAborted {
		t.Errorf("Run() Status = %v, want %v", got.Status, journal.StatusAborted)
	}
	if got.Patch.Winner != nil {
		t.Errorf("Run() recorded winner %q on a cancelled context", *got.Patch.Winner)
	}
}

func TestRunNilRunContext(t *testing.T) {
	got, err := join.New().Run(context.Background(), nil)
	if err == nil {
		t.Fatal("Run() error = nil, want an error for a nil run context")
	}
	if got.Status == journal.StatusOK {
		t.Errorf("Run() Status = %v alongside an error", got.Status)
	}
}

// Re-running join after a crash must land on the same winner the crashed
// attempt chose. Selection is a pure function of the blackboard, so this is
// the node-level statement of the determinism select_test.go proves for the
// rules themselves.
func TestRunIsIdempotentAcrossReruns(t *testing.T) {
	const reruns = 100
	candidates := []state.Candidate{
		cand("c4", true, 2), cand("c1", true, 2), cand("c2", false, 0), cand("c3", true, 5),
	}

	for _, strategy := range allStrategies {
		t.Run(string(strategy), func(t *testing.T) {
			node := join.New()
			first, err := node.Run(context.Background(), newRunContext(t, strategy, candidates))
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			for i := 1; i < reruns; i++ {
				rc := newRunContext(t, strategy, candidates)
				rc.Attempt = i + 1
				got, err := node.Run(context.Background(), rc)
				if err != nil {
					t.Fatalf("Run() rerun %d error = %v", i, err)
				}
				if *got.Patch.Winner != *first.Patch.Winner {
					t.Fatalf("Run() rerun %d winner = %q, want %q (a resumed run would promote a different candidate)",
						i, *got.Patch.Winner, *first.Patch.Winner)
				}
			}
		})
	}
}

// Promotion is manual (ADR-0004), so the note a human reads in the timeline
// has to name the directory to copy from and say belay will not do it.
func TestRunNoteExplainsManualPromotion(t *testing.T) {
	winner := cand("c2", true, 1)
	rc := newRunContext(t, config.SelectFewestIssues, []state.Candidate{cand("c1", true, 8), winner})

	got, err := join.New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, want := range []string{winner.ID, winner.Dir, "by hand"} {
		if !strings.Contains(got.Note, want) {
			t.Errorf("Run() Note = %q, want it to contain %q", got.Note, want)
		}
	}
}

// A winner with no recorded directory still wins — the selection only needs
// an ID — but the note must not tell a user to copy changes back from "".
func TestRunWinnerWithoutDirectory(t *testing.T) {
	rc := newRunContext(t, config.SelectFirstPass, []state.Candidate{{ID: "c1", TestPassed: true}})

	got, err := join.New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if *got.Patch.Winner != "c1" {
		t.Errorf("Run() Patch.Winner = %q, want %q", *got.Patch.Winner, "c1")
	}
	if strings.Contains(got.Note, "from  by hand") || strings.HasSuffix(got.Note, "from ") {
		t.Errorf("Run() Note = %q, want it to cope with a candidate that records no directory", got.Note)
	}
}

// Run must not depend on a logger the dispatcher always sets but a test (or
// a future caller) might not.
func TestRunWithoutLogger(t *testing.T) {
	rc := newRunContext(t, config.SelectFewestIssues, []state.Candidate{cand("c1", true, 0)})
	rc.Logger = nil

	if _, err := join.New().Run(context.Background(), rc); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
}
