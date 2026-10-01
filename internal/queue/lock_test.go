//go:build unix

package queue

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// holderEnv, when set, makes this test binary a process that takes the worker
// lock of the workspace it names, says so, and then waits to be killed. See
// TestMain and TestWorkerLockDiesWithItsHolder.
const holderEnv = "BELAY_QUEUE_TEST_HOLD_WORKER_LOCK"

func TestMain(m *testing.M) {
	if ws := os.Getenv(holderEnv); ws != "" {
		holdTheWorkerLock(ws)
		return
	}
	os.Exit(m.Run())
}

// holdTheWorkerLock is the child process's whole life: take the lock, say
// "locked", and sleep until killed.
func holdTheWorkerLock(ws string) {
	s, err := Open(ws)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(2)
	}
	lock, err := s.LockWorker()
	if err != nil {
		fmt.Println("lock:", err)
		os.Exit(2)
	}
	fmt.Println("locked")
	time.Sleep(time.Minute)
	_ = lock.Release()
	os.Exit(3)
}

// TestWorkerLockIsExclusive: one `belay queue start` at a time. A second
// worker is refused, told which process holds the queue, and can work once
// the first lets go.
func TestWorkerLockIsExclusive(t *testing.T) {
	ws := t.TempDir()
	first, second := mustOpen(t, ws), mustOpen(t, ws)

	held, err := first.LockWorker()
	if err != nil {
		t.Fatalf("first LockWorker: %v", err)
	}
	_, err = second.LockWorker()
	var active *ActiveError
	if !errors.As(err, &active) || !errors.Is(err, ErrWorkerActive) {
		t.Fatalf("second LockWorker while the first holds it = %v, want an *ActiveError wrapping ErrWorkerActive", err)
	}
	if active.PID != os.Getpid() {
		t.Errorf("the refusal names pid %d as the holder, want %d", active.PID, os.Getpid())
	}

	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	again, err := second.LockWorker()
	if err != nil {
		t.Fatalf("LockWorker after the first worker let go: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestWorkerLockDiesWithItsHolder is the reboot promise. A worker killed with
// SIGKILL never releases anything, and the queue must not stay locked behind
// it: the next `belay queue start` has to be able to pick up where it
// stopped. A lock that was a file's existence would stay taken forever.
func TestWorkerLockDiesWithItsHolder(t *testing.T) {
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	holder := exec.CommandContext(ctx, os.Args[0]) //nolint:gosec // this test binary, run again as the holder
	holder.Env = append(os.Environ(), holderEnv+"="+ws)
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := holder.Start(); err != nil {
		t.Fatalf("start the holder: %v", err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("the holder said %q (%v), want \"locked\"", line, err)
	}

	s := mustOpen(t, ws)
	_, err = s.LockWorker()
	var active *ActiveError
	if !errors.As(err, &active) {
		t.Fatalf("LockWorker while another process holds it = %v, want an *ActiveError", err)
	}
	if active.PID != holder.Process.Pid {
		t.Errorf("the refusal names pid %d as the holder, want the holder's %d", active.PID, holder.Process.Pid)
	}
	if pid, ok, err := s.WorkerActive(); err != nil || !ok || pid != holder.Process.Pid {
		t.Errorf("WorkerActive() = (%d, %v, %v), want (%d, true, nil)", pid, ok, err, holder.Process.Pid)
	}

	if err := holder.Process.Kill(); err != nil {
		t.Fatalf("kill the holder: %v", err)
	}
	_ = holder.Wait()

	lock, err := s.LockWorker()
	if err != nil {
		t.Fatalf("LockWorker after its holder was killed = %v, want the lock: it must die with its holder", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestAChildProcessDoesNotKeepTheWorkerLock: a worker starts agents and test
// suites, and one can outlive it. If such a process inherited the lock's file
// descriptor, it would hold the queue for as long as it ran, and no `belay
// queue start` could pick the queue up behind it.
func TestAChildProcessDoesNotKeepTheWorkerLock(t *testing.T) {
	ws := t.TempDir()
	s := mustOpen(t, ws)
	lock, err := s.LockWorker()
	if err != nil {
		t.Fatalf("LockWorker: %v", err)
	}
	if _, err := mustOpen(t, ws).LockWorker(); !errors.Is(err, ErrWorkerActive) {
		t.Fatalf("a second LockWorker while it is held = %v, want ErrWorkerActive", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	child := exec.CommandContext(ctx, "sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatalf("start a child: %v", err)
	}
	defer func() { cancel(); _ = child.Wait() }()

	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	again, err := mustOpen(t, ws).LockWorker()
	if err != nil {
		t.Fatalf("LockWorker with only the worker's child still alive = %v, want the lock", err)
	}
	if err := again.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestWorkerActiveOnlyLooks: asking whether a worker is busy must not leave
// the queue locked, or `belay queue status` would stop the next start.
func TestWorkerActiveOnlyLooks(t *testing.T) {
	ws := t.TempDir()
	s := mustOpen(t, ws)

	if pid, ok, err := s.WorkerActive(); err != nil || ok || pid != 0 {
		t.Fatalf("WorkerActive() with no worker = (%d, %v, %v), want (0, false, nil)", pid, ok, err)
	}
	lock, err := s.LockWorker()
	if err != nil {
		t.Fatalf("LockWorker after WorkerActive looked = %v, want the lock", err)
	}
	if pid, ok, err := mustOpen(t, ws).WorkerActive(); err != nil || !ok || pid != os.Getpid() {
		t.Errorf("WorkerActive() while held = (%d, %v, %v), want (%d, true, nil)", pid, ok, err, os.Getpid())
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok, err := s.WorkerActive(); err != nil || ok {
		t.Errorf("WorkerActive() after Release = (%v, %v), want (false, nil)", ok, err)
	}
}

// mustOpen opens the queue of workspace ws.
func mustOpen(t *testing.T, ws string) *Store {
	t.Helper()
	s, err := Open(ws)
	if err != nil {
		t.Fatalf("Open(%s): %v", ws, err)
	}
	return s
}
