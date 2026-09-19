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
//
// # A conditional node is decided once, when the run is created
//
// Some nodes belong only to some runs: one that promotes the change onto a
// branch, a gate that checks it against the policy in belay.yaml. Each is
// added by an Option, and every Option follows one rule. Whether a run has
// the node is decided once, when the run is created, and pinned in its
// manifest. Every later call to Default or Route for that run, including the
// one a resume makes days later, is given options built from the manifest,
// never from the configuration on disk at the time.
//
// The reason is resume. belay loads belay.yaml again when it resumes a run,
// and nothing stops the file changing in between. If options came from it, a
// run started with no policy, resumed after someone adds one, would route into
// a gate that judges the change against a baseline taken when the run began.
// This run never took one, so the gate could only fail it for something it did
// not do. The reverse is as bad: remove the policy mid-run and the resumed run
// finishes without the check it was started under, and says nothing. A run is
// one attempt at one goal under one set of rules, and those rules must not
// depend on when it was last resumed.
//
// Three things follow:
//
//   - An Option carries its decision in. It is built from a pinned value, and
//     wiring has no Config field, so an Option has no configuration to read.
//   - A node that routes into a conditional node learns that successor from
//     the Option that adds it, not from rc.Config, so an edge and the node it
//     leads to are decided by one value. A node may still choose on live
//     config between successors every run registers: plan picks approve or
//     code from graph.approval, and both are always there.
//   - The manifest field holding a pin is a control field: an older belay that
//     dropped it would resume the run as a different graph. ADR-0011 says
//     how such a field names a capability in the manifest's requires list.
//
// Fanout predates the rule. Default registers fanout and join from the live
// fanout.enabled, which is harmless only because no node routes into fanout
// yet. Whatever first does must turn that choice into a pinned Option too.
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
	// offRoute are registered but on no route this run walks: fanout and
	// join, which no node routes into yet, and the approve gate of a run
	// with graph.approval off. A run cannot reach any of them, and every one
	// of them must still be resolvable by name (see Default).
	offRoute []graph.Node
}

// Option changes the graph Default and Route build: it adds a conditional
// node, or changes a node's successor to lead into one.
//
// It receives the wiring and nothing else. The value it acts on is captured
// when the Option is constructed, from what the run's manifest pinned, which
// is the rule in the package documentation. No Option exists yet. Each slice
// that adds a conditional node adds one, with a row in this package's route
// tests.
type Option func(*wiring)

// wire builds the graph for cfg, then applies opts in the order given, each
// to the graph the ones before it built.
//
// The write node is deliberately NOT pinned to a workspace here.
// write.WithWorkspaceRoot takes precedence over RunContext.Workspace, so a
// registry built once per run and pinned to the original checkout would send
// every fanout candidate's write node at the wrong directory — the candidate
// copies are exactly the case the pin would break. Letting it follow the
// context means each execution is bounded by the tree it is actually working
// on.
func wire(cfg config.Config, opts []Option) *wiring {
	w := &wiring{route: []graph.Node{
		plan.New(),
		approve.New(),
		code.New(),
		write.New(),
		test.New(),
		fix.New(),
		review.New(),
	}}
	// graph.approval false means the plan node routes straight to code, so
	// the gate is on no walk this run takes. It is still registered, and must
	// be: a run started with the gate can be resumed by a process that reads
	// a belay.yaml where the flag has since been turned off, and it re-runs
	// the node the journal names. Routing to a node nobody registered ends
	// that run with nothing to show for what it spent. So the gate leaves the
	// route — which is what a person is shown — and stays in the registry,
	// which is what the dispatcher resolves names through.
	if !cfg.Graph.Approval {
		w.route = slices.DeleteFunc(w.route, func(n graph.Node) bool {
			return n.Name() == graph.NodeApprove
		})
		w.offRoute = append(w.offRoute, approve.New())
	}
	if cfg.Fanout.Enabled {
		w.offRoute = append(w.offRoute, fanout.New(), join.New())
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Default returns the registry for a standard belay run:
//
//	plan → approve → code → write → test → fix → review → END
//
// with test failures and failed quality gates both routing back through fix,
// bounded by graph.give_up. When fanout is enabled the graph also carries
// fanout and join, which turn N candidate workspaces into one winner. Each
// Option adds to or changes that graph.
//
// The approve gate is registered whatever graph.approval says, although a run
// with it off never reaches the gate (see Route). The flag is read from
// belay.yaml on every invocation, so a run started with the gate can be
// resumed after someone turns it off, and the journal still names approve as
// the node to re-run. A registry without it would end that run on an unknown
// node.
func Default(cfg config.Config, opts ...Option) (*graph.Registry, error) {
	w := wire(cfg, opts)
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
// never runs it. Nodes registered but not reachable are not on it: fanout and
// join, which nothing routes into yet, and the approve gate when
// graph.approval is false, since the plan node then routes straight to code.
// A dry run prints this, so it has to be the walk the run really takes rather
// than the walk the graph could take.
//
// Give it the options Default was given. With any others it describes a
// graph the dispatcher is not walking.
func Route(cfg config.Config, opts ...Option) []string {
	w := wire(cfg, opts)
	names := make([]string, len(w.route))
	for i, n := range w.route {
		names[i] = n.Name()
	}
	return names
}
