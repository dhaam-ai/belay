//go:build unix

package procgroup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// noSuchPID is a process id no system allocates: above Linux's largest
// pid_max (2^22) and macOS's PID_MAX (99999), so no process can hold it.
const noSuchPID = 1<<31 - 1

// child is a real process a test started in its own process group, the way
// internal/exec starts every process belay runs.
type child struct {
	cmd    *osexec.Cmd
	exited chan struct{}
	err    error
}

func (c *child) pid() int { return c.cmd.Process.Pid }

// startSleeper starts `sleep 60` as the leader of a new process group and
// kills the group when the test ends, whatever the test did to it.
func startSleeper(t *testing.T) *child {
	t.Helper()
	return startChild(t, osexec.Command("sleep", "60"), true)
}

// startChild starts cmd, as the leader of a new process group when leads is
// set, and kills it when the test ends, whatever the test did to it.
func startChild(t *testing.T, cmd *osexec.Cmd, leads bool) *child {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: leads}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", cmd.Args, err)
	}
	c := &child{cmd: cmd, exited: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.exited)
	}()
	t.Cleanup(func() {
		target := c.pid()
		if leads {
			target = -target
		}
		if err := syscall.Kill(target, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("clean up process %d: %v", target, err)
		}
		<-c.exited
	})
	return c
}

// startFollower starts `sleep 60` in this test's own process group, so it
// does not lead a group of its own.
func startFollower(t *testing.T) *child {
	t.Helper()
	return startChild(t, osexec.Command("sleep", "60"), false)
}

// retitleScript renames the perl process running it to "perl", dropping its
// arguments from what the platform reports, as a Node program setting
// process.title does, and then sleeps.
const retitleScript = `$0 = "perl"; sleep 60`

// startRetitled starts a process that rewrites its own arguments once it is
// running, and waits until the platform reports the new ones.
func startRetitled(t *testing.T) *child {
	t.Helper()
	c := startChild(t, osexec.Command("/usr/bin/perl", "-e", retitleScript), true)
	for deadline := time.Now().Add(5 * time.Second); ; {
		// macOS's ps prints what was left of the environment after the
		// new title, so what is checked is that the arguments are gone.
		if line, err := commandLine(c.pid()); err == nil && strings.HasPrefix(line, "perl") &&
			!strings.Contains(line, retitleScript) {
			return c
		} else if time.Now().After(deadline) {
			t.Fatalf("perl never renamed itself: the platform reports %q (%v)", line, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killed reports whether c exited within a few seconds, and was ended by
// SIGKILL when it did.
func (c *child) killed(t *testing.T) bool {
	t.Helper()
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
		return false
	}
	ws, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// alive reports whether c is still running a moment later: long enough for
// a signal Reap had sent to have landed.
func (c *child) alive() bool {
	select {
	case <-c.exited:
		return false
	case <-time.After(300 * time.Millisecond):
		return true
	}
}

func mustStartTime(t *testing.T, pid int) string {
	t.Helper()
	s, err := startTime(pid)
	if err != nil {
		t.Fatalf("startTime(%d): %v", pid, err)
	}
	return s
}

func encode(t *testing.T, r Record) []byte {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal %+v: %v", r, err)
	}
	return data
}

// records returns the group records in dir, parsed.
func records(t *testing.T, dir string) []Record {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, recordGlob))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	out := make([]Record, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p) //nolint:gosec // p is a file this test's own glob found under its temp dir
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var r Record
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatalf("parse %s: %v (%q)", p, err, data)
		}
		out = append(out, r)
	}
	return out
}

// privateState points belay's state directory at a temporary one for the
// test, so a test never writes into the home directory of whoever runs it.
func privateState(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	return state
}

// recordDir is where belay keeps the records of the processes started for
// the node execution whose evidence directory is evidence.
func recordDir(t *testing.T, evidence string) string {
	t.Helper()
	dir, err := RecordDir(evidence)
	if err != nil {
		t.Fatalf("RecordDir(%s): %v", evidence, err)
	}
	return dir
}

func TestWithDir(t *testing.T) {
	const dir = "/work/.belay/runs/r/nodes/004-test"
	if got := DirFrom(WithDir(context.Background(), dir)); got != dir {
		t.Errorf("DirFrom(WithDir(ctx, %q)) = %q", dir, got)
	}
	if got := DirFrom(context.Background()); got != "" {
		t.Errorf("DirFrom(a context WithDir never touched) = %q, want empty", got)
	}
}

// TestStartTime proves the half of the recycled-pid check that the platform
// supplies: a start time belongs to one process, it does not change while
// that process lives, and a pid with no process has none.
func TestStartTime(t *testing.T) {
	self := mustStartTime(t, os.Getpid())
	if self == "" {
		t.Fatal("startTime(this process) is empty")
	}
	if again := mustStartTime(t, os.Getpid()); again != self {
		t.Errorf("startTime changed while the process lived: %q, then %q", self, again)
	}
	// pid 1 started when the machine or container did, long before this
	// test: a start time that cannot tell the two apart tells nothing apart.
	if first := mustStartTime(t, 1); first == self {
		t.Errorf("startTime(1) = startTime(this process) = %q", self)
	}
	if got, err := startTime(noSuchPID); err == nil {
		t.Errorf("startTime(%d) = %q, nil; want an error for a pid no process holds", noSuchPID, got)
	}
}

// TestTrack: the record exists before the process does, names its group
// once it has started, and is gone once belay has reaped it.
func TestTrack(t *testing.T) {
	privateState(t)
	evidence := filepath.Join(t.TempDir(), "nodes", "004-test")
	argv := []string{"sleep", "60"}

	tr, err := Track(evidence, argv)
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	dir := recordDir(t, evidence)
	got := records(t, dir)
	if len(got) != 1 || got[0].PID != 0 || strings.Join(got[0].Argv, " ") != "sleep 60" {
		t.Fatalf("after Track, records = %+v; want one reservation naming no pid yet and the argv", got)
	}

	c := startSleeper(t)
	if err := tr.Started(c.pid()); err != nil {
		t.Fatalf("Started: %v", err)
	}
	got = records(t, dir)
	want := Record{PID: c.pid(), PGID: c.pid(), Start: mustStartTime(t, c.pid()), Argv: argv}
	if len(got) != 1 || got[0].PID != want.PID || got[0].PGID != want.PGID || got[0].Start != want.Start {
		t.Fatalf("after Started, records = %+v; want exactly %+v", got, want)
	}

	if err := tr.Done(); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if left := records(t, dir); len(left) != 0 {
		t.Errorf("after Done, %s holds %v; want nothing", dir, left)
	}
	// The run's record directory goes with its last record, so belay's state
	// directory does not keep one empty directory per step of every run.
	if _, err := os.Stat(filepath.Dir(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after Done, the run's record directory %s is still there (stat err %v)", filepath.Dir(dir), err)
	}
}

// TestTrackKeepsTheRecordOutsideTheWorkspace: the record of a process belay
// started is the thing that decides which process a resume kills, so it is
// kept where whatever runs in the workspace cannot write it -- under belay's
// own state directory, never in the run's evidence directory, which in a run
// that edits its workspace in place is inside that workspace.
func TestTrackKeepsTheRecordOutsideTheWorkspace(t *testing.T) {
	state := privateState(t)
	workspace := t.TempDir()
	evidence := filepath.Join(workspace, ".belay", "runs", "r", "nodes", "004-test")

	tr, err := Track(evidence, []string{"sleep", "60"})
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	t.Cleanup(func() { _ = tr.Done() })

	dir := recordDir(t, evidence)
	if rel, err := filepath.Rel(state, dir); err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("RecordDir = %s, want a directory under belay's state directory %s", dir, state)
	}
	if rel, err := filepath.Rel(workspace, dir); err == nil && !strings.HasPrefix(rel, "..") {
		t.Errorf("RecordDir = %s is inside the workspace %s", dir, workspace)
	}
	if n := len(records(t, dir)); n != 1 {
		t.Errorf("%s holds %d records, want the one Track wrote", dir, n)
	}
	if found, _ := filepath.Glob(filepath.Join(workspace, "*", "*", "*", "*", "*", recordGlob)); len(found) != 0 {
		t.Errorf("Track wrote %v inside the workspace", found)
	}
	other := filepath.Join(t.TempDir(), ".belay", "runs", "r", "nodes", "004-test")
	if recordDir(t, other) == dir {
		t.Errorf("two workspaces' runs share the record directory %s", dir)
	}
}

// TestReapNeverActsOnARecordInTheWorkspace is the security review's
// evidence. An agent working in place can write into .belay/runs/<id>/nodes,
// and a record it wrote there naming a process it chose -- with that
// process's real start time, which anyone can read from ps -- had Reap send
// it SIGKILL. A record found there is reported, and removed, and never acted
// on; one an older belay left there is reported the same way.
func TestReapNeverActsOnARecordInTheWorkspace(t *testing.T) {
	privateState(t)
	victim := startSleeper(t)
	root := filepath.Join(t.TempDir(), "nodes")
	path := filepath.Join(root, "004-test", "pgroup-1.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	forged := Record{PID: victim.pid(), PGID: victim.pid(), Start: mustStartTime(t, victim.pid()), Argv: []string{"sleep", "60"}}
	if err := os.WriteFile(path, encode(t, forged), 0o600); err != nil {
		t.Fatal(err)
	}

	got := Reap(root)

	if len(got) != 1 || got[0].Outcome != OutcomeUnverifiable || got[0].Err == nil {
		t.Errorf("Reap = %+v, want the record reported as one belay will not act on", got)
	}
	if !victim.alive() {
		t.Fatalf("Reap killed process %d on the word of a record anyone in the workspace could write", victim.pid())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the record is still in the workspace (stat err %v)", err)
	}
}

// TestReap is the proof the task asks for: Reap kills a group left running
// only when its leader is still the process belay started, and a recorded
// pid now held by any other process is never signalled.
func TestReap(t *testing.T) {
	tests := []struct {
		name string
		// child starts the live process the record is about; startSleeper
		// when nil.
		child func(t *testing.T) *child
		// record builds the file's contents for a live process c.
		record func(t *testing.T, c *child) []byte
		want   Outcome
		// wantKilled says whether c must have been SIGKILLed. Every row
		// that says no also proves c is still running afterwards.
		wantKilled bool
	}{
		{
			name: "a group whose leader is the process belay started is killed",
			record: func(t *testing.T, c *child) []byte {
				return encode(t, Record{PID: c.pid(), PGID: c.pid(), Start: mustStartTime(t, c.pid()), Argv: []string{"sleep", "60"}})
			},
			want:       OutcomeKilled,
			wantKilled: true,
		},
		{
			// The recorded process died and its pid was handed to c.
			// Only the start time can tell, and it must.
			name: "a recycled pid is never signalled",
			record: func(t *testing.T, c *child) []byte {
				return encode(t, Record{PID: c.pid(), PGID: c.pid(), Start: "Thu Jan  1 00:00:00 1970", Argv: []string{"sleep", "60"}})
			},
			want: OutcomeRecycled,
		},
		{
			// internal/exec starts every process with Setpgid, and a start
			// fails if the kernel refuses it, so a record naming a process
			// that did not lead its group is not one belay wrote.
			name: "a record naming a process that did not lead its group is never acted on",
			record: func(t *testing.T, c *child) []byte {
				return encode(t, Record{PID: c.pid(), PGID: syscall.Getpgrp(), Start: mustStartTime(t, c.pid()), Argv: []string{"sleep", "60"}})
			},
			want: OutcomeUnverifiable,
		},
		{
			// The record says the process led its group, but the live
			// process does not: it is not the one belay started.
			name:  "a live process that does not lead its own group is never signalled",
			child: startFollower,
			record: func(t *testing.T, c *child) []byte {
				return encode(t, Record{PID: c.pid(), PGID: c.pid(), Start: mustStartTime(t, c.pid()), Argv: []string{"sleep", "60"}})
			},
			want: OutcomeRecycled,
		},
		{
			name: "a live process running another program is never signalled",
			record: func(t *testing.T, c *child) []byte {
				return encode(t, Record{PID: c.pid(), PGID: c.pid(), Start: mustStartTime(t, c.pid()), Argv: []string{"claude", "-p", "implement the plan"}})
			},
			want: OutcomeRecycled,
		},
		{
			// Claude Code sets process.title = "claude", so the arguments
			// the platform reports are not the ones belay started it with;
			// what a program renaming itself keeps is its name.
			name:  "a process that rewrote its arguments but kept its name is still killed",
			child: startRetitled,
			record: func(t *testing.T, c *child) []byte {
				return encode(t, Record{PID: c.pid(), PGID: c.pid(), Start: mustStartTime(t, c.pid()), Argv: []string{"/usr/bin/perl", "-e", retitleScript}})
			},
			want:       OutcomeKilled,
			wantKilled: true,
		},
		{
			name: "a process that is gone is left alone",
			record: func(t *testing.T, _ *child) []byte {
				return encode(t, Record{PID: noSuchPID, PGID: noSuchPID, Start: "Thu Jan  1 00:00:00 1970", Argv: []string{"sleep", "60"}})
			},
			want: OutcomeGone,
		},
		{
			name: "a record with no start time cannot be verified, so it is never acted on",
			record: func(t *testing.T, c *child) []byte {
				return encode(t, Record{PID: c.pid(), PGID: c.pid(), Argv: []string{"sleep", "60"}})
			},
			want: OutcomeUnverifiable,
		},
		{
			name: "a reservation belay never filled in names no process",
			record: func(t *testing.T, _ *child) []byte {
				return encode(t, Record{Argv: []string{"sleep", "60"}})
			},
			want: OutcomeUnverifiable,
		},
		{
			name: "a damaged record is never acted on",
			record: func(*testing.T, *child) []byte {
				return []byte(`{"pid": 12`)
			},
			want: OutcomeUnverifiable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			privateState(t)
			start := tt.child
			if start == nil {
				start = startSleeper
			}
			c := start(t)
			root := filepath.Join(t.TempDir(), "nodes")
			dir := recordDir(t, filepath.Join(root, "004-test"))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "pgroup-1.json")
			if err := os.WriteFile(path, tt.record(t, c), 0o600); err != nil {
				t.Fatal(err)
			}

			got := Reap(root)

			if len(got) != 1 {
				t.Fatalf("Reap(%s) = %+v, want one result for the one record", root, got)
			}
			if got[0].Outcome != tt.want {
				t.Errorf("Outcome = %v, want %v (err %v)", got[0].Outcome, tt.want, got[0].Err)
			}
			if got[0].Path != path {
				t.Errorf("Path = %q, want %q", got[0].Path, path)
			}
			if tt.wantKilled && !c.killed(t) {
				t.Errorf("process %d was not killed", c.pid())
			}
			if !tt.wantKilled && !c.alive() {
				t.Errorf("process %d exited (%v); Reap must not signal a process it cannot prove is its own", c.pid(), c.err)
			}
			// Whatever the verdict, the record is spent: leaving it would
			// have the next resume act on it again.
			if left := records(t, dir); len(left) != 0 {
				t.Errorf("after Reap, %s still holds %v", dir, left)
			}
		})
	}
}

// TestReapFindsRecordsOnlyInNodeDirectories: records live one level down,
// in each node execution's evidence directory, and a root with no such
// directory is an empty result rather than an error.
func TestReapFindsRecordsOnlyInNodeDirectories(t *testing.T) {
	privateState(t)
	if got := Reap(filepath.Join(t.TempDir(), "nodes")); len(got) != 0 {
		t.Errorf("Reap(missing root) = %+v, want nothing", got)
	}

	root := filepath.Join(t.TempDir(), "nodes")
	runDir := filepath.Dir(recordDir(t, filepath.Join(root, "004-test")))
	// A file at the root and one two levels down are not records.
	for _, p := range []string{
		filepath.Join(runDir, "pgroup-1.json"),
		filepath.Join(runDir, "004-test", "deeper", "pgroup-1.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, encode(t, Record{PID: noSuchPID, PGID: noSuchPID, Start: "x"}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := Reap(root); len(got) != 0 {
		t.Errorf("Reap = %+v, want nothing: neither file is in a node directory", got)
	}
}

// TestParseStatStartTime covers Linux's start time, read from
// /proc/<pid>/stat, on every platform the suite runs on. The command name
// sits in parentheses and may itself hold spaces and parentheses, which is
// the whole difficulty.
func TestParseStatStartTime(t *testing.T) {
	// Fields 3 to 21 are filler here; field 22 is the start time.
	const tail = " S 1 10 10 0 -1 4194560 100 0 0 0 5 3 0 0 20 0 1 0 8765432 12345678 300"
	tests := []struct {
		name    string
		stat    string
		want    string
		wantErr bool
	}{
		{name: "a plain command", stat: "4242 (sleep)" + tail, want: "8765432"},
		{name: "a command with spaces and parentheses", stat: "4242 (my (odd) cmd)" + tail, want: "8765432"},
		{name: "no command name", stat: "4242 sleep S 1", wantErr: true},
		{name: "too few fields", stat: "4242 (sleep) S 1 10", wantErr: true},
		{name: "a start time that is not a number", stat: "4242 (sleep)" + strings.Replace(tail, "8765432", "soon", 1), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStatStartTime(tt.stat)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseStatStartTime(%q) = %q, %v; want an error: %v", tt.stat, got, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseStatStartTime(%q) = %q, want %q", tt.stat, got, tt.want)
			}
		})
	}
}
