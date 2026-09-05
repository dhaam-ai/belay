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

// diffFence is the Markdown fence the prompt asks the agent to wrap its
// diff in, and that extractDiff looks for. It lives in a constant because
// the prompt templates below are raw strings, which cannot contain a
// backtick.
const diffFence = "```"

// systemPrompt states the one rule that separates this node from the write
// node: propose, do not apply. It is belt and braces with the read-only
// AllowedTools set — a backend that ignores one still sees the other.
const systemPrompt = `You are the implementation step of belay, an autonomous coding orchestrator.

You implement a plan that has already been written and approved. Do not re-plan
it, do not ask clarifying questions, and do not run tests or commit anything.

You must not create, modify, or delete any file. A separate step applies your
work. Your only output is a patch.`

// promptTemplate is the first-invocation prompt. Its %s verbs are, in
// order: goal, plan path, plan text, workspace directory, fence, fence.
const promptTemplate = `# Goal

%s

# Approved plan (%s)

%s

# Your task

Implement the plan above for the repository at %s.

Read whatever files you need to get the change right. Do not write to any of
them: return the change as a patch instead.

# Required output

Reply with a short summary of what you changed, then exactly one fenced block
tagged "diff" holding a single unified diff against the repository root:

%sdiff
diff --git a/path/to/file.go b/path/to/file.go
--- a/path/to/file.go
+++ b/path/to/file.go
@@ -1,3 +1,4 @@
 unchanged line
+added line
%s

Use repository-relative paths with the usual a/ and b/ prefixes, and include
every file the change touches. If the plan requires no change at all, say so
and emit no fenced diff block.`

// resumeTemplate is appended when the run already has a session to
// continue. Its %s verbs are, in order: archived diff path, changed-file
// list.
const resumeTemplate = `

# Continuing your earlier work

This is a continuation of the same session. You already proposed a change,
archived at %s, touching:

%s

Build on that change rather than starting over, and make sure your reply
contains the complete diff for the whole change, not only the new increment:
the archived diff is replaced by this one, not added to it.`

// readOnlyTools is the tool allow-list the code node sends.
//
// The node's contract is that it proposes a change and never applies one,
// so it names only read tools rather than inheriting the backend's default
// set, which includes write and shell access. The spellings are Claude
// Code's, which per ADR-0005 is the only backend v0.1 ships; a future
// backend with different tool names needs its own mapping, and the
// residual risk of that is a node that reads less than it could, not one
// that silently edits the workspace.
func readOnlyTools() []string { return []string{"Read", "Grep", "Glob"} }

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
		AllowedTools: readOnlyTools(),
		MaxTurns:     rc.Config.Agent.MaxTurns,
		Model:        rc.Config.Agent.Model,
		SessionID:    rc.State.Code.SessionID,
	}
}

// buildPrompt renders the task prompt, extended with a continuation
// section when the run is resuming a session that already produced a diff.
func buildPrompt(rc *graph.RunContext, workDir, plan string) string {
	var b strings.Builder
	fmt.Fprintf(&b, promptTemplate,
		strings.TrimSpace(rc.Goal),
		rc.State.Plan.Path,
		strings.TrimSpace(plan),
		workDir,
		diffFence,
		diffFence,
	)
	if rc.State.Code.SessionID != "" && rc.State.Code.LastDiff != "" {
		fmt.Fprintf(&b, resumeTemplate, rc.State.Code.LastDiff, fileList(rc.State.Code.ChangedFiles))
	}
	return b.String()
}

// fileList renders paths as a Markdown bullet list, or a placeholder when
// there are none, so the resume section never degenerates into a dangling
// heading with nothing under it.
func fileList(paths []string) string {
	if len(paths) == 0 {
		return "- (the archived diff named no files)"
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
