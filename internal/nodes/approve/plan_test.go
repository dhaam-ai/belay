package approve_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/state"
)

// layoutFor builds a run layout the way a real run's does.
func layoutFor(t *testing.T) state.Layout {
	t.Helper()
	l, err := state.NewLayout(t.TempDir(), testRunID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	return l
}

// TestPlanArtifactResolvesWhatStateRecords covers the gate's rule for
// turning State.Plan.Path into a file, now that it is callable by anything
// that has to open the same file the gate will judge: both spellings a
// writer may have stored, and a flat refusal for anything else.
func TestPlanArtifactResolvesWhatStateRecords(t *testing.T) {
	l := layoutFor(t)

	tests := []struct {
		name     string
		recorded string
		wantName string
		wantErr  bool
	}{
		{"the artifact name alone", "plan.md", "plan.md", false},
		{"the run-relative path WriteArtifact returns", "artifacts/plan.md", "plan.md", false},
		{"a replaced plan's snapshot", "plan-replaced-1.md", "plan-replaced-1.md", false},
		{"nothing recorded yet", "", "", true},
		{"a traversal", "../../../etc/passwd", "", true},
		{"somewhere else in the run directory", "nodes/002-approve/plan.md", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, abs, err := approve.PlanArtifact(l, tt.recorded)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("PlanArtifact(%q) = %q, %q, nil; want an error", tt.recorded, name, abs)
				}
				if !errors.Is(err, approve.ErrPlanUnavailable) {
					t.Errorf("errors.Is(err, ErrPlanUnavailable) = false; err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanArtifact(%q): %v", tt.recorded, err)
			}
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if want := filepath.Join(l.ArtifactsDir(), tt.wantName); abs != want {
				t.Errorf("abs = %q, want %q", abs, want)
			}
		})
	}
}

// TestCheckPlanIsTheOneRuleForApprovability states what the gate means by a
// plan it can put in front of a person. It is exported so that everything
// which writes a plan -- a person's editor, the viewer, the node that asks
// for a new one -- asks the gate rather than inventing a second rule that
// would eventually disagree with it.
func TestCheckPlanIsTheOneRuleForApprovability(t *testing.T) {
	tests := []struct {
		name   string
		plan   string
		wantIs error
	}{
		{"an ordinary plan", planBody, nil},
		{"a plan with no step list is still a plan", "# Plan\n\nJust do the obvious thing.\n", nil},
		{"nothing at all", "", approve.ErrPlanEmpty},
		{"whitespace only", "   \n\t\n", approve.ErrPlanEmpty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := approve.CheckPlan([]byte(tt.plan))
			if tt.wantIs == nil {
				if err != nil {
					t.Fatalf("CheckPlan(%q) = %v, want nil", tt.plan, err)
				}
				return
			}
			if !errors.Is(err, tt.wantIs) {
				t.Fatalf("CheckPlan(%q) = %v, want %v", tt.plan, err, tt.wantIs)
			}
			// The message is shown to a person who has to fix the file, so
			// it says what is wrong with it in words, not a code.
			if !strings.Contains(err.Error(), "empty") {
				t.Errorf("error does not say what is wrong: %v", err)
			}
		})
	}
}
