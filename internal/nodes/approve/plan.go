package approve

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/dhaam-ai/belay/internal/state"
)

// ErrPlanEmpty reports a plan document with nothing in it but whitespace.
//
// An empty plan is not a plan a person can be asked to approve: they would
// be signing off on nothing, and the code node would be handed no
// instructions at all. It is its own error, rather than one more
// ErrPlanUnavailable, because the two have different answers. A plan that
// is not there is a run that cannot go on; a plan that is empty is a file
// somebody can write in, so the gate waits instead of ending the run.
var ErrPlanEmpty = errors.New("approve: the plan is empty")

// PlanArtifact resolves the plan artifact recorded in State.Plan.Path to
// the bare artifact name graph.RunContext.ReadArtifact wants and the
// absolute path a person needs to open.
//
// It refuses to guess. State.Plan.Path is documented as living under
// artifacts/, and both "plan.md" and "artifacts/plan.md" occur in practice
// depending on whether the writer stored WriteArtifact's return value; both
// are accepted. Anything else — a nested path, a path outside the run
// directory — is rejected rather than silently reduced to its basename,
// because reducing it could make the gate fingerprint and approve a
// different file than the one State points at.
//
// It is exported, and takes a state.Layout rather than a RunContext, so
// that everything which opens a run's plan while no node is running — the
// CLI recording a request for a new one, and the viewer after it — finds
// the same file the gate will judge, by the same rule. A second copy of
// this rule anywhere else would eventually disagree with this one, and the
// disagreement would show up as a plan a person can approve through one
// path and not through another.
func PlanArtifact(l state.Layout, recorded string) (name, abs string, err error) {
	if recorded == "" {
		return "", "", &PlanError{Path: "", Reason: "path is not recorded in state; the plan node must run first"}
	}

	name = filepath.Base(filepath.FromSlash(recorded))
	abs, err = l.ArtifactPath(name)
	if err != nil {
		return "", "", &PlanError{Path: recorded, Reason: "is not a usable artifact name", Err: err}
	}
	rel, err := filepath.Rel(l.RunDir(), abs)
	if err != nil {
		return "", "", &PlanError{Path: recorded, Reason: "does not resolve inside the run directory", Err: err}
	}

	if slashed := filepath.ToSlash(recorded); slashed != name && slashed != filepath.ToSlash(rel) {
		return "", "", &PlanError{
			Path:   recorded,
			Reason: fmt.Sprintf("is not a run artifact (want %q or %q)", filepath.ToSlash(rel), name),
		}
	}
	return name, abs, nil
}

// CheckPlan reports whether plan is a document the gate can put in front of
// a person: nil when it is, ErrPlanEmpty when it is not.
//
// This is the gate's whole opinion on a plan's contents, and deliberately a
// small one. It asks whether there is anything there, not whether the
// document is well formed: a plan with no numbered steps, no headings at
// all, or a shape internal/plandoc cannot edit is still a perfectly good
// plan for a person to read and for the code node to implement, and a gate
// that judged Markdown structure would start rejecting plans people wrote
// on purpose.
//
// It is exported so that every writer of a plan can check its bytes
// against the same rule before saving them, rather than discovering at the
// gate that what it wrote cannot be approved.
func CheckPlan(plan []byte) error {
	if len(bytes.TrimSpace(plan)) == 0 {
		return ErrPlanEmpty
	}
	return nil
}
