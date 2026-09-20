package plan

import (
	"fmt"
	"strings"

	"github.com/dhaam-ai/belay/internal/state"
)

// SystemPrompt establishes the planning role for the agent invocation.
//
// It does two jobs. First, it forbids editing: the plan node runs before
// any approval gate, so an agent that "helpfully" started implementing
// would put unreviewed changes in the repository the human is still
// deciding about. readOnlyTools enforces the same rule at the tool layer,
// because a prompt is advice and an allowlist is not.
//
// Second, it fixes the output contract. Whatever the agent returns is
// written verbatim to plan.md and read by both a human reviewer and the
// code node, so a conversational preamble or a fence around the document
// is not cosmetic damage — it is corruption of the artifact the rest of
// the graph consumes.
//
// It is exported so a run's evidence can be compared against the prompt
// the running binary actually holds.
const SystemPrompt = `You are a senior software engineer writing an implementation PLAN for a change another engineer will carry out.

You are in planning mode. Do not create, edit, delete, or move any file, and do not run any command that changes the repository or its dependencies. Reading and searching the code is expected; writing anything is not.

Your entire reply is the plan document itself, as GitHub-Flavored Markdown. Do not introduce it, do not summarize it afterwards, and do not wrap it in a code fence. The text you return is stored verbatim as the run's plan.md and is read both by a human reviewer deciding whether to approve it and by the coding agent that implements it.

Plan the work that was asked for and no more. A plan that quietly widens the goal is a plan a reviewer has to reject wholesale.`

// planTemplate is the user prompt. It carries the goal, points the agent
// at the repository it is planning against, and fixes the document's
// sections so that the approve node's reader, and the code node after it,
// meet the same shape on every run.
//
// The final instruction matters more than it looks: this invocation is
// headless, so an agent that stops to ask a clarifying question produces
// no plan at all. Stating an interpretation and continuing gives the
// human at the approval gate something concrete to correct.
const planTemplate = `# Goal

%s

# Repository

You are planning a change to the repository at %s. Read enough of it first to plan against what is actually there: match the languages, frameworks, directory layout, naming, error handling, and test conventions already in use rather than introducing your own.

# Deliverable

Write the plan with exactly these sections, in this order:

## Summary
Two or three sentences: what will change, and why that satisfies the goal.

## Context
What already exists that this change builds on or modifies. Name real files, packages, and symbols by path. If the goal touches code that does not exist yet, say so explicitly.

## Steps
An ordered list of implementation steps. Each step names the files it creates or modifies, describes the change in one or two sentences, and is small enough to verify on its own before the next one starts. Prefer several thin end-to-end steps over one large step per layer.

## Tests
How the change will be proven correct: which test files, which cases, and what each case pins down. Include the failure cases and edge cases, not only the happy path.

## Risks
What could go wrong — an interface this change breaks, a migration that is hard to reverse, a behavior that is expensive to test — and, for each, how the steps above reduce it.

## Out of scope
What this change deliberately does not do, so the reviewer can tell an omission from a decision.

# Rules

- Reference paths that exist in the repository; do not invent files you have not looked for.
- Do not write the implementation. Describe it precisely enough that someone else can.
- If the goal is ambiguous, choose the most reasonable interpretation, state it in one sentence at the top of Summary, and plan on that basis. Do not stop to ask a question: nobody is at the other end of this session.`

// revisionTemplate is appended to the user prompt when the run is being
// asked for a new plan rather than a first one.
//
// It is appended rather than woven in, and only when there is a request,
// so that a first plan's prompt is byte for byte what it has always been:
// every cassette recorded against it still replays, and the evidence of an
// old run still compares with the prompt this binary would send.
//
// The person's words are quoted as given and never interpreted. What they
// wrote is the whole reason this request exists, and a prompt that
// summarized it would be belay overruling the one instruction it was
// handed.
const revisionTemplate = `

# Revision

A plan for this goal already exists, and the person who read it asked for a different one. Write the plan again from the start: what you return replaces the previous plan in full, in exactly the sections listed above.

%s`

// revisionGuidance is the paragraph carrying what the person said, or the
// stand-in for a request they made without saying anything. Asking again
// with nothing to add is a legitimate request — they want another try, not
// a repeat of the same answer — and the agent still has to be told that
// much, or it will write the plan it just wrote.
func revisionGuidance(guidance string) string {
	if strings.TrimSpace(guidance) == "" {
		return "They did not say what was wrong with it. Take a different approach to the same goal rather than restating the previous plan."
	}
	return "This is what they said should be different, in their own words. Follow it:\n\n" + guidance
}

// userPrompt renders the plan request for goal against workDir, plus the
// revision section when r is a request for a new plan.
//
// It reads r.Request rather than r.Pending(). The node's Patch marks the
// request served, and the dispatcher may apply that Patch and then die
// before journalling the finish, which makes the node run again over a
// blackboard where the request already reads as served. Keying on Pending
// would send that re-run an ordinary plan prompt, silently dropping the
// one thing the person said was wrong. Keying on Request asks the same
// question both times.
func userPrompt(goal, workDir string, r state.Regeneration) string {
	prompt := fmt.Sprintf(planTemplate, goal, workDir)
	if r.Request == 0 {
		return prompt
	}
	return prompt + fmt.Sprintf(revisionTemplate, revisionGuidance(r.Guidance))
}
