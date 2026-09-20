package promote_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"

	"github.com/dhaam-ai/belay/internal/vcs"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// errNoWorktree is what fakeRepo returns for a directory no worktree is
// checked out at, as git does for a worktree that was removed.
var errNoWorktree = errors.New("fake repo: not a working tree")

// crash is what a fake panics with to stop an attempt at an exact point, the
// way a kill -9 would: the node never returns, so it cannot react to it.
type crash struct{ at string }

// callLog records every call that changes a repository or a workspace, in
// the order it happened, across the repository and the isolator. A test
// reads it to check the order promote acts in, and sets crashAt to stop the
// attempt right after one of them.
type callLog struct {
	mu      sync.Mutex
	calls   []string
	crashAt string
}

func (l *callLog) record(call string) {
	l.mu.Lock()
	l.calls = append(l.calls, call)
	stop := l.crashAt != "" && l.crashAt == call
	if stop {
		// Disarmed once it fires, so the re-run the test makes next is a
		// clean attempt, as it is after a real crash.
		l.crashAt = ""
	}
	l.mu.Unlock()
	if stop {
		panic(crash{at: call})
	}
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// fakeCommit is one commit in fakeRepo.
type fakeCommit struct {
	parents []string
	message vcs.Message
	// files are the files this commit itself changed. A merge commit
	// changes none of its own: what it brings in is its second parent's.
	files []string
}

// fakeRepo is a small model of a git repository: branches, commits with
// parents, worktrees checked out on branches, and uncommitted changes in
// each worktree. It answers the questions promote asks the way a real
// repository would, from what it holds, so a test can interrupt promote at
// any point and see what a re-run makes of the repository it left behind.
//
// It implements vcs.Repository, and the two questions promote asks on top of
// it: Tip and Carries.
type fakeRepo struct {
	mu sync.Mutex
	// log is shared with the isolator, so the order of their calls is one
	// sequence.
	log *callLog

	// worktrees maps a worktree directory to the branch checked out there.
	worktrees map[string]string
	// tips maps a branch to the commit it points to.
	tips map[string]string
	// commits maps a commit ID to its record.
	commits map[string]fakeCommit
	// dirty maps a worktree directory to the files changed there and not
	// yet committed.
	dirty map[string][]string
	// conflicts maps a branch to the files merging it conflicts on.
	conflicts map[string][]string
	// failures makes the named method return an error.
	failures map[string]error

	next int
}

var _ vcs.Repository = (*fakeRepo)(nil)

func newFakeRepo(log *callLog, base string) *fakeRepo {
	return &fakeRepo{
		log:       log,
		worktrees: map[string]string{},
		tips:      map[string]string{},
		commits:   map[string]fakeCommit{base: {}},
		dirty:     map[string][]string{},
		conflicts: map[string][]string{},
		failures:  map[string]error{},
	}
}

// branch creates branch at commit and checks it out in dir, with files
// changed and not committed.
func (r *fakeRepo) branch(dir, branch, commit string, uncommitted ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tips[branch] = commit
	r.worktrees[dir] = branch
	r.dirty[dir] = slices.Clone(uncommitted)
}

// commitOn records a commit on branch directly, standing in for history
// made outside promote: a person's commit, or an earlier run's.
func (r *fakeRepo) commitOn(branch string, msg vcs.Message, files ...string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.newID()
	r.commits[id] = fakeCommit{parents: []string{r.tips[branch]}, message: msg, files: files}
	r.tips[branch] = id
	return id
}

// humanMerge merges from into the branch checked out at dir the way a
// person resolving a conflict would: a merge commit with git's own message,
// which carries no trailer.
func (r *fakeRepo) humanMerge(dir, from string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	target := r.worktrees[dir]
	id := r.newID()
	r.commits[id] = fakeCommit{
		parents: []string{r.tips[target], r.tips[from]},
		message: vcs.Message{Subject: "Merge branch '" + from + "'"},
	}
	r.tips[target] = id
	delete(r.conflicts, from)
	return id
}

// removeWorktree is what destroying a candidate does to the repository: its
// worktree goes, and its branch goes too once another branch holds its
// commits, as the worktree isolator's Destroy does.
func (r *fakeRepo) removeWorktree(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	branch, ok := r.worktrees[dir]
	if !ok {
		return
	}
	delete(r.worktrees, dir)
	delete(r.dirty, dir)
	tip := r.tips[branch]
	for other, otherTip := range r.tips {
		if other != branch && r.reachable(otherTip)[tip] {
			delete(r.tips, branch)
			return
		}
	}
}

func (r *fakeRepo) newID() string {
	r.next++
	return fmt.Sprintf("commit-%02d", r.next)
}

// reachable is every commit reachable from tip, itself included.
func (r *fakeRepo) reachable(tip string) map[string]bool {
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		queue = append(queue, r.commits[id].parents...)
	}
	return seen
}

// merges counts the merge commits on branch, which is how a test sees
// whether a re-run merged a second time.
func (r *fakeRepo) merges(branch string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id := range r.reachable(r.tips[branch]) {
		if len(r.commits[id].parents) > 1 {
			n++
		}
	}
	return n
}

// commitsWithSubject counts the ordinary (single-parent) commits on branch
// whose subject is subject, which is how a test sees whether a re-run
// committed the same work twice.
func (r *fakeRepo) commitsWithSubject(branch, subject string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id := range r.reachable(r.tips[branch]) {
		c := r.commits[id]
		if len(c.parents) == 1 && c.message.Subject == subject {
			n++
		}
	}
	return n
}

func (r *fakeRepo) tipOf(branch string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tips[branch]
}

func (r *fakeRepo) messageOf(commit string) vcs.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commits[commit].message
}

// Commit implements vcs.Repository: nothing to commit returns the commit
// the worktree already has, as the contract requires.
func (r *fakeRepo) Commit(_ context.Context, dir string, msg vcs.Message) (string, error) {
	r.mu.Lock()
	if err := r.failures["Commit"]; err != nil {
		r.mu.Unlock()
		return "", err
	}
	branch, ok := r.worktrees[dir]
	if !ok {
		r.mu.Unlock()
		return "", fmt.Errorf("commit in %s: %w", dir, errNoWorktree)
	}
	if len(r.dirty[dir]) == 0 {
		tip := r.tips[branch]
		r.mu.Unlock()
		return tip, nil
	}
	id := r.newID()
	r.commits[id] = fakeCommit{parents: []string{r.tips[branch]}, message: msg, files: r.dirty[dir]}
	r.tips[branch] = id
	r.dirty[dir] = nil
	r.mu.Unlock()

	r.log.record("Commit " + dir)
	return id, nil
}

// Integrate implements vcs.Repository: a merge commit with two parents,
// nothing at all when the branch already holds from, and a conflict that
// changes nothing.
func (r *fakeRepo) Integrate(_ context.Context, dir, from string, msg vcs.Message) (vcs.Integration, error) {
	r.mu.Lock()
	if err := r.failures["Integrate"]; err != nil {
		r.mu.Unlock()
		return vcs.Integration{}, err
	}
	target, ok := r.worktrees[dir]
	if !ok {
		r.mu.Unlock()
		return vcs.Integration{}, fmt.Errorf("integrate into %s: %w", dir, errNoWorktree)
	}
	fromTip, ok := r.tips[from]
	if !ok {
		r.mu.Unlock()
		return vcs.Integration{}, fmt.Errorf("fake repo: no branch %q", from)
	}
	if r.reachable(r.tips[target])[fromTip] {
		tip := r.tips[target]
		r.mu.Unlock()
		return vcs.Integration{Tip: tip}, nil
	}
	if files := r.conflicts[from]; len(files) > 0 {
		tip := r.tips[target]
		r.mu.Unlock()
		return vcs.Integration{Tip: tip, Conflicts: slices.Clone(files)}, nil
	}
	id := r.newID()
	r.commits[id] = fakeCommit{parents: []string{r.tips[target], fromTip}, message: msg}
	r.tips[target] = id
	r.mu.Unlock()

	r.log.record("Integrate " + from)
	return vcs.Integration{Tip: id}, nil
}

// Changed implements vcs.Repository: every file a commit reachable from to
// but not from from changed.
func (r *fakeRepo) Changed(_ context.Context, from, to string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failures["Changed"]; err != nil {
		return nil, err
	}
	before := r.reachable(from)
	var files []string
	for id := range r.reachable(to) {
		if !before[id] {
			files = append(files, r.commits[id].files...)
		}
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

// Tip is the question promote asks for the commit a branch points to.
func (r *fakeRepo) Tip(_ context.Context, dir string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failures["Tip"]; err != nil {
		return "", err
	}
	branch, ok := r.worktrees[dir]
	if !ok {
		return "", fmt.Errorf("tip of %s: %w", dir, errNoWorktree)
	}
	return r.tips[branch], nil
}

// Carries is the question promote asks before it merges a winner: whether
// one commit on the branch at dir carries every trailer given.
func (r *fakeRepo) Carries(_ context.Context, dir string, trailers ...vcs.Trailer) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failures["Carries"]; err != nil {
		return false, err
	}
	branch, ok := r.worktrees[dir]
	if !ok {
		return false, fmt.Errorf("history of %s: %w", dir, errNoWorktree)
	}
	for id := range r.reachable(r.tips[branch]) {
		carried := r.commits[id].message.Trailers
		if len(trailers) > 0 && !slices.ContainsFunc(trailers, func(t vcs.Trailer) bool {
			return !slices.Contains(carried, t)
		}) {
			return true, nil
		}
	}
	return false, nil
}

// plainRepo is a vcs.Repository that cannot answer Tip or Carries, so a test
// can show what promote does with a repository that only meets the
// interface every repository must.
type plainRepo struct{ vcs.Repository }

// branchingIsolator is a belaytest.FakeIsolator whose workspaces are
// branches, named the way the worktree isolator names them. Destroying one
// removes its worktree from the fake repository and its directory from
// disk, so a re-run finds it gone exactly as it would after a real Destroy.
type branchingIsolator struct {
	*belaytest.FakeIsolator
	namespace string
	repo      *fakeRepo
	log       *callLog
	// branchErr makes Branch fail.
	branchErr error
}

func newBranchingIsolator(namespace string, repo *fakeRepo, log *callLog) *branchingIsolator {
	b := &branchingIsolator{FakeIsolator: &belaytest.FakeIsolator{}, namespace: namespace, repo: repo, log: log}
	b.DestroyFunc = func(_ context.Context, ws belay.Workspace) error {
		repo.removeWorktree(ws.Dir)
		if err := os.RemoveAll(ws.Dir); err != nil {
			return err
		}
		log.record("Destroy " + ws.ID)
		return nil
	}
	return b
}

// Branch derives a candidate's branch from its ID, as the worktree
// isolator does.
func (b *branchingIsolator) Branch(id string) (string, error) {
	if b.branchErr != nil {
		return "", b.branchErr
	}
	return b.namespace + "-" + id, nil
}
