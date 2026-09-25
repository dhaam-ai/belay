package fanout_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dhaam-ai/belay/internal/budget"
	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/fanout"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/google/go-cmp/cmp"
)

const testRunID = "20260101T000000Z-abcdef123456"

// planText is what every fixture writes to artifacts/plan.md, so a test can
// assert that the bytes reaching a candidate are the parent's plan and not
// something the node invented.
const planText = "# plan\n\n1. do the thing\n"

// recordingRunner is the candidate-run seam as a table test can drive it:
// it records the specs it was handed and answers with scripted outcomes.
//
// It is what keeps the fanout node a decision the test can inspect. The
// node's whole job is choosing ids, envelopes and a verdict; the goroutines,
// child dispatchers and durable writes that really execute a candidate live
// behind this interface, in internal/graph, and none of them has to exist
// for this package's tests to say what the node decided.
type recordingRunner struct {
	mu    sync.Mutex
	calls [][]graph.CandidateSpec

	// answer is consulted per call, so a test can make a resume disagree
	// with the attempt before it. When nil, outcomes and err are returned.
	answer   func(specs []graph.CandidateSpec) ([]graph.CandidateOutcome, error)
	outcomes []graph.CandidateOutcome
	err      error
}

var _ graph.CandidateRunner = (*recordingRunner)(nil)

func (r *recordingRunner) Run(_ context.Context, specs []graph.CandidateSpec) ([]graph.CandidateOutcome, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]graph.CandidateSpec(nil), specs...))
	answer := r.answer
	r.mu.Unlock()
	if answer != nil {
		return answer(specs)
	}
	return r.outcomes, r.err
}

// lastSpecs returns the specs of the most recent call, and fails the test
// when the node never called the runner at all.
func (r *recordingRunner) lastSpecs(t *testing.T) []graph.CandidateSpec {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		t.Fatal("the fanout node never asked the candidate runner to run anything")
	}
	return r.calls[len(r.calls)-1]
}

func (r *recordingRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// enabledConfig is a valid pinned fanout shape with both budget ceilings
// under the test's control: MaxTokens is disabled so a USD assertion cannot
// accidentally trip the token ceiling instead.
func enabledConfig(candidates int) config.Config {
	cfg := config.Default()
	cfg.Fanout.Enabled = true
	cfg.Fanout.Candidates = candidates
	cfg.Budget.MaxUSD = 10
	cfg.Budget.MaxTokens = 0
	return cfg
}

// newLayout returns a Layout rooted at the repository belay is operating on,
// and that repository's path, with the run's plan already archived.
//
// The run directory lives inside the workspace (<repo>/.belay/runs/<id>), so
// Layout.WorkspaceDir -- which is what the dispatcher puts on
// RunContext.Workspace, and therefore what a candidate is isolated from --
// is the repo itself.
func newLayout(t *testing.T) (state.Layout, string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(src, 0o750); err != nil {
		t.Fatalf("create source repo: %v", err)
	}
	layout, err := state.NewLayout(src, testRunID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		t.Fatalf("create run dir: %v", err)
	}
	plan, err := layout.ArtifactPath("plan.md")
	if err != nil {
		t.Fatalf("ArtifactPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(plan), 0o750); err != nil {
		t.Fatalf("create artifacts dir: %v", err)
	}
	if err := os.WriteFile(plan, []byte(planText), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return layout, src
}

// snapshot converts a manifest budget into the snapshot the dispatcher hands
// a node, so a test says "this run has already spent X" without touching a
// file the node no longer reads.
func snapshot(spent state.Budget) budget.Snapshot {
	return budget.Snapshot{
		LimitUSD:  spent.LimitUSD,
		SpentUSD:  spent.SpentUSD,
		TokensIn:  spent.TokensIn,
		TokensOut: spent.TokensOut,
		Estimated: spent.Estimated,
	}
}

// approvedState is the blackboard a run reaching fanout carries: a goal, an
// approved plan, and the leftovers of the parent's own work that must never
// reach a candidate.
func approvedState() state.State {
	s := state.NewState("add a feature")
	s.Plan = state.Plan{Path: "artifacts/plan.md", Approved: true, Digest: "sha256:abc"}
	s.Code = state.Code{SessionID: "parent-session", ChangedFiles: []string{"main.go"}}
	s.Test = state.Test{Total: 9, Passed: 9}
	return s
}

func newRunContext(cfg config.Config, layout state.Layout, runs graph.CandidateRunner, spent state.Budget) *graph.RunContext {
	return &graph.RunContext{
		Goal:          "add a feature",
		State:         approvedState(),
		Config:        cfg,
		Layout:        layout,
		Workspace:     layout.WorkspaceDir(),
		RunID:         layout.RunID(),
		Budget:        snapshot(spent),
		NodeName:      graph.NodeFanout,
		Step:          7,
		Attempt:       1,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		CandidateRuns: runs,
	}
}

// newFixture wires the common case: a valid layout with an archived plan,
// and a scriptable candidate runner.
func newFixture(t *testing.T, cfg config.Config, spent state.Budget) (*graph.RunContext, *recordingRunner, string) {
	t.Helper()
	layout, src := newLayout(t)
	runs := &recordingRunner{}
	return newRunContext(cfg, layout, runs, spent), runs, src
}

// completed is a candidate that ran to the end: tests green, a review with
// issues counted, a cost and a duration.
func completed(index int, id string, issues int, usd float64, ms int64) graph.CandidateOutcome {
	st := state.NewState("add a feature")
	st.Test = state.Test{Total: 12, Passed: 12}
	st.Review = state.Review{Issues: make([]belay.Issue, issues)}
	return graph.CandidateOutcome{
		ID:         id,
		Index:      index,
		RunDir:     filepath.Join("candidate-runs", id),
		Status:     state.RunStatusCompleted,
		State:      st,
		Spent:      belay.Usage{USD: usd, InputTokens: 100, OutputTokens: 50},
		Envelope:   state.Envelope{LimitUSD: 3},
		Steps:      6,
		DurationMS: ms,
		Workspace:  belay.Workspace{ID: id, Dir: filepath.Join("/tmp/candidates", id), Ephemeral: true},
		Note:       "completed",
		Provision:  state.ProvisionOK,
	}
}

func TestNodeName(t *testing.T) {
	if got := fanout.New().Name(); got != graph.NodeFanout {
		t.Fatalf("Name() = %q, want %q", got, graph.NodeFanout)
	}
}

// The node waits on other runs, so graph.node_timeout — which caps what one
// node does ITSELF — must not apply to it. Every node of every candidate is
// capped by that same setting already; applying it here as well would kill N
// candidates in the middle of work the run has already paid for.
func TestFanoutSupervisesItsCandidates(t *testing.T) {
	if _, ok := any(fanout.New()).(graph.Supervisor); !ok {
		t.Fatal("the fanout node does not implement graph.Supervisor, so graph.node_timeout would cut its candidates off")
	}
}

// A guard that refuses must refuse before a single candidate runs: every one
// of these is a wiring or configuration bug, and none of them is a reason to
// start — or pay for — a candidate run.
func TestRunConfigGuards(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		nilRuns bool
		want    error
	}{
		{
			name:   "fanout disabled",
			mutate: func(c *config.Config) { c.Fanout.Enabled = false },
			want:   fanout.ErrDisabled,
		},
		{
			name:   "one candidate",
			mutate: func(c *config.Config) { c.Fanout.Candidates = 1 },
			want:   fanout.ErrTooFewCandidates,
		},
		{
			name:   "zero candidates",
			mutate: func(c *config.Config) { c.Fanout.Candidates = 0 },
			want:   fanout.ErrTooFewCandidates,
		},
		{
			name:   "negative candidates",
			mutate: func(c *config.Config) { c.Fanout.Candidates = -3 },
			want:   fanout.ErrTooFewCandidates,
		},
		{
			name:    "no candidate runner",
			mutate:  func(*config.Config) {},
			nilRuns: true,
			want:    graph.ErrNoCandidateRunner,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := enabledConfig(3)
			tt.mutate(&cfg)
			rc, runs, _ := newFixture(t, cfg, state.Budget{})
			if tt.nilRuns {
				rc.CandidateRuns = nil
			}

			res, err := fanout.New().Run(context.Background(), rc)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Run() error = %v, want %v", err, tt.want)
			}
			if res.Patch.Candidates != nil {
				t.Errorf("Run() returned a candidate patch on a refusal: %v", *res.Patch.Candidates)
			}
			if got := runs.callCount(); got != 0 {
				t.Errorf("the candidate runner was called %d time(s) on a refusal", got)
			}
		})
	}
}

// A nil CandidateRuns is a typed error, never a panic — and a hand-built
// RunContext without a logger must not panic either.
func TestRunNilRunnerDoesNotPanic(t *testing.T) {
	layout, _ := newLayout(t)
	rc := newRunContext(enabledConfig(3), layout, nil, state.Budget{})
	rc.Logger = nil // the dispatcher always sets one; a hand-built context may not

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Run() panicked: %v", r)
		}
	}()
	if _, err := fanout.New().Run(context.Background(), rc); !errors.Is(err, graph.ErrNoCandidateRunner) {
		t.Fatalf("Run() error = %v, want graph.ErrNoCandidateRunner", err)
	}
}

func TestRunUnusableRunMetadata(t *testing.T) {
	// The dispatcher always supplies Workspace. If it is ever empty there is
	// nothing to isolate candidates from, and guessing would hand every
	// candidate a directory belay merely happens to be standing in.
	t.Run("context names no workspace", func(t *testing.T) {
		layout, _ := newLayout(t)
		runs := &recordingRunner{}
		rc := newRunContext(enabledConfig(3), layout, runs, state.Budget{})
		rc.Workspace = ""

		_, err := fanout.New().Run(context.Background(), rc)
		if !errors.Is(err, fanout.ErrRunMetadata) {
			t.Fatalf("Run() error = %v, want ErrRunMetadata", err)
		}
		if got := runs.callCount(); got != 0 {
			t.Errorf("the candidate runner was called %d time(s) with no workspace to isolate from", got)
		}
	})
}

// Every candidate works from the parent's approved plan, and its own run
// resolves that plan through its own Layout — so the bytes have to travel in
// the spec. A fanout that started N candidates with no plan would buy N
// identical failures.
func TestRunRefusesWithoutAPlanToHandCandidates(t *testing.T) {
	tests := []struct {
		name  string
		setUp func(t *testing.T, rc *graph.RunContext)
	}{
		{
			name:  "the blackboard records no plan",
			setUp: func(_ *testing.T, rc *graph.RunContext) { rc.State.Plan.Path = "" },
		},
		{
			name: "the plan artifact is gone",
			setUp: func(t *testing.T, rc *graph.RunContext) {
				path, err := rc.Layout.ArtifactPath("plan.md")
				if err != nil {
					t.Fatalf("ArtifactPath: %v", err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove plan: %v", err)
				}
			},
		},
		{
			name: "the plan artifact is blank",
			setUp: func(t *testing.T, rc *graph.RunContext) {
				path, err := rc.Layout.ArtifactPath("plan.md")
				if err != nil {
					t.Fatalf("ArtifactPath: %v", err)
				}
				if err := os.WriteFile(path, []byte("   \n\t\n"), 0o600); err != nil {
					t.Fatalf("blank the plan: %v", err)
				}
			},
		},
		{
			name:  "the plan path escapes the artifacts directory",
			setUp: func(_ *testing.T, rc *graph.RunContext) { rc.State.Plan.Path = "../../etc/passwd" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc, runs, _ := newFixture(t, enabledConfig(3), state.Budget{LimitUSD: 10})
			tt.setUp(t, rc)

			_, err := fanout.New().Run(context.Background(), rc)
			if !errors.Is(err, fanout.ErrNoPlan) {
				t.Fatalf("Run() error = %v, want ErrNoPlan", err)
			}
			if got := runs.callCount(); got != 0 {
				t.Errorf("the candidate runner was called %d time(s) with no plan to give it", got)
			}
		})
	}
}

// The pre-check exists to refuse while refusing is still free: nothing may
// run, and the error must stay recognisable as a budget overrun through this
// node's own wrapping, because that is what the dispatcher turns into a
// pause naming budget.max_usd instead of an ended run.
func TestRunBudgetPreCheckRefusesBeforeAnyCandidateRuns(t *testing.T) {
	tests := []struct {
		name  string
		cfg   func() config.Config
		spent state.Budget
	}{
		{
			name: "usd ceiling",
			cfg:  func() config.Config { return enabledConfig(3) },
			// $4 spent, 3 more candidates at $4 each = $16 against a $10 cap.
			spent: state.Budget{LimitUSD: 10, SpentUSD: 4},
		},
		{
			name: "token ceiling",
			cfg: func() config.Config {
				cfg := enabledConfig(3)
				cfg.Budget.MaxTokens = 1000
				return cfg
			},
			spent: state.Budget{LimitUSD: 10, TokensIn: 300, TokensOut: 100},
		},
		{
			name: "on_exceed warn still refuses, because warn is not consent to spend N times faster",
			cfg: func() config.Config {
				cfg := enabledConfig(3)
				cfg.Budget.OnExceed = config.OnExceedWarn
				return cfg
			},
			spent: state.Budget{LimitUSD: 10, SpentUSD: 4},
		},
		{
			name: "a single cent over the ceiling is still over it",
			cfg: func() config.Config {
				cfg := enabledConfig(2)
				cfg.Budget.MaxUSD = 3.00
				return cfg
			},
			spent: state.Budget{LimitUSD: 3.00, SpentUSD: 1.005},
		},
		{
			name: "a ceiling with nothing left to divide leaves no envelope to hand out",
			cfg: func() config.Config {
				cfg := enabledConfig(3)
				cfg.Budget.MaxUSD = 1.00
				return cfg
			},
			spent: state.Budget{LimitUSD: 1.00, SpentUSD: 1.00},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc, runs, _ := newFixture(t, tt.cfg(), tt.spent)

			res, err := fanout.New().Run(context.Background(), rc)
			if !errors.Is(err, belay.ErrBudgetExceeded) {
				t.Fatalf("Run() error = %v, want it to wrap belay.ErrBudgetExceeded", err)
			}
			var budgetErr *belay.BudgetError
			if !errors.As(err, &budgetErr) {
				t.Fatalf("Run() error = %v, want a *belay.BudgetError recoverable with errors.As", err)
			}
			if got := runs.callCount(); got != 0 {
				t.Fatalf("the candidate runner was called %d time(s) before the budget refusal", got)
			}
			if res.Patch.Candidates != nil {
				t.Errorf("Run() returned a candidate patch on a budget refusal: %v", *res.Patch.Candidates)
			}
		})
	}
}

func TestProjectCandidateCost(t *testing.T) {
	tests := []struct {
		name  string
		spent belay.Usage
		want  belay.Usage
	}{
		{
			name:  "a candidate is projected to cost at least what the run has cost so far",
			spent: belay.Usage{InputTokens: 1200, OutputTokens: 800, USD: 1.25},
			want:  belay.Usage{InputTokens: 1200, OutputTokens: 800, USD: 1.25, Estimated: true},
		},
		{
			name:  "a projection is an extrapolation even when its input was exact",
			spent: belay.Usage{USD: 2, Estimated: false},
			want:  belay.Usage{USD: 2, Estimated: true},
		},
		{
			name:  "nothing spent yet projects nothing",
			spent: belay.Usage{},
			want:  belay.Usage{Estimated: true},
		},
		{
			name:  "negative components clamp to zero rather than making a fanout look free",
			spent: belay.Usage{InputTokens: -5, OutputTokens: -5, USD: -100},
			want:  belay.Usage{Estimated: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fanout.ProjectCandidateCost(tt.spent); got != tt.want {
				t.Fatalf("ProjectCandidateCost(%+v) = %+v, want %+v", tt.spent, got, tt.want)
			}
		})
	}
}

// The ids and the envelopes a re-run asks for are the ones the crashed
// attempt asked for. This is what makes a resumed fanout adopt the candidate
// runs already on disk instead of paying for a second set beside them, and
// it is why an id carries no attempt suffix.
func TestFanoutCandidateIDsAreStableAcrossAttempts(t *testing.T) {
	cfg := enabledConfig(3)
	layout, src := newLayout(t)
	spent := state.Budget{LimitUSD: 10, SpentUSD: 1}

	var first []graph.CandidateSpec
	for _, attempt := range []int{1, 2, 3} {
		runs := &recordingRunner{outcomes: []graph.CandidateOutcome{
			completed(1, "cand-01", 0, 0.5, 100),
			completed(2, "cand-02", 1, 0.5, 200),
			completed(3, "cand-03", 2, 0.5, 300),
		}}
		rc := newRunContext(cfg, layout, runs, spent)
		rc.Attempt = attempt

		if _, err := fanout.New().Run(context.Background(), rc); err != nil {
			t.Fatalf("attempt %d: Run() error = %v", attempt, err)
		}
		specs := runs.lastSpecs(t)
		if attempt == 1 {
			first = specs
			continue
		}
		if diff := cmp.Diff(first, specs); diff != "" {
			t.Errorf("attempt %d asked for different candidates than attempt 1 (-attempt1 +attempt%d):\n%s", attempt, attempt, diff)
		}
	}

	wantIDs := []string{"cand-01", "cand-02", "cand-03"}
	gotIDs := make([]string, 0, len(first))
	for _, s := range first {
		gotIDs = append(gotIDs, s.ID)
		if s.Source != src {
			t.Errorf("spec %q Source = %q, want the run's workspace %q", s.ID, s.Source, src)
		}
	}
	if diff := cmp.Diff(wantIDs, gotIDs); diff != "" {
		t.Errorf("candidate ids mismatch (-want +got):\n%s", diff)
	}
}

// Everything a candidate needs travels in its spec, and nothing that would
// let the runner make a decision of its own does.
func TestFanoutBuildsSpecsFromThePinnedShape(t *testing.T) {
	cfg := enabledConfig(3)
	layout, src := newLayout(t)
	// $10 ceiling, $1 already spent, 3 candidates: $3 each.
	runs := &recordingRunner{outcomes: []graph.CandidateOutcome{
		completed(1, "cand-01", 0, 0, 1),
		completed(2, "cand-02", 0, 0, 1),
		completed(3, "cand-03", 0, 0, 1),
	}}
	rc := newRunContext(cfg, layout, runs, state.Budget{LimitUSD: 10, SpentUSD: 1})

	if _, err := fanout.New().Run(context.Background(), rc); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := []graph.CandidateSpec{
		{ID: "cand-01", Index: 1, Source: src, Envelope: state.Envelope{LimitUSD: 3}, Seed: state.SeedCandidateState(rc.State), Plan: []byte(planText)},
		{ID: "cand-02", Index: 2, Source: src, Envelope: state.Envelope{LimitUSD: 3}, Seed: state.SeedCandidateState(rc.State), Plan: []byte(planText)},
		{ID: "cand-03", Index: 3, Source: src, Envelope: state.Envelope{LimitUSD: 3}, Seed: state.SeedCandidateState(rc.State), Plan: []byte(planText)},
	}
	if diff := cmp.Diff(want, runs.lastSpecs(t)); diff != "" {
		t.Errorf("candidate specs mismatch (-want +got):\n%s", diff)
	}

	// The seed is the parent's goal and plan, and nothing that would have N
	// candidates continue the parent's one agent conversation or pass their
	// first gate on the parent's test results.
	for _, s := range runs.lastSpecs(t) {
		if s.Seed.Code.SessionID != "" {
			t.Errorf("spec %q carries the parent's agent session %q", s.ID, s.Seed.Code.SessionID)
		}
		if s.Seed.Test.Total != 0 {
			t.Errorf("spec %q carries the parent's test results", s.ID)
		}
		if !s.Seed.Plan.Approved || s.Seed.Goal != rc.Goal {
			t.Errorf("spec %q does not carry the goal and the approved plan: %+v", s.ID, s.Seed)
		}
	}
}

// The Result is a function of the candidates' end states and of nothing
// else. Two attempts handed the same outcomes must agree down to the Note,
// or a resumed run could report something the crashed attempt did not.
func TestFanoutResultIsAFunctionOfEndStatesNotTheAttempt(t *testing.T) {
	cfg := enabledConfig(3)
	layout, _ := newLayout(t)
	outcomes := []graph.CandidateOutcome{
		completed(1, "cand-01", 3, 0.40, 900),
		completed(2, "cand-02", 1, 0.55, 300),
		completed(3, "cand-03", 0, 0.25, 700),
	}

	var first graph.Result
	for _, attempt := range []int{1, 2} {
		rc := newRunContext(cfg, layout, &recordingRunner{outcomes: outcomes}, state.Budget{LimitUSD: 10, SpentUSD: 1})
		rc.Attempt = attempt
		rc.Step = 7 + attempt

		res, err := fanout.New().Run(context.Background(), rc)
		if err != nil {
			t.Fatalf("attempt %d: Run() error = %v", attempt, err)
		}
		if attempt == 1 {
			first = res
			continue
		}
		if diff := cmp.Diff(first, res); diff != "" {
			t.Errorf("attempt 2's Result differs from attempt 1's (-attempt1 +attempt2):\n%s", diff)
		}
	}
	if first.Note == "" {
		t.Error("Note is empty, want a summary of how the candidates ended")
	}
}

// Every candidate's end state becomes one state.Candidate, in index order,
// whatever order the outcomes arrive in: join compares them by ID and the
// blackboard is an absolute set, so a list that reordered itself between two
// attempts would make the run's record of its own candidates unstable.
func TestFanoutRecordsOutcomesAsCandidatesInIndexOrder(t *testing.T) {
	cfg := enabledConfig(4)
	layout, _ := newLayout(t)

	failed := completed(2, "cand-02", 0, 0.30, 400)
	failed.Status = state.RunStatusFailed
	failed.State.Test = state.Test{Total: 8, Passed: 5, Failed: 3}
	failed.Note = "the fix loop gave up"

	refused := graph.CandidateOutcome{
		ID:        "cand-03",
		Index:     3,
		RunDir:    filepath.Join("candidate-runs", "cand-03"),
		Status:    state.RunStatusFailed,
		Provision: state.ProvisionRefused,
		Note:      "the source tree is not a git repository",
	}

	aborted := completed(4, "cand-04", 2, 1.00, 250)
	aborted.Status = state.RunStatusAborted
	aborted.Spent.Estimated = true
	aborted.Note = "spent its envelope"

	// Deliberately out of index order: the runner returns outcomes as its
	// goroutines finished.
	runs := &recordingRunner{outcomes: []graph.CandidateOutcome{
		aborted, refused, completed(1, "cand-01", 2, 0.45, 800), failed,
	}}
	rc := newRunContext(cfg, layout, runs, state.Budget{LimitUSD: 10, SpentUSD: 1})

	res, err := fanout.New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Next != graph.NodeJoin {
		t.Errorf("Next = %q, want %q", res.Next, graph.NodeJoin)
	}
	if res.Status != journal.StatusOK {
		t.Errorf("Status = %v, want %v", res.Status, journal.StatusOK)
	}
	if err := res.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
	if res.Patch.Candidates == nil {
		t.Fatal("Patch.Candidates is nil, want one entry per candidate")
	}

	want := []state.Candidate{
		{
			ID: "cand-01", Dir: filepath.Join("/tmp/candidates", "cand-01"), TestPassed: true, IssueCount: 2,
			Ephemeral: true, Outcome: "completed", RunDir: filepath.Join("candidate-runs", "cand-01"),
			SpentUSD: 0.45, DurationMS: 800, Note: "completed",
		},
		{
			ID: "cand-02", Dir: filepath.Join("/tmp/candidates", "cand-02"), TestPassed: false, IssueCount: 0,
			Ephemeral: true, Outcome: "failed", RunDir: filepath.Join("candidate-runs", "cand-02"),
			SpentUSD: 0.30, DurationMS: 400, Note: "the fix loop gave up",
		},
		{
			ID: "cand-03", Outcome: "error", RunDir: filepath.Join("candidate-runs", "cand-03"),
			Note: "the source tree is not a git repository",
		},
		{
			ID: "cand-04", Dir: filepath.Join("/tmp/candidates", "cand-04"), TestPassed: true, IssueCount: 2,
			Ephemeral: true, Outcome: "aborted", RunDir: filepath.Join("candidate-runs", "cand-04"),
			SpentUSD: 1.00, Estimated: true, DurationMS: 250, Note: "spent its envelope",
		},
	}
	if diff := cmp.Diff(want, *res.Patch.Candidates); diff != "" {
		t.Errorf("candidates mismatch (-want +got):\n%s", diff)
	}

	// The whole fanout's bill, summed from the candidates' own ledgers, and
	// estimated because one of them was.
	wantUsage := belay.Usage{InputTokens: 300, OutputTokens: 150, USD: 1.75, Estimated: true}
	if diff := cmp.Diff(wantUsage, res.Usage); diff != "" {
		t.Errorf("Usage mismatch (-want +got):\n%s", diff)
	}

	// The node reports state through its Patch and nothing else: ADR-0002.
	if !fileMissing(t, layout.StatePath()) || !fileMissing(t, layout.JournalPath()) {
		t.Error("fanout wrote state.json or the journal; only the dispatcher may")
	}
}

func fileMissing(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

// A candidate whose provisioning hit a machine fault — a full disk, a git
// lock, a panic — has no durable end state, and a resume could still give it
// one. Settling a verdict about it would make a transient fault permanent,
// so the fanout pauses with NO Patch at all: state.Candidates is an absolute
// set, and half a fanout recorded in it is a lie join would act on.
func TestFanoutPausesWhenACandidateHasNoDurableEndState(t *testing.T) {
	diskFull := errors.New("write candidate-runs/cand-03: no space left on device")

	tests := []struct {
		name     string
		outcomes []graph.CandidateOutcome
		wantNote string
	}{
		{
			name: "a machine fault while provisioning",
			outcomes: []graph.CandidateOutcome{
				completed(1, "cand-01", 0, 0.4, 100),
				completed(2, "cand-02", 1, 0.4, 200),
				{ID: "cand-03", Index: 3, Err: diskFull},
			},
			wantNote: "no space left on device",
		},
		{
			name: "a candidate run that is still open",
			outcomes: []graph.CandidateOutcome{
				completed(1, "cand-01", 0, 0.4, 100),
				completed(2, "cand-02", 1, 0.4, 200),
				{ID: "cand-03", Index: 3, Status: state.RunStatusRunning},
			},
			wantNote: "cand-03",
		},
		{
			name: "a candidate the runner paused",
			outcomes: []graph.CandidateOutcome{
				completed(1, "cand-01", 0, 0.4, 100),
				completed(2, "cand-02", 1, 0.4, 200),
				{ID: "cand-03", Index: 3, Status: state.RunStatusPaused},
			},
			wantNote: "cand-03",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := enabledConfig(3)
			layout, _ := newLayout(t)
			rc := newRunContext(cfg, layout, &recordingRunner{outcomes: tt.outcomes}, state.Budget{LimitUSD: 10, SpentUSD: 1})

			res, err := fanout.New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run() error = %v, want a paused Result: a fault a resume could fix is not a verdict", err)
			}
			if res.Status != journal.StatusPaused {
				t.Errorf("Status = %v, want %v", res.Status, journal.StatusPaused)
			}
			if diff := cmp.Diff(state.Patch{}, res.Patch); diff != "" {
				t.Errorf("a paused fanout must carry no Patch (-want +got):\n%s", diff)
			}
			if !strings.Contains(res.Note, tt.wantNote) {
				t.Errorf("Note = %q, want it to name %q", res.Note, tt.wantNote)
			}
			// The parent's ledger must not learn the candidates' spend at a
			// pause: the resume settles the whole sum, which already
			// includes it, and recording it twice would inflate the run's
			// bill once per interruption.
			if (res.Usage != belay.Usage{}) {
				t.Errorf("Usage = %+v on a pause, want zero", res.Usage)
			}
		})
	}
}

// A refusal IS durable — the isolator will never make a workspace for this
// candidate here — so it settles rather than pausing, and join finds it
// ineligible.
func TestFanoutSettlesWhenEveryCandidateIsRefused(t *testing.T) {
	cfg := enabledConfig(2)
	layout, _ := newLayout(t)
	refusal := func(index int, id string) graph.CandidateOutcome {
		return graph.CandidateOutcome{
			ID: id, Index: index, RunDir: filepath.Join("candidate-runs", id),
			Provision: state.ProvisionRefused, Note: "the source tree is not one this isolator supports",
		}
	}
	rc := newRunContext(cfg, layout, &recordingRunner{
		outcomes: []graph.CandidateOutcome{refusal(1, "cand-01"), refusal(2, "cand-02")},
	}, state.Budget{LimitUSD: 10, SpentUSD: 1})

	res, err := fanout.New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Status != journal.StatusOK || res.Next != graph.NodeJoin {
		t.Fatalf("Result = {Status: %v, Next: %q}, want a settled route to join", res.Status, res.Next)
	}
	if res.Patch.Candidates == nil || len(*res.Patch.Candidates) != 2 {
		t.Fatalf("Patch.Candidates = %v, want both refusals recorded", res.Patch.Candidates)
	}
	for _, c := range *res.Patch.Candidates {
		if c.Outcome != "error" {
			t.Errorf("candidate %q Outcome = %q, want %q", c.ID, c.Outcome, "error")
		}
	}
}

// The runner reporting that it could not carry the fanout out at all is not
// a verdict about the run's code: ADR-0002 makes that an error, and the
// candidates' own spend stays where it is durable, in their own journals.
func TestFanoutReportsARunnerFailure(t *testing.T) {
	boom := errors.New("claim cand-01: candidate run directory is not one belay created")
	cfg := enabledConfig(3)
	layout, _ := newLayout(t)
	rc := newRunContext(cfg, layout, &recordingRunner{err: boom}, state.Budget{LimitUSD: 10, SpentUSD: 1})

	res, err := fanout.New().Run(context.Background(), rc)
	if !errors.Is(err, boom) {
		t.Fatalf("Run() error = %v, want it to wrap %v", err, boom)
	}
	if res.Patch.Candidates != nil {
		t.Errorf("Patch.Candidates = %v, want nil when no verdict was reached", *res.Patch.Candidates)
	}
}

// An outcome the node cannot place is a broken seam, not a verdict: a
// candidate silently dropped from an absolute set would leave join choosing
// between fewer candidates than the run paid for.
func TestFanoutRejectsOutcomesItCannotPlace(t *testing.T) {
	tests := []struct {
		name     string
		outcomes []graph.CandidateOutcome
	}{
		{
			name:     "fewer outcomes than candidates",
			outcomes: []graph.CandidateOutcome{completed(1, "cand-01", 0, 0.1, 10)},
		},
		{
			name: "two outcomes for one index",
			outcomes: []graph.CandidateOutcome{
				completed(1, "cand-01", 0, 0.1, 10),
				completed(1, "cand-02", 0, 0.1, 10),
			},
		},
		{
			name: "an index no candidate has",
			outcomes: []graph.CandidateOutcome{
				completed(1, "cand-01", 0, 0.1, 10),
				completed(9, "cand-09", 0, 0.1, 10),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := enabledConfig(2)
			layout, _ := newLayout(t)
			rc := newRunContext(cfg, layout, &recordingRunner{outcomes: tt.outcomes}, state.Budget{LimitUSD: 10, SpentUSD: 1})

			res, err := fanout.New().Run(context.Background(), rc)
			if !errors.Is(err, fanout.ErrRunnerContract) {
				t.Fatalf("Run() error = %v, want ErrRunnerContract", err)
			}
			if res.Patch.Candidates != nil {
				t.Errorf("Patch.Candidates = %v, want nil", *res.Patch.Candidates)
			}
		})
	}
}

// A cancelled context stops the fanout before it claims anything, rather
// than starting N candidate runs the caller has already asked belay to stop.
func TestRunCancelledBeforeAnythingIsClaimed(t *testing.T) {
	rc, runs, _ := newFixture(t, enabledConfig(3), state.Budget{LimitUSD: 10, SpentUSD: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fanout.New().Run(ctx, rc)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want it to wrap context.Canceled", err)
	}
	if got := runs.callCount(); got != 0 {
		t.Errorf("the candidate runner was called %d time(s) on a cancelled context", got)
	}
}
