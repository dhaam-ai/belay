package config

import (
	"errors"
	"strings"
	"testing"
)

// A node name belay does not route to must be reported by name, with the
// names it could have been, and the run must not start. The alternative —
// accepting agent.models.cod and quietly sending every node to agent.model —
// is the failure that would make routing untrustworthy: the user reads their
// file, sees a cheap model on the expensive node, and is billed for the
// expensive one with nothing in the output to say why.
func TestUnknownNodeInModelsIsRejectedByName(t *testing.T) {
	cfg := Default()
	cfg.Agent.Models = Models{"cod": "haiku"}

	err := cfg.Validate()
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate() = %v, want an error wrapping ErrInvalid", err)
	}
	const want = `agent.models: "cod" is not valid (want one of: plan, code, fix)`
	if got := err.Error(); !strings.Contains(got, want) {
		t.Errorf("message:\n  got  %s\n  want it to contain %s", got, want)
	}
}

// An entry with no model is a line that reads like routing and is not: the
// empty string is how an agent request asks for the backend's own default,
// which is a model nobody chose and the estimator has no name to price.
func TestEmptyModelForANodeIsRejected(t *testing.T) {
	cfg := Default()
	cfg.Agent.Models = Models{"code": ""}

	err := cfg.Validate()
	const want = `agent.models.code: "" is not valid (want a model name, or remove the line to use agent.model)`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Validate() = %v,\n  want it to contain %s", err, want)
	}
}

// A project belay.yaml that sets agent.models replaces the user-level one
// outright, as every other setting does. Merging key by key would leave a run
// routed half from one file and half from another, which no reader of either
// file could predict -- and the project file could not take a node off the
// strong model its author never knew the user config had put there.
func TestALaterLayerReplacesTheWholeModelsMap(t *testing.T) {
	cfg := Default()
	if err := cfg.Merge([]byte("agent:\n  models:\n    plan: opus\n    fix: opus\n"), "user.yaml"); err != nil {
		t.Fatalf("merge the user layer: %v", err)
	}
	user := cfg.Agent.Models
	if err := cfg.Merge([]byte("agent:\n  models:\n    code: haiku\n"), "belay.yaml"); err != nil {
		t.Fatalf("merge the project layer: %v", err)
	}

	if got, want := cfg.Agent.Models, (Models{"code": "haiku"}); len(got) != len(want) || got["code"] != want["code"] {
		t.Errorf("agent.models = %v, want %v: the project layer replaces the whole map", got, want)
	}
	if user["plan"] != "opus" || len(user) != 2 {
		t.Errorf("the user layer's map was mutated to %v; a merge must not write to the map below it", user)
	}
}

// The digest is how a resumed run proves it is resuming under the settings it
// started with. Routing changes what every step costs, so it has to be in it.
func TestRoutingIsInTheSnapshotAndTheDigest(t *testing.T) {
	routed := Default()
	routed.Agent.Models = Models{"code": "haiku"}

	if got, want := routed.Snapshot()["agent.models.code"], "haiku"; got != want {
		t.Errorf("Snapshot()[\"agent.models.code\"] = %q, want %q", got, want)
	}
	if routed.Digest() == Default().Digest() {
		t.Error("routing a node to another model did not change the digest")
	}
}
