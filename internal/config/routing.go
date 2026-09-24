package config

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	"gopkg.in/yaml.v3"
)

// routableNodes names every node that asks the backend for a model, in the
// order a run reaches them.
//
// It is the closed set agent.models may be keyed by, and it is deliberately
// shorter than the graph's node list. write, test, review and promote reach
// no model at all — they edit files, run a test command, lint, or merge a
// branch — and approve waits for a person. A key for one of those would be a
// setting that loads, validates and does nothing, which is the same silence
// as a typo and would teach a user that agent.models cannot be trusted.
//
// The names are spelled here rather than taken from internal/graph because
// internal/graph imports this package. TestRoutableNodesAreGraphNodes, which
// can import both, holds the two lists together, and TestEveryNodeThatAsks
// ForAModelIsRoutable in internal/nodes holds this list to the nodes that
// actually call [Agent.ModelFor]. A slice that teaches a fourth node to call
// a model adds it here in the same change, or that test fails.
var routableNodes = []string{"plan", "code", "fix"}

// RoutableNodes returns the node names agent.models may be keyed by, in the
// order a run reaches them.
//
// The result is a fresh slice; mutating it does not affect later calls. It is
// exported for the dry run, which prints one row per routable node, so the
// table and the validator can never disagree about which nodes there are.
func RoutableNodes() []string {
	return slices.Clone(routableNodes)
}

// Models routes individual nodes of one run to models of their own, keyed by
// node name: the strong model that writes the plan and repairs a failure, the
// cheap one for the mechanical middle. A node with no entry is sent
// agent.model, so an empty Models is exactly today's behaviour.
//
// # Why a map, and not three fields
//
// Every other setting in the schema is a named field, and strict decoding
// rejects a key that names none. Three fields — agent.models.plan,
// agent.models.code, agent.models.fix — would inherit that check for free.
// They are not used, because the message matters more than the mechanism:
// yaml's unknown-field error names the key and stops, while a routing typo is
// precisely the mistake where the user needs to be told what they could have
// written instead. Keyed by node, the check is belay's own, joined with every
// other configuration error rather than short-circuiting at the first, and it
// reads
//
//	agent.models: "cod" is not valid (want one of: plan, code, fix)
//
// The cost is that the closed set lives in [routableNodes] rather than in the
// struct, and that this type has to say what a merge does to it.
//
// # A later layer replaces the whole map
//
// UnmarshalYAML decodes into a fresh map, so a project belay.yaml that sets
// agent.models replaces the user-level one outright instead of merging key by
// key. That is the rule every other setting follows — the layer that wins,
// wins the whole field — and it is the one a person can predict: routing read
// half from one file and half from another is a run nobody can explain from
// either file alone. It also keeps [Config.Merge]'s copy honest, because the
// map a lower layer holds is never written to.
type Models map[string]string

// UnmarshalYAML decodes a mapping of node name to model into a fresh map.
//
// It does not check the node names. An unknown one is reported by
// [Config.Validate], together with every other problem in the file, rather
// than here, where it would be the only thing the user is told.
func (m *Models) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag == "!!null" {
		*m = nil
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("models must be a mapping of node name to model, not %s", kindName(n.Kind))
	}
	raw := map[string]string{}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	*m = raw
	return nil
}

// modelErrors checks agent.models: that every key names a node belay routes
// to, and that no entry is empty.
//
// Both rules are checked whatever the run would do. A model on a node this
// run never reaches costs nothing, but a name nobody can act on is a mistake
// whenever it is read, and reporting it only for the nodes on today's route
// would make the same file valid or invalid depending on a setting somewhere
// else in it.
//
// Keys are reported in the order they are written in [routableNodes] for the
// ones that are known and alphabetically for the rest, so two runs over the
// same file report the same problems in the same order.
func (c Config) modelErrors() []error {
	var errs []error
	for _, node := range slices.Sorted(maps.Keys(c.Agent.Models)) {
		if !slices.Contains(routableNodes, node) {
			errs = append(errs, &FieldError{
				Field:   "agent.models",
				Value:   strconv.Quote(node),
				Allowed: routableNodes,
			})
			continue
		}
		if c.Agent.Models[node] == "" {
			errs = append(errs, textError("agent.models."+node, "",
				"want a model name, or remove the line to use agent.model"))
		}
	}
	return errs
}
