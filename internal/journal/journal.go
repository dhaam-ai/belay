// Package journal implements belay's append-only run journal: the sole
// source of truth for "a crashed run resumes instead of restarts."
//
// # Format
//
// A journal is a file of newline-delimited JSON (NDJSON), one Record per
// line, in strictly increasing Record.Seq order starting at 1:
//
//	{"seq":1,"event":"run_started","ts":"2026-08-29T10:31:00Z"}
//	{"seq":2,"attempt":1,"node":"plan","event":"node_started","ts":"2026-08-29T10:31:00.4Z"}
//	{"seq":3,"attempt":1,"node":"plan","event":"node_finished","ts":"...","duration_ms":812,"status":"ok","next":"approve","usage":{"input_tokens":0,"output_tokens":0,"usd":0,"estimated":false}}
//
// # Durability
//
// Journal.Append opens the file with O_APPEND|O_CREATE|O_WRONLY, writes one
// line, and calls File.Sync before returning. Because every write only ever
// adds bytes past the current end of file — nothing already durable is ever
// rewritten — a crash during one Append can corrupt at most the record being
// written, never an earlier one. Read and ReadFile give that half-written
// final line its own signal (ReadResult.Incomplete) rather than an error, so
// callers can tell "nothing more happened" apart from "something happened
// but we don't know what."
//
// Open heals a torn tail it finds before resuming writes, so a crash never
// needs manual repair: the incomplete line is discarded (via an
// atomic-rename rewrite of the clean prefix, itself crash-safe) and the
// journal is once again a clean sequence of complete lines ready for the
// next Append.
//
// # Concurrency
//
// A *Journal serializes its own Append calls with a mutex, so many
// goroutines in one process may append concurrently and safely. Concurrent
// appends from separate PROCESSES to the same file are not supported: belay
// assumes exactly one dispatcher owns a run's journal at a time, and nothing
// here arbitrates across process boundaries (no flock, no lease).
//
// # Secrets
//
// Record.Note is free text, persisted and later displayed verbatim (for
// example by `belay timeline`). This package never inspects or redacts it —
// redaction is internal/exec's responsibility. Callers MUST pass
// already-redacted text to Append.
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxLineBytes bounds how large a single journal line may be before Read
// gives up on it. It is generous enough for a long Record.Note while still
// making a runaway or corrupted line fail fast instead of exhausting memory.
const maxLineBytes = 8 * 1024 * 1024

// FirstNode is the entry point of belay's node graph. ResolveStart returns
// it, at attempt 1, whenever a journal carries no history to resume from.
const FirstNode = "plan"

// Journal is a handle on one run's append-only journal file. The zero value
// is not usable; construct one with Open.
//
// A *Journal is safe for concurrent use by multiple goroutines within one
// process (see the package doc's Concurrency section).
type Journal struct {
	mu            sync.Mutex
	f             *os.File
	path          string
	lastSeq       uint64
	recoveredTorn bool
}

// Open opens the journal at path for appending, creating it if it does not
// exist. If the file already has records, Open streams them first (see
// ReadFile) to recover the next Seq to assign and to refuse to append onto a
// journal whose existing history is already corrupt (see Validate) — both
// checks that matter for a file that is itself the record of a previous
// crash.
//
// A torn final line left by an earlier crash is not an error: Open discards
// it — see the note on rewriteClean below for exactly how — as though that
// incomplete record had never been written, and RecoveredTornTail reports
// that it did so.
//
// Callers must call Close when done with the returned *Journal.
func Open(path string) (*Journal, error) {
	existing, err := ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("journal: open %s: %w", path, err)
	}
	if err := Validate(existing.Records); err != nil {
		return nil, fmt.Errorf("journal: open %s: refusing to append onto a corrupt journal: %w", path, err)
	}

	var lastSeq uint64
	for _, rec := range existing.Records {
		if rec.Seq > lastSeq {
			lastSeq = rec.Seq
		}
	}

	// A torn tail or a missing final delimiter must be healed before we
	// resume O_APPEND writes, or the next Append would land its bytes
	// directly onto the end of a broken or delimiter-less line, silently
	// turning one bad record into a permanently unreadable file.
	if existing.Incomplete {
		if err := rewriteClean(path, existing.Records); err != nil {
			return nil, fmt.Errorf("journal: open %s: %w", path, err)
		}
	} else if err := ensureTrailingNewline(path); err != nil {
		return nil, fmt.Errorf("journal: open %s: %w", path, err)
	}

	// #nosec G304 -- path is the run journal the caller named; belay's own
	// commands derive it from their run-directory convention, and this
	// package has no policy opinion about where a journal may live.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: open %s: %w", path, err)
	}

	return &Journal{
		f:             f,
		path:          path,
		lastSeq:       lastSeq,
		recoveredTorn: existing.Incomplete,
	}, nil
}

// rewriteClean atomically replaces the file at path with exactly records,
// each marshaled on its own newline-terminated line, discarding anything
// else (a torn final line). It writes a temporary file in the same
// directory and renames it into place, so a second crash during recovery
// itself still leaves path either fully in its old (torn) state or fully in
// its new (clean) state — never something in between.
func rewriteClean(path string, records []Record) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".recover-*")
	if err != nil {
		return fmt.Errorf("create recovery file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once the rename below succeeds

	for _, rec := range records {
		data, err := json.Marshal(rec)
		if err != nil {
			_ = tmp.Close()
			return fmt.Errorf("re-encode seq %d during recovery: %w", rec.Seq, err)
		}
		data = append(data, '\n')
		if _, err := tmp.Write(data); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("write seq %d during recovery: %w", rec.Seq, err)
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync recovery file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close recovery file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install recovered journal: %w", err)
	}
	return nil
}

// ensureTrailingNewline appends a single newline to path if the file is
// non-empty and does not already end in one.
//
// This closes the one gap Read tolerates without flagging Incomplete: a
// final record whose content is fully valid JSON but whose trailing
// delimiter byte itself was the one cut off by a crash. Read correctly
// counts that record as durable (its content is intact), but the file on
// disk needs its delimiter completed before another line can be appended
// after it without the two merging into one unparseable line.
func ensureTrailingNewline(path string) error {
	// #nosec G304 -- path is the run journal Open was called with; see the
	// same justification on the O_APPEND open in Open.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check trailing newline of %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("check trailing newline of %s: %w", path, err)
	}
	if size == 0 {
		return nil
	}

	last := make([]byte, 1)
	if _, err := f.ReadAt(last, size-1); err != nil {
		return fmt.Errorf("check trailing newline of %s: %w", path, err)
	}
	if last[0] == '\n' {
		return nil
	}

	if _, err := f.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("complete trailing newline of %s: %w", path, err)
	}
	return f.Sync()
}

// Path returns the path the journal was opened with.
func (j *Journal) Path() string { return j.path }

// LastSeq returns the Seq of the most recently appended record, or 0 if
// none has been appended yet in this journal's history.
func (j *Journal) LastSeq() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastSeq
}

// RecoveredTornTail reports whether Open found and discarded an incomplete
// final line left by a previous crash. Callers such as `belay run` can use
// it to tell the operator "resuming after an interrupted run."
func (j *Journal) RecoveredTornTail() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.recoveredTorn
}

// Append validates rec, assigns it the next Seq, fills in Time when rec.Time
// is the zero value, and durably appends it to the journal: write, then
// File.Sync, before returning. It returns the exact Record written,
// including the assigned Seq.
//
// Concurrent callers within one process are serialized; see the package
// doc's Concurrency section for what is and is not safe across processes.
//
// A failure from the underlying write or sync leaves the journal's Seq
// counter advanced even though the record may not be (fully) durable: retrying
// with the same logical event would otherwise risk writing two records that
// claim the same Seq. A partially written line from such a failure is
// exactly the torn-tail case Read tolerates.
func (j *Journal) Append(rec Record) (Record, error) {
	if rec.Time.IsZero() {
		rec.Time = time.Now().UTC()
	}
	if rec.Event.nodeScoped() && rec.Attempt == 0 {
		rec.Attempt = 1
	}
	if err := rec.validate(); err != nil {
		return Record{}, fmt.Errorf("journal: append: %w", err)
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	rec.Seq = j.lastSeq + 1

	data, err := json.Marshal(rec)
	if err != nil {
		return Record{}, fmt.Errorf("journal: append: encode seq %d: %w", rec.Seq, err)
	}
	data = append(data, '\n')

	j.lastSeq = rec.Seq

	if _, err := j.f.Write(data); err != nil {
		return Record{}, fmt.Errorf("journal: append: write seq %d: %w", rec.Seq, err)
	}
	if err := j.f.Sync(); err != nil {
		return Record{}, fmt.Errorf("journal: append: sync seq %d: %w", rec.Seq, err)
	}
	return rec, nil
}

// Close closes the underlying file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.f.Close(); err != nil {
		return fmt.Errorf("journal: close %s: %w", j.path, err)
	}
	return nil
}

// ReadResult is the outcome of streaming a journal.
type ReadResult struct {
	// Records holds every record that parsed cleanly, in file order.
	Records []Record
	// Incomplete is true when the stream's final line did not form a
	// complete, parseable record — the signature of a process killed
	// mid-write. It is never treated as an error: everything before that
	// line is fully valid and is still returned in Records.
	Incomplete bool
}

// Read streams records from r.
//
// Read tolerates exactly one failure shape without raising an error: a torn
// final line. Every earlier line must be a complete, valid Record, or Read
// reports it as a *MalformedRecordError — a broken line anywhere but the
// last is never a torn write, because a journal is only ever appended to,
// never rewritten in place.
//
// Read is streaming: it holds at most one line's bytes in memory at a time
// (bounded by maxLineBytes) and never copies a line it does not need to, so
// reading a very large journal costs O(1) scratch memory beyond the records
// it returns.
func Read(r io.Reader) (ReadResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	var (
		result     ReadResult
		line       int
		pendingErr error // set when the immediately preceding line failed to parse
	)

	for sc.Scan() {
		line++
		if pendingErr != nil {
			// Another line exists after the one that failed to parse, so
			// that failure was not a torn final write — it is corruption.
			return result, &MalformedRecordError{Line: line - 1, Err: pendingErr}
		}

		// parseLine decodes fully into rec (json.Unmarshal copies whatever
		// it keeps, e.g. every string field, into rec's own storage), so it
		// is safe to hand it sc.Bytes() directly without copying: nothing
		// retains a reference to the scanner's internal buffer past this
		// call, which the scanner is free to overwrite on the next Scan.
		rec, err := parseLine(sc.Bytes())
		if err != nil {
			pendingErr = err
			continue
		}
		result.Records = append(result.Records, rec)
	}
	if err := sc.Err(); err != nil {
		return result, fmt.Errorf("journal: read: %w", err)
	}

	if pendingErr != nil {
		result.Incomplete = true
	}
	return result, nil
}

// ReadFile opens path and streams it through Read.
//
// A missing file reads as an empty journal (ReadResult{}, nil error) rather
// than an error, matching ResolveStart's "empty/absent" case: a run that
// never started looks exactly like a run whose journal is empty.
func ReadFile(path string) (ReadResult, error) {
	// #nosec G304 -- path is the run journal the caller named; see the same
	// justification on the O_APPEND open in Open.
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ReadResult{}, nil
	}
	if err != nil {
		return ReadResult{}, fmt.Errorf("journal: read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	result, err := Read(f)
	if err != nil {
		return result, fmt.Errorf("journal: read %s: %w", path, err)
	}
	return result, nil
}

func parseLine(line []byte) (Record, error) {
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// ErrMalformedRecord indicates a journal line could not be parsed as a
// Record. It is never returned for a torn final line — see ReadResult.
var ErrMalformedRecord = errors.New("journal: malformed record")

// MalformedRecordError reports the 1-based line number of a journal line
// that failed to parse, and why. errors.Is matches it against
// ErrMalformedRecord; errors.As drills into the underlying parse error.
type MalformedRecordError struct {
	// Line is the 1-based line number of the record that failed to parse.
	Line int
	// Err is the underlying parse error.
	Err error
}

func (e *MalformedRecordError) Error() string {
	return fmt.Sprintf("journal: malformed record on line %d: %v", e.Line, e.Err)
}

// Unwrap exposes both ErrMalformedRecord (for errors.Is) and the underlying
// parse error (for errors.As), per the multi-error form errors.Is/As have
// supported since Go 1.20.
func (e *MalformedRecordError) Unwrap() []error { return []error{ErrMalformedRecord, e.Err} }

// ErrCorruptJournal indicates that a sequence of records does not form a
// valid journal history: Seq does not strictly increase from 1, or a node
// execution is not cleanly paired. errors.Is matches it against any
// corruption Validate or ResolveStart detects.
var ErrCorruptJournal = errors.New("journal: corrupt")

// CorruptionError explains the specific defect Validate or ResolveStart
// found. errors.Is matches it against ErrCorruptJournal.
type CorruptionError struct {
	// Seq is the sequence number of the record at which the defect was
	// detected.
	Seq uint64
	// Reason describes the defect in a form safe to show an operator.
	Reason string
}

func (e *CorruptionError) Error() string {
	return fmt.Sprintf("journal: corrupt at seq %d: %s", e.Seq, e.Reason)
}

// Unwrap exposes ErrCorruptJournal for errors.Is.
func (e *CorruptionError) Unwrap() []error { return []error{ErrCorruptJournal} }

func corrupt(seq uint64, format string, args ...any) error {
	return &CorruptionError{Seq: seq, Reason: fmt.Sprintf(format, args...)}
}

// Validate checks that records form a well-formed journal history:
//
//   - Seq strictly increases from 1 with no gap, repeat or inversion.
//   - Every node_started is closed by exactly one matching node_finished
//     (same node, same attempt) before any other record follows it.
//
// Validate never guesses at an ambiguous history: any deviation is reported
// as a *CorruptionError wrapping ErrCorruptJournal, never silently repaired
// or ignored.
func Validate(records []Record) error {
	_, _, err := scan(records)
	return err
}

// scan walks records once, validating structure, and returns the node
// execution left open at the end (if the history ends mid-node) and the
// last node_finished seen (if any). It is the shared core of Validate and
// ResolveStart.
func scan(records []Record) (open, lastFinished *Record, err error) {
	var prevSeq uint64
	for i := range records {
		rec := records[i]

		wantSeq := prevSeq + 1
		if rec.Seq != wantSeq {
			return nil, nil, corrupt(rec.Seq, "seq %d out of order: expected %d", rec.Seq, wantSeq)
		}
		prevSeq = rec.Seq

		switch rec.Event {
		case EventNodeStarted:
			if open != nil {
				return nil, nil, corrupt(rec.Seq,
					"node_started for %q attempt %d began while %q attempt %d was still open (no matching node_finished)",
					rec.Node, rec.Attempt, open.Node, open.Attempt)
			}
			r := rec
			open = &r
		case EventNodeFinished:
			if open == nil {
				return nil, nil, corrupt(rec.Seq, "node_finished for %q attempt %d has no matching node_started", rec.Node, rec.Attempt)
			}
			if open.Node != rec.Node || open.Attempt != rec.Attempt {
				return nil, nil, corrupt(rec.Seq,
					"node_finished for %q attempt %d does not match open node_started %q attempt %d",
					rec.Node, rec.Attempt, open.Node, open.Attempt)
			}
			r := rec
			lastFinished = &r
			open = nil
		default:
			if open != nil {
				return nil, nil, corrupt(rec.Seq,
					"%s appeared while %q attempt %d was still open (no matching node_finished)",
					rec.Event, open.Node, open.Attempt)
			}
		}
	}
	return open, lastFinished, nil
}

// RunState summarizes the overall status of a run as recorded in its
// journal.
type RunState int

const (
	// RunStateRunnable means nothing blocks starting or resuming: Node and
	// Attempt on the Result say where. This covers a brand-new run, a run
	// recovering from a node that died mid-execution, and a run in the
	// ordinary middle of its graph.
	RunStateRunnable RunState = iota
	// RunStatePaused means the run was explicitly paused. A plain `belay
	// run` must refuse; only `belay resume` may advance past this state.
	RunStatePaused
	// RunStateCompleted means the run finished successfully. Nothing
	// remains to resume.
	RunStateCompleted
	// RunStateFailed means the run ended in failure.
	RunStateFailed
	// RunStateAborted means the run was stopped before it could finish.
	RunStateAborted
)

var runStateNames = [...]string{
	RunStateRunnable:  "runnable",
	RunStatePaused:    "paused",
	RunStateCompleted: "completed",
	RunStateFailed:    "failed",
	RunStateAborted:   "aborted",
}

// String returns a lowercase name for s, such as "paused".
func (s RunState) String() string {
	if s < 0 || int(s) >= len(runStateNames) {
		return fmt.Sprintf("RunState(%d)", int(s))
	}
	return runStateNames[s]
}

var _ fmt.Stringer = RunStateRunnable

// Result is the resume decision computed by ResolveStart: where a run
// should start or resume next, and whether anything blocks that.
type Result struct {
	// State is the run's overall status. Callers must check it before
	// acting on Node and Attempt: for example, a plain `run` refuses when
	// State is RunStatePaused.
	State RunState
	// Node is the node to run next. It is always populated with belay's
	// best resolution of "where execution was", even when State blocks
	// acting on it, so a caller such as `belay timeline` can still show it.
	Node string
	// Attempt is the 1-based attempt number to use for Node.
	Attempt int
}

// ResolveStart computes the resume decision for a run from its journal
// records, given in file order (as returned by Read or ReadFile):
//
//   - No records at all (nil, empty, or a journal file that does not exist):
//     Result{State: RunStateRunnable, Node: FirstNode, Attempt: 1}.
//   - The most recent node_finished names its successor in Next: resume
//     there, at attempt 1.
//   - The most recent node_started has no matching node_finished: that node
//     died mid-execution. Re-run it at attempt+1.
//   - The journal's last record is a run-lifecycle marker (run_paused,
//     run_completed, run_failed or run_aborted): State reports it, while
//     Node and Attempt still carry the position resolved from the records
//     before it, for display.
//
// ResolveStart never guesses at an ambiguous or broken history: any seq or
// pairing defect is reported as a *CorruptionError wrapping
// ErrCorruptJournal, with Result the zero value.
func ResolveStart(records []Record) (Result, error) {
	open, lastFinished, err := scan(records)
	if err != nil {
		return Result{}, err
	}

	if open != nil {
		return Result{State: RunStateRunnable, Node: open.Node, Attempt: open.Attempt + 1}, nil
	}

	node, attempt := FirstNode, 1
	if lastFinished != nil {
		node, attempt = lastFinished.Next, 1
	}

	if len(records) == 0 {
		return Result{State: RunStateRunnable, Node: node, Attempt: attempt}, nil
	}

	switch records[len(records)-1].Event {
	case EventRunPaused:
		return Result{State: RunStatePaused, Node: node, Attempt: attempt}, nil
	case EventRunCompleted:
		return Result{State: RunStateCompleted, Node: node, Attempt: attempt}, nil
	case EventRunFailed:
		return Result{State: RunStateFailed, Node: node, Attempt: attempt}, nil
	case EventRunAborted:
		return Result{State: RunStateAborted, Node: node, Attempt: attempt}, nil
	default:
		return Result{State: RunStateRunnable, Node: node, Attempt: attempt}, nil
	}
}

// NodeStats summarizes every recorded execution of one node across a run.
// "Execution" counts node_finished records, so it counts loop iterations
// (test -> fix -> test is two executions of test) rather than crash-retry
// attempts of a single visit.
type NodeStats struct {
	// Node is the node name.
	Node string
	// Executions is the number of node_finished records for Node.
	Executions int
	// TotalDuration sums DurationMS across every execution of Node.
	TotalDuration time.Duration
	// ByStatus counts executions of Node by their outcome.
	ByStatus map[Status]int
}

// Stats summarizes records per node, in the order each node first appears.
// It considers only node_finished records; it does not validate records and
// does not fail on a malformed or corrupt history — it reports whatever it
// can, on the theory that a timeline view should degrade gracefully rather
// than go blank because part of the journal is unreadable as a strict
// history. Callers that need a hard correctness guarantee should call
// Validate (or ResolveStart) first.
func Stats(records []Record) []NodeStats {
	order := make([]string, 0)
	byNode := make(map[string]*NodeStats)

	for _, rec := range records {
		if rec.Event != EventNodeFinished {
			continue
		}
		s, ok := byNode[rec.Node]
		if !ok {
			s = &NodeStats{Node: rec.Node, ByStatus: make(map[Status]int)}
			byNode[rec.Node] = s
			order = append(order, rec.Node)
		}
		s.Executions++
		s.TotalDuration += time.Duration(rec.DurationMS) * time.Millisecond
		s.ByStatus[rec.Status]++
	}

	out := make([]NodeStats, len(order))
	for i, node := range order {
		out[i] = *byNode[node]
	}
	return out
}
