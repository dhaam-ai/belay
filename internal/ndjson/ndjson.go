// Package ndjson holds the byte-level discipline behind belay's append-only
// newline-delimited JSON files: write one line, fsync, never rewrite; read
// back tolerating exactly the one damaged shape an interrupted append can
// produce, and heal it before appending again.
//
// # Why it is its own package
//
// ADR-0003 made that discipline the run journal's, and it is the reason a
// crashed run resumes instead of restarting. The goal queue needs the same
// promise for the same reason — a list of work a reboot must not lose or
// double — and the two files have nothing else in common: different records,
// different sequencing, different arbitration between processes. Left in
// internal/journal, the second caller would have had to import a package
// about run history to borrow four file operations, or, far more likely,
// write its own. Two implementations of "the crash-safe append" is how one
// of them quietly stops fsyncing.
//
// So the durability lives here, alone, and each caller keeps its own record
// type, its own validation and its own locking.
//
// # What this package deliberately does not do
//
//   - It does not arbitrate between processes. There is no flock and no
//     lease: a caller whose file is shared by two processes brings its own.
//     internal/journal assumes one dispatcher owns a run; internal/queue
//     locks.
//   - It does not know any record type, assign any sequence number, or parse
//     anything. Scan hands out raw lines and reports where a caller's own
//     decoder said no.
//   - It does not buffer. Every AppendLine reaches the disk before it
//     returns, which is the whole point and also why it is not a fast way to
//     write a million lines.
//
// # The one damaged shape
//
// Because every write only ever adds bytes past the end of the file, a crash
// during one append can damage at most the line being written, never an
// earlier one. Scan therefore treats a final line its caller cannot decode
// as an incomplete write and says so (incomplete), while the same failure
// anywhere else is corruption and is returned as a *LineError naming the
// line. Healing that tail before the next append is the caller's job, with
// RewriteClean; appending onto a torn line would merge two records into one
// permanently unreadable one.
package ndjson

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MaxLineBytes bounds how large a single line may be before Scan gives up on
// it. It is generous enough for a long note while still making a runaway or
// corrupted line fail fast instead of exhausting memory.
const MaxLineBytes = 8 * 1024 * 1024

// LineError reports the 1-based line number at which a caller's decoder
// rejected a line, and why. errors.As drills into the underlying error.
//
// Scan returns one only for a line it knows is not the last, so a LineError
// always means real corruption rather than an interrupted write.
type LineError struct {
	// Line is the 1-based line number of the line that was rejected.
	Line int
	// Err is the error the caller's own decoder returned.
	Err error
}

func (e *LineError) Error() string {
	return fmt.Sprintf("ndjson: line %d: %v", e.Line, e.Err)
}

// Unwrap exposes the decoder's error, so errors.As reaches it.
func (e *LineError) Unwrap() error { return e.Err }

// Scan streams newline-delimited lines from r, handing each to each.
//
// each receives the line's bytes without their delimiter, and must copy
// anything it keeps: the buffer is reused on the next line. Returning an
// error from each means "this line does not decode", which Scan interprets
// by position, not by cause — see the package doc:
//
//   - A rejected line with another line after it is corruption, and Scan
//     returns a *LineError naming it. Everything decoded before it is still
//     reported to each, and the caller keeps whatever it accumulated.
//   - A rejected FINAL line is an interrupted append. Scan returns
//     incomplete = true and a nil error.
//
// Scan holds at most one line in memory at a time (bounded by MaxLineBytes),
// so a very large file costs O(1) scratch beyond what each retains. A line
// over that bound is returned as an error wrapping bufio.ErrTooLong.
func Scan(r io.Reader, each func(line []byte) error) (incomplete bool, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)

	var (
		line       int
		pendingErr error // set when the immediately preceding line was rejected
	)

	for sc.Scan() {
		line++
		if pendingErr != nil {
			// Another line exists after the one that was rejected, so that
			// rejection was not an interrupted final write — it is corruption.
			return false, &LineError{Line: line - 1, Err: pendingErr}
		}
		if err := each(sc.Bytes()); err != nil {
			pendingErr = err
			continue
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("ndjson: scan: %w", err)
	}
	return pendingErr != nil, nil
}

// AppendLine writes line to f, terminated by a newline, and fsyncs before
// returning.
//
// f must have been opened with O_APPEND, which is what makes the write land
// past the current end of file whatever else has been written since. The
// fsync is what makes "belay recorded it" mean the disk has it, not the page
// cache: without it, a power loss takes back records a caller has already
// acted on.
//
// A failure leaves whatever bytes reached the file in place. That is the
// torn tail Scan tolerates and RewriteClean heals, and it is why callers
// must not retry the same line onto the same descriptor.
func AppendLine(f *os.File, line []byte) error {
	data := make([]byte, 0, len(line)+1)
	data = append(data, line...)
	data = append(data, '\n')

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("ndjson: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("ndjson: sync: %w", err)
	}
	return nil
}

// RewriteClean atomically replaces the file at path with exactly lines, each
// on its own newline-terminated line, discarding anything else — a torn
// final line left by an interrupted append.
//
// It writes a temporary file in the same directory and renames it into
// place, so a second crash during the recovery itself still leaves path
// either fully in its old (torn) state or fully in its new (clean) state,
// never something in between.
func RewriteClean(path string, lines [][]byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".recover-*")
	if err != nil {
		return fmt.Errorf("create recovery file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once the rename below succeeds

	for i, line := range lines {
		data := make([]byte, 0, len(line)+1)
		data = append(data, line...)
		data = append(data, '\n')
		if _, err := tmp.Write(data); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("write line %d during recovery: %w", i+1, err)
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
		return fmt.Errorf("install recovered file: %w", err)
	}
	return nil
}

// EnsureTrailingNewline appends a single newline to path if the file is
// non-empty and does not already end in one. A missing file is not an error.
//
// This closes the one gap Scan tolerates without reporting it: a final line
// whose content is complete but whose trailing delimiter byte itself was the
// one cut off. Scan correctly counts that line as durable — its content is
// intact — but the file on disk needs its delimiter completed before another
// line can be appended after it without the two merging into one
// unparseable line.
func EnsureTrailingNewline(path string) error {
	// #nosec G304 -- path is the file the caller named; this package has no
	// policy opinion about where a caller's file may live.
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
