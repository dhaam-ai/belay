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
	"errors"
	"fmt"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/nodes/approve"
	"github.com/belay-dev/belay/internal/nodes/code"
	"github.com/belay-dev/belay/internal/nodes/fix"
	"github.com/belay-dev/belay/internal/nodes/plan"
	"github.com/belay-dev/belay/internal/nodes/review"
	"github.com/belay-dev/belay/internal/nodes/test"
	"github.com/belay-dev/belay/internal/nodes/write"
)

// ErrFanoutUnavailable reports that the configuration asks for best-of-N
// candidates, which cannot run yet.
//
// The fanout node is implemented but its partner join node is not, so a run
// with fanout enabled would create N candidate workspaces, spend N times the
// budget, and then fail on an unknown node with nothing to show for it.
// Refusing before the run starts costs the user nothing; discovering it after
// fanout costs them the whole run.
var ErrFanoutUnavailable = errors.New("nodes: fanout is enabled but the join node is not implemented yet")

// Default returns the registry for a standard belay run:
//
//	plan → approve → code → write → test → fix → review → END
//
// with test failures and failed quality gates both routing back through fix,
// bounded by graph.give_up.
//
// workspace is the absolute path of the repository the run operates on; the
// write node needs it explicitly because a fanout candidate's Layout is built
// against the candidate copy rather than the original checkout.
func Default(cfg config.Config, workspace string) (*graph.Registry, error) {
	if cfg.Fanout.Enabled {
		return nil, fmt.Errorf("%w: set fanout.enabled to false in belay.yaml", ErrFanoutUnavailable)
	}

	r := graph.NewRegistry()
	nodes := []graph.Node{
		plan.New(),
		approve.New(),
		code.New(),
		write.New(write.WithWorkspaceRoot(workspace)),
		test.New(),
		fix.New(),
		review.New(),
	}
	for _, n := range nodes {
		if err := r.Register(n); err != nil {
			return nil, fmt.Errorf("nodes: wire the default graph: %w", err)
		}
	}
	return r, nil
}
