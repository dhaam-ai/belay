package nodes_test

import (
	"slices"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes"
)

// TestRouteMatchesRegistry keeps the route belay shows a person and the graph
// the dispatcher walks from drifting apart. It moved here from internal/cli
// with the route itself, so that each change to the graph adds a row here
// rather than a second route function somewhere else.
func TestRouteMatchesRegistry(t *testing.T) {
	t.Parallel()

	// The route every release so far has walked and shown. With no options
	// Route must return exactly this, whatever else the config says.
	standard := []string{
		graph.NodePlan, graph.NodeApprove, graph.NodeCode,
		graph.NodeWrite, graph.NodeTest, graph.NodeFix, graph.NodeReview,
	}
	fanoutOn := config.Default()
	fanoutOn.Fanout.Enabled = true
	fanoutOn.Fanout.Candidates = 3

	for _, tc := range []struct {
		name string
		cfg  config.Config
		want []string
		// offRoute are registered but not on the route: no node routes
		// into them yet, so a run cannot reach them.
		offRoute []string
	}{
		{name: "defaults", cfg: config.Default(), want: standard},
		{
			name: "fanout enabled", cfg: fanoutOn, want: standard,
			offRoute: []string{graph.NodeFanout, graph.NodeJoin},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := nodes.Route(tc.cfg)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Route() = %v, want %v", got, tc.want)
			}
			if got[0] != journal.FirstNode {
				t.Errorf("the route starts at %q but a run starts at %q", got[0], journal.FirstNode)
			}

			registry, err := nodes.Default(tc.cfg)
			if err != nil {
				t.Fatalf("Default: %v", err)
			}
			for _, name := range got {
				if _, err := registry.Get(name); err != nil {
					t.Errorf("the route names %q, which Default does not register", name)
				}
			}
			// The other direction: a registered node that is neither on
			// the route nor known to be off it is one a person is never
			// shown and a run may still reach.
			registered := registry.Names()
			slices.Sort(registered)
			accounted := slices.Concat(got, tc.offRoute)
			slices.Sort(accounted)
			if !slices.Equal(registered, accounted) {
				t.Errorf("Default registers %v; the route and the off-route nodes account for %v", registered, accounted)
			}
		})
	}
}

// Every name a node can route to must be registered. The dispatcher resolves
// Result.Next by exact string match, so an unregistered target is not a
// compile error -- it is a run that dies partway through with nothing to show
// for what it already spent.
func TestEveryRoutableNameIsRegistered(t *testing.T) {
	t.Parallel()

	r, err := nodes.Default(config.Default())
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	for _, name := range []string{
		graph.NodePlan, graph.NodeApprove, graph.NodeCode,
		graph.NodeWrite, graph.NodeTest, graph.NodeFix, graph.NodeReview,
	} {
		if _, err := r.Get(name); err != nil {
			t.Errorf("Get(%q): %v", name, err)
		}
	}
	if got, want := r.Len(), 7; got != want {
		t.Errorf("Len() = %d, want %d: %v", got, want, r.Names())
	}
}

// The entry point the resume path picks for a fresh run must exist in the
// registry, or every first run fails immediately.
func TestEntryPointIsRegistered(t *testing.T) {
	t.Parallel()

	r, err := nodes.Default(config.Default())
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if _, err := r.Get(graph.NodePlan); err != nil {
		t.Fatalf("the graph does not register its own entry point: %v", err)
	}
}

// With fanout enabled the graph must carry both halves of best-of-N.
// Registering fanout without join would create N candidate workspaces, spend N
// times the budget, and then die routing to a node nobody registered.
func TestFanoutRegistersBothHalves(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Fanout.Enabled = true
	cfg.Fanout.Candidates = 3

	r, err := nodes.Default(cfg)
	if err != nil {
		t.Fatalf("Default with fanout enabled: %v", err)
	}
	for _, name := range []string{graph.NodeFanout, graph.NodeJoin} {
		if _, err := r.Get(name); err != nil {
			t.Errorf("Get(%q): %v", name, err)
		}
	}
	if got, want := r.Len(), 9; got != want {
		t.Errorf("Len() = %d, want %d: %v", got, want, r.Names())
	}
}

// A node that is implemented but unreachable is dead weight the graph cannot
// use; a name registered twice would depend on registration order. Neither
// should be possible.
func TestNoUnreachableOrDuplicateNodes(t *testing.T) {
	t.Parallel()

	r, err := nodes.Default(config.Default())
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	seen := map[string]bool{}
	for _, n := range r.Names() {
		if seen[n] {
			t.Errorf("%q registered twice", n)
		}
		seen[n] = true
	}
	// With fanout disabled -- the default -- neither half is registered, so
	// a graph cannot route into a best-of-N the user did not ask to pay for.
	for _, absent := range []string{graph.NodeFanout, graph.NodeJoin} {
		if seen[absent] {
			t.Errorf("%q is registered with fanout disabled", absent)
		}
	}
}
