//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/state"
)

// loopHarness owns one run's real, on-disk run directory — manifest.json,
// state.json and journal.ndjson under <workspace>/.belay/runs/<run id> — and
// the real graph.Registry (internal/nodes.Default) wired against it.
//
// Every TestLoop_* scenario builds its own loopHarness from newLoopHarness,
// so no run directory, registry or workspace is ever shared between tests:
// each gets a fresh t.TempDir(), a fresh run ID and a fresh manifest.
type loopHarness struct {
	t         *testing.T
	workspace string
	goal      string
	layout    state.Layout
	store     *state.Store
	cfg       config.Config
	registry  *graph.Registry
}

// newLoopHarness creates a brand-new run: a fresh workspace directory, a
// run directory under it, a manifest and an initial state.json, and the
// real node graph internal/nodes.Default wires for cfg.
//
// It does not start the dispatcher — callers build one with (*loopHarness).
// dispatcher once they have scripted whichever belaytest fakes the scenario
// needs.
func newLoopHarness(t *testing.T, cfg config.Config, goal string) *loopHarness {
	t.Helper()

	workspace := t.TempDir()

	runID, err := state.NewRunID()
	if err != nil {
		t.Fatalf("state.NewRunID() error = %v", err)
	}
	layout, err := state.NewLayout(workspace, runID)
	if err != nil {
		t.Fatalf("state.NewLayout(%q, %q) error = %v", workspace, runID, err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", layout.RunDir(), err)
	}

	store := state.NewStore(layout)
	man := state.NewManifest(time.Now().UTC(), runID, workspace, goal, cfg)
	if err := store.SaveManifest(man); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}
	if err := store.SaveState(state.NewState(goal)); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	registry, err := nodes.Default(cfg)
	if err != nil {
		t.Fatalf("nodes.Default() error = %v", err)
	}

	return &loopHarness{
		t: t, workspace: workspace, goal: goal,
		layout: layout, store: store, cfg: cfg, registry: registry,
	}
}

// seedFile writes content to a repository-relative path inside the target
// workspace before the run starts, standing in for the files an already
// checked-out repository would contain.
//
// This is what lets the fake agent's code-node script report a real,
// existing file as changed, so the real write node's containment and
// presence checks run against an actual file on an actual filesystem
// instead of a claim nothing on disk backs up.
func (h *loopHarness) seedFile(rel, content string) {
	h.t.Helper()
	abs := filepath.Join(h.workspace, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		h.t.Fatalf("seedFile(%q): MkdirAll(%s) error = %v", rel, filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
		h.t.Fatalf("seedFile(%q): WriteFile error = %v", rel, err)
	}
}

// dispatcher builds a *graph.Dispatcher over this run's real Store and
// Registry. opts.Store and opts.Registry are always overwritten with the
// harness's own; every other field (the adapters, in particular) is the
// caller's to script per scenario.
func (h *loopHarness) dispatcher(opts graph.Options) *graph.Dispatcher {
	h.t.Helper()
	opts.Store = h.store
	opts.Registry = h.registry
	opts.Config = h.cfg
	d, err := graph.NewDispatcher(opts)
	if err != nil {
		h.t.Fatalf("graph.NewDispatcher() error = %v", err)
	}
	return d
}

// state reads the run's current state.json back through the real Store.
func (h *loopHarness) state() state.State {
	h.t.Helper()
	st, err := h.store.LoadState()
	if err != nil {
		h.t.Fatalf("LoadState() error = %v", err)
	}
	return st
}

// manifest reads the run's current manifest.json back through the real
// Store.
func (h *loopHarness) manifest() state.Manifest {
	h.t.Helper()
	m, err := h.store.LoadManifest()
	if err != nil {
		h.t.Fatalf("LoadManifest() error = %v", err)
	}
	return m
}

// approvePlan marks the run's current plan approved, through a real
// state.Patch applied by the real Store — exactly what `belay resume` finds
// on disk after a human has approved the plan, never state.json edited by
// hand. It preserves whatever Path and Digest the plan node (and, if it
// paused there, the approve node) already recorded.
func (h *loopHarness) approvePlan() {
	h.t.Helper()
	st := h.state()
	updated := st.Plan
	updated.Approved = true
	if _, err := h.store.ApplyPatch(state.Patch{Plan: &updated}); err != nil {
		h.t.Fatalf("ApplyPatch(Plan.Approved=true) error = %v", err)
	}
}

// records returns every record in the run's journal, failing the test if
// the journal was left with a torn tail — which would mean a dispatcher
// call returned without cleanly closing the file it owns.
func (h *loopHarness) records() []journal.Record {
	h.t.Helper()
	res, err := journal.ReadFile(h.layout.JournalPath())
	if err != nil {
		h.t.Fatalf("journal.ReadFile() error = %v", err)
	}
	if res.Incomplete {
		h.t.Fatalf("journal has a torn tail after a clean dispatcher shutdown")
	}
	return res.Records
}

// timeline renders the run's journal as a compact sequence of strings, one
// per record, such as "plan#1 finished ok -> code". It first asserts the
// whole history is a structurally valid journal (journal.Validate) — the
// same property crash recovery depends on — so a malformed history fails
// loudly here rather than producing a confusing diff below.
//
// Attempt is the dispatcher's crash-retry counter, not a loop-iteration
// counter: a node visited a second time by an ordinary graph loop (not a
// crash) is attempt 1 again, so "test#1" can legitimately appear more than
// once in a timeline that loops through test and fix several times.
func (h *loopHarness) timeline() []string {
	h.t.Helper()
	recs := h.records()
	if err := journal.Validate(recs); err != nil {
		h.t.Fatalf("journal.Validate() = %v, want nil: the dispatcher wrote an unresumable journal", err)
	}

	out := make([]string, 0, len(recs))
	for _, r := range recs {
		switch r.Event {
		case journal.EventNodeStarted:
			out = append(out, fmt.Sprintf("%s#%d started", r.Node, r.Attempt))
		case journal.EventNodeFinished:
			s := fmt.Sprintf("%s#%d finished %s", r.Node, r.Attempt, r.Status)
			if r.Next != "" {
				s += " -> " + r.Next
			}
			out = append(out, s)
		default:
			out = append(out, r.Event.String())
		}
	}
	return out
}

// artifactPath returns the absolute path of a named run artifact under
// artifacts/, such as "plan.md".
func (h *loopHarness) artifactPath(name string) string {
	return filepath.Join(h.layout.ArtifactsDir(), name)
}

// nodeDir returns the absolute path of one node execution's evidence
// directory, nodes/<seq>-<name>/, matching state.Layout.NodeDir.
func (h *loopHarness) nodeDir(seq int, name string) string {
	h.t.Helper()
	dir, err := h.layout.NodeDir(seq, name)
	if err != nil {
		h.t.Fatalf("Layout.NodeDir(%d, %q) error = %v", seq, name, err)
	}
	return dir
}
