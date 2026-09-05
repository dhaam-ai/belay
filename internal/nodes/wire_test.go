package nodes_test

import (
	"errors"
	"testing"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/nodes"
)

// Every name a node can route to must be registered. The dispatcher resolves
// Result.Next by exact string match, so an unregistered target is not a
// compile error -- it is a run that dies partway through with nothing to show
// for what it already spent.
func TestEveryRoutableNameIsRegistered(t *testing.T) {
	t.Parallel()

	r, err := nodes.Default(config.Default(), "/repos/app")
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

	r, err := nodes.Default(config.Default(), "/repos/app")
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if _, err := r.Get(graph.NodePlan); err != nil {
		t.Fatalf("the graph does not register its own entry point: %v", err)
	}
}

// Fanout is refused up front rather than part way through. Discovering the
// missing join node after N candidate workspaces have been created means
// paying N times the budget for a run that cannot finish.
func TestFanoutIsRefusedBeforeSpending(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Fanout.Enabled = true
	cfg.Fanout.Candidates = 3

	if _, err := nodes.Default(cfg, "/repos/app"); !errors.Is(err, nodes.ErrFanoutUnavailable) {
		t.Fatalf("Default with fanout enabled = %v, want ErrFanoutUnavailable", err)
	}
}

// A node that is implemented but unreachable is dead weight the graph cannot
// use; a name registered twice would depend on registration order. Neither
// should be possible.
func TestNoUnreachableOrDuplicateNodes(t *testing.T) {
	t.Parallel()

	r, err := nodes.Default(config.Default(), "/repos/app")
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
	// fanout and join are deliberately absent until join exists.
	for _, absent := range []string{graph.NodeFanout, graph.NodeJoin} {
		if seen[absent] {
			t.Errorf("%q is registered, but best-of-N is not runnable yet", absent)
		}
	}
}
