# ADR-0017: Forking a run to re-run one node, and the goal queue

## Status
Accepted (part 1, forking a run). Part 2, the goal queue, is recorded below by the slice that builds it.

This record has two parts because the two features were designed together, share their primitives (the shared NDJSON package, write-once snapshots, `graph.Options.PauseAfter`), and answer the same question from two sides: how belay acts on work nobody is watching as it happens. Each part has its own context, decision and consequences.

## Part 1: Forking a run

### Context

The owner asked for this by name: re-run one node of an existing run, from the state the run was in just before that node, without paying again for everything before it. A fix loop gives up, and the person wants to give the last repair another go with a stronger model, not buy the plan and the code again. A review fails, and they want to re-run it after changing the linter settings.

Several facts about belay as it stands shape what such a command can be.

**The journal holds transitions, not state.** A `journal.Record` names a node, an attempt, an outcome and the next node. It never holds a `state.Patch`, and `Store.ApplyPatch` overwrites `state.json` in place, so the blackboard as it stood before a node cannot be rebuilt by replaying anything. Nor would replaying patches be right if they were kept: `belay resume` records an approval, and `belay plan --regenerate` a request for a new plan, by saving `state.json` directly, which no patch log would see.

**Slice 3a/3b already built what a fork needs, and decided how.** Those decisions are recorded here, because this is where they are used:

- *Visits* (`journal.Visits`). A visit is one pass of a run through a node. A gate that paused and ran again when the person resumed, and an attempt a crash or a Ctrl-C cut short and its re-run, are each one visit: a `node_finished` that names its own node as next, with status paused or aborted, hands the visit back to the same node. That is the rule commit 8f46bac first used so the timeline stopped reporting an approval as a loop, and it lives in one place so `belay timeline`'s numbers and `belay fork --visit` cannot disagree. The timeline declines to number the rows of a journal whose executions do not pair up, because such a journal has no single right grouping, and a number on it would be one a fork could act on.
- *Snapshots* (`state.Snapshot`). The dispatcher saves the blackboard it is about to hand a node as a write-once file, `snapshots/<seq>-<node>.json`, before every execution at attempt 1, keyed on that execution's `node_started` Seq, with a fingerprint of the workspace beside it. Keying every attempt-1 execution rather than only the start of each visit is deliberate: a person's approval or request for a new plan is written while the run waits at the gate, after the visit began, and a crash re-run of the gate must not lose it. A re-run at a later attempt reads the snapshot of the attempt it repeats, which is what fixed the fix node counting one repair twice.
- *No capability for snapshots.* A belay that predates them never reads them, re-runs an interrupted node from `state.json` exactly as before, and drops nothing this one depends on; the blackboard inside a snapshot is decoded as `state.json` is, migrated and capability-checked.
- *A snapshot that cannot be saved* means the node is not run: its attempt is closed as interrupted and charged nothing, rather than left open for the crash path to charge the costliest step the run has paid for.

**A recorded run must not be amended.** `journal.Open` heals a torn final line by rewriting the file, which is a write. `journal.ResolveStart` decides where a run goes next from its last records, so one record appended after `run_failed` makes a failed run resumable again. And a journal is the run's audit record: one that a later command could quietly rewrite is not an audit record.

**Spend is rebuilt from the journal.** Since the budget-integrity fix (ADR-0007, amended), a dispatch rebuilds its ledger from the `usage` of every `node_finished` in the run's journal, with the manifest only as a floor. Whatever usage a fork's journal carries, the fork is charged for.

**A node is given more than the blackboard.** The code and fix nodes edit the workspace through the agent; belay keeps no copy of a tree as it was, only the fingerprint beside each snapshot. The agent's conversation (`State.Code.SessionID`) is outside belay altogether, and later visits went on extending it.

### Decision

`belay fork <run-id> <node> [--visit N] [--model X] [--current-files]` makes a new run, with an id of its own, that re-runs one visit of an existing run from that visit's snapshot, runs that one node, and pauses. It never writes to the source run.

**Where it runs.** The fork works in a directory copy of the tree the source run's nodes worked in — the workspace for a run in place, the run's own worktree for one on a branch — under `<workspace>/.belay/forks/<fork-id>`, and its run directory is inside that copy. It always edits the copy in place and never commits: there is no branch for it to commit to. Continuing it is an ordinary resume of the new run, in its copy: `belay --workspace <copy> resume <fork-id>`, which `belay fork` writes out, as it writes out the matching `timeline` command, and `belay runs` in the source workspace names each fork with the command that lists it.

**What it is made of.**

- *Its journal* is the source's records from before the visit, then a `run_started` marking where the fork's own history begins. It stops short of the visit's own `node_started`, which the fork writes itself when it runs the node at attempt 1; a copy that kept it would read as a node that died while running, and the dispatcher would "recover" it at attempt 2 from the wrong snapshot. Seqs carry on from the source's, so the History entries the fork inherits on its blackboard, keyed on Seq, cannot collide with the ones it adds. The journal is written whole or not at all (`journal.Create`, on `ndjson.CreateNew`, which installs a complete, fsynced file with `link(2)` and never over an existing one).
- *Its inherited records carry no usage*, and *its ledger starts at zero*. Every dollar belongs to exactly one run. The amount the inherited history cost is recorded as `origin.inherited_usd` in the fork's manifest, together with the source, the visit, the tree copied and every way the fork differs from the original. `origin` is a record, not a pin, and names no capability.
- *Its blackboard* is the visit's snapshot, with the agent's session cleared. There is no falling back on `state.json`, which holds the blackboard after the visit: a run recorded before snapshots existed cannot be forked.
- *Its step count* carries on from the source's, so the node runs as the step it ran as. A step number is the run's identity, not its spending.
- *Its manifest* pins no archive directory — a copy nobody keeps must not file a plan into itself — and keeps its source's policy pin and baseline, because the copy is mid-change and cannot be a "before".
- *Its artifacts* are the plan and the reports its blackboard names, and nothing the source wrote after the visit.

**What it checks first, and says.** Before anything is created or spent, the fork compares each input it cannot put back with what the visit was given, and lists every difference by name:

- *Files.* The tree is surveyed and compared with the fingerprint taken before the visit. A tree that has changed is refused unless `--current-files` is given. The fingerprint's scheme tag is compared before the digest, so "fingerprinted by a different belay" is told apart from "your files changed", and an empty fingerprint is unknown, never "unchanged". The tree is surveyed again once copied, and a copy taken while it was changing is thrown away.
- *The plan.* `code` builds the plan it is given, so it is refused a plan that is not the one the gate approved; `plan` and `approve` judge whatever plan is there, so for them a changed plan is said, not refused.
- *Settings and the model.* Each setting that differs from what the source run was started with is named with both values, and `--model` is a model for the forked node only, routed and priced exactly as `agent.models` is.
- *The conversation* never carries over.

**What it refuses.** `fanout` and `join`, which run whole candidate runs that live in the source's directory; `promote`, which commits to a branch a fork does not have; a source whose manifest says running, or whose journal ends torn, because a step may still be changing its files; a visit with no snapshot, pointing a visit a fork inherited at the run that holds it.

It goes through `afterCompletion` like every dispatch, so a recording is saved — into the fork's copy, never over the source workspace's.

### Consequences

#### Positive
- A failed or disappointing step can be tried again — with another model, another setting, one more round — for the cost of that step, not of the run.
- The source is byte-identical afterwards, which a test asserts for its files, its run directory and its journal; both outcomes of the visit are kept, side by side.
- A fork is an ordinary run: it resumes, appears in `belay timeline` and `belay runs` (from its copy), can itself be forked, and is held to its source's guardrails.
- The fork's fidelity is checked rather than claimed: the blackboard comes from a snapshot, and the files are either shown unchanged by their fingerprint or accepted as they are now by the person, by name.

#### Negative
- A fork of any visit before the run's last change to the workspace usually needs `--current-files`, because belay never kept the old tree. In-place runs change the workspace as they go; a completed run's filed plan alone is a change.
- The fingerprint catches staleness, not content: a file rewritten with its size and modification time restored reads as unchanged (dircopy's survey states the trade).
- The fork's agent starts a new conversation and spends tokens rebuilding context the original visit already had.
- Each fork is a full copy of the tree, minus what dircopy skips, kept until a person deletes it; nothing prunes `.belay/forks`.
- A fork's timeline lists its inherited rows with no cost (they are not its spending), and its wall clock spans from the source's start.
- A fork's copy sits inside the source repository's working tree, under its git-ignored `.belay`, so git run in the copy finds that repository. A resume of the fork takes its in-place pin from its manifest all the same, as every resume takes its run's output pin.

#### Follow-ups
- Mark the boundary in a fork's `belay timeline`: where its own history begins, and its own wall clock.
- A way to delete forks; until then `rm -rf .belay/forks/<fork-id>` is safe, because the source never refers to them.
- In replay mode, a resumed fork looks for its cassette in its own copy unless `--cassette` names one.

### Alternatives Considered
- **Re-run the node in place, appending to the source's journal.** Makes a failed run resumable, throws away the record of the original outcome, and runs an agent against a tree the original may still own.
- **Keep the inherited records byte for byte and teach every reader of usage a boundary rule** (sum spend only after the last `run_started`). It works, but every reader that totals money — the ledger rebuild, the timeline — would have to learn the rule, and the one that forgot would charge or show a fork for its source's spending. Taking the usage off the inherited records means the journal itself says which run paid. The slice 3 design kept the records verbatim because it predates the ledger being rebuilt from the journal.
- **Fall back on `state.json` for a run recorded before snapshots.** It holds the blackboard after the visit, so the "re-run" would be of a step that never happened.
- **Copy the whole artifacts directory.** Puts evidence from the source's future in the fork.
- **Carry the agent session over.** Shows the node a conversation that already contains its own outcome.
- **Fork a fanout run's candidates.** They live in the source's directory, and joining them again belongs to fanout, which a fork never touches.

### Revisit If
- Most forks need `--current-files`: then snapshotting the changed files themselves becomes worth its disk.
- People fork to compare models more than to recover: then a command that forks several visits or models at once, and compares them, earns its place.
- `.belay/forks` grows large enough that people ask why.

## Part 2: The goal queue

<!-- slice:queue -- the slice that builds `belay queue` records its decision here. Keep this heading; replace this comment. -->

## References
- ADR-0002 (the dispatcher owns persistence), ADR-0003 (the append-only journal), ADR-0004 (directory copies; promotion of a copy is by hand), ADR-0007 (the budget ledger), ADR-0009 (record and replay: "replay" means cassettes), ADR-0011 (capabilities), ADR-0014 (routing is read live, policy is pinned)
- `internal/fork` (the package doc argues the design), `internal/cli/fork.go`
- `internal/journal/visits.go`, `internal/journal/create.go`, `internal/ndjson/create.go`, `internal/state/snapshot.go`, `internal/state/origin.go`
- `test/e2e/fork_test.go`
