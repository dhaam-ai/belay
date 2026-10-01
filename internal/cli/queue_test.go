package cli

import (
	"errors"
	"slices"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
)

// TestCreateRunRefusesARunThatAlreadyExists pins the guard the queue needs
// from createRun. Every earlier caller asked state.NewRunID for a fresh id, so
// the run it named could not exist yet. The queue is the first caller that
// supplies its own id -- it records the id in its log before the run exists,
// so a crash can find the run again -- and a caller that remembers an id can
// also be wrong about whether the run behind it was made. Creating a run whose
// manifest is already there would start it over: a fresh blackboard over its
// history, its spend reset to nothing, its approval forgotten. So createRun
// refuses, and touches neither file.
func TestCreateRunRefusesARunThatAlreadyExists(t *testing.T) {
	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const runID = "20260905T120000Z-abcdef123456"
	store, err := createRun(t.Context(), plan, runSpec{Workspace: ws, RunID: runID, Goal: "add tests"})
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}

	// The run has moved on since it was made: its plan is approved and it
	// has spent money.
	blackboard, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	blackboard.Plan.Approved = true
	if err := store.SaveState(blackboard); err != nil {
		t.Fatalf("save state: %v", err)
	}
	manifest, err := store.LoadManifest()
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.Step = 4
	manifest.Budget.SpentUSD = 1.25
	if err := store.SaveManifest(manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	layout := store.Layout()
	before := map[string]string{}
	for _, path := range []string{layout.StatePath(), layout.ManifestPath()} {
		before[path] = readFile(t, path)
	}

	_, err = createRun(t.Context(), plan, runSpec{Workspace: ws, RunID: runID, Goal: "something else"})
	if !errors.Is(err, errRunExists) {
		t.Fatalf("createRun over an existing run = %v, want an error wrapping errRunExists", err)
	}
	for path, want := range before {
		if got := readFile(t, path); got != want {
			t.Errorf("createRun refused, but changed %s:\nbefore: %s\nafter:  %s", path, want, got)
		}
	}
	if got, err := state.LoadManifest(layout.ManifestPath()); err != nil || got.Budget.SpentUSD != 1.25 {
		t.Errorf("the run's spend after the refusal = %v (error %v), want the $1.25 it had spent", got.Budget.SpentUSD, err)
	}
}

// TestResolveAppliesAnOverrideAndChecksIt: a queued goal's run is resolved
// like any other, from the configuration its copy holds, with the queue's
// own decisions on top -- in place, no archive, and no gate for a goal the
// person added with --no-approval. Those are applied before anything is
// built from the configuration, so the route a person is shown is the one
// the run walks, and the result is validated again, so an override can never
// produce a configuration belay.yaml itself could not.
func TestResolveAppliesAnOverrideAndChecksIt(t *testing.T) {
	ws := goWorkspace(t)
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger(), override: func(c *config.Config) {
		c.Graph.Approval = false
		c.Plan.Archive = false
	}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.config.Graph.Approval || plan.config.Plan.Archive {
		t.Errorf("the override did not reach the plan's configuration: approval %v, archive %v",
			plan.config.Graph.Approval, plan.config.Plan.Archive)
	}
	if slices.Contains(plan.route, graph.NodeApprove) {
		t.Errorf("route = %v, want no approve gate: the route is built after the override", plan.route)
	}

	if _, err := resolve(resolveRequest{workspace: ws, logger: quietLogger(), override: func(c *config.Config) {
		c.Budget.MaxUSD = -1
	}}); err == nil {
		t.Error("resolve accepted an override that leaves budget.max_usd negative")
	}
}
