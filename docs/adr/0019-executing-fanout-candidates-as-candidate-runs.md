# ADR-0019: Fanout candidates execute as concurrent candidate runs, each with its own journal, blackboard and budget envelope

## Status
Accepted

It corrects, without rewriting them, two earlier descriptions: ADR-0003's picture of one journal holding a record per node result "× fanout width", and the claim in `docs/architecture.md` that each candidate runs "its own subgraph" with "no synchronization until join". It builds on ADR-0002 (the dispatcher owns persistence), ADR-0004 and ADR-0012 (how a candidate's workspace is made), ADR-0007 as amended on 2026-09-19 (every step is charged what it spent; reaching the ceiling pauses), ADR-0011 (a control field names a capability) and ADR-0014 (the policy pin).

## Context

Best-of-N was in the README, in the configuration, in the graph and in ADR-0007's reason for existing, and it did not work. No node routed to `fanout`: `plan` and `approve` both named `code` as the next node. Had something routed there, the fanout node only prepared N workspaces, one after another, and handed join N candidates none of which had run a line of code, so no candidate's tests had passed and join could never choose a winner. A person who set `fanout.enabled: true` got one attempt at their goal and, until Wave 0b added a warning, no sign that anything was different.

The fanout node's own documentation explained why it stopped there, and the explanation deserves an answer rather than a workaround:

> the plan gives every candidate its own journal, and the v0.1 dispatcher does not yet provide one [...] Concurrency is a dispatcher capability, not a node one, and pretending otherwise here would produce N candidates whose journals interleave into a single run journal that no resume could untangle.

Checked against the code, every clause of that is right, and the first is an understatement.

**Interleaving is not merely hard to untangle; it is refused.** `journal.scan` treats a `node_started` opened while another is still open as corruption, and `journal.Open` refuses such a file from then on. Two concurrent candidates sharing a journal would break it on the second append.

**A journal per candidate is necessary and nowhere near sufficient.** The blackboard has one `Code`, one `Test`, one `Review` and one `Fix` block and a history keyed on one journal's sequence numbers. The budget ledger is rebuilt from one journal. A snapshot is keyed on one journal's sequence. The code node reads the plan through its own run's `Layout`. Process-group reaping walks one run's `nodes/` tree. A candidate needs every one of those, which is to say it needs to be a run.

**"Concurrency is a dispatcher capability" is right about persistence and wrong if it is read as "the dispatcher's loop must know about fanout".** The loop resolves `Result.Next` by exact name and `journal.ResolveStart` deliberately never guesses the graph's shape. Teaching the loop that `Next == fanout` means "start N sub-dispatchers" would put graph knowledge into the one component designed to have none, and would move the fanout's own decisions — how many candidates, what each is called, what each may spend, how their end states become a verdict — out of the one place they can be table-tested.

Two further problems were the node's own. Its candidate ids carried the attempt (`cand-01-a2`), so a crash inside the fanout made the re-run create a second set of workspaces and abandon the first "for the dispatcher to reclaim", which nothing did. And its cost projection assumed the run "reached fanout by planning and coding once", a premise the new route — plan, approve, fanout — removes.

## Decision

### A candidate is a run

Each candidate executes as a *candidate run* under `.belay/runs/<run-id>/candidate-runs/<cand-id>/`, with its own `journal.ndjson`, `state.json`, `manifest.json`, `artifacts/plan.md`, `snapshots/`, `nodes/` and budget ledger. Its own `graph.Dispatcher` drives it over the candidates' graph — code, write, the policy gate when the run has one, test, fix, review, starting at code, with a passing review ending the candidate — in a workspace the run's isolator made: a git worktree on `belay/<run-id>-<cand-id>` (ADR-0012) or a directory copy (ADR-0004) under `candidates/<cand-id>/`. The run directory sits beside the workspace rather than inside it, because promote destroys workspaces and the record of what was spent must outlive them.

A candidate's blackboard is seeded with the goal and the approved plan, and nothing else: every candidate starts its own conversation with the agent. Its manifest carries the parent's policy pin, so its own gate holds it to the guardrails the run was started under — without that, a candidate's gate would find no baseline, pass every change, and make best-of-N the way around a guardrail. Ids are bare (`cand-01`) and a pure function of the index, so a fanout's second attempt asks for exactly the candidates its first claimed, and the runner adopts them.

### The capability is the dispatcher's; the decision is the node's

The concurrency lives in `internal/graph`: a bounded pool, built by `NewDispatcher` when `Options.CandidateRegistry` is set and handed to nodes as `RunContext.CandidateRuns`. It claims all N candidates durably before any runs, provisions and drives them `fanout.concurrency` at a time with one child store and one child dispatcher each, recovers a panic per candidate, waits for every goroutine, and returns outcomes in candidate order. The fanout node decides everything else and starts no goroutine and writes no file. Every durable file still has exactly one writer, and no journal record gains a candidate tag. That is the original argument's conclusion, kept; what changed is that the dispatcher now provides the capability it said was missing.

### Whether a run fans out is pinned, and the pin reaches the nodes

`fanout.enabled`, `fanout.candidates` and `fanout.select` are recorded in the manifest when the run is created (`Manifest.SetFanout`), with the `fanout.candidate-runs` capability (ADR-0011), and every later invocation builds the graph from that pin: `nodes.WithFanout` replaces the work in the run's own graph with fanout and join, and opens the plan and the approve gate onto the fanout. A run created without the pin is walked as it always was, whatever `belay.yaml` says when it is resumed, and a drift between the two is a warning. The same pin is overlaid on the configuration every node reads (`Manifest.PinnedConfig`), so the fanout cannot count a different number of candidates, nor join use a different strategy, from the one the run began with. `fanout.concurrency` is deliberately not pinned: lowering it and resuming is how a person backs off a rate limit.

The run's own graph does not register the work nodes at all. The design proposed keeping them registered off the route so that a run created before this change could still be resumed, but such a run has no pin, never gets `WithFanout`, and keeps its full graph. Registering them would only turn a wiring bug that routed a fanout run into `code` into one silent attempt in the person's own workspace, where a registry without them stops the run on an unknown node.

### The budget is divided once, before any candidate runs

The fanout divides what is left of `budget.max_usd` (and of `budget.max_tokens`) evenly between the candidates, in the ledger's integer micro-dollars, before it claims anything, and each share is persisted in that candidate's manifest as its envelope and enforced by that candidate's own ledger. Because every candidate is claimed before any runs, the split happens once. An envelope is never lowered and may be raised when the ceiling is raised and the run resumed. `fanout.enabled` therefore requires `budget.max_usd` above zero: with no ceiling there is nothing to divide. Join and promote are exempt from the parent's ceiling, because they call no model and stopping them saves nothing while costing the run the delivery of work it has already paid for.

### A fanout settles only durable end states

A provisioning refusal no resume could change is written into the candidate's own manifest and settles as that candidate's outcome. Anything a resume could fix — a full disk, a git lock, a deadline, a panic — leaves the candidate's journal open and pauses the fanout with no Patch at all, so `belay resume` retries exactly that candidate while every finished one is adopted without running a node. One Ctrl-C pauses every candidate. A second Ctrl-C, or a `SIGKILL`, is the crash path, and the next resume heals each journal and reaps each candidate's recorded process groups before its node runs again — the single-run recovery, N + 1 times.

### What a candidate's quality gate can and cannot be

Several candidates reach their review node at once, and belay established, rather than assumed, what its gates do then.

golangci-lint takes one lock per machine, at `$TMPDIR/golangci-lint.lock`. The pinned version's own help (v2.12.2, `golangci-lint run --help`) says that without `--allow-serial-runners` it "exits with an error if it fails to acquire file lock on start". Measured, the default is a wait of about five seconds and then exit 3 with "parallel golangci-lint is running": with the lock held for 2 seconds a gate got through after 2.6; held for 5, 10 and 20 seconds it failed after 5.2 every time. Three gates started together over small modules queued inside the window and passed; five did not, three runs out of three. Giving each process its own `GOLANGCI_LINT_CACHE` changed nothing, because the lock is not in the cache. So the default is not safe under fanout, and not reliably broken either. belay passes `--allow-serial-runners` on every golangci-lint run: losers wait for the lock instead of failing, at the cost of wall-clock time inside their own envelope. `--allow-parallel-runners` was rejected because it shares a cache golangci-lint keys by file content while storing absolute paths (golangci/golangci-lint#3502), so a candidate could be handed a sibling's findings. `test/lintgate` pins the behaviour against the real tool, including a deterministic test that holds the lock for eight seconds itself.

SonarQube and AI review are refused with fanout. Both name the SonarQube project after the directory they review, and a candidate's directory is `cand-01`, `cand-02` and so on in every run in every repository, so every candidate would share projects with every other run's candidates and read a quality gate it did not produce. The refusal is checked against the pin, so a single run resumed while `belay.yaml` enables fanout is not refused for a fanout it does not have.

## Consequences

### Positive
- **Resume is the single-run path, N + 1 times.** A parent killed with three of five candidates in flight re-runs exactly those three open nodes, and the finished candidates run nothing. `test/e2e` proves it with a real `SIGKILL` of a child process and a crash-free control.
- **Nothing in the journal format, `journal.Validate`, `ResolveStart`, visits, `Patch`, `pkg/belay` or the dispatcher loop's ignorance of the graph's shape changed.**
- **Crash safety is inherited, not re-invented.** Snapshots, ledgers rebuilt from the journal, the estimated charge for a killed attempt and process-group reaping already worked per run, so they work per candidate, and every later improvement to them reaches candidates for free.
- **Every dollar belongs to exactly one ledger**, with a share known before anything is spent and shown by `--dry-run`.
- **A candidate run is inspectable with every existing tool**: `belay timeline <run> --candidate cand-02`, its snapshots, its artifacts, its evidence directories.
- **`fanout.concurrency: 1` is sequential execution for free.**

### Negative
- **The overshoot is N-fold.** A candidate is checked before a step and not during one, so each can pass its envelope by the cost of the step that crossed it: a fanout can end up to one step per candidate over `budget.max_usd`, and up to two per candidate when every candidate is killed during its first paid step, which the run has no evidence to charge for. A single run can overshoot by one step. The dry run says so.
- **Disk is N-fold, and nothing collects it.** A fanout run leaves N workspaces — N directory copies of the tree, or N worktrees — and N candidate-run directories, each with its own snapshots and evidence. Promote destroys ephemeral workspaces in a branch run; an in-place run keeps every candidate's copy, because the winner's change is copied back from it by hand. The `belay cleanup` command ADR-0004 named as a follow-up is still owed, and it is now N run directories larger.
- **An in-place fanout does not merge.** Its winner's change stays in the winner's workspace, and the run's next steps name the directory and the timeline command; only a branch run promotes the winner.
- **An unspent envelope is not given to the other candidates,** and lowering `budget.max_usd` mid-fanout does not take back a share already given.
- **N concurrent agents, test suites and linters compete** for CPU, for the agent's rate limit, and for golangci-lint's lock, which serialises review.
- **`select: fastest` is not stable across a change of `fanout.concurrency`,** because a duration measured under contention is a different number. `fewest_issues` and `first_pass` are.
- **A panic in a goroutine a node itself starts still ends the process.** Only the candidate's own goroutine is recovered.
- **The parent's blackboard has no `Test` or `Review` of its own in a fanout run.** The detail lives in the candidate runs.
- **A directory-copy candidate first provisioned after a crash copies the checkout as it is then.** A branch run is immune, because its candidates start from a commit.

### Follow-ups
- `belay cleanup`, from ADR-0004, now covering candidate runs and their workspaces.
- Scope SonarQube project keys per run and candidate, then lift the block on SonarQube and AI review.
- A cross-process lock on a run, so two `belay resume` of one parent cannot drive the same candidates.
- Deterministic reallocation of unspent envelopes at fixed points.
- Early cancellation of losers under `first_pass`, once "first" can be made stable across a resume.

## Alternatives Considered
- **One journal with candidate-tagged records.** It changes a closed record format, needs per-lane validation, resume points and visits, still needs N blackboards and N ledgers because the blackboard's blocks collide, and makes every existing tool lane-aware. It re-implements N runs inside one file.
- **Fanout inside the dispatcher loop, with no fanout node.** The literal reading of "concurrency is a dispatcher capability". The loop would have to recognise a graph shape, which `ResolveStart` and the node contract are built on it not doing, and the fanout's guards would move into the loop with it.
- **Sequential candidates inside the parent graph**, saving and restoring blackboard blocks per candidate. It multiplies wall time by N and adds save-and-restore of control state; `fanout.concurrency: 1` already gives sequential execution.
- **A shared live budget pool.** Which candidate is starved would depend on goroutine timing, breaking convergence on resume, and it rewards whichever candidate burns fastest.
- **Run-qualified candidate ids.** Both isolators are already rooted at one run's directory, so bare ids cannot collide across runs, and promote's two-trailer rule exists because they are bare.
- **Candidate runs as child processes.** Deferred: fault isolation at the price of process supervision, signal forwarding and output multiplexing. Each candidate is already a run directory, so the switch would be local to the candidate runner.
- **`--allow-parallel-runners` for golangci-lint.** Faster, and it hands a candidate a sibling's findings through the shared cache.
- **Keeping the work nodes registered in a fanout run's own graph.** See "Whether a run fans out is pinned" above: the reason given for it does not hold, and the cost is a silent single attempt where a loud stop belongs.

## Revisit If
- Measured overshoot on real runs regularly exceeds a quarter of `budget.max_usd`. That argues for a per-step guard inside the backend, or smaller envelopes.
- Candidates routinely finish far under their envelope while siblings stop on theirs. That argues for reallocation.
- Contention for tools or for the agent's rate limit dominates failures at the default concurrency.
- A golangci-lint release changes how its lock behaves; `test/lintgate` will say so.
- Anyone asks for two `belay resume` of one parent at once. That needs the run lock first.

## References
- ADR-0002, ADR-0003, ADR-0004, ADR-0007 (amended 2026-09-19), ADR-0011, ADR-0012, ADR-0014
- `internal/nodes/fanout` — the package doc, which quotes what it used to say
- `internal/nodes/wire.go` — `WithFanout`, `Candidate`, and the conditional-node rule
- `internal/graph/children.go` — the candidate runner
- `internal/state/candidate_run.go` — `FanoutPin`, `PinnedConfig`, `Envelope`, `CandidateRun`
- `internal/linter/golangci.go` — `golangciSerial`; `test/lintgate/concurrent_test.go`
- `test/e2e/fanout_test.go` — the end-to-end run and the crash test
