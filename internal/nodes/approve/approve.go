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
//   - A request for a new plan is outstanding (State.Plan.Regenerate):
//     route to graph.NodePlan. See "Asking for a new plan" below.
//   - The plan is there but cannot be approved as it stands — empty, or
//     not UTF-8 (see CheckPlan): pause, saying what is wrong with the file.
//   - Plan.Approved is true: route to graph.NodeCode.
//   - Approval is disabled but the gate ran anyway: route to graph.NodeCode
//     with a warning. See Node.Run for why this does not pause.
//   - Otherwise: pause.
//
// A plan that cannot be approved is checked first, and before Approved,
// because an approval recorded earlier says nothing about the bytes on
// disk now: the pause exists so a person can edit them, and they may have
// emptied the file since.
//
// On every path it re-reads the plan artifact and re-fingerprints it, so a
// plan a human edited during the pause is recorded as what was actually
// approved. Per ADR-0002 it writes nothing itself — the new digest travels
// home in Result.Patch.
//
// # Asking for a new plan
//
// A person reading the plan may want a different one rather than an edited
// one. They say so with `belay plan --regenerate "<guidance>" <run-id>`,
// which records a state.Regeneration request on the blackboard while the
// run is paused. The gate then routes back to graph.NodePlan.
//
// This is an edge in the graph, and deliberately not a call the CLI makes
// to the plan node on its own. A new plan costs money, takes minutes and
// can be interrupted, so it has to be a node execution like any other:
// journalled, charged to the budget ledger, visible in `belay timeline`,
// and re-run from its start if the process dies halfway. A CLI that
// invoked the agent itself would have none of that, and a crash would
// leave a run whose journal says it is waiting at the gate while an agent
// writes a plan nobody will ever see.
//
// The gate hands the request on untouched. Recording that it has been
// served belongs to the node that serves it (internal/nodes/plan), so a
// re-run after a crash between the two sends the agent the same request,
// with the same guidance, rather than a plan prompt with the one thing the
// person asked for missing.
package approve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
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

// readPlan returns the plan's absolute path and current contents.
//
// Locating the file is PlanArtifact's rule and judging its contents is
// CheckPlan's; this is only the read between them, so that a person's
// editor, the CLI and the gate all meet the same file and the same
// verdict. A plan that is not there is an error (see ErrPlanUnavailable);
// a plan that is there and empty comes back as ErrPlanEmpty, which Run
// answers differently.
func readPlan(rc *graph.RunContext) (abs string, data []byte, err error) {
	name, abs, err := PlanArtifact(rc.Layout, rc.State.Plan.Path)
	if err != nil {
		return "", nil, err
	}
	data, err = rc.ReadArtifact(name)
	if err != nil {
		return "", nil, &PlanError{Path: abs, Reason: "cannot be read", Err: err}
	}
	if err := CheckPlan(data); err != nil {
		// Returned as CheckPlan gave it, not wrapped in a PlanError: the
		// plan is there, and a plan that is there is not unavailable. Run
		// answers this with a pause, and abs is what the note names.
		return abs, nil, err
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

// unapprovableNote is the message for a plan that is on disk but cannot be
// approved — one a person emptied or saved in the wrong encoding while the
// run was paused. Like pauseNote it is one line, names the file, and ends
// with the command that continues the run, because the journal is the only
// place this sentence survives.
func unapprovableNote(planPath, reason, id string) string {
	return fmt.Sprintf("The plan at %s %s, so there is nothing to approve. Put a plan back in that file, then continue with: belay resume %s",
		planPath, reason, id)
}

// regenerateNote is the note for the edge back to the plan node.
//
// It names the request's number and not a word of the guidance the person
// typed. Notes are journalled and rendered by `belay timeline`, and belay
// only promises to redact captured subprocess output; what a person wrote
// about their own repository stays in state.json, which is theirs.
func regenerateNote(request int) string {
	return fmt.Sprintf("A new plan was asked for (request %d); returning to the plan node.", request)
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

	// Re-fingerprint what is on disk right now, which is not necessarily
	// what the plan node wrote: the pause exists precisely so a human can
	// change it. An absent recorded digest is not an edit — nothing was
	// ever fingerprinted to differ from.
	//
	// The zero Patch means "nothing changed", so only carry a Plan when the
	// fingerprint on the blackboard is genuinely out of date. Setting the
	// absolute value keeps re-application idempotent (see state.Patch), and
	// copying State.Plan carries the regeneration request through untouched
	// — serving it is the plan node's to record, not the gate's.
	var patch state.Patch
	var digest string
	edited := false
	if err == nil {
		digest = Digest(data)
		recorded := rc.State.Plan.Digest
		edited = recorded != "" && recorded != digest
		if recorded != digest {
			updated := rc.State.Plan
			updated.Digest = digest
			patch.Plan = &updated
		}
	}

	// A request for a new plan is answered whatever state the old plan is
	// in, so this comes before every other branch. The node it routes to
	// replaces the document, which makes the same edge the way out of a
	// plan somebody emptied or deleted. Pending beats Approved for a
	// blunter reason: an approval recorded before the request was made says
	// nothing about a plan that does not exist yet, and acting on it would
	// spend the run's budget writing the code the person just said they did
	// not want.
	if request := rc.State.Plan.Regenerate; request.Pending() {
		logger.Info("a new plan was asked for; returning to the plan node",
			slog.String("plan", planPath),
			slog.Int("request", request.Request))
		return graph.Result{
			Next:   graph.NodePlan,
			Status: journal.StatusOK,
			Patch:  patch,
			Note:   regenerateNote(request.Request),
		}, nil
	}

	if reason, wait := unapprovable(err); wait {
		// The plan is on disk and a person can fix it in the editor they
		// already have open, so the run waits instead of ending. Ending it
		// used to cost them the whole run, and everything it had spent, for
		// an editing slip -- `belay resume` refuses a run that has ended.
		//
		// Nothing is patched: State.Plan.Digest describes the plan the
		// person was shown, and fingerprinting a broken file would leave
		// the run's record claiming they approved these bytes.
		id := runID(rc.Layout)
		logger.Info("the plan cannot be approved as it stands",
			slog.String("plan", planPath),
			slog.String("reason", reason),
			slog.String("resume", "belay resume "+id))
		return graph.Result{
			Status: journal.StatusPaused,
			Note:   unapprovableNote(planPath, reason, id),
		}, nil
	}
	if err != nil {
		return graph.Result{}, err
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
