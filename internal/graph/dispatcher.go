package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/belay-dev/belay/internal/budget"
	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
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
	// ExitAborted reports a run stopped by one of the dispatcher's guards
	// rather than by the work itself: the budget ceiling, the MaxSteps cap,
	// a node timeout, or cancellation of the caller's context.
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
	// a resume, as restored from the manifest.
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

	// Adapters, passed through to every RunContext. Any may be nil when the
	// configuration selects none; a node that needs a nil adapter is
	// responsible for returning a clear error.
	Agent    belay.AgentBackend
	Runner   belay.TestRunner
	Linter   belay.Linter
	Reviewer belay.Reviewer
	Isolator belay.Isolator
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
	logger   *slog.Logger
	now      func() time.Time
	redact   func(string) string

	agent    belay.AgentBackend
	runner   belay.TestRunner
	linter   belay.Linter
	reviewer belay.Reviewer
	isolator belay.Isolator
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

	d := &Dispatcher{
		store:    opts.Store,
		registry: opts.Registry,
		cfg:      opts.Config,
		logger:   opts.Logger,
		now:      opts.Now,
		redact:   opts.Redact,
		agent:    opts.Agent,
		runner:   opts.Runner,
		linter:   opts.Linter,
		reviewer: opts.Reviewer,
		isolator: opts.Isolator,
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

	// Restore, never New: a resumed run must keep accumulating spend from
	// where the crash left off. A fresh ledger would hand every crash a full
	// ceiling to spend again, turning a hard cap into a suggestion. For a
	// fresh run the manifest's budget block is zero, so Restore is exactly
	// New — which is why this is unconditional rather than a branch that
	// could be wrong.
	ledger := budget.Restore(restoreSnapshot(man.Budget))

	// Heal the run-level bookkeeping a crash can leave behind, before any new
	// record is appended. start is computed above, from the records as the
	// crash left them, because it is the only thing that still knows which
	// attempt died.
	healed, err := d.heal(jrnl, read.Records, &start)
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

	return d.loop(ctx, jrnl, &man, ledger, start)
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
//     Next so that a crash during recovery itself still resolves back here.
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
func (d *Dispatcher) heal(jrnl *journal.Journal, records []journal.Record, start *journal.Result) (bool, error) {
	if len(records) == 0 {
		return false, nil
	}
	last := records[len(records)-1]

	switch last.Event {
	case journal.EventNodeStarted:
		note := fmt.Sprintf("interrupted: no completion record for attempt %d (the process died inside this node); re-running", last.Attempt)
		if _, err := jrnl.Append(journal.Record{
			Event: journal.EventNodeFinished, Node: last.Node, Attempt: last.Attempt,
			Status: journal.StatusAborted, Next: last.Node,
			Usage: journalUsage(belay.Usage{}), Note: note,
		}); err != nil {
			return false, fmt.Errorf("graph: close interrupted %q: %w", last.Node, err)
		}
		d.logger.Warn("belay/graph: recovered an interrupted node",
			slog.String("node", last.Node), slog.Int("crashed_attempt", last.Attempt),
			slog.Int("next_attempt", start.Attempt))
		return true, nil

	case journal.EventNodeFinished:
		event, runState, okToHeal := owedRunMarker(last.Status)
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

// owedRunMarker maps a terminal node status onto the run-level record that
// should have followed it. A StatusOK node is not terminal — the loop decides
// what follows it — so it owes nothing.
func owedRunMarker(status journal.Status) (journal.Event, journal.RunState, bool) {
	switch status {
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
func (d *Dispatcher) loop(
	ctx context.Context, jrnl *journal.Journal, man *state.Manifest,
	ledger *budget.Ledger, start journal.Result,
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
			return d.endRun(jrnl, man, ledger, journal.EventRunAborted, state.RunStatusAborted,
				ExitAborted, current, steps, d.note(err.Error()),
				fmt.Errorf("graph: budget ceiling reached before %q: %w", current, err))
		}
		if d.cfg.Graph.MaxSteps > 0 && man.Step >= d.cfg.Graph.MaxSteps {
			err := fmt.Errorf("%w: %d node executions performed, limit is graph.max_steps=%d (raise it, or the graph is not converging)",
				ErrMaxSteps, man.Step, d.cfg.Graph.MaxSteps)
			return d.endRun(jrnl, man, ledger, journal.EventRunAborted, state.RunStatusAborted,
				ExitAborted, current, steps, d.note(err.Error()), err)
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

		logger := d.logger.With(
			slog.String("node", current), slog.Int("step", step), slog.Int("attempt", attempt))
		logger.Info("belay/graph: node started")

		layout := d.store.Layout()
		exec := d.execute(ctx, node, &RunContext{
			Goal: man.Goal, State: st, Config: d.cfg, Layout: layout,
			// Supplied, not derived: six nodes previously reconstructed
			// the workspace by walking up from the run directory, which
			// yields belay's own tree for a zero Layout.
			Workspace: layout.WorkspaceDir(),
			RunID:     man.RunID,
			Budget:    ledger.Snapshot(d.cfg.Budget.MaxUSD),
			NodeName:  current, Step: step, Attempt: attempt, Logger: logger,
			Agent: d.agent, Runner: d.runner, Linter: d.linter,
			Reviewer: d.reviewer, Isolator: d.isolator,
		})

		man.Step = step
		steps++

		out, err := d.settle(jrnl, man, ledger, node.Name(), attempt, started.Seq, steps, exec, logger)
		if out != nil {
			return *out, err
		}
		if err != nil {
			return Outcome{ExitCode: ExitFailed, Node: current, Steps: steps, Usage: ledger.Total()}, err
		}

		current, attempt = exec.res.Next, 1
	}
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
	cancel := context.CancelFunc(func() {})
	if timeout := d.cfg.Graph.NodeTimeout.Duration(); timeout > 0 {
		nodeCtx, cancel = context.WithTimeout(ctx, timeout)
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
	node string, attempt int, startedSeq uint64, steps int, exec nodeOutcome, logger *slog.Logger,
) (*Outcome, error) {
	// The three rejected paths. None of them applies a Patch or records
	// Usage: each means the Result is untrustworthy or absent, and a run
	// that ends here executes nothing further, so under-counting a failed
	// node's spend cannot let a later node overspend.
	switch {
	case exec.timedOut:
		err := fmt.Errorf("%w: %q exceeded graph.node_timeout=%s", ErrNodeTimeout, node, d.cfg.Graph.NodeTimeout)
		return d.terminal(jrnl, man, ledger, node, attempt, steps, exec.elapsed,
			journal.StatusAborted, journal.EventRunAborted, state.RunStatusAborted, ExitAborted,
			d.note(err.Error()), err, logger)
	case exec.canceled:
		err := fmt.Errorf("%w: %q canceled: %w", ErrNodeAborted, node, context.Canceled)
		return d.terminal(jrnl, man, ledger, node, attempt, steps, exec.elapsed,
			journal.StatusAborted, journal.EventRunAborted, state.RunStatusAborted, ExitAborted,
			d.note(err.Error()), err, logger)
	case exec.err != nil:
		return d.terminal(jrnl, man, ledger, node, attempt, steps, exec.elapsed,
			journal.StatusFailed, journal.EventRunFailed, state.RunStatusFailed, ExitFailed,
			nodeErrorNotePrefix+d.note(exec.err.Error()),
			fmt.Errorf("%w: %q returned an error: %w", ErrNodeFailed, node, exec.err), logger)
	}
	if err := exec.res.Validate(); err != nil {
		return d.terminal(jrnl, man, ledger, node, attempt, steps, exec.elapsed,
			journal.StatusFailed, journal.EventRunFailed, state.RunStatusFailed, ExitFailed,
			invalidResultNotePrefix+d.note(err.Error()),
			fmt.Errorf("%w: %q returned an unusable Result: %w", ErrNodeFailed, node, err), logger)
	}

	// Accepted. Apply the patch FIRST — see the ordering contract on
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

// terminal closes an open node_started with a node_finished carrying status,
// then ends the run with event. It exists because journal.Validate rejects a
// run-level record written while a node execution is still open, so every
// rejected path must close its node before it may end its run.
//
//nolint:revive // the parameter list is the terminal-record shape; a struct here would only move it.
func (d *Dispatcher) terminal(
	jrnl *journal.Journal, man *state.Manifest, ledger *budget.Ledger,
	node string, attempt, steps int, elapsed time.Duration,
	status journal.Status, event journal.Event, runStatus state.RunStatus, exitCode int,
	note string, cause error, logger *slog.Logger,
) (*Outcome, error) {
	logger.Error("belay/graph: node ended the run",
		slog.String("status", status.String()), slog.String("note", note))

	if _, err := jrnl.Append(journal.Record{
		Event: journal.EventNodeFinished, Node: node, Attempt: attempt,
		DurationMS: elapsed.Milliseconds(), Status: status,
		Usage: journalUsage(belay.Usage{}), Note: note,
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
// snapshot. The budget snapshot is what a later resume restores from, so it is
// written after every node rather than only at the end of the run.
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
