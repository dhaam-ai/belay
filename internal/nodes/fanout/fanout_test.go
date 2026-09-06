package fanout_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dhaam-ai/belay/internal/budget"
	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/fanout"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
	"github.com/google/go-cmp/cmp"
)

const testRunID = "20260101T000000Z-abcdef123456"

// enabledConfig is a valid fanout configuration with both budget ceilings
// under the test's control: MaxTokens is disabled so a USD assertion
// cannot accidentally trip the token ceiling instead.
func enabledConfig(candidates int) config.Config {
	cfg := config.Default()
	cfg.Fanout.Enabled = true
	cfg.Fanout.Candidates = candidates
	cfg.Budget.MaxUSD = 10
	cfg.Budget.MaxTokens = 0
	return cfg
}

// newLayout returns a Layout over a fresh run directory, plus the source
// repository path a candidate would be copied from.
// newLayout returns a Layout rooted at the repository belay is operating on,
// and that repository's path.
//
// The run directory lives inside the workspace (<repo>/.belay/runs/<id>), so
// Layout.WorkspaceDir -- which is what the dispatcher puts on
// RunContext.Workspace, and therefore what fanout copies from -- is the repo
// itself. An earlier version rooted the Layout one level above the repo and
// relied on the manifest to name the real source separately; the two could
// then disagree, which is exactly what carrying the workspace on the context
// removes.
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

func newRunContext(cfg config.Config, layout state.Layout, iso belay.Isolator, spent state.Budget) *graph.RunContext {
	return &graph.RunContext{
		Goal:      "add a feature",
		State:     state.NewState("add a feature"),
		Config:    cfg,
		Layout:    layout,
		Workspace: layout.WorkspaceDir(),
		RunID:     layout.RunID(),
		Budget:    snapshot(spent),
		NodeName:  graph.NodeFanout,
		Step:      7,
		Attempt:   1,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Isolator:  iso,
	}
}

// newFixture wires the common case: a valid layout, a manifest recording
// spent, and a scriptable isolator.
func newFixture(t *testing.T, cfg config.Config, spent state.Budget) (*graph.RunContext, *belaytest.FakeIsolator, string) {
	t.Helper()
	layout, src := newLayout(t)
	iso := &belaytest.FakeIsolator{}
	return newRunContext(cfg, layout, iso, spent), iso, src
}

func TestNodeName(t *testing.T) {
	if got := fanout.New().Name(); got != graph.NodeFanout {
		t.Fatalf("Name() = %q, want %q", got, graph.NodeFanout)
	}
}

// A guard that refuses must refuse before the isolator is touched: every
// one of these is a wiring or configuration bug, and none of them is a
// reason to create — or pay for — a workspace.
func TestRunConfigGuards(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*config.Config)
		nilIsolate bool
		want       error
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
			name:       "nil isolator",
			mutate:     func(*config.Config) {},
			nilIsolate: true,
			want:       fanout.ErrNoIsolator,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := enabledConfig(3)
			tt.mutate(&cfg)
			rc, iso, _ := newFixture(t, cfg, state.Budget{})
			if tt.nilIsolate {
				rc.Isolator = nil
			}

			res, err := fanout.New().Run(context.Background(), rc)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Run() error = %v, want %v", err, tt.want)
			}
			if res.Patch.Candidates != nil {
				t.Errorf("Run() returned a candidate patch on a refusal: %v", *res.Patch.Candidates)
			}
			if calls := iso.CreateCalls(); len(calls) != 0 {
				t.Errorf("isolator Create called %d time(s) on a refusal: %v", len(calls), calls)
			}
		})
	}
}

// A nil Isolator is a typed error, never a panic.
func TestRunNilIsolatorDoesNotPanic(t *testing.T) {
	layout, _ := newLayout(t)
	cfg := enabledConfig(3)
	rc := newRunContext(cfg, layout, nil, state.Budget{})
	rc.Logger = nil // the dispatcher always sets one; a hand-built context may not

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Run() panicked: %v", r)
		}
	}()
	if _, err := fanout.New().Run(context.Background(), rc); !errors.Is(err, fanout.ErrNoIsolator) {
		t.Fatalf("Run() error = %v, want ErrNoIsolator", err)
	}
}

func TestRunUnusableRunMetadata(t *testing.T) {
	// The dispatcher always supplies Workspace. If it is ever empty there is
	// nothing to copy candidates from, and guessing would hand every
	// candidate a directory belay merely happens to be standing in.
	t.Run("context names no workspace", func(t *testing.T) {
		layout, _ := newLayout(t)
		iso := &belaytest.FakeIsolator{}
		rc := newRunContext(enabledConfig(3), layout, iso, state.Budget{})
		rc.Workspace = ""

		_, err := fanout.New().Run(context.Background(), rc)
		if !errors.Is(err, fanout.ErrRunMetadata) {
			t.Fatalf("Run() error = %v, want ErrRunMetadata", err)
		}
		if calls := iso.CreateCalls(); len(calls) != 0 {
			t.Errorf("isolator Create called %d time(s) with no workspace to copy from", len(calls))
		}
	})
}

// The pre-check exists to refuse while refusing is still free: nothing may
// be created, and the error must stay recognisable as a budget overrun
// through this node's own wrapping.
func TestRunBudgetPreCheckRefusesBeforeAnyCreate(t *testing.T) {
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
				cfg.Budget.MaxUSD = 0 // uncapped in dollars, capped in tokens
				cfg.Budget.MaxTokens = 1000
				return cfg
			},
			spent: state.Budget{TokensIn: 300, TokensOut: 100},
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc, iso, _ := newFixture(t, tt.cfg(), tt.spent)

			res, err := fanout.New().Run(context.Background(), rc)
			if !errors.Is(err, belay.ErrBudgetExceeded) {
				t.Fatalf("Run() error = %v, want it to wrap belay.ErrBudgetExceeded", err)
			}
			var budgetErr *belay.BudgetError
			if !errors.As(err, &budgetErr) {
				t.Fatalf("Run() error = %v, want a *belay.BudgetError recoverable with errors.As", err)
			}
			if calls := iso.CreateCalls(); len(calls) != 0 {
				t.Fatalf("isolator Create called %d time(s) before the budget refusal: %v", len(calls), calls)
			}
			if leaked := iso.Leaked(); len(leaked) > 0 {
				t.Errorf("workspaces leaked by a refusal that should have created none: %v", leaked)
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
			name:  "a candidate is projected to cost what the run has cost so far",
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

// ephemeralIsolator scripts an isolator that behaves like dircopy: every
// candidate gets its own ephemeral directory named after its id, under a
// candidates root beside the source repository.
func ephemeralIsolator(t *testing.T, onCreate func(calls int)) *belaytest.FakeIsolator {
	t.Helper()
	iso := &belaytest.FakeIsolator{}
	iso.CreateFunc = func(_ context.Context, src, id string) (belay.Workspace, error) {
		if onCreate != nil {
			onCreate(len(iso.CreateCalls()))
		}
		return belay.Workspace{ID: id, Dir: filepath.Join(src+"-candidates", id), Ephemeral: true}, nil
	}
	return iso
}

func candidateDirs(t *testing.T, src string, ids ...string) []state.Candidate {
	t.Helper()
	out := make([]state.Candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, state.Candidate{ID: id, Dir: filepath.Join(src+"-candidates", id)})
	}
	return out
}

func TestRunCreatesAndRecordsCandidates(t *testing.T) {
	cfg := enabledConfig(3)
	layout, src := newLayout(t)
	spent := state.Budget{LimitUSD: 10, SpentUSD: 1}
	iso := ephemeralIsolator(t, nil)
	rc := newRunContext(cfg, layout, iso, spent)

	res, err := fanout.New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
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
	if (res.Usage != belay.Usage{}) {
		t.Errorf("Usage = %+v, want zero: fanout calls no agent", res.Usage)
	}
	if res.Note == "" {
		t.Error("Note is empty, want a human-readable summary")
	}

	if res.Patch.Candidates == nil {
		t.Fatal("Patch.Candidates is nil, want the created candidates")
	}
	want := candidateDirs(t, src, "cand-01-a1", "cand-02-a1", "cand-03-a1")
	if diff := cmp.Diff(want, *res.Patch.Candidates); diff != "" {
		t.Errorf("candidates mismatch (-want +got):\n%s", diff)
	}

	// Every candidate is copied from the repository the manifest names, and
	// each id is one Layout accepts as a candidate directory.
	wantCalls := []belaytest.CreateCall{
		{Src: src, ID: "cand-01-a1"},
		{Src: src, ID: "cand-02-a1"},
		{Src: src, ID: "cand-03-a1"},
	}
	if diff := cmp.Diff(wantCalls, iso.CreateCalls()); diff != "" {
		t.Errorf("Create calls mismatch (-want +got):\n%s", diff)
	}
	for _, c := range *res.Patch.Candidates {
		if _, err := layout.CandidateDir(c.ID); err != nil {
			t.Errorf("candidate id %q is not a legal candidate directory: %v", c.ID, err)
		}
	}

	if calls := iso.DestroyCalls(); len(calls) != 0 {
		t.Errorf("Destroy called %d time(s) on a successful fanout: %v", len(calls), calls)
	}
	if leaked := iso.Leaked(); len(leaked) != 3 {
		// A successful fanout deliberately hands its workspaces on to join;
		// they are only "leaked" from FakeIsolator's point of view.
		t.Errorf("live workspaces = %d, want 3 handed to join", len(leaked))
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

// Candidate 4 of 5 failing must leave nothing behind: the three already
// created are destroyed, newest first.
func TestRunPartialFailureDestroysEverythingCreated(t *testing.T) {
	cfg := enabledConfig(5)
	layout, _ := newLayout(t)
	spent := state.Budget{LimitUSD: 100, SpentUSD: 1}

	boom := errors.New("disk full")
	iso := &belaytest.FakeIsolator{
		CreateFunc: func(_ context.Context, src, id string) (belay.Workspace, error) {
			return belay.Workspace{ID: id, Dir: filepath.Join(src+"-candidates", id), Ephemeral: true}, nil
		},
	}
	failing := &belaytest.FakeIsolator{}
	failing.CreateFunc = func(ctx context.Context, src, id string) (belay.Workspace, error) {
		if len(failing.CreateCalls()) == 4 {
			return belay.Workspace{}, boom
		}
		return iso.Create(ctx, src, id)
	}
	failing.DestroyFunc = func(ctx context.Context, ws belay.Workspace) error { return iso.Destroy(ctx, ws) }

	res, err := fanout.New().Run(context.Background(), newRunContext(cfg, layout, failing, spent))
	if !errors.Is(err, boom) {
		t.Fatalf("Run() error = %v, want it to wrap %v", err, boom)
	}
	if res.Patch.Candidates != nil {
		t.Errorf("Patch.Candidates = %v, want nil on a failed fanout", *res.Patch.Candidates)
	}
	if got := len(failing.CreateCalls()); got != 4 {
		t.Errorf("Create called %d time(s), want 4 (stop at the first failure, do not create the 5th)", got)
	}

	var destroyed []string
	for _, ws := range failing.DestroyCalls() {
		destroyed = append(destroyed, ws.ID)
	}
	want := []string{"cand-03-a1", "cand-02-a1", "cand-01-a1"}
	if diff := cmp.Diff(want, destroyed); diff != "" {
		t.Errorf("destroyed candidates mismatch (-want +got, newest first):\n%s", diff)
	}
	if leaked := iso.Leaked(); len(leaked) > 0 {
		t.Errorf("workspaces leaked after a partial failure: %v", leaked)
	}
}

// A destroy that itself fails must be reported, not swallowed: the disk it
// occupies is never coming back on its own.
func TestRunUnwindReportsDestroyFailures(t *testing.T) {
	cfg := enabledConfig(3)
	layout, _ := newLayout(t)
	spent := state.Budget{LimitUSD: 100, SpentUSD: 1}

	createBoom := errors.New("create failed")
	destroyBoom := errors.New("destroy failed")
	iso := &belaytest.FakeIsolator{}
	iso.CreateFunc = func(_ context.Context, src, id string) (belay.Workspace, error) {
		if len(iso.CreateCalls()) == 2 {
			return belay.Workspace{}, createBoom
		}
		return belay.Workspace{ID: id, Dir: filepath.Join(src+"-candidates", id), Ephemeral: true}, nil
	}
	iso.DestroyFunc = func(context.Context, belay.Workspace) error { return destroyBoom }

	_, err := fanout.New().Run(context.Background(), newRunContext(cfg, layout, iso, spent))
	if !errors.Is(err, createBoom) {
		t.Errorf("Run() error = %v, want it to wrap the create failure %v", err, createBoom)
	}
	if !errors.Is(err, destroyBoom) {
		t.Errorf("Run() error = %v, want it to also wrap the destroy failure %v", err, destroyBoom)
	}
}

func TestRunCancellationCleansUp(t *testing.T) {
	tests := []struct {
		name        string
		cancelAfter int // cancel once this many Create calls have started; 0 cancels up front
		wantCreated int
	}{
		{name: "cancelled before the first candidate", cancelAfter: 0, wantCreated: 0},
		{name: "cancelled midway", cancelAfter: 2, wantCreated: 2},
		{name: "cancelled after the last candidate", cancelAfter: 3, wantCreated: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := enabledConfig(3)
			layout, _ := newLayout(t)
			spent := state.Budget{LimitUSD: 100, SpentUSD: 1}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			iso := ephemeralIsolator(t, func(calls int) {
				if calls == tt.cancelAfter {
					cancel()
				}
			})
			if tt.cancelAfter == 0 {
				cancel()
			}

			res, err := fanout.New().Run(ctx, newRunContext(cfg, layout, iso, spent))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run() error = %v, want it to wrap context.Canceled", err)
			}
			if res.Patch.Candidates != nil {
				t.Errorf("Patch.Candidates = %v, want nil on a cancelled fanout", *res.Patch.Candidates)
			}
			if got := len(iso.CreateCalls()); got != tt.wantCreated {
				t.Errorf("Create called %d time(s), want %d", got, tt.wantCreated)
			}
			if got := len(iso.DestroyCalls()); got != tt.wantCreated {
				t.Errorf("Destroy called %d time(s), want %d", got, tt.wantCreated)
			}
			if leaked := iso.Leaked(); len(leaked) > 0 {
				t.Errorf("workspaces leaked by a cancelled fanout: %v", leaked)
			}
		})
	}
}

// A successful Create that returns no directory is a broken isolator, and
// the workspace it claims to have made is still unwound.
func TestRunRejectsWorkspaceWithoutDir(t *testing.T) {
	cfg := enabledConfig(3)
	layout, _ := newLayout(t)
	spent := state.Budget{LimitUSD: 100, SpentUSD: 1}

	iso := &belaytest.FakeIsolator{
		CreateResponses: []belay.Workspace{{ID: "cand-01-a1"}},
	}
	_, err := fanout.New().Run(context.Background(), newRunContext(cfg, layout, iso, spent))
	if !errors.Is(err, fanout.ErrIsolatorContract) {
		t.Fatalf("Run() error = %v, want ErrIsolatorContract", err)
	}
	if got := len(iso.CreateCalls()); got != 1 {
		t.Errorf("Create called %d time(s), want 1 (stop at the first unusable workspace)", got)
	}
	if leaked := iso.Leaked(); len(leaked) > 0 {
		t.Errorf("workspaces leaked: %v", leaked)
	}
}

// A re-run after a crash must not collide with the previous attempt's
// candidate directories, which are still on disk.
func TestRunCandidateIDsAreUniquePerAttempt(t *testing.T) {
	cfg := enabledConfig(2)
	layout, _ := newLayout(t)
	spent := state.Budget{LimitUSD: 100, SpentUSD: 1}

	seen := map[string]bool{}
	for _, attempt := range []int{0, 1, 2} {
		iso := ephemeralIsolator(t, nil)
		rc := newRunContext(cfg, layout, iso, spent)
		rc.Attempt = attempt

		res, err := fanout.New().Run(context.Background(), rc)
		if err != nil {
			t.Fatalf("attempt %d: Run() error = %v", attempt, err)
		}
		for _, c := range *res.Patch.Candidates {
			if attempt > 1 && seen[c.ID] {
				t.Errorf("attempt %d reused candidate id %q from an earlier attempt", attempt, c.ID)
			}
			seen[c.ID] = true
		}
	}
	// Attempt 0 is normalised to attempt 1, so three attempts produce two
	// distinct id sets of two candidates each.
	if len(seen) != 4 {
		t.Errorf("distinct candidate ids = %d, want 4: %v", len(seen), seen)
	}
}

// Cleanup must not be handed the very context whose cancellation caused it.
// A real isolator refuses work on a cancelled context — dircopy's Destroy
// returns immediately on one — so reusing it would leak exactly the
// workspaces this path exists to reclaim.
func TestRunCleanupUsesAnUncancelledContext(t *testing.T) {
	cfg := enabledConfig(3)
	layout, _ := newLayout(t)
	spent := state.Budget{LimitUSD: 100, SpentUSD: 1}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var destroyCtxErrs []error
	iso := ephemeralIsolator(t, func(calls int) {
		if calls == 2 {
			cancel()
		}
	})
	iso.DestroyFunc = func(ctx context.Context, _ belay.Workspace) error {
		mu.Lock()
		defer mu.Unlock()
		destroyCtxErrs = append(destroyCtxErrs, ctx.Err())
		return ctx.Err() // dircopy behaves this way: a cancelled context destroys nothing
	}

	_, err := fanout.New().Run(ctx, newRunContext(cfg, layout, iso, spent))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want it to wrap context.Canceled", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(destroyCtxErrs) != 2 {
		t.Fatalf("Destroy called %d time(s), want 2", len(destroyCtxErrs))
	}
	for i, ctxErr := range destroyCtxErrs {
		if ctxErr != nil {
			t.Errorf("Destroy call %d saw a cancelled context (%v); cleanup must outlive the cancellation that triggered it", i, ctxErr)
		}
	}
	if leaked := iso.Leaked(); len(leaked) > 0 {
		t.Errorf("workspaces leaked: %v", leaked)
	}
}
