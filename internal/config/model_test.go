package config_test

import (
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
)

// Every node must be sent a model, and a node name the graph does not have
// must not come back empty: an empty AgentRequest.Model silently means "the
// backend's own default", which is a model the user never chose and the cost
// estimator cannot price.
//
// Until routing existed, that model was agent.model for every node. It is now
// agent.models[node] where the user set one, and agent.model everywhere else,
// which is what makes a run with no agent.models cost exactly what it did.
//
// The test lives outside package config because it names the graph's nodes,
// and internal/graph imports internal/config.
func TestModelForRoutesEachNode(t *testing.T) {
	agent := config.Default().Agent
	// Not the default, so a ModelFor that ignored the receiver and returned
	// the built-in default could not pass.
	agent.Model = "claude-opus-test"
	agent.Models = config.Models{
		graph.NodePlan: "the-planning-model",
		graph.NodeFix:  "the-repair-model",
	}

	tests := []struct {
		name string
		node string
		want string
	}{
		{"a routed node is sent its own model", graph.NodePlan, "the-planning-model"},
		{"a second routed node is sent its own", graph.NodeFix, "the-repair-model"},
		{"a node with no entry falls back to agent.model", graph.NodeCode, agent.Model},
		{"a node that asks no model still has one", graph.NodeApprove, agent.Model},
		{"write", graph.NodeWrite, agent.Model},
		{"test", graph.NodeTest, agent.Model},
		{"review", graph.NodeReview, agent.Model},
		{"fanout", graph.NodeFanout, agent.Model},
		{"join", graph.NodeJoin, agent.Model},
		{"promote", graph.NodePromote, agent.Model},
		// ModelFor does not validate: Config.Validate has already refused
		// a name like this, and a lookup that failed here would fail in
		// the middle of a run rather than before it started.
		{"a name the graph does not have", "no-such-node", agent.Model},
		{"the empty name", "", agent.Model},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agent.ModelFor(tt.node); got != tt.want {
				t.Errorf("ModelFor(%q) = %q, want %q", tt.node, got, tt.want)
			}
		})
	}
}

// An empty entry is refused by Config.Validate, so ModelFor never sees one in
// a run. If one reaches it anyway -- a Config built in code, a future field
// that forgets the check -- the fallback is agent.model, never the empty
// string, because an empty AgentRequest.Model is the backend's own default:
// unchosen and unpriceable.
func TestModelForNeverReturnsTheEmptyModel(t *testing.T) {
	agent := config.Default().Agent
	agent.Models = config.Models{graph.NodeCode: ""}

	if got := agent.ModelFor(graph.NodeCode); got != agent.Model {
		t.Errorf("ModelFor(%q) = %q, want the fallback %q", graph.NodeCode, got, agent.Model)
	}
}
