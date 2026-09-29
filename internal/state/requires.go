package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ErrUnknownCapability reports a state.json or manifest.json that requires a
// capability this binary does not know.
var ErrUnknownCapability = errors.New("state: run requires a capability this belay does not have")

// CapabilityError names every capability a document requires that this
// binary does not know. It wraps ErrUnknownCapability.
type CapabilityError struct {
	// File is the document that declared the requirement: "state.json" or
	// "manifest.json".
	File string
	// Unknown lists the required capabilities this binary does not know,
	// sorted and without duplicates.
	Unknown []string
}

// Error implements error. It names each capability and says what to do,
// because the person reading it can only fix it by changing which belay
// they run.
func (e *CapabilityError) Error() string {
	quoted := make([]string, len(e.Unknown))
	for i, name := range e.Unknown {
		quoted[i] = strconv.Quote(name)
	}
	noun := "capability"
	if len(quoted) > 1 {
		noun = "capabilities"
	}
	return fmt.Sprintf("state: %s requires %s %s, which this belay does not have; upgrade belay to continue this run",
		e.File, noun, strings.Join(quoted, ", "))
}

// Unwrap reports ErrUnknownCapability.
func (e *CapabilityError) Unwrap() error { return ErrUnknownCapability }

// knownCapabilities is every capability this binary honours.
//
// A change that adds a control field — one whose loss changes what a run
// does — adds its name here, and sets it in requires on every document
// whose run depends on the field.
var knownCapabilities = map[string]bool{
	CapabilityOutputBranch:        true,
	CapabilityPlanRegenerate:      true,
	CapabilityFanoutCandidateRuns: true,
	CapabilityPolicyGate:          true,
}

// CapabilityPlanRegenerate is the capability a run declares, in its
// state.json, while it holds an unserved request for a new plan
// (ADR-0011).
//
// State.Plan.Regenerate is a control field: it is what makes the approve
// gate route back to the plan node instead of pausing again. A belay that
// did not know it would decode such a state.json, drop the request, pause
// at the gate as usual — and then, on the resume that approves the plan,
// write code against the very plan the person had asked to replace, having
// spent their budget on it. Naming the capability turns that into a
// refusal that says to upgrade.
//
// Unlike CapabilityOutputBranch it comes and goes: only a document with an
// unserved request declares it, because only such a run depends on the
// field. See State.requireRegeneration.
const CapabilityPlanRegenerate = "plan.regenerate"

// addCapability returns requires with name in it, sorted and without
// duplicates.
//
// Sorted and deduplicated because the list is rewritten every time its
// document is saved: a list that grew or reordered on each save would
// make every manifest.json differ from the last for no reason anyone
// reading the run could explain.
func addCapability(requires []string, name string) []string {
	out := slices.Clone(requires)
	out = append(out, name)
	slices.Sort(out)
	return slices.Compact(out)
}

// dropCapability returns requires without name, keeping the list's order
// and its non-nil-ness — a run whose last requirement has just gone away
// still writes "requires": [], like every run that never had one, rather
// than a null that reads as a document from before the list existed.
func dropCapability(requires []string, name string) []string {
	if len(requires) == 0 {
		return requires
	}
	out := make([]string, 0, len(requires))
	for _, have := range requires {
		if have != name {
			out = append(out, have)
		}
	}
	return out
}

// checkRequires reads the requires list of raw, a document named file that
// is already at its current schema version, and returns a
// *CapabilityError if it names anything knownCapabilities does not.
//
// It reads requires on its own, before the caller decodes the rest, so a
// field in a shape this binary cannot decode never hides the capability
// that explains why. And it runs only once the schema version is known to
// be current, since a later schema may give requires a different shape.
func checkRequires(file string, raw []byte) error {
	var head struct {
		Requires []string `json:"requires"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return fmt.Errorf("state: decode %s: %w", file, err)
	}
	unknown := unknownCapabilities(head.Requires, knownCapabilities)
	if len(unknown) == 0 {
		return nil
	}
	return &CapabilityError{File: file, Unknown: unknown}
}

// unknownCapabilities returns the entries of requires that known does not
// contain, sorted and without duplicates, or nil if known has them all.
//
// It sorts rather than trusting the writer, so the error a person reads is
// the same whatever order the document listed them in.
func unknownCapabilities(requires []string, known map[string]bool) []string {
	var unknown []string
	for _, name := range requires {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	slices.Sort(unknown)
	return slices.Compact(unknown)
}
