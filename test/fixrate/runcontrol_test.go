//go:build fixrate

package fixrate

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// TestRunControl_RefusesWhatRunRefuses is TestRun_Validation's table, run against the
// control arm's entry point. The control arm is a second way into a paid
// run, so it must refuse exactly what Run refuses — above all ModeLive
// without ConfirmLiveSpend typed out in full.
func TestRunControl_RefusesWhatRunRefuses(t *testing.T) {
	t.Parallel()
	validAgent := &belaytest.FakeAgent{}
	validSelection := Selection{Bugs: []string{"B01"}}

	tests := []struct {
		name    string
		opts    RunOptions
		wantErr error
	}{
		{"unknown mode", RunOptions{Mode: "bogus", Agent: validAgent, Selection: validSelection}, ErrUnknownMode},
		{"no agent", RunOptions{Mode: ModeReplay, Selection: validSelection}, ErrNoAgent},
		{"live mode without confirmation", RunOptions{Mode: ModeLive, Agent: validAgent, Selection: validSelection}, ErrLiveConfirmationRequired},
		{"live mode with wrong confirmation text", RunOptions{Mode: ModeLive, Agent: validAgent, Selection: validSelection, Confirm: "yes"}, ErrLiveConfirmationRequired},
		{"invalid selection", RunOptions{Mode: ModeReplay, Agent: validAgent, Selection: Selection{}}, ErrInvalidSelection},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := RunControl(t.Context(), tt.opts)
			if err == nil {
				t.Fatal("RunControl() = nil error, want one")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("RunControl() error = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

// repairsFromSecondEdit returns a scripted agent that changes nothing on
// its first editing call and applies plan on every editing call after it.
//
// It stands in for an agent that cannot find a bug from the goal alone but
// can once a failing test points at it — which is precisely what belay's
// fix loop supplies and the control arm, by construction, does not. Under
// ModeReplay that makes the pair a test of the PLUMBING: if the pipeline
// is sound, the belay arm resolves the task through its fix loop and the
// control arm, with one call, cannot. It says nothing about any real
// agent, which is what ModeReplay's provenance line already says.
func repairsFromSecondEdit(pristineSrc string, plan ScriptedPlan) *belaytest.FakeAgent {
	inner := NewScriptedAgent(pristineSrc, plan)
	var edits atomic.Int32
	return &belaytest.FakeAgent{
		NameValue: "fixrate-scripted-second-edit",
		Func: func(ctx context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
			if !isReadOnlyRequest(req) && edits.Add(1) == 1 {
				return belay.AgentResponse{Text: changedFilesReply(nil), SessionID: "fixrate-scripted-session", Turns: 1}, nil
			}
			return inner.Invoke(ctx, req)
		},
	}
}

// TestRunControl_EndToEnd_PairsWithTheBelayArm drives both arms over the
// same one-defect task through the shared pipeline, then pairs them.
//
// It proves four things at once, none of them about agent skill: the
// control arm makes exactly one call; both arms are judged by the
// identical injection, snapshot and evaluation (B25 is classified by the
// same code in both); the control arm's derived allowance and pinned model
// reach its Result; and the paired report registers a delta when one
// exists, so nothing in the pipeline quietly equalises the two arms.
func TestRunControl_EndToEnd_PairsWithTheBelayArm(t *testing.T) {
	fixtureDir, err := DefaultFixtureDir()
	if err != nil {
		t.Fatalf("DefaultFixtureDir() error = %v", err)
	}
	pristine := filepath.Join(fixtureDir, "src")
	sel := Selection{Bugs: []string{"B25"}} // one wrong-constant defect in policy.go, test-detected
	plan := AllRepair([]string{"policy.go"})

	belayAgent := repairsFromSecondEdit(pristine, plan)
	belayResult, err := Run(t.Context(), RunOptions{
		Mode: ModeReplay, FixtureDir: fixtureDir, Selection: sel, Agent: belayAgent, Now: fixedClock(t),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	controlAgent := repairsFromSecondEdit(pristine, plan)
	controlResult, err := RunControl(t.Context(), RunOptions{
		Mode: ModeReplay, FixtureDir: fixtureDir, Selection: sel, Agent: controlAgent, Now: fixedClock(t),
	})
	if err != nil {
		t.Fatalf("RunControl() error = %v", err)
	}

	// --- The control arm is one call, and nothing else ---
	if got := controlAgent.CallCount(); got != 1 {
		t.Errorf("the control arm made %d agent call(s), want exactly 1", got)
	}
	if got := belayAgent.CallCount(); got < 3 {
		t.Errorf("the belay arm made %d agent call(s), want at least plan + code + one fix", got)
	}

	// --- Both results label their arm and their mode ---
	for _, r := range []struct {
		name string
		got  *Result
		arm  Arm
	}{{"belay", belayResult, ArmBelay}, {"control", controlResult, ArmControl}} {
		switch {
		case r.got.Arm != r.arm:
			t.Errorf("%s Result.Arm = %q, want %q", r.name, r.got.Arm, r.arm)
		case r.got.ArmProvenance != r.arm.provenance():
			t.Errorf("%s Result.ArmProvenance = %q, want the arm's own disclosure", r.name, r.got.ArmProvenance)
		case r.got.Mode != ModeReplay || r.got.Provenance != ModeReplay.provenance():
			t.Errorf("%s Result mode/provenance = %q/%q, want replay's", r.name, r.got.Mode, r.got.Provenance)
		case r.got.UsageUSD != 0:
			t.Errorf("%s Result.UsageUSD = %v, want exactly 0 in replay mode", r.name, r.got.UsageUSD)
		case r.got.WallClockSeconds != 0:
			t.Errorf("%s Result.WallClockSeconds = %v, want 0 under a pinned clock", r.name, r.got.WallClockSeconds)
		case len(r.got.Defects) != 1 || r.got.Defects[0].ID != "B25":
			t.Errorf("%s Result.Defects = %+v, want exactly B25", r.name, r.got.Defects)
		}
	}
	if belayResult.Selection.Mode != controlResult.Selection.Mode ||
		strings.Join(belayResult.Selection.Bugs, ",") != strings.Join(controlResult.Selection.Bugs, ",") {
		t.Errorf("the arms measured different selections: %+v vs %+v", belayResult.Selection, controlResult.Selection)
	}

	// --- The same classifier, two different outcomes ---
	if got := belayResult.Defects[0]; got.Status != StatusRepaired {
		t.Errorf("belay arm B25 = %q (%s), want repaired through the fix loop", got.Status, got.Reason)
	}
	if got := controlResult.Defects[0]; got.Status != StatusNotRepaired || got.SourceChanged {
		t.Errorf("control arm B25 = %q source_changed=%v (%s), want not_repaired with policy.go untouched",
			got.Status, got.SourceChanged, got.Reason)
	}

	// --- Each arm's own account ---
	if belayResult.BelaySteps == 0 || belayResult.Control != nil {
		t.Errorf("belay Result: BelaySteps=%d Control=%+v, want graph steps and no control account",
			belayResult.BelaySteps, belayResult.Control)
	}
	cfg := belayConfig()
	switch c := controlResult.Control; {
	case c == nil:
		t.Fatal("control Result.Control = nil, want the single call's account")
	case c.Model != cfg.Agent.ModelFor(graph.NodeCode):
		t.Errorf("control Model = %q, want the code node's %q", c.Model, cfg.Agent.ModelFor(graph.NodeCode))
	case c.Allowance != controlAllowance(cfg):
		t.Errorf("control Allowance = %+v, want %+v", c.Allowance, controlAllowance(cfg))
	case c.Turns != 1:
		t.Errorf("control Turns = %d, want the 1 the agent reported", c.Turns)
	case controlResult.BelaySteps != 0 || controlResult.BelayStatus != "":
		t.Errorf("control Result carries belay fields (steps=%d status=%q); it ran no graph",
			controlResult.BelaySteps, controlResult.BelayStatus)
	}

	// --- The control arm's summary must not read as a belay run ---
	summary := controlResult.HumanSummary()
	if first := strings.SplitN(summary, "\n", 2)[0]; first != ModeReplay.provenance() {
		t.Errorf("control HumanSummary first line = %q, want the mode provenance", first)
	}
	if !strings.Contains(summary, ArmControl.provenance()) {
		t.Errorf("control HumanSummary omits the arm provenance:\n%s", summary)
	}
	if strings.Contains(summary, "belay run:") {
		t.Errorf("control HumanSummary reports a belay run it never made:\n%s", summary)
	}

	// --- And the pair registers the difference ---
	paired, err := summarizePaired(ModeReplay, fixedClock(t)(), []PairedTask{{
		TaskID:  "B25",
		Belay:   resultTaskOutcome("B25", belayResult),
		Control: resultTaskOutcome("B25", controlResult),
	}})
	if err != nil {
		t.Fatalf("summarizePaired() error = %v", err)
	}
	c := paired.Comparison
	if c.BelayOnly != 1 || c.ControlOnly != 0 || c.BothResolved != 0 || c.NeitherResolved != 0 {
		t.Errorf("paired cells = %+v, want belay_only=1 and nothing else", c)
	}
	if c.ResolveRateDelta != 1 {
		t.Errorf("ResolveRateDelta = %v, want 1", c.ResolveRateDelta)
	}
	// One discordant pair is no evidence at all: 2 * C(1,0)/2 = 1.
	if c.McNemarExactP != 1 {
		t.Errorf("McNemarExactP = %v, want 1 for a single discordant pair", c.McNemarExactP)
	}
	if paired.Control.Allowance == nil || *paired.Control.Allowance != controlAllowance(cfg) {
		t.Errorf("paired Control.Allowance = %v, want the derived allowance promoted", paired.Control.Allowance)
	}
	t.Logf("--- paired summary ---\n%s", paired.HumanSummary())
}

// TestRunControl_ReplayMode_RefusesNonZeroUsage carries the zero-cost
// guarantee over to the control arm: it is checked on the shared pipeline,
// not re-implemented per arm, so an agent that reports spend in ModeReplay
// fails the control arm exactly as it fails Run.
func TestRunControl_ReplayMode_RefusesNonZeroUsage(t *testing.T) {
	t.Parallel()
	fixtureDir, err := DefaultFixtureDir()
	if err != nil {
		t.Fatalf("DefaultFixtureDir() error = %v", err)
	}
	spendy := &belaytest.FakeAgent{Responses: []belay.AgentResponse{{Text: "ok", Usage: belay.Usage{USD: 0.01}}}}
	_, err = RunControl(t.Context(), RunOptions{
		Mode: ModeReplay, FixtureDir: fixtureDir, Selection: Selection{Bugs: []string{"B25"}},
		Agent: spendy, Now: fixedClock(t),
	})
	if !errors.Is(err, ErrReplayModeSpentMoney) {
		t.Fatalf("RunControl() error = %v, want it to wrap ErrReplayModeSpentMoney", err)
	}
}

// TestResultTaskOutcome is the one conversion both arms go through, which
// is what makes "judged by the identical check" structural rather than a
// caller's obligation: a task resolves, in either arm, only when every
// selected defect came back repaired.
func TestResultTaskOutcome(t *testing.T) {
	t.Parallel()

	allow := controlAllowance(belayConfig())
	tests := []struct {
		name         string
		result       Result
		wantArm      Arm
		wantResolved bool
		wantTurns    int
		wantModel    string
		wantNote     string
		wantAllow    bool
	}{
		{
			name:         "belay, every defect repaired",
			result:       Result{Arm: ArmBelay, Totals: Totals{Selected: 3, Repaired: 3}, BelayNote: "all gates green"},
			wantArm:      ArmBelay,
			wantResolved: true,
			wantNote:     "all gates green",
		},
		{
			name:    "belay, one left broken",
			result:  Result{Arm: ArmBelay, Totals: Totals{Selected: 3, Repaired: 2, NotRepaired: 1}},
			wantArm: ArmBelay,
		},
		// An invalid repair is a gamed check, and must never make a task
		// resolve however green the suite looks — in either arm.
		{
			name:    "belay, one gamed",
			result:  Result{Arm: ArmBelay, Totals: Totals{Selected: 3, Repaired: 2, InvalidRepair: 1}},
			wantArm: ArmBelay,
		},
		{
			name:    "control, one gamed",
			result:  Result{Arm: ArmControl, Totals: Totals{Selected: 2, Repaired: 1, InvalidRepair: 1}, Control: &ControlRunInfo{Allowance: allow}},
			wantArm: ArmControl, wantAllow: true,
		},
		{
			name:    "belay, one unverified",
			result:  Result{Arm: ArmBelay, Totals: Totals{Selected: 3, Repaired: 2, Unverified: 1}},
			wantArm: ArmBelay,
		},
		// No defects selected is not a resolved task; it is an empty one.
		{
			name:    "nothing selected",
			result:  Result{Arm: ArmBelay},
			wantArm: ArmBelay,
		},
		{
			name: "control, resolved, with its call's account",
			result: Result{
				Arm: ArmControl, Totals: Totals{Selected: 1, Repaired: 1},
				Control: &ControlRunInfo{Model: "sonnet", Turns: 17, Allowance: allow, RunErr: "backend hit its own ceiling"},
			},
			wantArm: ArmControl, wantResolved: true, wantTurns: 17, wantModel: "sonnet",
			wantNote: "backend hit its own ceiling", wantAllow: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := tt.result
			r.UsageUSD, r.UsageEstimated, r.WallClockSeconds = 1.5, true, 42
			got := resultTaskOutcome("t01", &r)
			switch {
			case got.TaskID != "t01":
				t.Errorf("TaskID = %q, want t01", got.TaskID)
			case got.Arm != tt.wantArm:
				t.Errorf("Arm = %q, want %q", got.Arm, tt.wantArm)
			case got.Resolved != tt.wantResolved:
				t.Errorf("Resolved = %v, want %v for %+v", got.Resolved, tt.wantResolved, r.Totals)
			case got.UsageUSD != 1.5 || !got.UsageEstimated:
				t.Errorf("usage = %v estimated=%v, want 1.5 and true carried through", got.UsageUSD, got.UsageEstimated)
			case got.WallClockSeconds != 42:
				t.Errorf("WallClockSeconds = %v, want the Result's 42", got.WallClockSeconds)
			case got.Turns != tt.wantTurns:
				t.Errorf("Turns = %d, want %d", got.Turns, tt.wantTurns)
			case got.Model != tt.wantModel:
				t.Errorf("Model = %q, want %q", got.Model, tt.wantModel)
			case got.Note != tt.wantNote:
				t.Errorf("Note = %q, want %q", got.Note, tt.wantNote)
			case (got.Allowance != nil) != tt.wantAllow:
				t.Errorf("Allowance = %v, want present=%v", got.Allowance, tt.wantAllow)
			case got.Reason == "":
				t.Error("Reason is empty; every outcome must say why it landed where it did")
			}
		})
	}
}
