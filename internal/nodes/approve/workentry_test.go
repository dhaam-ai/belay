package approve_test

import (
	"context"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/state"
)

// TestWithWorkEntryRoutesAnApprovedPlanThere pins the gate's one
// configurable edge: where an approved plan goes.
//
// A run that fans out hands its approved plan to N candidate runs, so the
// gate has to open onto the fanout node, not onto a code node the run's own
// route does not hold. Every other edge stays the gate's own: a pending
// request for a new plan still goes back to the plan node, and an unapproved
// plan still pauses. The note names where the run went, because it is
// journalled and a person reads it in the timeline.
func TestWithWorkEntryRoutesAnApprovedPlanThere(t *testing.T) {
	tests := []struct {
		name       string
		gate       *approve.Node
		approved   bool
		approval   bool
		regenerate state.Regeneration
		wantNext   string
		wantStatus journal.Status
		wantNote   string
	}{
		{
			name:       "without the option an approved plan goes to code, as it always has",
			gate:       approve.New(),
			approved:   true,
			approval:   true,
			wantNext:   graph.NodeCode,
			wantStatus: journal.StatusOK,
			wantNote:   "Plan approved; proceeding to code.",
		},
		{
			name:       "an approved plan in a fanout run goes to the fanout",
			gate:       approve.New(approve.WithWorkEntry(graph.NodeFanout)),
			approved:   true,
			approval:   true,
			wantNext:   graph.NodeFanout,
			wantStatus: journal.StatusOK,
			wantNote:   "Plan approved; proceeding to fanout.",
		},
		{
			name:       "a gate reached with approval off routes on to the fanout too",
			gate:       approve.New(approve.WithWorkEntry(graph.NodeFanout)),
			approval:   false,
			wantNext:   graph.NodeFanout,
			wantStatus: journal.StatusOK,
			wantNote:   "routing to fanout without pausing",
		},
		{
			name:       "an unapproved plan still pauses",
			gate:       approve.New(approve.WithWorkEntry(graph.NodeFanout)),
			approval:   true,
			wantNext:   "",
			wantStatus: journal.StatusPaused,
		},
		{
			name:       "a request for a new plan still goes back to the plan node",
			gate:       approve.New(approve.WithWorkEntry(graph.NodeFanout)),
			approved:   true,
			approval:   true,
			regenerate: state.Regeneration{Request: 1, Guidance: "fewer steps"},
			wantNext:   graph.NodePlan,
			wantStatus: journal.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := newRC(t, planBody)
			rc.State.Plan.Approved = tt.approved
			rc.State.Plan.Digest = approve.Digest([]byte(planBody))
			rc.Config.Graph.Approval = tt.approval
			rc.State.SetRegeneration(tt.regenerate)

			res, err := tt.gate.Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Next != tt.wantNext {
				t.Errorf("Next = %q, want %q", res.Next, tt.wantNext)
			}
			if res.Status != tt.wantStatus {
				t.Errorf("Status = %v, want %v", res.Status, tt.wantStatus)
			}
			if tt.wantNote != "" && !strings.Contains(res.Note, tt.wantNote) {
				t.Errorf("Note = %q, want it to contain %q", res.Note, tt.wantNote)
			}
		})
	}
}
