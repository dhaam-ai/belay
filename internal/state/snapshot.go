package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// SnapshotSchemaVersion is the schema version SaveSnapshotOnce writes and the
// only one LoadSnapshot reads. It versions the snapshot's own envelope; the
// blackboard inside carries state.json's schema version and is read as
// state.json is (see LoadSnapshot).
const SnapshotSchemaVersion = 1

// ErrUnsupportedSnapshotVersion reports a snapshot whose schema_version this
// belay cannot read.
var ErrUnsupportedSnapshotVersion = errors.New("state: unsupported snapshot schema version")

// Snapshot is the blackboard exactly as it stood before one node execution
// ran, written once and never changed.
//
// # Why the blackboard is copied rather than rebuilt
//
// Two things need the blackboard as it was before a node ran: a fork, which
// re-runs one visit of a finished run, and the dispatcher re-running an
// attempt a crash cut short. Nothing else on disk still has it. state.json
// is overwritten in place by every patch. The journal records transitions,
// not patches, so it cannot be replayed. And not every change to the
// blackboard is a patch at all: `belay resume` records a person's approval,
// and `belay plan --regenerate` their request for a new plan, by saving
// state.json directly while the run is paused. A log of patches would miss
// both.
//
// A copy needs no replaying. Every Patch field is an absolute set, so the
// state before an execution is simply the state as it stood then, whoever
// wrote it; there is no fold to get wrong and nothing to keep in step with
// Patch as it grows.
//
// # Which executions get one
//
// The dispatcher takes a snapshot before every execution at attempt 1, keyed
// on that execution's node_started Seq. That is the first execution of every
// visit (journal.Visit.StartSeq), which is what a fork starts from, and also
// every time a gate's visit is taken up again after the person it paused for
// has acted. A re-run at a later attempt takes none: it reads the snapshot of
// the attempt-1 execution it repeats.
//
// Snapshotting only the start of each visit would be wrong for the gate. A
// person who asks for a new plan while the run waits at the approve gate
// writes the request into state.json after that visit's snapshot was taken,
// and a crash while the gate is acting on it would have the re-run read the
// visit's snapshot and ask them to approve the plan they had just rejected.
//
// # Written once, before the node runs
//
// The dispatcher writes a snapshot after the execution's node_started is
// durable and before the node runs, and SaveSnapshotOnce never replaces one.
// Together those make a missing snapshot proof that no attempt of that
// execution has run the node yet — so state.json still holds the state from
// before it, and a snapshot taken from it now is the right one. A snapshot
// that exists is kept whatever state.json says, because state.json may
// already hold the patch of the attempt that crashed.
//
// # What a snapshot is not
//
// It is the blackboard and nothing else. The workspace's files are not in it
// (WorkspaceDigest only lets a fork tell whether they have changed since), nor
// the agent backend's own session, nor the manifest or the budget ledger,
// which a resume rebuilds from the journal.
//
// # Cost
//
// One more fsynced write per node execution, the size of state.json at the
// time. state.json's History grows with every node, so a run's snapshots
// together grow with the square of its length; graph.max_steps bounds that,
// and nothing prunes them, since a fork may ask for any visit.
//
// # No capability, and why
//
// Snapshots add no name to either document's requires list (ADR-0011). A
// capability keeps out a belay that would misread a run, and a belay that
// predates snapshots reads none: it re-runs an interrupted node from
// state.json, as every belay did before this one, and nothing it saves drops
// a field this one depends on. What a snapshot does carry, the blackboard, is
// read exactly as state.json is — migrated from an older version, and refused
// if it requires a capability this belay lacks — so a node is never re-run on
// a blackboard missing a field that changes what it does.
type Snapshot struct {
	// SchemaVersion is the snapshot envelope's schema version. SaveSnapshotOnce
	// sets it.
	SchemaVersion int `json:"schema_version"`
	// RunID is the run the snapshot belongs to.
	RunID string `json:"run_id"`
	// Node is the node the execution ran.
	Node string `json:"node"`
	// Seq is the Seq of the execution's node_started record, which names the
	// snapshot (see Layout.SnapshotPath). For the first execution of a visit
	// it is journal.Visit.StartSeq.
	Seq uint64 `json:"seq"`
	// Step is the step the execution ran as, the RunContext.Step its node saw.
	Step int `json:"step"`
	// TakenAt is when the snapshot was written.
	TakenAt time.Time `json:"taken_at"`
	// WorkspaceDigest fingerprints the workspace as it was when the snapshot
	// was taken, or is empty when no fingerprint was taken or taking one
	// failed. A reader must treat empty as "unknown", never as "unchanged".
	WorkspaceDigest string `json:"workspace_digest"`
	// State is the blackboard.
	State State `json:"state"`
}

// SnapshotsDir returns the run's snapshots/ directory.
func (l Layout) SnapshotsDir() string { return filepath.Join(l.root, "snapshots") }

// SnapshotPath returns the path of the snapshot taken before the execution
// whose node_started record has Seq seq: snapshots/<seq>-<node>.json, with
// seq zero-padded to eight digits so a directory listing reads in run order.
//
// It is keyed on the journal's Seq rather than the run's step because a fork
// finds it from the journal, which records Seqs and not steps. The node is in
// the name for the person reading the directory; the Seq alone is unique.
// It refuses a Seq of 0, which no journal record has, a node that could
// escape snapshots/, and a zero Layout.
func (l Layout) SnapshotPath(seq uint64, node string) (string, error) {
	if err := l.check(); err != nil {
		return "", err
	}
	if seq == 0 {
		return "", &PathSegmentError{Kind: "snapshot seq", Segment: "0", Reason: "no journal record has seq 0"}
	}
	if err := validateSegment("node name", node); err != nil {
		return "", err
	}
	return joinSafe("node name", l.SnapshotsDir(), fmt.Sprintf("%08d-%s.json", seq, node))
}

// LoadSnapshot reads the snapshot at path.
//
// A missing snapshot is an error wrapping fs.ErrNotExist, which is how the
// dispatcher learns that no attempt of an execution has run its node. A
// snapshot envelope at another schema version is an error wrapping
// ErrUnsupportedSnapshotVersion. The blackboard inside is decoded as
// LoadState decodes state.json: migrated from an older schema version, and
// refused with a *CapabilityError if it requires a capability this belay does
// not know.
func LoadSnapshot(path string) (Snapshot, error) {
	raw, err := readFile(path)
	if err != nil {
		return Snapshot{}, err
	}

	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Snapshot{}, fmt.Errorf("state: decode snapshot %s: %w", path, err)
	}
	if probe.SchemaVersion != SnapshotSchemaVersion {
		return Snapshot{}, fmt.Errorf("%w: %s is version %d, this belay reads version %d",
			ErrUnsupportedSnapshotVersion, path, probe.SchemaVersion, SnapshotSchemaVersion)
	}

	var doc struct {
		Snapshot
		// State is decoded separately, through decodeState.
		State json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Snapshot{}, fmt.Errorf("state: decode snapshot %s: %w", path, err)
	}
	st, err := decodeState(doc.State)
	if err != nil {
		return Snapshot{}, fmt.Errorf("state: snapshot %s: %w", path, err)
	}
	snap := doc.Snapshot
	snap.State = st
	return snap, nil
}

// SaveSnapshotOnce durably writes snap as the snapshot for its Seq and Node,
// unless one is already there, and reports whether it wrote.
//
// It never replaces a snapshot, whatever the new one holds: the first
// snapshot taken for an execution is the blackboard from before any attempt
// of it ran, and a later one might not be (see Snapshot). The write goes
// through atomicWriteFile, so a crash leaves either no snapshot or a whole
// one — never a torn file that would stop the next resume.
//
// The check and the write are not one atomic step. Like every Store method it
// relies on one dispatcher owning a run at a time; within that process the
// Store's lock serializes them.
func (s *Store) SaveSnapshotOnce(snap Snapshot) (written bool, err error) {
	path, err := s.layout.SnapshotPath(snap.Seq, snap.Node)
	if err != nil {
		return false, err
	}
	snap.SchemaVersion = SnapshotSchemaVersion

	s.mu.Lock()
	defer s.mu.Unlock()
	switch _, err := os.Lstat(path); {
	case err == nil:
		return false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("state: check for snapshot %s: %w", path, err)
	}
	if err := writeJSON(path, snap, filePerm); err != nil {
		return false, err
	}
	return true, nil
}
