# ADR-0003: Append-Only NDJSON Journal with Fsync Per Record as Source of Truth

## Status
Accepted

## Context
The run state must survive a crash. Three approaches were considered:

1. **SQLite (or another database)**: Transactional writes, ACID properties, query interface. Downsides: requires a CGo dependency (or a pure-Go port with its own schema migration burden), adds operational complexity (database file locking, schema versioning), complicates testing.

2. **Single mutable JSON state file**: One file (state.json) holds the current run state. Cheap to read and write, no schema. Downside: a crash during the write leaves the file in a torn or incomplete state, losing the entire run. Recovery would require reading a backup or reverting — fragile.

3. **Append-only NDJSON journal with fsync per record**: Each node result appends a new line (JSON object) to a journal file and calls fsync(). On resume, the journal is replayed from the beginning. Downsides: no update-in-place (always append), a torn final line must be tolerated on read, recovery requires replaying all history.

## Decision
An append-only NDJSON journal was chosen as the source of truth. Each state change (node completion, manifest update, budget consumption) appends a record and fsyncs. The run state is derived from the journal by replaying records in order. The final line may be incomplete (torn write); resume tolerates truncating it.

## Consequences

### Positive
- **Crash-safe without transactions**: Any complete line is durable. A crash mid-write leaves an incomplete final line that is safely ignored on resume.
- **No schema migration**: Journal is append-only; new record types can coexist with old ones. Backward compatibility is simpler.
- **Auditability**: Every state change is logged in order. Debugging failed runs is straightforward — replay the journal and inspect state at each step.
- **No external dependencies**: NDJSON is plain text; no database driver required. Runs are portable (email a journal, replay it elsewhere).

### Negative
- **Disk space**: A long run with many candidates generates a large journal (one record per node result × fanout width). No automatic compaction.
- **Read performance**: Resume must replay the entire journal from the beginning. For 1000-line journals this is instant; for 100,000-line journals it may be slow.
- **Torn final line handling**: The code must tolerate incomplete JSON on the final line. This requires a custom JSON decoder that stops at the last complete line.
- **No update-in-place**: Cannot correct a recorded decision (e.g., change a candidate's status). Only append a new record that supersedes it.

### Follow-ups
- Implement a custom NDJSON reader in `pkg/journal/` that truncates incomplete final lines and returns the last complete record.
- Document the recovery algorithm in code: "Replay the journal, applying each record to state; ignore lines after the last complete newline."
- Add a `belay journal compact` command (future) to rebuild the journal into a single frozen snapshot plus a new active journal, reducing replay time.

## Alternatives Considered
- **SQLite**: Would provide true transactions. Lost because: (1) adds a CGo dependency or pure-Go porting burden, (2) schema migration complexity, (3) user must manage database file (backup, lock timeouts), (4) harder to audit (logs are hidden, not in the file).
- **Single mutable JSON**: Simpler initially. Lost because: (1) crash mid-write loses the entire run, (2) no auditability, (3) no clear recovery strategy if corruption is detected.

## Revisit If
- Users report journal replay taking >10 seconds on resume (measure first; compact command may be sufficient).
- A use case emerges that requires updating past decisions (not just appending new ones).
- Disk usage becomes a constraint (journal size grows > 1 GB for reasonable runs).

## References
- Challenge #134 phase 3: "Persist Run State"
- JSON Lines format: https://jsonlines.org/
- pkg/journal/reader.go (T46)
