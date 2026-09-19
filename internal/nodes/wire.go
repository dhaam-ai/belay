// Package nodes assembles belay's node implementations into the graph the
// dispatcher walks.
//
// It exists so that exactly one place knows which nodes make up a run. The
// dispatcher resolves Result.Next through a graph.Registry by exact name, so
// a node that is implemented but never registered is not a compile error —
// it is a run that dies partway with an unknown-node error. Keeping the
// wiring in one function makes that gap visible here rather than at 3am.
//
// # One wiring, two views
//
// Default returns the graph as a registry, which is what the dispatcher needs:
// a set of nodes it can look up by name. Route returns the same graph as an
// order, which is what a person needs to see where a run is and what happens
// next. A graph.Registry cannot supply that order, because it is a set: Names
// sorts alphabetically. Both functions read one wiring value, so a node added
// to the route is registered by the same line that adds it, rather than by a
// second list someone must remember to keep in step.
package nodes

import (
	"fmt"
	"slices"

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

// wiring is the one description of a run's graph that Default and Route both
// read.
type wiring struct {
	// route is the walk a run takes, in order, from the entry point. Every
	// node on it is registered, by construction.
	route []graph.Node
	// offRoute are registered but on no route. Today that is fanout and
	// join: no node routes into fanout yet, so a run cannot reach either.
	offRoute []graph.Node
}

// wire builds the graph for cfg.
//
// The write node is deliberately NOT pinned to a workspace here.
// write.WithWorkspaceRoot takes precedence over RunContext.Workspace, so a
// registry built once per run and pinned to the original checkout would send
// every fanout candidate's write node at the wrong directory — the candidate
// copies are exactly the case the pin would break. Letting it follow the
// context means each execution is bounded by the tree it is actually working
// on.
func wire(cfg config.Config) *wiring {
	w := &wiring{route: []graph.Node{
		plan.New(),
		approve.New(),
		code.New(),
		write.New(),
		test.New(),
		fix.New(),
		review.New(),
	}}
	if cfg.Fanout.Enabled {
		w.offRoute = append(w.offRoute, fanout.New(), join.New())
	}
	return w
}

// Default returns the registry for a standard belay run:
//
//	plan → approve → code → write → test → fix → review → END
//
// with test failures and failed quality gates both routing back through fix,
// bounded by graph.give_up. When fanout is enabled the graph also carries
// fanout and join, which turn N candidate workspaces into one winner.
func Default(cfg config.Config) (*graph.Registry, error) {
	w := wire(cfg)
	r := graph.NewRegistry()
	for _, n := range slices.Concat(w.route, w.offRoute) {
		if err := r.Register(n); err != nil {
			return nil, fmt.Errorf("nodes: wire the default graph: %w", err)
		}
	}
	return r, nil
}

// Route returns the names of the nodes a run of the graph Default builds for
// cfg walks, in the order it walks them, starting at the entry point. It does
// not end with graph.End: that is where a run stops, not a node it runs.
//
// It is the order a person reads, not a trace. The graph has two cycles,
// test → fix → test and review → fix → test, and Route repeats neither: it
// lists fix once, after test, and a run whose tests and quality gate pass
// never runs it. Nodes registered but not reachable, such as fanout and join
// today, are not on it.
func Route(cfg config.Config) []string {
	w := wire(cfg)
	names := make([]string, len(w.route))
	for i, n := range w.route {
		names[i] = n.Name()
	}
	return names
}
