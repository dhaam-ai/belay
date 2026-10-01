# ADR-0017: Forking a run to re-run one node, and the goal queue

## Status
Accepted (the goal queue, slice 3d). The fork half is slice 3c's, written in parallel; its sections are marked below and merge into this file.

Two features share this record because they share their ground: both ask belay to act on work nobody is watching as it happens, both rest on the same merged primitives (internal/ndjson's append-only discipline, `createRun` writing the manifest last, `graph.Options.PauseOnCancel`, the output pin), and both create runs that live somewhere other than the workspace's own `.belay/runs`. Each half below stands on its own.

<!-- fork half: slice 3c writes the sections from here to "end fork half" -->

## Forking a run (slice 3c)

_Slice 3c's half of this record: `belay fork <run-id> <node>`. Written by that slice and merged here._

<!-- end fork half -->

<!-- queue half: slice 3d -->

## The goal queue (slice 3d)

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

<!-- end queue half -->

## References
- ADR-0002, ADR-0003, ADR-0004, ADR-0007, ADR-0010, ADR-0011, ADR-0012, ADR-0016
- internal/queue (the log, the locks, the fold, `Next`, the worker); internal/cli/queue.go (the commands and the driver)
- internal/cli/run.go (`createRun`, `dispatchOnce`, `afterCompletion`); internal/graph/dispatcher.go (`PauseOnCancel`)
- internal/ndjson; internal/isolate/dircopy
