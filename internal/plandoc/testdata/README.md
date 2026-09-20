# internal/plandoc testdata

Every file here is a **plan**, hand-authored to the shape
`internal/nodes/plan/prompt.go` asks the agent for: the sections Summary,
Context, Steps, Tests, Risks and Out of scope, with the steps as an ordered
list under `## Steps`. None of them is a golden file — nothing regenerates
them, and they are meant to be edited by hand when a new shape needs
covering, the way a person edits a plan.

Five of them exist for their bytes rather than their words, so open them
with care and do not let an editor "fix" them: `crlf.md` ends every line with
CR LF, `tabs.md` has a tab after each marker, `nofinal.md` and `tailless.md`
have no final newline, and `latin1.md` is Latin-1 and therefore not valid
UTF-8 at all.

## Plans the parser accepts

| File | What it covers |
| --- | --- |
| `harness.md` | Not hand-authored: byte for byte the plan `loopPlanResponse` in `test/e2e/loop_agent.go` returns, archived the way `plan.Node.Run` archives one. Its `## Steps` heading is followed by the list with no blank line between. Keep it identical to that function. |
| `canonical.md` | The ordinary plan: all six sections, a tight three-item list. |
| `nested.md` | A sub-list and a fenced Go block inside steps, with a paragraph after the fence — the step must not end at its first indented line. |
| `paren.md` | `1)` markers instead of `1.`. |
| `lazy.md` | `1. 1. 1.`, which CommonMark renders as 1, 2, 3. `Step.Number` reports what is written. |
| `ten.md` | Eleven steps, so both content columns (3 for one digit, 4 for two) appear, each with a continuation line indented to its own. |
| `crlf.md` | `canonical.md` with CR LF line endings. |
| `loose.md` | A loose list: a blank line between every item. |
| `one.md` | A plan with a single step, which `Delete` must refuse to empty. |
| `nofinal.md` | `one.md` with no final newline. |
| `tailless.md` | A plan whose step list is the last thing in it, with no newline after the last step, so there is no tail at all. An edit to that step must not add the newline the author never wrote. |
| `tabs.md` | A tab after each marker, and a continuation line indented four spaces. The tab starts at column 2 and stops at column 4, so the content column is 4 and that line belongs to step 1. |
| `unicode.md` | CJK, an em dash, accents, emoji and a ZWJ family sequence. |
| `fenced.md` | A `## Steps` heading and a numbered list inside a fence **before** the real heading, and another fence inside a step. Neither is a step. |

## Plans the parser refuses

| File | Refusal |
| --- | --- |
| `headings.md` | `ErrNoSteps`: `### Step 1` sub-headings instead of a list. |
| `bullets.md` | `ErrNoSteps`: bullets instead of a numbered list. |
| `nosection.md` | `ErrNoSteps`: no `## Steps` heading anywhere. |
| `empty.md` | `ErrNoSteps`: an empty file. |
| `latin1.md` | `ErrNotUTF8`: one `0xe9` byte where `é` belongs. |

A refusal is not a verdict on the plan. Each of these is a document belay
will still show, still let a person approve, and still hand to the code node;
it is only the step-editing operations that have nothing to work with.
