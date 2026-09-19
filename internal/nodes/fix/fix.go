// Package fix implements belay's repair node: the step that feeds a
// concrete failure back to the coding agent, and the step that decides when
// to stop trying.
//
// # Two entry paths, one node
//
// The graph reaches fix from a failing test node and from a failed review
// gate. Both are repairs, both resume the same agent session, and both
// route back to test afterwards, so they are one node with two prompts
// rather than two nodes. When both records look failing, the test failure
// wins — see detectCause for why a review computed against already-red code
// is not worth an attempt.
//
// # This node owns the give-up cap
//
// The test node deliberately routes here at the retry boundary instead of
// terminating the run itself, so that exactly one node decides when a run
// has tried enough. Run enforces graph.give_up before it spends anything:
// once State.Fix.Attempts has reached the cap it returns StatusFailed with
// a give_up_exhausted note and never calls the agent. An off-by-one here is
// expensive in both directions — one too many and the loop never ends, one
// too few and a run gives up with budget left — so the boundary is pinned
// by a table test across several cap values rather than reasoned about.
//
// # Attempts is an absolute count, and this node never resets it
//
// State.Fix.Attempts is set, not incremented: the Patch carries the new
// total, which is what lets the dispatcher re-apply it after a crash
// without inflating the count (ADR-0002). Run therefore computes
// Attempts+1 and sets it.
//
// It is not RunContext.Attempt. That counter is the dispatcher's own
// re-execution count for a single node — a crash resume runs the same fix
// attempt again — whereas State.Fix.Attempts counts distinct repairs across
// the whole loop. Capping on the wrong one would reset the retry budget
// every time the graph came back around to fix.
//
// Resetting the counter is a different decision, and it belongs to whoever
// observes a green outcome — the test node on a passing suite, the review
// node on a passing gate. fix cannot do it: it only ever runs while
// something is failing, so it never sees the green run that would justify a
// reset. If no node resets Attempts, a run that recovers and then breaks
// again much later inherits a nearly-exhausted budget and gives up almost
// immediately; that is a cross-node contract this package depends on and
// cannot enforce alone.
//
// # A repair adds to the files the run has changed
//
// State.Code.ChangedFiles is the run's record of which files its change
// touches. The review node hands it to a Reviewer, and this node's prompt
// lists it back to the agent. A repair edits the workspace too, so Run reads
// the changed-files block from the reply, with the code node's own parser
// (code.ParseChangedFilesBlock), and sets ChangedFiles to the union of the
// files already recorded and the files the repair reports: sorted, and each
// listed once. Code's other fields are carried through unchanged, because
// Patch.Code replaces the whole struct.
//
// The block is all it reads. The parser is shared with the code node; its
// fallback of salvaging paths out of any unified diff in a reply deliberately
// is not. This node's prompt quotes the failure it wants repaired, and a
// failure is routinely a unified diff — testify's "--- Expected / +++ Actual",
// a golden-file `diff -u` between two absolute paths — so a reply that quotes
// the failure back is the normal case here, where it never occurs in a reply
// to the code node. Salvaging from one would record a file the run never
// changed, or refuse the whole report over a path the agent only quoted, which
// ends the run for good after the repair was made and paid for. The cost is
// that a repair which emitted a diff instead of the block reports nothing; the
// prompt tells the agent that a file it does not list is recorded nowhere.
//
// This node forms the union, not the write node. write verifies a claim
// against the disk; belay keeps no pre-change baseline, so write has no way
// to learn what a repair touched except from a claim, and the only claim is
// the reply this node receives. For write to form it, this node would have to
// pass its report along in a second blackboard field that only write reads,
// and every repair would have to route through write, which changes the
// graph and what a repair costs against graph.max_steps. Neither would tell
// belay anything this node does not already know.
//
// It is a union, never a replacement. A repair's reply names what that
// repair touched, not what the run did, and replacing the list with it would
// drop the code node's files from the record while their edits sit on disk.
//
// It is sorted and each file listed once so that a re-run lands on the same
// list. Patch fields are absolute sets, and a crash can come after the
// dispatcher applied this node's Patch but before it journalled the finish.
// The re-run then starts from a list that already holds the repair's files,
// and the union of that with the same report is the same list, where an
// append would grow it and keeping arrival order could reorder it. A crash
// before the Patch was applied re-runs from the list as it was, and lands in
// the same place.
//
// Comparing the workspace before and after the agent call would not depend
// on the agent reporting honestly, but it does not converge. A re-run after a
// crash starts from a tree the first attempt already edited, so a file that
// attempt changed shows no change, and would drop out of the list.
//
// What this node does not do is check the reported paths against the disk.
// fix routes straight to test, so write never sees them. They pass the checks
// the code node applies to a path as written, so no absolute path, ".."
// segment or backslash gets in, and a reply that fails one is refused whole,
// as the code node refuses one. Whether a path is a regular file, or a
// symlink out of the tree, is a question about the disk that only write asks.
package fix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/code"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Errors returned by this node. Each one means the node could not reach a
// verdict at all, which is different from a repair that did not work: a
// failed repair is a Result the graph acts on, whereas these are wiring or
// environment problems a human has to resolve.
var (
	// ErrNilRunContext means Run was called without a RunContext. It is
	// always a dispatcher bug, and it is an error rather than a panic so
	// that one broken node cannot take a whole run's process down.
	ErrNilRunContext = errors.New("fix: nil run context")

	// ErrNoAgent means RunContext.Agent was nil. The fix node has no
	// degraded mode: repairing code is precisely what the agent is for.
	ErrNoAgent = errors.New("fix: no agent backend configured")

	// ErrNoFailureContext means neither a failing test run nor a failed
	// quality gate is recorded on the blackboard, so there is nothing
	// concrete to repair. Prompting an agent to "fix something" here would
	// spend a real attempt on a guess, so the node refuses instead.
	ErrNoFailureContext = errors.New("fix: no failing tests and no failed quality gate to repair")

	// ErrNoWorkspace means the workspace directory could not be derived
	// from the run layout — see workDir.
	ErrNoWorkspace = errors.New("fix: cannot derive the workspace directory from the run layout")
)

// Node files this node writes under its own execution directory. They are
// the evidence a human reads when a fix loop went wrong: the exact prompt
// sent, and the exact response received.
const (
	promptFile   = "prompt.txt"
	responseFile = "response.json"
)

// giveUpNote is the reason string a give-up Result carries. It is a stable
// token: the CLI and the timeline match on it, so it must not drift into
// prose.
const giveUpNote = "give_up_exhausted"

// systemPrompt frames the agent as a repairer rather than an author. The
// per-attempt instructions live in the prompt itself; this only sets the
// posture that a failing check is information, not an obstacle.
const systemPrompt = "You are a meticulous software engineer repairing your own work. " +
	"You diagnose before you edit, you change as little as possible, and you never make a " +
	"check pass by weakening the check."

// Node is the fix node: it feeds one concrete failure back to the coding
// agent, records the exchange, and routes back to the test node — or ends
// the run when the retry budget is gone.
//
// The zero Node is usable and carries no state of its own; everything it
// needs arrives in the RunContext.
type Node struct{}

// New returns the fix node.
func New() *Node { return &Node{} }

// Compile-time proof that Node satisfies the graph contract. Without it a
// signature drift in graph.Node would surface as a registration failure at
// runtime rather than a build failure here.
var _ graph.Node = (*Node)(nil)

// Name returns graph.NodeFix.
func (n *Node) Name() string { return graph.NodeFix }

// Run performs one fix attempt.
//
// It enforces the give-up cap first, before spending anything: at the cap
// it returns (Result{Status: StatusFailed, Note: "give_up_exhausted..."},
// nil) — a verdict, not an error, because a run that tried its budget and
// failed is an outcome the graph understands. Below the cap it builds a
// prompt from the actual failure, resumes the coding agent's session,
// writes prompt.txt and response.json, and routes back to the test node
// with State.Fix.Attempts set to its new total and State.Code.ChangedFiles
// grown by the files the repair reports (see the package documentation).
//
// A non-nil error means the attempt could not happen at all — no agent, no
// failure to repair, an unwritable run directory, or a backend that failed.
// Backend errors are wrapped, so errors.Is against belay.ErrToolchainMissing
// and friends still answers correctly at the dispatcher. A reply whose
// changed-file report names an unusable path is an error too, wrapping
// code.ErrFileList, for the code node's reason: the fix loop repairs code,
// and no edit repairs a report of which files were touched. By then the
// exchange is archived, and the edits it describes stay on disk.
func (n *Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc == nil {
		return graph.Result{}, ErrNilRunContext
	}
	if err := ctx.Err(); err != nil {
		return graph.Result{}, fmt.Errorf("fix: %w", err)
	}

	log := logger(rc)
	attempts := rc.State.Fix.Attempts
	giveUp := rc.Config.Graph.GiveUp
	c := detectCause(rc.State)

	if attempts >= giveUp {
		note := exhaustedNote(attempts, giveUp, c, rc.State)
		log.Warn("fix loop exhausted its retry budget",
			"attempts", attempts, "give_up", giveUp, "cause", c.String())
		return graph.Result{
			Status: journal.StatusFailed,
			Patch:  state.Patch{Fix: &state.Fix{Attempts: attempts, GiveUp: true}},
			Note:   note,
		}, nil
	}

	if c == causeNone {
		return graph.Result{}, ErrNoFailureContext
	}
	if rc.Agent == nil {
		return graph.Result{}, ErrNoAgent
	}
	dir, err := workDir(rc)
	if err != nil {
		return graph.Result{}, err
	}

	attempt := attempts + 1
	prompt := buildPrompt(promptInput{
		Goal:         goal(rc),
		Cause:        c,
		Attempt:      attempt,
		GiveUp:       giveUp,
		Test:         rc.State.Test,
		Review:       rc.State.Review,
		ChangedFiles: rc.State.Code.ChangedFiles,
	})

	// Written before the call, not after: an agent invocation can take
	// minutes and can be killed part-way, and the prompt is the first
	// thing a human needs in order to understand what belay asked for.
	if err := rc.WriteNodeFile(promptFile, []byte(prompt)); err != nil {
		return graph.Result{}, fmt.Errorf("fix: write %s: %w", promptFile, err)
	}

	req := belay.AgentRequest{
		Prompt:       prompt,
		SystemPrompt: systemPrompt,
		WorkDir:      dir,
		AllowedTools: editTools(),
		MaxTurns:     rc.Config.Agent.MaxTurns,
		Model:        rc.Config.Agent.ModelFor(graph.NodeFix),
		// Resuming the code node's session is what makes this a fix
		// rather than a fresh, contextless rewrite: the agent already
		// knows what it wrote and why.
		SessionID: rc.State.Code.SessionID,
	}

	resp, err := rc.Agent.Invoke(ctx, req)
	if err != nil {
		return graph.Result{}, fmt.Errorf("fix: agent %s: %w", rc.Agent.Name(), err)
	}

	if err := writeResponse(rc, resp); err != nil {
		return graph.Result{}, err
	}

	reported, err := code.ParseChangedFilesBlock(resp.Text)
	if err != nil {
		return graph.Result{}, fmt.Errorf("fix: the reply's changed-file report: %w", err)
	}
	changed := rc.State.Code
	changed.ChangedFiles = unionFiles(rc.State.Code.ChangedFiles, reported)

	log.Info("fix attempt completed",
		"attempt", attempt, "give_up", giveUp, "cause", c.String(),
		"session_resumed", req.SessionID != "", "reported_files", len(reported),
		"usd", resp.Usage.USD)

	return graph.Result{
		Next:   graph.NodeTest,
		Status: journal.StatusOK,
		Patch: state.Patch{
			Code: &changed,
			Fix:  &state.Fix{Attempts: attempt, GiveUp: false},
		},
		// Returned, never dropped: the fix loop is the most expensive part
		// of a run, and usage the budget guard never sees is spend it
		// cannot stop.
		Usage: resp.Usage,
		Note:  fmt.Sprintf("fix attempt %d of %d: %s", attempt, giveUp, causeNote(c, rc.State)),
	}, nil
}

// unionFiles returns the files recorded and the files reported, sorted and
// each listed once; see the package documentation for why.
//
// It builds a new slice rather than appending to recorded, which is the
// RunContext's snapshot of the blackboard: sorting that in place would reorder
// the dispatcher's own copy. Cloning also keeps a nil list nil and an empty
// one empty when nothing is reported, so a reply that reports nothing never
// turns the write node's [] into null in state.json.
func unionFiles(recorded, reported []string) []string {
	out := slices.Clone(recorded)
	out = append(out, reported...)
	slices.Sort(out)
	return slices.Compact(out)
}

// writeResponse archives the agent's reply next to the prompt that caused
// it. The response is marshalled rather than stored raw so that a reader
// gets the session ID and usage too, not only the text.
//
// AgentResponse.Raw is opaque backend output, so it can be malformed JSON
// and make the whole value unmarshalable. That must not cost the run an
// attempt it already paid for and already applied to the working tree, so
// a failed encode drops Raw, says so in the file, and continues.
func writeResponse(rc *graph.RunContext, resp belay.AgentResponse) error {
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		resp.Raw = json.RawMessage(`{"belay":"backend raw output was not valid JSON and was dropped"}`)
		data, err = json.MarshalIndent(resp, "", "  ")
		if err != nil {
			return fmt.Errorf("fix: encode %s: %w", responseFile, err)
		}
	}
	if err := rc.WriteNodeFile(responseFile, append(data, '\n')); err != nil {
		return fmt.Errorf("fix: write %s: %w", responseFile, err)
	}
	return nil
}

// exhaustedNote explains a give-up in one redacted line: the stable
// give_up_exhausted token, the attempt count, and what was still failing.
//
// It carries counts only — never a failure message or an issue description
// — because a note is journalled as written, and captured tool output is
// exactly where a leaked secret would be.
func exhaustedNote(attempts, giveUp int, c cause, st state.State) string {
	if giveUp < 1 {
		return fmt.Sprintf("%s: graph.give_up is %d, so no fix attempt is permitted", giveUpNote, giveUp)
	}
	note := fmt.Sprintf("%s: %d of %d fix attempts used", giveUpNote, attempts, giveUp)
	if c != causeNone {
		note += "; " + causeNote(c, st)
	}
	return note
}

// causeNote summarizes what is failing, in counts only, for a Result.Note.
func causeNote(c cause, st state.State) string {
	switch c {
	case causeTests:
		if st.Test.Total == 0 && st.Test.Failed == 0 {
			return "the test suite discovered no tests"
		}
		return fmt.Sprintf("%d of %d tests failing", st.Test.Failed, st.Test.Total)
	case causeReview:
		return fmt.Sprintf("quality gate failed with %d issues", issueCount(st.Review))
	case causeNone:
		return "nothing recorded as failing"
	default:
		return c.String()
	}
}

// goal returns the run's objective, preferring the RunContext's copy and
// falling back to the blackboard's.
func goal(rc *graph.RunContext) string {
	if rc.Goal != "" {
		return rc.Goal
	}
	return rc.State.Goal
}

// workDir returns the repository root the agent must run in.
//
// RunContext carries no workspace path of its own — Layout is a run's only
// map — and a run directory is always <workspace>/.belay/runs/<run-id> (see
// state.NewLayout), so the workspace is three levels above it. The
// derivation is asserted against state.NewLayout in this package's tests,
// so a change to the layout fails here loudly instead of pointing an agent
// at the wrong directory.
func workDir(rc *graph.RunContext) (string, error) {
	if rc.Workspace == "" {
		return "", ErrNoWorkspace
	}
	return rc.Workspace, nil
}

// logger returns rc's logger, or a discarding one. A node that panics
// because a caller left Logger unset would fail for a reason that has
// nothing to do with the run.
func logger(rc *graph.RunContext) *slog.Logger {
	if rc.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return rc.Logger
}

// editTools is the tool allow-list this node sends.
//
// It must be set explicitly. An empty AllowedTools means "the backend's
// default", and a headless agent's default is to ask permission before
// editing — permission nobody can grant, because there is no one at the other
// end of the session. The agent then describes the repair instead of making
// it, and the loop runs its whole give_up budget without changing a line.
//
// That is not hypothetical: it is what the first live run did. Three fix
// attempts, a dollar spent, and the same three lint findings at the end.
//
// Bash is deliberately withheld. The test node runs the suite and the write
// node records the change; a repair does not need a shell, and a shell is
// where an agent reaches the network and the commit history. The spellings
// are Claude Code's, the only backend v0.1 ships (ADR 0005).
func editTools() []string { return []string{"Read", "Grep", "Glob", "Edit", "Write"} }
