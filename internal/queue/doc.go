//go:build unix

// Package queue keeps belay's goal queue: the goals a person adds with `belay
// queue add`, worked through unattended, one run per goal, by `belay queue
// start`.
//
// # One append-only log
//
// The queue is a single file, .belay/queue/queue.ndjson, of newline-delimited
// JSON records (Record), only ever appended to. Its state is what folding the
// records in order gives (fold): every goal, where it stands, and what the
// last worker did.
//
// A log rather than a queue.json rewritten in place, or a file per goal,
// because what the queue needs to say in the morning is history, not just a
// state: that a goal was claimed and its run picked up again after a reboot,
// that the last start was cut off before it could record stopping. A file
// rewritten in place keeps only where things ended. And a log is what belay
// already knows how to make crash-proof: the bytes are written through
// internal/ndjson, the run journal's own discipline (ADR-0003) — one line, one
// fsync, never a rewrite, and a torn final line healed before the next append —
// so the queue gets that promise without a second implementation of it.
//
// # The fold refuses what no belay writes
//
// Every decision the queue makes is a reading of the fold, so the fold never
// guesses. A record out of sequence, a record missing what its event needs, an
// id added twice, an event about a goal nobody added, and any move a goal
// cannot make are refused with ErrCorruptQueue and the seq where the history
// went wrong. The move that matters most is the claim: a goal is claimed once,
// and only from the queue, so no history in which a goal was claimed twice
// survives a fold, and a goal claimed twice is the only way one could run
// twice.
//
// # Two locks, because two kinds of process share the queue
//
// A person adds goals from one terminal while `belay queue start` works
// through them from another, and both write the log. Nothing in
// internal/ndjson arbitrates between processes, so this package does, with two
// flock(2) locks under .belay/queue:
//
//   - The log lock, queue.lock, is held by every writer for the few
//     milliseconds it takes to read the log, check that what it is about to
//     record still makes sense, and append it. That check under the lock is
//     what keeps `belay queue rm` from removing a goal a worker claimed a
//     moment earlier, and two `belay queue add`s from giving out one id.
//   - The worker lock, worker.lock, is held by one `belay queue start` for as
//     long as it works (WorkerLock). It is what makes the queue run one goal at
//     a time across every process on the machine, not just within one.
//
// Both are flocks rather than files whose existence means "locked", because a
// flock dies with its holder: the kernel drops it when a killed process's
// descriptors are closed, so a reboot or a kill -9 never leaves the queue
// locked behind a process that is gone. flock is not reliable on network
// filesystems, and a queue in a workspace on one is not supported.
//
// # Claim before create
//
// A Worker takes a goal by appending entry_claimed, naming the run the goal
// will be and the copy it will run in, before it makes either. Only then does
// it copy the workspace and make the run, and only then dispatch it. So
// whenever a crash lands, the log already names exactly what was being made,
// and the next worker makes that same run rather than a second one:
//
//   - after the claim, with no copy or half of one: the copy is cleared and
//     made again, and the run made in it. Nothing has run, since the run did
//     not exist.
//   - after the copy, before the run's manifest: the same. createRun writes
//     the manifest last, so a run without one was never made.
//   - inside the run: the run exists, and is continued; the dispatcher heals
//     its journal and re-runs the step that died, as for any crashed run.
//   - after the run ended, before the goal was settled: the run's own state
//     says how it ended, and the goal is settled from that without running
//     anything again.
//
// Claiming after creating would leave a window in which a run exists that no
// record names, and the next worker would claim the goal again, under a new
// run id, and pay for it twice.
//
// Every step a worker takes begins by reading the log and the run of the goal
// in hand from disk, and asks Next what to do. Nothing is carried from one
// step to the next in memory, so a worker killed between any two steps and a
// new one started over the same disk are the same worker.
//
// # Parked, never overridden
//
// A goal whose run stops at the approve gate for a person to read its plan is
// parked: settled as waiting, and the worker moves on to the next goal. It is
// never driven again until a person approves it (Store.Approve, after the
// approval is recorded in the run itself). The worker cannot tell a person's
// absence from a person's "no", and belay never removes a human gate on its
// own. The only goal that skips review is one added with no_approval, which
// the person typed when they added it; nothing else sets it, and it travels
// in the log through every crash and restart.
//
// # One goal at a time, each in a copy of its own
//
// One worker holds the queue, and it has one goal in hand at a time: nothing
// is claimed while a claimed goal is unsettled, and a goal already in hand,
// claimed or approved, is finished before a queued one is claimed. Each goal
// runs in its own copy of the workspace, taken as it stands when the goal is
// claimed, so a goal that fails leaves nothing behind for the next one to
// trip over, and none of them edits the checkout the person is working in.
// Running goals side by side would compete for the agent's rate limit and the
// machine, and gain nothing the queue needs.
//
// # What a reboot does
//
// Nothing, until `belay queue start` is run again. The worker lock died with
// the worker, the log names the goal it had in hand, and the new worker
// continues that goal, then the rest. A worker that stopped by itself records
// why (worker_stopped); one with no record was cut off, and the next worker
// says so (Report.PreviousCutOff).
//
// # What this package does not do
//
//   - It does not configure, dispatch or narrate a run. Those belong to
//     internal/cli, behind the Driver interface; this package knows the
//     queue, and the order things must happen in.
//   - It never retries a goal that failed, stopped, or paused at one of
//     belay's limits. A person decides whether the limit was wrong.
//   - It does not run as a service or start itself after a reboot. A queue
//     worked through unattended spends money unattended, and starting that is
//     a person's decision each time.
//   - It does not chain goals. Each starts from the workspace as it stands
//     when it is claimed, and none sees another's result.
package queue
