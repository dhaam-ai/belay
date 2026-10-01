package state

import (
	"fmt"
	"path/filepath"
)

// Origin is where a fork came from: the run it was forked from, the visit of
// that run it re-runs, and what the history it inherited cost.
//
// `belay fork` makes a new run that starts from the blackboard as it stood
// before one visit of an existing run, so everything the fork's journal holds
// before that visit is history it inherited rather than work it did. Origin is
// how a person, and `belay runs`, tell the two apart, and how they find the
// source again.
//
// # Every dollar belongs to one run
//
// InheritedUSD is what that history cost the run that paid for it. It is
// shown, never charged: the fork's ledger starts at zero, because the fork's
// journal carries none of the inherited spend (the fork writes its inherited
// records without their usage), and the ledger is rebuilt from the journal.
// Charging it again would count the same money twice, and could stop a fork
// at a ceiling it had not spent towards.
//
// # A record, not a pin
//
// Origin names no capability (ADR-0011). Nothing a run does depends on it:
// the ledger starts at zero because of what the journal holds, not because
// anything reads this field, and the fork's shape is pinned by the same
// fields every run's is. A belay that predates forks would drop it, losing the
// record of where the run came from and changing nothing about what the run
// does, which is the test a capability has to pass. So a fork stays
// resumable by any belay that reads schema version 2.
type Origin struct {
	// RunID is the run this one was forked from: its source.
	RunID string `json:"run_id"`
	// Workspace is the source run's workspace, the repository `belay fork`
	// was pointed at, where the source can be found again.
	Workspace string `json:"workspace"`
	// Tree is the directory the fork's copy was made from: the tree the
	// source run's nodes worked in. It is Workspace for a run that edited it
	// in place; for a run on a branch it is the run's own worktree, when that
	// still existed.
	Tree string `json:"tree"`
	// Node is the node the fork re-runs.
	Node string `json:"node"`
	// Visit is which visit to Node the fork re-runs, numbered as `belay
	// timeline` numbers them (journal.Visit.Ordinal).
	Visit int `json:"visit"`
	// StartSeq is the Seq of the node_started that began that visit in the
	// source's journal. Every record of this run's journal before it was
	// inherited; the record at it is this run's own run_started.
	StartSeq uint64 `json:"start_seq"`
	// InheritedUSD is what the inherited history cost, in dollars: shown,
	// never charged (see "Every dollar belongs to one run").
	InheritedUSD float64 `json:"inherited_usd"`
	// InheritedEstimated reports whether any part of InheritedUSD was
	// estimated rather than reported. It is sticky, as everywhere belay totals
	// money: a figure built even partly from a guess is never shown as though
	// it were not.
	InheritedEstimated bool `json:"inherited_estimated"`
	// Deviations lists every way the fork's inputs differ from what the
	// visit it re-runs was given, in the words belay announced them before it
	// spent anything. Empty, never nil, for a fork that differs in nothing.
	Deviations []string `json:"deviations"`
}

// ForksDir returns the directory that holds the copies `belay fork` makes of
// the workspace at workspaceDir: <workspace>/.belay/forks. Each fork works in
// <ForksDir>/<fork-id>, and its own run directory is inside that copy, so the
// fork is found with --workspace pointed at the copy.
//
// It refuses a relative workspace for the reason NewLayout does: a relative
// path resolves against whatever directory belay happens to be running in.
func ForksDir(workspaceDir string) (string, error) {
	if !filepath.IsAbs(workspaceDir) {
		return "", fmt.Errorf("%w: workspace %q is not an absolute path", ErrInvalidPathSegment, workspaceDir)
	}
	return filepath.Join(workspaceDir, ".belay", "forks"), nil
}
