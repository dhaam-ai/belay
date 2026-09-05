package test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/pkg/belay"
	"github.com/belay-dev/belay/pkg/belay/belaytest"
)

// TestRunRoutesToFixEvenAtTheGiveUpBoundary pins the ownership split: the fix
// node enforces Config.Graph.GiveUp, this node never does.
//
// If this node terminated the run at the cap, the two would both enforce it and
// a run would stop an attempt early. The cap is checked in exactly one place,
// and this test is what keeps it there.
func TestRunRoutesToFixEvenAtTheGiveUpBoundary(t *testing.T) {
	t.Parallel()

	const giveUp = 3

	tests := []struct {
		name       string
		attempts   int
		alreadyOut bool
	}{
		{name: "no attempts yet", attempts: 0},
		{name: "below the cap", attempts: giveUp - 1},
		{name: "exactly at the cap", attempts: giveUp},
		{name: "past the cap", attempts: giveUp + 1},
		{name: "fix loop already flagged give-up", attempts: giveUp, alreadyOut: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := &belaytest.FakeRunner{
				Responses: []belay.TestReport{{Total: 4, Passed: 1, Failed: 3}},
			}
			rc, _ := newRC(t, runner)
			rc.Config.Graph.GiveUp = giveUp
			rc.State.Fix.Attempts = tt.attempts
			rc.State.Fix.GiveUp = tt.alreadyOut

			res, err := New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if res.Next != graph.NodeFix {
				t.Errorf("Next = %q, want %q: the fix node enforces the cap, not this one",
					res.Next, graph.NodeFix)
			}
			if res.Status != journal.StatusOK {
				t.Errorf("Status = %v, want StatusOK: reaching the cap is not this node's failure", res.Status)
			}
			// Fix belongs to the fix node. Writing it here would make two
			// nodes owners of one counter.
			if res.Patch.Fix != nil {
				t.Error("Patch.Fix was set; State.Fix belongs to the fix node")
			}
		})
	}
}

// TestAZeroTestRunIsNotGreen pins the documented decision: a suite that matched
// nothing is suspicious, so it takes the failing branch and says why, rather
// than being reported as a pass.
func TestAZeroTestRunIsNotGreen(t *testing.T) {
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{{}}}
	rc, _ := newRC(t, runner)

	res, err := New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if res.Next == graph.NodeReview {
		t.Fatal("a zero-test run routed to review; a suite that matched nothing is not green")
	}
	if res.Next != graph.NodeFix {
		t.Errorf("Next = %q, want %q", res.Next, graph.NodeFix)
	}
	if res.Patch.Test.Total != 0 {
		t.Errorf("Test.Total = %d, want 0", res.Patch.Test.Total)
	}
	// The note has to carry the explanation, because the fix node gets no
	// Failures to work from in this case.
	if res.Note != "fake-runner: no tests discovered (0 total), not treated as passing" {
		t.Errorf("Note = %q, want it to explain the empty run", res.Note)
	}
}

// TestRunWritesNoControlState is the ADR 0002 invariant: a node returns a Patch
// and never persists. If it wrote state.json itself, a crash mid-node could
// half-apply an update and resume would be wrong.
func TestRunWritesNoControlState(t *testing.T) {
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{{Total: 3, Passed: 3}}}
	rc, _ := newRC(t, runner)

	if _, err := New().Run(context.Background(), rc); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, p := range []string{rc.Layout.StatePath(), rc.Layout.ManifestPath(), rc.Layout.JournalPath()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("node wrote %s; the dispatcher owns that file", filepath.Base(p))
		}
	}
}

// TestRerunningAStepOverwritesItsReport covers the crash-recovery path: the
// dispatcher re-runs a node whose start has no matching finish, and a second
// execution at the same Step must not leave two reports behind.
func TestRerunningAStepOverwritesItsReport(t *testing.T) {
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{
		{Total: 4, Passed: 2, Failed: 2},
		{Total: 4, Passed: 4},
	}}
	rc, _ := newRC(t, runner)

	first, err := New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	rc.Attempt = 2
	second, err := New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}

	if first.Patch.Test.ReportPath != second.Patch.Test.ReportPath {
		t.Errorf("report path changed across attempts: %q then %q",
			first.Patch.Test.ReportPath, second.Patch.Test.ReportPath)
	}
	if first.Next != graph.NodeFix || second.Next != graph.NodeReview {
		t.Errorf("routing = %q then %q, want fix then review", first.Next, second.Next)
	}

	entries, err := os.ReadDir(rc.Layout.ArtifactsDir())
	if err != nil {
		t.Fatalf("read artifacts dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("artifacts dir holds %d files, want 1 (the re-run must overwrite)", len(entries))
	}

	//nolint:gosec // path is the run dir joined with the node's own returned relative path
	data, err := os.ReadFile(filepath.Join(rc.Layout.RunDir(), second.Patch.Test.ReportPath))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var got belay.TestReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if got.Failed != 0 || got.Passed != 4 {
		t.Errorf("archived report = %+v, want the second attempt's result", got)
	}
}

// TestNoteIsRedactedByConstruction: the note is built from counts and a fixed
// vocabulary, so no failure message or path can leak into the journal through it.
func TestNoteIsRedactedByConstruction(t *testing.T) {
	t.Parallel()

	secret := "/home/alice/secret-repo/token_test.go"
	r := belay.TestReport{
		Total: 2, Failed: 1,
		Failures: []belay.TestFailure{{
			Name:    "TestToken",
			File:    secret,
			Message: "expected token sk-live-abcdef",
		}},
	}

	got := note("go-test", r)
	if want := "go-test: 2 tests, 1 failed"; got != want {
		t.Errorf("note() = %q, want %q", got, want)
	}
	for _, leak := range []string{secret, "sk-live-abcdef", "TestToken"} {
		if strings.Contains(got, leak) {
			t.Errorf("note() leaked %q: %q", leak, got)
		}
	}
}

// TestConfigIsNotConsultedForRouting documents that the node reads no config to
// decide where to go: routing is a pure function of the report.
func TestConfigIsNotConsultedForRouting(t *testing.T) {
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{{Total: 2, Passed: 2}}}
	rc, _ := newRC(t, runner)
	rc.Config = config.Config{Graph: config.Graph{GiveUp: 0, MaxSteps: 0}}

	res, err := New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Next != graph.NodeReview {
		t.Errorf("Next = %q, want %q", res.Next, graph.NodeReview)
	}
}
