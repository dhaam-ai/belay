package approve_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/nodes/approve"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
)

// testRunID is fixed so assertions can name the exact resume command a human
// would be told to type.
const testRunID = "20260905T101112Z-ab12cd34"

const planBody = "# Plan\n\n1. Add the thing.\n2. Test the thing.\n"

// newRC builds a RunContext backed by a real run directory. plan is written
// through RunContext.WriteArtifact — the same call the plan node makes — so
// State.Plan.Path holds exactly what a real run would put there.
func newRC(t *testing.T, plan string) *graph.RunContext {
	t.Helper()

	layout, err := state.NewLayout(t.TempDir(), testRunID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	rc := &graph.RunContext{
		Goal:      "add the thing",
		State:     state.NewState("add the thing"),
		Config:    config.Default(),
		Layout:    layout,
		Workspace: layout.WorkspaceDir(),
		RunID:     layout.RunID(),
		NodeName:  graph.NodeApprove,
		Step:      2,
		Attempt:   1,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if plan != "" {
		rel, werr := rc.WriteArtifact("plan.md", []byte(plan))
		if werr != nil {
			t.Fatalf("WriteArtifact: %v", werr)
		}
		rc.State.Plan.Path = rel
	}
	return rc
}

// run executes the gate and fails the test if it returned an error.
func run(t *testing.T, rc *graph.RunContext) graph.Result {
	t.Helper()
	res, err := approve.New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if verr := res.Validate(); verr != nil {
		t.Fatalf("Run returned a Result the dispatcher cannot act on: %v", verr)
	}
	return res
}

func TestName(t *testing.T) {
	if got, want := approve.New().Name(), graph.NodeApprove; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
}

// The whole point of the node: an unapproved plan pauses, and the pause note
// tells the human exactly what to open and what to type.
func TestUnapprovedPauses(t *testing.T) {
	rc := newRC(t, planBody)
	res := run(t, rc)

	if got, want := res.Status, journal.StatusPaused; got != want {
		t.Errorf("Status = %v, want %v", got, want)
	}
	if res.Next != "" {
		t.Errorf("Next = %q, want empty: a paused gate routes nowhere", res.Next)
	}

	wantPath := filepath.Join(rc.Layout.ArtifactsDir(), "plan.md")
	if !filepath.IsAbs(wantPath) {
		t.Fatalf("test setup: %q is not absolute", wantPath)
	}
	if !strings.Contains(res.Note, wantPath) {
		t.Errorf("Note does not name the absolute plan path %q:\n%s", wantPath, res.Note)
	}
	if want := "belay resume " + testRunID; !strings.Contains(res.Note, want) {
		t.Errorf("Note does not carry the exact resume command %q:\n%s", want, res.Note)
	}
	if strings.Contains(res.Note, "\n") {
		t.Errorf("Note must stay one line so it reads in a timeline:\n%q", res.Note)
	}
}

// TestPauseNoteIsExact pins the note verbatim. It is the product's most
// visible sentence — for many users it is the only thing belay says between
// planning and coding — so it should change deliberately, not by accident.
func TestPauseNoteIsExact(t *testing.T) {
	rc := newRC(t, planBody)
	res := run(t, rc)

	want := "Plan is waiting for your approval. Review or edit " +
		filepath.Join(rc.Layout.ArtifactsDir(), "plan.md") +
		", then continue with: belay resume " + testRunID

	if res.Note != want {
		t.Errorf("pause note drifted.\n got: %s\nwant: %s", res.Note, want)
	}
	t.Logf("the note a human sees:\n%s", res.Note)
}

// A node that calls an agent reports Usage; this one calls nothing, so a
// non-zero Usage here would put phantom spend on the budget ledger.
func TestUsageIsZero(t *testing.T) {
	if got := run(t, newRC(t, planBody)).Usage; got != (belay.Usage{}) {
		t.Errorf("Usage = %+v, want the zero Usage: the gate calls no agent", got)
	}
}

func TestPlanErrors(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T, rc *graph.RunContext)
		wantPath func(rc *graph.RunContext) string
		wantIs   error
	}{
		{
			name: "no plan recorded in state",
			setup: func(_ *testing.T, rc *graph.RunContext) {
				rc.State.Plan.Path = ""
			},
			wantPath: func(*graph.RunContext) string { return "" },
		},
		{
			name: "plan artifact is missing from disk",
			setup: func(t *testing.T, rc *graph.RunContext) {
				t.Helper()
				if err := os.Remove(filepath.Join(rc.Layout.ArtifactsDir(), "plan.md")); err != nil {
					t.Fatalf("remove plan: %v", err)
				}
			},
			wantPath: func(rc *graph.RunContext) string {
				return filepath.Join(rc.Layout.ArtifactsDir(), "plan.md")
			},
			wantIs: fs.ErrNotExist,
		},
		{
			name: "plan artifact is empty",
			setup: func(t *testing.T, rc *graph.RunContext) {
				t.Helper()
				p := filepath.Join(rc.Layout.ArtifactsDir(), "plan.md")
				if err := os.WriteFile(p, []byte("   \n\t\n"), 0o600); err != nil {
					t.Fatalf("truncate plan: %v", err)
				}
			},
			wantPath: func(rc *graph.RunContext) string {
				return filepath.Join(rc.Layout.ArtifactsDir(), "plan.md")
			},
		},
		{
			name: "plan path escapes the artifacts directory",
			setup: func(_ *testing.T, rc *graph.RunContext) {
				rc.State.Plan.Path = "../../../etc/passwd"
			},
			wantPath: func(*graph.RunContext) string { return "../../../etc/passwd" },
		},
		{
			name: "plan path points outside artifacts",
			setup: func(_ *testing.T, rc *graph.RunContext) {
				rc.State.Plan.Path = "nodes/002-approve/plan.md"
			},
			wantPath: func(*graph.RunContext) string { return "nodes/002-approve/plan.md" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Approved, so a bug that pauses instead of erroring is visible.
			rc := newRC(t, planBody)
			rc.State.Plan.Approved = true
			tt.setup(t, rc)

			res, err := approve.New().Run(context.Background(), rc)
			if err == nil {
				t.Fatalf("Run() = %+v, nil; want an error, not a pause", res)
			}
			if !errors.Is(err, approve.ErrPlanUnavailable) {
				t.Errorf("errors.Is(err, ErrPlanUnavailable) = false; err = %v", err)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("errors.Is(err, %v) = false; err = %v", tt.wantIs, err)
			}

			var perr *approve.PlanError
			if !errors.As(err, &perr) {
				t.Fatalf("errors.As(err, *PlanError) = false; err = %v", err)
			}
			if got, want := perr.Path, tt.wantPath(rc); got != want {
				t.Errorf("PlanError.Path = %q, want %q", got, want)
			}
			if !strings.Contains(err.Error(), tt.wantPath(rc)) && tt.wantPath(rc) != "" {
				t.Errorf("error message does not name the path %q: %v", tt.wantPath(rc), err)
			}
		})
	}
}

// A cancelled context must come back as a cancellation, not as a pause the
// dispatcher would journal as a legitimate approval wait.
func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := approve.New().Run(ctx, newRC(t, planBody))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if res.Status != journal.StatusUnknown {
		t.Errorf("Status = %v, want the zero Status alongside an error", res.Status)
	}
}

// The dispatcher scopes a logger per execution, but a nil one must not
// panic a gate whose whole job is to stop the run cleanly.
func TestNilLoggerDoesNotPanic(t *testing.T) {
	rc := newRC(t, planBody)
	rc.Logger = nil
	if got := run(t, rc).Status; got != journal.StatusPaused {
		t.Fatalf("Status = %v, want %v", got, journal.StatusPaused)
	}
}
