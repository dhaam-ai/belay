package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
)

// TestWithWorkEntryRoutesWhereTheRunsWorkBegins pins the plan node's one
// configurable edge: where a run with no approval gate on its walk goes once
// its plan is written.
//
// A run that fans out does not code; it hands the approved plan to N
// candidate runs, so its work begins at the fanout node. WithWorkEntry is
// how that is said, and it changes only the edge that used to name the code
// node. The edge to the approve gate is not the work entry's to change:
// graph.approval still decides it, and so does a person's request for a new
// plan, which must go back to the gate they asked it from whatever the run's
// work is.
func TestWithWorkEntryRoutesWhereTheRunsWorkBegins(t *testing.T) {
	tests := []struct {
		name       string
		node       *Node
		approval   bool
		regenerate state.Regeneration
		wantNext   string
	}{
		{
			name:     "without the option the work begins at code, as it always has",
			node:     New(),
			wantNext: graph.NodeCode,
		},
		{
			name:     "a fanout run with approval off goes straight to the fanout",
			node:     New(WithWorkEntry(graph.NodeFanout)),
			wantNext: graph.NodeFanout,
		},
		{
			name:     "a fanout run with approval on still stops at the gate",
			node:     New(WithWorkEntry(graph.NodeFanout)),
			approval: true,
			wantNext: graph.NodeApprove,
		},
		{
			name:       "a revised plan goes back to the gate it was asked for from",
			node:       New(WithWorkEntry(graph.NodeFanout)),
			regenerate: state.Regeneration{Request: 1, Guidance: "smaller steps"},
			wantNext:   graph.NodeApprove,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := newRunContext(t, planAgent(testPlan))
			rc.Config.Graph.Approval = tt.approval
			rc.State.SetRegeneration(tt.regenerate)

			res, err := tt.node.Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Next != tt.wantNext {
				t.Errorf("Next = %q, want %q", res.Next, tt.wantNext)
			}
			// The note is journalled and shown in the timeline, so it has
			// to name the edge the run really took.
			if !strings.HasSuffix(res.Note, "next "+tt.wantNext) {
				t.Errorf("Note = %q, want it to end %q", res.Note, "next "+tt.wantNext)
			}
		})
	}
}
