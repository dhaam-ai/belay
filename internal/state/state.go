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
// registered for it. A version above the current one was written by a
// newer belay, so its error says to upgrade, as a *CapabilityError does.
//
// Both are at version 2, and the one registered migration takes either file
// from version 1, which every release up to v0.1.4 wrote, to version 2 (see
// migrateFromVersion1). Version 2 adds only the requires list described
// below; every other field keeps its shape and meaning. The bump exists
// because those releases accept version 1 and nothing else, and never read
// requires: a run this belay has saved is one they refuse, rather than one
// they resume after dropping whatever they cannot read. The cost is that
// once this belay has saved a run, including one a released belay started,
// no released belay can resume it.
//
// # Capabilities
//
// Both documents also carry a requires list: the names of the capabilities
// the run depends on, such as "output.branch" for a run whose output is a
// git branch. A capability is a promise that whatever reads the document
// understands a field that changes what the run does. LoadState and
// LoadManifest refuse a document that requires a capability this binary
// does not know, with a *CapabilityError that names it and says to upgrade.
//
// The list exists because encoding/json ignores keys it does not recognize.
// A belay that predates a control field would decode a run carrying it,
// drop the field without a word, and write the file back without it on its
// next save; the run would then carry on doing something other than what it
// was started to do. Bumping schema_version stops binaries that predate the
// bump, and is the only thing that can, since they already shipped. The
// requires list stops every binary from this one on, without a schema bump
// each time a control field is added. ADR-0011 records the decision.
//
// Each document declares only what its own fields need. state.json and
// manifest.json are loaded and rewritten independently — Store.ApplyPatch
// reads and saves state.json without opening the manifest — so neither
// file can rely on the other to protect it. The check runs before the
// document is decoded, so a field in a shape this binary cannot read never
// hides the capability that explains why.
//
// This deliberately does not refuse unknown keys in general
// (json.Decoder.DisallowUnknownFields). A field that only adds information
// is safe for an older binary to drop, and refusing it would turn every
// additive change into a break. Only a field whose loss changes what the
// run does earns a capability name.
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
// version LoadState accepts without a registered migration. It is 2, not 1,
// so that released belay refuses a state.json this one has written; see
// "Schema versioning" in the package documentation.
const StateSchemaVersion = 2

// ErrUnsupportedStateVersion reports a state.json whose schema_version
// LoadState does not know how to read and has no migration registered for.
var ErrUnsupportedStateVersion = errors.New("state: unsupported state schema version")

// VersionError explains which unsupported state.json schema version
// was found. It wraps ErrUnsupportedStateVersion.
type VersionError struct {
	// Got is the schema_version found on disk.
	Got int
}

// Error implements error. For a version above StateSchemaVersion it says to
// upgrade belay, as CapabilityError does: a newer belay wrote the file, and
// running that one is the only fix. A lower version no migration covers
// was not written by any belay this one can point to, so it says only that
// the version is not supported.
func (e *VersionError) Error() string {
	if e.Newer() {
		return fmt.Sprintf("state: state.json schema version %d is newer than this belay reads (%d); upgrade belay to continue this run",
			e.Got, StateSchemaVersion)
	}
	return fmt.Sprintf("state: state.json schema version %d is not supported (want %d)", e.Got, StateSchemaVersion)
}

// Unwrap reports ErrUnsupportedStateVersion.
func (e *VersionError) Unwrap() error { return ErrUnsupportedStateVersion }

// Newer reports whether the version found is above StateSchemaVersion,
// which means a newer belay wrote the document.
func (e *VersionError) Newer() bool { return e.Got > StateSchemaVersion }

// Migration upgrades a raw state.json document exactly one schema
// version forward. A migration for version N is registered at
// stateMigrations[N] and must return a document whose own schema_version
// is N+1; decodeState re-probes and applies migrations repeatedly until it
// reaches StateSchemaVersion.
type Migration func(raw []byte) ([]byte, error)

// stateMigrations holds every registered Migration, keyed by the version
// it upgrades from. Version 1, which every released belay wrote, upgrades
// to 2 by migrateFromVersion1.
var stateMigrations = map[int]Migration{1: migrateFromVersion1}

// State is the blackboard graph nodes read and the dispatcher patches
// after every node — belay's working memory for one run.
//
// The zero State is not a valid on-disk document (SchemaVersion is 0);
// construct one with NewState.
type State struct {
	// SchemaVersion is this document's schema version.
	SchemaVersion int `json:"schema_version"`
	// Requires lists the capabilities this document's fields depend on,
	// sorted and without duplicates; see "Capabilities" in the package
	// documentation. Empty, never nil, for a run that depends on none.
	Requires []string `json:"requires"`
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
	// Policy is the most recent policy gate verdict.
	Policy Policy `json:"policy"`
	// Candidates lists every best-of-N fanout candidate's outcome, once
	// fanout has run. Empty, never nil, for a run with no fanout.
	Candidates []Candidate `json:"candidates"`
	// Winner is the ID of the fanout candidate selected to proceed, or
	// empty if fanout has not run or has not yet selected one.
	Winner string `json:"winner"`
	// Promotion is what the promote node put on the run's branch, or the
	// zero Promotion for a run that has not promoted anything. It is a
	// record, not control state: promote asks the repository what it
	// already holds rather than reading this back.
	Promotion Promotion `json:"promotion"`
	// History is the ordered log of completed node executions. Empty,
	// never nil, for a run that has not completed a node yet.
	History []HistoryEntry `json:"history"`
}

// NewState returns the initial blackboard for a run pursuing goal: schema
// current, everything else zero, with Requires, Candidates and History set
// to non-nil empty slices so a freshly started run serializes
// "requires": [], "candidates": [] and "history": [] rather than null.
func NewState(goal string) State {
	return State{
		SchemaVersion: StateSchemaVersion,
		Requires:      []string{},
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
	// Regenerate is the standing request for a new plan, if a person has
	// asked for one. Record it with State.SetRegeneration, never by
	// assignment, so the run's Requires list stays in step with it.
	Regenerate Regeneration `json:"regenerate"`
}

// Regeneration is a person's request, made while the run is paused at the
// approve gate, for the plan node to write the plan again — with guidance
// about what the last one got wrong.
//
// It is two counters and a sentence rather than a boolean, because the
// same run may ask more than once and the request has to survive a crash
// at any point. The person's side writes Request and Guidance; the plan
// node's side writes Served, and neither ever decrements. Pending is the
// whole protocol: a request that has been made and not yet answered.
//
// Guidance is kept after the request is served. It is what the plan the
// run is now working from was asked for, which is worth as much to whoever
// reads the run afterwards as it was to the node that used it.
type Regeneration struct {
	// Request is the number of the most recent request for a new plan.
	// Zero means nobody has ever asked.
	Request int `json:"request"`
	// Guidance is what the person said the next plan should do
	// differently, in their own words. It may be empty: asking again with
	// nothing to add is a legitimate request.
	Guidance string `json:"guidance"`
	// Served is the number of the last request the plan node answered.
	Served int `json:"served"`
}

// Pending reports whether a request for a new plan is outstanding, which
// is what the approve gate routes on.
//
// It asks whether Request is ahead of Served, not whether the two differ.
// A Served that somehow ran ahead — a hand-edited state.json, or a
// document written by a belay that counts differently — would otherwise
// read as a request nobody made, and the gate would send the run back to
// the plan node on every pass until graph.max_steps ended it.
func (r Regeneration) Pending() bool { return r.Request > r.Served }

// SetRegeneration records r as this run's request for a new plan, and
// keeps Requires in step with it.
//
// It is one call rather than an assignment for the reason Manifest.SetOutput
// is: the field and the capability that protects it are one decision (see
// CapabilityPlanRegenerate). A caller that assigned Plan.Regenerate
// directly would leave a run whose next step depends on a field older
// belays silently drop, and nothing would notice until one of them resumed
// it and wrote code against the plan the person had just asked to replace.
func (s *State) SetRegeneration(r Regeneration) {
	s.Plan.Regenerate = r
	s.requireRegeneration()
}

// requireRegeneration adds CapabilityPlanRegenerate to Requires while a
// request is unserved, and removes it once one is not.
//
// Removing is the unusual half, and it is deliberate. A pin like
// Manifest.Output describes a run for its whole life, so its capability is
// added once and stays. A regeneration request is outstanding only between
// the person making it and the plan node answering it — normally one node
// execution. A requirement that outlived it would make every run that had
// ever asked for a new plan permanently unreadable by an older belay, for
// a field that no longer changes anything the run does.
func (s *State) requireRegeneration() {
	if s.Plan.Regenerate.Pending() {
		s.Requires = addCapability(s.Requires, CapabilityPlanRegenerate)
		return
	}
	s.Requires = dropCapability(s.Requires, CapabilityPlanRegenerate)
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
	// ChangedFiles lists the paths, relative to the workspace root, the run
	// has changed so far: what the agent reported, and, in a run whose
	// workspace can say what really changed (Manifest.Changes), everything
	// the workspace says changed as well, as the write node verified it.
	ChangedFiles []string `json:"changed_files"`
	// Unreported is the part of ChangedFiles the agent changed and never
	// reported, as the write node found it, sorted. It is judged like every
	// other changed file; it is kept apart so that a run cannot hide an
	// edit by leaving it out of its report, and so that every place the run
	// is shown can say which files those were.
	Unreported []string `json:"unreported,omitempty"`
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
	// PolicyViolations is how many policy violations this candidate's own
	// run ended with.
	PolicyViolations int `json:"policy_violations"`
	// Ephemeral mirrors the belay.Workspace.Ephemeral of the workspace
	// the isolator created for this candidate, so whatever cleans up
	// afterwards knows which directories belay may destroy.
	//
	// False is the safe value, and it is deliberately the zero one: a
	// candidate recorded before this field existed, or one an isolator
	// handed back as the source tree unchanged ("no isolation"), reads as
	// not ephemeral, and nothing destroys a tree it cannot prove belay
	// made. The cost of getting it wrong in that direction is a directory
	// left behind; in the other, it is the user's own checkout.
	Ephemeral bool `json:"ephemeral"`
	// Outcome is how this candidate's own run ended, as the short lowercase
	// word its journal's run marker uses: "completed", "failed", "aborted",
	// or "error" for a candidate that never ran at all. Free text by design,
	// like HistoryEntry.Status.
	//
	// Empty means no verdict was recorded — which is every candidate a belay
	// that only prepared workspaces wrote — and join reads that as eligible,
	// so nothing about an existing run's blackboard changes. A candidate that
	// does record one is eligible only when it says "completed": tests that
	// passed in a run whose quality gate then failed, and whose fix loop gave
	// up, would otherwise promote code a single run would have refused.
	Outcome string `json:"outcome"`
	// RunDir is where this candidate's own run lives, relative to the parent
	// run directory ("candidate-runs/cand-01"). It is relative because a run
	// directory can be moved or copied, and a reader that has the parent has
	// the candidate.
	RunDir string `json:"run_dir"`
	// SpentUSD is what this candidate's run cost, read from its own journal.
	SpentUSD float64 `json:"spent_usd"`
	// Estimated reports whether any part of SpentUSD was estimated rather
	// than reported — an attempt a crash left open is charged the largest
	// step its run has paid for. It is sticky, exactly as everywhere else
	// belay totals money: a figure built even partly from a guess must never
	// be shown as though it were not.
	Estimated bool `json:"estimated"`
	// DurationMS is the sum of this candidate's own node durations in
	// milliseconds, which is what the fastest strategy compares. Summing the
	// node records rather than taking wall-clock time excludes the downtime
	// between a crash and the resume that healed it, which no candidate
	// should be blamed for.
	DurationMS int64 `json:"duration_ms"`
	// Note is a short, human-readable sentence about how this candidate
	// ended, for the cases a reader would otherwise have to open a journal to
	// understand — a refused workspace, a spent envelope. May be empty.
	Note string `json:"note"`
	// ChangedFiles is the candidate's recorded change, from its own
	// blackboard (Code.ChangedFiles), relative to its workspace: what its
	// write step recorded and its policy gate judged. promote commits a
	// winner's work only when its worktree holds nothing beyond it.
	ChangedFiles []string `json:"changed_files,omitempty"`
	// Unreported is the part of ChangedFiles the candidate's agent did not
	// report (Code.Unreported), which promote names in the commit.
	Unreported []string `json:"unreported,omitempty"`
}

// Promotion is what a run's promote node put on its branch: the record a
// person reads afterwards to see what the run produced, and what `belay
// runs` and a pull request are built from.
//
// It is written by the promote node's Patch and never read back by it.
// The node works out what to do by asking the repository what it already
// contains, so a Promotion this binary wrote is a report, and losing it
// costs a reader information rather than changing what a re-run does.
type Promotion struct {
	// Branch is the branch the change was promoted onto.
	Branch string `json:"branch"`
	// Base is the commit the branch started from, so Base..Commit is the
	// run's whole contribution.
	Base string `json:"base"`
	// Commit is the branch's tip after the promotion.
	Commit string `json:"commit"`
	// Candidate is the ID of the fanout candidate that was merged, or
	// empty when the run promoted its own work.
	Candidate string `json:"candidate"`
	// Files is how many files differ between Base and Commit.
	Files int `json:"files"`
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
// ErrUnsupportedStateVersion) rather than guessing at the shape. If the
// document requires a capability this binary does not know, it returns a
// *CapabilityError (wrapping ErrUnknownCapability).
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

	if err := checkRequires("state.json", raw); err != nil {
		return State{}, err
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
