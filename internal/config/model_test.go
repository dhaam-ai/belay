package config_test

import (
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
)

// Until per-node routing exists, every node must be sent agent.model, and a
// node name the graph does not have must not come back empty: an empty
// AgentRequest.Model silently means "the backend's own default", which is a
// model the user never chose and the cost estimator cannot price.
//
// The test lives outside package config because it names the graph's nodes,
// and internal/graph imports internal/config.
func TestModelForReturnsTheAgentModelForEveryNode(t *testing.T) {
	agent := config.Default().Agent
	// Not the default, so a ModelFor that ignored the receiver and returned
	// the built-in default could not pass.
	agent.Model = "claude-opus-test"

	tests := []struct {
		node string
		want string
	}{
		{graph.NodePlan, agent.Model},
		{graph.NodeApprove, agent.Model},
		{graph.NodeCode, agent.Model},
		{graph.NodeWrite, agent.Model},
		{graph.NodeTest, agent.Model},
		{graph.NodeFix, agent.Model},
		{graph.NodeReview, agent.Model},
		{graph.NodeFanout, agent.Model},
		{graph.NodeJoin, agent.Model},
		{"no-such-node", agent.Model},
		{"", agent.Model},
	}
	for _, tt := range tests {
		t.Run("node="+tt.node, func(t *testing.T) {
			if got := agent.ModelFor(tt.node); got != tt.want {
				t.Errorf("ModelFor(%q) = %q, want %q", tt.node, got, tt.want)
			}
		})
	}
}
