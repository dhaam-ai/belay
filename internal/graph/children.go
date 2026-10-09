package graph

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/filerecord"
	"github.com/dhaam-ai/belay/internal/journal"
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
// The Dispatcher builds one when it is given Options.CandidateRegistry, and
// leaves RunContext.CandidateRuns nil otherwise. internal/cli gives a run
// that fans out the candidates' graph, and every other run none, so a node
// handed a nil runner refuses with ErrNoCandidateRunner rather than running
// candidates for a run that never asked for them. The interface stays the seam the node
// is table-tested against, so the decisions can be tested with no
// filesystem and the pool with no node.

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
// An error means the run as a whole could not be carried out: the caller's
// context was cancelled, or the parent run could not be read to claim
// against. A fault that stopped one candidate is not one of those — it
// belongs on that candidate's CandidateOutcome.Err, because the node has to
// be able to pause the run over it rather than end the run, and because the
// candidates that did end still have end states worth reporting.
//
// Outcomes are returned either way, one per spec and in the order the specs
// came in, each naming the candidate it belongs to. A node places them by
// index, so a set missing the candidate that went wrong would quietly drop
// one the run has already paid for.
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

// defaultPlanArtifact is the name a candidate's plan is filed under when the
// seed blackboard names none. internal/nodes/plan calls it the same thing;
// it is repeated here rather than imported because internal/nodes imports
// this package and not the other way round.
const defaultPlanArtifact = "plan.md"

// candidateRunEntries are the names a candidate run directory may hold. A
// directory holding anything else is not one belay wrote, and the debris of
// a torn claim is only ever removed when every name in it is one of these —
// the alternative being that a directory somebody else put there is deleted
// on the strength of a name collision.
var candidateRunEntries = map[string]bool{
	"manifest.json": true, "state.json": true, "journal.ndjson": true,
	"artifacts": true, "snapshots": true, "nodes": true, "worktree": true,
}

// candidateRunner is the Dispatcher's CandidateRunner: a bounded pool that
// gives each candidate a run of its own and waits for all of them.
//
// # One writer per file
//
// Every durable file a candidate produces belongs to the candidate. Each one
// gets its own state.Store, its own journal and its own Dispatcher, under
// candidate-runs/<id>/, and the pool writes nothing in the parent run at all
// — it only reads the parent's manifest, once, for the run id and the goal.
// That is not an optimisation: internal/journal refuses a node_started
// opened while another is still open, so two candidates sharing one journal
// would corrupt it on the second append and journal.Open would refuse the
// file forever after. Directory nesting makes that unreachable rather than
// merely avoided.
//
// # Claim every candidate before any of them runs
//
// Run works in two phases. It claims all N first — a directory, the seed
// blackboard, the plan, and the manifest written last — in index order, on
// the calling goroutine, and only then starts anything. Three things rest on
// that order:
//
//   - the envelope split is computed once. Nothing later re-divides the
//     parent's remaining ceiling, so no crash, resume or edit to belay.yaml
//     can make the envelopes total more than what was left;
//   - a Ctrl-C finds every candidate durable. One that never reached a
//     goroutine still has a directory and an envelope, so a resume adopts it
//     instead of inventing a new one;
//   - the estimated charge an interrupted attempt is given cannot shrink a
//     later candidate's share, because no share is computed after the first
//     attempt.
//
// A candidate that cannot be claimed stops the phase from completing, and
// then nothing runs. Claiming is cheap and spends nothing, so refusing a
// fanout that cannot be whole costs a person nothing and saves them paying
// for candidates whose results could never be compared with the missing
// one's.
//
// # Adopt or create
//
// A candidate whose run directory already exists is adopted, never started a
// second time. That is what makes the parent's resume correct when it died
// with some candidates finished, some inside a node and some never started:
// the finished ones are read back with no node executed at all, the
// interrupted ones heal their own journals and re-run their own open node,
// and the unprovisioned ones are reclaimed and given a workspace. Ids are
// derived from the index alone, so the directory a resume looks for is the
// one the crashed attempt made.
//
// # What it reports, and what it refuses to decide
//
// The pool reports; the node decides. An outcome carries what the
// candidate's own files say, and CandidateOutcome.Err carries a fault a
// resume could fix — a full disk, a directory belay cannot recognise, a
// panic. Nothing durable is recorded about such a fault and the candidate's
// journal is left as it is, so the node can pause the run and a resume
// retries exactly that provisioning. A refusal the isolator says can never
// work here (belay.ErrUnsupported) is the opposite: it is written into the
// candidate's own manifest and reproduced on every adoption afterwards,
// without the isolator being asked again.
type candidateRunner struct {
	parent      *Dispatcher
	registry    *Registry
	agentFor    func(id string) belay.AgentBackend
	concurrency int
}

// newCandidateRunner returns the runner NewDispatcher injects through
// RunContext. It is built with the Dispatcher rather than with a copy of its
// adapters so that a candidate run is wired from exactly what the parent was
// wired from, and a field added to Options reaches candidates without a
// second place remembering to pass it on.
func newCandidateRunner(
	parent *Dispatcher, registry *Registry, agentFor func(id string) belay.AgentBackend, concurrency int,
) *candidateRunner {
	return &candidateRunner{parent: parent, registry: registry, agentFor: agentFor, concurrency: concurrency}
}

// Run implements CandidateRunner.
//
// It always returns exactly one outcome per spec, in the order the specs
// came in, each carrying that spec's ID and Index — including for a
// candidate that never ran, and including when Run itself reports an error.
// The fanout node places outcomes by index and refuses a set it cannot
// place, because state.Candidates is an absolute set: a set missing the
// candidate that went wrong would silently drop one the run has already paid
// for, and leave join choosing between fewer candidates than really ran.
func (r *candidateRunner) Run(ctx context.Context, specs []CandidateSpec) ([]CandidateOutcome, error) {
	// Identity first, before anything that can fail: every slot names the
	// candidate it belongs to even on a path that gets no further.
	outcomes := make([]CandidateOutcome, len(specs))
	for i, spec := range specs {
		outcomes[i] = CandidateOutcome{ID: spec.ID, Index: spec.Index, Envelope: spec.Envelope}
	}
	if len(specs) == 0 {
		return outcomes, nil
	}
	parent, err := r.parent.store.LoadManifest()
	if err != nil {
		return outcomes, fmt.Errorf("graph: load the parent manifest before claiming candidates: %w", err)
	}

	claims := make([]*candidateClaim, len(specs))
	whole := true
	for i, spec := range specs {
		c, err := r.claim(parent, spec)
		if err != nil {
			outcomes[i].Err = err
			whole = false
			r.parent.logger.Warn("belay/graph: could not claim a candidate run, so none of them runs",
				slog.String("candidate", spec.ID), slog.String("error", err.Error()))
			continue
		}
		claims[i] = c
		outcomes[i].RunDir = c.runDir
	}
	// Nothing runs unless every candidate has been claimed. Claiming is
	// cheap and spends nothing, so refusing a fanout that cannot be whole
	// costs a person nothing, and it saves them paying for candidates whose
	// results could never be compared with the missing one's.
	if !whole {
		return outcomes, nil
	}

	r.drive(ctx, claims, outcomes)

	// Reported only once every goroutine has exited, so a caller acting on
	// the cancellation knows no candidate is still writing to a file.
	if err := ctx.Err(); err != nil {
		return outcomes, fmt.Errorf("graph: the candidate runs were interrupted: %w", err)
	}
	return outcomes, nil
}

// drive runs the claimed candidates, at most concurrency of them at a time,
// and returns once every goroutine it started has exited.
//
// The pool is a fixed set of workers pulling from one channel fed in index
// order, rather than a goroutine per candidate waiting on a semaphore. Two
// things follow from that shape, and both are the point of it: candidates
// are admitted in index order, and a candidate that has not been admitted
// has no goroutine of its own, so nothing about it has started. A worker
// checks the cancellation again before it admits one, because the feeder
// stopping and the cancellation arriving are ready at the same moment.
//
// Each worker writes into outcomes[i], a slot no other goroutine touches, so
// the result is in index order however the candidates finish and no lock is
// needed to build it. Waiting for every worker before returning is what lets
// a node act on the outcomes: no candidate can still be writing to a file
// once Run has returned.
func (r *candidateRunner) drive(ctx context.Context, claims []*candidateClaim, outcomes []CandidateOutcome) {
	workers := min(max(r.concurrency, 1), len(claims))
	next := make(chan int)

	go func() {
		defer close(next)
		for i := range claims {
			select {
			case next <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				// The feeder stops sending on a cancellation, and the
				// two are ready at the same moment when a worker is
				// between candidates, so which one the runtime picks is
				// a coin toss. This makes the answer the same either
				// way: a candidate the cancellation reached first is
				// left claimed and unprovisioned, which is what a
				// resume adopts. Admitting it would copy a whole tree
				// for a workspace nobody asked for.
				if ctx.Err() != nil {
					continue
				}
				r.one(ctx, claims[i], &outcomes[i])
			}
		}()
	}
	wg.Wait()
}

// one takes a single candidate from wherever it is to wherever it can get,
// on its own goroutine.
func (r *candidateRunner) one(ctx context.Context, c *candidateClaim, out *CandidateOutcome) {
	// A panic is recovered into the outcome rather than allowed to end the
	// process, so a bug in one candidate's node does not cost its siblings
	// work that has already been paid for. It becomes a fault like any
	// other: nothing durable is written about it, the candidate's journal
	// is left with its node_started open, and the next attempt heals it and
	// runs that node again.
	//
	// The isolation is honest but partial, and ADR-0019 says so. It covers
	// a panic on this goroutine. A panic in a goroutine a node itself
	// starts — an exec pipe, a backend stream — still ends the process,
	// exactly as it does for a single run today.
	defer func() {
		if p := recover(); p != nil {
			r.parent.logger.Error("belay/graph: a candidate run panicked",
				slog.String("candidate", c.spec.ID), slog.Any("panic", p),
				slog.String("stack", string(debug.Stack())))
			out.Err = fmt.Errorf("graph: candidate %q panicked: %v", c.spec.ID, p)
		}
	}()

	switch c.provision {
	case state.ProvisionRefused:
		// Durable, and reproduced from the manifest with no isolator call:
		// the conditions that produced it are ones no resume changes.
		out.Provision, out.Note = state.ProvisionRefused, c.note
		return
	case state.ProvisionOK:
		// Adopted with its workspace already made. Making a second one
		// would hand the candidate an empty tree and throw away the work
		// its own run has already paid for.
	default:
		if !r.makeWorkspace(ctx, c, out) {
			return
		}
	}
	r.resume(ctx, c, out)
}

// makeWorkspace provisions the candidate's workspace and records it in the
// candidate's own manifest, and reports whether the candidate can go on to
// run.
func (r *candidateRunner) makeWorkspace(ctx context.Context, c *candidateClaim, out *CandidateOutcome) bool {
	iso := r.parent.isolator
	if iso == nil {
		out.Err = fmt.Errorf("graph: candidate %q has no isolator to make a workspace with", c.spec.ID)
		return false
	}

	// Strictly before Create, and only for a candidate whose run directory
	// was already there: both isolators refuse to create over the debris a
	// Create killed part-way leaves behind (a non-empty destination, a
	// worktree git still has registered). A candidate claimed in this call
	// has nothing to reclaim, because the manifest is written before any
	// Create is attempted.
	//
	// Reclaim and not Destroy. Destroy's contract is "at most once, for a
	// Workspace Create returned", and here Create never returned one, so
	// asking Destroy to cope would turn a precise contract into a vague
	// one. It is optional and consumer-side: an isolator without it leaves
	// the candidate as a fault naming the directory, which pauses the
	// fanout rather than losing the candidate.
	if c.adopted {
		reclaimer, ok := iso.(Reclaimer)
		if !ok {
			out.Err = fmt.Errorf(
				"graph: candidate %q was claimed but never given a workspace, and %T cannot reclaim what a "+
					"half-finished one left behind; remove %s and resume",
				c.spec.ID, iso, c.layout.RunDir())
			return false
		}
		if err := reclaimer.Reclaim(ctx, c.spec.ID); err != nil {
			out.Err = fmt.Errorf("graph: reclaim candidate %q before making its workspace again: %w", c.spec.ID, err)
			return false
		}
	}

	ws, err := iso.Create(ctx, c.spec.Source, c.spec.ID)
	switch {
	case errors.Is(err, belay.ErrUnsupported):
		// The isolator says this can never work here. pkg/belay defines
		// exactly one marker for that and this is it: "well-formed, but
		// this implementation cannot do it", as opposed to a source that
		// is merely dirty right now, which a person can commit and resume.
		// Nothing more specific is matched, because internal/graph has no
		// build tag and both isolator packages are unix-only — and erring
		// towards "retry on resume" loses a candidate to nothing worse
		// than a pause somebody can see.
		r.refuse(c, out, err)
		return false
	case err != nil:
		out.Err = fmt.Errorf("graph: make a workspace for candidate %q: %w", c.spec.ID, err)
		return false
	case ws.Dir == "":
		out.Err = fmt.Errorf("graph: the isolator returned no directory for candidate %q", c.spec.ID)
		return false
	}

	// A directory copy has no git to ask what the candidate changed, so the
	// copy itself is recorded before the candidate's agent runs. It is taken
	// before the workspace is recorded as made: a crash in between leaves a
	// claim that is reclaimed and made again on resume, with a new record,
	// never a candidate judged against no record at all.
	if pin := c.man.Changes; pin != nil && pin.From == state.ChangesFromRecord {
		rec, err := filerecord.Take(ctx, ws.Dir, nil)
		if err == nil {
			err = rec.Save(c.layout.FileRecordPath())
		}
		if err != nil {
			out.Err = fmt.Errorf("graph: record the workspace of candidate %q: %w", c.spec.ID, err)
			return false
		}
		// The candidate's own manifest says its run directory holds the
		// record, which the parent's pin could not: the parent takes none.
		recorded := *pin
		recorded.Record = true
		c.man.SetChanges(recorded)
	}

	c.man.Workspace = ws.Dir
	block := *c.man.Candidate
	block.Provision, block.Workspace = state.ProvisionOK, ws
	c.man.SetCandidate(block)
	if err := state.SaveManifest(c.layout.ManifestPath(), c.man); err != nil {
		out.Err = fmt.Errorf("graph: record the workspace of candidate %q: %w", c.spec.ID, err)
		return false
	}

	c.workspace, c.provision = ws, state.ProvisionOK
	out.Workspace, out.Provision = ws, state.ProvisionOK
	return true
}

// refuse records a durable provisioning refusal in the candidate's own
// manifest, so that every later adoption reproduces it without asking the
// isolator again. A manifest it cannot write leaves the candidate claimed
// and unprovisioned, which the next attempt retries — the safe direction.
func (r *candidateRunner) refuse(c *candidateClaim, out *CandidateOutcome, cause error) {
	note := r.parent.note(cause.Error())
	block := *c.man.Candidate
	block.Provision, block.ProvisionNote = state.ProvisionRefused, note
	c.man.SetCandidate(block)
	if err := state.SaveManifest(c.layout.ManifestPath(), c.man); err != nil {
		out.Err = fmt.Errorf("graph: record the refusal of candidate %q: %w", c.spec.ID, err)
		return
	}
	c.provision, c.note = state.ProvisionRefused, note
	out.Provision, out.Note = state.ProvisionRefused, note
	r.parent.logger.Warn("belay/graph: a candidate's workspace was refused for good",
		slog.String("candidate", c.spec.ID), slog.String("reason", note))
}

// resume drives the candidate's own run to wherever it gets, then reads its
// end state back out of its own files.
//
// Resume rather than Run: a candidate adopted at a pause — one that met its
// envelope, and whose envelope a raised parent ceiling has since lifted —
// has to be able to continue, and Run refuses a paused run by design.
func (r *candidateRunner) resume(ctx context.Context, c *candidateClaim, out *CandidateOutcome) {
	layout, err := c.layout.WithWorkTree(c.workspace.Dir)
	if err != nil {
		out.Err = fmt.Errorf("graph: point candidate %q at its workspace: %w", c.spec.ID, err)
		return
	}
	store := state.NewStore(layout)
	child, err := NewDispatcher(r.childOptions(c, store))
	if err != nil {
		out.Err = fmt.Errorf("graph: build the dispatcher for candidate %q: %w", c.spec.ID, err)
		return
	}

	_, runErr := child.Resume(ctx)
	r.collect(c, store, out, runErr)
}

// childOptions is how a candidate run is wired, and every line of it is a
// decision:
//
//   - Store, and therefore Layout, journal and ledger, are the candidate's
//     own — the one-writer-per-file rule;
//   - Entry is code, because the candidate's journal is empty and
//     journal.FirstNode is "plan", a node its registry does not have;
//   - Config is the parent's with the budget replaced by this candidate's
//     persisted envelope, enforced as abort. budget.on_exceed: warn is
//     deliberately not honoured inside a candidate: a warning per candidate
//     is N runs with no ceiling at all;
//   - PauseOnCancel is always on, whatever the parent was given. One Ctrl-C
//     has to leave every candidate resumable, and a candidate ended as
//     aborted could never be continued — the money its run had already
//     spent would buy nothing. The parent's own setting is not consulted,
//     because a candidate has no other way to be resumed: nothing but its
//     parent's fanout node ever reaches it;
//   - PauseAfter is zero, explicitly. It pauses a run part-way through a
//     call, which is how a fork runs exactly one node; a candidate paused
//     like that would be neither finished nor resumable, since the fanout
//     node that adopts it would find a run its own registry cannot
//     continue past;
//   - Isolator and VCS are nil: a candidate never fans out again, and it
//     commits nothing — promote commits the winner's work;
//   - Changes is the parent's factory, so a candidate's write node and
//     policy gate read what its own workspace changed;
//   - the agent, runner, linter and reviewer are shared, which pkg/belay
//     already requires to be safe under fanout, with CandidateAgent the one
//     hook for a backend that needs a per-candidate view.
func (r *candidateRunner) childOptions(c *candidateClaim, store *state.Store) Options {
	p := r.parent
	agent := p.agent
	if r.agentFor != nil {
		agent = r.agentFor(c.spec.ID)
	}
	return Options{
		Store:         store,
		Registry:      r.registry,
		Config:        candidateConfig(p.cfg, c.envelope),
		Entry:         NodeCode,
		Logger:        p.logger.With(slog.String("candidate", c.spec.ID)),
		Now:           p.now,
		Redact:        p.redact,
		PauseOnCancel: true,
		PauseAfter:    0,
		Fingerprint:   p.fingerprint,
		Agent:         agent,
		Runner:        p.runner,
		Linter:        p.linter,
		Reviewer:      p.reviewer,
		// The parent's factory, called with this candidate's own manifest
		// and Layout, so the candidate reads its own workspace's change.
		Changes: p.changes,
	}
}

// candidateConfig is the parent's configuration with this candidate's
// envelope as its ceiling. Everything else — the graph's limits, the agent,
// the test command — is the run's, because a candidate is doing the run's
// work and only the money is divided.
func candidateConfig(parent config.Config, env state.Envelope) config.Config {
	parent.Budget = config.Budget{
		MaxUSD: env.LimitUSD, MaxTokens: env.LimitTokens, OnExceed: config.OnExceedAbort,
	}
	return parent
}

// collect fills out from the candidate's own files. Status comes from the
// journal's run marker and never from the manifest: the journal is the
// commit record, and a kill between the marker and SaveManifest leaves a
// manifest still saying "running" for a run that completed.
//
// runErr is what the child dispatcher returned. It is not itself a verdict —
// a candidate whose fix loop gave up returns ErrNodeFailed and is perfectly
// durable — so it is reported only when the journal shows the run did not
// reach an end state at all, which is the one case a resume has to retry.
func (r *candidateRunner) collect(c *candidateClaim, store *state.Store, out *CandidateOutcome, runErr error) {
	read, err := journal.ReadFile(c.layout.JournalPath())
	if err != nil {
		out.Err = errors.Join(runErr, fmt.Errorf("graph: read the journal of candidate %q: %w", c.spec.ID, err))
		return
	}
	start, err := journal.ResolveStart(read.Records)
	if err != nil {
		out.Err = errors.Join(runErr, fmt.Errorf("graph: resolve the end state of candidate %q: %w", c.spec.ID, err))
		return
	}
	out.Status = runStatusFor(start.State)

	for _, rec := range read.Records {
		if rec.Event == journal.EventNodeFinished {
			out.DurationMS += rec.DurationMS
		}
		if note, ended := runMarkerNote(rec); ended {
			out.Note = note
		}
	}

	man, err := store.LoadManifest()
	if err != nil {
		out.Err = errors.Join(runErr, fmt.Errorf("graph: read the manifest of candidate %q: %w", c.spec.ID, err))
		return
	}
	out.Steps = man.Step
	out.Spent = rebuildLedger(read.Records, man.Budget).Total()
	if man.Candidate != nil {
		out.Envelope = man.Candidate.Envelope
		out.Provision = man.Candidate.Provision
		out.Workspace = man.Candidate.Workspace
	}

	st, err := store.LoadState()
	if err != nil {
		out.Err = errors.Join(runErr, fmt.Errorf("graph: read the blackboard of candidate %q: %w", c.spec.ID, err))
		return
	}
	out.State = st

	if start.State == journal.RunStateRunnable {
		// The run stopped without recording how it ended, which is the one
		// thing this runner may not settle: something machine-shaped got in
		// the way, and a resume has to try it again.
		if runErr == nil {
			runErr = fmt.Errorf("graph: candidate %q stopped at %q without reaching an end state", c.spec.ID, start.Node)
		}
		out.Err = runErr
	}
}

// runMarkerNote reports the note of a run-level record that ends a run, and
// whether rec is one. The last of them is how a candidate ended, in a
// sentence a person can read without opening the journal — and it is the
// same sentence however many attempts it took, which is what keeps a
// resumed fanout's Result equal to the one it would have settled first time.
func runMarkerNote(rec journal.Record) (string, bool) {
	switch rec.Event {
	case journal.EventRunCompleted, journal.EventRunFailed,
		journal.EventRunAborted, journal.EventRunPaused:
		return rec.Note, true
	default:
		return "", false
	}
}

// candidateClaim is one candidate's run directory as the claim left it:
// where it lives, what it may spend, and how far it has got.
type candidateClaim struct {
	spec   CandidateSpec
	layout state.Layout
	man    state.Manifest
	// runDir is the run directory relative to the parent's, slash
	// separated, which is what an outcome and the blackboard record.
	runDir    string
	envelope  state.Envelope
	provision state.Provision
	workspace belay.Workspace
	// note is why a refused candidate was refused, as its manifest records
	// it, so an adoption reports the same sentence the first attempt did.
	note string
	// adopted reports that the run directory was already there when this
	// call found it, which is what decides whether debris a Create that
	// died part-way left has to be reclaimed before another is attempted.
	adopted bool
}

// claim makes sure spec has a run directory, either by writing one or by
// adopting the one a previous attempt of the fanout node left, and returns
// what it found.
//
// Adopt-or-create is what makes the parent's resume correct. Ids are derived
// from the index alone, so the directory a resume looks for is the one the
// crashed attempt made, and a candidate is never started a second time
// beside the one that is already there with money spent in it.
func (r *candidateRunner) claim(parent state.Manifest, spec CandidateSpec) (*candidateClaim, error) {
	layout, err := state.NewCandidateRunLayout(r.parent.store.Layout(), spec.ID)
	if err != nil {
		return nil, fmt.Errorf("graph: candidate %q: %w", spec.ID, err)
	}
	rel, err := filepath.Rel(r.parent.store.Layout().RunDir(), layout.RunDir())
	if err != nil {
		return nil, fmt.Errorf("graph: locate candidate %q under its parent: %w", spec.ID, err)
	}
	runDir := filepath.ToSlash(rel)

	info, err := os.Lstat(layout.RunDir())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return r.claimFresh(parent, spec, layout, runDir)
	case err != nil:
		return nil, fmt.Errorf("graph: inspect the run directory of candidate %q: %w", spec.ID, err)
	case info.Mode()&fs.ModeSymlink != 0:
		// Lstat, not Stat: a symlink pointing anywhere at all would
		// otherwise have an agent's workspace written through it.
		return nil, fmt.Errorf("%w: %s is a symlink", ErrCandidateRunCorrupt, layout.RunDir())
	case !info.IsDir():
		return nil, fmt.Errorf("%w: %s is not a directory", ErrCandidateRunCorrupt, layout.RunDir())
	}

	if _, err := os.Stat(layout.ManifestPath()); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("graph: inspect the manifest of candidate %q: %w", spec.ID, err)
		}
		// No manifest, and a claim writes it last: this is the debris of a
		// claim a crash cut short, so it is cleared and made again rather
		// than written over. Whatever it holds was left mid-write, and a
		// snapshot or an artifact from it would be read by the new run as
		// if the new run had produced it.
		if err := removeTornClaim(layout.RunDir()); err != nil {
			return nil, err
		}
		return r.claimFresh(parent, spec, layout, runDir)
	}
	return r.adopt(spec, layout, runDir)
}

// removeTornClaim deletes the debris of a claim a crash cut short, having
// first established that the directory holds nothing but the files a
// candidate run is made of.
//
// The containment rule is deliberately narrow, and it is three checks deep.
// state.NewCandidateRunLayout has already established that the path is one
// safe segment under candidate-runs/; the caller has established that it is
// a real directory and not a symlink; and this establishes that belay wrote
// it, for which the names inside are the only evidence there is. Anything
// else is ErrCandidateRunCorrupt and is left exactly where it is.
func removeTornClaim(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("graph: inspect the torn candidate run at %s: %w", dir, err)
	}
	for _, e := range entries {
		if !candidateRunEntries[e.Name()] {
			return fmt.Errorf("%w: %s holds %q, which no candidate run does",
				ErrCandidateRunCorrupt, dir, e.Name())
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("graph: remove the torn candidate run at %s: %w", dir, err)
	}
	return nil
}

// adopt takes over the run directory a previous attempt of the fanout node
// left, rather than starting the candidate a second time beside it.
//
// Nothing in the directory is rewritten but the envelope. The blackboard
// holds what that candidate reached, the journal is what its end state is
// read from and what its own dispatcher heals, and the plan is part of the
// record of what it worked from.
//
// The envelope it continues with is the larger of the one it was claimed
// with and the share the node has just computed. It is never lowered: part
// of it may already be spent, so taking it back returns no money, and a
// candidate that stopped at its own envelope has to be continuable by
// raising budget.max_usd and resuming, which is the settled rule for every
// limit belay enforces. It may rise, and it is written back when it does, so
// that the next adoption starts from the raised figure and the candidate's
// own ledger enforces it. See budget.PartitionAtLeast, which states the same
// rule for the arithmetic that produces the share.
//
// The cost, and ADR-0019 states it: lowering budget.max_usd mid-fanout does
// not claw back what was already reserved.
func (r *candidateRunner) adopt(spec CandidateSpec, layout state.Layout, runDir string) (*candidateClaim, error) {
	man, err := state.LoadManifest(layout.ManifestPath())
	if err != nil {
		return nil, fmt.Errorf("graph: read the manifest of candidate %q: %w", spec.ID, err)
	}
	if man.Candidate == nil {
		return nil, fmt.Errorf("%w: %s holds a manifest that is not a candidate run's",
			ErrCandidateRunCorrupt, layout.RunDir())
	}

	envelope := raiseEnvelope(man.Candidate.Envelope, spec.Envelope)
	if envelope != man.Candidate.Envelope {
		block := *man.Candidate
		block.Envelope = envelope
		man.SetCandidate(block)
		man.Budget.LimitUSD = envelope.LimitUSD
		if err := state.SaveManifest(layout.ManifestPath(), man); err != nil {
			return nil, fmt.Errorf("graph: raise the envelope of candidate %q: %w", spec.ID, err)
		}
		r.parent.logger.Info("belay/graph: raised an adopted candidate's envelope to the current share",
			slog.String("candidate", spec.ID), slog.Float64("envelope_usd", envelope.LimitUSD))
	}

	r.parent.logger.Info("belay/graph: adopted a candidate run a previous attempt left",
		slog.String("candidate", spec.ID), slog.String("provision", string(man.Candidate.Provision)))
	return &candidateClaim{
		spec: spec, layout: layout, man: man, runDir: runDir,
		envelope:  envelope,
		provision: man.Candidate.Provision,
		workspace: man.Candidate.Workspace,
		note:      man.Candidate.ProvisionNote,
		adopted:   true,
	}, nil
}

// claimFresh writes one candidate's run directory: its blackboard, its copy
// of the plan, and its manifest last.
//
// The manifest is written LAST, the rule createRun follows and for the same
// reason: a directory without one is a torn claim and never a run, so the
// window a crash can land in leaves debris that is recognisable rather than
// a half-built run that looks finished.
func (r *candidateRunner) claimFresh(
	parent state.Manifest, spec CandidateSpec, layout state.Layout, runDir string,
) (*candidateClaim, error) {
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		return nil, fmt.Errorf("graph: create the run directory of candidate %q: %w", spec.ID, err)
	}
	if err := state.SaveState(layout.StatePath(), spec.Seed); err != nil {
		return nil, fmt.Errorf("graph: write the blackboard of candidate %q: %w", spec.ID, err)
	}
	if err := writePlan(layout, spec); err != nil {
		return nil, err
	}

	goal := spec.Seed.Goal
	if goal == "" {
		goal = parent.Goal
	}
	// RunID is <parent>-<cand>, and it lives in the manifest rather than in
	// the Layout, whose RunID is the directory name and so the bare
	// candidate id. Nothing in the graph reads the Layout's; runContext
	// takes the manifest's.
	man := state.NewManifest(
		r.parent.now().UTC(), parent.RunID+"-"+spec.ID, spec.Source, goal,
		candidateConfig(r.parent.cfg, spec.Envelope))
	man.SetCandidate(state.CandidateRun{
		ParentRunID: parent.RunID, ID: spec.ID, Index: spec.Index, Envelope: spec.Envelope,
	})
	// A candidate is held to the guardrails its parent was started under.
	// Its nodes read the policy from RunContext.Policy, which runContext
	// fills from the manifest of the run being driven -- this one, not the
	// parent's -- so a pin left behind here is a candidate whose policy gate
	// finds no baseline and passes every change it makes, and a join that
	// never has a violation to refuse. Through SetPolicy, so the capability
	// travels with it: a belay that cannot hold a candidate to a policy must
	// refuse to resume one rather than walk its gate unguarded.
	//
	// The parent's baseline is the right one. A fanout run's own nodes never
	// change its workspace -- its work is the candidates' -- so each
	// candidate is made from the tree the baseline surveyed. The exception
	// is a person editing their own checkout while an in-place run waits at
	// the approve gate, which a directory copy then carries into every
	// candidate; that is the copy's known limit, not this pin's, and a
	// branch run is immune because its candidates start from a commit.
	if parent.Policy != nil {
		man.SetPolicy(*parent.Policy)
	}
	// Where the candidate's change is read from, decided by the parent when
	// it was created for exactly this: a fanout run's own nodes never change
	// its workspace, so the pin describes the workspaces its candidates get.
	// A candidate on a git worktree is measured from the parent's base
	// commit; one in a directory copy, from the record makeWorkspace takes.
	if parent.Changes != nil {
		man.SetChanges(*parent.Changes)
	}
	if err := state.SaveManifest(layout.ManifestPath(), man); err != nil {
		return nil, fmt.Errorf("graph: write the manifest of candidate %q: %w", spec.ID, err)
	}

	r.parent.logger.Info("belay/graph: claimed a candidate run",
		slog.String("candidate", spec.ID), slog.Float64("envelope_usd", spec.Envelope.LimitUSD))
	return &candidateClaim{
		spec: spec, layout: layout, man: man, runDir: runDir, envelope: spec.Envelope,
	}, nil
}

// raiseEnvelope returns the envelope an adopted candidate continues with:
// the higher of what it was promised and what it would be given now, in each
// dimension separately.
//
// Zero is uncapped, here as everywhere else belay reads a ceiling, so it
// counts as the higher of the two rather than the lower — a candidate
// claimed while budget.max_tokens was unset is not retroactively capped by
// one set afterwards.
//
// It is a pure function of the persisted value and the share, and the share
// is itself a pure function of the live ceiling, the parent's spend before
// the fanout and N — never of what any sibling actually spent, or of the
// order the goroutines ran in. So two resumes of the same run with the same
// configuration produce the same envelopes, and a fanout a crash interrupted
// converges rather than drifting.
func raiseEnvelope(persisted, share state.Envelope) state.Envelope {
	var env state.Envelope
	if persisted.LimitUSD > 0 && share.LimitUSD > 0 {
		env.LimitUSD = max(persisted.LimitUSD, share.LimitUSD)
	}
	if persisted.LimitTokens > 0 && share.LimitTokens > 0 {
		env.LimitTokens = max(persisted.LimitTokens, share.LimitTokens)
	}
	return env
}

// writePlan copies the plan into the candidate's own artifacts directory, so
// that the code node — which resolves the plan through its own run's Layout
// — finds it there. It is written once, when the candidate is claimed, and
// never rewritten afterwards: the plan a candidate worked from is part of
// the record of what it did.
func writePlan(layout state.Layout, spec CandidateSpec) error {
	if len(spec.Plan) == 0 {
		return nil
	}
	name := path.Base(path.Clean(filepath.ToSlash(strings.TrimSpace(spec.Seed.Plan.Path))))
	if name == "" || name == "." || name == "/" {
		name = defaultPlanArtifact
	}
	abs, err := layout.ArtifactPath(name)
	if err != nil {
		return fmt.Errorf("graph: candidate %q: %w", spec.ID, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return fmt.Errorf("graph: create the artifacts directory of candidate %q: %w", spec.ID, err)
	}
	if err := os.WriteFile(abs, spec.Plan, 0o600); err != nil {
		return fmt.Errorf("graph: write the plan of candidate %q: %w", spec.ID, err)
	}
	return nil
}
