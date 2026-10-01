//go:build unix

package fork_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/fork"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/isolate/dircopy"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

const (
	goal     = "make the widget work"
	sourceID = "20260901T000000Z-50c4ce000000"
	planText = "# Plan\n\n1. Make the widget work.\n"
)

// cost is what every scripted node reports spending.
var cost = belay.Usage{USD: 0.10, InputTokens: 100, OutputTokens: 10}

// survey is the fingerprint every run belay starts records: dircopy's survey
// with a copy's default skip list, as internal/cli wires it.
func survey(ctx context.Context, dir string) (string, error) {
	s, err := dircopy.SurveyTree(ctx, dir, dircopy.DefaultSkip())
	return s.Digest, err
}

// scripted is a node that runs a script and counts its runs.
type scripted struct {
	name  string
	run   func(rc *graph.RunContext) (graph.Result, error)
	calls int
	// seen holds a copy of every RunContext it was given.
	seen []graph.RunContext
}

func (s *scripted) Name() string { return s.name }

func (s *scripted) Run(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
	s.calls++
	s.seen = append(s.seen, *rc)
	return s.run(rc)
}

// writeFile replaces dir/name with body, and fails the test if it cannot.
func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
}

// script returns the nodes of the run every test forks. As the real
// dispatcher records it:
//
//	plan → approve → code → test (fails) → fix → test (fails) → fix → test → review → END
//
// plan writes the plan; code and each fix rewrite main.go, so the workspace
// changes at code and at each fix and not after the second fix; code opens
// the agent session "sess-code"; each test writes a report; every node
// reports spending cost.
func script(t *testing.T) []graph.Node {
	t.Helper()
	tests := 0
	return []graph.Node{
		&scripted{name: graph.NodePlan, run: func(rc *graph.RunContext) (graph.Result, error) {
			rel, err := rc.WriteArtifact("plan.md", []byte(planText))
			if err != nil {
				return graph.Result{}, err
			}
			return graph.Result{Next: graph.NodeApprove, Status: journal.StatusOK, Usage: cost,
				Patch: state.Patch{Plan: &state.Plan{Path: rel, Digest: approve.Digest([]byte(planText)), Approved: true}}}, nil
		}},
		&scripted{name: graph.NodeApprove, run: func(*graph.RunContext) (graph.Result, error) {
			return graph.Result{Next: graph.NodeCode, Status: journal.StatusOK, Usage: cost}, nil
		}},
		&scripted{name: graph.NodeCode, run: func(rc *graph.RunContext) (graph.Result, error) {
			writeFile(t, rc.Workspace, "main.go", "v1\n")
			return graph.Result{Next: graph.NodeTest, Status: journal.StatusOK, Usage: cost,
				Patch: state.Patch{Code: &state.Code{SessionID: "sess-code", ChangedFiles: []string{"main.go"}}}}, nil
		}},
		&scripted{name: graph.NodeTest, run: func(rc *graph.RunContext) (graph.Result, error) {
			tests++
			rel, err := rc.WriteArtifact(fmt.Sprintf("test-%04d.json", rc.Step), []byte("{}\n"))
			if err != nil {
				return graph.Result{}, err
			}
			if tests < 3 {
				return graph.Result{Next: graph.NodeFix, Status: journal.StatusOK, Usage: cost,
					Patch: state.Patch{Test: &state.Test{Total: 1, Failed: 1, ReportPath: rel}}}, nil
			}
			return graph.Result{Next: graph.NodeReview, Status: journal.StatusOK, Usage: cost,
				Patch: state.Patch{Test: &state.Test{Total: 1, Passed: 1, ReportPath: rel}}}, nil
		}},
		&scripted{name: graph.NodeFix, run: func(rc *graph.RunContext) (graph.Result, error) {
			attempts := rc.State.Fix.Attempts + 1
			// Each repair writes a body of a different length, so a survey
			// tells the trees apart whatever the clock's resolution.
			writeFile(t, rc.Workspace, "main.go", fmt.Sprintf("v%d%s\n", attempts+1, strings.Repeat("x", attempts)))
			return graph.Result{Next: graph.NodeTest, Status: journal.StatusOK, Usage: cost,
				Patch: state.Patch{Fix: &state.Fix{Attempts: attempts}}}, nil
		}},
		&scripted{name: graph.NodeReview, run: func(*graph.RunContext) (graph.Result, error) {
			return graph.Result{Next: graph.End, Status: journal.StatusOK, Usage: cost,
				Patch: state.Patch{Review: &state.Review{Source: "fake", Gate: belay.GatePass}}}, nil
		}},
	}
}

// source is a finished run, made by the real dispatcher over script, so that
// the journal, snapshots and fingerprints a fork reads are the ones belay
// writes.
type source struct {
	ws     string
	layout state.Layout
	cfg    config.Config
}

// newSource records the scripted run in a fresh workspace.
func newSource(t *testing.T) *source {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	writeFile(t, ws, "main.go", "v0\n")
	writeFile(t, ws, "README.md", "the widget\n")

	layout, err := state.NewLayout(ws, sourceID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	cfg := config.Default()
	if err := os.MkdirAll(layout.ArtifactsDir(), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := state.SaveState(layout.StatePath(), state.NewState(goal)); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := state.SaveManifest(layout.ManifestPath(), state.NewManifest(now, sourceID, ws, goal, cfg)); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	reg := graph.NewRegistry()
	reg.MustRegister(script(t)...)
	d, err := graph.NewDispatcher(graph.Options{
		Store: state.NewStore(layout), Registry: reg, Config: cfg, Fingerprint: survey,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	out, err := d.Run(context.Background())
	if err != nil || out.Status != state.RunStatusCompleted {
		t.Fatalf("recording the source run: %v, %v; want it completed", out.Status, err)
	}
	return &source{ws: ws, layout: layout, cfg: cfg}
}

// request asks to fork visit of node in s, with the source's own
// configuration and its plan read from the run's artifacts.
func (s *source) request(node string, visit int) fork.Request {
	return fork.Request{
		Source: s.layout, Tree: s.ws, Node: node, Visit: visit,
		Config: s.cfg, Survey: survey,
		Plan: func() ([]byte, error) {
			return os.ReadFile(filepath.Join(s.layout.ArtifactsDir(), "plan.md")) //nolint:gosec // the test's own run
		},
	}
}

// records reads the source's journal.
func (s *source) records(t *testing.T) []journal.Record {
	t.Helper()
	read, err := journal.ReadFile(s.layout.JournalPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return read.Records
}

// visit finds the ordinal-th visit to node in the source's journal.
func (s *source) visit(t *testing.T, node string, ordinal int) journal.Visit {
	t.Helper()
	visits, err := journal.Visits(s.records(t))
	if err != nil {
		t.Fatalf("Visits: %v", err)
	}
	for _, v := range visits {
		if v.Node == node && v.Ordinal == ordinal {
			return v
		}
	}
	t.Fatalf("the source has no %s #%d", node, ordinal)
	return journal.Visit{}
}

// rewriteSnapshot loads the snapshot of node #ordinal, applies change and
// writes it back in place, for a test that needs a snapshot belay itself
// would not have written.
func (s *source) rewriteSnapshot(t *testing.T, node string, ordinal int, change func(*state.Snapshot)) {
	t.Helper()
	v := s.visit(t, node, ordinal)
	path, err := s.layout.SnapshotPath(v.StartSeq, node)
	if err != nil {
		t.Fatalf("SnapshotPath: %v", err)
	}
	snap, err := state.LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	change(&snap)
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// setManifest applies change to the source's manifest.
func (s *source) setManifest(t *testing.T, change func(*state.Manifest)) {
	t.Helper()
	m, err := state.LoadManifest(s.layout.ManifestPath())
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	change(&m)
	if err := state.SaveManifest(s.layout.ManifestPath(), m); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
}
