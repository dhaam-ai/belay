package code

import (
	"fmt"
	"strings"

	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/pkg/belay"
)

// Evidence files this node writes into its own execution directory.
const (
	// promptFile holds the exact Prompt sent to the backend.
	promptFile = "prompt.txt"
	// responseFile holds the belay.AgentResponse received, as JSON.
	responseFile = "response.json"
)

// fence is the Markdown fence run used by the prompt templates below and by
// both reply parsers. It lives in a constant because the templates are raw
// strings, which cannot contain a backtick.
const fence = "```"

// changedFilesTag is the info string on the fenced block the prompt asks for
// and that parseChangedFiles reads back.
//
// It is deliberately not a language name. No syntax highlighter claims
// "changed-files", so a block carrying it cannot be something a model
// pretty-printed for its own reasons: its presence is evidence the agent
// answered this specific instruction, which is what lets the parser prefer it
// over anything else in the reply.
const changedFilesTag = "changed-files"

// changedFilesExample is the block shape the prompt demonstrates. It is built
// from the same constants the parser matches on, so the instruction and the
// implementation cannot drift apart.
const changedFilesExample = fence + changedFilesTag + "\n" +
	"internal/calc/calc.go\n" +
	"internal/calc/calc_test.go\n" +
	fence

// systemPrompt states what this node is for: the agent makes the change
// itself, in the workspace, and reports what it touched.
//
// It is belt and braces with the AllowedTools set — a backend that ignores
// one still sees the other — and it keeps the four constraints that were
// always right. Re-planning and clarifying questions both stall a run nobody
// is watching. Running tests duplicates the test node, which is the only
// place a structured belay.TestReport enters the graph. Committing removes
// the property that makes a run abandonable: that nothing it did reached
// history.
const systemPrompt = `You are the implementation step of belay, an autonomous coding orchestrator.

You implement a plan that has already been written and approved. Do not re-plan
it, do not ask clarifying questions, do not run tests, and do not commit
anything.

Make the change yourself. Create, modify and delete files in the repository
directly, with your editing tools. Nothing downstream applies a patch on your
behalf: if you do not edit the files, the repository is left exactly as you
found it and the run makes no progress.

A separate step then inspects the workspace and records what changed. It works
from the list of files you report, so that list must name every file you
touched, and nothing you did not.`

// promptTemplate is the first-invocation prompt. Its verbs are, in order:
// goal, plan path, plan text, workspace directory, the fenced-block tag
// (quoted), and the example block.
const promptTemplate = `# Goal

%s

# Approved plan (%s)

%s

# Your task

Implement the plan above in the repository at %s.

Read whatever files you need to get the change right, then make it: edit the
files the plan changes, create the ones it adds, delete the ones it retires.
You are editing this repository in place.

# Required output

Reply with a short summary, in prose, of what you changed and why. Then exactly
one fenced block tagged %q, holding the repository-relative
path of every file you created, modified or deleted — one path per line, and
nothing else:

%s

Paths are relative to the repository root named above. No absolute paths, no
".." segments, no "a/" or "b/" prefixes, no globs, no bullets, no quotes, no
comments.

That list is load-bearing. The next step verifies every path in it against the
workspace, and refuses the whole change — failing the run — if any path escapes
the repository or names something that is not a regular file. A file you list
but did not touch is recorded as changed anyway; a file you changed but did not
list is not recorded at all, and nothing later in the run will know it exists.
List exactly what you touched.

If the plan requires no change at all, say so and emit no fenced block.`

// resumeTemplate is appended when the run already has a session to continue.
// Its verbs are, in order: the changed-file list recorded so far, and the
// fenced-block tag (quoted).
//
// It exists to correct the one thing a resumed session reliably gets wrong.
// The agent is being asked for the same kind of output a second time, and the
// natural reading of a second request is "report what you did this time" — but
// state.Code.ChangedFiles is an absolute set that replaces wholesale, not an
// increment, and the write node verifies exactly what it is handed. A reply
// naming only the newly touched files would quietly drop every file the
// earlier pass changed out of the record, while those edits sat on disk
// unaccounted for.
const resumeTemplate = `

# Continuing your earlier work

This is a continuation of the same session. The edits you already made are
still on disk — nothing undid them, and a later step has already verified and
recorded them. As of that record, the change touched:

%s

Do not redo that work and do not revert it. Read those files as they now stand
and build on them.

Your %q block must list the cumulative set: every file the
whole change touches, including the ones above that you are not editing again
this time — not only what you change in this pass.`

// editTools is the tool allow-list this node sends.
//
// Read, Grep and Glob let the agent understand the repository before it
// changes it. Edit and Write are what let it change the repository at all,
// and they are the point: this node's contract is that the workspace is
// different after it runs, while the write node only verifies and records
// what is already on disk. Withholding the edit tools, as an earlier revision
// of this node did, leaves nothing anywhere in the graph able to modify a
// file, and a run then loops test -> fix -> test against an untouched tree
// until the give-up budget is gone.
//
// Bash is withheld, and that exclusion is the deliberate one — do not add it
// back as a convenience. The test node owns running the suite and is the only
// place a structured belay.TestReport enters the graph; an agent that runs
// tests itself spends budget on a verdict nothing reads, and tends to keep
// working until they pass rather than returning to the graph that is supposed
// to decide what happens next. Committing is worse: a run is abandonable
// precisely because nothing it did reached history, and one commit from
// inside a best-of-N candidate takes that away. A shell is also the step
// where an agent reaches the network. Everything this node legitimately
// needs — reading, searching, editing — it has without one.
//
// The spellings are Claude Code's, which per ADR-0005 is the only backend
// v0.1 ships. A future backend with different tool names needs its own
// mapping; the residual risk of that is a node granted less than it needs,
// which fails loudly, rather than one granted more than it should have.
func editTools() []string { return []string{"Read", "Grep", "Glob", "Edit", "Write"} }

// buildRequest assembles the single AgentRequest this node issues.
//
// SessionID comes straight from the blackboard: passing the previous
// invocation's session back is what turns a fix loop into one continuing
// conversation instead of a cold restart per attempt. An empty
// State.Code.SessionID means "no session yet", and an empty
// AgentRequest.SessionID is exactly how the contract spells "start fresh",
// so the first call needs no special case.
func buildRequest(rc *graph.RunContext, workDir, plan string) belay.AgentRequest {
	return belay.AgentRequest{
		Prompt:       buildPrompt(rc, workDir, plan),
		SystemPrompt: systemPrompt,
		WorkDir:      workDir,
		AllowedTools: editTools(),
		MaxTurns:     rc.Config.Agent.MaxTurns,
		Model:        rc.Config.Agent.Model,
		SessionID:    rc.State.Code.SessionID,
	}
}

// buildPrompt renders the task prompt, extended with a continuation section
// when the run is resuming a session.
//
// The continuation gate is the session alone. It used to also require an
// archived diff, which made sense while the node's output was a patch, but
// under in-place editing the earlier work is on disk whether or not any
// artifact recorded it — and a resumed session that is not told so is liable
// to start the whole change over.
func buildPrompt(rc *graph.RunContext, workDir, plan string) string {
	var b strings.Builder
	fmt.Fprintf(&b, promptTemplate,
		strings.TrimSpace(rc.Goal),
		rc.State.Plan.Path,
		strings.TrimSpace(plan),
		workDir,
		changedFilesTag,
		changedFilesExample,
	)
	if rc.State.Code.SessionID != "" {
		fmt.Fprintf(&b, resumeTemplate, fileList(rc.State.Code.ChangedFiles), changedFilesTag)
	}
	return b.String()
}

// fileList renders paths as a Markdown bullet list, or a placeholder when
// there are none, so the resume section never degenerates into a dangling
// heading with nothing under it.
func fileList(paths []string) string {
	if len(paths) == 0 {
		return "- (no files were recorded for the earlier pass)"
	}
	var b strings.Builder
	for i, p := range paths {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		b.WriteString(p)
	}
	return b.String()
}
