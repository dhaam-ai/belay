package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
)

// TestResolveAppliesAnOverrideBeforeItResolvesAnything: a command that
// changes its run's configuration has to change it before belay decides
// anything from it -- where the output goes, which models are priced -- or
// the plan would describe one run and the dispatcher walk another. And the
// result is held to the same rules as belay.yaml.
func TestResolveAppliesAnOverrideBeforeItResolvesAnything(t *testing.T) {
	ws := goWorkspace(t)

	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger(), override: func(c *config.Config) {
		c.Output.Mode = config.OutputInPlace
		c.Agent.Models = config.Models{"fix": "opus"}
	}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := plan.config.Agent.ModelFor("fix"); got != "opus" {
		t.Errorf("fix is sent %q, want the override's opus", got)
	}
	var priced []string
	for _, m := range plan.models {
		priced = append(priced, m.node+"="+m.model)
	}
	if !strings.Contains(strings.Join(priced, " "), "fix=opus") {
		t.Errorf("models resolved %v, want fix=opus among them: the override comes before pricing", priced)
	}

	_, err = resolve(resolveRequest{workspace: ws, logger: quietLogger(), override: func(c *config.Config) {
		c.Agent.Models = config.Models{"test": "opus"}
	}})
	if err == nil || !strings.Contains(err.Error(), "agent.models") {
		t.Errorf("resolve with an override belay.yaml could not hold = %v, want the configuration refused", err)
	}
}

// TestNextStepsFindARunInItsCopy: a fork lives in a copy of the workspace,
// so every command the closing report writes out for it names that copy, or
// a person pasting it would be told the run does not exist. A fork paused
// after the one node it re-ran is waiting for nothing, so it is not told to
// read a plan; a fork whose gate paused to ask is. A run that lives where
// the command ran is told what it always was.
func TestNextStepsFindARunInItsCopy(t *testing.T) {
	const id = "20260902T000000Z-f0f0f0f0f0f0"
	copyDir := filepath.Join(t.TempDir(), "my work", ".belay", "forks", id)
	layout, err := state.NewLayout(copyDir, id)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	quoted := "'" + copyDir + "'"
	in := "belay --workspace " + quoted + " "

	tests := []struct {
		name    string
		view    outcomeView
		want    []string
		wantNot []string
	}{
		{
			name: "paused after the node it re-ran",
			view: outcomeView{layout: layout, workspace: copyDir, pausedAsAsked: true,
				outcome: graph.Outcome{Status: state.RunStatusPaused, Node: graph.NodeTest, Steps: 1}},
			want:    []string{in + "timeline " + id, in + "resume " + id, "test"},
			wantNot: []string{"read the plan"},
		},
		{
			name: "its gate paused to ask",
			view: outcomeView{layout: layout, workspace: copyDir,
				outcome: graph.Outcome{Status: state.RunStatusPaused, Node: graph.NodeApprove, Steps: 1}},
			want: []string{"read the plan", in + "resume " + id},
		},
		{
			name: "interrupted",
			view: outcomeView{layout: layout, workspace: copyDir, runErr: graph.ErrInterrupted,
				outcome: graph.Outcome{Status: state.RunStatusPaused, Node: graph.NodeFix}},
			want: []string{in + "resume " + id},
		},
		{
			name: "failed",
			view: outcomeView{layout: layout, workspace: copyDir,
				outcome: graph.Outcome{Status: state.RunStatusFailed, Node: graph.NodeFix, Steps: 1}},
			want: []string{in + "timeline " + id},
		},
		{
			name: "ended",
			view: outcomeView{layout: layout, workspace: copyDir,
				outcome: graph.Outcome{Status: state.RunStatusAborted, Node: graph.NodeFix, Steps: 1}},
			want: []string{in + "timeline " + id},
		},
		{
			name: "a run where the command ran",
			view: outcomeView{layout: layout,
				outcome: graph.Outcome{Status: state.RunStatusPaused, Node: graph.NodeApprove, Steps: 2}},
			want:    []string{"belay resume " + id},
			wantNot: []string{"--workspace"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(nextSteps(tt.view), "\n")
			for _, phrase := range tt.want {
				if !strings.Contains(got, phrase) {
					t.Errorf("next steps never say %q:\n%s", phrase, got)
				}
			}
			for _, phrase := range tt.wantNot {
				if strings.Contains(got, phrase) {
					t.Errorf("next steps say %q, and must not:\n%s", phrase, got)
				}
			}
		})
	}
}
