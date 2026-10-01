//go:build fixrate

package fixrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// This file is the control arm: the same backend, on the same task, with
// belay's graph taken away.
//
// # Why belay needs one at all
//
// Every number this harness produced before this file measured belay
// against nothing. "belay repairs 18 of 25 seeded defects" is unfalsifiable
// as a claim about belay, because the reader cannot tell which part of the
// 18 belongs to belay's dispatcher, fix loop and review gate, and which
// part belongs to the coding agent underneath, which would have repaired
// them if it had simply been pointed at the repository. The only honest
// published figure is a DELTA: the same backend, the same corpus, the same
// budget, with and without belay's orchestration.
//
// So the control arm invokes belay.AgentBackend exactly once per task. No
// plan node, no approve gate, no write verification, no test node, no fix
// loop, no review gate -- nothing between the goal and the backend. What
// it keeps identical to the belay arm is everything that is NOT
// orchestration:
//
//   - the goal text, verbatim (the caller passes the same string to both
//     arms, which is why runControl takes a goal rather than composing one);
//   - the model, read from the same config.Agent.ModelFor(graph.NodeCode)
//     the code node reads, so the two arms cannot drift onto different
//     models even if the configuration changes;
//   - the tool allow-list belay's code node sends (controlTools);
//   - a turn and wall-clock allowance at least as large as anything belay's
//     graph could have used (controlAllowance).
//
// # The asymmetries, stated rather than hidden
//
// The two arms cannot be byte-identical, and pretending otherwise would be
// the dishonest part. These differences are deliberate, and every one of
// them is a thing belay ADDS -- which is the quantity under measurement:
//
//  1. The prompt is not the code node's prompt. It cannot be: that prompt
//     cites a plan document, and in the control arm no plan node ever ran
//     to write one. The control arm gets the goal plus the smallest framing
//     that makes a single call well-formed at all (controlPromptTemplate) --
//     "the repository is here, edit it in place, nothing runs after you".
//  2. No SystemPrompt. belay's code node sends one; that is part of what
//     belay contributes, so the control arm gets the backend's own default
//     and the delta is credited to belay rather than quietly handed to it.
//  3. No session continuation, because there is no second call to continue.
//  4. Whatever the backend does with the tools is unsupervised. The control
//     arm never re-runs tests, never re-reads the workspace, and never asks
//     for a second attempt. A task it fails is simply failed.
//
// A report built on this file must repeat these in the reader's own view;
// see PairedResult.Provenance and README.md.
//
// # What it is NOT
//
// It is not a second backend, and it is not the design's "bare-native"
// arm: the owner dropped the second backend (2026-09-23), so both arms
// drive Claude Code. That is the comparison worth publishing anyway --
// orchestration, holding the backend fixed.

// ErrNoGoal reports a control-arm call with no goal text. The goal is the
// one thing both arms must share verbatim, so an empty one is a wiring
// bug that would silently make the comparison meaningless rather than
// merely wrong.
var ErrNoGoal = errors.New("fixrate: a goal is required")

// ErrNoWorkspace reports a control-arm call with no workspace. There is no
// default: a relative or empty path would point a backend with write access
// at whatever directory the harness happens to be running in, which is
// belay's own tree (the same reasoning state.NewLayout rejects a relative
// workspace for).
var ErrNoWorkspace = errors.New("fixrate: a workspace is required")

// Arm names one measurement arm of a paired comparison. It is recorded on
// every per-arm number a report carries, so no figure can be read without
// knowing which arm produced it.
type Arm string

const (
	// ArmBelay drives belay's real, unmodified dispatcher graph -- what
	// Run does.
	ArmBelay Arm = "belay"

	// ArmControl drives the same backend with one raw
	// belay.AgentBackend.Invoke per task and no graph at all -- what
	// runControl does. "Control arm" is CONTEXT.md's term for it, and the
	// only one this package uses.
	ArmControl Arm = "control"
)

// String returns a's canonical spelling.
func (a Arm) String() string { return string(a) }

// provenance returns the mandatory, human-readable disclosure of what an
// arm's numbers do and do not measure. It sits beside Mode.provenance in
// every report, because an arm label and a mode label answer different
// questions and a number needs both: the mode says whether a real agent
// was involved at all, the arm says whether belay's graph was.
func (a Arm) provenance() string {
	switch a {
	case ArmBelay:
		return "arm: belay — belay's real, unmodified dispatcher graph " +
			"(plan -> approve -> code -> write -> test -> fix -> review), with its fix loop and quality gate."
	case ArmControl:
		return "arm: control — the SAME backend invoked exactly once per task with no graph: no plan, " +
			"no fix loop, no quality gate, no second attempt. Same goal text, same model, same tools as " +
			"belay's code node, and a turn/time allowance at least as large as belay's whole graph could " +
			"have used. It is not a different backend and not a different model; the only variable is " +
			"belay's orchestration."
	default:
		return "arm: " + string(a) + " — unrecognized; this arm's numbers cannot be vouched for"
	}
}

// Allowance is the ceiling one control-arm call is given: how many agent
// turns it may take and how long it may run.
//
// It exists as a named, reported value rather than two arguments because a
// reader of a losing control arm will ask, first, whether it was starved.
// The answer has to be in the report, with the derivation legible.
type Allowance struct {
	// MaxTurns is the AgentRequest.MaxTurns the single call is given.
	MaxTurns int `json:"max_turns"`
	// Timeout bounds the single call's wall clock. Zero means unbounded,
	// which is how belay itself spells an absent graph.node_timeout.
	Timeout time.Duration `json:"timeout"`
	// AgentCalls is the number of agent-invoking node executions belay's
	// graph could have made under the same configuration -- the multiplier
	// the other two fields were derived with.
	AgentCalls int `json:"agent_calls"`
}

// MarshalJSON writes Timeout as a Go duration string ("1h15m0s") rather
// than the nanosecond integer time.Duration marshals to by default. This
// is a figure a human reads out of a report to check the control arm was
// not starved, and 4500000000000 does not answer that question.
func (a Allowance) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		MaxTurns   int    `json:"max_turns"`
		Timeout    string `json:"timeout"`
		AgentCalls int    `json:"agent_calls"`
	}{a.MaxTurns, a.Timeout.String(), a.AgentCalls})
}

// controlAllowance derives the control arm's allowance from cfg: the
// largest turn and wall-clock budget belay's graph could have spent on
// agent invocations under the same configuration.
//
// # The derivation
//
// belay's graph invokes the agent at exactly three nodes: plan (once),
// code (once), and fix (up to config.Graph.GiveUp times -- internal/nodes/
// fix returns StatusFailed without invoking anything once
// State.Fix.Attempts reaches GiveUp, so GiveUp is the count of
// agent-invoking fix executions, not one more than it). Every one of those
// calls is capped at config.Agent.MaxTurns turns and, by the dispatcher,
// at config.Graph.NodeTimeout wall clock. So the most the graph could ever
// hand a backend is:
//
//	AgentCalls = 2 + Graph.GiveUp
//	MaxTurns   = Agent.MaxTurns    * AgentCalls
//	Timeout    = Graph.NodeTimeout * AgentCalls
//
// With belay's shipped defaults (max_turns 30, give_up 3, node_timeout
// 15m) that is 5 calls, 150 turns and 75 minutes for the control arm's
// single invocation.
//
// # Two deliberate choices
//
// The plan node's allowance is INCLUDED even though the control arm has no
// planning step. The control arm's one call has to do the planning, the
// coding and any self-correction inside itself, so charging it for the
// planning turns it cannot skip would starve it. Where a choice tilts the
// comparison, this function tilts it toward the control arm: a control arm
// that loses must not be able to lose because it ran out of room.
//
// config.Graph.MaxSteps is deliberately NOT part of the derivation. It
// caps total node EXECUTIONS, most of which (approve, write, test, policy,
// review) invoke no agent, so multiplying by it would invent an allowance
// belay's graph could not actually reach through a backend.
//
// A zero NodeTimeout stays zero rather than becoming zero-times-N: belay
// spells "no per-node ceiling" as zero (the dispatcher's `timeout > 0`
// guard), and the control arm inherits the absence of a ceiling rather
// than a ceiling of nothing.
func controlAllowance(cfg config.Config) Allowance {
	calls := 2 + cfg.Graph.GiveUp
	if calls < 1 {
		// A configuration validator rejects give_up < 1, so this is
		// unreachable through a loaded config; it is here so a
		// hand-built Config in a test cannot produce a negative
		// allowance that would read as "unbounded".
		calls = 1
	}
	return Allowance{
		MaxTurns:   cfg.Agent.MaxTurns * calls,
		Timeout:    cfg.Graph.NodeTimeout.Duration() * time.Duration(calls),
		AgentCalls: calls,
	}
}

// controlTools is the tool allow-list the control arm sends: the same one
// belay's code node sends.
//
// internal/nodes/code's editTools is unexported, so these spellings are
// replicated rather than imported -- the same accommodation agent.go makes
// for the changed-files protocol. Replication alone would let the code node
// gain or lose a tool without anything noticing, so the equality is a
// guarded property, not a comment: TestControlTools_MatchesTheCodeNode
// drives the real code node and compares this list against the
// AllowedTools it actually sent.
//
// Bash is absent here because it is absent there, and for the reason given
// at internal/nodes/code's editTools: a shell is where an agent reaches the
// network and where it can commit. Granting the control arm a shell belay's
// own code node withholds would make the control arm the more capable of
// the two, and the delta would no longer be about orchestration.
func controlTools() []string { return []string{"Read", "Grep", "Glob", "Edit", "Write"} }

// controlPromptTemplate is the whole of the framing the control arm adds
// around the goal.
//
// It is as small as a well-formed single call allows, and every clause in
// it earns its place by removing an excuse rather than by adding guidance:
// the backend is told where the repository is (it cannot act otherwise),
// that it should edit in place (belay's code node's contract too -- see
// that package's "the agent edits the workspace directly"), and that
// nothing runs afterwards (true, and without it a backend may reasonably
// stop at a description of what it would do, which would measure the
// framing rather than the backend).
//
// It carries no instruction belay's graph would have supplied through a
// plan, a test report or a review finding. Those are belay's contribution
// and the control arm does not get them. In particular it does not ask for
// a regression test: where a corpus's goal template asks for one -- as the
// SWE-bench template does -- that ask lives in the GOAL, which both arms
// receive verbatim, so both arms are asked identically.
const controlPromptTemplate = `%s

The repository is at %s. Make the change there yourself: edit the files in
place with the tools you have. Nothing runs after you and nothing will ask
you for a second attempt, so leave the repository in the state you want it
judged in.`

// controlPrompt renders the single prompt the control arm sends.
func controlPrompt(goal, workspace string) string {
	return fmt.Sprintf(controlPromptTemplate, strings.TrimSpace(goal), workspace)
}

// controlRunResult is what runControl hands back: what the single call
// spent, what it said, and the allowance it was given.
type controlRunResult struct {
	// Usage is the call's reported spend. It is populated even when the
	// call failed; see runControl.
	Usage belay.Usage
	// Turns is the number of turns the backend reported taking, or 0 if
	// it does not report them -- not necessarily "no turns happened"
	// (belay.AgentResponse.Turns).
	Turns int
	// Text is the backend's final reply.
	Text string
	// Model is the model the call was pinned to, recorded so a report can
	// show both arms were pinned to the same one rather than assert it.
	Model string
	// Allowance is the ceiling the call was given.
	Allowance Allowance
	// RunErr is the backend's own error message, if any, or empty.
	//
	// Like belayRunResult.RunErr, it is deliberately NOT runControl's
	// error return: a backend that gave up, hit its own ceiling or
	// produced nothing usable is a legitimate outcome of the control arm
	// -- the task is simply not resolved -- and an outcome this harness
	// must classify rather than abort on. runControl's error return is
	// reserved for this harness's own wiring being wrong.
	RunErr string
}

// runControl invokes agent exactly once against workspace with goal, and
// returns what the call spent and said.
//
// It is the control arm in full: there is no loop in this function, no
// second call, and nothing that reads the workspace back. See this file's
// header comment for what is held identical to the belay arm and what is
// deliberately not.
//
// # Spend is counted on a failed call
//
// belay.AgentBackend.Invoke documents that a backend which produced
// partial output before hitting a problem it can identify -- its own
// budget guard, say -- should return a populated AgentResponse together
// with an error. Whatever it reports having spent, it has spent, so
// runControl reads resp.Usage on the error path too, exactly as
// internal/nodes/code does ("Usage travels with the error"). A control arm
// whose failures were free would flatter its own cost per resolved task,
// which is the one number this whole lane exists to publish honestly.
//
// # The timeout is the caller's ctx, narrowed
//
// Allowance.Timeout is applied with context.WithTimeout, so a backend that
// honours ctx (Invoke's contract requires it) stops there. A zero Timeout
// leaves ctx untouched rather than expiring immediately.
func runControl(ctx context.Context, workspace, goal string, agent belay.AgentBackend, cfg config.Config) (controlRunResult, error) {
	if agent == nil {
		return controlRunResult{}, fmt.Errorf("fixrate: control arm: %w", ErrNoAgent)
	}
	if strings.TrimSpace(goal) == "" {
		return controlRunResult{}, fmt.Errorf("fixrate: control arm: %w", ErrNoGoal)
	}
	if workspace == "" {
		return controlRunResult{}, fmt.Errorf("fixrate: control arm: %w", ErrNoWorkspace)
	}

	allow := controlAllowance(cfg)
	model := cfg.Agent.ModelFor(graph.NodeCode)

	if allow.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, allow.Timeout)
		defer cancel()
	}

	resp, err := agent.Invoke(ctx, belay.AgentRequest{
		Prompt:       controlPrompt(goal, workspace),
		WorkDir:      workspace,
		AllowedTools: controlTools(),
		MaxTurns:     allow.MaxTurns,
		Model:        model,
		// SystemPrompt and SessionID are deliberately absent; see this
		// file's header comment on the asymmetries.
	})

	out := controlRunResult{
		Usage:     resp.Usage,
		Turns:     resp.Turns,
		Text:      resp.Text,
		Model:     model,
		Allowance: allow,
	}
	if err != nil {
		out.RunErr = err.Error()
	}
	return out, nil
}

// RunControl is Run with belay's graph taken away: it measures the control
// arm over exactly the corpus, the selection and the evaluation Run uses.
//
// It shares Run's whole pipeline (runArm) and replaces only the drive
// step. The defects are injected by the same subprocess, snapshotted the
// same way, and after the one call the workspace is re-tested, re-linted
// and put through the same cheat-aware classifier -- so the two arms are
// judged by one check, by construction, rather than by two copies of it.
// The Result is the same type, labelled ArmControl, with Control set and
// the Belay* fields empty.
//
// Every refusal Run makes, RunControl makes, because it is runArm that
// makes them: above all, ModeLive without RunOptions.Confirm set to
// ConfirmLiveSpend, and a ModeReplay agent that reports spend. Like Run,
// it never constructs an agent; a live control arm is the caller's own
// wiring and the caller's own decision to spend.
//
// The single call gets the belay arm's goal (belayGoal) and is configured
// from the belay arm's configuration (belayConfig), so the model it is
// pinned to and the allowance it is given are derived from the very
// configuration the belay arm runs under.
func RunControl(ctx context.Context, opts RunOptions) (*Result, error) {
	return runArm(ctx, opts, ArmControl, driveControl)
}

// driveControl is the control arm's drive step: one raw backend call.
func driveControl(ctx context.Context, workspace string, agent belay.AgentBackend, _ func() time.Time, _ *slog.Logger) (armOutcome, error) {
	r, err := runControl(ctx, workspace, belayGoal, agent, belayConfig())
	if err != nil {
		return armOutcome{}, err
	}
	return armOutcome{usage: r.Usage, control: &r}, nil
}
