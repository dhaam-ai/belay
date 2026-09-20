//go:build unix

package git

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/vcs"
)

// runMessage is the shape promote really commits with: a subject and the
// run's ID as a trailer.
func runMessage(subject string) vcs.Message {
	return vcs.Message{
		Subject:  subject,
		Body:     "Made by belay.",
		Trailers: []vcs.Trailer{{Key: "Belay-Run", Value: "20260918T120000Z-1f2e3d4c5b6a"}},
	}
}

// TestCommit pins the one property every later node depends on: committing
// with nothing to commit makes no commit and reports the commit that is
// already there. The dispatcher re-runs a node whose start has no finish, so
// a promote interrupted after its commit must not add a second one.
func TestCommit(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name string
		// change makes the working tree dirty, and returns the paths the
		// commit must contain.
		change func(dir string) []string
		// commits says how many commits the repository gains.
		commits int
	}{
		{
			name:   "nothing to commit",
			change: func(string) []string { return nil },
		},
		{
			name: "a modification, a deletion and an untracked file in one commit",
			change: func(dir string) []string {
				f.write(dir, "a.txt", "modified\n")
				if err := os.Remove(filepath.Join(dir, "sub", "b.txt")); err != nil {
					t.Fatalf("Remove error = %v", err)
				}
				f.write(dir, "added.txt", "new\n")
				return []string{"a.txt", "added.txt", "sub/b.txt"}
			},
			commits: 1,
		},
		{
			name: "belay's own directory is never committed",
			change: func(dir string) []string {
				f.write(dir, "a.txt", "modified\n")
				f.write(dir, ".belay/runs/r1/journal.ndjson", "{}\n")
				return []string{"a.txt"}
			},
			commits: 1,
		},
		{
			name: "a file whose name holds a space and a newline",
			change: func(dir string) []string {
				f.write(dir, "odd name\nhere.txt", "odd\n")
				return []string{"odd name\nhere.txt"}
			},
			commits: 1,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := f.repo("commit" + strconv.Itoa(i))
			r := f.open(dir)
			before := f.git(dir, "rev-parse", "HEAD")
			beforeCount := f.count(dir)
			want := tt.change(dir)

			msg := runMessage("belay: " + tt.name)
			got, err := r.Commit(t.Context(), dir, msg)
			if err != nil {
				t.Fatalf("Commit() error = %v", err)
			}
			if n := f.count(dir) - beforeCount; n != tt.commits {
				t.Errorf("the repository gained %d commits, want %d", n, tt.commits)
			}
			if tt.commits == 0 {
				if got != before {
					t.Errorf("Commit() = %q with nothing to commit, want the commit already there, %q", got, before)
				}
				return
			}
			if head := f.git(dir, "rev-parse", "HEAD"); got != head {
				t.Errorf("Commit() = %q, want the new HEAD %q", got, head)
			}
			if body := f.git(dir, "log", "-1", "--format=%B"); strings.TrimRight(body, "\n") != msg.Text() {
				t.Errorf("the commit message is %q, want %q", body, msg.Text())
			}
			if run := f.git(dir, "log", "-1", "--format=%(trailers:key=Belay-Run,valueonly)"); strings.TrimSpace(run) != "20260918T120000Z-1f2e3d4c5b6a" {
				t.Errorf("git does not read Belay-Run as a trailer: %q", run)
			}
			changed := strings.Split(f.git(dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "HEAD"), "\x00")
			changed = slices.DeleteFunc(changed, func(s string) bool { return s == "" })
			slices.Sort(changed)
			if !slices.Equal(changed, want) {
				t.Errorf("the commit holds %q, want %q", changed, want)
			}
			if status, err := r.Status(t.Context(), dir); err != nil || len(status) != 0 {
				t.Errorf("the tree is still dirty afterwards: %q (error %v)", status, err)
			}
		})
	}
}

// TestCommitConverges runs Commit twice, which is what a crash between the
// commit and the journal record produces on resume. The second call must
// find nothing to commit and return the same commit as the first.
func TestCommitConverges(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("converge")
	r := f.open(dir)
	f.write(dir, "a.txt", "changed\n")

	first, err := r.Commit(t.Context(), dir, runMessage("belay: change a.txt"))
	if err != nil {
		t.Fatalf("first Commit() error = %v", err)
	}
	count := f.count(dir)

	second, err := r.Commit(t.Context(), dir, runMessage("belay: change a.txt"))
	if err != nil {
		t.Fatalf("second Commit() error = %v", err)
	}
	if second != first {
		t.Errorf("second Commit() = %q, want the first's %q", second, first)
	}
	if n := f.count(dir); n != count {
		t.Errorf("the second Commit added %d commits", n-count)
	}
}

// TestCommitFromASubdirectory proves a commit covers the whole working tree,
// not the directory it was handed. A candidate's workspace can be a
// subdirectory of its worktree, and a change outside it is still part of the
// run's work.
func TestCommitFromASubdirectory(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("subdir")
	r := f.open(dir)
	f.write(dir, "a.txt", "changed at the top\n")
	f.write(dir, "sub/b.txt", "changed below\n")

	if _, err := r.Commit(t.Context(), filepath.Join(dir, "sub"), runMessage("belay: both")); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	changed := strings.Split(f.git(dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "HEAD"), "\x00")
	changed = slices.DeleteFunc(changed, func(s string) bool { return s == "" })
	slices.Sort(changed)
	if want := []string{"a.txt", "sub/b.txt"}; !slices.Equal(changed, want) {
		t.Errorf("the commit holds %q, want %q", changed, want)
	}
}

// TestCommitRefusesAMessageGitWouldMangle pins who validates a message.
// vcs.Message.Text formats and does not validate, and says so: refusing one
// is the Repository's job. An empty subject makes a commit nobody can read
// in a log, and a newline in a trailer breaks the trailer block that carries
// the run's ID.
func TestCommitRefusesAMessageGitWouldMangle(t *testing.T) {
	f := newFixture(t)
	dir := f.repo("message")
	r := f.open(dir)

	tests := []struct {
		name string
		msg  vcs.Message
	}{
		{name: "an empty subject", msg: vcs.Message{Body: "b"}},
		{name: "a subject of only whitespace", msg: vcs.Message{Subject: "  \n\t"}},
		{name: "a subject holding a newline", msg: vcs.Message{Subject: "one\ntwo"}},
		{name: "a trailer key holding a newline", msg: vcs.Message{Subject: "s", Trailers: []vcs.Trailer{{Key: "A\nB", Value: "v"}}}},
		{name: "a trailer value holding a newline", msg: vcs.Message{Subject: "s", Trailers: []vcs.Trailer{{Key: "A", Value: "one\ntwo"}}}},
		{name: "a trailer key holding a colon", msg: vcs.Message{Subject: "s", Trailers: []vcs.Trailer{{Key: "A:B", Value: "v"}}}},
		{name: "an empty trailer key", msg: vcs.Message{Subject: "s", Trailers: []vcs.Trailer{{Key: "", Value: "v"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.write(dir, "a.txt", "changed for "+tt.name+"\n")
			before := f.count(dir)

			_, err := r.Commit(t.Context(), dir, tt.msg)
			if !errors.Is(err, ErrInvalidMessage) {
				t.Errorf("Commit() error = %v, want %v", err, ErrInvalidMessage)
			}
			if n := f.count(dir); n != before {
				t.Errorf("a refused message still made %d commits", n-before)
			}
		})
	}
}

// TestRefusesAForeignGitDir is the threat this package is most careful
// about. An agent has write access to its own worktree, and the worktree's
// .git is a file it can rewrite. Two redirects are dangerous, and both are
// proven here against a real repository:
//
//   - at another repository, where the run's work would be committed;
//   - at this repository's MAIN worktree, which is worse, because the common
//     directory still matches and the candidate's files would be staged into
//     the user's own index and committed onto the branch they have checked
//     out. Verified: with the redirect in place, `git commit` in the
//     candidate put a commit on refs/heads/main.
func TestRefusesAForeignGitDir(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name string
		// gitlink returns the contents to write over the worktree's .git.
		gitlink func(top, other string) string
	}{
		{
			name:    "another repository's git directory",
			gitlink: func(_, other string) string { return "gitdir: " + filepath.Join(other, ".git") + "\n" },
		},
		{
			name:    "this repository's main worktree",
			gitlink: func(top, _ string) string { return "gitdir: " + filepath.Join(top, ".git") + "\n" },
		},
		{
			name: "a git directory the agent built inside its own worktree",
			gitlink: func(top, _ string) string {
				// It names the real common directory, so the common-dir
				// check alone passes, while HEAD and the index are the
				// agent's to choose.
				candidate := filepath.Join(filepath.Dir(top), "candidate-"+filepath.Base(top))
				fake := filepath.Join(candidate, "fake")
				f.write(fake, "commondir", filepath.Join(top, ".git")+"\n")
				f.write(fake, "HEAD", "ref: refs/heads/main\n")
				f.write(fake, "gitdir", filepath.Join(candidate, ".git")+"\n")
				return "gitdir: " + fake + "\n"
			},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			top := f.repo("victim" + strconv.Itoa(i))
			other := f.repo("other" + strconv.Itoa(i))
			r := f.open(top)
			candidate := filepath.Join(filepath.Dir(top), "candidate-"+filepath.Base(top))
			f.git(top, "branch", "cand")
			f.git(top, "worktree", "add", "-q", candidate, "cand")

			f.write(candidate, "work.txt", "the agent's work\n")
			if err := os.WriteFile(filepath.Join(candidate, ".git"), []byte(tt.gitlink(top, other)), 0o600); err != nil {
				t.Fatalf("WriteFile(.git) error = %v", err)
			}
			topCount, otherCount := f.count(top), f.count(other)

			_, err := r.Commit(t.Context(), candidate, runMessage("belay: promote"))
			if !errors.Is(err, ErrForeignGitDir) {
				t.Errorf("Commit() error = %v, want %v", err, ErrForeignGitDir)
			}
			_, err = r.Integrate(t.Context(), candidate, "cand", runMessage("belay: merge"))
			if !errors.Is(err, ErrForeignGitDir) {
				t.Errorf("Integrate() error = %v, want %v", err, ErrForeignGitDir)
			}
			// Reading is refused for the same reason as writing. A promotion
			// that believed a tip or a trailer out of another repository
			// would skip a merge it still owes and destroy the winner that
			// held the only copy of the work.
			_, err = r.Tip(t.Context(), candidate)
			if !errors.Is(err, ErrForeignGitDir) {
				t.Errorf("Tip() error = %v, want %v", err, ErrForeignGitDir)
			}
			_, err = r.Carries(t.Context(), candidate, vcs.Trailer{Key: "Belay-Run", Value: "run1"})
			if !errors.Is(err, ErrForeignGitDir) {
				t.Errorf("Carries() error = %v, want %v", err, ErrForeignGitDir)
			}
			if n := f.count(top); n != topCount {
				t.Errorf("the repository gained %d commits", n-topCount)
			}
			if n := f.count(other); n != otherCount {
				t.Errorf("the other repository gained %d commits", n-otherCount)
			}
		})
	}
}

// TestIntegrate covers every way one line of work meets another. A conflict
// is a verdict, not an error: the caller pauses the run and a human decides,
// with the working tree exactly as it was.
func TestIntegrate(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name string
		// setup prepares the repository at top, whose main branch is the
		// target and whose "cand" branch is the work to integrate.
		setup func(top string)
		// merges is how many merge commits main gains.
		merges        int
		wantConflicts []string
		wantErr       error
		// tipMoves says whether main's tip must change.
		tipMoves bool
	}{
		{
			name: "work that would fast-forward is still a merge commit",
			setup: func(top string) {
				commitOn(f, top, "cand", "from the candidate\n")
			},
			merges:   1,
			tipMoves: true,
		},
		{
			name: "work on a different file merges cleanly",
			setup: func(top string) {
				f.git(top, "checkout", "-q", "cand")
				f.write(top, "candidate.txt", "new file\n")
				f.git(top, "add", "-A")
				f.git(top, "commit", "-qm", "candidate work")
				f.git(top, "checkout", "-q", "main")
				f.write(top, "main.txt", "main moved on\n")
				f.git(top, "add", "-A")
				f.git(top, "commit", "-qm", "main work")
			},
			merges:   1,
			tipMoves: true,
		},
		{
			name: "work the branch already contains makes no commit",
			setup: func(top string) {
				commitOn(f, top, "cand", "already merged\n")
				f.git(top, "merge", "-q", "--no-ff", "-m", "merged by hand", "cand")
			},
		},
		{
			name:  "a branch with no work of its own makes no commit",
			setup: func(string) {},
		},
		{
			name: "a conflict changes nothing and is not an error",
			setup: func(top string) {
				commitOn(f, top, "cand", "the candidate's line\n")
				f.write(top, "a.txt", "the run branch's line\n")
				f.git(top, "commit", "-qam", "main edits the same file")
			},
			wantConflicts: []string{"a.txt"},
		},
		{
			name: "several conflicts come back sorted",
			setup: func(top string) {
				f.git(top, "checkout", "-q", "cand")
				f.write(top, "a.txt", "candidate a\n")
				f.write(top, "sub/b.txt", "candidate b\n")
				f.git(top, "commit", "-qam", "candidate edits both")
				f.git(top, "checkout", "-q", "main")
				f.write(top, "a.txt", "main a\n")
				f.write(top, "sub/b.txt", "main b\n")
				f.git(top, "commit", "-qam", "main edits both")
			},
			wantConflicts: []string{"a.txt", "sub/b.txt"},
		},
		{
			name: "a modify/delete conflict counts too",
			setup: func(top string) {
				commitOn(f, top, "cand", "the candidate keeps editing\n")
				if err := os.Remove(filepath.Join(top, "a.txt")); err != nil {
					t.Fatalf("Remove error = %v", err)
				}
				f.git(top, "commit", "-qam", "main deletes the file")
			},
			wantConflicts: []string{"a.txt"},
		},
		{
			name: "a dirty target is refused",
			setup: func(top string) {
				commitOn(f, top, "cand", "from the candidate\n")
				f.write(top, "a.txt", "uncommitted work in the way\n")
			},
			wantErr: ErrDirtyTarget,
		},
		{
			name: "an untracked file in the target is in the way too",
			setup: func(top string) {
				commitOn(f, top, "cand", "from the candidate\n")
				f.write(top, "stray.txt", "untracked\n")
			},
			wantErr: ErrDirtyTarget,
		},
		{
			name: "belay's own directory does not count as dirty",
			setup: func(top string) {
				commitOn(f, top, "cand", "from the candidate\n")
				f.write(top, ".belay/runs/r1/journal.ndjson", "{}\n")
			},
			merges:   1,
			tipMoves: true,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			top := f.repo("integrate" + strconv.Itoa(i))
			r := f.open(top)
			f.git(top, "branch", "cand")
			tt.setup(top)
			before := f.git(top, "rev-parse", "HEAD")
			beforeMerges := f.mergeCount(top)
			beforeTree := f.tree(top)

			msg := runMessage("belay: integrate the winner")
			got, err := r.Integrate(t.Context(), top, "cand", msg)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Integrate() error = %v, want %v", err, tt.wantErr)
				}
				if now := f.git(top, "rev-parse", "HEAD"); now != before {
					t.Errorf("a refused Integrate moved HEAD from %s to %s", before, now)
				}
				return
			}
			if err != nil {
				t.Fatalf("Integrate() error = %v", err)
			}
			if !slices.Equal(got.Conflicts, tt.wantConflicts) {
				t.Errorf("Integrate().Conflicts = %q, want %q", got.Conflicts, tt.wantConflicts)
			}
			if n := f.mergeCount(top) - beforeMerges; n != tt.merges {
				t.Errorf("main gained %d merge commits, want %d", n, tt.merges)
			}
			head := f.git(top, "rev-parse", "HEAD")
			if got.Tip != head {
				t.Errorf("Integrate().Tip = %q, want HEAD %q", got.Tip, head)
			}
			if moved := head != before; moved != tt.tipMoves {
				t.Errorf("HEAD moved = %t, want %t", moved, tt.tipMoves)
			}
			if len(tt.wantConflicts) > 0 {
				if now := f.tree(top); now != beforeTree {
					t.Error("a conflict changed the working tree; it must be left exactly as it was")
				}
				if _, err := os.Stat(filepath.Join(top, ".git", "MERGE_HEAD")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("a conflict left MERGE_HEAD behind (stat error %v)", err)
				}
				status, err := r.Status(t.Context(), top)
				if err != nil || len(status) != 0 {
					t.Errorf("a conflict left the tree dirty: %q (error %v)", status, err)
				}
			}
			if tt.merges > 0 {
				if body := f.git(top, "log", "-1", "--format=%B"); strings.TrimRight(body, "\n") != msg.Text() {
					t.Errorf("the merge message is %q, want %q", body, msg.Text())
				}
				if parents := strings.Fields(f.git(top, "rev-list", "--parents", "-1", "HEAD")); len(parents) != 3 {
					t.Errorf("the merge commit has %d parents, want 2: a fast-forward would leave no record of the merge", len(parents)-1)
				}
			}
		})
	}
}

// TestIntegrateConverges is the promise the promote node is built on: an
// integration that already happened is not repeated. Running it twice must
// leave one merge commit, whatever the first run did.
func TestIntegrateConverges(t *testing.T) {
	f := newFixture(t)
	top := f.repo("converge")
	r := f.open(top)
	f.git(top, "branch", "cand")
	commitOn(f, top, "cand", "from the candidate\n")

	first, err := r.Integrate(t.Context(), top, "cand", runMessage("belay: integrate"))
	if err != nil {
		t.Fatalf("first Integrate() error = %v", err)
	}
	second, err := r.Integrate(t.Context(), top, "cand", runMessage("belay: integrate"))
	if err != nil {
		t.Fatalf("second Integrate() error = %v", err)
	}
	if second.Tip != first.Tip {
		t.Errorf("second Integrate().Tip = %q, want the first's %q", second.Tip, first.Tip)
	}
	if n := f.mergeCount(top); n != 1 {
		t.Errorf("there are %d merge commits, want 1", n)
	}
}

// TestIntegrateHandlesAnInterruptedMerge covers what a crash between git
// writing MERGE_HEAD and committing leaves behind. belay's own half-done
// merge is undone and redone, so the run converges. A merge someone else
// started, or one whose result has been edited since, is refused instead:
// aborting it would throw away a human's conflict resolution.
func TestIntegrateHandlesAnInterruptedMerge(t *testing.T) {
	f := newFixture(t)

	t.Run("belay's own untouched merge is redone", func(t *testing.T) {
		top := f.repo("ours")
		r := f.open(top)
		f.git(top, "branch", "cand")
		commitOn(f, top, "cand", "from the candidate\n")
		// --no-commit leaves exactly what a crash after MERGE_HEAD leaves.
		f.git(top, "merge", "-q", "--no-ff", "--no-commit", "cand")

		got, err := r.Integrate(t.Context(), top, "cand", runMessage("belay: integrate"))
		if err != nil {
			t.Fatalf("Integrate() error = %v", err)
		}
		if len(got.Conflicts) != 0 {
			t.Errorf("Integrate().Conflicts = %q, want none", got.Conflicts)
		}
		if n := f.mergeCount(top); n != 1 {
			t.Errorf("there are %d merge commits, want 1", n)
		}
		if _, err := os.Stat(filepath.Join(top, ".git", "MERGE_HEAD")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("MERGE_HEAD is still there (stat error %v)", err)
		}
	})

	t.Run("a merge someone has resolved by hand is refused", func(t *testing.T) {
		top := f.repo("theirs")
		r := f.open(top)
		f.git(top, "branch", "cand")
		commitOn(f, top, "cand", "the candidate's line\n")
		f.write(top, "a.txt", "the run branch's line\n")
		f.git(top, "commit", "-qam", "main edits the same file")
		f.gitFails(top, "merge", "--no-ff", "cand")
		// A human resolving the conflict, but not committing yet.
		f.write(top, "a.txt", "the line a human decided on\n")
		f.git(top, "add", "a.txt")

		_, err := r.Integrate(t.Context(), top, "cand", runMessage("belay: integrate"))
		if !errors.Is(err, ErrMergeInProgress) {
			t.Fatalf("Integrate() error = %v, want %v", err, ErrMergeInProgress)
		}
		body, err := os.ReadFile(filepath.Join(top, "a.txt")) //nolint:gosec // G304: top is a repository this test made under t.TempDir().
		if err != nil || string(body) != "the line a human decided on\n" {
			t.Errorf("the human's resolution is now %q (read error %v); it must not be thrown away", body, err)
		}
	})
}

// TestIntegrateAfterAHumanMerge is the other side of the conflict pause: a
// human merges the candidate themselves and commits, and the next
// integration finds the work already there and adds nothing.
func TestIntegrateAfterAHumanMerge(t *testing.T) {
	f := newFixture(t)
	top := f.repo("human")
	r := f.open(top)
	f.git(top, "branch", "cand")
	commitOn(f, top, "cand", "the candidate's line\n")
	f.write(top, "a.txt", "the run branch's line\n")
	f.git(top, "commit", "-qam", "main edits the same file")

	got, err := r.Integrate(t.Context(), top, "cand", runMessage("belay: integrate"))
	if err != nil || len(got.Conflicts) == 0 {
		t.Fatalf("Integrate() = %+v, %v; want a conflict", got, err)
	}

	f.gitFails(top, "merge", "--no-ff", "-m", "merged by a human", "cand")
	f.write(top, "a.txt", "the line a human decided on\n")
	f.git(top, "add", "a.txt")
	f.git(top, "commit", "-qm", "merged by a human")
	theirs := f.git(top, "rev-parse", "HEAD")
	merges := f.mergeCount(top)

	got, err = r.Integrate(t.Context(), top, "cand", runMessage("belay: integrate"))
	if err != nil {
		t.Fatalf("Integrate() after the human merge error = %v", err)
	}
	if got.Tip != theirs {
		t.Errorf("Integrate().Tip = %q, want the human's merge commit %q", got.Tip, theirs)
	}
	if n := f.mergeCount(top); n != merges {
		t.Errorf("Integrate added %d merge commits on top of the human's", n-merges)
	}
}

// TestChanged pins the file list a promotion reports. A rename is two paths,
// because both of them differ between the commits; git's rename detection
// would report only the new one and quietly drop the old path from the list.
func TestChanged(t *testing.T) {
	f := newFixture(t)
	top := f.repo("changed")
	r := f.open(top)
	base := f.git(top, "rev-parse", "HEAD")

	f.write(top, "a.txt", "modified\n")
	f.write(top, "added.txt", "new\n")
	f.write(top, "odd name\nhere.txt", "odd\n")
	f.git(top, "mv", "sub/b.txt", "sub/renamed.txt")
	f.git(top, "add", "-A")
	f.git(top, "commit", "-qm", "a bit of everything")
	tip := f.git(top, "rev-parse", "HEAD")

	got, err := r.Changed(t.Context(), base, tip)
	if err != nil {
		t.Fatalf("Changed() error = %v", err)
	}
	slices.Sort(got)
	want := []string{"a.txt", "added.txt", "odd name\nhere.txt", "sub/b.txt", "sub/renamed.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("Changed() = %q, want %q", got, want)
	}

	same, err := r.Changed(t.Context(), tip, tip)
	if err != nil || len(same) != 0 {
		t.Errorf("Changed(tip, tip) = %q, %v; want nothing and no error", same, err)
	}
	if _, err := r.Changed(t.Context(), base, "no-such-revision"); err == nil {
		t.Error("Changed(<unknown revision>) error = nil, want an error")
	}
}

// TestRepositoryHooksNeverRun proves the hardening holds for every command
// that can fire one. A repository's hooks are code belay never agreed to run
// on the user's behalf: post-checkout fires on `worktree add`, pre-commit and
// commit-msg on `commit`, pre-merge-commit and post-merge on `merge`, and
// reference-transaction on every ref update, including the branch creation
// and deletion belay does with update-ref.
func TestRepositoryHooksNeverRun(t *testing.T) {
	f := newFixture(t)
	top := f.repo("hooks")
	r := f.open(top)
	marker := filepath.Join(f.root, "hooks-fired")
	for _, hook := range []string{
		"post-checkout", "pre-commit", "commit-msg", "prepare-commit-msg", "post-commit",
		"pre-merge-commit", "post-merge", "reference-transaction", "post-index-change",
	} {
		f.writeExecutable(filepath.Join(top, ".git", "hooks", hook),
			"#!/bin/sh\necho "+hook+" >> "+strconv.Quote(marker)+"\n")
	}

	head := f.git(top, "rev-parse", "HEAD")
	if err := r.CreateBranch(t.Context(), "cand", head); err != nil {
		t.Fatalf("CreateBranch() error = %v", err)
	}
	wt := filepath.Join(f.root, "hooks-wt")
	if err := r.AddWorktree(t.Context(), wt, "cand"); err != nil {
		t.Fatalf("AddWorktree() error = %v", err)
	}
	f.write(wt, "candidate.txt", "work\n")
	if _, err := r.Commit(t.Context(), wt, runMessage("belay: candidate work")); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if _, err := r.Integrate(t.Context(), top, "cand", runMessage("belay: integrate")); err != nil {
		t.Fatalf("Integrate() error = %v", err)
	}
	if err := r.RemoveWorktree(t.Context(), wt); err != nil {
		t.Fatalf("RemoveWorktree() error = %v", err)
	}
	tip, _, err := r.BranchTip(t.Context(), "cand")
	if err != nil {
		t.Fatalf("BranchTip() error = %v", err)
	}
	if err := r.DeleteBranch(t.Context(), "cand", tip); err != nil {
		t.Fatalf("DeleteBranch() error = %v", err)
	}

	if body, err := os.ReadFile(marker); !errors.Is(err, os.ErrNotExist) { //nolint:gosec // G304: marker is a path this test built under t.TempDir().
		t.Errorf("the repository's hooks ran: %q (read error %v)", body, err)
	}
}
