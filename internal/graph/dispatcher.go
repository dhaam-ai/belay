package graph

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/dhaam-ai/belay/internal/budget"
	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/procgroup"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/internal/vcs"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Process exit codes. The CLI maps an Outcome.ExitCode straight onto
// os.Exit, so these are part of belay's user-visible contract and must not
// be renumbered.
const (
	// ExitOK reports a run that reached End, or one that paused cleanly and
	// can be continued with `belay resume`. Pausing is a normal, successful
	// outcome of an invocation — the run has not failed, it is waiting.
	ExitOK = 0
	// ExitFailed reports a run that ended in failure: a node returned
	// StatusFailed, returned a non-nil error, or returned a Result the
	// dispatcher could not act on.
	ExitFailed = 1
	// ExitAborted reports a run stopped by one of the dispatcher's own
	// limits rather than by the work itself: the budget ceiling, the
	// MaxSteps cap, a node timeout, or cancellation of the caller's
	// context. Every one of them pauses the run rather than ending it, and
	// they exit with this code all the same: the invocation still stopped
	// short of where the graph would have taken it, and a script must be
	// able to tell that from a pause the graph chose, such as the approval
	// gate. Outcome.Status says the run can be resumed, and the note says
	// which setting to raise first.
	ExitAborted = 3
	// ExitPaused reports that a plain Run refused to advance a run whose
	// journal ends in run_paused. It is distinct from ExitOK because the
	// caller did nothing wrong and no work happened — the correct next
	// command is `belay resume`.
	ExitPaused = 4
)

// Errors the dispatcher returns. Every one is matchable with errors.Is so the
// CLI can explain the failure without parsing a message.
var (
	// ErrRunPaused means Run was called on a run whose journal ends in
	// run_paused. Only Resume may advance past that point.
	ErrRunPaused = errors.New("graph: run is paused; use resume to continue it")
	// ErrRunEnded means Run or Resume was called on a run that already
	// reached a terminal failure or abort. It is not resumable: start a new
	// run instead.
	ErrRunEnded = errors.New("graph: run has already ended")
	// ErrMaxSteps means the run performed config.Graph.MaxSteps node
	// executions without reaching End. It is the guard against a fix loop
	// that never converges.
	ErrMaxSteps = errors.New("graph: max steps exceeded")
	// ErrNodeTimeout means a single node exceeded config.Graph.NodeTimeout.
	ErrNodeTimeout = errors.New("graph: node timeout exceeded")
	// ErrNodeFailed means a node ended the run in failure — by returning
	// StatusFailed, by returning a non-nil error, or by returning a Result
	// that failed Validate.
	ErrNodeFailed = errors.New("graph: node failed")
	// ErrNodeAborted means a node returned StatusAborted, ending the run.
	ErrNodeAborted = errors.New("graph: node aborted")
	// ErrNoResumePoint means the journal resolved to an empty next node.
	// It is the signature of a crash between a terminal node_finished record
	// and the run-level marker that should have followed it.
	ErrNoResumePoint = errors.New("graph: journal resolved to no next node")
	// ErrInvalidOptions means NewDispatcher was given an unusable Options.
	ErrInvalidOptions = errors.New("graph: invalid dispatcher options")
	// ErrInterrupted means the caller cancelled the run and
	// Options.PauseOnCancel paused it rather than ending it. It wraps
	// context.Canceled as well. The run is resumable: Resume re-runs the
	// node that was interrupted.
	ErrInterrupted = errors.New("graph: run interrupted; resuming re-runs the interrupted node")
)

// nodeErrorNotePrefix marks a node_finished note that came from a node
// returning a non-nil error rather than from a node returning StatusFailed
// with a verdict of its own.
//
// Both end the run with StatusFailed, because from the run's point of view
// both are terminal. They are not the same event, though: a clean
// StatusFailed means the node did its job and the answer was "no", while an
// error means the node could not reach an answer at all — a missing
// toolchain, an unreachable adapter. Anything reading the journal back (the
// timeline, an operator, a bug report) has to be able to tell a broken
// machine from a failing build, so the distinction is carried in the note
// under this fixed, greppable prefix.
const nodeErrorNotePrefix = "node error: "

// invalidResultNotePrefix marks a node_finished note produced by a node whose
// Result failed Validate. That is always a wiring bug in the node, never a
// user error, and it is recorded distinctly so it is not mistaken for one.
const invalidResultNotePrefix = "invalid result: "

// maxNoteRunes bounds a note the dispatcher composes itself, so a pathological
// adapter error cannot write a megabyte into every journal line.
const maxNoteRunes = 500

// Outcome is the result of one Run or Resume call: everything the CLI needs to
// report what happened and pick an exit status, with no second query.
type Outcome struct {
	// Status is the run's lifecycle state as persisted to the manifest.
	Status state.RunStatus
	// ExitCode is the process exit code for this outcome, one of ExitOK,
	// ExitFailed, ExitAborted or ExitPaused.
	ExitCode int
	// Node is the last node executed, or — when nothing ran — the node the
	// run would have started at.
	Node string
	// Steps is the number of node executions this call performed. It is not
	// the run's cumulative step count, which lives in the manifest.
	Steps int
	// Usage is the run's cumulative usage, including everything spent before
	// a resume, as rebuilt from the journal.
	Usage belay.Usage
	// Note is a short, redacted, human-readable reason, matching the note
	// written to the journal.
	Note string
}

// Options configures a Dispatcher. Adapters are injected rather than
// constructed here: which agent, runner, linter, reviewer and isolator a run
// uses is a wiring decision that belongs to the caller assembling the run, not
// to the loop that drives it.
type Options struct {
	// Store persists manifest.json and state.json. Required.
	Store *state.Store
	// Registry resolves node names to implementations. Required.
	Registry *Registry
	// Config is the run's resolved configuration, supplying the graph's
	// MaxSteps, NodeTimeout and budget ceilings.
	Config config.Config
	// Entry is the node a run whose journal is empty starts at. It
	// defaults to journal.FirstNode, which is "plan", so a caller that
	// never sets it sees the run it always did.
	//
	// It exists for a run whose graph does not begin at the beginning: a
	// fanout candidate runs code, write, test, fix and review over a plan
	// its parent has already had approved, and a registry without a plan
	// node would end such a run on its first step with an unknown node.
	//
	// It is ignored the moment the journal holds any record at all.
	// Where a run with history resumes is journal.ResolveStart's decision
	// and nothing else's, because the journal is the only thing that knows
	// which node died and at which attempt; applying Entry over it would
	// re-run the entry node on every resume, paying again for work the
	// journal says is done.
	Entry string
	// Logger receives the dispatcher's structured log. Defaults to
	// slog.Default.
	Logger *slog.Logger
	// Now supplies the clock used for manifest timestamps, so a test can
	// assert exact values. Defaults to time.Now.
	Now func() time.Time
	// Redact sanitizes text the dispatcher composes into a note from an
	// error it did not author — an adapter's error string, which can quote
	// a command line. A node's own Note arrives already redacted (see
	// Result.Note); this covers the text the dispatcher adds itself.
	//
	// The dispatcher has no way to learn a run's secrets on its own, so a
	// caller holding them should pass exec.NewRedactor(...).Redact here.
	// Defaults to a no-op, which still collapses whitespace and truncates.
	Redact func(string) string
	// PauseOnCancel pauses the run, rather than ending it, when ctx is
	// cancelled, so Resume can continue it. An attempt the cancellation
	// interrupted is closed as one to run again, and Resume re-runs its
	// node at the next attempt; a cancellation between nodes pauses the
	// run before the next one starts. Run returns ErrInterrupted. See
	// "Cancellation" on Dispatcher.
	//
	// It is off by default, so a caller that never set it still gets the
	// run ended as aborted. `belay run` and `belay resume` set it, because
	// their help promises that Ctrl-C leaves a run `belay resume` picks up.
	PauseOnCancel bool

	// Fingerprint digests the workspace as it stands, and is recorded in the
	// snapshot the dispatcher takes before a node execution (state.Snapshot).
	// A fork compares it with the tree it is about to re-run in, to tell
	// whether the files have changed since the visit it re-runs.
	//
	// It is called with the run's workspace directory once per snapshot, so
	// at most once per node execution. A failure is logged and recorded as an
	// empty digest, which a reader treats as unknown: the run itself never
	// depends on the digest, so nothing about it is worth stopping a run for.
	// Nil records no digest. It is a function rather than an import because
	// the fingerprint that fits belongs to the isolator (dircopy.SurveyTree
	// skips what a copy skips), and dircopy builds only on unix.
	Fingerprint func(ctx context.Context, dir string) (string, error)

	// PauseAfter, when above zero, pauses the run once this call has run that
	// many nodes, before the next one starts, so Resume continues it from
	// there. It is how a fork runs exactly one node: PauseAfter 1.
	//
	// It counts the node executions this call performs (Outcome.Steps), never
	// the run's step count: a fork carries its source run's steps on, and a
	// limit measured against them would stop a fork before it ran anything.
	// It only ever stops a run that was going on to another node — after an
	// execution whose Result the dispatcher accepted with StatusOK and a Next
	// other than End. A node that fails, pauses, aborts or ends the run ends
	// the call exactly as it would without the limit, and so does one that is
	// interrupted, timed out or stopped by the budget.
	//
	// The pause is the one the caller asked for, so it exits ExitOK, like the
	// approval gate's, and not ExitAborted like the dispatcher's own limits.
	// Zero, the default, sets no limit; a negative value is refused by
	// NewDispatcher.
	PauseAfter int

	// Adapters, passed through to every RunContext. Any may be nil when the
	// configuration selects none; a node that needs a nil adapter is
	// responsible for returning a clear error.
	Agent    belay.AgentBackend
	Runner   belay.TestRunner
	Linter   belay.Linter
	Reviewer belay.Reviewer
	Isolator belay.Isolator

	// VCS is the version control handed to every node as RunContext.VCS.
	// Leave it nil for a run whose output is not a branch. The run's output
	// mode itself is not an option: the dispatcher reads it from the
	// manifest it already loads, which is where a run's pins live.
	VCS vcs.Repository
}

// Dispatcher drives one run's node graph: it resolves where to start, executes
// nodes in order, and owns every durable write the run makes.
//
// # Ordering: apply the patch, then journal the completion
//
// The single most important decision in this package is the order of the two
// durable writes that follow a node execution. The dispatcher applies
// Result.Patch through the Store first, and only then appends the
// node_finished record to the journal.
//
// The window between those two writes is where a crash is unrecoverable if the
// order is wrong, because journal.ResolveStart reads the journal alone to
// decide where to resume:
//
//   - Patch first, journal second (what this code does). A crash in the window
//     leaves the state mutation durable and no node_finished record. The
//     journal's last record is a node_started with nothing closing it, so
//     ResolveStart re-runs that node at attempt+1. Re-running is safe by
//     construction: Node's own contract requires implementations to be
//     re-runnable for exactly this reason, and state.Patch is idempotent —
//     every field is an absolute set, and History is deduplicated on Seq — so
//     the re-run cannot half-apply or double-apply anything. The cost is one
//     wasted node execution.
//
//   - Journal first, patch second. A crash in the window leaves a durable
//     node_finished record whose patch was never applied. ResolveStart reads
//     that record and resumes at its Next, skipping the node entirely, and the
//     rest of the run reads a State missing that node's output — a plan path
//     that is empty, a test report from two iterations ago — with nothing
//     anywhere signalling that a write was lost. The run does not crash; it
//     quietly produces a wrong answer.
//
// So the two failure modes are "pay for one node twice" and "silently corrupt
// the run", and the ordering is chosen to make only the first reachable. The
// journal is the commit record, and a commit record must never be durable
// before the effect it claims.
//
// The same rule sets the journal's internal order: journal.Validate rejects any
// run-level record written while a node_started is still open, so a terminal
// node_finished is always appended before the run_completed, run_failed,
// run_paused or run_aborted that ends the run.
//
// # Before the node runs: snapshot the blackboard
//
// Re-running a node after a crash is only safe if the re-run is given the
// blackboard its crashed attempt was given. Patch first, journal second means
// state.json may already hold that attempt's patch, and a node that computes
// its patch from what it reads — fix counting its repairs — would count the
// crashed attempt as well as its own. So before every execution at attempt 1
// the dispatcher saves the blackboard it is about to hand the node as a
// write-once snapshot, keyed on the execution's node_started Seq, and a re-run
// at a later attempt is handed the snapshot of the attempt it repeats instead
// of state.json (see blackboardFor, attemptOneSeq, and state.Snapshot, which
// argues the design).
//
// A crash in an execution recorded by a belay that took no snapshots leaves
// none to read. The re-run then takes one from state.json, which is what
// every belay before snapshots did, and may count that attempt twice.
//
// The snapshot is written after the node_started and before the node runs.
// The first makes its key durable before anything depends on it; the second
// means a missing snapshot proves no attempt of the execution has run the
// node, so state.json can still be trusted to hold the blackboard from before
// it. A node whose snapshot cannot be saved is not run at all (see notRun).
//
// # Cancellation: end the run, or pause it
//
// A cancelled context stops the run either way. What differs is whether it can
// be continued. By default the dispatcher ends it as aborted, as it always
// has, so a caller that never asked for anything else sees no change. With
// Options.PauseOnCancel it pauses the run instead: the interrupted attempt is
// closed with the record heal writes for an attempt a crash left open
// (journal.Record.Interrupted), run_paused follows it, and Resume re-runs the
// node at the next attempt. That is the crash-recovery path with the journal
// already healed. The attempt number, the step count, the discarded Result and
// the re-run are what a kill at the same moment would have produced — an
// interrupted execution is not counted as a step, exactly as a killed one is
// not (see settle) — but the journal says the run was asked to stop, and the
// manifest says paused rather than running.
//
// Pausing needs no event or status of its own, and no capability in the
// run's requires list. A capability keeps out a belay that would misread the
// run, and no release can misread this one. v0.1.x refuses schema version 2
// outright (ADR-0011), and the release that brings version 2 brings with it
// the two rules a paused attempt depends on: journal.ResolveStart re-runs an
// interrupted attempt at the next attempt number instead of reusing the
// number, and `belay resume` approves only a plan the gate paused to ask
// about, never one a Ctrl-C stopped at the gate. Without those, a resume
// would continue the run, and continue it wrongly.
//
// A cancellation that arrives between nodes pauses the run before the next
// one starts, with no attempt opened. Resume then starts that node afresh.
//
// The dispatcher's own limits pause a run too, and for a different reason.
// The budget ceiling, graph.max_steps and graph.node_timeout are not verdicts
// on the work: they are the figures a person chose beforehand for how much of
// it they were willing to pay for, and a person may raise one. Ending the run
// put the money and the work already done out of reach, so the only way on
// was a new run from nothing, which would spend it all again. So the run
// pauses, with the setting to raise in its note, and Resume re-checks each
// limit against the configuration as it is then: unlike the graph's shape,
// which is pinned in the manifest when the run is created, the limits are
// deliberately read live (see runContext). A limit left as it was is simply
// met again, having run nothing and spent nothing.
//
// A node that outlived graph.node_timeout has its attempt closed as one to
// run again, exactly as a cancelled one does, so that raising the limit and
// resuming re-runs the step that overran.
//
// # Before the first write: establish the run
//
// Opening the journal writes to a run: journal.Open creates the file and heals
// a torn tail. So dispatch reads the manifest, then calls state.Store.Preflight
// — which refuses a run this belay may not read, and saves the manifest at the
// current schema version — and only then opens anything. A run declaring a
// capability this belay lacks in state.json alone was otherwise healed,
// appended to and, at the first guard, ended for good, although belay had
// already established it could not read it (ADR-0011).
//
// # What the dispatcher owns
//
// The dispatcher opens, writes and closes the *journal.Journal, and it is the
// only writer of manifest.json and state.json during a run. Nodes are pure:
// they read a State snapshot and return a Result. That split is what makes the
// ordering above enforceable in one place instead of in eight node packages.
type Dispatcher struct {
	store    *state.Store
	registry *Registry
	cfg      config.Config
	entry    string
	logger   *slog.Logger
	now      func() time.Time
	redact   func(string) string

	pauseOnCancel bool
	fingerprint   func(ctx context.Context, dir string) (string, error)
	pauseAfter    int

	agent    belay.AgentBackend
	runner   belay.TestRunner
	linter   belay.Linter
	reviewer belay.Reviewer
	isolator belay.Isolator
	vcs      vcs.Repository
}

// NewDispatcher returns a Dispatcher for the run described by opts. It does not
// touch the filesystem: the run directory is read when Run or Resume is called,
// so constructing a Dispatcher never holds the journal open.
func NewDispatcher(opts Options) (*Dispatcher, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("%w: Store is required", ErrInvalidOptions)
	}
	if opts.Registry == nil {
		return nil, fmt.Errorf("%w: Registry is required", ErrInvalidOptions)
	}
	if opts.Registry.Len() == 0 {
		return nil, fmt.Errorf("%w: Registry has no nodes registered", ErrInvalidOptions)
	}
	if opts.PauseAfter < 0 {
		return nil, fmt.Errorf("%w: PauseAfter is %d; it counts node executions, so it cannot be negative",
			ErrInvalidOptions, opts.PauseAfter)
	}

	d := &Dispatcher{
		store:    opts.Store,
		registry: opts.Registry,
		cfg:      opts.Config,
		entry:    opts.Entry,
		logger:   opts.Logger,
		now:      opts.Now,
		redact:   opts.Redact,

		pauseOnCancel: opts.PauseOnCancel,
		fingerprint:   opts.Fingerprint,
		pauseAfter:    opts.PauseAfter,

		agent:    opts.Agent,
		runner:   opts.Runner,
		linter:   opts.Linter,
		reviewer: opts.Reviewer,
		isolator: opts.Isolator,
		vcs:      opts.VCS,
	}
	if d.logger == nil {
		d.logger = slog.Default()
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.redact == nil {
		d.redact = func(s string) string { return s }
	}
	return d, nil
}

// Run starts or continues a run.
//
// It refuses, with ErrRunPaused and ExitPaused, a run whose journal ends in
// run_paused: a pause is a request for human input, and silently stepping past
// it would defeat the approval gate that created it. Every other resumable
// state — a fresh run, a run interrupted mid-node by a crash, a run in the
// ordinary middle of its graph — Run advances, because recovering from a crash
// is not the same act as overriding a pause.
func (d *Dispatcher) Run(ctx context.Context) (Outcome, error) {
	return d.dispatch(ctx, false)
}

// Resume continues a run, including one paused for approval. It is otherwise
// identical to Run.
func (d *Dispatcher) Resume(ctx context.Context) (Outcome, error) {
	return d.dispatch(ctx, true)
}

// dispatch is the shared body of Run and Resume; resuming selects whether a
// paused run may be advanced.
func (d *Dispatcher) dispatch(ctx context.Context, resuming bool) (Outcome, error) {
	man, err := d.store.LoadManifest()
	if err != nil {
		return Outcome{ExitCode: ExitFailed}, fmt.Errorf("graph: load manifest: %w", err)
	}

	// Before the first write to this run, and so before the journal is
	// opened: establish that belay may read the whole run, and bring the
	// manifest up to the version belay writes. Store.Preflight says why both
	// belong here rather than further in. The manifest is loaded first only
	// because a manifest this belay cannot read is not a run it can dispatch
	// at all.
	if err := d.store.Preflight(); err != nil {
		return Outcome{ExitCode: ExitFailed}, fmt.Errorf("graph: %w", err)
	}

	// Open before reading: Open heals a torn tail left by a previous crash,
	// so the records read afterwards are the ones the next Append will
	// actually follow.
	jrnl, err := journal.Open(d.store.Layout().JournalPath())
	if err != nil {
		return Outcome{ExitCode: ExitFailed}, fmt.Errorf("graph: open journal: %w", err)
	}
	// Every record is fsynced by Append before it returns, so a Close error
	// cannot cost us a record; the run's durability does not depend on it.
	defer func() { _ = jrnl.Close() }()

	read, err := journal.ReadFile(d.store.Layout().JournalPath())
	if err != nil {
		return Outcome{ExitCode: ExitFailed}, fmt.Errorf("graph: read journal: %w", err)
	}
	start, err := journal.ResolveStart(read.Records)
	if err != nil {
		return Outcome{ExitCode: ExitFailed}, fmt.Errorf("graph: resolve resume point: %w", err)
	}
	// Where a run with any history at all goes next is ResolveStart's
	// decision: it is the only thing that knows which node died, at which
	// attempt, and what the last finished node named as its successor.
	// Options.Entry answers the one question the journal cannot, which is
	// where a run that has never run anything begins.
	if d.entry != "" && len(read.Records) == 0 {
		start.Node = d.entry
	}

	// Rebuilt, never New: a resumed run must keep accumulating spend from
	// where the crash left off. A fresh ledger would hand every crash a full
	// ceiling to spend again, turning a hard cap into a suggestion. For a
	// fresh run the journal and the manifest's budget block are both empty,
	// so this is exactly New — which is why it is unconditional rather than
	// a branch that could be wrong. See rebuildLedger for why the journal,
	// and not the manifest, is what it is rebuilt from.
	ledger := rebuildLedger(read.Records, man.Budget)

	// Heal the run-level bookkeeping a crash can leave behind, before any new
	// record is appended. start is computed above, from the records as the
	// crash left them, because it is the only thing that still knows which
	// attempt died.
	healed, err := d.heal(jrnl, read.Records, &start, ledger)
	if err != nil {
		return Outcome{ExitCode: ExitFailed}, err
	}
	if healed && start.State != journal.RunStateRunnable {
		if err := d.persist(&man, runStatusFor(start.State), start.Node, ledger); err != nil {
			return Outcome{ExitCode: ExitFailed}, err
		}
	}

	if out, done, err := d.checkStartState(jrnl, start, resuming, ledger); done {
		return out, err
	}

	// A re-run at a later attempt is given the snapshot of the execution it
	// repeats, found from the records as the crash left them, like start.
	var repeats uint64
	if start.Attempt > 1 {
		repeats = attemptOneSeq(read.Records, start.Node)
	}
	return d.loop(ctx, jrnl, &man, ledger, start, repeats)
}

// attemptOneSeq returns the Seq of the node_started that began the execution
// a re-run of node at a later attempt repeats: the last attempt-1
// node_started for node in records. The attempts of one execution are
// consecutive — a crash or a Ctrl-C re-runs the node at the next attempt
// before any other node runs — so the last attempt 1 is theirs. For a gate
// taken up again after a person acted, that is the execution after the
// resume, not the start of the gate's visit.
//
// It returns 0 when records hold none, which no journal belay writes does.
func attemptOneSeq(records []journal.Record, node string) uint64 {
	for i := len(records) - 1; i >= 0; i-- {
		if r := records[i]; r.Event == journal.EventNodeStarted && r.Node == node && r.Attempt == 1 {
			return r.Seq
		}
	}
	return 0
}

// heal repairs a journal whose final record shows the previous process died
// between two writes, so that the next Append cannot corrupt it and so that no
// crash leaves a run stuck.
//
// It handles the two windows a crash can land in:
//
//   - A node_started with nothing closing it: the process died inside that
//     node. journal.Validate treats a second node_started opened over the
//     first as corruption, and refuses to open such a file ever again — so
//     retrying without closing the dead attempt would brick the run's journal
//     on its first crash. heal closes it as aborted, naming the same node in
//     Next so that a crash during recovery itself still resolves back here,
//     at the attempt after the one it closed (journal.Record.Interrupted),
//     and charges ledger for it: see interruptedCharge. It also kills whatever
//     the dead process left running, because the agent and the test suite
//     outlive a belay killed with SIGKILL: see reap.
//
//   - A terminal node_finished with no run-level marker after it: the process
//     died between ending a node and recording that the run ended. Left alone,
//     ResolveStart reports the run as merely runnable, which would step a
//     failed run onwards or walk straight through an approval gate. heal
//     appends the marker that was owed and corrects start.State to match.
//
// start is updated in place. It is never re-derived from the healed records:
// the pre-heal decision is what still carries the dead attempt's number, and
// the attempt after a crash must be that number plus one.
func (d *Dispatcher) heal(
	jrnl *journal.Journal, records []journal.Record, start *journal.Result, ledger *budget.Ledger,
) (bool, error) {
	if len(records) == 0 {
		return false, nil
	}
	last := records[len(records)-1]

	switch last.Event {
	case journal.EventNodeStarted:
		// Reaped before the record is written, not after. A crash in
		// between leaves the journal ending in this same node_started, so
		// the next resume heals again and kills whatever is still running.
		// Journalling first would leave behind records that no later heal
		// would ever look for, because the journal would no longer end in an
		// attempt nothing closed.
		killed := d.reap()
		charge := interruptedCharge(ledger)
		note := fmt.Sprintf("interrupted: no completion record for attempt %d (the process died inside this node); "+
			"charged $%.2f, estimated, since what it spent is unknown", last.Attempt, charge.USD)
		if killed > 0 {
			groups := "process group"
			if killed != 1 {
				groups = "process groups"
			}
			note += fmt.Sprintf("; killed %d %s it had left running", killed, groups)
		}
		note += "; re-running"
		// Journal first, then Record: the record is what a later resume
		// rebuilds the ledger from, so a crash between the two costs this
		// dispatch the charge but never the next one.
		if _, err := jrnl.Append(interruptedAttempt(last.Node, last.Attempt, 0, charge, note)); err != nil {
			return false, fmt.Errorf("graph: close interrupted %q: %w", last.Node, err)
		}
		ledger.Record(last.Node, charge)
		d.logger.Warn("belay/graph: recovered an interrupted node",
			slog.String("node", last.Node), slog.Int("crashed_attempt", last.Attempt),
			slog.Int("next_attempt", start.Attempt), slog.Float64("charged_usd", charge.USD))
		return true, nil

	case journal.EventNodeFinished:
		event, runState, okToHeal := owedRunMarker(last)
		if !okToHeal {
			return false, nil
		}
		if _, err := jrnl.Append(journal.Record{Event: event, Note: last.Note}); err != nil {
			return false, fmt.Errorf("graph: record owed %s: %w", event, err)
		}
		start.State = runState
		d.logger.Warn("belay/graph: recorded a run marker a crash had owed",
			slog.String("event", event.String()), slog.String("node", last.Node))
		return true, nil

	default:
		// A run-level marker already closes the journal: nothing is owed.
		return false, nil
	}
}

// interruptedAttempt is the node_finished that closes an attempt the
// dispatcher will run again: aborted, naming its own node as Next
// (journal.Record.Interrupted). heal writes it for an attempt a crash left
// open, and pauseInsideNode for one a cancellation cut short. Building both
// here keeps them the one shape that ResolveStart resumes at the next attempt
// and owedRunMarker owes nothing for.
//
// usage is what the attempt is charged, and the two callers know different
// things. pauseInsideNode passes the node's own report, because a node cut
// short by a cancellation has still spent whatever its backend said it spent.
// heal has no report at all, because the process that could have made one
// died, and passes interruptedCharge.
func interruptedAttempt(node string, attempt int, elapsed time.Duration, usage belay.Usage, note string) journal.Record {
	return journal.Record{
		Event: journal.EventNodeFinished, Node: node, Attempt: attempt,
		DurationMS: elapsed.Milliseconds(), Status: journal.StatusAborted, Next: node,
		Usage: journalUsage(usage), Note: note,
	}
}

// reap kills every process group this run started and never reaped, and
// reports how many it killed.
//
// A process belay starts for a node outlives a belay that is killed with
// SIGKILL, or quit by a second Ctrl-C: it is in a process group of its own,
// which the terminal's Ctrl-C never reaches, and the kernel gives it to init.
// An agent CLI left that way keeps spending. internal/exec records every such
// process in its node's evidence directory, and this is where the records are
// acted on, on the one path that means the previous process died inside a
// node.
//
// It only ever reports. A record belay cannot act on is no reason to refuse to
// continue a run, and procgroup.Reap never signals a process it cannot prove
// belay started (see its doc on recycled pids).
func (d *Dispatcher) reap() int {
	killed := 0
	for _, r := range procgroup.Reap(d.store.Layout().NodesDir()) {
		switch {
		case r.Outcome == procgroup.OutcomeKilled:
			killed++
			d.logger.Warn("belay/graph: killed a process this run had left running",
				slog.Int("pid", r.Record.PID), slog.Any("command", r.Record.Argv))
		case r.Err != nil:
			d.logger.Warn("belay/graph: could not act on the record of a process this run started",
				slog.String("outcome", r.Outcome.String()), slog.Int("pid", r.Record.PID),
				slog.Any("command", r.Record.Argv), slog.String("error", r.Err.Error()))
		default:
			d.logger.Info("belay/graph: a process this run started is no longer its own",
				slog.String("outcome", r.Outcome.String()), slog.Int("pid", r.Record.PID),
				slog.Any("command", r.Record.Argv))
		}
	}
	return killed
}

// interruptedCharge is what heal charges an attempt a crash left open: the
// most any single node execution of this run has recorded, in each dimension
// (budget.Ledger.Largest), marked as estimated. ADR-0007 records the decision.
//
// The attempt's real cost is unknowable. The process that would have
// reported it was killed, and an agent may have spent any amount before the
// kill, so there is no right answer, only a choice of which way to be wrong.
// Charging nothing, as heal once did, is wrong in the direction that breaks
// the budget ceiling: a run killed during a paid step and resumed pays for
// that step twice and counts it once. Charging too much only stops a run
// early. So the charge leans high, and it is taken from the run's own
// evidence, the costliest step it has actually paid for, rather than from a
// price table that would be a second source of truth for what a backend
// costs.
//
// Estimated is always set on a non-zero charge. It is sticky in the ledger, so
// every total that includes the charge says it is partly a guess, which is
// the truth.
//
// When the run has recorded nothing yet, the charge is zero. With no step
// paid for there is no evidence to charge from, and inventing a figure would
// be a guess dressed as a measurement. That leaves one gap open: a run killed
// during its first paid step can go past its ceiling by what that step cost.
func interruptedCharge(ledger *budget.Ledger) belay.Usage {
	charge := ledger.Largest()
	if charge.InputTokens == 0 && charge.OutputTokens == 0 && charge.USD == 0 {
		return belay.Usage{}
	}
	charge.Estimated = true
	return charge
}

// owedRunMarker maps a node_finished that ended the run onto the run-level
// record that should have followed it. A StatusOK node is not terminal — the
// loop decides what follows it — so it owes nothing.
//
// Neither does an attempt closed as interrupted (journal.Record.Interrupted),
// although its status is aborted. heal writes one and then re-runs its node,
// so a crash right after it leaves exactly that record last. Owing run_aborted
// for it would end, for good, a run whose only fault was dying twice.
func owedRunMarker(last journal.Record) (journal.Event, journal.RunState, bool) {
	if last.Interrupted() {
		return journal.EventUnknown, journal.RunStateRunnable, false
	}
	switch last.Status {
	case journal.StatusFailed:
		return journal.EventRunFailed, journal.RunStateFailed, true
	case journal.StatusAborted:
		return journal.EventRunAborted, journal.RunStateAborted, true
	case journal.StatusPaused:
		return journal.EventRunPaused, journal.RunStatePaused, true
	default:
		return journal.EventUnknown, journal.RunStateRunnable, false
	}
}

// runStatusFor maps a journal RunState onto the manifest's RunStatus, so a
// healed journal and the manifest agree about how the run ended.
func runStatusFor(s journal.RunState) state.RunStatus {
	switch s {
	case journal.RunStatePaused:
		return state.RunStatusPaused
	case journal.RunStateCompleted:
		return state.RunStatusCompleted
	case journal.RunStateFailed:
		return state.RunStatusFailed
	case journal.RunStateAborted:
		return state.RunStatusAborted
	default:
		return state.RunStatusRunning
	}
}

// checkStartState handles every resume state that does no work: a paused run a
// plain Run must refuse, a run that already ended, and the run_started record a
// fresh journal needs. done reports whether dispatch should return immediately.
func (d *Dispatcher) checkStartState(
	jrnl *journal.Journal, start journal.Result, resuming bool, ledger *budget.Ledger,
) (out Outcome, done bool, err error) {
	switch start.State {
	case journal.RunStatePaused:
		if !resuming {
			return Outcome{
				Status:   state.RunStatusPaused,
				ExitCode: ExitPaused,
				Node:     start.Node,
				Usage:    ledger.Total(),
				Note:     fmt.Sprintf("run is paused at %q; use `belay resume` to continue it", start.Node),
			}, true, ErrRunPaused
		}
		if _, err := jrnl.Append(journal.Record{Event: journal.EventRunResumed}); err != nil {
			return Outcome{ExitCode: ExitFailed}, true, fmt.Errorf("graph: journal run_resumed: %w", err)
		}
	case journal.RunStateCompleted:
		return Outcome{
			Status:   state.RunStatusCompleted,
			ExitCode: ExitOK,
			Node:     End,
			Usage:    ledger.Total(),
			Note:     "run already completed",
		}, true, nil
	case journal.RunStateFailed:
		return Outcome{
			Status: state.RunStatusFailed, ExitCode: ExitFailed, Node: start.Node,
			Usage: ledger.Total(), Note: "run already failed",
		}, true, ErrRunEnded
	case journal.RunStateAborted:
		return Outcome{
			Status: state.RunStatusAborted, ExitCode: ExitAborted, Node: start.Node,
			Usage: ledger.Total(), Note: "run already aborted",
		}, true, ErrRunEnded
	case journal.RunStateRunnable:
		// A journal with no records at all is a run that has not started.
		// Anything else is already under way and needs no new marker; in
		// particular a run recovering from a crash may have an open
		// node_started, and journal.Validate rejects any run-level record
		// written while one is open.
		if jrnl.LastSeq() == 0 {
			if _, err := jrnl.Append(journal.Record{Event: journal.EventRunStarted}); err != nil {
				return Outcome{ExitCode: ExitFailed}, true, fmt.Errorf("graph: journal run_started: %w", err)
			}
		}
	}
	return Outcome{}, false, nil
}

// loop executes nodes until the run ends. It is the body described by the
// ordering contract on Dispatcher.
//
// repeats is the Seq of the attempt-1 node_started the first execution
// repeats when it is a re-run at a later attempt (see attemptOneSeq), and is
// otherwise unused: only the first execution of a dispatch can be a re-run,
// since every node after it starts at attempt 1.
func (d *Dispatcher) loop(
	ctx context.Context, jrnl *journal.Journal, man *state.Manifest,
	ledger *budget.Ledger, start journal.Result, repeats uint64,
) (Outcome, error) {
	current, attempt, steps := start.Node, start.Attempt, 0

	for {
		if current == End {
			return d.endRun(jrnl, man, ledger, journal.EventRunCompleted, state.RunStatusCompleted,
				ExitOK, End, steps, "run completed", nil)
		}
		if current == "" {
			// ResolveStart could not name a successor: a terminal
			// node_finished exists but the run-level marker that should
			// follow it does not. Refuse rather than guess at the graph.
			return d.endRun(jrnl, man, ledger, journal.EventRunFailed, state.RunStatusFailed,
				ExitFailed, current, steps, ErrNoResumePoint.Error(), ErrNoResumePoint)
		}

		// Both guards run BEFORE the node does, and before any node_started
		// is appended: a run that is already over budget must not pay for
		// one more node to discover it, and an aborting run must leave the
		// journal with nothing open.
		if err := ledger.Check(d.cfg.Budget); err != nil {
			cause := fmt.Errorf("graph: budget ceiling reached before %q: %w", current, err)
			return d.pauseAtLimit(jrnl, man, ledger, current, steps,
				d.limitNote(cause, "budget.max_usd or budget.max_tokens"), cause)
		}
		if d.cfg.Graph.MaxSteps > 0 && man.Step >= d.cfg.Graph.MaxSteps {
			err := fmt.Errorf("%w: %d node executions performed, limit is graph.max_steps=%d (raise it, or the graph is not converging)",
				ErrMaxSteps, man.Step, d.cfg.Graph.MaxSteps)
			return d.pauseAtLimit(jrnl, man, ledger, current, steps,
				d.limitNote(err, "graph.max_steps"), err)
		}
		// A run its caller has cancelled starts nothing more. Started on a
		// context that is already done, the node would be cut off at once,
		// or, if it never looks at its context, do its work only to have it
		// thrown away. This comes after the two guards: a run they would
		// end is ended now, not paused to meet them again on resume.
		if d.pauseOnCancel && errors.Is(ctx.Err(), context.Canceled) {
			return d.pauseInterrupted(jrnl, man, ledger, current, steps)
		}

		node, err := d.registry.Get(current)
		if err != nil {
			return d.endRun(jrnl, man, ledger, journal.EventRunFailed, state.RunStatusFailed,
				ExitFailed, current, steps, d.note(err.Error()),
				fmt.Errorf("graph: cannot dispatch: %w", err))
		}

		st, err := d.store.LoadState()
		if err != nil {
			return Outcome{ExitCode: ExitFailed, Node: current, Steps: steps, Usage: ledger.Total()},
				fmt.Errorf("graph: load state before %q: %w", current, err)
		}

		step := man.Step + 1
		started, err := jrnl.Append(journal.Record{
			Event: journal.EventNodeStarted, Node: current, Attempt: attempt,
		})
		if err != nil {
			return Outcome{ExitCode: ExitFailed, Node: current, Steps: steps, Usage: ledger.Total()},
				fmt.Errorf("graph: journal node_started for %q: %w", current, err)
		}

		rc := d.runContext(man, st, ledger, current, step, attempt)
		// Held apart from rc: the node receives rc by pointer, and what the
		// dispatcher logs after the node returns must not depend on what
		// the node did to it.
		logger := rc.Logger

		// The blackboard the node is given: its own snapshot at attempt 1,
		// and at a later attempt the snapshot of the attempt it repeats,
		// never state.json, which may hold that attempt's patch.
		key := started.Seq
		if attempt > 1 {
			if repeats == 0 {
				logger.Warn("belay/graph: the journal does not show the attempt this re-run repeats; " +
					"it is given state.json, which may already hold that attempt's patch")
			} else {
				key = repeats
			}
		}
		view, err := d.blackboardFor(ctx, rc, key)
		if err != nil {
			return d.notRun(jrnl, ledger, current, attempt, steps, err)
		}
		rc.State = view
		logger.Info("belay/graph: node started")

		exec := d.execute(ctx, node, rc)
		steps++

		out, err := d.settle(jrnl, man, ledger, node.Name(), attempt, started.Seq, step, steps, exec, logger)
		if out != nil {
			return *out, err
		}
		if err != nil {
			return Outcome{ExitCode: ExitFailed, Node: current, Steps: steps, Usage: ledger.Total()}, err
		}

		current, attempt = exec.res.Next, 1

		// Only here, where settle has accepted a StatusOK Result that goes on
		// to another node: every other outcome has already returned, so the
		// limit can never hide one (see Options.PauseAfter).
		if d.pauseAfter > 0 && steps >= d.pauseAfter {
			nodes := "nodes"
			if steps == 1 {
				nodes = "node"
			}
			return d.endRun(jrnl, man, ledger, journal.EventRunPaused, state.RunStatusPaused, ExitOK, current, steps,
				fmt.Sprintf("paused before %q, as asked, after running %d %s; use `belay resume` to continue",
					current, steps, nodes), nil)
		}
	}
}

// runContext builds the RunContext for one execution of node: the only place
// one is assembled, so everything a node is allowed to see is decided here.
//
// It reads and returns; it writes nothing. That keeps it out of the ordering
// contract on Dispatcher: building a node's view of the run is not a durable
// effect, so a crash while doing it loses nothing and leaves nothing to heal.
//
// Config is d.cfg, the configuration this process loaded. On a resume that is
// read from disk again and can differ from what the run started with. A value
// that must stay the same for the whole of a run belongs on man, which was
// written when the run was created, and a field carrying one should be filled
// from man rather than from d.cfg; internal/nodes states the same rule for
// the shape of the graph.
//
// It does not decide which node runs, or check the budget or the step cap:
// the loop has done all three before a node needs a context.
func (d *Dispatcher) runContext(
	man *state.Manifest, st state.State, ledger *budget.Ledger, node string, step, attempt int,
) *RunContext {
	layout := d.store.Layout()
	return &RunContext{
		Goal: man.Goal, State: st, Config: d.cfg, Layout: layout,
		// Supplied, not derived: six nodes previously reconstructed the
		// workspace by walking up from the run directory, which yields
		// belay's own tree for a zero Layout.
		Workspace: layout.WorkspaceDir(),
		RunID:     man.RunID,
		Budget:    ledger.Snapshot(d.cfg.Budget.MaxUSD),
		NodeName:  node, Step: step, Attempt: attempt,
		Logger: d.logger.With(
			slog.String("node", node), slog.Int("step", step), slog.Int("attempt", attempt)),
		Agent: d.agent, Runner: d.runner, Linter: d.linter,
		Reviewer: d.reviewer, Isolator: d.isolator,
		// Run-long values, so they come from man, which was written when
		// the run was created, never from d.cfg.
		Output:     man.Output,
		ArchiveDir: man.ArchivedPlan.Dir,
		VCS:        d.vcs,
	}
}

// blackboardFor returns the blackboard the execution rc describes must be
// given, and makes sure the snapshot it comes from is on disk before the node
// runs (state.Snapshot).
//
// seq names the snapshot: the Seq of the node_started of the execution at
// attempt 1. When that snapshot exists, an earlier attempt of the execution
// took it, and its blackboard is the truth — state.json may already carry
// the patch of the attempt that crashed. When it does not, no attempt has run
// the node yet, because the snapshot is always written before the node runs:
// state.json still holds the blackboard from before the execution, and it is
// saved as the snapshot now.
//
// It is called after the execution's node_started is durable, so a snapshot
// is never keyed on a Seq the journal could give out again, and before the
// node runs, which is the other half of the invariant above.
func (d *Dispatcher) blackboardFor(ctx context.Context, rc *RunContext, seq uint64) (state.State, error) {
	path, err := rc.Layout.SnapshotPath(seq, rc.NodeName)
	if err != nil {
		return state.State{}, err
	}
	snap, err := state.LoadSnapshot(path)
	switch {
	case err == nil:
		return snap.State, nil
	case !errors.Is(err, fs.ErrNotExist):
		return state.State{}, err
	}

	if _, err := d.store.SaveSnapshotOnce(state.Snapshot{
		RunID: rc.RunID, Node: rc.NodeName, Seq: seq, Step: rc.Step,
		TakenAt: d.now().UTC(), WorkspaceDigest: d.workspaceDigest(ctx, rc),
		State: rc.State,
	}); err != nil {
		return state.State{}, err
	}
	return rc.State, nil
}

// workspaceDigest fingerprints rc's workspace for a snapshot, or returns ""
// when there is no Options.Fingerprint or it fails. See Options.Fingerprint
// for why a failure is only logged.
func (d *Dispatcher) workspaceDigest(ctx context.Context, rc *RunContext) string {
	if d.fingerprint == nil {
		return ""
	}
	digest, err := d.fingerprint(ctx, rc.Workspace)
	if err != nil {
		rc.Logger.Warn("belay/graph: could not fingerprint the workspace; this snapshot records no digest",
			slog.String("error", err.Error()))
		return ""
	}
	return digest
}

// notRun ends this dispatch without running node, whose snapshot could not be
// taken (see blackboardFor), and closes its attempt as one to run again.
//
// A node run without its snapshot is one a crash could not be recovered from
// correctly, so it is not run at all. The attempt is closed with the record
// heal writes for one a crash left open (interruptedAttempt), which Resume
// re-runs at the next attempt — but charged nothing, because nothing ran.
// Leaving it open for heal to close would work too, and would charge it the
// costliest step the run has paid for (interruptedCharge), for a node that
// never started: a run whose disk keeps refusing the snapshot would spend its
// ceiling on nothing, one resume at a time.
//
// The manifest is left as it was, as it is for the loop's other failures to
// persist. If even the closing record cannot be written, the attempt stays
// open and heal closes it later.
func (d *Dispatcher) notRun(
	jrnl *journal.Journal, ledger *budget.Ledger, node string, attempt, steps int, cause error,
) (Outcome, error) {
	err := fmt.Errorf("graph: could not save the blackboard snapshot %q needs before it runs: %w", node, cause)
	note := d.note(err.Error()) + "; the node did not run, and resuming runs it again"
	if _, jerr := jrnl.Append(interruptedAttempt(node, attempt, 0, belay.Usage{}, note)); jerr != nil {
		err = errors.Join(err, fmt.Errorf("graph: journal node_finished for %q: %w", node, jerr))
	}
	return Outcome{ExitCode: ExitFailed, Node: node, Steps: steps, Usage: ledger.Total(), Note: note}, err
}

// nodeOutcome is one node execution as the dispatcher observed it, before any
// of it has been trusted or persisted.
type nodeOutcome struct {
	res      Result
	err      error
	elapsed  time.Duration
	timedOut bool
	canceled bool
}

// execute runs one node under a context derived from the run's context with
// config.Graph.NodeTimeout applied, and reports what happened.
//
// The deadline is authoritative: if it expired, the execution is a timeout
// regardless of what the node returned. A node that ignores its context and
// runs long is precisely what the cap exists to stop, so believing its Result
// over the clock would make the cap advisory.
func (d *Dispatcher) execute(ctx context.Context, n Node, rc *RunContext) nodeOutcome {
	nodeCtx := ctx
	// Every process a node starts goes through internal/exec, which records
	// it in this directory, so that a belay killed with SIGKILL can find the
	// process again and heal can kill it (internal/procgroup). It is this
	// execution's own evidence directory, the one the node writes its prompt
	// and captured output into.
	if dir, err := rc.Layout.NodeDir(rc.Step, rc.NodeName); err != nil {
		rc.Logger.Warn("belay/graph: no evidence directory for this node, so the processes it starts are not recorded",
			slog.String("error", err.Error()))
	} else {
		nodeCtx = procgroup.WithDir(nodeCtx, dir)
	}
	cancel := context.CancelFunc(func() {})
	// A node that supervises candidate runs is exempt: what it waits on is
	// already capped, node by node, by this same setting. See Supervisor,
	// which argues the bound the exemption leaves in place.
	_, supervises := n.(Supervisor)
	if timeout := d.cfg.Graph.NodeTimeout.Duration(); timeout > 0 && !supervises {
		nodeCtx, cancel = context.WithTimeout(nodeCtx, timeout)
	}
	defer cancel()

	started := d.now()
	res, err := n.Run(nodeCtx, rc)
	elapsed := d.now().Sub(started)
	if elapsed < 0 {
		elapsed = 0
	}

	return nodeOutcome{
		res:      res,
		err:      err,
		elapsed:  elapsed,
		timedOut: errors.Is(nodeCtx.Err(), context.DeadlineExceeded),
		canceled: errors.Is(ctx.Err(), context.Canceled),
	}
}

// settle turns one nodeOutcome into durable writes, in the order the
// Dispatcher doc defends: apply the patch, then append node_finished, then —
// if the run ends here — append the run-level marker.
//
// A nil Outcome means the run continues to the next node. A non-nil Outcome is
// the run's final answer, and the returned error its cause.
//
//nolint:gocritic // the branching here is the run's terminal-state table; splitting it hides the shape.
func (d *Dispatcher) settle(
	jrnl *journal.Journal, man *state.Manifest, ledger *budget.Ledger,
	node string, attempt int, startedSeq uint64, step, steps int, exec nodeOutcome, logger *slog.Logger,
) (*Outcome, error) {
	// The rejected paths. None of them applies a Patch: each means the
	// Result is untrustworthy or absent, so nothing it claims about the
	// workspace may be believed.
	//
	// Every one of them does record Usage. Spend is not a verdict: a backend
	// that reports having burned $2.50 has burned it, and whether the
	// dispatcher then accepts, rejects or throws away the node's Result
	// changes nothing about the money. Recording zero here is what let a run
	// cross budget.max_usd and, once Options.PauseOnCancel made an
	// interrupted run resumable through an ordinary Ctrl-C, spend the whole
	// ceiling over again on the resume (ADR-0007).
	//
	// A node cut off part-way often has nothing to report, and then this
	// records the zero it reported. That is a separate case from an attempt
	// belay never saw finish at all, which heal closes.
	//
	// The timeout is checked first. It is the dispatcher's own guard, and a
	// node that outlived it is not excused by a cancellation that arrived
	// while it did.
	switch {
	case exec.timedOut:
		err := fmt.Errorf("%w: %q exceeded graph.node_timeout=%s", ErrNodeTimeout, node, d.cfg.Graph.NodeTimeout)
		return d.pauseInsideNode(jrnl, man, ledger, node, attempt, steps, exec.elapsed, exec.res.Usage,
			d.limitNote(err, "graph.node_timeout"), err, logger)
	case exec.canceled && d.pauseOnCancel:
		return d.pauseInsideNode(jrnl, man, ledger, node, attempt, steps, exec.elapsed, exec.res.Usage,
			fmt.Sprintf("interrupted: belay was asked to stop while attempt %d was running; resuming re-runs it", attempt),
			fmt.Errorf("%w: at %q: %w", ErrInterrupted, node, context.Canceled), logger)
	case exec.canceled:
		err := fmt.Errorf("%w: %q canceled: %w", ErrNodeAborted, node, context.Canceled)
		return d.terminal(jrnl, man, ledger, node, attempt, steps, exec.elapsed, exec.res.Usage,
			journal.StatusAborted, journal.EventRunAborted, state.RunStatusAborted, ExitAborted,
			d.note(err.Error()), err, logger)
	case exec.err != nil:
		return d.terminal(jrnl, man, ledger, node, attempt, steps, exec.elapsed, exec.res.Usage,
			journal.StatusFailed, journal.EventRunFailed, state.RunStatusFailed, ExitFailed,
			nodeErrorNotePrefix+d.note(exec.err.Error()),
			fmt.Errorf("%w: %q returned an error: %w", ErrNodeFailed, node, exec.err), logger)
	}
	if err := exec.res.Validate(); err != nil {
		return d.terminal(jrnl, man, ledger, node, attempt, steps, exec.elapsed, exec.res.Usage,
			journal.StatusFailed, journal.EventRunFailed, state.RunStatusFailed, ExitFailed,
			invalidResultNotePrefix+d.note(err.Error()),
			fmt.Errorf("%w: %q returned an unusable Result: %w", ErrNodeFailed, node, err), logger)
	}

	// Accepted, and only now is this execution one of the run's steps.
	// man.Step counts the executions the run acted on: it is what
	// graph.max_steps is measured against, and what names a node's evidence
	// directory. An execution that timed out, failed or was interrupted
	// produced no Result to act on, and a kill at the same moment would have
	// counted nothing either — the process died before it could write the
	// manifest. Counting one would spend a step of the run's budget on an
	// interruption, and move the re-run's evidence directory beside the
	// attempt it replaces instead of over it.
	man.Step = step
	// Apply the patch FIRST — see the ordering contract on
	// Dispatcher. History is keyed on the node_started record's Seq: a real,
	// already-durable journal sequence number unique to this attempt, which
	// is what makes re-applying this patch after a crash idempotent.
	patch := exec.res.Patch
	patch.History = &state.HistoryEntry{
		Seq: startedSeq, Node: node, Attempt: attempt,
		Status: exec.res.Status.String(), Next: exec.res.Next,
		Time: d.now().UTC(), Summary: exec.res.Note,
	}
	if _, err := d.store.ApplyPatch(patch); err != nil {
		return &Outcome{ExitCode: ExitFailed, Node: node, Steps: steps, Usage: ledger.Total()},
			fmt.Errorf("graph: apply patch from %q: %w", node, err)
	}
	ledger.Record(node, exec.res.Usage)

	// Next is the journal's resume pointer, and ResolveStart reads it
	// verbatim — it never guesses the graph's shape. A paused node therefore
	// has to name itself: a pause is not a hand-off, and resuming means
	// re-running the gate now that a human has acted. Leaving it empty
	// resolves to no node at all and makes the paused run unresumable.
	next := ""
	switch exec.res.Status {
	case journal.StatusOK:
		next = exec.res.Next
	case journal.StatusPaused:
		next = node
	default:
		// StatusFailed and StatusAborted end the run; nothing follows them.
	}
	if _, err := jrnl.Append(journal.Record{
		Event: journal.EventNodeFinished, Node: node, Attempt: attempt,
		DurationMS: exec.elapsed.Milliseconds(), Status: exec.res.Status,
		Next: next, Usage: journalUsage(exec.res.Usage), Note: exec.res.Note,
	}); err != nil {
		return &Outcome{ExitCode: ExitFailed, Node: node, Steps: steps, Usage: ledger.Total()},
			fmt.Errorf("graph: journal node_finished for %q: %w", node, err)
	}
	logger.Info("belay/graph: node finished",
		slog.String("status", exec.res.Status.String()), slog.String("next", next),
		slog.Duration("elapsed", exec.elapsed))

	switch {
	case exec.res.Done():
		out, err := d.endRun(jrnl, man, ledger, journal.EventRunCompleted, state.RunStatusCompleted,
			ExitOK, End, steps, "run completed", nil)
		return &out, err
	case exec.res.Status == journal.StatusOK:
		if err := d.persist(man, state.RunStatusRunning, exec.res.Next, ledger); err != nil {
			return &Outcome{ExitCode: ExitFailed, Node: node, Steps: steps, Usage: ledger.Total()}, err
		}
		return nil, nil
	case exec.res.Status == journal.StatusPaused:
		note := exec.res.Note
		if note == "" {
			note = fmt.Sprintf("paused at %q", node)
		}
		out, err := d.endRun(jrnl, man, ledger, journal.EventRunPaused, state.RunStatusPaused,
			ExitOK, node, steps, note+"; use `belay resume` to continue", nil)
		return &out, err
	case exec.res.Status == journal.StatusAborted:
		out, err := d.endRun(jrnl, man, ledger, journal.EventRunAborted, state.RunStatusAborted,
			ExitAborted, node, steps, exec.res.Note,
			fmt.Errorf("%w: %q returned StatusAborted", ErrNodeAborted, node))
		return &out, err
	default: // journal.StatusFailed, returned cleanly as a verdict.
		out, err := d.endRun(jrnl, man, ledger, journal.EventRunFailed, state.RunStatusFailed,
			ExitFailed, node, steps, exec.res.Note,
			fmt.Errorf("%w: %q returned StatusFailed", ErrNodeFailed, node))
		return &out, err
	}
}

// pauseInsideNode pauses a run whose context was cancelled while the node was
// running, so that Resume can re-run it. It closes the interrupted attempt
// with the record heal writes for one a crash left open, then appends
// run_paused: the order journal.Validate requires. A crash between the two
// leaves that record last, which owedRunMarker owes nothing for, so the run is
// still resumable.
//
// Whatever the node returned is discarded, except what it spent. A node cut
// off part-way cannot vouch for its Result, and re-running it from the
// blackboard it started from is what the Node contract already makes safe;
// but the money the backend reported is gone whether or not the Result is
// used, so usage is recorded and journalled (see settle).
//
// A node that outlived graph.node_timeout is paused the same way, and for the
// same reason: closing the attempt as one to run again is what makes raising
// the limit and resuming run that node, instead of starting the whole run
// afresh. note and cause are the reason, which is all that differs between
// the two callers.
//
//nolint:revive // the parameter list is this record's shape, as terminal's is.
func (d *Dispatcher) pauseInsideNode(
	jrnl *journal.Journal, man *state.Manifest, ledger *budget.Ledger,
	node string, attempt, steps int, elapsed time.Duration, usage belay.Usage,
	note string, cause error, logger *slog.Logger,
) (*Outcome, error) {
	logger.Warn("belay/graph: pausing the run so this node runs again on resume",
		slog.String("note", note))

	ledger.Record(node, usage)
	if _, err := jrnl.Append(interruptedAttempt(node, attempt, elapsed, usage, note)); err != nil {
		return &Outcome{ExitCode: ExitFailed, Node: node, Steps: steps, Usage: ledger.Total()},
			fmt.Errorf("graph: journal node_finished for %q: %w", node, err)
	}

	out, err := d.pauseAtLimit(jrnl, man, ledger, node, steps, note, cause)
	return &out, err
}

// pauseAtLimit ends this dispatch by pausing the run at node rather than
// ending it: the state each of the dispatcher's own limits leaves a run in,
// and the one a cancellation leaves it in under Options.PauseOnCancel. It
// must not be called while a node_started is open; pauseInsideNode closes one
// first.
//
// The exit code stays ExitAborted. This invocation did stop short of where the
// graph would have taken it, and a script has to be able to tell that from a
// pause the graph chose, such as the approval gate (see ExitAborted).
// Outcome.Status is what says the run can be continued.
func (d *Dispatcher) pauseAtLimit(
	jrnl *journal.Journal, man *state.Manifest, ledger *budget.Ledger,
	node string, steps int, note string, cause error,
) (Outcome, error) {
	return d.endRun(jrnl, man, ledger, journal.EventRunPaused, state.RunStatusPaused,
		ExitAborted, node, steps, note, cause)
}

// limitNote is what a run paused by one of the dispatcher's own limits carries
// into the journal and the timeline: what stopped it, and which setting to
// raise to get past it. `belay run` writes the command out; this is what
// anyone reading the run's own record later sees.
func (d *Dispatcher) limitNote(cause error, setting string) string {
	return d.note(cause.Error()) + "; paused, so raising " + setting + " and resuming continues this run"
}

// pauseInterrupted ends this dispatch of a run its caller cancelled, pausing
// the run at node so that Resume continues it from there. It must not be
// called while a node_started is open; pauseInsideNode closes one first.
func (d *Dispatcher) pauseInterrupted(
	jrnl *journal.Journal, man *state.Manifest, ledger *budget.Ledger, node string, steps int,
) (Outcome, error) {
	return d.pauseAtLimit(jrnl, man, ledger, node, steps,
		fmt.Sprintf("interrupted at %q; use `belay resume` to continue", node),
		fmt.Errorf("%w: at %q: %w", ErrInterrupted, node, context.Canceled))
}

// terminal closes an open node_started with a node_finished carrying status,
// then ends the run with event. It exists because journal.Validate rejects a
// run-level record written while a node execution is still open, so every
// rejected path must close its node before it may end its run.
//
// usage is whatever the node reported spending before it was rejected, which
// terminal both journals and records: see settle on why spend survives a
// verdict the dispatcher throws away.
//
//nolint:revive // the parameter list is the terminal-record shape; a struct here would only move it.
func (d *Dispatcher) terminal(
	jrnl *journal.Journal, man *state.Manifest, ledger *budget.Ledger,
	node string, attempt, steps int, elapsed time.Duration, usage belay.Usage,
	status journal.Status, event journal.Event, runStatus state.RunStatus, exitCode int,
	note string, cause error, logger *slog.Logger,
) (*Outcome, error) {
	logger.Error("belay/graph: node ended the run",
		slog.String("status", status.String()), slog.String("note", note))

	ledger.Record(node, usage)
	if _, err := jrnl.Append(journal.Record{
		Event: journal.EventNodeFinished, Node: node, Attempt: attempt,
		DurationMS: elapsed.Milliseconds(), Status: status,
		Usage: journalUsage(usage), Note: note,
	}); err != nil {
		return &Outcome{ExitCode: ExitFailed, Node: node, Steps: steps, Usage: ledger.Total()},
			fmt.Errorf("graph: journal node_finished for %q: %w", node, err)
	}

	out, err := d.endRun(jrnl, man, ledger, event, runStatus, exitCode, node, steps, note, cause)
	return &out, err
}

// endRun appends the run-level marker that closes the journal, persists the
// manifest's final status, and builds the Outcome. It must never be called
// while a node_started is open.
//
//nolint:revive // as terminal: this is the run's closing-record shape.
func (d *Dispatcher) endRun(
	jrnl *journal.Journal, man *state.Manifest, ledger *budget.Ledger,
	event journal.Event, runStatus state.RunStatus, exitCode int,
	node string, steps int, note string, cause error,
) (Outcome, error) {
	out := Outcome{
		Status: runStatus, ExitCode: exitCode, Node: node,
		Steps: steps, Usage: ledger.Total(), Note: note,
	}

	if _, err := jrnl.Append(journal.Record{Event: event, Note: note}); err != nil {
		out.ExitCode = ExitFailed
		return out, fmt.Errorf("graph: journal %s: %w", event, err)
	}
	if err := d.persist(man, runStatus, node, ledger); err != nil {
		out.ExitCode = ExitFailed
		return out, err
	}
	return out, cause
}

// persist writes the manifest's lifecycle fields and the ledger's current
// snapshot. A later resume rebuilds the ledger from the journal, and uses this
// snapshot only as the floor beneath it (rebuildLedger); it is written after
// every node all the same, so `belay runs` and anything else that reads the
// manifest see the spend as it stands.
func (d *Dispatcher) persist(
	man *state.Manifest, status state.RunStatus, current string, ledger *budget.Ledger,
) error {
	man.Status = status
	man.CurrentNode = current
	man.Budget = manifestBudget(ledger.Snapshot(d.cfg.Budget.MaxUSD))
	man.Touch(d.now().UTC())
	if err := d.store.SaveManifest(*man); err != nil {
		return fmt.Errorf("graph: save manifest: %w", err)
	}
	return nil
}

// note sanitizes text the dispatcher composes from an error it did not author:
// it redacts through the configured Redactor, collapses whitespace onto one
// line so a journal record stays one line, and truncates.
func (d *Dispatcher) note(s string) string {
	s = strings.Join(strings.Fields(d.redact(s)), " ")
	if r := []rune(s); len(r) > maxNoteRunes {
		s = string(r[:maxNoteRunes]) + "..."
	}
	return s
}

// journalUsage converts a belay.Usage to the journal's own Usage shape. The
// result is always non-nil: the journal reads a present usage block as "this
// node's cost was accounted for", which the dispatcher always does, even when
// the amount is zero.
func journalUsage(u belay.Usage) *journal.Usage {
	return &journal.Usage{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		USD: u.USD, Estimated: u.Estimated,
	}
}

// rebuildLedger returns the ledger a dispatch of this run starts from: the
// Usage of every node_finished in records, with the manifest's budget block as
// a floor (budget.Rebuild).
//
// The journal is the source of truth for spend, as it is for everything else
// a resume decides. settle appends a node's node_finished, Usage and all, and
// only afterwards saves the manifest, so a crash between the two leaves a
// manifest that never heard of that node's cost. Restoring from the manifest,
// as the dispatcher once did, let a resumed run spend it again.
//
// Only node_finished carries Usage (journal.Record). A record with none — a
// journal written before usage was accounted for — contributes nothing, and
// the floor still carries whatever that run's manifest saved.
func rebuildLedger(records []journal.Record, saved state.Budget) *budget.Ledger {
	var entries []budget.NodeUsage
	for _, r := range records {
		if r.Event != journal.EventNodeFinished || r.Usage == nil {
			continue
		}
		entries = append(entries, budget.NodeUsage{Node: r.Node, Usage: belay.Usage{
			InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens,
			USD: r.Usage.USD, Estimated: r.Usage.Estimated,
		}})
	}
	return budget.Rebuild(entries, restoreSnapshot(saved))
}

// restoreSnapshot converts the manifest's budget block into the ledger's
// Snapshot shape. The two structs are declared separately, by documented
// agreement, so neither package has to import the other.
func restoreSnapshot(b state.Budget) budget.Snapshot {
	return budget.Snapshot{
		LimitUSD: b.LimitUSD, SpentUSD: b.SpentUSD,
		TokensIn: b.TokensIn, TokensOut: b.TokensOut, Estimated: b.Estimated,
	}
}

// manifestBudget is the inverse of restoreSnapshot.
func manifestBudget(s budget.Snapshot) state.Budget {
	return state.Budget{
		LimitUSD: s.LimitUSD, SpentUSD: s.SpentUSD,
		TokensIn: s.TokensIn, TokensOut: s.TokensOut, Estimated: s.Estimated,
	}
}
