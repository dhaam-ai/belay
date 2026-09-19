package nodes

import (
	"context"
	"slices"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
)

// This is an internal test because no real Option exists yet: the first ones
// arrive with the slices that add conditional nodes. An Option built here,
// straight from the unexported wiring, proves the mechanism those slices will
// use before any of them depends on it.

// probeNode is a node no real Option adds. Its name is not a graph constant,
// so it cannot be mistaken for one.
type probeNode string

func (p probeNode) Name() string { return string(p) }

func (probeNode) Run(context.Context, *graph.RunContext) (graph.Result, error) {
	return graph.Result{Next: graph.End, Status: journal.StatusOK}, nil
}

// insertAfter is a test-only Option that puts n on the route directly after
// the node named after.
func insertAfter(after string, n graph.Node) Option {
	return func(w *wiring) {
		i := slices.IndexFunc(w.route, func(m graph.Node) bool { return m.Name() == after })
		w.route = slices.Insert(w.route, i+1, n)
	}
}

// The route with no options is pinned by TestRouteMatchesRegistry's
// "defaults" row; these rows are what an Option adds to it.
func TestOptionsChangeTheRouteAndTheRegistryTogether(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		opts []Option
		want []string
	}{
		{
			name: "one option",
			opts: []Option{insertAfter(graph.NodeWrite, probeNode("probe"))},
			want: []string{
				graph.NodePlan, graph.NodeApprove, graph.NodeCode, graph.NodeWrite,
				"probe", graph.NodeTest, graph.NodeFix, graph.NodeReview,
			},
		},
		{
			// Options apply in the order given, each to the graph the ones
			// before it built: the second finds the node the first added.
			// Two slices each adding a node rely on this to compose.
			name: "options apply in order",
			opts: []Option{
				insertAfter(graph.NodeWrite, probeNode("probe")),
				insertAfter("probe", probeNode("probe-2")),
			},
			want: []string{
				graph.NodePlan, graph.NodeApprove, graph.NodeCode, graph.NodeWrite,
				"probe", "probe-2", graph.NodeTest, graph.NodeFix, graph.NodeReview,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.Default()
			if got := Route(cfg, tc.opts...); !slices.Equal(got, tc.want) {
				t.Errorf("Route() = %v, want %v", got, tc.want)
			}

			// The same options must reach the registry, or the route would
			// show a person a node the dispatcher cannot find.
			registry, err := Default(cfg, tc.opts...)
			if err != nil {
				t.Fatalf("Default: %v", err)
			}
			want := slices.Sorted(slices.Values(tc.want))
			if got := registry.Names(); !slices.Equal(got, want) {
				t.Errorf("Default registers %v, want exactly the route's nodes %v", got, want)
			}
		})
	}
}
