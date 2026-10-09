// Package fanout implements belay's best-of-N fanout node: the step that
// gives a run N independent candidate runs at the same goal so a later join
// node can keep the best one.
//
// # What this node does, and what it does not
//
// Run decides the fanout and asks rc.CandidateRuns to carry it out. It
// chooses how many candidates there are, what each is called, what each may
// spend, and what the blackboard each starts from holds; then it turns the
// end states that come back into state.Candidates, a summed Usage and a
// Note, and routes to graph.NodeJoin. That is all it does. It starts no
// goroutine, creates no workspace and writes no run file.
//
// # Why the concurrency is not here
//
// An earlier version of this node created the candidate workspaces itself,
// sequentially, and said so:
//
//	"the plan gives every candidate its own journal, and the v0.1 dispatcher
//	does not yet provide one [...] Concurrency is a dispatcher capability,
//	not a node one, and pretending otherwise here would produce N candidates
//	whose journals interleave into a single run journal that no resume could
//	untangle."
//
// That was right, and understated. A candidate needs more than a journal: a
// blackboard with one Code, Test, Review and Fix block; a ledger rebuilt
// from one journal; snapshots keyed on one journal's sequence numbers; a
// plan resolved through its own Layout. The unit of isolation is a RUN, not
// a journal — and internal/journal does not merely fail to untangle two
// candidates sharing one file, it rejects the second append and refuses the
// file from then on.
//
// So the two halves are split along the line ADR-0002 already draws
// everywhere else. The CAPABILITY — goroutines, child stores, child
// dispatchers, durable writes — lives behind graph.CandidateRunner, which
// the dispatcher builds and injects. The DECISION stays here, which is what
// keeps this node a table-testable function of its RunContext with no
// filesystem and no concurrency in the way.
//
// # Ids are bare, and stable across attempts
//
// A candidate is "cand-01", never "cand-01-a2". The id is derived from the
// candidate's index alone, so the fanout node's second attempt asks for
// exactly the candidates its first attempt asked for, and the runner adopts
// the candidate runs already on disk instead of paying for a second set
// beside them. Attempt-suffixed ids made every resumed fanout buy its work
// twice and left the first attempt's workspaces for nothing to collect.
//
// They are bare rather than run-qualified for a second reason: both
// isolators are rooted at one run's own candidates directory, so two runs
// cannot collide, and the promote node matches a Belay-Candidate trailer
// against exactly this value.
//
// # Cost
//
// Fanout is the one node that multiplies a run's bill by N, which is why
// ADR-0007 put a budget ceiling in belay on day one and why fanout.enabled
// defaults to false. Run therefore performs a mandatory budget PRE-check
// before it asks for anything — see ProjectCandidateCost for the projection
// and its justification, and Envelopes for the reservation that bounds the
// fanout once it is under way. Discovering the overrun after four of five
// candidates have already been paid for is exactly the failure the ceiling
// exists to prevent, so the check happens while refusing is still free. It is
// a check on launching, and an attempt that finds its candidates already
// claimed adopts them without it (see Run).
//
// # A Result only when every candidate has a durable end state
//
// A durable end state is one that re-deriving would reach again: a candidate
// run whose journal ends in run_completed, run_failed or run_aborted, or a
// candidate whose provisioning was REFUSED — an unsupported source, a dirty
// source, an id the isolator rejects — which no resume changes.
//
// Everything else is a fault a resume could fix: a full disk, a git index
// lock, a deadline, a panic. Run does not manufacture a verdict about one.
// It returns a paused Result with NO Patch at all, because state.Candidates
// is an absolute set and half a fanout recorded in it is a lie join would
// act on. The dispatcher records the pause the way it records the approve
// gate's, so belay resume re-runs this node, the finished candidates are
// adopted without executing anything, and only the failed provisioning is
// tried again.
//
// A candidate whose own run paused at a limit is neither of those. Its
// envelope, graph.max_steps and graph.node_timeout apply to a candidate run
// as to any run, and resuming without raising the limit meets it again,
// having run nothing. Run reports it as the limit it is: an error wrapping
// each candidate's cause, which the dispatcher pauses the run over with exit
// 3, naming each candidate, the reason its own journal gives and the setting
// to raise (see stoppedError). It once reported "its run is paused, not
// ended" with exit 0, as if a person had something to read, and a resume
// went round the same limit for ever.
//
// Every answer, settled or not, carries what the candidates have spent so far
// as Usage, a running total the dispatcher charges only the increase on
// (see spentBy), so the run's own record is right at every pause.
//
// # Errors versus results
//
// Per ADR-0002 a Result reports an outcome this node understands and an
// error reports that it could not reach a verdict at all. Fanout reached
// with fanout.enabled false, with fewer than two candidates, without a
// candidate runner, without a plan to give the candidates, or handed
// outcomes it cannot place are all in the second category: they are wiring
// or configuration bugs in the caller, not verdicts about the run's code.
// A budget refusal is an error too, and the dispatcher turns that one into a
// pause naming budget.max_usd, so raising the ceiling and resuming continues
// the run; so is a ceiling that is no longer set at all (budget.ErrNoCeiling),
// and so are candidates stopped at a limit. A runner that could not carry the
// fanout out at all, because it could not read the parent run, is a machine
// fault and pauses with a Result instead (see faulted).
package fanout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/dhaam-ai/belay/internal/budget"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// minCandidates is the smallest candidate count that means anything:
// "best of one" is just a run. config.Config.Validate already rejects a
// smaller value when fanout is enabled; Run re-checks rather than trusting
// that validation ran, because the cost of being wrong is N candidate runs.
const minCandidates = 2

// Sentinel errors returned by Run. Callers detect them with errors.Is.
//
// A budget refusal is not among them: it is reported with the
// *belay.BudgetError that internal/budget produced, so
// errors.Is(err, belay.ErrBudgetExceeded) and errors.As(err, &budgetErr)
// both keep working through this node.
var (
	// ErrDisabled reports that the fanout node ran while
	// fanout.enabled was false.
	//
	// This is deliberately an error rather than a quiet pass-through to
	// join. Reaching this node means the graph routed to it, and the only
	// way that happens with fanout disabled is a wiring bug — the run's
	// manifest pins the fanout, and state.Manifest.PinnedConfig overlays
	// that pin on the configuration this node reads, so even belay.yaml
	// turning fanout off between two attempts cannot produce it. Passing
	// through would hand join zero candidates and turn a clear bug at its
	// source into a confusing failure one node later; running the
	// candidates anyway would spend N times a run's budget the user
	// explicitly did not ask to spend. Failing here costs nothing and
	// names the actual problem.
	ErrDisabled = errors.New("fanout: node reached while fanout.enabled is false")

	// ErrTooFewCandidates reports a candidate count below minCandidates
	// while fanout is enabled.
	ErrTooFewCandidates = errors.New("fanout: candidate count is too low")

	// ErrRunMetadata reports that the run context does not carry what
	// fanout needs from it: the path of the repository candidates are
	// isolated from, and the run's spend so far, which is the only evidence
	// the budget pre-check has to work from. Failing here is deliberate — a
	// fanout that cannot see the ledger must not launch N candidates on the
	// assumption that it is affordable.
	ErrRunMetadata = errors.New("fanout: run metadata is unusable")

	// ErrNoPlan reports that the approved plan could not be read, so there
	// is nothing to give the candidates to work from.
	//
	// It is a refusal rather than a warning because a candidate run
	// resolves the plan through its OWN Layout: the parent's artifacts
	// directory is not one its code node can see, which is why the bytes
	// travel in the spec. A fanout that started N candidates without them
	// would buy N identical failures at the first agent call, which is the
	// spend the pre-check above exists to avoid.
	ErrNoPlan = errors.New("fanout: no plan to give the candidates")

	// ErrRunnerContract reports outcomes this node cannot place: fewer than
	// there are candidates, two for one position, or one for a position no
	// candidate has.
	//
	// It is an error rather than a partial Patch because state.Candidates
	// is an absolute set. Recording whatever could be placed would quietly
	// drop a candidate the run has already paid for, and leave join
	// choosing between fewer candidates than really ran — with no trace of
	// the one that went missing.
	ErrRunnerContract = errors.New("fanout: the candidate runner returned outcomes that cannot be placed")
)

// Node is the fanout graph node. It holds no state: everything it needs
// arrives in the graph.RunContext, so one value is safe to register once
// and reuse for the life of a process.
type Node struct{}

// New returns the fanout node.
func New() *Node { return &Node{} }

var (
	_ graph.Node       = (*Node)(nil)
	_ graph.Supervisor = (*Node)(nil)
)

// Name returns graph.NodeFanout.
func (n *Node) Name() string { return graph.NodeFanout }

// SupervisesCandidates exempts this node from config.Graph.NodeTimeout.
//
// What it waits on is other runs' work, and every node of every candidate
// run is capped by that same setting already. Applying it here as well would
// not make the run safer, it would make it wrong: the default node_timeout
// is 15 minutes, so any fanout worth running would cross it, and crossing it
// would cut off N candidates in the middle of work the run has already paid
// for. See graph.Supervisor, which argues the bound the exemption leaves in
// place.
func (n *Node) SupervisesCandidates() {}

// Run decides this run's fanout, has the candidate runner carry it out, and
// routes to graph.NodeJoin.
//
// The order of operations is the point of this function: every check that
// could refuse the fanout runs before anything is claimed, so a refusal
// costs nothing. Configuration and adapter guards come first because they
// are free, the plan read next because it is free too, the budget pre-check
// after them because it is the one that stops real money, and only then is a
// single candidate allowed to run.
func (n *Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	count, err := checkConfig(rc)
	if err != nil {
		return graph.Result{}, err
	}

	src, spent, err := runMetadata(rc)
	if err != nil {
		return graph.Result{}, err
	}

	plan, err := readPlan(rc)
	if err != nil {
		return graph.Result{}, err
	}

	// The projection decides whether to launch N candidates, so it is asked
	// only of a fanout that has not launched any. Once an earlier attempt
	// has claimed them, each holds an envelope that is reserved, persisted
	// and never lowered, so refusing to adopt them takes no money back: it
	// only strands work the run has already paid for until someone raises a
	// ceiling that was never the obstacle.
	claimed, err := rc.CandidateRuns.Claimed(candidateIDs(count))
	if err != nil {
		return faulted(logger(rc), fmt.Errorf("could not tell whether the candidates are already claimed: %w", err),
			count, belay.Usage{}), nil
	}
	var projected belay.Usage
	if !claimed {
		if projected, err = affordable(rc, count, spent); err != nil {
			return graph.Result{}, err
		}
	}

	// The whole remaining ceiling is divided here, once, before a single
	// candidate is claimed — see Envelopes. It happens after the pre-check
	// above rather than instead of it: the pre-check refuses a fanout this
	// run's own history says it cannot afford, and the split refuses one
	// the ceiling has no room left for at all.
	envelope, err := Envelopes(rc.Budget, rc.Config.Budget, count)
	if err != nil {
		return graph.Result{}, err
	}

	// Asked to stop before anything is claimed, stop. The dispatcher would
	// pause this attempt anyway; starting N candidate runs first would make
	// that pause cost N workspaces.
	if err := ctx.Err(); err != nil {
		return graph.Result{}, fmt.Errorf("fanout: cancelled before claiming any candidate: %w", err)
	}

	specs := buildSpecs(rc, src, plan, envelope, count)

	log := logger(rc)
	log.Info("fanout: budget pre-check passed, running candidates",
		"candidates", count,
		"adopting", claimed,
		"src", src,
		"spent_usd", spent.USD,
		"projected_usd_per_candidate", projected.USD,
		"envelope_usd", envelope.LimitUSD,
		"envelope_tokens", envelope.LimitTokens,
		"max_usd", rc.Config.Budget.MaxUSD,
	)

	outcomes, err := rc.CandidateRuns.Run(ctx, specs)
	if err != nil {
		// What the candidates that ran reported spending travels with every
		// answer (see spentBy). A candidate the runner never reached reports
		// nothing this time, so the figure can be short of the truth; the
		// dispatcher charges only the increase on what earlier attempts
		// recorded, so a short figure is never counted twice, and the next
		// complete one makes up the difference.
		if ctx.Err() != nil {
			// The caller's own cancellation: the dispatcher decides what
			// that is, and pauses the run as interrupted.
			return graph.Result{Usage: spentBy(outcomes)}, fmt.Errorf("fanout: running %d candidates: %w", count, err)
		}
		return faulted(log, err, count, spentBy(outcomes)), nil
	}

	placed, err := place(outcomes, specs)
	if err != nil {
		return graph.Result{}, err
	}
	// What the candidates have spent so far travels with every answer
	// below, settled or not: see spentBy.
	soFar := spentBy(placed)
	if stopped := atLimit(placed); len(stopped) > 0 {
		return graph.Result{Usage: soFar}, stoppedError(log, stopped, notDurable(placed), count)
	}
	if open := notDurable(placed); len(open) > 0 {
		return paused(log, open, count, soFar), nil
	}
	return settled(log, placed), nil
}

// buildSpecs turns the decision into the N specs the runner executes.
//
// Every candidate gets the same envelope and the same seed: the fanout is N
// tries at ONE goal, and a split that favoured one candidate would be
// choosing a winner before any of them had run.
func buildSpecs(rc *graph.RunContext, src string, plan []byte, envelope state.Envelope, count int) []graph.CandidateSpec {
	seed := state.SeedCandidateState(rc.State)
	specs := make([]graph.CandidateSpec, 0, count)
	for i := 1; i <= count; i++ {
		specs = append(specs, graph.CandidateSpec{
			ID:       candidateID(i),
			Index:    i,
			Source:   src,
			Envelope: envelope,
			Seed:     seed,
			Plan:     plan,
		})
	}
	return specs
}

// place puts each outcome in its candidate's position, so the blackboard
// records candidates in index order however the goroutines that ran them
// happened to finish.
//
// Index rather than ID is the key, because Index is what the spec defined
// the position as; the ID is checked against it so that an outcome carrying
// the right slot and the wrong name cannot be filed under a candidate the
// node never asked for.
func place(outcomes []graph.CandidateOutcome, specs []graph.CandidateSpec) ([]graph.CandidateOutcome, error) {
	placed := make([]graph.CandidateOutcome, len(specs))
	filled := make([]bool, len(specs))
	for _, o := range outcomes {
		i := o.Index - 1
		if i < 0 || i >= len(specs) {
			return nil, fmt.Errorf("%w: an outcome for candidate %q claims position %d of %d",
				ErrRunnerContract, o.ID, o.Index, len(specs))
		}
		if filled[i] {
			return nil, fmt.Errorf("%w: two outcomes claim position %d, the second for candidate %q",
				ErrRunnerContract, o.Index, o.ID)
		}
		if o.ID != specs[i].ID {
			return nil, fmt.Errorf("%w: position %d belongs to candidate %q, but its outcome names %q",
				ErrRunnerContract, o.Index, specs[i].ID, o.ID)
		}
		placed[i] = o
		filled[i] = true
	}
	for i, ok := range filled {
		if !ok {
			return nil, fmt.Errorf("%w: no outcome for candidate %q; %d of %d candidates reported",
				ErrRunnerContract, specs[i].ID, len(outcomes), len(specs))
		}
	}
	return placed, nil
}

// spentBy is what the candidates in outcomes have spent between them, as
// their own journals record it.
//
// It is a running total, and every answer Run gives carries it: settled,
// paused over a fault, stopped at a limit, or cut short. The dispatcher
// charges a supervising node only what no earlier attempt of it was charged
// (graph's supervises), so the parent's ledger, its manifest and
// `belay runs` say what the run has really spent at every pause rather than
// only once the fanout settles, and nothing is counted twice. Estimated is
// sticky, as everywhere else belay totals money.
func spentBy(outcomes []graph.CandidateOutcome) belay.Usage {
	var usage belay.Usage
	for _, o := range outcomes {
		usage.InputTokens += o.Spent.InputTokens
		usage.OutputTokens += o.Spent.OutputTokens
		usage.USD += o.Spent.USD
		usage.Estimated = usage.Estimated || o.Spent.Estimated
	}
	return usage
}

// atLimit returns the candidates whose own runs paused at one of belay's
// limits, in index order.
func atLimit(outcomes []graph.CandidateOutcome) []graph.CandidateOutcome {
	var stopped []graph.CandidateOutcome
	for _, o := range outcomes {
		if o.Err == nil && o.Stopped != nil {
			stopped = append(stopped, o)
		}
	}
	return stopped
}

// stoppedError reports a fanout some of whose candidates' runs paused at a
// limit belay enforces on each of them: the envelope, graph.max_steps or
// graph.node_timeout.
//
// It is an error, not a paused Result, and that is the decision. A paused
// Result is how a node asks a person to look at something and resume, and
// the dispatcher exits 0 for it; this fanout cannot go on until a setting is
// raised, and resuming without raising it meets the same limit again, having
// run nothing. The dispatcher pauses the run over an error wrapping one of
// its limits (graph.LimitSettings), with exit 3 and the setting in its note,
// exactly as it does for its own. The error wraps every candidate's cause, so
// errors.Is finds each limit, and its text names each candidate, the reason
// its own journal gives, and the setting that would get it further.
//
// Candidates with a fault of their own are named too: the resume that follows
// raising the limit retries them as well.
func stoppedError(log *slog.Logger, stopped []graph.CandidateOutcome, faults []string, count int) error {
	parts := make([]string, 0, len(stopped))
	causes := make([]error, 0, len(stopped))
	for _, o := range stopped {
		parts = append(parts, fmt.Sprintf("%s stopped at %s, so raise %s: %s",
			o.ID, limitName(o.Stopped), strings.Join(graph.LimitSettings(o.Stopped), " and "),
			graph.StoppedReason(o.Note)))
		causes = append(causes, o.Stopped)
	}
	msg := fmt.Sprintf("fanout: %d of %d candidates stopped at a limit belay applies to each candidate run: %s",
		len(stopped), count, strings.Join(parts, "; "))
	if len(faults) > 0 {
		msg += fmt.Sprintf("; and %d could not be finished for another reason, which resuming retries: %s",
			len(faults), strings.Join(faults, "; "))
	}
	log.Warn("fanout: candidates stopped at a limit, so this fanout is not settled",
		"stopped", len(stopped), "faults", len(faults), "candidates", count)
	return &limitError{msg: msg, causes: causes}
}

// limitName says which limit a candidate's run stopped at, in the words a
// person reading the parent's note needs: its envelope is not a setting of
// its own, and budget.max_usd is what raises it.
func limitName(err error) string {
	switch {
	case errors.Is(err, belay.ErrBudgetExceeded):
		return "its envelope"
	case errors.Is(err, graph.ErrMaxSteps):
		return "graph.max_steps, which counts each candidate's steps"
	case errors.Is(err, graph.ErrNodeTimeout):
		return "graph.node_timeout, which caps each candidate's steps"
	default:
		return "a limit"
	}
}

// limitError is stoppedError's error: a sentence of its own, wrapping every
// candidate's cause.
type limitError struct {
	msg    string
	causes []error
}

func (e *limitError) Error() string   { return e.msg }
func (e *limitError) Unwrap() []error { return e.causes }

// notDurable returns the candidates whose end state a resume could still
// change, in index order, each with the reason it is not final. A candidate
// stopped at a limit is not among them: atLimit reports it.
func notDurable(outcomes []graph.CandidateOutcome) []string {
	var open []string
	for _, o := range outcomes {
		switch {
		case o.Err == nil && o.Stopped != nil:
			// Reported by atLimit, as the limit it is.
		case o.Err != nil:
			open = append(open, fmt.Sprintf("%s: %s", o.ID, o.Err))
		case o.Provision == state.ProvisionRefused:
			// Durable: the isolator will refuse identically forever.
		case durableStatus(o.Status):
			// Durable: its journal carries a run marker.
		default:
			open = append(open, fmt.Sprintf("%s: its run is %s, not ended", o.ID, o.Status))
		}
	}
	return open
}

// durableStatus reports whether a candidate run's own journal ended it.
//
// Paused is deliberately absent. A paused candidate is the one thing a
// resume is certain to be able to move, and settling it as though it had
// ended would throw away a run that is only waiting to be asked again.
func durableStatus(s state.RunStatus) bool {
	switch s {
	case state.RunStatusCompleted, state.RunStatusFailed, state.RunStatusAborted:
		return true
	default:
		return false
	}
}

// paused reports that the fanout cannot be settled yet, with no Patch.
//
// The empty Patch is the point, and it is not an omission: state.Patch
// fields are absolute sets, so recording the candidates that did end would
// hand join a partial fanout and let it pick a winner out of a field the run
// has not finished paying for. Usage is what the candidates have spent so
// far, the running total spentBy describes.
func paused(log *slog.Logger, open []string, count int, spent belay.Usage) graph.Result {
	log.Warn("fanout: some candidates have no durable end state, so this fanout is not settled",
		"unfinished", len(open), "candidates", count, "reasons", strings.Join(open, "; "))
	return graph.Result{
		Status: journal.StatusPaused,
		Usage:  spent,
		Note: fmt.Sprintf("%d of %d candidates could not be finished: %s; resuming retries only those",
			len(open), count, strings.Join(open, "; ")),
	}
}

// faulted reports a fanout the candidate runner could not carry out at all,
// with no Patch: it could not read the parent run it claims against, or tell
// whether the candidates were already claimed.
//
// That is a machine fault, and a machine fault pauses, as a candidate's own
// fault does (see paused). It was once returned as an error, which the
// dispatcher ends a run over for good, so an unreadable file put every
// candidate the run had already paid for out of reach. Nothing about the
// candidates was decided, so there is nothing to record, and a resume tries
// again.
func faulted(log *slog.Logger, fault error, count int, spent belay.Usage) graph.Result {
	log.Warn("fanout: the candidate runs could not be carried out, so this fanout is paused",
		"candidates", count, "error", fault.Error())
	return graph.Result{
		Status: journal.StatusPaused,
		Usage:  spent,
		Note: fmt.Sprintf("the %d candidates could not be run: %v; nothing about them was decided, and resuming tries again",
			count, fault),
	}
}

// settled turns N durable end states into the blackboard join reads, the
// fanout's bill, and a sentence describing what happened.
func settled(log *slog.Logger, outcomes []graph.CandidateOutcome) graph.Result {
	candidates := make([]state.Candidate, 0, len(outcomes))
	usage := spentBy(outcomes)
	passed := 0
	for _, o := range outcomes {
		c := asCandidate(o)
		if c.TestPassed {
			passed++
		}
		candidates = append(candidates, c)
	}

	log.Info("fanout: every candidate reached a durable end state",
		"candidates", len(candidates), "passed", passed, "usd", usage.USD, "next", graph.NodeJoin)

	return graph.Result{
		Next:   graph.NodeJoin,
		Status: journal.StatusOK,
		Patch:  state.Patch{Candidates: &candidates},
		Usage:  usage,
		Note: fmt.Sprintf("%d candidate runs ended (%s); %d passed their tests",
			len(candidates), tally(candidates), passed),
	}
}

// asCandidate projects one candidate run's end state onto the blackboard
// shape join compares.
//
// Dir and Ephemeral come from the workspace the isolator really made, not
// from any path this node could compute: Dir is what a human or the promote
// node takes the winner's changes from, and Ephemeral is the only evidence
// that destroying it is safe. TestPassed is derived rather than carried,
// because "passed" has one definition in belay — a test run that ran
// something and failed none of it — and a candidate that never reached its
// test node must not read as passing. PolicyViolations is what stops
// best-of-N being the way around a guardrail; see join's eligible.
func asCandidate(o graph.CandidateOutcome) state.Candidate {
	return state.Candidate{
		ID:         o.ID,
		Dir:        o.Workspace.Dir,
		TestPassed: o.State.Test.Total > 0 && o.State.Test.Failed == 0,
		IssueCount: len(o.State.Review.Issues),
		// Counted from the candidate's own blackboard, like its review
		// issues: join refuses a candidate that broke a guardrail, and a
		// count assembled anywhere else could disagree with the verdict the
		// candidate's gate actually recorded.
		PolicyViolations: len(o.State.Policy.Violations),
		// What the candidate's own write step recorded and its gate judged,
		// so promote can commit the winner's work and nothing beyond it.
		ChangedFiles: slices.Clone(o.State.Code.ChangedFiles),
		Unreported:   slices.Clone(o.State.Code.Unreported),
		Ephemeral:    o.Workspace.Ephemeral,
		Outcome:      outcomeWord(o),
		RunDir:       o.RunDir,
		SpentUSD:     o.Spent.USD,
		Estimated:    o.Spent.Estimated,
		DurationMS:   o.DurationMS,
		Note:         o.Note,
	}
}

// outcomeWord names how a candidate ended, in the vocabulary
// state.Candidate.Outcome documents.
//
// A refused candidate is "error" whatever its run status says, because it
// never ran: there is no verdict about code in it, and reporting the
// "failed" its empty run directory would otherwise imply would tell a reader
// that N attempts at their goal failed when in fact none of them was made.
func outcomeWord(o graph.CandidateOutcome) string {
	if o.Provision == state.ProvisionRefused {
		return "error"
	}
	return o.Status.String()
}

// tally counts the candidates by outcome for the Note, in a fixed order so
// that two attempts over the same end states produce the same sentence.
func tally(candidates []state.Candidate) string {
	counts := make(map[string]int, 4)
	for _, c := range candidates {
		counts[c.Outcome]++
	}
	words := make([]string, 0, len(counts))
	for word := range counts {
		words = append(words, word)
	}
	sort.Strings(words)
	parts := make([]string, 0, len(words))
	for _, word := range words {
		parts = append(parts, fmt.Sprintf("%d %s", counts[word], word))
	}
	return strings.Join(parts, ", ")
}

// candidateID names candidate index, 1-based.
//
// It is a pure function of the index: no attempt number, no run id. See the
// package doc's "Ids are bare, and stable across attempts" for both halves
// of that argument — adopt-or-create on resume, and promote's bare
// Belay-Candidate trailer.
func candidateID(index int) string { return fmt.Sprintf("cand-%02d", index) }

// candidateIDs names the first count candidates, in index order.
func candidateIDs(count int) []string {
	ids := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		ids = append(ids, candidateID(i))
	}
	return ids
}

// readPlan returns the bytes of the approved plan, which every candidate run
// starts from.
//
// The path is the one the blackboard records, resolved through the run's own
// Layout, and anything that is not a bare artifact name is refused rather
// than repaired — the same position state.Layout takes on a path segment it
// does not like. A blank plan is refused too: it would reach every candidate
// as an empty prompt and buy N empty answers.
func readPlan(rc *graph.RunContext) ([]byte, error) {
	raw := strings.TrimSpace(rc.State.Plan.Path)
	name := strings.TrimPrefix(path.Clean(raw), "artifacts/")
	if raw == "" || name == "" || name == "." || strings.Contains(name, "/") {
		return nil, fmt.Errorf("%w: the blackboard records the plan at %q, which is not an artifact of this run", ErrNoPlan, rc.State.Plan.Path)
	}
	data, err := rc.ReadArtifact(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoPlan, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("%w: %q is empty", ErrNoPlan, rc.State.Plan.Path)
	}
	return data, nil
}

// ProjectCandidateCost projects a floor under what one fanout candidate will
// cost, given spent, the run's cumulative usage up to this node.
//
// # A lower bound, kept as a refuse-early floor
//
// The projection is the run to date, replayed once per candidate: what this
// run has cost so far is the only evidence about this goal, this repository
// and this model that belay has, and no price table lives here on purpose —
// pricing belongs to the agent backend, which is the only layer that knows
// which model ran (see internal/budget's Record).
//
// It used to be argued as a high estimate, on the premise that "this run
// reached fanout by planning and coding once". That premise is gone: under
// the fanout route the run reaches this node straight after approve, so the
// spend before it is the plan alone, and a candidate that codes, tests,
// fixes and reviews will cost several times it. So this is a weak LOWER
// bound, and it is kept for exactly what a lower bound is good for — a
// fanout it already refuses is one that could never have fitted. What
// actually bounds a candidate once it is running is its envelope
// (see Envelopes), which is a cap rather than a guess.
//
// The result is always marked Estimated: it is an extrapolation from this
// run's own history, never a quote.
//
// # Its blind spot
//
// A run that has spent nothing yet — replay mode, or a graph that reaches
// fanout before any agent call — projects zero, and zero is affordable
// against any ceiling. With no spend recorded there is nothing to
// extrapolate from, and inventing a number would be a price table by
// another name. Run logs a warning in that case rather than refusing,
// because a $0 run so far is a legitimate way to run belay, not evidence
// of an impending overrun — and the envelope still caps every candidate.
func ProjectCandidateCost(spent belay.Usage) belay.Usage {
	return belay.Usage{
		// Negative components cannot come from a well-behaved ledger, but
		// they can come from a hand-edited manifest.json, and a negative
		// projection would make every fanout look affordable.
		InputTokens:  max(0, spent.InputTokens),
		OutputTokens: max(0, spent.OutputTokens),
		USD:          max(0, spent.USD),
		Estimated:    true,
	}
}

// affordable is the mandatory budget pre-check: it refuses count candidates
// whose projected cost would not fit under the run's ceiling, before a
// single candidate run exists.
//
// It refuses regardless of budget.on_exceed. That is a deliberate
// departure from Ledger.Check, which honours OnExceedWarn and lets a run
// continue past its ceiling: "warn" means the user accepted watching a run
// drift over budget, not that they accepted multiplying its burn rate by N
// at the moment it is already there. Ledger.CanAfford — the committed
// contract this calls — takes the same position.
func affordable(rc *graph.RunContext, count int, spent belay.Usage) (belay.Usage, error) {
	projected := ProjectCandidateCost(spent)
	if projected.USD == 0 && projected.InputTokens == 0 && projected.OutputTokens == 0 {
		logger(rc).Warn("fanout: no spend recorded yet, so the per-candidate cost projection is zero and the budget pre-check cannot bound this fanout",
			"candidates", count,
			"max_usd", rc.Config.Budget.MaxUSD,
		)
	}

	snapshot := budget.Snapshot{
		LimitUSD:  rc.Config.Budget.MaxUSD,
		SpentUSD:  spent.USD,
		TokensIn:  spent.InputTokens,
		TokensOut: spent.OutputTokens,
		Estimated: spent.Estimated,
	}
	if err := budget.Restore(snapshot).CanAfford(count, projected, rc.Config.Budget); err != nil {
		return belay.Usage{}, fmt.Errorf("fanout: refusing to launch %d candidates projected at $%.4f each on top of $%.4f already spent: %w",
			count, projected.USD, spent.USD, err)
	}
	return projected, nil
}

// runMetadata reads the two things this node needs about the run itself: the
// repository candidates are isolated from, and the run's spend so far.
//
// Both facts arrive on the RunContext. This node used to read manifest.json
// itself, which worked but made a node depend on a file the dispatcher owns
// — and ADR-0002 keeps that ownership in one place precisely so a node
// cannot disagree with it.
func runMetadata(rc *graph.RunContext) (src string, spent belay.Usage, err error) {
	if rc.Workspace == "" {
		return "", belay.Usage{}, fmt.Errorf("%w: run context names no workspace to isolate candidates from", ErrRunMetadata)
	}
	return rc.Workspace, belay.Usage{
		InputTokens:  rc.Budget.TokensIn,
		OutputTokens: rc.Budget.TokensOut,
		USD:          rc.Budget.SpentUSD,
		Estimated:    rc.Budget.Estimated,
	}, nil
}

// checkConfig validates everything Run can check without touching disk or
// spending anything, and returns the candidate count to fan out to.
//
// rc.Config is the PINNED configuration: state.Manifest.PinnedConfig has
// already overlaid the shape this run was created with, so these are checks
// against what the run agreed to, not against belay.yaml as it is now.
func checkConfig(rc *graph.RunContext) (int, error) {
	if !rc.Config.Fanout.Enabled {
		return 0, ErrDisabled
	}
	if n := rc.Config.Fanout.Candidates; n < minCandidates {
		return 0, fmt.Errorf("%w: fanout.candidates is %d, want at least %d", ErrTooFewCandidates, n, minCandidates)
	}
	if rc.CandidateRuns == nil {
		return 0, fmt.Errorf("%w: a run routed to fanout must be given a candidate registry", graph.ErrNoCandidateRunner)
	}
	return rc.Config.Fanout.Candidates, nil
}

// logger returns rc's logger, falling back to the default one. The
// dispatcher always sets Logger; a test building a RunContext by hand
// often does not, and a nil-pointer panic is never the right answer to a
// missing log destination.
func logger(rc *graph.RunContext) *slog.Logger {
	if rc.Logger != nil {
		return rc.Logger
	}
	return slog.Default()
}
