// Package state owns belay's on-disk run state: manifest.json, the run's
// identity, lifecycle and budget ledger, and state.json, the blackboard
// that graph nodes read and that the dispatcher patches after every node.
//
// # Ownership
//
// Per ADR-0002, a node is a pure function that returns a Result carrying a
// Patch and never touches disk itself. This package is what the dispatcher
// applies that Patch through, and it is the only code in belay that writes
// manifest.json or state.json. It does not own journal.ndjson — that
// belongs to internal/journal — but Layout knows its path, since a Layout
// is the map of an entire run directory, not just the two files this
// package writes.
//
// # Durability
//
// Every write in this package goes through atomicWriteFile: write a
// temporary file in the run directory, fsync it, rename it over the
// target, then fsync the directory. The rename is what makes the update
// atomic — a reader sees either the whole old file or the whole new one,
// never a partial write — and the directory fsync is what makes the
// rename itself survive a crash, not merely a process exit. See ADR-0003
// for the same reasoning applied to the journal.
//
// # Concurrency
//
// Store serializes its own reads and writes with a mutex: safe for many
// goroutines inside one dispatcher process, not for two processes sharing
// a run directory. Nothing in this package arbitrates across process
// boundaries — belay assumes exactly one dispatcher owns a run at a time.
//
// # Schema versioning
//
// Both on-disk documents carry a schema_version field. LoadState and
// LoadManifest reject a version they do not recognize with a typed error
// (VersionError, ManifestVersionError) unless a migration has been
// registered for it; today neither package has one, since both schemas
// are still at version 1.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// cloneSlice returns an independent copy of s, preserving nil versus
// non-nil-empty exactly.
//
// append([]T(nil), s...) cannot do this: appending zero elements onto a
// nil destination returns nil regardless of whether s itself was nil or
// non-nil-empty. That distinction is not cosmetic here — it is what
// belay.QualityReport.Issues's own "never nil" contract depends on, and
// what lets NewState's "candidates: [] rather than null" guarantee survive
// a Patch (see Patch.Apply) instead of silently reverting to null the
// first time something patches Candidates with an empty slice.
func cloneSlice[T any](s []T) []T {
	if s == nil {
		return nil
	}
	out := make([]T, len(s))
	copy(out, s)
	return out
}

// StateSchemaVersion is the schema version SaveState writes and the only
// version LoadState accepts without a registered migration.
const StateSchemaVersion = 1

// ErrUnsupportedStateVersion reports a state.json whose schema_version
// LoadState does not know how to read and has no migration registered for.
var ErrUnsupportedStateVersion = errors.New("state: unsupported state schema version")

// VersionError explains which unsupported state.json schema version
// was found. It wraps ErrUnsupportedStateVersion.
type VersionError struct {
	// Got is the schema_version found on disk.
	Got int
}

// Error implements error.
func (e *VersionError) Error() string {
	return fmt.Sprintf("state: state.json schema version %d is not supported (want %d)", e.Got, StateSchemaVersion)
}

// Unwrap reports ErrUnsupportedStateVersion.
func (e *VersionError) Unwrap() error { return ErrUnsupportedStateVersion }

// Migration upgrades a raw state.json document exactly one schema
// version forward. A migration for version N is registered at
// stateMigrations[N] and must return a document whose own schema_version
// is N+1; decodeState re-probes and applies migrations repeatedly until it
// reaches StateSchemaVersion.
type Migration func(raw []byte) ([]byte, error)

// stateMigrations holds no entries today: StateSchemaVersion is state.json's
// only version so far. It exists so a future schema bump has somewhere to
// register its upgrade path without changing decodeState's logic.
var stateMigrations = map[int]Migration{}

// State is the blackboard graph nodes read and the dispatcher patches
// after every node — belay's working memory for one run.
//
// The zero State is not a valid on-disk document (SchemaVersion is 0);
// construct one with NewState.
type State struct {
	// SchemaVersion is this document's schema version.
	SchemaVersion int `json:"schema_version"`
	// Goal is the run's objective, as given by the caller that started it.
	Goal string `json:"goal"`
	// Plan is the plan node's output.
	Plan Plan `json:"plan"`
	// Code is the code node's output.
	Code Code `json:"code"`
	// Test is the most recent test run's result.
	Test Test `json:"test"`
	// Review is the most recent review's result.
	Review Review `json:"review"`
	// Fix is the fix loop's own progress.
	Fix Fix `json:"fix"`
	// Candidates lists every best-of-N fanout candidate's outcome, once
	// fanout has run. Empty, never nil, for a run with no fanout.
	Candidates []Candidate `json:"candidates"`
	// Winner is the ID of the fanout candidate selected to proceed, or
	// empty if fanout has not run or has not yet selected one.
	Winner string `json:"winner"`
	// History is the ordered log of completed node executions. Empty,
	// never nil, for a run that has not completed a node yet.
	History []HistoryEntry `json:"history"`
}

// NewState returns the initial blackboard for a run pursuing goal: schema
// current, everything else zero, with Candidates and History set to
// non-nil empty slices so a freshly started run serializes
// "candidates": [] and "history": [] rather than null.
func NewState(goal string) State {
	return State{
		SchemaVersion: StateSchemaVersion,
		Goal:          goal,
		Candidates:    []Candidate{},
		History:       []HistoryEntry{},
	}
}

// Plan is the blackboard's record of the plan node's output.
type Plan struct {
	// Path is where the plan document was archived, typically under
	// Layout.ArtifactPath (for example "plan.md").
	Path string `json:"path"`
	// Approved reports whether a human (or an auto-approve policy) has
	// signed off on the plan. The approve node reads this to decide
	// whether coding may begin.
	Approved bool `json:"approved"`
	// Digest is a content fingerprint of the plan, used to detect that a
	// resumed run's plan has not silently changed underneath it.
	Digest string `json:"digest"`
}

// Code is the blackboard's record of the code node's output.
type Code struct {
	// SessionID identifies the agent backend session that produced the
	// current code changes, so a later invocation (for example inside the
	// fix loop) can resume it rather than starting fresh. Empty if the
	// backend has no session concept.
	SessionID string `json:"session_id"`
	// LastDiff is where the most recent diff was archived, typically under
	// Layout.ArtifactPath (for example "diff-0007.patch").
	LastDiff string `json:"last_diff"`
	// ChangedFiles lists the paths, relative to the workspace root, the
	// code node reported changing.
	ChangedFiles []string `json:"changed_files"`
}

// Test is the blackboard's projection of a belay.TestReport: the fields
// the graph acts on, plus where the full report was archived. Unlike
// belay.TestReport, Test never carries Duration or Raw — those are not
// part of the blackboard, and live only in the archived report at
// ReportPath.
type Test struct {
	// Passed is belay.TestReport.Passed.
	Passed int `json:"passed"`
	// Total is belay.TestReport.Total.
	Total int `json:"total"`
	// Failed is belay.TestReport.Failed.
	Failed int `json:"failed"`
	// Failures is belay.TestReport.Failures, reusing belay's own type
	// rather than a parallel declaration.
	Failures []belay.TestFailure `json:"failures"`
	// ReportPath is where the dispatcher archived the full
	// belay.TestReport (as JSON), typically under Layout.ArtifactPath (for
	// example "test.json"). Empty if no test has run yet.
	ReportPath string `json:"report_path"`
}

// NewTest projects r onto the blackboard's Test shape. reportPath is where
// the dispatcher archived the full report, for anything that later needs
// Duration or Raw.
func NewTest(r belay.TestReport, reportPath string) Test {
	return Test{
		Passed:     r.Passed,
		Total:      r.Total,
		Failed:     r.Failed,
		Failures:   cloneSlice(r.Failures),
		ReportPath: reportPath,
	}
}

// Report reconstructs a belay.TestReport from t. Duration and Raw are not
// part of the blackboard projection and always come back zero; read them
// from the full report archived at t.ReportPath.
func (t Test) Report() belay.TestReport {
	return belay.TestReport{
		Total:    t.Total,
		Passed:   t.Passed,
		Failed:   t.Failed,
		Failures: cloneSlice(t.Failures),
	}
}

// Review is the blackboard's projection of a belay.QualityReport: every
// field the graph acts on. Unlike belay.QualityReport, Review never
// carries Raw, which the graph never reads and which legitimately differs
// in shape between adapters.
//
// Review is a pure function of belay.QualityReport (see NewReview and
// Review.QualityReport): two conforming reviewers that agree on Source,
// Gate, Counts, Issues and Summary always project to byte-identical Review
// JSON. That is what lets a deterministic linter and an AI reviewer be
// fully interchangeable to everything downstream of state.json — the same
// guarantee belay.QualityReport's own doc comment makes for the two
// adapters themselves.
type Review struct {
	// Source is belay.QualityReport.Source.
	Source string `json:"source"`
	// Gate is belay.QualityReport.Gate.
	Gate belay.GateStatus `json:"gate"`
	// Issues is belay.QualityReport.Issues, reusing belay's own type
	// rather than a parallel declaration.
	Issues []belay.Issue `json:"issues"`
	// Counts is belay.QualityReport.Counts.
	Counts belay.Counts `json:"counts"`
	// Summary is belay.QualityReport.Summary, carried through verbatim.
	// The adapter's own prose belongs to the adapter; the pointer to the
	// archived report is ReportPath, not something appended here.
	Summary string `json:"summary"`
	// ReportPath is the run-relative path of the archived full report,
	// mirroring Test.ReportPath. The blackboard keeps only the fields the
	// graph acts on, so this is how a human or a later node reaches the
	// adapter's untruncated output.
	ReportPath string `json:"report_path"`
}

// NewReview projects q onto the blackboard's Review shape, recording where
// the full report was archived. Pass an empty reportPath when the report was
// not written to disk.
func NewReview(q belay.QualityReport, reportPath string) Review {
	return Review{
		Source:     q.Source,
		Gate:       q.Gate,
		Issues:     cloneSlice(q.Issues),
		Counts:     q.Counts,
		Summary:    q.Summary,
		ReportPath: reportPath,
	}
}

// QualityReport reconstructs a belay.QualityReport from r, preserving
// Gate, Counts and Issues exactly — the fields the graph acts on. Raw is
// not part of the blackboard projection and always comes back nil; Issues
// comes back non-nil even if r.Issues was nil, matching
// belay.QualityReport.Issues's own "never nil" contract.
func (r Review) QualityReport() belay.QualityReport {
	issues := cloneSlice(r.Issues)
	if issues == nil {
		issues = []belay.Issue{}
	}
	return belay.QualityReport{
		Source:  r.Source,
		Gate:    r.Gate,
		Counts:  r.Counts,
		Issues:  issues,
		Summary: r.Summary,
	}
}

// Fix is the blackboard's record of the fix loop's own progress.
type Fix struct {
	// Attempts is the number of fix attempts made so far. It is always the
	// absolute count, never a delta a caller should add to — see Patch.
	Attempts int `json:"attempts"`
	// GiveUp reports whether the fix loop has exhausted its retry budget
	// (see Graph.GiveUp in internal/config) and stopped trying.
	GiveUp bool `json:"give_up"`
}

// Candidate is one best-of-N fanout candidate's outcome, as recorded on
// the blackboard once it finishes.
type Candidate struct {
	// ID is the candidate's identifier, also used as its
	// Layout.CandidateDir name.
	ID string `json:"id"`
	// Dir is the absolute path to the candidate's isolated workspace.
	Dir string `json:"dir"`
	// TestPassed reports whether the candidate's test run was OK (see
	// belay.TestReport.OK).
	TestPassed bool `json:"test_passed"`
	// IssueCount is the candidate's review issue count, at or above
	// whatever severity the run's selection strategy cares about.
	IssueCount int `json:"issue_count"`
}

// HistoryEntry records one completed node execution on the blackboard.
//
// Seq is the journal record sequence number the dispatcher assigned to
// this execution's node_finished event, and it is what makes appending a
// HistoryEntry through a Patch idempotent: applying a Patch whose History
// carries a Seq already present in State.History replaces that entry in
// place instead of appending a duplicate — see Patch.Apply.
type HistoryEntry struct {
	// Seq is the journal sequence number of this execution's completion
	// record. Must be non-zero; see Patch.Apply.
	Seq uint64 `json:"seq"`
	// Node is the node that ran.
	Node string `json:"node"`
	// Attempt is the 1-based execution attempt of Node this entry
	// describes.
	Attempt int `json:"attempt"`
	// Status is the node's outcome, as a short lowercase word such as "ok"
	// or "failed". Free text by design: this package does not police the
	// dispatcher's status vocabulary, only records it.
	Status string `json:"status"`
	// Next is the node the dispatcher chose to run after this one, empty
	// if this execution was terminal.
	Next string `json:"next"`
	// Time is when this execution finished.
	Time time.Time `json:"time"`
	// Summary is a short, human-readable note about the execution, such as
	// a failure reason. May be empty.
	Summary string `json:"summary"`
}

// LoadState reads and decodes the state.json at path.
//
// If the document's schema_version is not StateSchemaVersion, LoadState
// looks for a registered migration and applies it repeatedly until the
// document reaches StateSchemaVersion; if none is registered for the
// version found, it returns a *VersionError (wrapping
// ErrUnsupportedStateVersion) rather than guessing at the shape.
func LoadState(path string) (State, error) {
	raw, err := readFile(path)
	if err != nil {
		return State{}, err
	}
	return decodeState(raw)
}

// decodeState is LoadState's migration-aware core, split out so tests can
// exercise it directly against in-memory fixtures.
func decodeState(raw []byte) (State, error) {
	for {
		var probe struct {
			SchemaVersion int `json:"schema_version"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return State{}, fmt.Errorf("state: decode state.json: %w", err)
		}
		if probe.SchemaVersion == StateSchemaVersion {
			break
		}
		migrate, ok := stateMigrations[probe.SchemaVersion]
		if !ok {
			return State{}, &VersionError{Got: probe.SchemaVersion}
		}
		migrated, err := migrate(raw)
		if err != nil {
			return State{}, fmt.Errorf("state: migrate state.json from version %d: %w", probe.SchemaVersion, err)
		}
		raw = migrated
	}

	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, fmt.Errorf("state: decode state.json: %w", err)
	}
	return st, nil
}

// SaveState atomically writes st to path as indented JSON. See
// atomicWriteFile for the durability guarantee this provides.
func SaveState(path string, st State) error {
	return writeJSON(path, st, filePerm)
}
