package policy_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	gate "github.com/dhaam-ai/belay/internal/nodes/policy"
	"github.com/dhaam-ai/belay/internal/policy"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// newRC builds a RunContext for a run whose workspace holds files, whose
// change set is changed, and whose manifest pinned pin.
func newRC(t *testing.T, pin *state.PolicyPin, files map[string]string, changed []string) *graph.RunContext {
	t.Helper()
	ws := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	layout, err := state.NewLayout(ws, "20260101T000000Z-0123456789ab")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := os.MkdirAll(layout.ArtifactsDir(), 0o750); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	st := state.NewState("ship it")
	st.Code.ChangedFiles = changed
	return &graph.RunContext{
		Goal: "ship it", State: st, Layout: layout, Workspace: ws,
		RunID: "20260101T000000Z-0123456789ab", NodeName: graph.NodePolicy, Step: 4, Attempt: 1,
		Policy: pin,
	}
}

func pin(p policy.Policy, base policy.Baseline) *state.PolicyPin {
	return &state.PolicyPin{Policy: p, Baseline: base}
}

func TestName(t *testing.T) {
	t.Parallel()
	if got := gate.New().Name(); got != graph.NodePolicy {
		t.Errorf("Name() = %q, want %q", got, graph.NodePolicy)
	}
}

// The hazard this covers is precise: a run starts with no policy, crashes,
// and is resumed after somebody adds one. Nothing registers the gate for
// such a run, so it should be unreachable -- but if it is ever reached, by a
// hand-edited manifest or a wiring mistake, it must hand back a verdict and
// not an error. An error here ends a run that has done nothing wrong, and it
// would do it again on every resume.
func TestARunWithNoBaselineGetsAResultAndNotAnError(t *testing.T) {
	t.Parallel()

	rc := newRC(t, nil, map[string]string{"infra/main.tf": "resource {}\n"}, []string{"infra/main.tf"})
	res, err := gate.New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run returned an error for a run with no baseline: %v", err)
	}
	if res.Status != journal.StatusOK {
		t.Errorf("Status = %s, want %s", res.Status, journal.StatusOK)
	}
	if res.Next != graph.NodeTest {
		t.Errorf("Next = %q, want %q", res.Next, graph.NodeTest)
	}
	if res.Patch.Policy != nil {
		t.Error("a run with no baseline recorded a policy verdict; belay checked nothing, so it must claim nothing")
	}
	if !strings.Contains(res.Note, "baseline") {
		t.Errorf("Note = %q; it must say why nothing was checked", res.Note)
	}
}

func TestRunRoutesOnTheVerdict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		policy     policy.Policy
		files      map[string]string
		changed    []string
		wantNext   string
		wantStatus journal.Status
		wantGate   belay.GateStatus
		wantRule   string
	}{
		{
			name:       "a clean change goes on to the tests",
			policy:     policy.Policy{ForbiddenPaths: []string{"infra/**"}},
			files:      map[string]string{"a.go": "package a\n"},
			changed:    []string{"a.go"},
			wantNext:   graph.NodeTest,
			wantStatus: journal.StatusOK,
			wantGate:   belay.GatePass,
		},
		{
			name:       "a repairable violation goes to the fix loop",
			policy:     policy.Policy{RequiredTests: []policy.TestRule{{When: "**/*.go", Require: "**/*_test.go"}}},
			files:      map[string]string{"a.go": "package a\n"},
			changed:    []string{"a.go"},
			wantNext:   graph.NodeFix,
			wantStatus: journal.StatusOK,
			wantGate:   belay.GateFail,
			wantRule:   string(policy.RuleRequiredTests),
		},
		{
			name:       "a forbidden path ends the run",
			policy:     policy.Policy{ForbiddenPaths: []string{"infra/**"}},
			files:      map[string]string{"infra/main.tf": "resource {}\n"},
			changed:    []string{"infra/main.tf"},
			wantStatus: journal.StatusFailed,
			wantGate:   belay.GateFail,
			wantRule:   string(policy.RuleForbiddenPath),
		},
		{
			name:       "a size ceiling ends the run",
			policy:     policy.Policy{MaxChangedFiles: 1},
			files:      map[string]string{"a.go": "package a\n", "b.go": "package b\n"},
			changed:    []string{"a.go", "b.go"},
			wantStatus: journal.StatusFailed,
			wantGate:   belay.GateFail,
			wantRule:   string(policy.RuleMaxChangedFiles),
		},
		{
			name: "one violation of each class ends the run",
			policy: policy.Policy{
				ForbiddenPaths: []string{"infra/**"},
				RequiredTests:  []policy.TestRule{{When: "**/*.go", Require: "**/*_test.go"}},
			},
			files:      map[string]string{"a.go": "package a\n", "infra/main.tf": "resource {}\n"},
			changed:    []string{"a.go", "infra/main.tf"},
			wantStatus: journal.StatusFailed,
			wantGate:   belay.GateFail,
			wantRule:   string(policy.RuleForbiddenPath),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rc := newRC(t, pin(tt.policy, policy.Baseline{}), tt.files, tt.changed)
			res, err := gate.New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Status != tt.wantStatus {
				t.Errorf("Status = %s, want %s (note: %s)", res.Status, tt.wantStatus, res.Note)
			}
			if res.Status == journal.StatusOK && res.Next != tt.wantNext {
				t.Errorf("Next = %q, want %q", res.Next, tt.wantNext)
			}
			if res.Usage != (belay.Usage{}) {
				t.Errorf("Usage = %+v, want zero: this node calls no agent", res.Usage)
			}
			if res.Patch.Policy == nil {
				t.Fatal("Run recorded no policy verdict on the blackboard")
			}
			block := *res.Patch.Policy
			if block.Gate != tt.wantGate {
				t.Errorf("Patch.Policy.Gate = %s, want %s", block.Gate, tt.wantGate)
			}
			if tt.wantRule != "" {
				var found bool
				for _, v := range block.Violations {
					found = found || v.RuleID == tt.wantRule
				}
				if !found {
					t.Errorf("no violation of %s in %+v", tt.wantRule, block.Violations)
				}
				if !strings.Contains(res.Note, tt.wantRule) {
					t.Errorf("Note = %q; it must name the rule that fired", res.Note)
				}
			}
			if block.ReportPath == "" {
				t.Error("the verdict names no archived report")
			}
			raw, err := os.ReadFile(filepath.Join(rc.Layout.RunDir(), block.ReportPath))
			if err != nil {
				t.Fatalf("read the archived report: %v", err)
			}
			var archived belay.QualityReport
			if err := json.Unmarshal(raw, &archived); err != nil {
				t.Fatalf("the archived report is not a belay.QualityReport: %v", err)
			}
			if archived.Gate != tt.wantGate {
				t.Errorf("the archived report's Gate = %s, want %s", archived.Gate, tt.wantGate)
			}
			if archived.Issues == nil {
				t.Error("the archived report's Issues is null, not []")
			}
		})
	}
}

// The dispatcher re-runs a node whose start has no finish, so the gate must
// converge: the same change judged twice must produce the same verdict and
// the same artifact, or a resumed run could end differently from the attempt
// that crashed.
func TestRunConvergesOnReRun(t *testing.T) {
	t.Parallel()

	p := policy.Policy{ForbiddenPaths: []string{"infra/**"}, MaxChangedLines: 1}
	files := map[string]string{"infra/main.tf": "resource {}\n", "a.go": "package a\n"}
	rc := newRC(t, pin(p, policy.Baseline{}), files, []string{"a.go", "infra/main.tf"})

	first, err := gate.New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.Patch.Policy == nil {
		t.Fatal("Run recorded no policy verdict")
	}
	firstArtifact, err := os.ReadFile(filepath.Join(rc.Layout.RunDir(), first.Patch.Policy.ReportPath))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	again, err := gate.New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run again: %v", err)
	}
	if again.Patch.Policy == nil {
		t.Fatal("a re-run recorded no policy verdict")
	}
	if first.Status != again.Status || first.Note != again.Note {
		t.Errorf("a re-run reached a different verdict:\n first: %s %q\n again: %s %q",
			first.Status, first.Note, again.Status, again.Note)
	}
	secondArtifact, err := os.ReadFile(filepath.Join(rc.Layout.RunDir(), again.Patch.Policy.ReportPath))
	if err != nil {
		t.Fatalf("read artifact again: %v", err)
	}
	if string(firstArtifact) != string(secondArtifact) {
		t.Error("a re-run wrote a different artifact for an unchanged tree")
	}
}

// A rule belay cannot evaluate is no verdict, never a pass. A pinned policy
// this belay cannot compile can only come from a manifest a newer belay
// wrote or a person edited, and letting the change through would be belay
// reporting a guardrail as satisfied when it was skipped.
func TestRunRefusesAPolicyItCannotEvaluate(t *testing.T) {
	t.Parallel()

	rc := newRC(t, pin(policy.Policy{ForbiddenPaths: []string{"../escape"}}, policy.Baseline{}),
		map[string]string{"a.go": "package a\n"}, []string{"a.go"})
	res, err := gate.New().Run(context.Background(), rc)
	if err == nil {
		t.Fatal("Run returned no error for a policy it cannot evaluate")
	}
	if res.Status == journal.StatusOK {
		t.Errorf("Status = %s; a node that reached no verdict must not report OK", res.Status)
	}
}

func TestRunRefusesAContextItCannotHonour(t *testing.T) {
	t.Parallel()

	if _, err := gate.New().Run(context.Background(), nil); err == nil {
		t.Error("Run(nil) returned no error")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rc := newRC(t, pin(policy.Policy{MaxChangedFiles: 1}, policy.Baseline{}), nil, []string{"a.go", "b.go"})
	if _, err := gate.New().Run(ctx, rc); err == nil {
		t.Error("Run with a cancelled context returned no error")
	}
}

// The baseline comes from the pin and nowhere else. A gate that surveyed the
// workspace for its own baseline would be comparing the tree to itself and
// could never find a new dependency.
func TestTheBaselineComesFromThePin(t *testing.T) {
	t.Parallel()

	files := map[string]string{"go.mod": "module x\n\nrequire github.com/pkg/errors v0.9.1\n"}
	p := policy.Policy{NoNewDependencies: true}

	had := pin(p, policy.Baseline{Dependencies: []policy.Dependency{
		{Manifest: "go.mod", Name: "github.com/pkg/errors"},
	}})
	res, err := gate.New().Run(context.Background(), newRC(t, had, files, []string{"go.mod"}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != journal.StatusOK || res.Next != graph.NodeTest {
		t.Errorf("a dependency the baseline already had was reported as new: %s %q", res.Status, res.Note)
	}

	res, err = gate.New().Run(context.Background(), newRC(t, pin(p, policy.Baseline{}), files, []string{"go.mod"}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Next != graph.NodeFix {
		t.Errorf("Next = %q, want %q: a new dependency is something the agent can be asked to do without",
			res.Next, graph.NodeFix)
	}
}
