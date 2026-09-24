package graph_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// The seam these tests pin is the one the fanout node is written against: a
// node decides what to ask for, hands it to RunContext.CandidateRuns, and
// turns what comes back into a Result. What really runs candidates is a
// later piece of work, so what matters here is that the contract is complete
// enough to write that node — and to table-test it — against a fake, which
// is what miniFanout below is.

// fakeCandidateRunner stands in for the pool the dispatcher builds. It
// records what it was asked for and replays what it was scripted with,
// including in an order no caller may depend on.
type fakeCandidateRunner struct {
	asked    []graph.CandidateSpec
	outcomes []graph.CandidateOutcome
	err      error
}

func (f *fakeCandidateRunner) Run(_ context.Context, specs []graph.CandidateSpec) ([]graph.CandidateOutcome, error) {
	f.asked = specs
	return f.outcomes, f.err
}

// miniFanout is the shape of the node this seam exists for, reduced to the
// three things it does with the runner: refuse when there is none, ask for
// the specs it decided on, and record what comes back by index.
type miniFanout struct{ specs []graph.CandidateSpec }

func (miniFanout) Name() string { return graph.NodeFanout }

func (m miniFanout) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc.CandidateRuns == nil {
		return graph.Result{}, graph.ErrNoCandidateRunner
	}
	outcomes, err := rc.CandidateRuns.Run(ctx, m.specs)
	if err != nil {
		return graph.Result{}, err
	}
	cands := make([]state.Candidate, len(m.specs))
	var total belay.Usage
	for _, o := range outcomes {
		cands[o.Index-1] = state.Candidate{
			ID: o.ID, Dir: o.Workspace.Dir, RunDir: o.RunDir,
			Outcome:    o.Status.String(),
			TestPassed: o.State.Test.Total > 0 && o.State.Test.Failed == 0,
			IssueCount: len(o.State.Review.Issues),
			SpentUSD:   o.Spent.USD, Estimated: o.Spent.Estimated,
			DurationMS: o.DurationMS, Ephemeral: o.Workspace.Ephemeral, Note: o.Note,
		}
		total.USD += o.Spent.USD
	}
	return graph.Result{
		Next: graph.NodeJoin, Status: journal.StatusOK,
		Patch: state.Patch{Candidates: &cands}, Usage: total,
	}, nil
}

// candidateOutcome is a candidate that ended the way its index says, so a
// test can tell one slot from another.
func candidateOutcome(id string, index int, usd float64) graph.CandidateOutcome {
	st := state.NewState("ship it")
	st.Test.Total, st.Test.Passed = index, index
	st.Review.Issues = make([]belay.Issue, index)
	return graph.CandidateOutcome{
		ID: id, Index: index, RunDir: "candidate-runs/" + id,
		Status: state.RunStatusCompleted, State: st,
		Spent:      belay.Usage{USD: usd},
		Envelope:   state.Envelope{LimitUSD: 1.00, LimitTokens: 1000},
		Steps:      3,
		DurationMS: int64(index) * 100,
		Workspace:  belay.Workspace{ID: id, Dir: "/tmp/" + id, Ephemeral: true},
		Note:       id + " finished",
		Provision:  state.ProvisionOK,
	}
}

// candidateSpecs is what miniFanout decided to ask for: three candidates,
// each with its own envelope and the goal and plan alone to work from.
func candidateSpecs() []graph.CandidateSpec {
	specs := make([]graph.CandidateSpec, 0, 3)
	for _, id := range []string{"cand-01", "cand-02", "cand-03"} {
		specs = append(specs, graph.CandidateSpec{
			ID: id, Index: len(specs) + 1, Source: "/repo",
			Envelope: state.Envelope{LimitUSD: 1.00, LimitTokens: 1000},
			Seed:     state.SeedCandidateState(state.NewState("ship it")),
			Plan:     []byte("# plan\n"),
		})
	}
	return specs
}

func TestACandidateRunnerIsSomethingANodeCanBeTestedAgainst(t *testing.T) {
	specs := candidateSpecs()
	diskFull := errors.New("disk is full")

	tests := []struct {
		name    string
		runner  graph.CandidateRunner
		wantIDs []string
		wantUSD float64
		wantErr error
	}{
		{
			// The node cannot run candidates itself, so a context with no
			// runner is a wiring bug it must name rather than work around.
			name:    "a node with no runner refuses by name",
			runner:  nil,
			wantErr: graph.ErrNoCandidateRunner,
		},
		{
			// The order outcomes come back in is the order goroutines
			// happened to finish. Index, not position, is what fixes a
			// candidate's slot on the blackboard.
			name: "outcomes are recorded by index, not in the order they arrive",
			runner: &fakeCandidateRunner{outcomes: []graph.CandidateOutcome{
				candidateOutcome("cand-03", 3, 0.30),
				candidateOutcome("cand-01", 1, 0.10),
				candidateOutcome("cand-02", 2, 0.20),
			}},
			wantIDs: []string{"cand-01", "cand-02", "cand-03"},
			wantUSD: 0.60,
		},
		{
			name:    "a runner that could not run the candidates reports why",
			runner:  &fakeCandidateRunner{err: diskFull},
			wantErr: diskFull,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := miniFanout{specs: specs}
			rc := &graph.RunContext{Goal: "ship it", CandidateRuns: tt.runner}

			res, err := node.Run(context.Background(), rc)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Run() error = %v, want one matching %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}

			var gotIDs []string
			for _, c := range *res.Patch.Candidates {
				gotIDs = append(gotIDs, c.ID)
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Errorf("candidate ids = %v, want %v", gotIDs, tt.wantIDs)
			}
			if got := (*res.Patch.Candidates)[0]; got.IssueCount != 1 || !got.TestPassed || got.DurationMS != 100 {
				t.Errorf("candidate 1 = %+v, want the outcome whose Index is 1", got)
			}
			if !closeTo(res.Usage.USD, tt.wantUSD) {
				t.Errorf("Result.Usage.USD = %v, want %v", res.Usage.USD, tt.wantUSD)
			}

			// The specs the node decided on reach the runner unchanged:
			// ids, envelopes, seeds and the plan a candidate reads.
			fake, ok := tt.runner.(*fakeCandidateRunner)
			if !ok {
				t.Fatalf("runner is %T, want the fake", tt.runner)
			}
			if !reflect.DeepEqual(fake.asked, specs) {
				t.Errorf("the runner was asked for\n  %+v\nwant\n  %+v", fake.asked, specs)
			}
		})
	}
}

// TestCandidateRunSentinelsAreDistinct pins the two failures a candidate
// runner has to report separately: no runner at all, which is a wiring bug,
// and a directory under candidate-runs/ that belay did not write, which is a
// refusal to touch someone else's files.
func TestCandidateRunSentinelsAreDistinct(t *testing.T) {
	if errors.Is(graph.ErrNoCandidateRunner, graph.ErrCandidateRunCorrupt) {
		t.Error("ErrNoCandidateRunner matches ErrCandidateRunCorrupt; a caller cannot tell the two apart")
	}
	for _, err := range []error{graph.ErrNoCandidateRunner, graph.ErrCandidateRunCorrupt} {
		if !strings.HasPrefix(err.Error(), "graph: ") {
			t.Errorf("%v does not name its package; every sentinel in this package does", err)
		}
	}
}
