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
package queue
