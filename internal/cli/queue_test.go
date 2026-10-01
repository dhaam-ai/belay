package cli

import (
	"errors"
	"testing"

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
