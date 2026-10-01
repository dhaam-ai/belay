//go:build unix

package queue

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrWorkerActive reports that another worker holds the queue: another `belay
// queue start` is working through it now.
var ErrWorkerActive = errors.New("queue: another worker is working through this queue")

// ActiveError is the refusal LockWorker gives while another worker holds the
// queue. errors.Is matches it against ErrWorkerActive.
type ActiveError struct {
	// PID is the process the worker lock's file names as its holder, or 0
	// when it names none. It is for the person reading the refusal: the lock
	// is what decides, never the number.
	PID int
}

func (e *ActiveError) Error() string {
	if e.PID == 0 {
		return ErrWorkerActive.Error()
	}
	return fmt.Sprintf("%v (pid %d)", ErrWorkerActive, e.PID)
}

// Unwrap exposes ErrWorkerActive for errors.Is.
func (e *ActiveError) Unwrap() error { return ErrWorkerActive }

// WorkerLock is the hold one worker has on the queue for as long as it works
// through it. Take it with Store.LockWorker.
//
// It is an exclusive flock(2) on .belay/queue/worker.lock, and its strength
// is what happens when its holder dies. The kernel drops a flock when the
// last descriptor of its open file is closed, and a process that dies, by
// SIGKILL or a power cut, has every descriptor closed for it. So a worker
// that is killed leaves the queue unlocked behind it, and the next `belay
// queue start` after a reboot can pick up where it stopped. A lock that was
// the existence of a file would stay taken by a dead process forever, and
// every recovery would begin with a person deleting it by hand.
//
// The descriptor is opened, as Go opens every file, close-on-exec, so the
// agents and test suites a worker starts never inherit it. One of them can
// outlive its worker, and holding the queue for as long as it ran would stop
// every later start.
//
// flock is not reliable on network filesystems, and a queue in a workspace on
// one is not supported. That is documented, not detected: nothing portable can
// tell.
type WorkerLock struct {
	f *os.File
}

// LockWorker takes the worker lock, or refuses with an *ActiveError naming
// the process that holds it.
//
// It writes this process's id into the lock's file, for the refusal another
// worker will give. The id is only ever read for a person: a recycled pid
// cannot make a lock look held, because the lock is the flock and not the
// number.
func (s *Store) LockWorker() (*WorkerLock, error) {
	f, err := s.openLockFile(s.workerLockPath())
	if err != nil {
		return nil, err
	}
	if err := takeWorkerLock(f); err != nil {
		holder := holderOf(f)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &ActiveError{PID: holder}
		}
		return nil, fmt.Errorf("queue: lock %s: %w", s.workerLockPath(), err)
	}
	if err := writeHolder(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("queue: record this worker in %s: %w", s.workerLockPath(), err)
	}
	return &WorkerLock{f: f}, nil
}

// takeWorkerLock takes an exclusive flock on f without waiting for it.
//
// A refusal is tried again for a moment before it is believed. WorkerActive
// takes the same lock for an instant to see whether a worker holds it, and a
// start that landed in that instant would otherwise be told, wrongly, that
// another worker is busy. A real worker holds the lock for hours, so a few
// tries over a tenth of a second cannot mistake one for the other.
func takeWorkerLock(f *os.File) error {
	const tries = 5
	var err error
	for range tries {
		if err = flock(f, syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}

// Release lets go of the queue, by closing the lock's descriptor; the kernel
// drops the flock with it. The lock's file is left where it is. Removing it
// would let a worker that opened it a moment earlier lock a file nobody else
// can reach any more, while the next worker locks a new one.
func (l *WorkerLock) Release() error {
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("queue: release the worker lock: %w", err)
	}
	return nil
}

// WorkerActive reports whether a worker holds the queue now, and the process
// the lock's file names as holding it. `belay queue status` asks it; nothing
// decides anything on its answer.
//
// It looks by trying the lock and letting go at once, so it never leaves the
// queue locked. A worker starting in that instant retries (see
// takeWorkerLock) rather than being turned away.
func (s *Store) WorkerActive() (pid int, active bool, err error) {
	f, err := os.Open(s.workerLockPath())
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("queue: look at %s: %w", s.workerLockPath(), err)
	}
	defer func() { _ = f.Close() }()

	switch err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB); {
	case err == nil:
		return 0, false, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return holderOf(f), true, nil
	default:
		return 0, false, fmt.Errorf("queue: look at %s: %w", s.workerLockPath(), err)
	}
}

// lockLog takes the log lock, waiting for it, and returns the function that
// lets go of it. Every write to the log happens under it (see Store.append).
//
// It is a second flock, on .belay/queue/queue.lock, rather than the worker
// lock, because the two are held by different processes for very different
// lengths of time. A worker holds the queue for hours; `belay queue add` from
// another terminal must still be able to add a goal in the meantime, and only
// needs the log to itself for the few milliseconds one append takes.
func (s *Store) lockLog() (func(), error) {
	f, err := s.openLockFile(s.logLockPath())
	if err != nil {
		return nil, err
	}
	if err := flock(f, syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("queue: lock %s: %w", s.logLockPath(), err)
	}
	return func() { _ = f.Close() }, nil
}

// openLockFile opens, creating it and the queue's directory if need be, a
// file this package locks.
func (s *Store) openLockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return nil, fmt.Errorf("queue: create %s: %w", s.dir, err)
	}
	// #nosec G304 -- path is one of this package's own lock files, under the
	// queue directory the Store was opened with.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("queue: open %s: %w", path, err)
	}
	return f, nil
}

// flock applies how to f, trying again when a signal interrupts the call.
func flock(f *os.File, how int) error {
	for {
		// #nosec G115 -- a file descriptor is a small non-negative int.
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// writeHolder records this process as the lock's holder.
func writeHolder(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return err
}

// holderOf reads the process a lock's file names, or 0 when it names none
// that can be read.
func holderOf(f *os.File) int {
	data, err := io.ReadAll(io.NewSectionReader(f, 0, 64))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}
