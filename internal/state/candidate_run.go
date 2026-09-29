package state

import (
	"path/filepath"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// CapabilityFanoutCandidateRuns is the capability both documents of a
// fanout run declare in their requires list (ADR-0011).
//
// Manifest.Fanout and Manifest.Candidate are control fields. A belay that
// did not know the pin would decode a fanout run's manifest, drop it, and
// resume the run as an ordinary single run — spending the whole ceiling
// again on one attempt, and abandoning candidate workspaces and candidate
// runs nothing would ever collect. One that did not know the candidate block
// would be worse: the envelope lives there, so it would resume a candidate
// with no ceiling of its own and let it spend the parent's entire budget.
// Naming the capability turns both into a refusal that says to upgrade.
//
// One name covers both fields because they are one feature: a candidate run
// only ever exists under a pinned parent, and no belay will ever understand
// one without the other.
const CapabilityFanoutCandidateRuns = "fanout.candidate-runs"

// Envelope is what one candidate run may spend: a reservation taken out of
// the parent run's remaining ceiling when the candidate is claimed, and
// persisted in the candidate's own manifest so it survives a crash and an
// edit to belay.yaml.
//
// It is deliberately not a budget.Snapshot. A Snapshot's TokensIn and
// TokensOut are tokens already spent, and its LimitUSD is the run's own
// ceiling; an envelope is a cap in both dimensions and nothing else. The two
// were confused once, which left the token half of a candidate's reservation
// with nowhere to live and every candidate free to spend the parent's whole
// token budget. See internal/budget's Partition for the arithmetic that
// fills one in.
//
// A zero LimitUSD or LimitTokens means uncapped in that dimension, matching
// what budget.Ledger.Check makes of a zero ceiling. That is why fanout
// requires budget.max_usd above zero: an uncapped envelope per candidate is
// N uncapped runs.
type Envelope struct {
	// LimitUSD is the most this candidate run may spend, in dollars.
	LimitUSD float64 `json:"limit_usd"`
	// LimitTokens is the most this candidate run may spend, in input and
	// output tokens together.
	LimitTokens int64 `json:"limit_tokens"`
}

// Provision is how far a candidate's workspace got, and whether that is
// final.
//
// The distinction the three values draw is the one that decides whether a
// resume retries: a refusal is durable and is reproduced forever, while
// anything else is a fault a resume may be able to fix.
type Provision string

// The provisioning states a candidate run's manifest records.
const (
	// ProvisionPending is a candidate that has been claimed — its run
	// directory and envelope exist — but whose workspace does not yet. It is
	// the zero value, and so also what a torn write reads as: a resume
	// reclaims whatever a half-finished Create left and tries again.
	ProvisionPending Provision = ""
	// ProvisionOK is a candidate whose workspace exists and is recorded.
	ProvisionOK Provision = "ok"
	// ProvisionRefused is a candidate the isolator will never make a
	// workspace for here — an unsupported source, a dirty source, an id it
	// rejects. No resume changes any of those, so the refusal is durable:
	// adoption reproduces it from this manifest without calling the isolator
	// again, and fanout may settle it as an end state.
	ProvisionRefused Provision = "refused"
)

// CandidateRun is the manifest block that makes a run one fanout
// candidate's run.
//
// It is the whole of what a candidate run knows about its parent. Nothing
// here is read back from the parent's blackboard, so a candidate is
// adoptable from its own directory alone — which is what lets a resumed
// fanout collect a candidate that finished before the crash without running
// a single node of it.
type CandidateRun struct {
	// ParentRunID is the run this candidate belongs to. With ID it makes the
	// run's own RunID, <parent>-<id>, which is what the dispatcher and
	// anything reading the manifest call this run. The candidate's Layout
	// knows only the bare ID, which is its directory name.
	ParentRunID string `json:"parent_run_id"`
	// ID is the candidate's identifier, "cand-01". It is stable across
	// attempts of the fanout node, and bare rather than run-qualified: both
	// isolators are already rooted at one run's candidates directory, so two
	// runs cannot collide, and the promote node matches a Belay-Candidate
	// trailer against exactly this value.
	ID string `json:"id"`
	// Index is the candidate's 1-based position, which fixes where its
	// outcome lands in the parent blackboard's Candidates list however the
	// goroutines running them happen to finish.
	Index int `json:"index"`
	// Workspace is the tree this candidate works in, as the isolator
	// described it. Plain data, like every other workspace belay records, so
	// a resumed run can destroy it without the isolator that made it.
	Workspace belay.Workspace `json:"workspace"`
	// Provision is how far this candidate's workspace got.
	Provision Provision `json:"provision"`
	// ProvisionNote is why, in a sentence a person can act on, for a
	// candidate whose provisioning was refused. Empty otherwise.
	ProvisionNote string `json:"provision_note"`
	// Envelope is what this candidate run may spend. It is set when the
	// candidate is claimed and may afterwards only rise; see
	// budget.PartitionAtLeast.
	Envelope Envelope `json:"envelope"`
}

// FanoutPin is the parent manifest's record that this run fans out, and of
// the two things about the fanout that must not change under it.
//
// A run's shape is decided once, when it is created, and never re-read from
// live configuration (the rule internal/nodes states for every conditional
// node). Candidates decides the ids and the envelope split, so changing it
// mid-run would abandon claimed candidates or reserve more than the ceiling
// left; Select decides the winner, so changing it could make a resumed join
// pick a different candidate than the attempt that crashed reported. Neither
// is the dispatcher's live limits, which are deliberately still read live:
// see Manifest.PinnedConfig.
type FanoutPin struct {
	// Candidates is how many candidates this run fans out to.
	Candidates int `json:"candidates"`
	// Select names the strategy join picks the winner with, as
	// config.SelectStrategy spells it. It is a string rather than that type
	// so that a manifest written by a newer belay, naming a strategy this
	// one does not implement, is data to report rather than a decode error.
	Select string `json:"select"`
}

// SetFanout pins the shape of this run's fanout, and records the capability
// that pin depends on.
//
// It is one call rather than two for the reason SetOutput is: the field and
// its capability are one decision, and a caller that assigned Manifest.Fanout
// directly would write a manifest an older belay silently misreads, with
// nothing to catch it until that run was resumed and paid for all over
// again. See CapabilityFanoutCandidateRuns.
func (m *Manifest) SetFanout(p FanoutPin) {
	m.Fanout = &p
	m.Requires = addCapability(m.Requires, CapabilityFanoutCandidateRuns)
}

// SetCandidate records that this run is one fanout candidate's, and the
// capability that record depends on.
//
// Unlike SetOutput, there is no shape of the argument that requires nothing:
// a manifest with a candidate block is a candidate run, and every reader of
// one needs to understand the envelope it is bounded by.
func (m *Manifest) SetCandidate(c CandidateRun) {
	m.Candidate = &c
	m.Requires = addCapability(m.Requires, CapabilityFanoutCandidateRuns)
}

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

// PinnedConfig returns cfg with every value this run pinned when it was
// created overlaid on it: the configuration this run's nodes must read.
//
// It exists because pinning the registry is not enough. A node reads
// rc.Config, which the dispatcher fills from the file as this process loaded
// it, so a node that asks whether fanout is enabled, or which selection
// strategy to use, would get the answer belay.yaml gives today rather than
// the one the run was started with. Overlaying once, where the RunContext is
// assembled, means no node has to remember to ask the manifest, and a node
// that reads the live value by accident cannot be written.
//
// It overlays the graph's SHAPE and the run's guardrails, and nothing else.
// The dispatcher's limits —
// budget.max_usd, budget.max_tokens, graph.max_steps and graph.node_timeout
// — are deliberately read live, so that raising one and resuming continues a
// run that met it, and they are not touched here. Neither is
// fanout.isolator, which is not shape but how a workspace is made, and which
// the CLI has already resolved into the isolator itself.
//
// cfg is taken and returned by value, so the caller's configuration is never
// the thing that gets pinned.
func (m Manifest) PinnedConfig(cfg config.Config) config.Config {
	// The policy a run is held to comes from Manifest.Policy, never from the
	// file, so the live section is cleared rather than merely left unread. A
	// node that reached for rc.Config.Policy by accident would be judging
	// the change against a belay.yaml somebody may have rewritten since the
	// run began -- and clearing it here means such a node cannot be written.
	cfg.Policy = config.Policy{}
	if m.Fanout != nil {
		cfg.Fanout.Enabled = true
		cfg.Fanout.Candidates = m.Fanout.Candidates
		// Carried through as written, even when this belay does not know the
		// strategy: reporting an unknown name is join's to do, in one place,
		// and a default guessed here would silently pick a different winner.
		cfg.Fanout.Select = config.SelectStrategy(m.Fanout.Select)
	}
	return cfg
}

// SeedCandidateState returns the blackboard a candidate run starts from: a
// new one, carrying the parent's goal and the plan it approved, and nothing
// else.
//
// Everything left behind is left behind on purpose. Code.SessionID would
// have N candidates continue the parent's one agent conversation, and so
// each other's. Test, Review and Fix describe code no candidate wrote, and
// seeding them would let a candidate's first gate pass on the parent's
// results. Candidates and Winner would make a candidate look like a fanout,
// which it must never be. History belongs to a journal this run does not
// have.
//
// Plan.Regenerate is dropped as well: a candidate run has no plan node and
// no approve gate, so a request carried into one could never be served, and
// the run would declare a capability (CapabilityPlanRegenerate) for a field
// nothing in it reads.
func SeedCandidateState(parent State) State {
	seed := NewState(parent.Goal)
	seed.Plan = Plan{
		Path:     parent.Plan.Path,
		Approved: parent.Plan.Approved,
		Digest:   parent.Plan.Digest,
	}
	return seed
}
