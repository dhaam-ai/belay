// Package promote implements belay's promote node: the step that turns a run's
// gated change into a commit on the run's own branch, and, when fanout chose a
// winner, merges that winner in with a merge commit.
//
// It exists only in a run whose output is a branch. internal/nodes adds it
// with WithBranchOutput, which also routes a passing quality gate and a
// selected winner here instead of ending the run. A run that edits its
// workspace in place never has this node, so nothing about in-place runs
// changes.
//
// # The repository is the memory
//
// The dispatcher re-runs a node whose start has no finish, which is how a
// crash inside promote presents on resume. Promote's effects are real: a
// commit, a merge, workspaces destroyed. A node that remembered what it had
// done, in state or in a file or in a variable, would either do it again or
// have to trust a record an attempt wrote before it died. So promote
// remembers nothing and asks the repository instead:
//
//   - Committing a worktree with nothing to commit makes no commit and returns
//     the commit already there (vcs.Repository.Commit). A re-run after the
//     commit landed finds a clean tree and the same commit.
//   - Merging a branch the target already contains makes no commit
//     (vcs.Repository.Integrate checks ancestry). A re-run after the merge
//     landed merges nothing.
//   - Whether the winner's work is already on the run's branch is a question
//     about that branch's history; see "A winner that is already gone".
//
// The Result is then built from the end state, never from what the attempt
// did: the branch's tip, how many files differ from the run's base, the
// winner's ID. A Note saying "committed 3 files" on the attempt that
// committed and "nothing to commit" on its re-run would put two accounts of
// one promotion into the journal. Because every field is a function of the
// repository, an interrupted promote and its re-run return byte-identical
// Results, and the node owns no bookkeeping at all.
//
// # Two paths
//
// A run without fanout promotes its own work. The run's worktree holds
// exactly the tree its tests and quality gate passed, and promote commits all
// of it as one commit whose message is the goal, with a Belay-Run trailer
// naming the run. It commits from the worktree's top, not from the
// subdirectory a run may have started in: the change is whatever the run's
// agent changed, and that can include a go.mod above it. Nothing commits
// before this node does, because golangci-lint scopes its findings with
// --new-from-rev=HEAD, and an earlier commit would hide the run's own changes
// from the gate that was meant to judge them.
//
// A run whose join selected a winner promotes that winner. Its uncommitted
// work is committed on its own branch, the branch is merged into the run's
// branch as one merge commit, and only then are the candidates destroyed.
// Both commits carry Belay-Run and Belay-Candidate trailers. The order is the
// point: destroying first would throw away the winner's uncommitted work,
// which is all of what it did.
//
// # The approved plan
//
// A branch run's plan is part of what it delivers, so the one commit carries
// it (ADR-0016). Just before it commits, promote writes the plan the run's
// code was written from (internal/planarchive's Approved) into the tree it
// is about to commit, at the path the run's manifest pinned when the run was
// created (graph.RunContext.ArchiveDir). The CLI moves it out of the run
// directory after the run completes: it records where the plan went and
// removes the run's copy, which a node has no business doing.
//
// It is promote, and not a step before promote, because promote is the only
// place that knows whether the commit has happened. A plan written by an
// earlier node would sit uncommitted in the worktree while promote's re-run
// question — is this work already committed? — was being asked about a tree
// that had changed under it; a node of its own would add a step to every
// branch run's route and journal for one file. Here the rule is the node's
// usual one, asked of the repository: the plan is written while no commit of
// this promotion is in the branch's history, and never after. Before the
// commit a re-run writes the same bytes to the same path, which Place
// adopts; after it, the re-run leaves the tree alone. Either way the tree,
// the commit and the Result are the ones an uninterrupted promote produces,
// even when the plan could not be read on the first attempt and can on the
// second. A winner's promotion writes the plan into the winner's tree before
// committing the winner's work there, so the merge brings plan and code in
// together; the run's own worktree takes no uncommitted file, which the
// merge would refuse.
//
// The plan is not what the tests and the quality gate judged — it is the
// record of what their code was written from — and it cannot fail either.
// So nothing about it can change the Result: a plan that cannot be written
// is logged, the change is committed without it, and the CLI tells the
// person the plan stayed in the run directory.
//
// # A merge conflict is a pause
//
// When the winner does not merge cleanly, a person has to decide what to
// keep. Promote returns journal.StatusPaused and touches nothing: no Patch, no
// workspace destroyed, the run's branch where it was. The Note names the
// conflicting files, the candidate's branch, the run's worktree and the
// commands to finish by hand. The dispatcher records the pause exactly as it
// records the approve gate's, with the same exit code, and `belay resume`
// runs promote again, which then finds the person's merge on the branch and
// finishes. A failure would have been wrong twice over: a failed run cannot be
// resumed, and it would strand a winner that was already selected and paid
// for.
//
// # A winner that is already gone
//
// Promote destroys candidates, and a crash can land after that and before the
// dispatcher records the node as finished. The re-run then finds the winner's
// worktree gone, and perhaps its branch too. It cannot merge what is not there,
// and it must not guess.
//
// So before it touches the winner, promote asks the run's branch one
// question: does its history hold a commit carrying both this run's Belay-Run
// trailer and the winner's Belay-Candidate trailer? Promote's own commits
// carry both, so once the winner's work is on the branch, whether by
// promote's merge or by a person merging the branch promote committed it on,
// the answer is yes and promote goes straight to reporting. It asks for both
// trailers because candidate IDs repeat from run to run, and an earlier run's
// merged branch may be part of the history this run started from; its
// Belay-Candidate trailer alone would read as "already done" and skip a merge
// this run still owes.
//
// That answer is only worth having if it is yes whenever the winner might be
// gone, so promote destroys the winner only once the branch carries that
// record. A winner whose work arrived some other way, such as a person
// merging commits the candidate made itself, is promoted but kept, and the
// Note says where it is. The losing candidates are needed by nothing and go
// either way. A winner that is gone with no record is ErrWinnerMissing:
// something other than promote removed it, and whether its work reached the
// branch is not something promote can know.
//
// The question is not part of vcs.Repository. Promote looks for it on the
// run's repository as an optional interface, the io.WriterTo pattern, and
// looks for Branch on the run's isolator the same way; see history and
// brancher. A run whose adapters lack either can still promote its own work,
// and refuses to merge a winner with ErrUnpromotable. An isolator promote
// merges from must also tolerate Destroy on a workspace it already destroyed,
// since a re-run cleans up again.
//
// # Errors versus results
//
// Per ADR-0002, an error means promote reached no verdict. A graph wired
// wrongly (promote in an in-place run, no repository, a winner that is not a
// candidate) and a repository that cannot answer are errors, and none of them
// leaves a Patch or destroys anything. A conflict is a verdict. So is a
// candidate workspace that could not be destroyed once the winner was merged:
// the run has succeeded, and the litter is named in the Note and logged at
// Error rather than turned into the failure of a run whose result is already
// committed.
//
// # What it does not do
//
// It does not push, open a pull request, rebase, or touch any branch but the
// run's own; publishing happens after the run completes, outside the graph.
// It does not choose who authors a commit, which is the user's own identity
// and the repository implementation's business, and it does not sign. It
// never destroys a candidate the isolator did not mark ephemeral, which may be
// the user's own tree. And it never runs in an in-place run: nothing puts it
// there, and it refuses if something does.
//
// ADR-0012 will record the decision to deliver a run's result on a branch.
// It is not written yet: the slice that makes branch output selectable, and
// so makes this node reachable, writes it.
package promote

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/planarchive"
	rules "github.com/dhaam-ai/belay/internal/policy"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/internal/vcs"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// The trailers promote writes into the commits it makes.
const (
	// trailerRun names the run a commit belongs to.
	trailerRun = "Belay-Run"
	// trailerCandidate names the fanout candidate whose work a commit
	// carries.
	trailerCandidate = "Belay-Candidate"
)

// subjectWidth is the longest commit subject promote writes: the width git's
// own tools, and most hosting sites, show on one line.
const subjectWidth = 72

// noteFiles is how many conflicting files a pause Note names before it counts
// the rest instead.
const noteFiles = 5

// shortCommit is how much of a commit ID a Note shows: enough to type into
// git, short enough to read.
const shortCommit = 12

// Errors returned by this package. Each means promote reached no verdict.
var (
	// ErrNotBranchOutput reports promote in a run whose output is not a
	// branch. Only WithBranchOutput registers promote, so this is a wiring
	// bug.
	ErrNotBranchOutput = errors.New("promote: the run's output is not a branch")

	// ErrNoVCS reports a branch run whose context carries no repository to
	// commit to.
	ErrNoVCS = errors.New("promote: the run context has no repository")

	// ErrIncompleteOutput reports a branch run whose pinned output lacks
	// something promote needs: the branch, its base, or an absolute
	// worktree path.
	ErrIncompleteOutput = errors.New("promote: the run's branch output is incomplete")

	// ErrNoWinner reports candidates with no winner. join ends such a run
	// itself, so reaching promote with one is a wiring bug.
	ErrNoWinner = errors.New("promote: the run has candidates but no winner")

	// ErrUnknownWinner reports a winner that is not one of the run's
	// candidates.
	ErrUnknownWinner = errors.New("promote: the winner is not one of the run's candidates")

	// ErrUnpromotable reports a winner the run's adapters cannot merge: an
	// isolator that does not keep candidates on branches, a repository that
	// cannot say what its branch already holds, or a winner whose recorded
	// workspace is unusable.
	ErrUnpromotable = errors.New("promote: the winner cannot be merged")

	// ErrWinnerMissing reports a winner whose workspace no longer exists
	// while nothing on the run's branch records that its work was promoted.
	// See "A winner that is already gone" in the package documentation.
	ErrWinnerMissing = errors.New("promote: the winner's workspace is gone, and the run's branch does not record promoting it")
)

// brancher is implemented by an isolator whose candidates are git branches,
// which is what the worktree isolator does. It is an optional interface
// rather than a method on belay.Isolator because belay.Isolator carries a
// compatibility promise and a directory copy has no branch to name.
type brancher interface {
	// Branch returns the name of the branch the workspace created with id
	// is checked out on.
	Branch(id string) (string, error)
}

// history is what promote asks a repository before it merges a winner, on
// top of vcs.Repository. It is optional for the same reason brancher is:
// vcs.Repository is what every repository must do, and only merging a winner
// needs more.
type history interface {
	// Tip returns the commit the branch checked out at dir points to.
	Tip(ctx context.Context, dir string) (string, error)
	// Carries reports whether the history of the branch checked out at dir
	// holds one commit whose message carries every trailer given.
	Carries(ctx context.Context, dir string, trailers ...vcs.Trailer) (bool, error)
}

// Node is the promote graph node. It holds no state: everything it needs
// arrives on the graph.RunContext, so one value is safe to register once and
// reuse for the life of a process.
type Node struct{}

// New returns the promote node.
func New() *Node { return &Node{} }

var (
	_ graph.Node     = (*Node)(nil)
	_ graph.Unpriced = (*Node)(nil)
)

// Name returns graph.NodePromote.
func (n *Node) Name() string { return graph.NodePromote }

// SpendsNothing exempts this node from the dispatcher's budget ceiling
// check.
//
// It is not an exemption from the ceiling but the observation that the
// ceiling has nothing to say about a node that cannot spend: promote runs
// git, calls no agent, and its Usage is zero by construction. It is also the
// last node between a winner the run has already bought and its delivery, so
// stopping here saves nothing and leaves the work selected, paid for and
// out of reach until someone edits belay.yaml — the case a fanout whose
// candidates spent the ceiling reaches every time. See graph.Unpriced.
func (n *Node) SpendsNothing() {}

// Run promotes the run's change onto its branch and ends the run.
//
// A promotion routes to graph.End with journal.StatusOK and a Patch setting
// state.Promotion. A winner that conflicts with the run's branch pauses the
// run with no Patch. A wiring bug, or a repository that cannot answer, returns
// an error and no Patch. Usage stays zero: promote calls no agent.
func (n *Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc == nil {
		return failed(), errors.New("promote: nil run context")
	}
	if err := ctx.Err(); err != nil {
		return graph.Result{Status: journal.StatusAborted}, fmt.Errorf("promote: %w", err)
	}
	if err := checkOutput(rc); err != nil {
		return failed(), err
	}
	if len(rc.State.Candidates) == 0 && rc.State.Winner == "" {
		return promoteRun(ctx, rc)
	}
	return promoteWinner(ctx, rc)
}

// checkOutput refuses a context promote cannot act on: a run that is not
// on a branch, has no repository, or whose pinned branch is missing a part.
func checkOutput(rc *graph.RunContext) error {
	out := rc.Output
	if !out.Branched() {
		mode := out.Mode
		if mode == "" {
			mode = state.OutputInPlace
		}
		return fmt.Errorf("%w: its output mode is %q, and only a run on a branch has a promote node",
			ErrNotBranchOutput, mode)
	}
	if rc.VCS == nil {
		return fmt.Errorf("%w: a run on a branch needs one to commit to", ErrNoVCS)
	}
	var missing []string
	if out.Branch == "" {
		missing = append(missing, "a branch")
	}
	if out.Base == "" {
		missing = append(missing, "a base commit")
	}
	if out.WorkTree == "" {
		missing = append(missing, "a worktree")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: the manifest records no %s", ErrIncompleteOutput, strings.Join(missing, ", no "))
	}
	if !filepath.IsAbs(out.WorkTree) {
		// git would resolve it against whatever directory belay runs in.
		return fmt.Errorf("%w: worktree %q is not an absolute path", ErrIncompleteOutput, out.WorkTree)
	}
	return nil
}

// promoteRun commits the run's own gated change.
func promoteRun(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if res, stop, err := accountFor(ctx, rc, rc.Output.WorkTree, rc.Workspace, rc.State.Code.ChangedFiles); err != nil || stop {
		return res, err
	}
	archive(ctx, rc, rc.Workspace, rc.Output.WorkTree, trailers(rc.RunID, ""))
	tip, err := rc.VCS.Commit(ctx, rc.Output.WorkTree, workMessage(rc, "", rc.State.Code.Unreported))
	if err != nil {
		if res, ok := unfinished(rc, err); ok {
			return res, nil
		}
		return failed(), fmt.Errorf("promote: commit the run's change in %s: %w", rc.Output.WorkTree, err)
	}
	promotion, err := record(ctx, rc, tip, "")
	if err != nil {
		return failed(), err
	}
	logger(rc).Info("promote: the run's change is on its branch",
		slog.String("branch", promotion.Branch), slog.String("commit", promotion.Commit),
		slog.Int("files", promotion.Files))
	return promoted(promotion, ""), nil
}

// promoteWinner merges the winning candidate into the run's branch and then
// destroys the candidates it is done with. See the package documentation for
// why every step asks the repository first.
func promoteWinner(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	winner, err := findWinner(rc.State)
	if err != nil {
		return failed(), err
	}
	branch, repo, err := mergeable(rc, winner)
	if err != nil {
		return failed(), err
	}

	mark := trailers(rc.RunID, winner.ID)
	recorded, err := repo.Carries(ctx, rc.Output.WorkTree, mark...)
	if err != nil {
		return failed(), fmt.Errorf("promote: ask whether %s already holds candidate %s: %w",
			rc.Output.Branch, winner.ID, err)
	}
	if !recorded {
		if err := present(rc, winner); err != nil {
			return failed(), err
		}
		if res, stop, err := accountFor(ctx, rc, worktreeTop(winner.Dir, rc.Output.Prefix), winner.Dir, winner.ChangedFiles); err != nil || stop {
			return res, err
		}
		archive(ctx, rc, winner.Dir, winner.Dir, mark)
		if _, err := rc.VCS.Commit(ctx, winner.Dir, workMessage(rc, winner.ID, winner.Unreported)); err != nil {
			if res, ok := unfinished(rc, err); ok {
				return res, nil
			}
			return failed(), fmt.Errorf("promote: commit candidate %s's work in %s: %w", winner.ID, winner.Dir, err)
		}
		merge, err := rc.VCS.Integrate(ctx, rc.Output.WorkTree, branch, mergeMessage(rc, winner.ID))
		if err != nil {
			if res, ok := unfinished(rc, err); ok {
				return res, nil
			}
			return failed(), fmt.Errorf("promote: merge %s into %s: %w", branch, rc.Output.Branch, err)
		}
		if len(merge.Conflicts) > 0 {
			return conflicted(rc, winner.ID, branch, merge.Conflicts), nil
		}
		// Asked again rather than inferred from the merge: a winner the
		// branch already contained made no merge commit, and only the
		// branch's history can say whether it now records this winner.
		if recorded, err = repo.Carries(ctx, rc.Output.WorkTree, mark...); err != nil {
			return failed(), fmt.Errorf("promote: ask whether %s now holds candidate %s: %w",
				rc.Output.Branch, winner.ID, err)
		}
	}

	tip, err := repo.Tip(ctx, rc.Output.WorkTree)
	if err != nil {
		return failed(), fmt.Errorf("promote: read the tip of %s: %w", rc.Output.Branch, err)
	}
	promotion, err := record(ctx, rc, tip, winner.ID)
	if err != nil {
		return failed(), err
	}
	// Only now, with the promotion confirmed by the repository, is anything
	// destroyed.
	cleanup := clean(ctx, rc, winner.ID, recorded)
	logger(rc).Info("promote: the winner is on the run's branch",
		slog.String("branch", promotion.Branch), slog.String("candidate", winner.ID),
		slog.String("commit", promotion.Commit), slog.Int("files", promotion.Files))
	return promoted(promotion, cleanup), nil
}

// archive writes the run's plan into root, the tree promote is about to
// commit, at the path the run's manifest pinned (rc.ArchiveDir), so the
// commit carries it. dir is the working tree that commit is made in, and
// mark the trailers that commit carries. See "The approved plan" in the
// package documentation.
//
// It writes nothing once the branch checked out at dir holds a commit
// carrying mark: that commit is this promotion's, the tree is committed, and
// a file written into it now would make a second commit on a re-run. A
// repository that cannot answer the question, one without Carries, gets the
// plan written anyway, and Place's convergence is what keeps a re-run from
// changing the tree; one that fails to answer gets nothing, because not
// writing can only leave the plan in the run directory.
//
// Nothing here can change the Result: every reason the plan stays out of
// the commit is logged and the promotion goes ahead, because a run whose
// change passed its quality gate has succeeded whether or not its plan
// could be filed, and the CLI tells the person where the plan is.
func archive(ctx context.Context, rc *graph.RunContext, root, dir string, mark []vcs.Trailer) {
	if rc.ArchiveDir == "" {
		return
	}
	rel := planarchive.Rel(rc.ArchiveDir, rc.RunID, rc.Goal)
	log := logger(rc).With(slog.String("plan", rel))
	if repo, ok := rc.VCS.(history); ok {
		carried, err := repo.Carries(ctx, dir, mark...)
		if err != nil {
			log.Warn("promote: cannot tell whether this promotion already committed, so the plan stays out of its commit",
				slog.Any("error", err))
			return
		}
		if carried {
			return
		}
	}
	plan, err := planarchive.Approved(rc.Layout, rc.State.Plan)
	if err == nil {
		_, err = planarchive.Place(root, rel, plan)
	}
	if err != nil {
		log.Warn("promote: the run's plan cannot go into its commit; the change is committed without it",
			slog.Any("error", err))
		return
	}
	log.Info("promote: the approved plan goes into the commit")
}

// findWinner returns the candidate join selected.
func findWinner(st state.State) (state.Candidate, error) {
	if st.Winner == "" {
		return state.Candidate{}, fmt.Errorf("%w: %d candidates were recorded and none was selected; join ends such a run instead of routing here",
			ErrNoWinner, len(st.Candidates))
	}
	i := slices.IndexFunc(st.Candidates, func(c state.Candidate) bool { return c.ID == st.Winner })
	if i < 0 {
		return state.Candidate{}, fmt.Errorf("%w: %q is not among the %d recorded candidates",
			ErrUnknownWinner, st.Winner, len(st.Candidates))
	}
	return st.Candidates[i], nil
}

// mergeable returns the winner's branch and the questions promote asks the
// repository, or ErrUnpromotable when the run's adapters cannot merge a
// winner at all. It changes nothing, so a refusal leaves everything as it was.
func mergeable(rc *graph.RunContext, winner state.Candidate) (string, history, error) {
	b, ok := rc.Isolator.(brancher)
	if !ok {
		return "", nil, fmt.Errorf("%w: the run's isolator (%T) does not keep candidates on branches, so there is no branch to merge",
			ErrUnpromotable, rc.Isolator)
	}
	repo, ok := rc.VCS.(history)
	if !ok {
		return "", nil, fmt.Errorf("%w: the run's repository (%T) cannot say whether its branch already holds a candidate, which promote must know before it merges or destroys anything",
			ErrUnpromotable, rc.VCS)
	}
	if !filepath.IsAbs(winner.Dir) {
		return "", nil, fmt.Errorf("%w: candidate %s's workspace %q is not an absolute path", ErrUnpromotable, winner.ID, winner.Dir)
	}
	branch, err := b.Branch(winner.ID)
	if err != nil {
		return "", nil, fmt.Errorf("%w: name candidate %s's branch: %w", ErrUnpromotable, winner.ID, err)
	}
	return branch, repo, nil
}

// present reports whether the winner's workspace still exists, which is
// only asked when the run's branch does not record promoting it.
func present(rc *graph.RunContext, winner state.Candidate) error {
	_, err := os.Stat(winner.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: candidate %s's workspace %s no longer exists, and no commit on %s carries its %s and %s trailers",
			ErrWinnerMissing, winner.ID, winner.Dir, rc.Output.Branch, trailerRun, trailerCandidate)
	}
	if err != nil {
		return fmt.Errorf("promote: check candidate %s's workspace %s: %w", winner.ID, winner.Dir, err)
	}
	return nil
}

// record builds the Promotion from the repository's answers alone.
func record(ctx context.Context, rc *graph.RunContext, tip, candidate string) (state.Promotion, error) {
	files, err := rc.VCS.Changed(ctx, rc.Output.Base, tip)
	if err != nil {
		return state.Promotion{}, fmt.Errorf("promote: list the files %s changes since %s: %w", tip, rc.Output.Base, err)
	}
	return state.Promotion{
		Branch:    rc.Output.Branch,
		Base:      rc.Output.Base,
		Commit:    tip,
		Candidate: candidate,
		Files:     len(files),
	}, nil
}

// clean destroys the candidates promote is done with and returns what the
// Note says about any it did not destroy. It destroys nothing the isolator
// did not mark ephemeral, and keeps the winner unless the run's branch
// records promoting it, since without that record a re-run would need the
// workspace. A failure to destroy is reported, never returned: the run's
// result is already on its branch.
func clean(ctx context.Context, rc *graph.RunContext, winner string, recorded bool) string {
	log := logger(rc)
	var kept, leaked []string
	for _, c := range rc.State.Candidates {
		if !c.Ephemeral {
			continue
		}
		if c.ID == winner && !recorded {
			kept = append(kept, c.Dir)
			continue
		}
		ws := belay.Workspace{ID: c.ID, Dir: c.Dir, Ephemeral: c.Ephemeral}
		if err := rc.Isolator.Destroy(ctx, ws); err != nil {
			log.Error("promote: could not destroy a candidate's workspace; the run's result is unaffected",
				slog.String("candidate", c.ID), slog.String("dir", c.Dir), slog.Any("error", err))
			leaked = append(leaked, fmt.Sprintf("%s (%s)", c.ID, c.Dir))
		}
	}

	var note strings.Builder
	if len(kept) > 0 {
		fmt.Fprintf(&note, "; kept the winner's workspace %s, because no commit on %s carries its %s trailer; remove it once you no longer need it",
			kept[0], rc.Output.Branch, trailerCandidate)
	}
	if len(leaked) > 0 {
		fmt.Fprintf(&note, "; could not remove the workspace of candidate %s; see the log", strings.Join(leaked, ", "))
	}
	return note.String()
}

// promoted is the Result of a promotion, with its Note built from the
// Promotion alone plus what clean had to say.
func promoted(p state.Promotion, cleanup string) graph.Result {
	what := "the run's change"
	if p.Candidate != "" {
		what = "candidate " + p.Candidate
	}
	var note string
	if p.Files == 0 {
		note = fmt.Sprintf("%s holds no change from %s: %s changed no files", p.Branch, short(p.Base), what)
	} else {
		note = fmt.Sprintf("%s holds %s at %s: %d %s changed since %s",
			p.Branch, what, short(p.Commit), p.Files, plural(p.Files, "file", "files"), short(p.Base))
	}
	return graph.Result{
		Next:   graph.End,
		Status: journal.StatusOK,
		Patch:  state.Patch{Promotion: &p},
		Note:   note + cleanup,
	}
}

// conflicted is the Result of a winner that does not merge cleanly: a pause
// that changes nothing and tells a person how to finish.
func conflicted(rc *graph.RunContext, winner, branch string, files []string) graph.Result {
	sorted := slices.Clone(files)
	slices.Sort(sorted)
	logger(rc).Warn("promote: the winner conflicts with the run's branch; pausing for a person to merge it",
		slog.String("candidate", winner), slog.String("branch", branch), slog.Any("files", sorted))
	return graph.Result{
		Status: journal.StatusPaused,
		Note: fmt.Sprintf("candidate %s conflicts with %s in %s: in %s, run `git merge %s`, resolve the conflicts and commit, then `belay resume %s`",
			winner, rc.Output.Branch, fileList(sorted), rc.Output.WorkTree, branch, rc.RunID),
	}
}

// unfinished turns a repository's refusal of a working tree that already has
// uncommitted changes into a pause, and reports whether err was one.
//
// It is the other half of conflicted. A conflict pauses and tells a person to
// merge by hand in the run's worktree; stopping part-way through that merge
// is the ordinary shape of being interrupted, and the repository then refuses
// to commit or merge over changes it did not make (vcs.ErrDirtyTarget). An
// error there would end the run, and an ended run cannot be resumed, so the
// person one `git commit` from finishing would lose everything the run spent.
// Pausing costs nothing and `belay resume` finishes the moment they commit.
//
// Every other error is still an error: promote reached no verdict, which is
// what ADR-0002 means by one.
func unfinished(rc *graph.RunContext, err error) (graph.Result, bool) {
	if !errors.Is(err, vcs.ErrDirtyTarget) {
		return graph.Result{}, false
	}
	logger(rc).Warn("promote: the run's worktree has uncommitted changes; pausing for a person to finish",
		slog.String("branch", rc.Output.Branch), slog.String("worktree", rc.Output.WorkTree),
		slog.Any("error", err))
	return graph.Result{
		Status: journal.StatusPaused,
		Note: fmt.Sprintf("%s has changes belay did not commit, so promoting would sweep them in: in %s, run `git status`, finish with `git commit`, then `belay resume %s`",
			rc.Output.Branch, rc.Output.WorkTree, rc.RunID),
	}, true
}

// workMessage is the message of a commit that records work: the run's own
// change, or, given a candidate, that candidate's. unreported are the files
// in it the agent changed and did not report; the body names them, because
// the commit is what a pull request's reader sees, and they should not have
// to open the run's journal to learn which files the agent left out of its
// account.
func workMessage(rc *graph.RunContext, candidate string, unreported []string) vcs.Message {
	subject, body := describe(rc.Goal, rc.RunID)
	if len(unreported) > 0 {
		body = strings.TrimSpace(body + "\n\nThe agent changed these files without reporting them; belay " +
			"recorded and judged them with the rest: " + strings.Join(unreported, ", ") + ".")
	}
	return vcs.Message{Subject: subject, Body: body, Trailers: trailers(rc.RunID, candidate)}
}

// accountFor stands between a tree and its commit: it returns a pause, and
// true, when the tree at top holds anything beyond what the run's steps
// recorded and judged, or, for a guarded run, when what it is about to
// commit breaks the run's policy now.
//
// The set the gate judged and the set promote commits must be one set. A
// commit carries everything git says the tree changed since the run's base
// (vcs.Repository.Commit curates nothing), and between the gate and here the
// test step runs code the agent wrote, the review step runs linters, and a
// paused run's worktree is open to a person. Any of them can change a file.
// A file no step recorded (recorded is the run's, or the winner's, own
// Code.ChangedFiles, relative to workspace) is named and the run pauses:
// committing it would ship something no gate judged while the run reports
// what it verified, and a person can say whether it belongs far more cheaply
// than belay can guess. The approved plan promote itself writes is the one
// file it accounts for on its own.
//
// It pauses rather than fails because nothing is lost: nothing has been
// committed, and `belay resume` asks again once the file is dealt with.
// Including the file and naming it instead was considered and rejected, as
// the less safe of the two: by then it is on the branch a pull request
// proposes.
//
// For a guarded run, the change about to be committed is judged against the
// pinned policy once more, because a file the gate passed can have been
// rewritten since. A violation pauses for the same reason; a rule that
// cannot be evaluated is no verdict, an error.
//
// It converges: a re-run after the commit finds the same change committed
// and the plan beside it, and reaches the same answer.
func accountFor(ctx context.Context, rc *graph.RunContext, top, workspace string, recorded []string) (graph.Result, bool, error) {
	changed, err := rc.VCS.Changes(ctx, top, rc.Output.Base)
	if err != nil {
		return failed(), false, fmt.Errorf("promote: read what %s changed since %s: %w", top, short(rc.Output.Base), err)
	}
	prefix := rc.Output.Prefix
	plan := ""
	if rc.ArchiveDir != "" {
		plan = prefix + planarchive.Rel(rc.ArchiveDir, rc.RunID, rc.Goal)
	}
	var stray, outside, inside []string
	for _, p := range changed {
		if p == plan {
			continue
		}
		rel, within := strings.CutPrefix(p, prefix)
		switch {
		case !within:
			outside = append(outside, p)
		case !slices.Contains(recorded, rel):
			stray = append(stray, p)
		default:
			inside = append(inside, rel)
		}
	}
	if len(stray)+len(outside) > 0 {
		logger(rc).Warn("promote: the tree holds changes no step of the run recorded; pausing",
			slog.String("tree", top), slog.Any("files", slices.Concat(stray, outside)))
		return graph.Result{Status: journal.StatusPaused, Note: unaccountedNote(rc, top, prefix, stray, outside)}, true, nil
	}
	if rc.Policy == nil {
		return graph.Result{}, false, nil
	}
	report, err := rules.Evaluate(ctx, rc.Policy.Policy, rc.Policy.Baseline, rules.Change{
		Workspace: workspace, Files: inside, FoldCase: rc.Policy.FoldCase,
	})
	if err != nil {
		return failed(), false, fmt.Errorf("promote: judge the change about to be committed: %w", err)
	}
	if report.Clean() {
		return graph.Result{}, false, nil
	}
	var fired []string
	for _, v := range report.Violations {
		if !slices.Contains(fired, string(v.Rule)) {
			fired = append(fired, string(v.Rule))
		}
	}
	logger(rc).Warn("promote: the change about to be committed breaks the run's policy; pausing",
		slog.String("rules", strings.Join(fired, ", ")))
	return graph.Result{
		Status: journal.StatusPaused,
		Note: fmt.Sprintf("the change about to be committed breaks this run's policy (%s: %s), though it passed the "+
			"gate: something changed it since. Undo that in %s, then `belay resume %s`",
			report.Summary(), strings.Join(fired, ", "), top, rc.RunID),
	}, true, nil
}

// unaccountedNote tells a person what accountFor found and how to go on.
//
// A change outside the directory a run was started in gets a sentence of
// its own, because undoing it is not always the answer: an agent that added
// a dependency in a subdirectory may have had to touch the go.mod above it.
// No step of such a run records or judges anything outside its directory,
// so the remedy for a change that must reach above it is to run from the
// repository's top.
func unaccountedNote(rc *graph.RunContext, top, prefix string, stray, outside []string) string {
	var parts []string
	if len(stray) > 0 {
		parts = append(parts, fmt.Sprintf("changes no step of this run recorded or judged (%s)", fileList(stray)))
	}
	if len(outside) > 0 {
		parts = append(parts, fmt.Sprintf("changes outside %s, the directory this run was started in, which no step "+
			"of it records or judges (%s)", strings.TrimSuffix(prefix, "/"), fileList(outside)))
	}
	note := fmt.Sprintf("%s holds %s, and belay commits only what its steps recorded: undo them in %s "+
		"(`git -C %s status` lists them), then `belay resume %s`; a change you want can be made after the run",
		top, strings.Join(parts, ", and "), top, top, rc.RunID)
	if len(outside) > 0 {
		note += ", and one that has to reach above " + strings.TrimSuffix(prefix, "/") +
			" needs a run started at the repository's top"
	}
	return note
}

// worktreeTop returns the top of the worktree holding dir, a workspace at
// prefix inside it.
func worktreeTop(dir, prefix string) string {
	if prefix == "" {
		return dir
	}
	return strings.TrimSuffix(filepath.Clean(dir), string(filepath.Separator)+filepath.FromSlash(strings.TrimSuffix(prefix, "/")))
}

// mergeMessage is the message of the merge commit that brings a winner in.
func mergeMessage(rc *graph.RunContext, candidate string) vcs.Message {
	return vcs.Message{
		Subject:  "Merge candidate " + candidate,
		Body:     strings.TrimSpace(rc.Goal),
		Trailers: trailers(rc.RunID, candidate),
	}
}

// trailers names the run and, when given one, the candidate.
func trailers(runID, candidate string) []vcs.Trailer {
	out := []vcs.Trailer{{Key: trailerRun, Value: runID}}
	if candidate != "" {
		out = append(out, vcs.Trailer{Key: trailerCandidate, Value: candidate})
	}
	return out
}

// describe turns a goal into a commit subject and body. The subject is the
// goal's first line with its whitespace collapsed, shortened to subjectWidth;
// the body is the whole goal whenever the subject is not all of it, so
// nothing the person typed is lost.
func describe(goal, runID string) (subject, body string) {
	full := strings.TrimSpace(goal)
	if full == "" {
		return "belay run " + runID, ""
	}
	first, _, _ := strings.Cut(full, "\n")
	subject = strings.Join(strings.Fields(first), " ")
	if utf8.RuneCountInString(subject) > subjectWidth {
		subject = string([]rune(subject)[:subjectWidth-3]) + "..."
	}
	if subject == full {
		return subject, ""
	}
	return subject, full
}

// fileList names up to noteFiles files and counts the rest.
func fileList(files []string) string {
	if len(files) <= noteFiles {
		return strings.Join(files, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(files[:noteFiles], ", "), len(files)-noteFiles)
}

// short shortens a commit ID for a Note.
func short(commit string) string {
	if len(commit) > shortCommit {
		return commit[:shortCommit]
	}
	return commit
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func failed() graph.Result { return graph.Result{Status: journal.StatusFailed} }

// logger returns rc's logger, falling back to the default one. The dispatcher
// always sets Logger; a test building a RunContext by hand often does not.
func logger(rc *graph.RunContext) *slog.Logger {
	if rc.Logger != nil {
		return rc.Logger
	}
	return slog.Default()
}
