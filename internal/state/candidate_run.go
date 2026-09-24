package state

import "path/filepath"

// CandidateRunsDir returns the run's candidate-runs/ directory, which holds
// one full run directory per fanout candidate.
//
// It is deliberately not CandidatesDir. That one is the isolator's root —
// candidates/<id> is a candidate's workspace, the tree its agent edits and
// its tests run in — and promote destroys a loser's workspace, and the
// winner's too once the branch records the promotion. A candidate's journal,
// blackboard and manifest are the record of what belay did with the person's
// money, so they live beside the workspace rather than inside it, where a
// cleanup would take them with it.
func (l Layout) CandidateRunsDir() string { return filepath.Join(l.root, "candidate-runs") }

// NewCandidateRunLayout returns the Layout of one fanout candidate's own
// run: <parent-run>/candidate-runs/<id>, with its own manifest.json,
// state.json, journal.ndjson, artifacts, snapshots and node evidence
// directories.
//
// A candidate is a run, not a journal entry. The blackboard has one Code,
// Test, Review and Fix block; a ledger is rebuilt from one journal; a
// snapshot is keyed on one journal's sequence numbers; and internal/journal
// rejects a node_started opened while another is still open. N candidates
// sharing any of those would corrupt them on the second write, so each gets
// the whole shape a run has — and, for free, everything that already makes a
// run survive a crash.
//
// The returned Layout's workspace is the parent's: the tree this candidate
// will be isolated from, and the only absolute path there is before an
// isolator has made it one of its own. Its nodes must never work in it. The
// caller replaces it with WithWorkTree(ws.Dir) as soon as Isolator.Create
// has returned, which is why the claim step — which writes only files under
// RunDir — can happen before any workspace exists.
//
// id is refused rather than cleaned, for the reason validateSegment gives,
// and a zero parent Layout is refused outright: building a run's files
// relative to whatever directory belay happens to be running in is never
// what a caller meant.
func NewCandidateRunLayout(parent Layout, id string) (Layout, error) {
	if err := parent.check(); err != nil {
		return Layout{}, err
	}
	root, err := joinSafe("candidate id", parent.CandidateRunsDir(), id)
	if err != nil {
		return Layout{}, err
	}
	return Layout{workspace: parent.workspace, root: root}, nil
}
