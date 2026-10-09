//go:build unix

// Package procgroup records the process groups belay starts, so that a belay
// killed with SIGKILL can find and kill them when its run is resumed.
//
// # Why this exists
//
// internal/exec starts every process belay runs (the coding agent, the test
// suite, a linter) as the leader of a process group of its own, and kills that
// group when the process's context is cancelled or its deadline passes. That
// holds only while belay is alive to do it. A belay killed with SIGKILL, or
// quit by a second Ctrl-C, dies with those groups still running. They are not
// in the terminal's foreground group, so the terminal's SIGINT never reaches
// them, and the kernel hands them to init. An agent CLI left that way keeps
// spending, with nobody watching it and no budget ledger counting.
//
// So every process belay starts for a node is recorded on disk until belay has
// reaped it. When the dispatcher heals a run whose last attempt a crash left
// open, it calls Reap, which kills every group still recorded.
//
// # Where the records are kept
//
// Not in the run's directory. A record decides which process a resume sends
// SIGKILL, and in a run that edits its workspace in place, the run's
// directory is inside that workspace, where the agent works: a record it
// wrote there, naming a process of its choosing with that process's real
// start time (anyone can read it from ps), would have belay kill it. So the
// records for a node execution are kept under belay's own state directory
// ($XDG_STATE_HOME/belay, or ~/.local/state/belay), in a directory named by a
// digest of the run's evidence directory (RecordDir): one per run and node
// execution, found again from the same run on any resume. The callers still
// name the evidence directory; only this package knows where its records go.
//
// That keeps them away from anything confined to the workspace, which is
// what an agent's file tools are. It is not a wall against code running as
// the person, which can write belay's state directory as easily as their
// home: the checks below are the second line, and are argued there. A
// record found in the run's directory -- written there by an earlier build of
// belay, or by anything else -- is reported and removed, and never acted on.
// Without HOME or XDG_STATE_HOME belay has nowhere private to keep them,
// Track fails, and internal/exec refuses to start the process, as it refuses
// any process it could not stop after a crash.
//
// # When the record is written
//
// A reservation naming the command is written before the process starts, and
// the group is filled in as soon as it has started, before belay waits on it.
// The group cannot be written any earlier: its id is the new process's pid,
// and the kernel only assigns that at fork. A belay killed between the fork
// and the second write leaves a reservation with no pid in it. Reap reports
// it and does nothing else, because it names a command that may still be
// running but no process belay can prove is the one it started.
//
// The writes are not fsynced. The threat is belay being killed, and what a
// killed process wrote is already in the kernel's page cache, which outlives
// it. Only a crash of the whole machine loses those writes, and that ends the
// recorded processes too.
//
// # Never signal a process belay did not start
//
// A pid is handed out again once its process has exited and been reaped.
// Between belay's death and the resume that reaps, a recorded process may
// have ended, and its pid, which is also its group id, may now belong to
// something unrelated. Killing that would be far worse than the leak this
// package exists to close. So Reap kills only a process that still matches its
// record in three ways, each one the platform reports:
//
//   - its start time is the recorded one;
//   - it leads its own process group. internal/exec starts every process
//     with Setpgid, and a start fails when the kernel refuses it, so every
//     process belay started leads a group, and a record saying otherwise is
//     not one belay wrote;
//   - its program is the recorded one: the first word of its command line,
//     by base name, is argv[0]'s. Not the whole argv, on purpose. A program
//     may rewrite the arguments the platform reports, and Claude Code does
//     (process.title = "claude"), so a match on every argument would stop
//     belay ever reaping an orphaned agent, the case this package exists for.
//     What a program renaming itself keeps is its name.
//
// A record it cannot check (one with no pid, no start time, or damaged) is
// reported and removed, never acted on. None of the three stops a forger who
// can read ps and write a record: what stops that is where records are kept.
// They stop the accidents a pid's reuse could otherwise line up.
//
// On Linux the start time is the starttime field of /proc/<pid>/stat, in clock
// ticks since boot, qualified by the boot id so that a reboot cannot make two
// processes agree. Elsewhere it is what `ps -o lstart=` prints in UTC, which
// has one-second resolution. That is enough: a pid is reused within the same
// second only if every other pid was handed out in between, because Linux and
// macOS both allocate pids in sequence, and neither can start that many
// processes in a second. Reading the start time through sysctl on macOS would
// give microseconds without a subprocess, but the standard library cannot
// address kern.proc.pid.<pid>, and golang.org/x/sys is not a dependency.
//
// # Limits
//
//   - A group whose leader has exited is not chased, even if others in it live
//     on: with the leader gone, nothing proves the group is still the one belay
//     started. internal/exec makes the same choice for a child that exits
//     cleanly and leaves descendants behind.
//   - Only the processes belay starts are recorded. One that the agent moves
//     into a session of its own is outside every group belay knows about.
//
// # Why a context value
//
// The directory reaches internal/exec on the context (WithDir), not in a
// field. Every process belay starts goes through internal/exec, but by way of
// an adapter (the agent backend, a test runner, a linter) behind interfaces
// that pkg/belay promises to keep stable. A field would have to be threaded
// through all of them, and an adapter written later that forgot it would
// reopen the leak without a word. On the context, the dispatcher sets it once
// per node execution, and every process the node starts is recorded.
package procgroup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// recordPrefix and recordGlob name a record file inside a node's evidence
// directory.
const (
	recordPrefix = "pgroup-"
	recordGlob   = recordPrefix + "*.json"
)

// errNoProcess reports that no process holds a pid. startTime returns it, so
// that Reap can tell a process that is gone from one it could not check.
var errNoProcess = errors.New("procgroup: no such process")

// dirKey is the context key WithDir stores the directory under.
type dirKey struct{}

// WithDir returns a copy of ctx that tells internal/exec to record, in dir,
// every process it starts under that context.
func WithDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, dirKey{}, dir)
}

// DirFrom returns the directory WithDir put in ctx, or "" when there is none,
// in which case nothing is recorded.
func DirFrom(ctx context.Context) string {
	dir, ok := ctx.Value(dirKey{}).(string)
	if !ok {
		return ""
	}
	return dir
}

// Record is one process belay started, as it is written to disk.
type Record struct {
	// PID is the process belay started, or 0 in a reservation written before
	// it started.
	PID int `json:"pid"`
	// PGID is the process group PID belongs to. It equals PID when the
	// process leads its own group, as internal/exec asks every process to.
	PGID int `json:"pgid"`
	// Start is the process's start time as the platform reports it. Reap
	// compares it, as an opaque string, with what the process holding PID
	// reports then.
	Start string `json:"start,omitempty"`
	// Argv is the command, already redacted by internal/exec, so that a
	// report can say what was left running.
	Argv []string `json:"argv"`
}

// RecordDir returns the directory belay keeps the records of the processes
// started for one node execution in, given that execution's evidence
// directory: under belay's state directory, never inside the run's own
// directory. See "Where the records are kept".
func RecordDir(evidence string) (string, error) {
	run, err := runRecordDir(filepath.Dir(evidence))
	if err != nil {
		return "", err
	}
	return filepath.Join(run, filepath.Base(evidence)), nil
}

// runRecordDir returns the directory that holds one record directory per
// node execution of the run whose nodes/ directory is nodes. Its name is a
// digest of that path with every symbolic link in it resolved, so a resume
// that names the workspace another way -- /var for /private/var -- finds
// the same records.
func runRecordDir(nodes string) (string, error) {
	state, err := stateDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical(nodes)))
	return filepath.Join(state, "procgroups", hex.EncodeToString(sum[:16])), nil
}

// stateDir is belay's state directory: $XDG_STATE_HOME/belay, or
// $HOME/.local/state/belay where the XDG base directory specification puts
// it when XDG_STATE_HOME is unset. A relative value of either is ignored, as
// that specification asks.
func stateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "belay"), nil
	}
	if home := os.Getenv("HOME"); filepath.IsAbs(home) {
		return filepath.Join(home, ".local", "state", "belay"), nil
	}
	return "", errors.New("procgroup: neither XDG_STATE_HOME nor HOME names a directory to keep process records in")
}

// canonical returns path with every symbolic link in the part of it that
// exists resolved, and the rest as written.
func canonical(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(canonical(parent), filepath.Base(path))
}

// Tracked is the record of one process from before it starts until belay has
// reaped it. Track makes one.
type Tracked struct {
	path string
	rec  Record
}

// Track writes a reservation for a process about to start with argv, for
// the node execution whose evidence directory is evidence, in that
// execution's RecordDir, which it creates if need be. Call Started once the
// process has started, and Done once it has been reaped.
//
// internal/exec refuses to start a process Track could not record, because
// belay could not stop that process after a crash.
func Track(evidence string, argv []string) (*Tracked, error) {
	dir, err := RecordDir(evidence)
	if err != nil {
		return nil, err
	}
	f, err := createRecord(dir)
	if err != nil {
		return nil, err
	}
	t := &Tracked{path: f.Name(), rec: Record{Argv: slices.Clone(argv)}}
	data, err := json.Marshal(t.rec)
	if err == nil {
		_, err = f.Write(data)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("procgroup: write %s: %w", t.path, err), t.Done())
	}
	return t, nil
}

// createRecord creates an empty record file in dir, making dir first. It
// tries twice, because Done removes a record directory it has emptied, and
// can do so between this making the directory and creating the file in it.
func createRecord(dir string) (*os.File, error) {
	var err error
	for range 2 {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("procgroup: create %s: %w", dir, err)
		}
		var f *os.File
		if f, err = os.CreateTemp(dir, recordPrefix+"*.json"); err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			break
		}
	}
	return nil, fmt.Errorf("procgroup: create record in %s: %w", dir, err)
}

// Started fills in the process's group and start time, replacing the
// reservation.
//
// It writes whatever it learned even when it returns an error: a record with
// a pid and no start time still lets a report name the process, although Reap
// will not act on it.
func (t *Tracked) Started(pid int) error {
	t.rec.PID = pid
	var errs []error
	if pgid, err := syscall.Getpgid(pid); err != nil {
		errs = append(errs, fmt.Errorf("procgroup: process group of %d: %w", pid, err))
	} else {
		t.rec.PGID = pgid
	}
	if start, err := startTime(pid); err != nil {
		errs = append(errs, fmt.Errorf("procgroup: start time of %d: %w", pid, err))
	} else {
		t.rec.Start = start
	}
	if err := t.write(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// write replaces the record through a rename, so that a belay killed part-way
// leaves the previous version whole rather than a torn one.
func (t *Tracked) write() error {
	data, err := json.Marshal(t.rec)
	if err != nil {
		return fmt.Errorf("procgroup: encode record: %w", err)
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("procgroup: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, t.path); err != nil {
		return errors.Join(fmt.Errorf("procgroup: replace %s: %w", t.path, err), removeIfPresent(tmp))
	}
	return nil
}

// Done removes the record. Call it once the process has been reaped, and not
// before: the record is what finds the group if belay dies first.
//
// It also removes the record directory, and the run's, once they are empty,
// so belay's state directory does not fill with one empty directory per step
// of every run there has ever been.
func (t *Tracked) Done() error {
	if err := removeIfPresent(t.path); err != nil {
		return err
	}
	removeIfEmpty(filepath.Dir(t.path))
	removeIfEmpty(filepath.Dir(filepath.Dir(t.path)))
	return nil
}

// removeIfEmpty removes dir if nothing is in it. A directory that is not
// empty, or already gone, is left as it is: tidying up is never a failure.
func removeIfEmpty(dir string) {
	_ = os.Remove(dir)
}

// Outcome is what Reap did about one record.
type Outcome int

// The outcomes of reaping one record.
const (
	// OutcomeKilled means the recorded process was still the one belay
	// started, and its group, or the process alone if it did not lead one,
	// was sent SIGKILL.
	OutcomeKilled Outcome = iota + 1
	// OutcomeGone means no process holds the recorded pid any more.
	OutcomeGone
	// OutcomeRecycled means a process holds the recorded pid but is not the
	// one belay started -- it started at another time, does not lead its own
	// process group, or runs another program -- and was not signalled.
	OutcomeRecycled
	// OutcomeUnverifiable means the record names no process belay can check
	// (a reservation, one with no start time, or a damaged file), or the check
	// or the kill failed; Reaped.Err says which. Nothing was signalled.
	OutcomeUnverifiable
)

// String names o for a log line.
func (o Outcome) String() string {
	switch o {
	case OutcomeKilled:
		return "killed"
	case OutcomeGone:
		return "gone"
	case OutcomeRecycled:
		return "pid reused by another process"
	case OutcomeUnverifiable:
		return "unverifiable"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// Reaped is Reap's report on one record.
type Reaped struct {
	// Path is the record file.
	Path string
	// Record is what the file held, as far as it could be read.
	Record Record
	// Outcome is what Reap did.
	Outcome Outcome
	// Err says why a record was unverifiable, or why its file could not be
	// removed.
	Err error
}

// Reap kills every process group recorded for the run whose nodes/ directory
// is root, whose leader is still the process belay started, and removes every
// record it finds, whatever it did about it: a record left behind would be
// acted on again by the next resume. The records are the ones Track kept for
// the node executions under root (see RecordDir).
//
// A record in one of root's own evidence directories, where whatever runs in
// the workspace can write, is reported as unverifiable and removed, and never
// acted on.
//
// It reports what it did rather than failing. Reaping is part of healing a
// run, and a record belay cannot act on is no reason to refuse to continue
// the run; the caller logs the report.
func Reap(root string) []Reaped {
	var out []Reaped
	dir, dirErr := runRecordDir(root)
	where := "belay's state directory"
	if dirErr == nil {
		where = dir
	}
	// The patterns are constants, and Glob's only error is ErrBadPattern.
	found, _ := filepath.Glob(filepath.Join(root, "*", recordGlob))
	for _, path := range found {
		r := read(path)
		r.Outcome = OutcomeUnverifiable
		r.Err = errors.Join(r.Err, fmt.Errorf("procgroup: %s is in the run's own directory, where whatever "+
			"runs in the workspace can write; belay keeps the records it acts on in %s", path, where))
		out = append(out, spend(r))
	}
	if dirErr != nil {
		return append(out, Reaped{Path: root, Outcome: OutcomeUnverifiable, Err: dirErr})
	}
	kept, _ := filepath.Glob(filepath.Join(dir, "*", recordGlob))
	for _, path := range kept {
		out = append(out, spend(reapOne(path)))
		removeIfEmpty(filepath.Dir(path))
	}
	removeIfEmpty(dir)
	return out
}

// spend removes the record r is about, adding a failure to remove it to r.
func spend(r Reaped) Reaped {
	if err := removeIfPresent(r.Path); err != nil {
		r.Err = errors.Join(r.Err, err)
	}
	return r
}

// read reads the record at path, as far as it can be read, into a Reaped
// that is unverifiable until shown otherwise.
func read(path string) Reaped {
	r := Reaped{Path: path, Outcome: OutcomeUnverifiable}
	data, err := os.ReadFile(path) //nolint:gosec // path came from a glob of a directory belay named itself
	if err != nil {
		r.Err = fmt.Errorf("procgroup: read %s: %w", path, err)
		return r
	}
	if err := json.Unmarshal(data, &r.Record); err != nil {
		r.Err = fmt.Errorf("procgroup: parse %s: %w", path, err)
	}
	return r
}

// reapOne acts on the record at path, and says what it did. It kills only a
// process that matches its record in all three ways the package doc gives:
// start time, leading its own group, and program.
func reapOne(path string) Reaped {
	r := read(path)
	if r.Err != nil {
		return r
	}
	rec := r.Record
	switch {
	case rec.PID <= 0 || rec.Start == "":
		r.Err = fmt.Errorf("procgroup: %s names no process whose start time was recorded", path)
		return r
	case rec.PGID != rec.PID:
		r.Err = fmt.Errorf("procgroup: %s names process %d in group %d, and every process belay starts "+
			"leads its own group", path, rec.PID, rec.PGID)
		return r
	}

	now, err := startTime(rec.PID)
	switch {
	case errors.Is(err, errNoProcess):
		r.Outcome = OutcomeGone
		return r
	case err != nil:
		r.Err = fmt.Errorf("procgroup: start time of %d: %w", rec.PID, err)
		return r
	case now != rec.Start:
		r.Outcome = OutcomeRecycled
		return r
	}

	switch pgid, err := syscall.Getpgid(rec.PID); {
	case errors.Is(err, syscall.ESRCH):
		r.Outcome = OutcomeGone
		return r
	case err != nil:
		r.Err = fmt.Errorf("procgroup: process group of %d: %w", rec.PID, err)
		return r
	case pgid != rec.PID:
		r.Outcome = OutcomeRecycled
		return r
	}

	line, err := commandLine(rec.PID)
	switch {
	case errors.Is(err, errNoProcess):
		r.Outcome = OutcomeGone
		return r
	case err != nil:
		r.Err = fmt.Errorf("procgroup: command line of %d: %w", rec.PID, err)
		return r
	case !sameProgram(rec.Argv, line):
		r.Outcome = OutcomeRecycled
		return r
	}

	// The negative pid signals the whole group the process leads.
	switch err := syscall.Kill(-rec.PID, syscall.SIGKILL); {
	case errors.Is(err, syscall.ESRCH):
		r.Outcome = OutcomeGone
	case err != nil:
		r.Err = fmt.Errorf("procgroup: kill group %d: %w", rec.PID, err)
	default:
		r.Outcome = OutcomeKilled
	}
	return r
}

// sameProgram reports whether line, a process's command line as the platform
// reports it now, runs the program argv started: it begins with argv[0]
// itself, or its first word has argv[0]'s base name, which is what a program
// that rewrites its own arguments keeps (see the package doc).
func sameProgram(argv []string, line string) bool {
	if len(argv) == 0 || argv[0] == "" {
		return false
	}
	line = strings.TrimSpace(line)
	if line == argv[0] || strings.HasPrefix(line, argv[0]+" ") {
		return true
	}
	first, _, _ := strings.Cut(line, " ")
	return filepath.Base(first) == filepath.Base(argv[0])
}

// removeIfPresent removes path, treating a file that is already gone as
// removed.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("procgroup: remove %s: %w", path, err)
	}
	return nil
}
