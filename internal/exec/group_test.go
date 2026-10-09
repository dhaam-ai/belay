//go:build unix

package exec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/procgroup"
)

// recordsOf points procgroup's state directory at a temporary one for the
// test, and returns where it keeps the records of the processes started for
// the node execution whose evidence directory is evidence.
func recordsOf(t *testing.T, evidence string) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir, err := procgroup.RecordDir(evidence)
	if err != nil {
		t.Fatalf("RecordDir(%s): %v", evidence, err)
	}
	return dir
}

// groupRecords returns the process-group records in dir, parsed. A record
// being rewritten at that instant is skipped rather than failed on; the
// callers poll.
func groupRecords(t *testing.T, dir string) []procgroup.Record {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "pgroup-*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	var out []procgroup.Record
	for _, p := range paths {
		data, err := os.ReadFile(p) //nolint:gosec // p is a file the test's own glob found under its temp dir
		if err != nil {
			continue
		}
		var r procgroup.Record
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// dirEntries lists dir, treating a directory that does not exist as empty.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(des))
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

// TestRunRecordsTheProcessBeforeItStarts: with a directory on the context,
// the record is on disk before the process runs its first instruction, so
// there is no moment in which a belay killed with SIGKILL leaves a process
// nobody wrote down. The child lists the directory as it starts, and must
// find itself there. Once Run has returned the record is gone: the process
// was reaped, and there is nothing left to chase.
func TestRunRecordsTheProcessBeforeItStarts(t *testing.T) {
	evidence := filepath.Join(t.TempDir(), "nodes", "004-test")
	dir := recordsOf(t, evidence)

	res, err := quietRunner().Run(procgroup.WithDir(t.Context(), evidence), Command{
		Path:     testExe(t),
		Args:     []string{dir},
		ExtraEnv: helperEnvFor("ls", ""),
	})
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "entry:pgroup-") {
		t.Errorf("the child found no record of itself in %s as it started; it saw %q", dir, res.Stdout)
	}
	if left := dirEntries(t, dir); len(left) != 0 {
		t.Errorf("after Run returned, %s still holds %v", dir, left)
	}
}

// TestRunRecordsTheRunningProcessGroup: while the process runs, its record
// names the group it leads and the start time procgroup.Reap will check, so
// a resume after a kill -9 can find it and prove it is the one belay started.
func TestRunRecordsTheRunningProcessGroup(t *testing.T) {
	evidence := filepath.Join(t.TempDir(), "nodes", "004-test")
	dir := recordsOf(t, evidence)
	ctx, cancel := context.WithCancel(procgroup.WithDir(t.Context(), evidence))
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := quietRunner().Run(ctx, Command{
			Path:     testExe(t),
			ExtraEnv: helperEnvFor("stall", ""),
			Timeout:  30 * time.Second,
			Grace:    200 * time.Millisecond,
		})
		done <- err
	}()

	var rec procgroup.Record
	for deadline := time.Now().Add(10 * time.Second); rec.PID == 0; {
		if time.Now().After(deadline) {
			t.Fatalf("no record naming a process appeared in %s; it holds %v", dir, dirEntries(t, dir))
		}
		for _, r := range groupRecords(t, dir) {
			if r.PID > 0 {
				rec = r
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if rec.PGID != rec.PID {
		t.Errorf("record %+v: the process does not lead the group recorded for it", rec)
	}
	if pgid, err := syscall.Getpgid(rec.PID); err != nil || pgid != rec.PID {
		t.Errorf("record names pid %d, whose group is %d (err %v); want a live group leader", rec.PID, pgid, err)
	}
	if rec.Start == "" {
		t.Errorf("record %+v has no start time: procgroup.Reap could never act on it", rec)
	}
	if len(rec.Argv) == 0 || rec.Argv[0] != testExe(t) {
		t.Errorf("record argv = %q, want the command that was run", rec.Argv)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want context.Canceled", err)
	}
	if left := dirEntries(t, dir); len(left) != 0 {
		t.Errorf("the record outlived the process: %s holds %v", dir, left)
	}
}

// TestRunRefusesAProcessItCannotRecord: a process belay could not record is
// one it could not stop after a crash, so it is not started at all.
func TestRunRefusesAProcessItCannotRecord(t *testing.T) {
	// belay's state directory, where the records go, is under a file, so
	// no record can be written.
	blocker := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", blocker)
	pidDir := t.TempDir()

	_, err := quietRunner().Run(procgroup.WithDir(t.Context(), filepath.Join(t.TempDir(), "nodes", "004-test")), Command{
		Path:     testExe(t),
		ExtraEnv: helperEnvFor("args", pidDir),
	})

	var se *StartError
	if !errors.As(err, &se) {
		t.Errorf("Run = %v (%T), want a *StartError", err, err)
	}
	if n := countPIDFiles(t, pidDir); n != 0 {
		t.Errorf("%d helper process(es) ran; a process belay could not record must never start", n)
	}
}

// TestRunWithoutADirectoryRecordsNothing is a guard: a caller that never put
// a directory on the context, which is every caller outside a node
// execution, sees Run behave exactly as before.
func TestRunWithoutADirectoryRecordsNothing(t *testing.T) {
	if procgroup.DirFrom(t.Context()) != "" {
		t.Fatal("the test context already carries a directory")
	}
	wd := t.TempDir()
	res, err := quietRunner().Run(t.Context(), Command{
		Path:     testExe(t),
		Args:     []string{wd},
		Dir:      wd,
		ExtraEnv: helperEnvFor("ls", ""),
	})
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, res.Stderr)
	}
	if left := dirEntries(t, wd); len(left) != 0 {
		t.Errorf("Run wrote %v into the working directory with no record directory on the context", left)
	}
}
