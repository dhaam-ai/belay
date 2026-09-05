// Package approve implements belay's human approval gate: the node that
// stands between planning and coding so a person can read — and edit —
// plan.md before any code is written.
//
// # It never blocks on a terminal
//
// This node does not prompt, does not read os.Stdin, and does not wait. It
// returns journal.StatusPaused; the dispatcher journals the pause and exits
// 0; the human resumes later with `belay resume <run-id>`, possibly on
// another day and possibly after a reboot.
//
// That is not a stylistic choice. A gate that blocked on a TTY would make
// belay's durability story false: the run would die with the SSH session
// that started it, and "resumable" would mean "resumable as long as nobody
// closes their laptop". Everything the human needs to act — the absolute
// path of the plan and the exact resume command — therefore travels in
// Result.Note, which the dispatcher durably journals, rather than in a
// prompt nobody may be present to answer.
//
// # What it decides
//
// The gate is a pure function of State.Plan and Config.Graph.Approval:
//
//   - Plan.Approved is true: route to graph.NodeCode.
//   - Approval is disabled but the gate ran anyway: route to graph.NodeCode
//     with a warning. See Node.Run for why this does not pause.
//   - Otherwise: pause.
//
// On every path it re-reads the plan artifact and re-fingerprints it, so a
// plan a human edited during the pause is recorded as what was actually
// approved. Per ADR-0002 it writes nothing itself — the new digest travels
// home in Result.Patch.
package approve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/state"
)

// ErrPlanUnavailable reports that the approve gate could not obtain the plan
// it was asked to gate.
//
// It is deliberately an error and not a pause. Pausing would park the run
// forever waiting for a human to approve a document that does not exist,
// which is a worse failure than stopping now and saying so.
var ErrPlanUnavailable = errors.New("approve: plan unavailable")

// PlanError names the plan the approve gate could not use, and says why. It
// wraps both ErrPlanUnavailable and, when there was one, the underlying
// cause — so errors.Is(err, ErrPlanUnavailable) classifies it generically
// and errors.Is(err, fs.ErrNotExist) still recognizes a missing file.
type PlanError struct {
	// Path is the plan the gate was working with: the absolute artifact
	// path once it is known, otherwise the value recorded in State.Plan.
	Path string
	// Reason is a short explanation of what disqualified the plan.
	Reason string
	// Err is the underlying cause, or nil when there was none.
	Err error
}

// Error implements error.
func (e *PlanError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("approve: plan %q %s: %v", e.Path, e.Reason, e.Err)
	}
	return fmt.Sprintf("approve: plan %q %s", e.Path, e.Reason)
}

// Unwrap reports ErrPlanUnavailable and the underlying cause.
func (e *PlanError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrPlanUnavailable}
	}
	return []error{ErrPlanUnavailable, e.Err}
}

// Digest returns the content fingerprint the approve gate records for a
// plan: "sha256:" followed by lowercase hex, the same shape
// config.Config.Digest and the replay cassettes use.
//
// It is exported because the digest is a contract, not an implementation
// detail: whatever marks a plan approved must fingerprint it the same way,
// or every resumed run would report an edit that never happened.
func Digest(plan []byte) string {
	sum := sha256.Sum256(plan)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Node is the approve gate. It holds no state and is safe to reuse across
// runs and goroutines; the zero Node is ready to use.
type Node struct{}

// New returns the approve gate.
func New() *Node { return &Node{} }

var _ graph.Node = (*Node)(nil)

// Name returns graph.NodeApprove.
func (n *Node) Name() string { return graph.NodeApprove }

// planFile resolves the plan artifact recorded in State.Plan to the bare
// artifact name RunContext.ReadArtifact wants and the absolute path a human
// needs to open.
//
// It refuses to guess. State.Plan.Path is documented as living under
// artifacts/, and both "plan.md" and "artifacts/plan.md" occur in practice
// depending on whether the writer stored WriteArtifact's return value; both
// are accepted. Anything else — a nested path, a path outside the run
// directory — is rejected rather than silently reduced to its basename,
// because reducing it could make the gate fingerprint and approve a
// different file than the one State points at.
func planFile(rc *graph.RunContext) (name, abs string, err error) {
	recorded := rc.State.Plan.Path
	if recorded == "" {
		return "", "", &PlanError{Path: "", Reason: "path is not recorded in state; the plan node must run first"}
	}

	name = filepath.Base(filepath.FromSlash(recorded))
	abs, err = rc.Layout.ArtifactPath(name)
	if err != nil {
		return "", "", &PlanError{Path: recorded, Reason: "is not a usable artifact name", Err: err}
	}
	rel, err := filepath.Rel(rc.Layout.RunDir(), abs)
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

// readPlan returns the plan's absolute path and current contents.
//
// An empty plan is treated as unavailable rather than approvable: a
// zero-byte plan.md means the human would be signing off on nothing, and
// the code node would be handed no instructions at all.
func readPlan(rc *graph.RunContext) (abs string, data []byte, err error) {
	name, abs, err := planFile(rc)
	if err != nil {
		return "", nil, err
	}
	data, err = rc.ReadArtifact(name)
	if err != nil {
		return "", nil, &PlanError{Path: abs, Reason: "cannot be read", Err: err}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return "", nil, &PlanError{Path: abs, Reason: "is empty"}
	}
	return abs, data, nil
}

// runID recovers the run's identifier from the name of its directory, which
// is what state.NewLayout built the directory from. RunContext carries no
// manifest, and the resume command in the pause note is useless without it.
func runID(l state.Layout) string {
	id := filepath.Base(l.RunDir())
	if id == "" || id == "." || id == string(filepath.Separator) {
		return "<run-id>"
	}
	return id
}

// pauseNote is the message a waiting human sees. It is one line by design —
// it has to read well in `belay timeline` and in the journal — and it
// carries the only two facts that make the pause actionable: where the plan
// is, and the exact command that continues the run.
func pauseNote(planPath, id string) string {
	return fmt.Sprintf("Plan is waiting for your approval. Review or edit %s, then continue with: belay resume %s", planPath, id)
}

// noteApproved is the note for the ordinary approved pass.
const noteApproved = "Plan approved; proceeding to code."

// noteApprovalDisabled is the note for a gate that ran despite
// graph.approval being off. See Node.Run for why this routes rather than
// pauses.
const noteApprovalDisabled = "Approval is disabled (graph.approval=false) but the approve gate ran anyway; " +
	"routing to code without pausing. This is a wiring bug: the plan node should route straight to code."

// shortDigest abbreviates a "sha256:<64 hex>" fingerprint for prose. The
// full digest still goes to State via the Patch; this is only for reading.
func shortDigest(digest string) string {
	const keep = len("sha256:") + 12
	if len(digest) <= keep {
		return digest
	}
	return digest[:keep] + "..."
}

// withEdit appends the edited-plan clause to note when the plan on disk no
// longer matches the fingerprint State recorded.
//
// An edit is reported, never rejected: editing the plan during the pause is
// the feature, not an error. What would be wrong is proceeding silently,
// leaving the audit trail claiming a digest that no longer describes the
// document the code node was actually given.
func withEdit(note string, edited bool, digest string) string {
	if !edited {
		return note
	}
	return note + fmt.Sprintf(" The plan was edited since it was written; recorded new digest %s.", shortDigest(digest))
}

// Run executes the gate. It never reads os.Stdin and always returns
// promptly.
func (n *Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if err := ctx.Err(); err != nil {
		return graph.Result{}, fmt.Errorf("approve: %w", err)
	}

	logger := rc.Logger
	if logger == nil {
		logger = slog.Default()
	}

	planPath, data, err := readPlan(rc)
	if err != nil {
		return graph.Result{}, err
	}

	// Re-fingerprint what is on disk right now, which is not necessarily
	// what the plan node wrote: the pause exists precisely so a human can
	// change it. An absent recorded digest is not an edit — nothing was
	// ever fingerprinted to differ from.
	digest := Digest(data)
	recorded := rc.State.Plan.Digest
	edited := recorded != "" && recorded != digest

	// The zero Patch means "nothing changed", so only carry a Plan when the
	// fingerprint on the blackboard is genuinely out of date. Setting the
	// absolute value keeps re-application idempotent (see state.Patch).
	var patch state.Patch
	if recorded != digest {
		updated := rc.State.Plan
		updated.Digest = digest
		patch.Plan = &updated
	}

	if rc.State.Plan.Approved {
		logger.Info("plan approved; proceeding to code",
			slog.String("plan", planPath),
			slog.Bool("edited", edited),
			slog.String("digest", digest))
		return graph.Result{
			Next:   graph.NodeCode,
			Status: journal.StatusOK,
			Patch:  patch,
			Note:   withEdit(noteApproved, edited, digest),
		}, nil
	}

	// A run with graph.approval off should never reach this node at all —
	// the plan node routes straight to code. If it does, the wiring is
	// wrong, and there are two ways to be wrong about it. Pausing would
	// strand a run the user explicitly asked to be unattended, in a state
	// only a human at a terminal can clear, which is the exact failure the
	// approval flag exists to avoid; routing on merely does what the
	// configuration asked for, loudly. So: proceed, warn in the log, and
	// say so in the note, where the journal preserves the evidence.
	if !rc.Config.Graph.Approval {
		logger.Warn("approve gate reached with approval disabled; routing to code without pausing",
			slog.String("plan", planPath),
			slog.String("next", graph.NodeCode))
		return graph.Result{
			Next:   graph.NodeCode,
			Status: journal.StatusOK,
			Patch:  patch,
			Note:   withEdit(noteApprovalDisabled, edited, digest),
		}, nil
	}

	id := runID(rc.Layout)
	logger.Info("plan is waiting for human approval",
		slog.String("plan", planPath),
		slog.String("resume", "belay resume "+id))
	return graph.Result{
		Status: journal.StatusPaused,
		Patch:  patch,
		Note:   pauseNote(planPath, id),
	}, nil
}
