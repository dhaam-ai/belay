# ADR-0017: Forking a run to re-run one node, and the goal queue

## Status
Accepted (part 1, forking a run, and part 2, the goal queue).

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

### Context

The owner asked for it by name: add goals, walk away, and belay works through them one at a time, surviving crashes and reboots, and reports. Everything belay had was designed for a person at the terminal, and an unattended queue runs into each piece of it:

- **The approve gate pauses.** A run stops after its plan so a person can read it. Nobody is there to read it in the night.
- **Every run has its own ceiling** (ADR-0007), and a queue is many runs. A person at a terminal sees a run spend; nobody watches a queue spend.
- **Nothing arbitrates between processes.** internal/journal and internal/state assume one dispatcher owns a run. A queue is written by `belay queue add` in one terminal while `belay queue start` works through it in another.
- **A queue is a promise about many runs, and a crash can land between any two of its steps.** The worst outcome is not losing a goal: it is running one twice, which pays for it twice and leaves two half-results nobody asked for.
- **A run's identity was always minted fresh.** `belay run` asks `state.NewRunID` for an id the moment before it makes the run, so it could never meet one that already exists. A queue must record which run a goal is before that run exists, and so is the first caller that supplies its own id.

### Decision

`belay queue add|list|status|rm|approve|start`, built on internal/queue.

**One append-only log.** The queue is `.belay/queue/queue.ndjson`, a closed set of records (`entry_added`, `entry_removed`, `entry_claimed`, `entry_settled`, `entry_approved`, `worker_started`, `worker_stopped`) written through internal/ndjson — one line, one fsync, a torn final line healed before the next append. Its state is a fold of the records, and the fold refuses, with the seq where it went wrong, any history no belay writes: a gap in the sequence, an id given twice, a move a goal cannot make. Above all a second claim: a goal is claimed once, from the queue, and no history that claims it twice survives a fold.

**Two flocks.** Every writer holds the log lock (`queue.lock`) for the milliseconds it takes to read the log, check its change against the fold, and append; a store never writes a record the fold would refuse. One `belay queue start` holds the worker lock (`worker.lock`) for as long as it works, so the queue runs one goal at a time across every process on the machine. Both are flock(2), which the kernel drops when its holder dies, so a reboot or a `kill -9` never leaves a queue locked behind a process that is gone.

**Claim before create.** The worker records `entry_claimed` — the goal's run id and the path of its copy — before it makes either. Only then does it copy the workspace, make the run with `createRun`, and dispatch it. Every step starts from the disk and asks one pure function, `Next`, what to do, so a worker killed between any two steps and a new one started over the same disk are the same worker: the claim names exactly what was being made, and the next worker makes that run rather than a second one. `createRun` now refuses an id whose manifest exists, and the worker looks at the run again before it clears anything from a copy, so no step can start a run over or delete one.

**A copy per goal, made when the goal is claimed.** Each goal runs in a directory copy of the workspace (internal/isolate/dircopy, with its skip list) at `.belay/queue/work/<run-id>`, taken as the workspace stands when belay reaches the goal, and its run lives inside that copy. Queued goals always run in place — the copy is the isolation — and pin no archive directory, whatever `belay.yaml` says: a branch run would find the person's own repository a few directories above the copy, and a plan filed into a copy is lost with it. Each goal's run reads the `belay.yaml` in its copy.

**Parked, never overridden.** A goal whose run stops at the approve gate because the gate asked about its plan is settled as waiting, and the worker moves on to the next goal. It is never driven again until a person approves it with `belay queue approve <goal>`, which records the approval in the run first (the approval the gate reads) and in the queue second (the signal that sends the next worker back to it). Only a goal the person added with `--no-approval` skips review; belay never turns a gate off on its own. A goal whose run is paused at the gate without the gate having asked — a Ctrl-C while the gate ran, or the budget ceiling stopping the run before the gate — is not parked: only the run's journal can tell the two apart, and the worker asks it.

**One ceiling per goal, announced first.** Every goal is held to `budget.max_usd` by its own run's ledger. `belay queue start` says how many goals it will run, the ceiling, and the product, before it spends anything, and refuses `budget.on_exceed: warn`, because an overrun nobody is watching is not a warning.

**Ctrl-C and reboots.** The worker dispatches with `PauseOnCancel` and calls `afterCompletion`, as `belay resume` does: Ctrl-C pauses the goal in progress where it stands, the worker records that it stopped and settles nothing, and the next start carries that goal on before anything else. A reboot needs nothing but `belay queue start` again. A run belay paused at one of its own limits is settled as paused, named in `belay queue status` with the command that carries it on, and never retried by the queue.

**`belay runs` keeps its JSON.** Queued goals' runs live in their copies, so `belay runs` in the workspace does not list them; its table says where they are and prints the `--workspace` command that lists each. Nothing in its documented JSON shape changes.

No `requires` capability is added. A queued goal's run is an ordinary in-place run with no field an older belay would misread, and the queue's log is not a run document.

### Consequences

#### Positive
- No goal runs twice, at any crash point. Tests kill the worker before the copy, half way through it, after it, inside the run, and after the run ended but before the goal was settled; every time a new worker finishes each goal once, under the run its first claim named.
- The approve gate means the same thing in a queue as at a terminal. Planning is cheap and coding is not, so a queue with approval on plans overnight and waits for the morning; approved goals run on the next start, from the plans that were paid for.
- Spend is bounded per goal and announced before it happens, and a goal's ledger carries on across a crash rather than starting over.
- A crash, a kill or a reboot leaves nothing to repair, and `belay queue status` says when the last start was cut off.
- The person's checkout is never touched.

#### Negative
- Disk grows with the number of goals times the size of the workspace, as ADR-0004 accepted for fanout. Copies are kept, because a finished goal's change is in its copy, and nothing removes them.
- Taking a finished goal's change is by hand, as for a dircopy fanout winner (internal/nodes/join): the copy has no `.git`, so it is compared and copied, not merged.
- The copy is made with dircopy's skip list, so it has no `node_modules`, `vendor`, `.venv` or build output. A goal whose tests need them must be able to rebuild them, or fail at its tests.
- Goals are independent: each starts from the workspace as it is when claimed, and none sees another's result.
- Each goal's run reads the `belay.yaml` in its copy, so raising a limit for a goal paused at one means editing that file (or passing `--config`) before resuming it by hand.
- A run paused at a limit whose settle a crash cut off is continued once by the next start, because only the journal's notes, in words, tell that pause from a Ctrl-C's. At the budget or the step cap the dispatcher meets the limit again at once, spending nothing; at `graph.node_timeout` it re-runs the step that overran once, within the goal's budget.
- flock is not reliable on network filesystems; a queue in a workspace on one is unsupported, which is documented and not detected.
- `graph.approval` is still read live. A `--no-approval` goal resumed by hand with `belay --workspace <copy> resume` reads its copy's `belay.yaml`, and may stop at the gate: more review than the person asked for, never less.

#### Follow-ups
- Taking a finished goal's change: a command that shows or applies the difference between a goal's copy and the workspace.
- A queue-wide ceiling, once there is a definition of "tonight" that survives a restart without handing each restart a fresh one.
- Removing the copies of goals whose change has been taken, with ADR-0004's `belay cleanup`.
- Listing queued goals' runs in `belay runs` itself, which changes its JSON shape and so is a decision of its own.
- Pinning `graph.approval` at `createRun` (already on the backlog), after which a `--no-approval` goal resumed by hand keeps its choice.

### Alternatives Considered
- **Turn `graph.approval` off for queued goals**: removes a gate the person configured, on belay's own authority. Rejected outright.
- **Abandon a goal that pauses**: throws away a plan that already cost money and would have to be bought again.
- **Run goals in the checkout, one after another**: a goal that fails leaves a broken tree for the next goal's fix loop, and the person's checkout changes under them.
- **Run goals concurrently**: they compete for the agent's rate limit and the machine, and the queue gains nothing it needs.
- **A `queue.json` rewritten in place, or a file per goal**: both lose the history — a claim picked up after a reboot, a start that was cut off — that the morning report reads, and neither avoids the lock `rm` racing a claim needs.
- **A lock that is a file's existence**: stays taken by a dead process, so every reboot would begin with a person deleting it.
- **One baseline copy made at `belay queue start` for every goal**: reproducible batches, but a person's edits during the day would not reach goals claimed after them. Settled the other way: each goal's copy is the workspace as it stands when the goal is claimed.
- **A daemon or a system service**: a queue spends money unattended, and starting that is a person's decision each time.

### Revisit If
- People ask for goals that build on one another, which would call for a stop-on-failure mode in one shared tree, or branches.
- People want to cap a night rather than each goal.
- The disk the copies take becomes the complaint, or most finished goals are taken by hand the same way.

## References
- ADR-0002 (the dispatcher owns persistence), ADR-0003 (the append-only journal), ADR-0004 (directory copies; promotion of a copy is by hand), ADR-0007 (the budget ledger), ADR-0009 (record and replay: "replay" means cassettes), ADR-0011 (capabilities), ADR-0014 (routing is read live, policy is pinned)
- `internal/fork` (the package doc argues the design), `internal/cli/fork.go`
- `internal/journal/visits.go`, `internal/journal/create.go`, `internal/ndjson/create.go`, `internal/state/snapshot.go`, `internal/state/origin.go`
- `test/e2e/fork_test.go`
- For part 2: ADR-0010 (the CLI is designed for how people think), ADR-0012 (a run's output is pinned when it is created), ADR-0016 (archiving the plan, which a queued goal does not)
- `internal/queue` (the package doc argues the design), `internal/cli/queue.go`, `internal/cli/run.go` (`createRun`, `dispatchOnce`, `afterCompletion`)
- `test/e2e/queue_test.go`
