//go:build e2e

package e2e

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// TestLoop_FixLoopEditsAreRecordedInChangedFiles drives the real graph
// through code -> write -> test (red) -> fix -> test (green) -> review -> END,
// with code reporting a.go and fix reporting its own files, and checks what
// the run ends up recording in State.Code.ChangedFiles.
//
// The crash row kills belay inside the fix node's agent call, after the
// repair's edit has landed on disk, and resumes the run with a fresh
// dispatcher over the same run directory. Crash recovery closes that attempt
// as interrupted and re-runs fix at attempt 2 (see the dispatcher's heal). The
// re-run must record the same list an uninterrupted run records.
//
// What that row proves is the recovery path — heal, the re-run at the next
// attempt, the list at the end — and no more. The crash lands before fix
// returns, so no Patch was applied and the re-run starts from the very
// blackboard an uninterrupted run starts from. The window where convergence
// is actually at stake is a crash after the dispatcher applied fix's Patch and
// before it journalled node_finished: the re-run then starts from a list that
// already holds the repair's files. TestFixRecordsTheFilesItsRepairTouched in
// internal/nodes/fix covers that one, by running fix a second time on the
// state its own patch produced.
func TestLoop_FixLoopEditsAreRecordedInChangedFiles(t *testing.T) {
	tests := []struct {
		name       string
		fixReports []string
		crashInFix bool
		want       []string
	}{
		{
			name:       "code changes a.go and fix changes b.go",
			fixReports: []string{"b.go"},
			want:       []string{"a.go", "b.go"},
		},
		{
			name:       "the same run, killed inside fix and resumed",
			fixReports: []string{"b.go"},
			crashInFix: true,
			want:       []string{"a.go", "b.go"},
		},
		{
			name:       "a fix that also touches a file code changed lists it once",
			fixReports: []string{"b.go", "a.go"},
			want:       []string{"a.go", "b.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Graph.Approval = false
			cfg.Graph.GiveUp = 3

			h := newLoopHarness(t, cfg, "add an Add helper")
			h.seedFile("a.go", "package app\n")
			h.seedFile("b.go", "package app\n")

			responses := []belay.AgentResponse{
				loopPlanResponse(),
				loopCodeResponse("sess-1", "a.go"),
				loopFixResponse("sess-1", 1, tt.fixReports...),
			}
			failing := loopFailingTestReport(4, 1, "TestAdd", "Add(2, 2) = 3, want 4")

			if tt.crashInFix {
				crashInsideFix(t, h, responses[:2], failing)
				responses = responses[2:]
			}

			agent := &belaytest.FakeAgent{Responses: responses}
			reports := []belay.TestReport{failing, loopPassingTestReport(4)}
			if tt.crashInFix {
				// The failing run happened before the crash.
				reports = reports[1:]
			}
			runner := &belaytest.FakeRunner{Responses: reports}
			linter := &belaytest.FakeLinter{Responses: []belay.QualityReport{loopPassingQualityReport("fake-linter")}}

			// A fresh Store and registry, as a new belay process builds them.
			registry, err := nodes.Default(cfg)
			if err != nil {
				t.Fatalf("nodes.Default() error = %v", err)
			}
			d, err := graph.NewDispatcher(graph.Options{
				Store: state.NewStore(h.layout), Registry: registry, Config: cfg,
				Agent: agent, Runner: runner, Linter: linter,
			})
			if err != nil {
				t.Fatalf("graph.NewDispatcher() error = %v", err)
			}
			run := d.Run
			if tt.crashInFix {
				run = d.Resume
			}
			out, err := run(context.Background())
			if err != nil {
				t.Fatalf("dispatch error = %v, want nil", err)
			}
			if out.Status != state.RunStatusCompleted {
				t.Fatalf("Outcome.Status = %v, want %v", out.Status, state.RunStatusCompleted)
			}

			timeline := h.timeline()
			if tt.crashInFix {
				for _, want := range []string{"fix#1 finished aborted -> fix", "fix#2 started", "fix#2 finished ok -> write"} {
					if !slices.Contains(timeline, want) {
						t.Errorf("timeline has no %q; the resume did not re-run fix through crash recovery:\n%v", want, timeline)
					}
				}
			}

			got := h.state().Code
			if diff := cmp.Diff(tt.want, got.ChangedFiles); diff != "" {
				t.Errorf("state.Code.ChangedFiles at the end of the run (-want +got):\n%s", diff)
			}
			// fix replaces Code wholesale, so it must carry the rest through.
			if got.SessionID != "sess-1" {
				t.Errorf("state.Code.SessionID = %q, want %q", got.SessionID, "sess-1")
			}
			if !strings.HasPrefix(got.LastDiff, "artifacts/diff-") {
				t.Errorf("state.Code.LastDiff = %q, want the write node's diff artifact", got.LastDiff)
			}
		})
	}
}

// crashInsideFix runs h's run in a first belay process that dies inside the
// fix node: plan and code answer from responses, the test run fails, and the
// fix node's agent call edits b.go and then kills the process before it
// answers. It returns once the crash has left fix#1 started with nothing
// closing it.
func crashInsideFix(t *testing.T, h *loopHarness, responses []belay.AgentResponse, failing belay.TestReport) {
	t.Helper()
	agent := &belaytest.FakeAgent{}
	agent.Func = func(_ context.Context, _ belay.AgentRequest) (belay.AgentResponse, error) {
		if n := agent.CallCount(); n <= len(responses) {
			return responses[n-1], nil
		}
		h.seedFile("b.go", "package app\n\nfunc Add(a, b int) int { return a + b }\n")
		panic(durabilityCrash{node: graph.NodeFix, attempt: 1})
	}
	runner := &belaytest.FakeRunner{Responses: []belay.TestReport{failing}}
	d := h.dispatcher(graph.Options{Agent: agent, Runner: runner})

	durabilityRunExpectingCrash(t, func() (graph.Outcome, error) { return d.Run(context.Background()) })

	recs := h.records()
	if last := recs[len(recs)-1]; last.Event != journal.EventNodeStarted || last.Node != graph.NodeFix {
		t.Fatalf("last journal record after the crash = %v %q, want fix's node_started left open", last.Event, last.Node)
	}
}
