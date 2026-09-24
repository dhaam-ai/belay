package config_test

import (
	"slices"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
)

// config spells the routable node names itself, because internal/graph
// imports internal/config and cannot be imported back. This test can see
// both, so it is where the two lists are held together: a node renamed in
// graph and not here would turn every agent.models key into a name the
// validator rejects, and the rename would be found by a user rather than by
// the build.
//
// It lives outside package config for the same reason model_test.go does.
func TestRoutableNodesAreGraphNodes(t *testing.T) {
	nodes := []string{
		graph.NodePlan, graph.NodeApprove, graph.NodeCode, graph.NodeWrite,
		graph.NodeTest, graph.NodeFix, graph.NodeReview, graph.NodeFanout,
		graph.NodeJoin, graph.NodePromote,
	}
	for _, routable := range config.RoutableNodes() {
		if !slices.Contains(nodes, routable) {
			t.Errorf("config routes %q, which is not a node of the graph %v", routable, nodes)
		}
	}
}

// RoutableNodes is printed by the dry run and listed in every rejection
// message, so a caller that sorted or truncated the slice it was given would
// change what the next caller reads.
func TestRoutableNodesCannotBeMutatedByACaller(t *testing.T) {
	first := config.RoutableNodes()
	want := slices.Clone(first)
	slices.Reverse(first)
	if got := config.RoutableNodes(); !slices.Equal(got, want) {
		t.Errorf("RoutableNodes() = %v after a caller reversed its own copy, want %v", got, want)
	}
}
