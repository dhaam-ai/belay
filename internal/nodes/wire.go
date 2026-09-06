// Package nodes assembles belay's node implementations into the graph the
// dispatcher walks.
//
// It exists so that exactly one place knows which nodes make up a run. The
// dispatcher resolves Result.Next through a graph.Registry by exact name, so
// a node that is implemented but never registered is not a compile error —
// it is a run that dies partway with an unknown-node error. Keeping the
// wiring in one function makes that gap visible here rather than at 3am.
package nodes

import (
	"fmt"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/nodes/code"
	"github.com/dhaam-ai/belay/internal/nodes/fanout"
	"github.com/dhaam-ai/belay/internal/nodes/fix"
	"github.com/dhaam-ai/belay/internal/nodes/join"
	"github.com/dhaam-ai/belay/internal/nodes/plan"
	"github.com/dhaam-ai/belay/internal/nodes/review"
	"github.com/dhaam-ai/belay/internal/nodes/test"
	"github.com/dhaam-ai/belay/internal/nodes/write"
)

// Default returns the registry for a standard belay run:
//
//	plan → approve → code → write → test → fix → review → END
//
// with test failures and failed quality gates both routing back through fix,
// bounded by graph.give_up. When fanout is enabled the graph also carries
// fanout and join, which turn N candidate workspaces into one winner.
//
// The write node is deliberately NOT pinned to a workspace here.
// write.WithWorkspaceRoot takes precedence over RunContext.Workspace, so a
// registry built once per run and pinned to the original checkout would send
// every fanout candidate's write node at the wrong directory — the candidate
// copies are exactly the case the pin would break. Letting it follow the
// context means each execution is bounded by the tree it is actually working
// on.
func Default(cfg config.Config) (*graph.Registry, error) {
	r := graph.NewRegistry()
	nodes := []graph.Node{
		plan.New(),
		approve.New(),
		code.New(),
		write.New(),
		test.New(),
		fix.New(),
		review.New(),
	}
	if cfg.Fanout.Enabled {
		nodes = append(nodes, fanout.New(), join.New())
	}
	for _, n := range nodes {
		if err := r.Register(n); err != nil {
			return nil, fmt.Errorf("nodes: wire the default graph: %w", err)
		}
	}
	return r, nil
}
