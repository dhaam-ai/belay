// Package code implements belay's code node: the graph step that hands an
// approved plan to the agent backend and captures the change the agent
// proposes.
//
// # What this node does not do
//
// It does not touch the workspace. The agent is invoked with a read-only
// tool allow-list and asked to reply with a unified diff; applying that
// diff is the write node's job. Keeping "propose" and "apply" in separate
// nodes is what lets a run be inspected, paused, or abandoned between the
// two, and it is why this node's only outputs are artifacts plus a
// state.Patch.
//
// # Session reuse
//
// The node passes State.Code.SessionID back to the backend as
// AgentRequest.SessionID and records the SessionID the backend returns.
// That is the whole point of the node existing separately from the fix
// loop: a second pass continues one conversation instead of paying to
// rebuild the agent's understanding of the repository from cold.
//
// # Persistence
//
// Per ADR-0002 the node writes no state.json, manifest.json or journal. It
// writes only its own evidence (prompt.txt, response.json) and the diff
// artifact, and returns everything else as a Patch for the dispatcher to
// apply.
package code

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
)

// Errors returned by this package. Every one of them reports a condition a
// retry cannot fix — a missing adapter, an unapproved plan, an unreadable
// artifact — which is why they are Go errors rather than a Result carrying
// journal.StatusFailed: StatusFailed routes into the fix loop, and none of
// these is something the fix loop can repair by editing code.
var (
	// ErrNoRunContext reports a nil *graph.RunContext. Only a broken
	// dispatcher can produce one; the node reports it instead of
	// panicking inside an orchestrator that may be hours into a run.
	ErrNoRunContext = errors.New("belay/code: nil run context")

	// ErrNoAgent reports that RunContext.Agent was nil. The code node
	// cannot degrade to "no agent" the way a review node can degrade to a
	// linter — an agent is its entire reason to exist.
	ErrNoAgent = errors.New("belay/code: no agent backend configured")

	// ErrPlanNotApproved reports that the run requires plan approval and
	// the plan has not been approved. Reaching the code node in that
	// state is a graph wiring bug: the approve node should have paused
	// the run first.
	ErrPlanNotApproved = errors.New("belay/code: plan is not approved")

	// ErrPlanUnreadable reports that the plan artifact named by
	// State.Plan.Path could not be read. Recover the path with
	// errors.As against *PlanError.
	ErrPlanUnreadable = errors.New("belay/code: plan artifact is unreadable")

	// ErrAgent reports that AgentBackend.Invoke failed. It always wraps
	// the backend's own error too, so errors.Is against
	// belay.ErrToolchainMissing, belay.ErrBudgetExceeded and
	// belay.ErrUnsupported still works through it — the fix loop and the
	// budget guard both depend on that.
	ErrAgent = errors.New("belay/code: agent invocation failed")

	// ErrWorkspace reports a Layout whose run directory is not shaped
	// like <workspace>/.belay/runs/<run id>, leaving the node unable to
	// tell the agent which repository to read.
	ErrWorkspace = errors.New("belay/code: cannot resolve the workspace directory")
)

// Reasons carried inside a *PlanError for the two failures that are not an
// underlying I/O error.
var (
	errPlanPath  = errors.New("not a run artifact path")
	errPlanEmpty = errors.New("artifact is empty")
)

// PlanError reports which plan artifact the code node could not read, and
// why. It wraps ErrPlanUnreadable, so callers can detect the condition
// generically with
//
//	errors.Is(err, code.ErrPlanUnreadable)
//
// or recover the offending path with
//
//	var pe *code.PlanError
//	errors.As(err, &pe)
type PlanError struct {
	// Path is the value of State.Plan.Path that could not be resolved to
	// a readable artifact. It may be empty, which is itself the failure.
	Path string
	// Err is the underlying reason: an os error from ReadArtifact, or a
	// package-internal reason such as "artifact is empty".
	Err error
}

// Error implements error.
func (e *PlanError) Error() string {
	path := e.Path
	if path == "" {
		path = "(unset)"
	}
	if e.Err != nil {
		return fmt.Sprintf("belay/code: cannot read plan artifact %q: %v", path, e.Err)
	}
	return fmt.Sprintf("belay/code: cannot read plan artifact %q", path)
}

// Unwrap reports ErrPlanUnreadable and, when present, the underlying
// reason, so errors.Is reaches both.
func (e *PlanError) Unwrap() []error {
	if e.Err != nil {
		return []error{ErrPlanUnreadable, e.Err}
	}
	return []error{ErrPlanUnreadable}
}

// Node is the graph's code step. It holds no state, so one value is safe
// to register once and run many times, including concurrently from a
// best-of-N fanout: everything it needs arrives in the RunContext.
type Node struct{}

// New returns a code Node.
func New() *Node { return &Node{} }

// Name implements graph.Node and always returns graph.NodeCode.
func (*Node) Name() string { return graph.NodeCode }

var _ graph.Node = (*Node)(nil)

// Run implements graph.Node: it reads the approved plan, asks the agent to
// implement it, archives the prompt and the response, and returns the
// proposed change as a state.Patch routed to the write node.
//
// Run is safe to re-execute. Every file it writes has a name derived from
// the RunContext (this execution's node directory, and a diff artifact
// numbered by Step), so a second run after a crash overwrites its own
// previous output in place rather than accumulating a second copy of it.
func (*Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc == nil {
		return graph.Result{}, ErrNoRunContext
	}
	if err := ctx.Err(); err != nil {
		return graph.Result{}, fmt.Errorf("belay/code: canceled before invoking the agent: %w", err)
	}
	if rc.Agent == nil {
		return graph.Result{}, ErrNoAgent
	}
	if rc.Config.Graph.Approval && !rc.State.Plan.Approved {
		return graph.Result{}, fmt.Errorf(
			"%w: graph.approval is enabled but state.Plan.Approved is false for plan %q",
			ErrPlanNotApproved, rc.State.Plan.Path)
	}

	plan, err := readPlan(rc)
	if err != nil {
		return graph.Result{}, err
	}
	workDir, err := workspaceDir(rc.Layout)
	if err != nil {
		return graph.Result{}, err
	}

	req := buildRequest(rc, workDir, plan)
	if err := rc.WriteNodeFile(promptFile, []byte(req.Prompt)); err != nil {
		return graph.Result{}, err
	}

	log := logger(rc)
	log.Debug("code: invoking agent",
		"backend", rc.Agent.Name(),
		"resumed_session", req.SessionID != "",
		"plan", rc.State.Plan.Path)

	resp, err := rc.Agent.Invoke(ctx, req)
	if err != nil {
		return graph.Result{}, fmt.Errorf("%w: backend %q: %w", ErrAgent, rc.Agent.Name(), err)
	}
	if err := writeResponse(rc, resp); err != nil {
		return graph.Result{}, err
	}

	diff := extractDiff(resp.Text)
	code, err := recordChange(rc, resp, diff)
	if err != nil {
		return graph.Result{}, err
	}
	log.Info("code: agent replied",
		"backend", rc.Agent.Name(),
		"turns", resp.Turns,
		"proposed_diff", diff != "",
		"files", len(code.ChangedFiles),
		"diff_artifact", code.LastDiff,
		"usd", resp.Usage.USD)

	return graph.Result{
		Next:   graph.NodeWrite,
		Status: journal.StatusOK,
		Patch:  state.Patch{Code: &code},
		Usage:  resp.Usage,
		Note:   note(rc.Agent.Name(), req.SessionID != "", diff != "", code),
	}, nil
}

// recordChange archives whatever change resp proposes and folds it, with
// the session id, into the run's Code record.
//
// Prior values are carried forward rather than overwritten with zero,
// because state.Patch.Code is an absolute set of the whole struct: a
// backend that reports no SessionID means "I have no session concept", not
// "forget the session you already have", and an invocation that proposes
// no new diff leaves the previously archived diff as the most recent one.
// LastDiff and ChangedFiles move together for the same reason — they
// describe one patch, and a Code record pairing attempt two's file list
// with attempt one's diff would be worse than either alone.
func recordChange(rc *graph.RunContext, resp belay.AgentResponse, diff string) (state.Code, error) {
	code := rc.State.Code
	code.ChangedFiles = cloneFiles(code.ChangedFiles)
	if resp.SessionID != "" {
		code.SessionID = resp.SessionID
	}
	if diff == "" {
		return code, nil
	}

	rel, err := rc.WriteArtifact(diffArtifactName(rc.Step), []byte(diff))
	if err != nil {
		return state.Code{}, err
	}
	code.LastDiff = rel
	code.ChangedFiles = changedFiles(diff)
	return code, nil
}

// cloneFiles copies a changed-file list, preserving nil, so a Patch never
// aliases the RunContext's own State snapshot: state.Patch.Code assigns
// the struct wholesale, which would otherwise leave two State values
// sharing one backing array.
func cloneFiles(files []string) []string {
	if files == nil {
		return nil
	}
	out := make([]string, len(files))
	copy(out, files)
	return out
}

// note composes Result.Note.
//
// It deliberately reports only structural facts and never quotes the
// agent's own text. graph.Result.Note is surfaced in the journal and the
// timeline and must already be redacted; the node cannot vouch for
// arbitrary model output, so it says nothing it did not compute itself.
func note(backend string, resumed, proposed bool, code state.Code) string {
	session := "new session"
	if resumed {
		session = "resumed session"
	}
	if !proposed {
		return fmt.Sprintf("code: %s proposed no change (%s)", backend, session)
	}
	return fmt.Sprintf("code: %s proposed a change to %d file(s) (%s)",
		backend, len(code.ChangedFiles), session)
}

// readPlan returns the text of the plan artifact named by State.Plan.Path.
func readPlan(rc *graph.RunContext) (string, error) {
	path := rc.State.Plan.Path
	name, ok := artifactName(path)
	if !ok {
		return "", &PlanError{Path: path, Err: errPlanPath}
	}
	data, err := rc.ReadArtifact(name)
	if err != nil {
		return "", &PlanError{Path: path, Err: err}
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", &PlanError{Path: path, Err: errPlanEmpty}
	}
	return string(data), nil
}

// artifactName maps the run-relative path State.Plan.Path holds onto the
// bare name RunContext.ReadArtifact expects.
//
// WriteArtifact returns paths of the form "artifacts/plan.md" while
// ReadArtifact takes "plan.md", so the two do not compose without this
// step. Anything that is not a bare artifact name — an absolute path, a
// nested path, a traversal — is rejected rather than repaired, matching
// state.Layout's own refusal to sanitize path segments.
func artifactName(path string) (string, bool) {
	name := filepath.ToSlash(strings.TrimSpace(path))
	name = strings.TrimPrefix(name, "artifacts/")
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

// workspaceDir recovers the workspace root from a run Layout.
//
// state.NewLayout builds a run directory as <workspace>/.belay/runs/<run
// id> and Layout deliberately exposes only paths inside it, but the agent
// must be pointed at the workspace itself. Inverting NewLayout is safer
// than wiring the workspace path into the node separately, where the two
// copies could drift apart; the shape is verified rather than assumed, so
// a Layout built some other way fails loudly instead of pointing the agent
// at an arbitrary parent directory.
func workspaceDir(l state.Layout) (string, error) {
	runDir := l.RunDir()
	runsDir := filepath.Dir(runDir)
	belayDir := filepath.Dir(runsDir)
	ws := filepath.Dir(belayDir)
	if filepath.Base(runsDir) != "runs" || filepath.Base(belayDir) != ".belay" {
		return "", fmt.Errorf("%w: run directory %q is not <workspace>/.belay/runs/<run id>",
			ErrWorkspace, runDir)
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", ErrWorkspace, ws, err)
	}
	return abs, nil
}

// writeResponse archives the backend's response as this execution's
// response.json.
func writeResponse(rc *graph.RunContext, resp belay.AgentResponse) error {
	// AgentResponse.Raw is opaque, backend-specific bytes; the contract
	// does not promise they parse as JSON. Encoding an invalid Raw would
	// fail the whole node over a debugging artifact, so it is dropped
	// instead — the fields the graph actually reads are still archived.
	if len(resp.Raw) > 0 && !json.Valid(resp.Raw) {
		resp.Raw = nil
	}
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return fmt.Errorf("belay/code: encode %s: %w", responseFile, err)
	}
	return rc.WriteNodeFile(responseFile, append(data, '\n'))
}

// logger returns rc's logger, or slog.Default when the dispatcher supplied
// none, matching the convention every other belay adapter uses.
func logger(rc *graph.RunContext) *slog.Logger {
	if rc.Logger != nil {
		return rc.Logger
	}
	return slog.Default()
}
