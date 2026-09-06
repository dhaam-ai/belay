// Package code implements belay's code node: the graph step that hands an
// approved plan to the agent backend, which implements it by editing the
// workspace in place, and records which files the agent says it touched.
//
// # Where the change is made
//
// Here. The agent is invoked with an allow-list that includes Edit and
// Write and is told to change the files itself; nothing downstream applies
// a patch on its behalf. That follows from what belay actually ships rather
// than from preference:
//
//   - Per ADR-0005 the only v0.1 backend is Claude Code, and the adapter in
//     internal/agent/claude runs it with AgentRequest.WorkDir as its working
//     directory. It edits in place natively. Asking it for a patch instead
//     would mean paying a model to describe an edit it is already able to
//     make.
//
//   - internal/isolate/dircopy deliberately omits .git from a candidate
//     workspace, so a write node built on "git apply" would be broken for
//     every best-of-N candidate. The alternative — hand-writing a patch
//     applier with no dependencies — is a large, error-prone thing to own
//     for no gain.
//
//   - A node that mutates nothing is idempotent by construction, which is
//     what ADR-0002 wants from the node the dispatcher re-executes after a
//     crash. Concentrating the mutation here leaves the write node with
//     that property.
//
// So the division of labour is: this node changes the workspace and reports
// what changed; internal/nodes/write verifies that report against the disk
// and records the authoritative materialization patch. It is not a
// propose/apply split — it is a change/verify split.
//
// # The changed-file report
//
// The prompt asks for a fenced block tagged "changed-files" holding one
// repository-relative path per line, and that block is parsed back into
// state.Code.ChangedFiles. Its accuracy is load-bearing in both directions:
// the write node verifies every path in it and refuses the whole set if one
// escapes the workspace, and a file the agent changed but did not list is
// recorded nowhere and invisible to everything downstream. A unified diff
// is still harvested as a fallback when a backend volunteers one; see
// changeSet for the precedence, and cleanPath for what is refused outright.
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
// writes only its own evidence (prompt.txt, response.json) and the summary
// artifact, and returns everything else as a Patch for the dispatcher to
// apply. It does not set state.Code.LastDiff: that field names the
// materialization record, which the write node produces, and this node has
// no patch to put there.
package code

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
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

	// ErrFileList reports that the agent's reply named a path this node
	// will not pass on: an absolute path, one that climbs out of the
	// repository with "..", or one that names no file at all. Recover the
	// offending entry with errors.As against *FileListError.
	//
	// It is a hard error rather than a StatusFailed Result for the same
	// reason as the rest of this list: the fix loop repairs code, and no
	// amount of editing repairs a reply that misreported which files it
	// touched.
	ErrFileList = errors.New("belay/code: the reply named an unusable path")

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

// FileListError reports one path in the agent's reply that this node refused
// to record, and why. It wraps ErrFileList, so callers can detect the
// condition generically with
//
//	errors.Is(err, code.ErrFileList)
//
// or recover the offending entry with
//
//	var fe *code.FileListError
//	errors.As(err, &fe)
type FileListError struct {
	// Path is the entry exactly as the reply wrote it, before trimming or
	// cleaning, so the message shows what the agent actually emitted.
	Path string
	// Source names where the entry came from — the changed-files block, or
	// a unified diff recovered as a fallback. The two are different
	// diagnoses: the first is an agent ignoring an explicit instruction,
	// the second is salvage from output that was never asked for.
	Source string
	// Reason is a short, human-readable explanation.
	Reason string
}

// Error implements error.
func (e *FileListError) Error() string {
	return fmt.Sprintf("belay/code: refused path %q from %s: %s", e.Path, e.Source, e.Reason)
}

// Unwrap reports ErrFileList.
func (e *FileListError) Unwrap() error { return ErrFileList }

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
// implement it in the workspace, archives the prompt, the response and the
// agent's summary, and returns the reported change as a state.Patch routed
// to the write node.
//
// Run is safe to re-execute in the sense ADR-0002 requires of it: every
// file it writes has a name derived from the RunContext (this execution's
// node directory, and a summary artifact numbered by Step), so a second run
// after a crash overwrites its own previous output in place rather than
// accumulating a second copy of it. The workspace is a different matter —
// re-running means a second agent invocation, which edits files again. That
// is inherent to the node that owns the mutation, and it is why the write
// node, not this one, is the graph's idempotent record-keeper.
//
// # Errors
//
// A malformed changed-file report is a Go error wrapping ErrFileList, not a
// StatusFailed Result. By then the agent has already edited the workspace,
// and those edits stay on disk: the run stops with prompt.txt,
// response.json and the summary artifact all archived, which is what a
// post-mortem needs to see what was asked and what the agent believed it
// did.
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
	workDir, err := workspaceDir(rc)
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
	summary, err := rc.WriteArtifact(summaryArtifactName(rc.Step), summaryBytes(resp.Text))
	if err != nil {
		return graph.Result{}, err
	}

	files, err := changeSet(resp.Text)
	if err != nil {
		return graph.Result{}, err
	}
	code, err := recordChange(rc, resp, files)
	if err != nil {
		return graph.Result{}, err
	}
	log.Info("code: agent replied",
		"backend", rc.Agent.Name(),
		"turns", resp.Turns,
		"reported_change", files != nil,
		"files", len(code.ChangedFiles),
		"summary_artifact", summary,
		"usd", resp.Usage.USD)

	return graph.Result{
		Next:   graph.NodeWrite,
		Status: journal.StatusOK,
		Patch:  state.Patch{Code: &code},
		Usage:  resp.Usage,
		Note:   note(rc.Agent.Name(), req.SessionID != "", files != nil, code),
	}, nil
}

// recordChange folds the reported change, and the session id, into the run's
// Code record.
//
// Prior values are carried forward rather than overwritten with zero,
// because state.Patch.Code is an absolute set of the whole struct: a
// backend that reports no SessionID means "I have no session concept", not
// "forget the session you already have", and an invocation that reports no
// files leaves the previously recorded change standing as the most recent
// one. A nil files is exactly that "reported nothing" case, and it is
// distinct from an empty non-nil slice, which is a considered claim that
// the set is empty.
func recordChange(rc *graph.RunContext, resp belay.AgentResponse, files []string) (state.Code, error) {
	code := rc.State.Code
	code.ChangedFiles = cloneFiles(code.ChangedFiles)
	if resp.SessionID != "" {
		code.SessionID = resp.SessionID
	}
	if files == nil {
		return code, nil
	}
	code.ChangedFiles = files
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
func note(backend string, resumed, reported bool, code state.Code) string {
	session := "new session"
	if resumed {
		session = "resumed session"
	}
	if !reported {
		return fmt.Sprintf("code: %s reported no change (%s)", backend, session)
	}
	return fmt.Sprintf("code: %s reported changes to %d file(s) (%s)",
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
// workspaceDir is the repository the agent edits.
//
// The dispatcher supplies it on the RunContext. It used to be reconstructed
// by walking up from the run directory, which made this node one of six
// places that had to know the ".belay/runs/<id>" shape -- and for an unset
// Layout every one of them resolved to ".", belay's own tree, which is the
// last directory an agent with edit tools should be pointed at.
func workspaceDir(rc *graph.RunContext) (string, error) {
	if rc.Workspace == "" {
		return "", fmt.Errorf("%w: run context names no workspace", ErrWorkspace)
	}
	return rc.Workspace, nil
}

// summaryArtifactName returns the artifact name for the summary the agent
// wrote at step.
//
// # Why this is not called a patch
//
// This node used to archive the reply as diff-%04d.patch, which was true
// while the reply was a patch and is a lie now that it is prose. The name
// also collided in spirit with the write node, which writes its own
// diff-%04d.patch: the two only stayed distinct because the nodes occupy
// different steps, so the artifacts directory held two files under one
// naming scheme meaning two different things. write owns the authoritative
// materialization record; what this node has to add is the agent's own
// account of what it did and why, which is the first thing a post-mortem
// wants and the one thing no amount of filesystem inspection can
// reconstruct. So it is archived under its own name, as Markdown, because
// that is what it is.
//
// Numbering by graph.RunContext.Step rather than by a fixed name is what
// keeps a fix loop's history intact: every pass through the code node runs
// at a later step, so attempt one's summary is still on disk when attempt
// two writes its own. Zero padding keeps the artifacts directory sorted the
// way the run actually happened.
func summaryArtifactName(step int) string { return fmt.Sprintf("summary-%04d.md", step) }

// summaryBytes normalizes a reply for archiving: trailing whitespace
// trimmed and exactly one final newline, so the artifact is a well-formed
// text file rather than whatever the backend happened to emit. A reply that
// is entirely whitespace archives as zero bytes, which is an honest record
// of a backend that said nothing.
func summaryBytes(text string) []byte {
	s := strings.TrimRight(text, " \t\r\n")
	if s == "" {
		return nil
	}
	return []byte(s + "\n")
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
