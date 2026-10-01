package cli

import (
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
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
