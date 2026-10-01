//go:build unix

package queue

import (
	"fmt"
	"path/filepath"
	"time"
)

// Store is one workspace's queue on disk: the log, the two locks that order
// the processes using it, and the directory each goal's copy is made in.
//
// A Store holds no open file and no state between calls. Every mutation reads
// the log afresh under the log lock, so any number of Store values, in any
// number of processes, see one queue.
type Store struct {
	dir string
	now func() time.Time
}

// Option configures a Store. See WithClock.
type Option func(*Store)

// WithClock sets the clock a Store stamps records with, so a test can assert
// exact times. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// Open returns the queue of the workspace at workspace, which must be an
// absolute path.
//
// It touches nothing on disk. The queue's directory is made by the first
// thing that writes to it, so a command that only reads a queue, such as
// `belay runs` looking for runs in the queue's copies, never creates one.
func Open(workspace string, opts ...Option) (*Store, error) {
	// Absolute for the reason state.NewLayout gives: a relative path
	// resolves against whatever directory belay happens to be running in.
	if !filepath.IsAbs(workspace) {
		return nil, fmt.Errorf("queue: workspace %q is not an absolute path", workspace)
	}
	s := &Store{dir: filepath.Join(workspace, ".belay", "queue"), now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Dir returns the queue's directory, .belay/queue under the workspace.
func (s *Store) Dir() string { return s.dir }

// LogPath returns the path of the queue's log.
func (s *Store) LogPath() string { return filepath.Join(s.dir, "queue.ndjson") }

// WorkRoot returns the directory each goal's copy is made in, one directory
// per claimed goal, named for its run.
func (s *Store) WorkRoot() string { return filepath.Join(s.dir, "work") }

// workerLockPath is the file a worker locks for as long as it works.
func (s *Store) workerLockPath() string { return filepath.Join(s.dir, "worker.lock") }
