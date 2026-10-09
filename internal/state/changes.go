package state

import "path/filepath"

// ChangesFrom names where a run learns what its workspace changed.
type ChangesFrom string

const (
	// ChangesFromGit asks git what differs from Changes.Base in the run's
	// working tree: a branch run, a fanout candidate on a git worktree, and
	// a run that edits a git checkout in place.
	ChangesFromGit ChangesFrom = "git"
	// ChangesFromRecord compares the workspace with the file record taken
	// when the workspace was handed to the run (Layout.FileRecordPath): a
	// workspace with no git repository, and the copies belay makes for
	// queued goals and fanout candidates, which leave .git out.
	ChangesFromRecord ChangesFrom = "record"
	// ChangesReported has no ground truth, and takes the agent's report.
	// Changes.Why says why, and the policy gate says so in every verdict.
	ChangesReported ChangesFrom = "reported"
)

// Changes is where a run learns what its workspace really changed, as
// opposed to what its agent says it changed.
//
// It exists because the agent's report is a claim. The policy gate judged
// it, and an agent that edited infra/main.tf and belay.yaml while reporting
// only src/app.py walked past every rule, with both edits committed. The
// write node now records the workspace's own answer beside the report, and
// the gate judges that.
//
// It is pinned when the run is created, like Output, because the answer
// depends on a "before" only the run's creation can capture: the commit a
// branch was cut from, the files a person had already changed in a checkout,
// a record of a directory with no git in it. A resume asks the same question
// of the same base.
//
// It names no capability (ADR-0011). Every run that depends on it to stay
// safe is a guarded one, and a guarded run already requires policy.gate,
// which no released belay knows; a belay that knows policy.gate knows this
// field too. An unguarded run that lost it would record its agent's report
// instead of the truth, which is what every belay before it did.
type Changes struct {
	// From is where the change is read from.
	From ChangesFrom `json:"from"`
	// Base is the full commit id a git run's change is measured from:
	// Output.Base for a branch run, and HEAD when the run was created for a
	// run that edits a checkout in place.
	Base string `json:"base,omitempty"`
	// Record says the run directory holds a file record the change is read
	// against: the whole workspace for ChangesFromRecord, or, for a git run
	// in place, the files the person had already changed when it began, so
	// that their edits are not charged to the run.
	Record bool `json:"record,omitempty"`
	// Why is, for ChangesReported, the reason there is no ground truth, in
	// words the gate can put in its verdict.
	Why string `json:"why,omitempty"`
}

// SetChanges pins where this run learns what its workspace changed.
func (m *Manifest) SetChanges(c Changes) { m.Changes = &c }

// FileRecordPath returns <run>/file-record.json, where a run keeps the
// record of its workspace's files that its change is read against (see
// Changes.Record). A zero Layout returns "".
func (l Layout) FileRecordPath() string {
	if l.root == "" {
		return ""
	}
	return filepath.Join(l.root, "file-record.json")
}
