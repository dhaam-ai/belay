//go:build e2e && unix

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/fork"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/isolate/dircopy"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// Fork suite: `belay fork` end to end, over belay's real node graph and real
// directory copies, with the agent, the test runner and the linter faked for
// the reason the rest of this package fakes them.
//
// The scenario is the one forks exist for. A run's fix loop gives up; the
// person does not want to pay for the plan and the code again, only to give
// the last repair another go with a stronger model and one more round. They
// fork the fix, it runs once and pauses, they resume the fork, and it
// finishes, while the run that gave up stays exactly as it was.

// forkSurvey is the fingerprint every run belay starts records beside its
// snapshots (internal/cli wires the same function).
func forkSurvey(ctx context.Context, dir string) (string, error) {
	s, err := dircopy.SurveyTree(ctx, dir, dircopy.DefaultSkip())
	return s.Digest, err
}

// forkTreeSums hashes every file under dir but the forks, with its mode and
// modification time, so a test can say a tree did not change in any way.
func forkTreeSums(t *testing.T, dir string) map[string]string {
	t.Helper()
	forks := filepath.Join(dir, ".belay", "forks")
	sums := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == forks {
			return fs.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // the test's own tree
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		rel, _ := filepath.Rel(dir, path)
		sums[rel] = info.Mode().String() + " " + info.ModTime().UTC().String() + " " + hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
	return sums
}

// TestFork_AGivenUpFixLoopIsRetriedFromItsLastFix forks the fix visit that
// gave a run up, with one more round allowed and a stronger model, and
// carries the fork to the end.
func TestFork_AGivenUpFixLoopIsRetriedFromItsLastFix(t *testing.T) {
	cfg := config.Default()
	cfg.Graph.Approval = false
	cfg.Graph.GiveUp = 2

	// The source: plan, code, then a fix loop whose tests never pass, until
	// fix gives up on its third visit.
	h := newLoopHarness(t, cfg, "add an Add helper")
	h.seedFile("src/app.go", "package src\n")
	srcAgent := &belaytest.FakeAgent{Responses: []belay.AgentResponse{
		loopPlanResponse(),
		loopCodeResponse("sess-1", "src/app.go"),
		loopFixResponse("sess-1", 1),
		loopFixResponse("sess-1", 2),
	}}
	failing := &belaytest.FakeRunner{Responses: []belay.TestReport{loopFailingTestReport(4, 4, "TestAdd", "Add(2, 2) = 3, want 4")}}
	passingGate := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopPassingQualityReport("fake-linter")}}
	out, err := h.dispatcher(graph.Options{Agent: srcAgent, Runner: failing, Linter: passingGate, Fingerprint: forkSurvey}).
		Run(context.Background())
	if err == nil || out.Status != state.RunStatusFailed {
		t.Fatalf("source run = %v, %v; want it to fail when fix gives up", out.Status, err)
	}
	before := forkTreeSums(t, h.workspace)

	// The fork: the last fix, one more round, a stronger model.
	forked := cfg
	forked.Graph.GiveUp = 3
	forked.Agent.Models = config.Models{graph.NodeFix: "opus"}
	spec, err := fork.Prepare(context.Background(), fork.Request{
		Source: h.layout, Tree: h.workspace, Node: graph.NodeFix,
		Config: forked, Survey: forkSurvey,
		Plan: func() ([]byte, error) { return os.ReadFile(h.artifactPath("plan.md")) }, //nolint:gosec // the test's own run
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if spec.Visit.Ordinal != 3 || spec.Visits != 3 || spec.FilesChanged {
		t.Fatalf("chose fix #%d of %d (files changed %v), want the last of 3, on unchanged files",
			spec.Visit.Ordinal, spec.Visits, spec.FilesChanged)
	}
	root, err := state.ForksDir(h.workspace)
	if err != nil {
		t.Fatalf("ForksDir: %v", err)
	}
	iso, err := dircopy.New(root)
	if err != nil {
		t.Fatalf("dircopy.New: %v", err)
	}
	const forkID = "20260102T000000Z-f0f0f0f0f0f0"
	f, err := spec.Create(context.Background(), forkID, iso, time.Now())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	forkAgent := &belaytest.FakeAgent{Responses: []belay.AgentResponse{loopFixResponse("sess-2", 3)}}
	passing := &belaytest.FakeRunner{Responses: []belay.TestReport{loopPassingTestReport(4)}}
	registry, err := nodes.Default(forked)
	if err != nil {
		t.Fatalf("nodes.Default: %v", err)
	}
	opts := graph.Options{
		Store: state.NewStore(f.Layout), Registry: registry, Config: forked,
		Agent: forkAgent, Runner: passing, Linter: passingGate, Fingerprint: forkSurvey,
	}

	// The fork's first dispatch: fix, once, then a pause.
	opts.PauseAfter = 1
	d, err := graph.NewDispatcher(opts)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	first, err := d.Run(context.Background())
	if err != nil || first.Status != state.RunStatusPaused || first.Steps != 1 || first.Node != graph.NodeWrite {
		t.Fatalf("fork's first dispatch = {%v, node %q, steps %d}, %v; want a pause before write after one step",
			first.Status, first.Node, first.Steps, err)
	}
	calls := forkAgent.Calls()
	if len(calls) != 1 {
		t.Fatalf("the fork called the agent %d times, want once, for fix", len(calls))
	}
	if calls[0].SessionID != "" || calls[0].Model != "opus" {
		t.Errorf("fix was sent session %q and model %q; want a new conversation and opus", calls[0].SessionID, calls[0].Model)
	}
	st, err := state.LoadState(f.Layout.StatePath())
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.Fix.Attempts != 3 {
		t.Errorf("Fix.Attempts = %d after the fork's repair, want 3: the source's two and this one, counted once", st.Fix.Attempts)
	}

	// Carried on: write, test, review, the end.
	opts.PauseAfter = 0
	d, err = graph.NewDispatcher(opts)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	final, err := d.Resume(context.Background())
	if err != nil || final.Status != state.RunStatusCompleted {
		t.Fatalf("resuming the fork = %v, %v; want it to complete", final.Status, err)
	}

	// Its own spend only, and its history's said, not charged.
	man, err := state.LoadManifest(f.Layout.ManifestPath())
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	var paid float64
	srcRecords := h.records()
	for _, r := range srcRecords {
		if r.Seq < spec.Visit.StartSeq && r.Usage != nil {
			paid += r.Usage.USD
		}
	}
	if want := loopFixResponse("", 3).Usage.USD; man.Budget.SpentUSD != want {
		t.Errorf("fork spent %v, want %v: the one repair it paid for, and none of its history", man.Budget.SpentUSD, want)
	}
	if man.Origin == nil || man.Origin.InheritedUSD != paid || man.Origin.RunID != h.layout.RunID() {
		t.Errorf("Origin = %+v, want the source %s and its $%v of history", man.Origin, h.layout.RunID(), paid)
	}

	// The fork's journal is one resumable history, with the source's before
	// its boundary, unchanged but for the spending.
	read, err := journal.ReadFile(f.Layout.JournalPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := journal.Validate(read.Records); err != nil {
		t.Errorf("the fork's journal is not a valid history: %v", err)
	}
	for i, r := range read.Records[:spec.Visit.StartSeq-1] {
		want := srcRecords[i]
		want.Usage = nil
		if diff := cmp.Diff(want, r); diff != "" {
			t.Errorf("inherited record %d (-source +fork):\n%s", i+1, diff)
		}
	}

	// And the run that gave up is exactly as it was: not a byte, a mode or a
	// modification time of its files or its run directory moved.
	if diff := cmp.Diff(before, forkTreeSums(t, h.workspace)); diff != "" {
		t.Errorf("the source changed (-before +after):\n%s", diff)
	}
	if read, err := journal.ReadFile(h.layout.JournalPath()); err != nil || read.Records[len(read.Records)-1].Event != journal.EventRunFailed {
		t.Errorf("the source journal no longer ends in run_failed: %v", err)
	}
}
