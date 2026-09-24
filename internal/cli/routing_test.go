package cli

import (
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
)

// routedConfig is a belay.yaml that sends the two thinking steps to opus and
// the mechanical one to haiku, which is the shape routing exists for.
const routedConfig = `version: 1
agent:
  model: sonnet
  models:
    plan: opus
    code: haiku
    fix: opus
`

// A dry run is where a person decides whether to spend, so it has to say what
// each step would be billed at -- not only the ceiling, which says what belay
// will stop at, and not only agent.model, which after routing is just the
// fallback.
func TestDryRunPricesEveryNodesModel(t *testing.T) {
	t.Setenv(anthropicKeyEnv, "sk-ant-test")
	ws := goWorkspace(t)
	writeConfig(t, ws, routedConfig)

	out, _, err := runCLI(t, "run", "--dry-run", "--workspace", ws, "add tests")
	if err != nil {
		t.Fatalf("belay run --dry-run: %v\n%s", err, out)
	}

	for _, want := range []string{
		// Each node that spends, with the model it was routed to.
		graph.NodePlan, "opus",
		graph.NodeCode, "haiku",
		graph.NodeFix,
		// The list rate of each, per million tokens, in and out.
		"$5.00", "$25.00", "$1.00",
		"per million",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run never says %q; got:\n%s", want, out)
		}
	}
}

// belay cannot know how many tokens a goal will take, so it must not multiply
// a rate by a guess and print the product. A wrong dollar projection is worse
// than none: a person budgets against it, and belay would have taught them a
// number it invented.
func TestDryRunStatesRatesAndInventsNoTotal(t *testing.T) {
	t.Setenv(anthropicKeyEnv, "sk-ant-test")
	ws := goWorkspace(t)
	writeConfig(t, ws, routedConfig)

	out, _, err := runCLI(t, "run", "--dry-run", "--workspace", ws, "add tests")
	if err != nil {
		t.Fatalf("belay run --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "cannot know how many tokens") {
		t.Errorf("the dry run does not say why it states no total:\n%s", out)
	}
	for _, forbidden := range []string{
		"estimated total", "estimated cost", "this run will cost", "projected",
	} {
		if strings.Contains(strings.ToLower(out), forbidden) {
			t.Errorf("the dry run invents a projection (%q):\n%s", forbidden, out)
		}
	}
}

// A model the backend cannot run is worth catching before the run spends
// anything, and the refusal has to name the setting that holds it: after
// routing there are four places a model can come from, and "the model is
// wrong" would leave a person searching their belay.yaml for which one.
func TestAModelTheBackendCannotPriceStopsTheRunBeforeItSpends(t *testing.T) {
	t.Setenv(anthropicKeyEnv, "sk-ant-test")

	tests := []struct {
		name   string
		config string
		want   []string
	}{
		{
			name: "a routed node's model",
			config: "version: 1\nagent:\n  model: sonnet\n  models:\n    code: claude-unobtainium-9\n",
			want:   []string{"agent.models.code", "claude-unobtainium-9"},
		},
		{
			name:   "the fallback model every other node uses",
			config: "version: 1\nagent:\n  model: claude-unobtainium-9\n",
			want:   []string{"agent.model", "claude-unobtainium-9"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := goWorkspace(t)
			writeConfig(t, ws, tt.config)

			out, _, err := runCLI(t, "run", "--dry-run", "--workspace", ws, "add tests")
			if err == nil {
				t.Fatalf("belay run --dry-run accepted a model it cannot price:\n%s", out)
			}
			for _, want := range append(tt.want, "belay would refuse to start this run", "nothing was spent") {
				if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
					t.Errorf("the refusal never says %q; got:\n%s", want, out)
				}
			}
		})
	}
}

// The same refusal must happen on the way into a real run, not only in the
// dry run: a person who never types --dry-run must not be the one who finds
// out, halfway through, that a model does not exist.
//
// It is asserted at the resolve seam rather than by running belay, because
// both paths read the same blockers -- startRun refuses on them before it
// creates a run -- and because a test that really started a run to prove the
// refusal would call the backend on the day the refusal is broken, which is
// the one day it must not.
func TestResolveBlocksAModelItCannotPriceBeforeAnythingIsCreated(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nagent:\n  model: sonnet\n  models:\n    fix: claude-unobtainium-9\n")

	p, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var found bool
	for _, b := range p.blockers {
		if strings.Contains(b.problem, "agent.models.fix") && strings.Contains(b.problem, "claude-unobtainium-9") {
			found = true
			if b.fix == "" {
				t.Error("the blocker says what is wrong but not what to do about it")
			}
		}
	}
	if !found {
		t.Errorf("no blocker names agent.models.fix and the model it holds; got %+v", p.blockers)
	}
}

// A replayed run calls no backend and spends nothing, so there is no rate to
// show and belay must not refuse the run for the want of one. The models are
// still named: which model recorded a cassette is worth seeing.
func TestAReplayedRunIsNotRefusedForHavingNoRates(t *testing.T) {
	ws := goWorkspace(t)
	emptyCassette(t, ws)
	writeConfig(t, ws, "version: 1\nagent:\n  mode: replay\n  model: sonnet\n  models:\n    code: haiku\n")

	out, _, err := runCLI(t, "run", "--dry-run", "--workspace", ws, "add tests")
	if err != nil {
		t.Fatalf("belay run --dry-run in replay mode: %v\n%s", err, out)
	}
	if !strings.Contains(out, "haiku") {
		t.Errorf("the dry run does not name the model each node would use:\n%s", out)
	}
	if !strings.Contains(out, "spends nothing") {
		t.Errorf("the dry run does not say why it shows no rates:\n%s", out)
	}
}
