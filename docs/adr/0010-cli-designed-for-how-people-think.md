# 10. The CLI is designed against a cognitive model, not a feature list

Status: Accepted

## Context

belay asks a stranger to point an autonomous agent at their repository, let it
edit files, and spend their money — while they are not watching. Every command
is therefore a trust negotiation before it is a feature.

Most CLI tools are designed by accretion: a flag per capability, output shaped
by whatever was easiest to print. That produces tools experts tolerate and
everyone else abandons. Since the project's stated goal is that a working
developer can adopt belay into a daily loop, "an expert can figure it out" is
not a sufficient bar.

We needed a shared vocabulary for arguing about interface decisions, so that a
reviewer can say *why* an output is wrong rather than only that they dislike
it, and so that nine people contributing commands produce one coherent tool.

## Decision

The CLI is designed against the six cognitive systems in John Whalen's
*Designing for How People Think* (O'Reilly, 2019). Every user-facing command
must have an answer for each system, and a reviewer may reject output that
does not.

**1. Vision and attention.** One idea per line. Emphasis is reserved for the
two things people actually scan for: what is happening now, and what it has
cost. `NO_COLOR` and non-TTY output are honoured everywhere, so piping to a
file yields clean text.

**2. Wayfinding.** A person must always be able to answer: where am I in the
graph, what happens next, how do I stop, and how do I get back. Commands that
act on an implicit target — `resume` with no run id, `timeline` with no run id
— must say which target they chose.

**3. Memory.** Never require recall. Run ids are machine-shaped and nobody
remembers them, so runs are recognised by their goal text and relative age
("14 minutes ago"), and every pause prints a copy-pasteable resume command.

**4. Language.** The user's words, not the type system's. The argument to
`belay run` is what they want built. `StatusAborted` is "stopped"; a run
waiting at the approval gate says it is waiting for a plan review, not
`approve`.

**5. Decision-making.** Cost is shown before it is spent, not after. `--dry-run`
resolves everything and creates nothing. Estimated figures are marked as
estimates, because the budget ledger tracks that distinction precisely and
presenting a guess as a fact would waste it.

**6. Emotion.** The fear is a runaway agent wrecking a repository or burning
money. Ceilings are visible up front, Ctrl-C is safe and says so, and expected
states are described as expected — a crashed run's torn journal tail is
reported as "the run was interrupted here", because that is what durability
looks like from the inside, not as corruption.

## Consequences

- Commands cost more to write. Every one needs an empty state, a damaged
  state, a `--json` form, and prose a non-expert can read.
- Output strings become part of the interface. Several are pinned by golden
  tests, so changing them is deliberate rather than incidental.
- Reviews get a shared vocabulary: "this fails wayfinding — it acts on the most
  recent run without saying which" is a concrete, arguable objection.
- The framework is a lens, not a rulebook. It will occasionally recommend
  something that a terminal makes awkward; when that happens the deviation
  should be recorded rather than the framework quietly dropped.

## Alternatives considered

- **No explicit model.** Cheapest, and what most tools do. Rejected because
  multiple contributors writing commands independently is exactly the condition
  under which an interface fragments — and this project's commands were in fact
  written in parallel by different authors.
- **Copy an existing CLI's conventions** (git, docker, kubectl). Useful for
  surface grammar and partly adopted, but those tools are designed for daily
  experts who have already paid the learning cost. belay's adoption problem is
  the first ten minutes.
- **Nielsen's usability heuristics.** Sound and better known, but pitched at
  evaluating an existing interface rather than deriving one, and weaker on the
  memory and emotion dimensions that dominate here.

## Revisit if

- User reports show people failing at a step none of the six systems predicted,
  which would mean the model is missing a dimension for terminal work.
- The `--json` surface becomes the primary interface for most users, at which
  point the human-facing rendering matters less than the schema's stability.
