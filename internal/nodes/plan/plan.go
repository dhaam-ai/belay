// Package plan implements belay's entry node: the step that turns a run's
// goal into a written implementation plan before any code is touched.
//
// # What it does
//
// Run asks the configured belay.AgentBackend for a plan, archives the
// answer as the run's plan.md artifact, and returns a state.Patch carrying
// that artifact's run-relative path and a content digest. It routes to the
// approve node when config.Graph.Approval is set and straight to the code
// node when it is not. Approved always starts false: this node records
// that a plan exists, never that anyone agreed with it.
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

	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
)

// Artifact and evidence file names this node writes.
const (
	// ArtifactName is the run artifact the plan document is archived as.
	// It is a stable name, not a per-attempt one: re-running the node
	// after a crash must land on the same path rather than accumulate
	// plan-1.md, plan-2.md for one logical plan.
	ArtifactName = "plan.md"
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
		Prompt:       userPrompt(rc.Goal, workDir),
		SystemPrompt: SystemPrompt,
		WorkDir:      workDir,
		AllowedTools: readOnlyTools(),
		MaxTurns:     rc.Config.Agent.MaxTurns,
		Model:        rc.Config.Agent.Model,
	}
	if err := writeRequestEvidence(rc, req); err != nil {
		return graph.Result{}, err
	}

	resp, invokeErr := rc.Agent.Invoke(ctx, req)
	// The response is recorded before the error is acted on: a backend may
	// return a populated response alongside an error (its own budget
	// guard, for one — see belay.AgentBackend.Invoke), and that partial
	// output is exactly what someone diagnosing the failure wants.
	writeErr := writeResponseEvidence(rc, resp)
	if invokeErr != nil {
		return graph.Result{}, fmt.Errorf("plan: agent %q: %w", rc.Agent.Name(), invokeErr)
	}
	if writeErr != nil {
		return graph.Result{}, writeErr
	}

	text := strings.TrimSpace(resp.Text)
	if text == "" {
		return graph.Result{}, fmt.Errorf("%w: backend %q returned %d byte(s) of whitespace in %d turn(s)",
			ErrEmptyPlan, rc.Agent.Name(), len(resp.Text), resp.Turns)
	}
	// Trimmed, with exactly one trailing newline: the digest below covers
	// the bytes on disk, so normalizing here is what stops two runs whose
	// answers differed only in trailing blank lines from reading as two
	// different plans.
	doc := []byte(text + "\n")

	path, err := rc.WriteArtifact(ArtifactName, doc)
	if err != nil {
		return graph.Result{}, fmt.Errorf("plan: archive plan: %w", err)
	}

	next := graph.NodeCode
	if rc.Config.Graph.Approval {
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
