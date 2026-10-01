// Package nodes assembles belay's node implementations into the graph the
// dispatcher walks.
//
// It exists so that exactly one place knows which nodes make up a run. The
// dispatcher resolves Result.Next through a graph.Registry by exact name, so
// a node that is implemented but never registered is not a compile error —
// it is a run that dies partway with an unknown-node error. Keeping the
// wiring in one function makes that gap visible here rather than at 3am.
//
// # One wiring, two views
//
// Default returns the graph as a registry, which is what the dispatcher needs:
// a set of nodes it can look up by name. Route returns the same graph as an
// order, which is what a person needs to see where a run is and what happens
// next. A graph.Registry cannot supply that order, because it is a set: Names
// sorts alphabetically. Both functions read one wiring value, so a node added
// to the route is registered by the same line that adds it, rather than by a
// second list someone must remember to keep in step.
//
// # A conditional node is decided once, when the run is created
//
// Some nodes belong only to some runs: the one that promotes the change onto
// a branch (WithBranchOutput), the gate that holds it to the policy in
// belay.yaml (WithPolicyGate). Each is added by an Option, and every Option
// follows one rule. Whether a run has the node is decided once, when the run
// is created, and pinned in its manifest. Every later call to Default or Route for that run, including the
// one a resume makes days later, is given options built from the manifest,
// never from the configuration on disk at the time.
//
// The reason is resume. belay loads belay.yaml again when it resumes a run,
// and nothing stops the file changing in between. If options came from it, a
// run started with no policy, resumed after someone adds one, would route into
// a gate that judges the change against a baseline taken when the run began.
// This run never took one, so the gate could only fail it for something it did
// not do. The reverse is as bad: remove the policy mid-run and the resumed run
// finishes without the check it was started under, and says nothing. A run is
// one attempt at one goal under one set of rules, and those rules must not
// depend on when it was last resumed.
//
// Three things follow:
//
//   - An Option carries its decision in. It is built from a pinned value, and
//     wiring has no Config field, so an Option has no configuration to read.
//   - A node that routes into a conditional node learns that successor from
//     the Option that adds it, not from rc.Config, so an edge and the node it
//     leads to are decided by one value. A node may still choose on live
//     config between successors every run registers: plan picks approve or
//     code from graph.approval, and both are always there.
//   - The manifest field holding a pin is a control field: an older belay that
//     dropped it would resume the run as a different graph. ADR-0011 says
//     how such a field names a capability in the manifest's requires list.
//
// # Fanout follows the rule now, too
//
// Fanout predated the rule. Default used to register fanout and join from the
// live fanout.enabled, which was harmless only because no node routed into
// fanout, and this paragraph said that whatever first did would have to turn
// the choice into a pinned Option. WithFanout is that Option. A run fans out
// because its manifest says so (state.Manifest.Fanout, set at creation), and
// fanout.enabled in belay.yaml routes nothing on its own: a run created
// without the pin is walked as it always was, whatever the file says when it
// is resumed.
//
// A fanout run is two graphs. Its own walk is plan, approve, fanout and join,
// then promote for a run that delivers on a branch; Default and Route build
// that one. Each candidate is a run of its own, walked over the work -- code,
// write, the policy gate, test, fix and review -- and Candidate and
// CandidateRoute build that one. The work is deliberately not registered in
// the parent: a parent that held a code node could be routed into it by a
// wiring bug and take one silent attempt in the person's own workspace,
// where a registry without one stops the run on an unknown node.
package nodes

import (
	"fmt"
	"slices"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/nodes/code"
	"github.com/dhaam-ai/belay/internal/nodes/fanout"
	"github.com/dhaam-ai/belay/internal/nodes/fix"
	"github.com/dhaam-ai/belay/internal/nodes/join"
	"github.com/dhaam-ai/belay/internal/nodes/plan"
	"github.com/dhaam-ai/belay/internal/nodes/policy"
	"github.com/dhaam-ai/belay/internal/nodes/promote"
	"github.com/dhaam-ai/belay/internal/nodes/review"
	"github.com/dhaam-ai/belay/internal/nodes/test"
	"github.com/dhaam-ai/belay/internal/nodes/write"
)

// wiring is the one description of a run's graph that Default and Route both
// read.
type wiring struct {
	// route is the walk a run takes, in order, from the entry point. Every
	// node on it is registered, by construction.
	route []graph.Node
	// offRoute are registered but on no route this run walks: the approve
	// gate of a run with graph.approval off. A run cannot reach it, and it
	// must still be resolvable by name (see Default).
	offRoute []graph.Node

	// fanout and branch are what WithFanout and WithBranchOutput decided,
	// recorded for fanOut, which acts on them once every option has run.
	// See fanOut for why it waits.
	fanout bool
	branch bool
}

// workNodes are the nodes that do a run's work, in the order a run walks
// them: the nodes a fanout run hands to its candidates, and the only nodes a
// candidate's own graph may hold.
var workNodes = []string{
	graph.NodeCode, graph.NodeWrite, graph.NodePolicy, graph.NodeTest, graph.NodeFix, graph.NodeReview,
}

// replace swaps the registered node of the same name for n, wherever it sits.
// An Option that wires an edge has to replace the node that carries it,
// because a node's successors are fixed when it is built: that is what keeps
// an edge and the node it leads to one decision (see Option).
//
// A node that is not there is not an error. The graph a run gets depends on
// its pins -- the policy gate is absent from an unguarded run -- and an
// Option describes the edges of whatever nodes the run has.
func (w *wiring) replace(n graph.Node) {
	for _, list := range [][]graph.Node{w.route, w.offRoute} {
		if i := slices.IndexFunc(list, func(m graph.Node) bool { return m.Name() == n.Name() }); i >= 0 {
			list[i] = n
		}
	}
}

// after puts n on the route directly after the node named before, or at the
// end when the route has no such node.
func (w *wiring) after(before string, n graph.Node) {
	i := slices.IndexFunc(w.route, func(m graph.Node) bool { return m.Name() == before })
	if i < 0 {
		w.route = append(w.route, n)
		return
	}
	w.route = slices.Insert(w.route, i+1, n)
}

// Option changes the graph Default and Route build: it adds a conditional
// node, or changes a node's successor to lead into one.
//
// It receives the wiring and nothing else. The value it acts on is captured
// when the Option is constructed, from what the run's manifest pinned, which
// is the rule in the package documentation. WithBranchOutput is the first
// one. Each slice that adds a conditional node adds another, with a row in
// this package's route tests.
type Option func(*wiring)

// WithBranchOutput builds the graph of a run that delivers its result on a
// git branch: the promote node is registered and put on the route after the
// quality gate, and the two nodes that would otherwise end the run are wired
// into it — a passing gate, and a selected fanout winner. The second edge is
// recorded rather than wired here, because join exists only in a run that
// fans out, and fanOut builds it once it knows both pins.
//
// It is opt-in, and it is the whole difference between the two output modes
// as far as the graph is concerned. A run without it is exactly the run
// belay has always had, editing its workspace in place and ending when the
// gate passes.
//
// Whether a run gets it is decided once, when the run is created, and pinned
// in the manifest as state.Output; every later call for that run, a resume
// included, is given the same option. See this package's documentation for
// why an edge may not be read from configuration that can change mid-run.
func WithBranchOutput() Option {
	return func(w *wiring) {
		w.branch = true
		w.replace(review.New(review.WithOnPass(graph.NodePromote)))
		w.after(graph.NodeReview, promote.New())
	}
}

// WithPolicyGate builds the graph of a run whose change is held to the
// guardrails in belay.yaml: the policy gate is registered and put on the
// route between write and test, and write is rewired to lead into it.
//
// Both halves are this one Option, because a gate nothing routes into is the
// failure the whole feature exists to prevent -- a guardrail configured,
// accepted, and walked past on every run. A registry holding the node while
// write still routed to test would be exactly that, and it would look
// correct in every listing.
//
// It is opt-in, and a run without it is exactly the run belay has always
// had. Whether a run gets it is decided once, when the run is created, and
// pinned in the manifest as state.Manifest.Policy; every later call for that
// run, a resume included, is given the same option. See this package's
// documentation for why an edge may not be read from configuration that can
// change mid-run -- the gate is the case that documentation was written
// about, because a policy that arrived mid-run could only fail a run for
// something it did not do.
//
// The gate needs no rewiring of its own successors. It sends a clean change
// to the test node and a repairable violation to the fix node, and both are
// on every route; the fix node routes back through write, so the gate is
// re-entered on every pass of the repair loop rather than only the first.
func WithPolicyGate() Option {
	return func(w *wiring) {
		w.replace(write.New(write.WithOnVerified(graph.NodePolicy)))
		w.after(graph.NodeWrite, policy.New())
	}
}

// WithFanout builds the graph of a run that fans out: its walk is plan,
// approve, fanout and join -- then promote, for a run that delivers on a
// branch -- and the plan and the gate open onto the fanout rather than onto
// code.
//
// The work itself leaves the run's own graph altogether. Each candidate is a
// run of its own, driven over the graph Candidate builds, and the parent
// never codes, so none of code, write, the policy gate, test, fix or review
// is registered here. That is a choice, argued in this package's
// documentation: a registry without them stops a run routed there by a bug,
// where one holding them would quietly code once in the person's workspace.
//
// It takes no argument. What it decides is only that the run fans out; how
// many candidates and which strategy picks the winner reach the fanout and
// join nodes through rc.Config, which state.Manifest.PinnedConfig has
// already overlaid with the run's pin, so the two cannot disagree.
//
// Whether a run gets it is decided once, when the run is created, and pinned
// in the manifest as state.Manifest.Fanout; every later call for that run, a
// resume included, is given the same option, and fanout.enabled in the file
// changes nothing about a run already started.
func WithFanout() Option {
	return func(w *wiring) { w.fanout = true }
}

// fanOut turns a run's graph into a fanout run's: the work leaves it, fanout
// and join join it after the gate, and the plan and the gate open onto the
// fanout. wire calls it once every option has run.
//
// It waits for the other options because two of them were written before a
// run could fan out, and both anchor on nodes this removes. WithPolicyGate
// puts its gate after write, and WithBranchOutput puts promote after review
// and wants join to promote the winner. Acting on WithFanout where it fell in
// the list would make a run's graph depend on which pin was read first: a
// policy gate added after the work had gone would land at the end of the
// parent's walk, and a join made before the branch pin was seen would end
// the run instead of promoting its winner. Waiting makes the order of the
// options irrelevant, which is what the rest of this package assumes of
// them.
func (w *wiring) fanOut() {
	selected := join.New()
	if w.branch {
		selected = join.New(join.WithOnSelected(graph.NodePromote))
	}
	isWork := func(n graph.Node) bool { return slices.Contains(workNodes, n.Name()) }
	w.route = slices.DeleteFunc(w.route, isWork)
	w.offRoute = slices.DeleteFunc(w.offRoute, isWork)

	// After the gate when the run stops at it, and straight after the plan
	// when graph.approval has taken the gate off the walk.
	opening := graph.NodePlan
	if slices.ContainsFunc(w.route, func(n graph.Node) bool { return n.Name() == graph.NodeApprove }) {
		opening = graph.NodeApprove
	}
	w.after(opening, fanout.New())
	w.after(graph.NodeFanout, selected)
	w.replace(plan.New(plan.WithWorkEntry(graph.NodeFanout)))
	w.replace(approve.New(approve.WithWorkEntry(graph.NodeFanout)))
}

// Candidate returns the registry one fanout candidate's own run is driven
// over: code, write, test, fix and review, with the policy gate between
// write and test when it is given WithPolicyGate.
//
// It is the parent's work, and nothing else a run has. No plan and no gate:
// the parent's plan was approved before any candidate existed, and a
// candidate starts at code (graph.Options.Entry). No fanout of its own. No
// promote: a candidate's passing gate ends the candidate's run, and promoting
// the winner is the parent's, after join has chosen one.
//
// An option that would add any of those is refused with an error rather than
// wired, because the failure it would cause is expensive and late. A
// candidate whose review routed to a promote node it does not have would end
// on an unknown node after paying for all of its work, N times over.
// WithPolicyGate is the one option a candidate takes, and it must take it
// whenever the run it belongs to has a policy: the candidates are where a
// fanout run's change is made, so they are where it must be judged.
func Candidate(opts ...Option) (*graph.Registry, error) {
	w, err := candidate(opts)
	if err != nil {
		return nil, err
	}
	r := graph.NewRegistry()
	for _, n := range w.route {
		if err := r.Register(n); err != nil {
			return nil, fmt.Errorf("nodes: wire the candidate graph: %w", err)
		}
	}
	return r, nil
}

// CandidateRoute returns the names of the nodes one candidate's run walks, in
// order, from code -- the candidate's half of what a dry run shows a person,
// as Route is the parent's half. It is nil for options Candidate refuses,
// since there is no graph to show.
//
// Give it the options Candidate was given.
func CandidateRoute(opts ...Option) []string {
	w, err := candidate(opts)
	if err != nil {
		return nil
	}
	names := make([]string, len(w.route))
	for i, n := range w.route {
		names[i] = n.Name()
	}
	return names
}

// candidate builds the graph Candidate and CandidateRoute both read, and
// refuses any option that leaves something in it a candidate cannot run.
func candidate(opts []Option) (*wiring, error) {
	w := &wiring{route: []graph.Node{code.New(), write.New(), test.New(), fix.New(), review.New()}}
	for _, opt := range opts {
		opt(w)
	}
	if w.fanout {
		return nil, fmt.Errorf("nodes: a candidate run cannot fan out: candidates are the fanout")
	}
	for _, n := range slices.Concat(w.route, w.offRoute) {
		if !slices.Contains(workNodes, n.Name()) {
			return nil, fmt.Errorf("nodes: a candidate run cannot hold the %s node: "+
				"it does the run's work, and %s belongs to the run that fanned out", n.Name(), n.Name())
		}
	}
	return w, nil
}

// wire builds the graph for cfg, then applies opts in the order given, each
// to the graph the ones before it built, and then -- for a run given
// WithFanout -- turns it into a fanout run's graph (see fanOut).
//
// The write node is deliberately NOT pinned to a workspace here.
// write.WithWorkspaceRoot takes precedence over RunContext.Workspace, so a
// registry built once per run and pinned to the original checkout would send
// every fanout candidate's write node at the wrong directory — the candidate
// copies are exactly the case the pin would break. Letting it follow the
// context means each execution is bounded by the tree it is actually working
// on.
func wire(cfg config.Config, opts []Option) *wiring {
	w := &wiring{route: []graph.Node{
		plan.New(),
		approve.New(),
		code.New(),
		write.New(),
		test.New(),
		fix.New(),
		review.New(),
	}}
	// graph.approval false means the plan node routes straight to the work, so
	// the gate is on no walk this run takes. It is still registered, and must
	// be: a run started with the gate can be resumed by a process that reads
	// a belay.yaml where the flag has since been turned off, and it re-runs
	// the node the journal names. Routing to a node nobody registered ends
	// that run with nothing to show for what it spent. So the gate leaves the
	// route — which is what a person is shown — and stays in the registry,
	// which is what the dispatcher resolves names through.
	if !cfg.Graph.Approval {
		w.route = slices.DeleteFunc(w.route, func(n graph.Node) bool {
			return n.Name() == graph.NodeApprove
		})
		w.offRoute = append(w.offRoute, approve.New())
	}
	// fanout.enabled is deliberately not read here, although it used to be.
	// Whether a run fans out is its manifest's pin, which reaches this as
	// WithFanout; see the package documentation.
	for _, opt := range opts {
		opt(w)
	}
	if w.fanout {
		w.fanOut()
	}
	return w
}

// Default returns the registry for a standard belay run:
//
//	plan → approve → code → write → test → fix → review → END
//
// with test failures and failed quality gates both routing back through fix,
// bounded by graph.give_up, and fix routing back through write so that a
// repair's paths are verified like any other change. WithBranchOutput adds
// promote after review, for a run that commits its change to a branch;
// WithPolicyGate adds the policy gate between write and test, for a run whose
// change is held to guardrails. WithFanout makes it a fanout run's graph,
//
//	plan → approve → fanout → join → END
//
// with the work handed to candidate runs (see Candidate, and ADR-0019), and
// promote after join for a run that also delivers on a branch. Each Option
// adds to or changes that graph.
//
// The approve gate is registered whatever graph.approval says, although a run
// with it off never reaches the gate (see Route). The flag is read from
// belay.yaml on every invocation, so a run started with the gate can be
// resumed after someone turns it off, and the journal still names approve as
// the node to re-run. A registry without it would end that run on an unknown
// node.
func Default(cfg config.Config, opts ...Option) (*graph.Registry, error) {
	w := wire(cfg, opts)
	r := graph.NewRegistry()
	for _, n := range slices.Concat(w.route, w.offRoute) {
		if err := r.Register(n); err != nil {
			return nil, fmt.Errorf("nodes: wire the default graph: %w", err)
		}
	}
	return r, nil
}

// Route returns the names of the nodes a run of the graph Default builds for
// cfg walks, in the order it walks them, starting at the entry point. It does
// not end with graph.End: that is where a run stops, not a node it runs.
//
// It is the order a person reads, not a trace. The graph has two cycles,
// test → fix → test and review → fix → test, and Route repeats neither: it
// lists fix once, after test, and a run whose tests and quality gate pass
// never runs it. A node registered but not reachable is not on it: the
// approve gate when graph.approval is false, since the plan node then routes
// straight to where the work begins. Nor is a candidate's work, for a run
// that fans out: that is CandidateRoute's, because the parent never walks it.
// A dry run prints this, so it has to be the walk the run really takes rather
// than the walk the graph could take.
//
// Give it the options Default was given. With any others it describes a
// graph the dispatcher is not walking.
func Route(cfg config.Config, opts ...Option) []string {
	w := wire(cfg, opts)
	names := make([]string, len(w.route))
	for i, n := range w.route {
		names[i] = n.Name()
	}
	return names
}
