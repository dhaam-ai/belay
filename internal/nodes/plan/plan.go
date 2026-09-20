// Package plan implements belay's entry node: the step that turns a run's
// goal into a written implementation plan before any code is touched.
//
// # What it does
//
// Run asks the configured belay.AgentBackend for a plan, archives the
// answer as the run's plan.md artifact, and returns a state.Patch carrying
// that artifact's run-relative path and a content digest. It routes to the
// approve node when config.Graph.Approval is set, or when it is answering
// a request for a new plan, and straight to the code node otherwise.
// Approved always starts false: this node records that a plan exists,
// never that anyone agreed with it.
//
// # Purity
//
// Per ADR-0002 the node writes no state.json, no manifest.json and no
// journal record. The two things it does write are content rather than
// control state — the plan.md artifact, and this execution's evidence
// files under the node's own directory — and neither can corrupt a resume.
//
// # Evidence
//
// Every execution leaves three files behind, via RunContext.WriteNodeFile:
//
//	prompt.txt     the Prompt field exactly as sent, byte for byte
//	request.json   the whole belay.AgentRequest, including SystemPrompt
//	response.json  the whole belay.AgentResponse, including Usage and Raw
//
// prompt.txt is verbatim so that a human can replay it against the backend
// without unpicking a wrapper, and request.json exists because the system
// prompt is half the model's input: a plan that came out wrong cannot be
// diagnosed from the user prompt alone, and the system prompt is a
// constant of whichever belay version happened to run.
//
// # Asking again
//
// The same node writes a run's second plan, and its third. A person at the
// approve gate who wants a different plan records a state.Regeneration
// request, and the gate routes back here (see internal/nodes/approve). The
// node reads the request's guidance into a "# Revision" section of the
// prompt, writes the answer over the same plan.md, and marks the request
// served in its Patch.
//
// Three details are load-bearing:
//
//   - It keys on Regenerate.Request, not Regenerate.Pending. The Patch that
//     marks the request served may be applied and durably saved before the
//     dispatcher journals this node's finish; the re-run after a crash in
//     that window then reads a request that already looks served. Keying on
//     Pending would send that re-run an ordinary plan prompt, dropping the
//     one thing the person said was wrong. Keying on Request asks the same
//     question both times.
//   - A run answering a request routes to the approve gate whatever
//     graph.approval says now. The request exists only because somebody was
//     standing at that gate; sending them to the code node instead would
//     spend their budget on a plan they have never seen.
//   - A revision the backend cannot fill does not end the run. See
//     previousPlanStands: unlike a first plan, there is something to fall
//     back to.
//
// The node never writes the snapshot of the plan it replaces. Whoever
// records the request writes it first, under ReplacedArtifact's name, so
// that no single interrupted execution can lose both documents.
//
// # Failure
//
// A backend that fails is a Go error, never a StatusFailed Result. The
// plan node is the graph's entry point: it has no fallback branch to take
// and nothing downstream can proceed without a plan, so reporting failure
// as a routable Result would only invite the dispatcher to route somewhere
// there is no point going. Errors from the backend are wrapped with %w, so
// errors.Is(err, belay.ErrToolchainMissing) still holds at the dispatcher
// and "claude is not installed" stays distinguishable from "the model
// produced nothing useful" (ErrEmptyPlan).
package plan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Artifact and evidence file names this node writes.
const (
	// ArtifactName is the run artifact the plan document is archived as.
	// It is a stable name, not a per-attempt one: re-running the node
	// after a crash must land on the same path rather than accumulate
	// plan-1.md, plan-2.md for one logical plan.
	ArtifactName = "plan.md"
	// ReplacedPrefix begins the name of the artifact holding the plan a
	// request for a new one replaced; see ReplacedArtifact.
	ReplacedPrefix = "plan-replaced-"
	// PromptFile is the node file holding belay.AgentRequest.Prompt
	// exactly as sent.
	PromptFile = "prompt.txt"
	// RequestFile is the node file holding the whole belay.AgentRequest as
	// JSON, so the system prompt and model that produced a plan are
	// recoverable after the fact.
	RequestFile = "request.json"
	// ResponseFile is the node file holding the whole belay.AgentResponse
	// as JSON, including Usage and the backend's own Raw output.
	ResponseFile = "response.json"
)

// ReplacedArtifact names the artifact holding the plan that request
// replaced: "plan-replaced-1.md" for the first request of a run, and so on.
//
// One name per request, rather than one "plan-previous.md" overwritten
// each time, because a person who asked twice has two plans behind them
// and may want either. The snapshot is written by whoever records the
// request, before the plan node runs, so that the document the person was
// reading survives being replaced. It is never written by this node: a
// node that both replaced the plan and kept the copy could lose both in
// one interrupted execution.
func ReplacedArtifact(request int) string {
	return fmt.Sprintf("%s%d.md", ReplacedPrefix, request)
}

// DigestPrefix labels the hash algorithm state.Plan.Digest is computed
// with, matching config.Config.Digest's own "sha256:<hex>" spelling.
const DigestPrefix = "sha256:"

// Errors this node returns. Each is a Go error rather than a StatusFailed
// Result, for the reason given in the package doc.
var (
	// ErrNoAgent reports that RunContext.Agent was nil. Every adapter on a
	// RunContext is allowed to be nil when configuration selects none, so
	// the node that needs one says so instead of panicking.
	ErrNoAgent = errors.New("plan: no agent backend configured")

	// ErrEmptyPlan reports that the backend returned nothing usable — an
	// empty or whitespace-only response. It is deliberately not an empty
	// plan.md: an empty artifact would satisfy every downstream path check
	// and turn "the model said nothing" into a plan the approve node
	// shows a human as if it were real.
	ErrEmptyPlan = errors.New("plan: agent returned an empty plan")

	// ErrNoWorkspace reports that the run layout does not resolve to a
	// workspace directory — see workspaceDir. belay.AgentRequest.WorkDir
	// is required and must be the repository the agent plans against;
	// guessing at a relative path would point the agent at whatever
	// directory the dispatcher happened to start in.
	ErrNoWorkspace = errors.New("plan: run layout has no workspace directory")
)

// readOnlyTools is the tool set the planning agent is restricted to.
//
// Read, Glob and Grep are Claude Code's read-only tools; Edit, Write, Bash
// and NotebookEdit are the ones that change a repository, and none of them
// appear here. Restricting the request is belt to the system prompt's
// braces: the instruction not to edit code is advice, whereas an allowlist
// that omits every mutating tool is enforced by the backend.
//
// Source: https://code.claude.com/docs/en/permissions — the permission
// system table lists "Read-only | File reads, Grep" as the tier needing no
// approval, and the file-permission section names Read, Edit, Write, Glob,
// Grep and NotebookEdit as the built-in file tools.
//
// A backend that does not recognize these names is expected to ignore them
// (belay.AgentRequest.AllowedTools is documented as backend-specific);
// belay ships Claude Code as its only backend in v0.1 per ADR-0005.
func readOnlyTools() []string { return []string{"Read", "Glob", "Grep"} }

// Node is the plan node. Construct one with New.
//
// It holds no state: everything an execution needs arrives on the
// RunContext, which is what makes the node safe to register once and
// re-run any number of times.
type Node struct{}

// New returns the plan node.
func New() *Node { return &Node{} }

// Name returns graph.NodePlan.
func (*Node) Name() string { return graph.NodePlan }

// Run asks the agent for an implementation plan, archives it as the
// ArtifactName artifact, and returns the routing decision plus a
// state.Patch setting state.Plan.
//
// Routing is the only branch: graph.NodeApprove when
// rc.Config.Graph.Approval is true, graph.NodeCode when it is false.
// Result.Usage carries the backend's own belay.Usage so the dispatcher's
// budget ledger sees what this node cost — a node that drops it is a node
// the budget guard cannot see.
//
// Run is safe to re-run. It writes the plan to a fixed artifact name with
// a truncating write, so a second execution replaces the first execution's
// document rather than appending to it or creating a second one, and
// returns the same path.
func (*Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if err := ctx.Err(); err != nil {
		return graph.Result{}, fmt.Errorf("plan: %w", err)
	}
	if rc.Agent == nil {
		return graph.Result{}, ErrNoAgent
	}
	workDir, err := workspaceDir(rc)
	if err != nil {
		return graph.Result{}, err
	}

	req := belay.AgentRequest{
		Prompt:       userPrompt(rc.Goal, workDir, rc.State.Plan.Regenerate),
		SystemPrompt: SystemPrompt,
		WorkDir:      workDir,
		AllowedTools: readOnlyTools(),
		MaxTurns:     rc.Config.Agent.MaxTurns,
		Model:        rc.Config.Agent.ModelFor(graph.NodePlan),
	}
	if err := writeRequestEvidence(rc, req); err != nil {
		return graph.Result{}, err
	}

	request := rc.State.Plan.Regenerate
	resp, invokeErr := rc.Agent.Invoke(ctx, req)
	// The response is recorded before the error is acted on: a backend may
	// return a populated response alongside an error (its own budget
	// guard, for one — see belay.AgentBackend.Invoke), and that partial
	// output is exactly what someone diagnosing the failure wants.
	//
	// The same partial response is why every error return below carries
	// resp.Usage: what the backend reports having spent it has spent, and
	// graph.Result.Usage is the one field the dispatcher reads from a
	// Result it rejects.
	writeErr := writeResponseEvidence(rc, resp)
	if invokeErr != nil {
		if res, ok := previousPlanStands(rc, request, resp.Usage); ok {
			return res, nil
		}
		return graph.Result{Usage: resp.Usage}, fmt.Errorf("plan: agent %q: %w", rc.Agent.Name(), invokeErr)
	}
	if writeErr != nil {
		return graph.Result{Usage: resp.Usage}, writeErr
	}

	text := strings.TrimSpace(resp.Text)
	if text == "" {
		if res, ok := previousPlanStands(rc, request, resp.Usage); ok {
			return res, nil
		}
		return graph.Result{Usage: resp.Usage}, fmt.Errorf("%w: backend %q returned %d byte(s) of whitespace in %d turn(s)",
			ErrEmptyPlan, rc.Agent.Name(), len(resp.Text), resp.Turns)
	}
	// Trimmed, with exactly one trailing newline: the digest below covers
	// the bytes on disk, so normalizing here is what stops two runs whose
	// answers differed only in trailing blank lines from reading as two
	// different plans.
	doc := []byte(text + "\n")

	path, err := rc.WriteArtifact(ArtifactName, doc)
	if err != nil {
		return graph.Result{Usage: resp.Usage}, fmt.Errorf("plan: archive plan: %w", err)
	}

	// A run answering a request for a new plan goes back to the gate
	// whatever graph.approval says now. The request can only exist because
	// somebody was standing at that gate reading the last plan; sending
	// them to the code node instead would spend their budget on a plan
	// they have never seen. graph.approval is read from belay.yaml on every
	// invocation, so "now" and "when the run started" are not the same
	// question.
	next := graph.NodeCode
	if rc.Config.Graph.Approval || request.Request > 0 {
		next = graph.NodeApprove
	}

	logger(rc).Debug("plan written",
		"path", path, "bytes", len(doc), "next", next, "usd", resp.Usage.USD)

	return graph.Result{
		Next:   next,
		Status: journal.StatusOK,
		Patch: state.Patch{Plan: &state.Plan{
			Path: path,
			// Approved is false by design. Only the approve node, or a
			// configuration that skips it, may say otherwise.
			Approved: false,
			Digest:   Digest(doc),
			// Serving the request is recorded here, in the Patch of the
			// node that served it, because a node writes no state itself
			// (ADR-0002) and because Patch fields are absolute sets: a
			// Plan built without it would silently drop the request, and
			// the re-run after a crash would ask the agent a question
			// missing the one thing the person said was wrong.
			Regenerate: served(request),
		}},
		Usage: resp.Usage,
		// Note carries no agent text: it is journalled and rendered, and
		// belay only guarantees redaction for captured subprocess output,
		// not for a note a node composes itself.
		Note: fmt.Sprintf("plan archived to %s (%d bytes); next %s", path, len(doc), next),
	}, nil
}

// Digest returns the fingerprint recorded in state.Plan.Digest for the
// exact bytes archived as the plan artifact: DigestPrefix followed by the
// hex SHA-256 of plan.
//
// It is exported so that a later reader — the approve node, or a human
// checking whether the plan they signed off on is the plan on disk — can
// recompute the same value from the artifact's contents rather than
// reimplementing the formula and hoping it matches.
func Digest(plan []byte) string {
	sum := sha256.Sum256(plan)
	return DigestPrefix + hex.EncodeToString(sum[:])
}

// workspaceDir recovers the target repository root from a run's Layout.
//
// state.Layout exposes paths inside the run directory, not the workspace
// it was built from, and belay.AgentRequest.WorkDir needs the latter.
// state.NewLayout builds its root as
// filepath.Join(workspaceDir, ".belay", "runs", runID), so three parents
// up from RunDir is that workspace — see TestWorkspaceDirMatchesNewLayout,
// which fails if that construction ever changes.
//
// A result that is not absolute means the Layout was never built by
// NewLayout (the zero Layout, typically), and is reported rather than
// passed to a backend that would resolve it against its own process
// directory.
func workspaceDir(rc *graph.RunContext) (string, error) {
	if rc.Workspace == "" {
		return "", ErrNoWorkspace
	}
	return rc.Workspace, nil
}

// writeRequestEvidence records the prompt exactly as sent, plus the whole
// request, before the backend is called — so a run killed mid-invocation
// still shows what it was asked to do.
func writeRequestEvidence(rc *graph.RunContext, req belay.AgentRequest) error {
	if err := rc.WriteNodeFile(PromptFile, []byte(req.Prompt)); err != nil {
		return fmt.Errorf("plan: record prompt: %w", err)
	}
	data, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return fmt.Errorf("plan: encode request: %w", err)
	}
	if err := rc.WriteNodeFile(RequestFile, append(data, '\n')); err != nil {
		return fmt.Errorf("plan: record request: %w", err)
	}
	return nil
}

// writeResponseEvidence records the whole response as JSON.
//
// belay.AgentResponse.Raw is backend-defined and only promised to be that
// backend's own output; a backend that puts something json.Marshal cannot
// encode there must not cost the run its plan, so Raw is dropped (with a
// warning) rather than allowed to fail the node.
func writeResponseEvidence(rc *graph.RunContext, resp belay.AgentResponse) error {
	if len(resp.Raw) > 0 && !json.Valid(resp.Raw) {
		logger(rc).Warn("plan: agent response Raw is not valid JSON; recording the rest",
			"bytes", len(resp.Raw))
		resp.Raw = nil
	}
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return fmt.Errorf("plan: encode response: %w", err)
	}
	if err := rc.WriteNodeFile(ResponseFile, append(data, '\n')); err != nil {
		return fmt.Errorf("plan: record response: %w", err)
	}
	return nil
}

// logger returns rc's execution-scoped logger, falling back to the default
// one. A missing logger is a dispatcher wiring slip; it must not turn into
// a nil dereference inside a node.
func logger(rc *graph.RunContext) *slog.Logger {
	if rc.Logger != nil {
		return rc.Logger
	}
	return slog.Default()
}

var _ graph.Node = (*Node)(nil)

// served returns r with its outstanding request marked answered.
//
// It never lowers Served. A document whose Served already ran ahead of
// Request is nonsense, but rewriting it downwards would make this node the
// thing that wrote the nonsense, and state.Regeneration's whole protocol is
// that neither counter goes backwards.
func served(r state.Regeneration) state.Regeneration {
	if r.Served < r.Request {
		r.Served = r.Request
	}
	return r
}

// previousPlanStands answers a request for a new plan that the backend
// could not fill, with the plan the person already had.
//
// A first plan has no fallback: nothing downstream can proceed without a
// plan, so a backend that fails there is an error and the run ends (see
// the package doc). A revision is the one case where that reasoning does
// not hold. There is a plan on disk -- the one the person was reading when
// they asked for another -- so ending the run would take a good plan away
// from them as punishment for a backend that timed out, and `belay resume`
// refuses a run that has ended.
//
// So: route back to the gate, with the request marked served so the gate
// does not send the run round the same loop again, and with the plan the
// person had left exactly where it was. The check is deliberately the
// gate's own (approve.CheckPlan, approve.PlanArtifact): if what is on disk
// is not something the gate would show a person, there is nothing to fall
// back to and the caller's error stands.
//
// ok is false when the error should be returned as it always was.
func previousPlanStands(rc *graph.RunContext, request state.Regeneration, usage belay.Usage) (graph.Result, bool) {
	if request.Request == 0 {
		return graph.Result{}, false
	}
	name, _, err := approve.PlanArtifact(rc.Layout, rc.State.Plan.Path)
	if err != nil {
		return graph.Result{}, false
	}
	data, err := rc.ReadArtifact(name)
	if err != nil {
		return graph.Result{}, false
	}
	if err := approve.CheckPlan(data); err != nil {
		return graph.Result{}, false
	}

	plan := rc.State.Plan
	plan.Digest = Digest(data)
	// Unapproved, whatever it was before: what the person approved, if
	// they ever did, is not what they asked for, and the gate is where
	// they say whether the plan they still have will do.
	plan.Approved = false
	plan.Regenerate = served(request)

	logger(rc).Warn("no new plan was written; the previous plan stands",
		"request", request.Request, "plan", rc.State.Plan.Path)

	return graph.Result{
		Next:   graph.NodeApprove,
		Status: journal.StatusOK,
		Patch:  state.Patch{Plan: &plan},
		Usage:  usage,
		Note: fmt.Sprintf("No new plan could be written for request %d, so the previous plan stands; what the backend returned is in this step's %s.",
			request.Request, ResponseFile),
	}, true
}
