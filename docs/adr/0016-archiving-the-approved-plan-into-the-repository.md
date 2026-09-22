# ADR-0016: A finished run moves its plan into the repository

## Status
Accepted

## Context

A run's plan is the best record there is of why its code looks the way it does: what belay was asked for, what it proposed, and what a person approved before any code was written. Until now it lived at `.belay/runs/<run-id>/artifacts/plan.md` and nowhere else — in a directory the README calls safe to delete, beside the agent's raw transcripts and a branch run's whole second checkout. The plan outlives the run it belongs to; the rest of that directory does not.

The owner asked for exactly this, in these words: "once task done remove temp file of plan and move to docs folder in current working directory".

Two things in belay make that less simple than it sounds.

**A run delivers its result in one of two ways.** Since ADR-0012 a run in a git repository works in its own worktree and commits once, in the `promote` node, on `belay/<run-id>`; a run anywhere else edits the workspace in place and commits nothing. "Beside the code" therefore means two different things: a file in the person's working tree, and a file in the commit the run delivers.

**A move can fail half way.** The plan is one file, and between removing it from one place and having it in another there is a window in which a crash loses it. Whatever we do here has to survive `kill -9` at every point, because belay's whole premise is that an interrupted run loses nothing.

There is also a smaller thing the same slice settles: `.belay` is scratch, but belay never told git so. Every repository that ignored it did so by hand.

## Decision

When a run completes successfully, belay moves the plan it followed out of the run's artifacts and into the repository, at `<plan.archive_dir>/<run-id>-<slug>.md` relative to the tree the run worked in. `plan.archive` (default `true`) and `plan.archive_dir` (default `docs/plans`) decide whether and where; both are read when the run is created and pinned in its manifest (`archived_plan.dir`), so a run files its plan where it said it would when it started, whatever `belay.yaml` says by then. `belay run` also writes `.belay/.gitignore` holding `*` on first use in a repository.

### Who writes the file

**A run that edits its workspace in place**: the CLI, after the dispatcher has returned and the run has completed (`afterCompletion`, before the pull request and before the worktree is tidied). The plan appears as a new, untracked file beside the change, for the person to commit with it or delete. belay stages nothing and commits nothing.

**A run on a branch**: the `promote` node, which writes the plan into the tree it is about to commit, just before its single commit, so the plan is part of what the branch — and the pull request `--pr` opens — delivers. The CLI still finishes the move afterwards: it records where the plan went and removes the run's copy, which is not a node's business.

It is `promote` and not a step before it because `promote` is the only place that knows whether the commit has happened. The node's rule is to ask the repository rather than remember: it writes the plan only while no commit of this promotion is in the branch's history, and never after. Before the commit, a re-run writes the same bytes to the same path and the write is adopted; after it, the re-run leaves the tree alone. That is what keeps an interrupted promote and its re-run byte-identical even when the plan could not be read on the first attempt and can on the second — without the rule, the re-run makes a second commit.

### A move, not a copy, and never a loss

Three durable steps, each taken only once the one before it is on the disk: place the plan in the repository (write a temporary file beside the destination, fsync it, hard-link it into place, fsync every directory on the way), record its path in the manifest, then remove the run's copy. At every point in between the plan exists, whole, in at least one place, and running the move again finishes from wherever the last attempt stopped: a destination already holding exactly those bytes is an earlier attempt's and is adopted rather than duplicated, and the run's copy is removed only while the archived copy is still the plan. Recording before removing is the point of the order — the other way round, a crash leaves a plan in the repository that nothing points to.

### What is refused

Never overwriting: a file already at the destination that is not the plan is left exactly as it is, and the plan stays in the run directory with a reason. Never leaving the workspace: a `..`, an absolute path or a `docs/` that is a symbolic link out of the workspace is refused, at load time where the configuration can be read and at move time where only the disk knows. Only completed runs: a failed, stopped or unfinished run's plan describes code that does not exist. Only the plan the code was written from: a plan edited after the run last read it no longer fingerprints to `state.Plan.Digest`, and is not archived.

Nothing about archival can change how a run is reported. A plan that cannot be moved is a message, never a failed run and never a different exit code, and `belay plan --archive <run-id>` finishes the move once the way is clear.

### What stays where it was

`journal.ndjson`, `manifest.json`, `state.json`, the snapshots and every other artifact — including the plans a `--regenerate` replaced — stay in the run directory, so `belay timeline` is unchanged, byte for byte, by a move. `belay plan <run-id>` reads a finished run's plan from wherever it now is: the archived copy in the workspace, the run's commit (via `git cat-file`) for a branch run whose branch has not been taken, or the run's own copy when the move never happened.

## Consequences

### Positive
- The record of why the code changed survives `rm -rf .belay`, and on a branch it travels with the commit and the pull request, reviewed alongside the change it describes.
- Plans sort chronologically in `docs/plans`, and every name begins with the run id, which `belay timeline` and `belay plan` take.
- The plan is the approved bytes and nothing else, so it still fingerprints to what the run recorded, and a person can check that what was filed is what was approved.
- `.belay/.gitignore` means run state is never offered for a commit by accident, which is what makes one deliberate file in the repository meaningful.

### Negative
- belay writes a file into the person's repository that they did not name. It is announced twice before anything is spent — in `--dry-run` and in the run's opening lines — and one setting turns it off.
- A documentation site that publishes everything under `docs/` will publish filed plans once they are committed, and a Markdown linter in someone's CI may have opinions about a plan that a branch run committed.
- Two new settings change `Config.Digest` for every configuration, so every manifest's digest differs from one written by an earlier belay.
- After a move, `state.Plan.Path` names a file that is not there any more. The manifest's pointer supersedes it; nothing reads the plan again once a run is over.
- The default cassette path is under `.belay`, so a recording meant to be committed now needs `--cassette` pointing somewhere else, or `git add -f`.

### Follow-ups
- Forks and queue runs (ADR-0017) work in copies of the checkout; they should pin no archive directory, so they never file a plan into a copy nobody keeps.
- Show the archived plan's path in `belay runs --json` and in the timeline header.

## Alternatives Considered

- **Leave it off by default.** Least surprise, and rejected: a plan nobody finds stays in a directory the README says is safe to delete, which is the problem. Opting out is one key, and belay says where the plan will go before it spends anything.
- **An archive node at the end of the graph.** Durable through the journal, and rejected: a problem filing a document would become a failed run, the route, `graph.max_steps` and every route-asserting test would change, and the fix-rate and e2e suites, which drive the dispatcher directly, would start writing into fixture repositories. Handing a finished run over to a person is the CLI's job, as "commit it" already is.
- **A step before promote, in branch mode.** Rejected: the plan would sit uncommitted in the worktree while promote asked whether the work was already committed, so a crash between the two would leave the tree changed under a promotion already made. Only promote can ask that question and act on the answer in the same breath.
- **Copy instead of move.** Simpler, and rejected: the owner asked for the temporary file to be removed, and two copies diverge the moment somebody edits one.
- **A provenance header in the filed file** (an HTML comment naming the run and the goal). Rejected: the file is then not the plan, and the plan's digest no longer describes it. The run id is in the name, the manifest points at the file, and on a branch the commit carries a `Belay-Run` trailer.
- **Filing into the person's checkout in branch mode.** Rejected outright: a branch run never writes into the checkout, which is the promise ADR-0012 exists to make.
- **The git repository root rather than the workspace.** Rejected: belay also works on trees that are not repositories, and a run started in a monorepo subdirectory belongs next to that subdirectory's code.
- **Committing the plan in an in-place run.** Rejected: in place, belay commits nothing at all, and this is not the feature that changes that.

## Revisit If
- People routinely delete filed plans, or turn `plan.archive` off, which would mean the default is wrong.
- A documentation-site build or a CI linter breaks on filed plans often enough that a different default directory, outside `docs/`, would serve people better.
- A run gains a second document worth keeping, at which point "the plan" becomes "the run's record" and the shape here should generalise.

## References
- ADR-0002: nodes return verdicts; the dispatcher owns persistence
- ADR-0010: the CLI is designed against a cognitive model — say what will happen before it happens
- ADR-0011: cross-version safety for run state, and when a field earns a capability
- ADR-0012: git worktrees for isolation, and a run's result on its own branch
- `internal/planarchive`, `internal/nodes/promote`, `internal/cli/archive.go`, `docs/config.md`
