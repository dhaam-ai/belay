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

// A model outside the claude backend's price table does not stop a run.
// belay refused to start on one, and a CLI upgrade, a dated identifier or an
// alias such as sonnet[1m] or opusplan was enough to cause that. The backend
// now prices such a model at the highest rate it knows, so the dry run states
// that rate, never a zero, and the run is held to its ceiling at the safe end
// (internal/agent/claude's resolvePrice argues it). The blocker blockModel
// raises is still there for a backend that refuses a model; this backend no
// longer does.
func TestAModelOutsideThePriceTableIsShownAtTheHighestKnownRate(t *testing.T) {
	t.Setenv(anthropicKeyEnv, "sk-ant-test")

	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "a routed node's model",
			config: "version: 1\nagent:\n  model: sonnet\n  models:\n    code: claude-unobtainium-9\n",
			want:   "code claude-unobtainium-9 $10.00 in / $50.00 out per million tokens",
		},
		{
			name:   "the fallback model every other node uses",
			config: "version: 1\nagent:\n  model: claude-unobtainium-9\n",
			want:   "plan claude-unobtainium-9 $10.00 in / $50.00 out per million tokens",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := goWorkspace(t)
			writeConfig(t, ws, tt.config)

			out, _, err := runCLI(t, "run", "--dry-run", "--workspace", ws, "add tests")
			if err != nil {
				t.Fatalf("belay run --dry-run refused a model outside the price table: %v\n%s", err, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("the dry run does not show the highest known rate for the model (%q):\n%s", tt.want, out)
			}
		})
	}
}

// resolve no longer blocks a model outside the price table, and the rate it
// records for that node is the highest the backend knows, not a zero.
func TestResolvePricesAModelOutsideTheTableAtTheHighestKnownRate(t *testing.T) {
	ws := goWorkspace(t)
	writeConfig(t, ws, "version: 1\nagent:\n  model: sonnet\n  models:\n    fix: claude-unobtainium-9\n")

	p, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, b := range p.blockers {
		if strings.Contains(b.problem, "claude-unobtainium-9") {
			t.Errorf("resolve blocked a model outside the price table: %+v", b)
		}
	}
	var found bool
	for _, m := range p.models {
		if m.node == "fix" {
			found = true
			if !m.priced || m.rate.Input != 10 || m.rate.Output != 50 {
				t.Errorf("fix's model is recorded as %+v, want priced at $10 in and $50 out", m)
			}
		}
	}
	if !found {
		t.Errorf("no model is recorded for fix; got %+v", p.models)
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
