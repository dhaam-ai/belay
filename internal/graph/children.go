package graph

import (
	"context"
	"errors"

	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// This file holds the seam between the node that decides a fanout and the
// machinery that executes one.
//
// The split is deliberate, and it is the same one ADR-0002 draws everywhere
// else in the graph. Running N candidates means N goroutines, N run
// directories and N durable writers, and internal/nodes may start none of
// those: a node is a function of its inputs. Deciding how many candidates
// there are, what each may spend, which ids they carry and what their end
// states mean for the run is a graph decision, and that belongs to the node,
// which stays table-testable because everything it needs arrives through
// RunContext.
//
// So the capability lives here, behind CandidateRunner, and the decision
// stays in the node. The loop learns nothing: it never inspects a Result's
// Next for fanout, and journal.ResolveStart still never guesses the graph's
// shape.
//
// Nothing in the dispatcher builds a CandidateRunner yet, so
// RunContext.CandidateRuns is nil in every run belay performs today and a
// node handed one refuses with ErrNoCandidateRunner. The types are here
// first, and on their own, so the node can be written and table-tested
// against a fake before the pool that really runs candidates exists.

// Errors this seam reports. Callers match them with errors.Is.
var (
	// ErrNoCandidateRunner reports that a node needing candidate runs was
	// executed in a run that has none configured. It is a wiring bug — a
	// run routed to fanout without the dispatcher having been given a
	// candidate registry — and never a user error.
	ErrNoCandidateRunner = errors.New("graph: no candidate runner configured")
	// ErrCandidateRunCorrupt reports a directory under candidate-runs/
	// that belay cannot prove it wrote: a symlink, or a tree with none of
	// the files a claim leaves behind. belay removes what it recognises as
	// its own debris and refuses everything else, because the alternative
	// is deleting a directory somebody else put there.
	ErrCandidateRunCorrupt = errors.New("graph: candidate run directory is not one belay created")
)

// CandidateRunner executes a run's fanout candidates: one full belay run per
// candidate, each with its own journal, blackboard, manifest and budget
// envelope, under the parent run's candidate-runs/ directory.
//
// It is the one capability a node cannot have. The Dispatcher builds one and
// injects it through RunContext.CandidateRuns; the fanout node decides what
// to ask of it and what the answers mean.
//
// Run is given every candidate at once rather than one at a time. How many
// execute together is the runner's to decide (fanout.concurrency), and a
// caller that asked for them one by one could not be given a bounded pool
// without the node learning about goroutines. It returns once every
// candidate has a durable end state and every goroutine it started has
// exited, so a node may act on what it returns without waiting for anything
// else.
//
// An error means the run as a whole could not be carried out — the caller's
// context was cancelled, a candidate could not be claimed. Outcomes may
// still be returned alongside it for the candidates that did end.
type CandidateRunner interface {
	Run(ctx context.Context, specs []CandidateSpec) ([]CandidateOutcome, error)
}

// CandidateSpec is one candidate as the fanout node decided it: everything
// the runner needs, and nothing that would let it make a decision of its
// own.
type CandidateSpec struct {
	// ID is the candidate's identifier, "cand-01". It is derived from the
	// candidate's index alone, so that it is stable across attempts of the
	// fanout node: a re-run after a crash adopts the candidate run this id
	// already names instead of paying for a second one beside it.
	//
	// It is bare rather than run-qualified. Both isolators are already
	// rooted at one run's candidates directory, so two runs cannot
	// collide, and the promote node matches a Belay-Candidate trailer
	// against exactly this value.
	ID string
	// Index is the candidate's 1-based position, and the slot its outcome
	// belongs in however the goroutines happen to finish.
	Index int
	// Source is the tree the candidate's workspace is isolated from: the
	// parent run's workspace.
	Source string
	// Envelope is what this candidate run may spend, as the node
	// partitioned the parent's remaining ceiling. It is persisted in the
	// candidate's own manifest, so it survives a crash and an edit to
	// belay.yaml.
	Envelope state.Envelope
	// Seed is the blackboard the candidate run starts from: the goal and
	// the approved plan, and nothing else (state.SeedCandidateState).
	Seed state.State
	// Plan is plan.md, which the runner copies into the candidate's own
	// artifacts directory as it claims it, because the code node resolves
	// the plan through its own run's Layout.
	Plan []byte
}

// CandidateOutcome is one candidate's end state, as read back from its own
// run. Every field comes from the candidate's own files: the runner reports
// what happened, and the node decides what it means.
type CandidateOutcome struct {
	// ID and Index echo the spec, so an outcome can be placed without
	// depending on the order outcomes arrive in.
	ID    string
	Index int
	// RunDir is where this candidate's run lives, relative to the parent
	// run directory ("candidate-runs/cand-01").
	RunDir string
	// Status is how the candidate's run ended, taken from its journal's
	// run marker rather than from its manifest: the journal is the commit
	// record, and a crash between the two leaves a manifest still saying
	// "running" for a run that completed.
	Status state.RunStatus
	// State is the candidate's final blackboard — its tests, its review,
	// the code it wrote — which is what the node turns into a
	// state.Candidate the join node can compare.
	State state.State
	// Spent is what this candidate's run cost, rebuilt from its own
	// journal. Estimated is set when any part of it was charged for an
	// attempt a crash left open.
	Spent belay.Usage
	// Envelope is the ceiling this candidate actually ran under, read back
	// from its manifest rather than from the spec: an adopted candidate
	// keeps the envelope it was claimed with.
	Envelope state.Envelope
	// Steps is how many node executions the candidate run performed.
	Steps int
	// DurationMS sums the candidate's own node durations, which is what
	// the fastest strategy compares. Summing node records rather than
	// taking wall-clock time excludes the downtime between a crash and the
	// resume that healed it, which no candidate should be blamed for.
	DurationMS int64
	// Workspace is the tree the candidate worked in, as the isolator
	// described it, so promote and cleanup know what they may destroy.
	Workspace belay.Workspace
	// Note is a short, human-readable sentence about how this candidate
	// ended, for a reader who would otherwise have to open its journal.
	Note string
	// Provision is how far this candidate's workspace got, and whether
	// that is final. A refusal is durable and may be settled; see
	// state.Provision.
	Provision state.Provision
	// Err is a fault a resume could fix — a full disk, a git lock, a
	// panic — which is why it is a field rather than the Run error: the
	// other candidates still have end states worth reporting, and a node
	// that settles a Result for this one would make a transient fault
	// permanent. A node seeing it set pauses the run instead.
	Err error
}

// Reclaimer removes whatever a Create that died part-way may have left under
// an id, so that the next Create can succeed.
//
// It is optional and consumer-side: an Isolator that does not implement it
// is used exactly as before, and pkg/belay.Isolator does not change, because
// the compatibility promise it carries is not worth spending on a cleanup
// belay does for itself.
//
// Destroy cannot serve instead. Its contract is "at most once, for a
// Workspace Create returned", and here there is no Workspace at all — the
// process died before Create could return one. Asking Destroy to tolerate
// that would turn a precise contract into a vague one.
type Reclaimer interface {
	Reclaim(ctx context.Context, id string) error
}
