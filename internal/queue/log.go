//go:build unix

package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/dhaam-ai/belay/internal/ndjson"
)

// ErrNoSuchGoal reports an id no goal in the queue has.
var ErrNoSuchGoal = errors.New("queue: no such goal")

// ErrWrongState reports a change a goal cannot make from where it stands,
// such as removing a goal a worker has claimed. errors.As recovers a
// *StateError saying where the goal stands.
var ErrWrongState = errors.New("queue: the goal cannot make that change from where it stands")

// ErrGoalInHand reports a claim made while another goal is claimed and not yet
// settled. The queue runs one goal at a time.
var ErrGoalInHand = errors.New("queue: another goal is already in hand")

// StateError is a change refused because of where its goal stands. errors.Is
// matches it against ErrWrongState.
type StateError struct {
	// ID is the goal, and State where it stands.
	ID    int
	State State
	// Change is what was refused: "remove", "claim", "settle" or "approve".
	Change string
}

func (e *StateError) Error() string {
	return fmt.Sprintf("queue: cannot %s goal %d: it is %s", e.Change, e.ID, e.State)
}

// Unwrap exposes ErrWrongState for errors.Is.
func (e *StateError) Unwrap() error { return ErrWrongState }

// Read returns the queue as its log says, and writes nothing, not even to
// heal a torn final line: that is the next writer's job, under the log lock.
// A queue nobody has made reads as an empty one.
//
// It takes no lock. Every append is one write of one whole line, so a reader
// running beside a writer sees the line or a torn tail it leaves out, never
// a record half applied.
func (s *Store) Read() (Queue, error) {
	read, err := s.readLog()
	if err != nil {
		return Queue{}, err
	}
	q, err := fold(read.records)
	if err != nil {
		return Queue{}, fmt.Errorf("queue: read %s: %w", s.LogPath(), err)
	}
	return q, nil
}

// Add puts goal at the end of the queue and returns it, with the id it was
// given. noApproval is the person's own decision, made as they add the goal,
// that its plan need not wait for them; it travels with the goal from here
// on, through every crash and restart, and nothing else ever sets it.
//
// The space around goal is trimmed, as `belay run` trims its description,
// and a goal with nothing left is refused.
func (s *Store) Add(goal string, noApproval bool) (Entry, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return Entry{}, fmt.Errorf("%w: a goal must say what you want built", ErrInvalidRecord)
	}
	var id int
	q, err := s.append(func(q Queue) (Record, error) {
		// One more than the last id ever given, removed goals included, so
		// an id never names two goals.
		id = 1
		if n := len(q.Entries); n > 0 {
			id = q.Entries[n-1].ID + 1
		}
		return Record{Event: EventAdded, ID: id, Goal: goal, NoApproval: noApproval}, nil
	})
	if err != nil {
		return Entry{}, err
	}
	e, _ := q.Entry(id)
	return e, nil
}

// Remove takes goal id back out of the queue. Only a queued goal can be
// removed: once a worker has claimed one, its run exists or is about to, and
// removing it from the queue would leave that run belonging to nothing.
func (s *Store) Remove(id int) (Entry, error) {
	return s.change(Record{Event: EventRemoved, ID: id}, "remove", nil)
}

// Approve records that a person approved the plan of goal id, which must be
// waiting at it. The next worker continues the goal's run past the gate.
//
// It records the person's decision in the queue only. The approval the gate
// reads is the run's own, which the caller records first (see `belay queue
// approve`): approving here first would send the next worker into a gate
// that still had nothing approved, and it would park the goal again.
func (s *Store) Approve(id int) (Entry, error) {
	return s.change(Record{Event: EventApproved, ID: id}, "approve", nil)
}

// claim records that a worker has taken goal id, and names the run it will
// be and the copy it will run in, before either exists. See "Claim before
// create" in the package documentation.
func (s *Store) claim(id int, runID, dir string) (Entry, error) {
	rec := Record{Event: EventClaimed, ID: id, RunID: runID, Dir: dir}
	return s.change(rec, "claim", func(q Queue) error {
		for _, other := range q.Entries {
			if other.State == StateClaimed {
				return fmt.Errorf("%w: goal %d is claimed and not yet settled", ErrGoalInHand, other.ID)
			}
		}
		return nil
	})
}

// settle records how goal id's run ended, or that it is parked at its plan.
func (s *Store) settle(id int, state State, note string) (Entry, error) {
	return s.change(Record{Event: EventSettled, ID: id, State: state, Note: note}, "settle", nil)
}

// workerStarted records that a worker, process pid, began working through
// the queue.
func (s *Store) workerStarted(pid int) error {
	_, err := s.append(func(Queue) (Record, error) {
		return Record{Event: EventWorkerStarted, PID: pid}, nil
	})
	return err
}

// workerStopped records that the worker stopped, for stop, with note saying
// more. A note is cut to a length a person reads, as a run's journal notes
// are: it is usually an error, and an error can quote a great deal.
func (s *Store) workerStopped(stop Stop, note string) error {
	_, err := s.append(func(Queue) (Record, error) {
		return Record{Event: EventWorkerStopped, Stop: stop, Note: clip(note)}, nil
	})
	return err
}

// maxNoteRunes bounds a note the queue writes, as internal/graph bounds the
// notes it writes into a run's journal.
const maxNoteRunes = 500

// clip cuts s to maxNoteRunes runes, marking the cut.
func clip(s string) string {
	runes := []rune(s)
	if len(runes) <= maxNoteRunes {
		return s
	}
	return string(runes[:maxNoteRunes-1]) + "…"
}

// change appends rec, a move of one goal, refusing it with a *StateError
// when the goal cannot make it from where it stands, and with whatever check
// returns. Both are decided under the log lock, from the log as it is then,
// which is what keeps a change from racing another process's.
func (s *Store) change(rec Record, verb string, check func(Queue) error) (Entry, error) {
	q, err := s.append(func(q Queue) (Record, error) {
		e, ok := q.Entry(rec.ID)
		if !ok {
			return Record{}, fmt.Errorf("%w: %d", ErrNoSuchGoal, rec.ID)
		}
		// The fold's own rule for the move, applied to a copy: one place
		// says which moves a goal can make, whether it is being read back
		// or about to be written.
		if err := e.apply(rec); err != nil {
			current, _ := q.Entry(rec.ID)
			return Record{}, &StateError{ID: rec.ID, State: current.State, Change: verb}
		}
		if check != nil {
			if err := check(q); err != nil {
				return Record{}, err
			}
		}
		return rec, nil
	})
	if err != nil {
		return Entry{}, err
	}
	e, _ := q.Entry(rec.ID)
	return e, nil
}

// append records one change to the queue, and returns the queue as it then
// stands.
//
// Under the log lock it reads the log, refuses one that does not fold, heals
// a torn final line, asks build for the record to add given the queue as it
// is now, checks that the queue with the record added still folds, and
// appends the record, fsynced, before it lets go of the lock. Every writer
// goes through here, so no two can interleave a read and an append, and a
// record that would make the log unreadable is never written.
func (s *Store) append(build func(Queue) (Record, error)) (Queue, error) {
	unlock, err := s.lockLog()
	if err != nil {
		return Queue{}, err
	}
	defer unlock()

	read, err := s.readLog()
	if err != nil {
		return Queue{}, err
	}
	q, err := fold(read.records)
	if err != nil {
		return Queue{}, fmt.Errorf("queue: refusing to write to %s: %w", s.LogPath(), err)
	}
	if err := s.heal(read); err != nil {
		return Queue{}, err
	}

	rec, err := build(q)
	if err != nil {
		return Queue{}, err
	}
	if err := rec.validate(); err != nil {
		return Queue{}, err
	}
	// fold has checked that the records run 1, 2, ... with no gap.
	rec.Seq = uint64(len(read.records)) + 1
	rec.Time = s.now().UTC()
	next, err := fold(append(read.records, rec))
	if err != nil {
		return Queue{}, fmt.Errorf("queue: refusing to write a record the queue's own history refuses: %w", err)
	}

	line, err := json.Marshal(rec)
	if err != nil {
		return Queue{}, fmt.Errorf("queue: encode seq %d: %w", rec.Seq, err)
	}
	// #nosec G304 -- the log of the queue this Store was opened on.
	f, err := os.OpenFile(s.LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Queue{}, fmt.Errorf("queue: open %s: %w", s.LogPath(), err)
	}
	if err := ndjson.AppendLine(f, line); err != nil {
		_ = f.Close()
		return Queue{}, fmt.Errorf("queue: append to %s: %w", s.LogPath(), err)
	}
	if err := f.Close(); err != nil {
		return Queue{}, fmt.Errorf("queue: close %s: %w", s.LogPath(), err)
	}
	return next, nil
}

// heal makes the log a clean run of whole lines again before anything is
// appended to it: a torn final line, which an append cut short leaves, is
// cut off, and a final line that lost only its newline gets it back.
// Appending onto either would fuse two records into one line nobody can read.
func (s *Store) heal(read logRead) error {
	if read.incomplete {
		if err := ndjson.RewriteClean(s.LogPath(), read.lines); err != nil {
			return fmt.Errorf("queue: heal %s: %w", s.LogPath(), err)
		}
		return nil
	}
	if err := ndjson.EnsureTrailingNewline(s.LogPath()); err != nil {
		return fmt.Errorf("queue: heal %s: %w", s.LogPath(), err)
	}
	return nil
}

// logRead is the log as one read of it found it.
type logRead struct {
	// records are the whole records, in order, and lines their bytes as
	// they were written, kept so a heal rewrites them exactly.
	records []Record
	lines   [][]byte
	// incomplete is set when the final line was torn and left out.
	incomplete bool
}

// readLog reads the whole log. A log that does not exist reads as empty; a
// torn final line is left out and reported; a line that cannot be read with
// another after it is corruption, refused with ErrCorruptQueue, because an
// append can only ever damage the last line.
func (s *Store) readLog() (logRead, error) {
	f, err := os.Open(s.LogPath())
	if errors.Is(err, fs.ErrNotExist) {
		return logRead{}, nil
	}
	if err != nil {
		return logRead{}, fmt.Errorf("queue: read %s: %w", s.LogPath(), err)
	}
	defer func() { _ = f.Close() }()

	var read logRead
	incomplete, err := ndjson.Scan(f, func(line []byte) error {
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		read.records = append(read.records, rec)
		// Scan reuses its buffer for the next line.
		read.lines = append(read.lines, slices.Clone(line))
		return nil
	})
	if err != nil {
		var broken *ndjson.LineError
		if errors.As(err, &broken) {
			return logRead{}, fmt.Errorf("%w: line %d of %s cannot be read: %w",
				ErrCorruptQueue, broken.Line, s.LogPath(), broken.Err)
		}
		return logRead{}, fmt.Errorf("queue: read %s: %w", s.LogPath(), err)
	}
	read.incomplete = incomplete
	return read, nil
}
