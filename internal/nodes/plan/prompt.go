package plan

import "fmt"

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

// userPrompt renders the plan request for goal against workDir.
func userPrompt(goal, workDir string) string {
	return fmt.Sprintf(planTemplate, goal, workDir)
}
