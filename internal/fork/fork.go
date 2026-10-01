// Package fork makes a fork: a new run, with an ID of its own, that re-runs
// one node of an existing run from the blackboard as it stood before that
// node, without paying again for the nodes that came before it.
//
// It is the only code in belay that reads one run in order to write another,
// and it is built in two halves so that the reading can be shown before any
// writing happens. Prepare reads the source and decides everything: which
// visit, from which snapshot, on which files, differing from the original in
// which ways. It writes nothing, which is what lets `belay fork --dry-run`
// describe a fork exactly. Spec.Create then makes it.
//
// # Fork, never amend
//
// The source run is read and never written: not a note in its journal, not a
// healed tail, not an upgraded manifest, not a snapshot. Two reasons, either
// enough on its own. A journal is the run's audit record, and one that a fork
// could quietly rewrite is not an audit record. And a journal is also where
// the run goes next: journal.ResolveStart decides from its last records, so
// a single record appended after run_failed would turn a run that ended into
// one belay resume picks up again.
//
// So this package never builds a journal.Journal or a state.Store for the
// source. It reads the journal with journal.ReadFile, which repairs nothing,
// and never with journal.Open, which heals a torn final line by rewriting the
// file. It loads the manifest and the snapshot with their Load functions,
// and never calls state.Store.Preflight, which saves a manifest written by an
// older belay at the current schema version. The source can only be read
// here, by construction.
//
// # What a fork is made of
//
// A fork is an ordinary run, and every belay command that works on a run
// works on it.
//
//   - Its workspace is a directory copy of the tree the source run's nodes
//     worked in, made by the caller's isolator under the source workspace's
//     .belay/forks/<fork-id>, and its run directory is inside that copy. A
//     fork never runs in anybody's checkout, and `belay --workspace <copy>`
//     finds it as it finds any run. It always edits its copy in place and
//     never commits: there is no branch for it to commit to.
//
//   - Its journal begins with the source's records from before the visit,
//     then a run_started marking where the fork's own history begins. It
//     stops short of the visit's own node_started, which the fork writes
//     itself when it runs the node, at attempt 1. A copy that kept it would
//     read as a node that died while running, and the dispatcher would
//     "recover" it as attempt 2, from the snapshot of the wrong execution.
//     Seqs carry on from the source's, so the History entries the fork
//     inherits on its blackboard, which are keyed on Seq, can never collide
//     with the ones it adds.
//
//   - Its blackboard is the visit's snapshot (state.Snapshot): exactly what
//     the node was given. Not state.json, which holds the blackboard as the
//     run left it, after the visit; there is no falling back on it for a run
//     recorded before snapshots existed, because a fork from the wrong
//     blackboard is a re-run of a step that never happened.
//
//   - Its step count carries on from the source's too, so the node runs as
//     the same step it ran as, and its evidence lands where the source's did
//     in its own run directory. A step number is the run's identity, not its
//     spending.
//
//   - Its ledger starts at zero. Every dollar belongs to exactly one run: the
//     inherited records are written without their usage, because the
//     dispatcher rebuilds a run's ledger from its journal (ADR-0007), and the
//     amount is reported in the manifest's state.Origin instead. Charging a
//     fork for its source's spending would count the same money twice, and
//     could stop it at a ceiling it had not spent towards.
//
// # The inputs belay cannot put back
//
// A snapshot holds the blackboard and nothing else, and a node is given more
// than the blackboard. Each of the rest is checked, and anything that differs
// from what the visit was given is said before a cent is spent
// (Spec.Deviations), and recorded in the fork's manifest.
//
//   - The files. belay keeps no copy of a tree as it was, only a fingerprint
//     of it beside each snapshot (graph.Options.Fingerprint), so a fork can
//     only tell whether the tree has changed since. If it has, re-running the
//     node on today's files is a different experiment, and the fork asks
//     first: it refuses unless the caller accepts the files as they are now
//     (--current-files). A fingerprint belay did not record, or one recorded
//     by a different scheme, is no evidence that nothing changed, so it is
//     treated as changed; the scheme is compared before the digest, because
//     "recorded by a different belay" and "your files changed" are different
//     answers.
//   - The agent's conversation is not carried over. The source's went on
//     after the visit, and resuming it would show the node a conversation
//     from its own future. The fork's blackboard names no session, so the
//     next agent call starts a new one.
//   - The plan, which the nodes before code read and code takes as its
//     instructions. code is refused a plan that is not the one the gate
//     approved, because it would build something nobody approved; the plan
//     node and the gate judge whatever plan is there themselves, so for them a
//     changed plan is said, not refused.
//   - The configuration, which the caller resolves now. Each setting that
//     differs from what the source was started with is said, by name, with
//     both values. Those a fork does not take from belay.yaml are left out:
//     the policy it inherits pinned from its source, and output, archival and
//     fanout, which every fork turns off.
//
// # What it refuses
//
//   - fanout, join and promote. Fanout and join run whole candidate runs, which
//     live in the source run's directory, and a fork never touches that.
//     Promote commits to the run's branch, and a fork has none.
//   - A source that may still be running: a manifest that says running, or a
//     journal whose last record was cut off. A step may still be changing the
//     files a fork would copy.
//   - A visit with no snapshot, which is every visit of a run recorded before
//     snapshots existed, and every visit a fork inherited rather than ran.
//     The refusal names where the visit was really run.
package fork

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/state"
)

// The refusals Prepare and Create make. Each arrives as a *Refusal, whose
// message is a sentence about the run in question; errors.Is matches it
// against one of these.
var (
	// ErrSourceActive means the source run may still be running.
	ErrSourceActive = errors.New("fork: the source run may still be running")
	// ErrNoSuchVisit means the source run never made the visit asked for.
	ErrNoSuchVisit = errors.New("fork: the source run has no such visit")
	// ErrUnsupportedNode means the node is one a fork cannot re-run.
	ErrUnsupportedNode = errors.New("fork: belay cannot fork this node")
	// ErrNoSnapshot means the source run kept no snapshot of the visit.
	ErrNoSnapshot = errors.New("fork: the source run kept no snapshot of that visit")
	// ErrFilesChanged means the tree may not be the one the visit saw, and
	// the caller did not accept it as it is now.
	ErrFilesChanged = errors.New("fork: the files have changed since that visit")
	// ErrPlanChanged means code would be given a plan that is not the one the
	// gate approved.
	ErrPlanChanged = errors.New("fork: the plan has changed since that visit")
	// ErrNoPlan means the plan the node reads cannot be found.
	ErrNoPlan = errors.New("fork: the plan that visit read cannot be found")
)

// Refusal is why belay will not make a fork, in a sentence about the run in
// question. errors.Is matches it against the sentinel in Err.
type Refusal struct {
	// Err is the sentinel naming the kind of refusal.
	Err error
	// Reason is the sentence a person reads.
	Reason string
}

// Error implements error. It is the sentence alone: the sentinel's own text
// is a category, and the person reading has the specific case in front of
// them.
func (r *Refusal) Error() string { return r.Reason }

// Unwrap reports the sentinel.
func (r *Refusal) Unwrap() error { return r.Err }

// refuse builds a *Refusal.
func refuse(kind error, format string, a ...any) error {
	return &Refusal{Err: kind, Reason: fmt.Sprintf(format, a...)}
}

// unsupported are the nodes a fork cannot re-run, and why.
var unsupported = map[string]string{
	graph.NodeFanout:  "it runs whole candidate runs, which live in the source run's directory, and a fork never touches that",
	graph.NodeJoin:    "it chooses among candidate runs, which live in the source run's directory, and a fork never touches that",
	graph.NodePromote: "it commits the run's change to the run's branch, and a fork has no branch to commit to",
}

// planReaders are the nodes whose work depends on what the plan says: the
// plan node, which reads the plan a person asked it to replace; the gate,
// which shows it; and code, which builds it. Every node after code works from
// the code, and the plan is only a record for them.
var planReaders = map[string]bool{graph.NodePlan: true, graph.NodeApprove: true, graph.NodeCode: true}

// Request is what Prepare is asked to fork.
type Request struct {
	// Source is the run to fork. It is read and never written.
	Source state.Layout
	// Tree is the directory the source run's nodes worked in, which is what
	// the fork copies and what its fingerprint is compared against: the
	// workspace for a run in place, the run's worktree for one on a branch.
	// Working that out is the caller's, which knows where each kind of run
	// works.
	Tree string
	// Node is the node to re-run.
	Node string
	// Visit is which visit to Node, numbered as `belay timeline` numbers
	// them (journal.Visit.Ordinal). Zero means the last.
	Visit int
	// AcceptCurrentFiles re-runs the node on the files as they are now when
	// they may not be the ones the visit saw, rather than refusing.
	AcceptCurrentFiles bool
	// Config is the configuration the fork runs under.
	Config config.Config
	// Survey fingerprints a tree, as graph.Options.Fingerprint does for the
	// source's snapshots. It must be the same function, or no tree would ever
	// match.
	Survey func(ctx context.Context, dir string) (string, error)
	// Plan reads the plan the source run worked from, wherever it is now: in
	// the run's artifacts, or moved into the repository once the run
	// finished. A missing plan is an error wrapping fs.ErrNotExist. It is the
	// caller's because the caller knows where finished runs file their plans.
	Plan func() ([]byte, error)
}

// Spec is a fork Prepare has decided on and Create will make.
type Spec struct {
	// Source is the source run's manifest, as Prepare read it.
	Source state.Manifest
	// Visit is the visit the fork re-runs.
	Visit journal.Visit
	// Visits is how many visits the source made to the node, so a person can
	// be told "fix #2 of 3".
	Visits int
	// Snapshot is the blackboard as it stood before the visit.
	Snapshot state.Snapshot
	// Tree is the directory the fork copies.
	Tree string
	// Digest is Tree's fingerprint when Prepare surveyed it. Create surveys
	// again once the copy is made, and refuses a copy taken while the tree
	// was changing.
	Digest string
	// FilesChanged reports whether Tree may differ from the tree the visit
	// saw, and the caller accepted it as it is now.
	FilesChanged bool
	// Inherited is the history the fork's journal begins with: the source's
	// records before the visit, without their usage.
	Inherited []journal.Record
	// InheritedUSD is what that history cost, and InheritedEstimated whether
	// any of it was estimated. A fork of a fork adds its source's own
	// inheritance, so the figure is the whole cost of the blackboard the fork
	// starts from.
	InheritedUSD       float64
	InheritedEstimated bool
	// Deviations lists every way the fork's inputs differ from what the visit
	// was given. Empty, never nil, when there are none.
	Deviations []string

	request Request
	// plan is the plan to give the fork, or nil when the visit had none or it
	// cannot be found.
	plan []byte
}

// Prepare reads the source run and decides the fork, writing nothing.
//
// It refuses, with a *Refusal, every fork belay will not make (see the
// package documentation), and returns any other error when the source cannot
// be read at all.
func Prepare(ctx context.Context, req Request) (*Spec, error) {
	if why, ok := unsupported[req.Node]; ok {
		return nil, refuse(ErrUnsupportedNode, "belay cannot fork %s: %s", req.Node, why)
	}
	man, err := state.LoadManifest(req.Source.ManifestPath())
	if err != nil {
		return nil, fmt.Errorf("fork: read run %s: %w", req.Source.RunID(), err)
	}
	if man.Status == state.RunStatusRunning {
		return nil, refuse(ErrSourceActive,
			"run %s is still running, or it stopped part-way through a step and has not been resumed; "+
				"a fork copies its files, and a step may still be changing them", man.RunID)
	}
	read, err := journal.ReadFile(req.Source.JournalPath())
	if err != nil {
		return nil, fmt.Errorf("fork: read the journal of run %s: %w", man.RunID, err)
	}
	if read.Incomplete {
		return nil, refuse(ErrSourceActive,
			"the journal of run %s ends in a record that was cut off as it was written, so a step may "+
				"have been running when it stopped", man.RunID)
	}
	visits, err := journal.Visits(read.Records)
	if err != nil {
		return nil, fmt.Errorf("fork: run %s: %w", man.RunID, err)
	}
	visit, total, err := choose(man.RunID, visits, req.Node, req.Visit)
	if err != nil {
		return nil, err
	}
	snap, err := snapshotOf(req.Source, man, visit)
	if err != nil {
		return nil, err
	}

	inherited, usd, estimated := inherit(read.Records, visit.StartSeq)
	if man.Origin != nil {
		usd += man.Origin.InheritedUSD
		estimated = estimated || man.Origin.InheritedEstimated
	}
	if err := startsAt(inherited, visit); err != nil {
		return nil, fmt.Errorf("fork: run %s: %w", man.RunID, err)
	}

	spec := &Spec{
		Source: man, Visit: visit, Visits: total, Snapshot: snap, Tree: req.Tree,
		Inherited: inherited, InheritedUSD: usd, InheritedEstimated: estimated,
		Deviations: []string{},
		request:    req,
	}
	if err := spec.checkFiles(ctx); err != nil {
		return nil, err
	}
	if err := spec.checkPlan(); err != nil {
		return nil, err
	}
	spec.noteModel()
	spec.noteConversation()
	spec.noteShape()
	spec.noteSettings()
	return spec, nil
}

// label names the visit the way `belay timeline` does: "fix #2".
func (s *Spec) label() string { return fmt.Sprintf("%s #%d", s.Visit.Node, s.Visit.Ordinal) }

// note adds a deviation.
func (s *Spec) note(format string, a ...any) {
	s.Deviations = append(s.Deviations, fmt.Sprintf(format, a...))
}

// choose finds the visit asked for: the ordinal-th visit to node, or its last
// when ordinal is zero, and how many visits node had.
func choose(runID string, visits []journal.Visit, node string, ordinal int) (journal.Visit, int, error) {
	var mine []journal.Visit
	var ran []string
	for _, v := range visits {
		if !slices.Contains(ran, v.Node) {
			ran = append(ran, v.Node)
		}
		if v.Node == node {
			mine = append(mine, v)
		}
	}
	switch {
	case len(mine) == 0 && len(ran) == 0:
		return journal.Visit{}, 0, refuse(ErrNoSuchVisit, "run %s never ran any step, so there is nothing to fork", runID)
	case len(mine) == 0:
		return journal.Visit{}, 0, refuse(ErrNoSuchVisit, "run %s never ran %s; it ran %s", runID, node, strings.Join(ran, ", "))
	case ordinal == 0:
		return mine[len(mine)-1], len(mine), nil
	case ordinal < 0 || ordinal > len(mine):
		return journal.Visit{}, 0, refuse(ErrNoSuchVisit, "run %s ran %s %s; --visit takes %s",
			runID, node, times(len(mine)), visitRange(len(mine)))
	default:
		return mine[ordinal-1], len(mine), nil
	}
}

// times says how many times something happened.
func times(n int) string {
	if n == 1 {
		return "once"
	}
	return strconv.Itoa(n) + " times"
}

// visitRange says which numbers --visit takes for a node visited n times.
func visitRange(n int) string {
	if n == 1 {
		return "1"
	}
	return fmt.Sprintf("1 to %d", n)
}

// snapshotOf loads the snapshot taken before v's first execution, and says
// where the visit was really run when the source has none.
func snapshotOf(source state.Layout, man state.Manifest, v journal.Visit) (state.Snapshot, error) {
	path, err := source.SnapshotPath(v.StartSeq, v.Node)
	if err != nil {
		return state.Snapshot{}, err
	}
	snap, err := state.LoadSnapshot(path)
	switch {
	case err == nil:
		return snap, nil
	case !errors.Is(err, fs.ErrNotExist):
		return state.Snapshot{}, fmt.Errorf("fork: run %s: %w", man.RunID, err)
	case man.Origin != nil && v.StartSeq < man.Origin.StartSeq:
		// Visits before the fork's own run_started are the ones it
		// inherited, and they were run, and snapshotted, in its source. The
		// prefix is the source's records unchanged, so the visit has the
		// same number there.
		return state.Snapshot{}, refuse(ErrNoSnapshot,
			"%s #%d of run %s was not run by it: run %s inherited it from run %s, which holds its snapshot; "+
				"fork that run instead: belay --workspace %s fork %s %s --visit %d",
			v.Node, v.Ordinal, man.RunID, man.RunID, man.Origin.RunID,
			man.Origin.Workspace, man.Origin.RunID, v.Node, v.Ordinal)
	default:
		return state.Snapshot{}, refuse(ErrNoSnapshot,
			"run %s kept no snapshot of %s #%d, which every run recorded by a belay older than snapshots "+
				"lacks; its state.json holds the blackboard as the run left it, after that step, not "+
				"before it, so belay has nothing to re-run the step from", man.RunID, v.Node, v.Ordinal)
	}
}

// inherit returns the records before startSeq as a fork's journal holds them,
// with every node_finished stripped of its usage, and what that usage came
// to.
func inherit(records []journal.Record, startSeq uint64) (prefix []journal.Record, usd float64, estimated bool) {
	for _, r := range records {
		if r.Seq >= startSeq {
			break
		}
		if r.Usage != nil {
			usd += r.Usage.USD
			estimated = estimated || r.Usage.Estimated
			r.Usage = nil
		}
		prefix = append(prefix, r)
	}
	return prefix, usd, estimated
}

// startsAt checks that a journal made of prefix and a run_started boundary
// resumes at v's node, at attempt 1: what the fork's dispatcher will compute
// before it runs anything. Every journal belay writes passes; one that did
// not would have the fork run a node nobody asked for.
func startsAt(prefix []journal.Record, v journal.Visit) error {
	boundary := journal.Record{Seq: v.StartSeq, Event: journal.EventRunStarted}
	start, err := journal.ResolveStart(append(slices.Clone(prefix), boundary))
	if err != nil {
		return err
	}
	if start.State != journal.RunStateRunnable || start.Node != v.Node || start.Attempt != 1 {
		return fmt.Errorf("its journal before %s #%d leads to %s at attempt %d (%s), not to %s at attempt 1",
			v.Node, v.Ordinal, start.Node, start.Attempt, start.State, v.Node)
	}
	return nil
}

// checkFiles surveys the tree and compares it with the fingerprint taken
// before the visit, refusing unless the caller accepted the tree as it is.
func (s *Spec) checkFiles(ctx context.Context) error {
	digest, err := s.request.Survey(ctx, s.Tree)
	if err != nil {
		return fmt.Errorf("fork: survey %s: %w", s.Tree, err)
	}
	s.Digest = digest
	why := s.filesWhy(s.Snapshot.WorkspaceDigest, digest)
	if why == "" {
		return nil
	}
	if !s.request.AcceptCurrentFiles {
		return refuse(ErrFilesChanged, "%s; belay keeps no copy of them as they were", why)
	}
	s.FilesChanged = true
	s.note("%s; it is re-run on them as they are now", why)
	return nil
}

// filesWhy says why the tree may not be the one the visit saw, or "" when
// its fingerprint shows it is.
func (s *Spec) filesWhy(recorded, current string) string {
	switch {
	case recorded == "":
		return fmt.Sprintf("belay recorded no fingerprint of the files in %s when %s ran, so it cannot tell "+
			"whether they have changed since", s.Tree, s.label())
	case scheme(recorded) == "" || scheme(recorded) != scheme(current):
		return fmt.Sprintf("the files in %s were fingerprinted by a different belay when %s ran, so they "+
			"cannot be compared with how they are now", s.Tree, s.label())
	case recorded != current:
		return fmt.Sprintf("the files in %s have changed since %s ran", s.Tree, s.label())
	default:
		return ""
	}
}

// scheme returns the scheme a digest names, everything before its last
// colon: "tree1:sha256" of "tree1:sha256:<hex>". It is "" for a digest that
// names none.
func scheme(digest string) string {
	i := strings.LastIndex(digest, ":")
	if i < 0 {
		return ""
	}
	return digest[:i]
}

// checkPlan finds the plan the visit's blackboard names, and holds the nodes
// that read it to it (see planReaders).
func (s *Spec) checkPlan() error {
	recorded := s.Snapshot.State.Plan
	if recorded.Path == "" {
		return nil
	}
	var (
		plan []byte
		err  = fs.ErrNotExist
	)
	if s.request.Plan != nil {
		plan, err = s.request.Plan()
	}
	reads := planReaders[s.Visit.Node]
	switch {
	case err != nil && reads:
		return refuse(ErrNoPlan, "%s reads the plan, and the plan run %s worked from cannot be read: %v",
			s.label(), s.Source.RunID, err)
	case err != nil:
		s.note("the plan run %s worked from is not where it left it, so the fork has no copy of it; "+
			"nothing it re-runs reads it", s.Source.RunID)
		return nil
	}
	s.plan = plan
	if !reads || approve.Digest(plan) == recorded.Digest {
		return nil
	}
	if s.Visit.Node == graph.NodeCode {
		return refuse(ErrPlanChanged, "the plan has changed since %s ran, and code builds the plan it is given: "+
			"re-running it now would build a plan nobody approved in that form; fork approve instead, to have "+
			"the gate judge the plan as it is now", s.label())
	}
	s.note("the plan has changed since %s ran; it is given the plan as it is now", s.label())
	return nil
}

// noteModel says when the node is sent a model other than the one the
// source run was started with for it.
func (s *Spec) noteModel() {
	node := s.Visit.Node
	if !slices.Contains(config.RoutableNodes(), node) {
		return
	}
	now := s.request.Config.Agent.ModelFor(node)
	then := s.Source.ConfigSnapshot["agent.models."+node]
	if then == "" {
		then = s.Source.ConfigSnapshot["agent.model"]
	}
	switch {
	case then == "":
		s.note("%s is sent the model %q; the run did not record which model it was started with", node, now)
	case then != now:
		s.note("%s is sent the model %q; the run was started with %q for it", node, now, then)
	}
}

// noteConversation says that the agent's conversation stays behind, when the
// blackboard names one.
func (s *Spec) noteConversation() {
	if s.Snapshot.State.Code.SessionID == "" {
		return
	}
	s.note("the agent's conversation does not carry over: the fork starts a new one, because the run's went " +
		"on past this point and would show the step its own future")
}

// noteShape says how the fork's graph differs from its source's: a fork
// always works in place, on its own copy, as a single run.
func (s *Spec) noteShape() {
	if out := s.Source.Output; out.Branched() {
		s.note("the run worked on its branch %s; the fork edits its own copy in place and commits nothing", out.Branch)
	}
	if pin := s.Source.Fanout; pin != nil {
		s.note("the run fanned out into %d candidates; the fork is a single run", pin.Candidates)
	}
}

// noteSettings names every setting that differs between the configuration
// the source run was started with and the one the fork runs under, with both
// values.
//
// The model is left to noteModel, which says it in terms of the node. The
// settings a fork does not take from belay.yaml are left out altogether:
// policy, which it inherits pinned from its source, and output, plan archival
// and fanout, which it turns off. Neither is a difference in what the node is
// given.
func (s *Spec) noteSettings() {
	now, _ := state.RedactSnapshot(s.request.Config.Snapshot())
	then := s.Source.ConfigSnapshot
	keys := slices.Sorted(maps.Keys(now))
	for k := range then {
		if _, ok := now[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		if notAnInput(k) {
			continue
		}
		was, wasOK := then[k]
		is, isOK := now[k]
		if was == is && wasOK == isOK {
			continue
		}
		s.note("%s: was %s, now %s", k, value(was, wasOK), value(is, isOK))
	}
}

// notAnInput reports whether a configuration key is one a fork does not take
// from belay.yaml, or one noteModel reports instead.
func notAnInput(key string) bool {
	if key == "agent.model" {
		return true
	}
	for _, prefix := range []string{"agent.models.", "output.", "plan.archive", "fanout.", "policy."} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// value renders one setting's value for a deviation.
func value(v string, ok bool) string {
	if !ok {
		return "not recorded"
	}
	return strconv.Quote(v)
}
