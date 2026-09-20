//go:build unix

package git

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/internal/vcs"
)

// promoteHistory is the optional interface internal/nodes/promote looks for
// on a run's repository, on top of vcs.Repository, restated here so that a
// change to either signature stops compiling. promote cannot import this
// package — internal/graph and the nodes stay free of the unix build tag
// internal/exec brings — so nothing but a pair of assertions holds the two
// ends of that seam together.
type promoteHistory interface {
	Tip(ctx context.Context, dir string) (string, error)
	Carries(ctx context.Context, dir string, trailers ...vcs.Trailer) (bool, error)
}

var _ promoteHistory = (*Repo)(nil)

// commitTrailers commits a new file in dir under a message written exactly as
// vcs.Message.Text renders one, which is the shape promote's commits have.
// It returns the commit's ID.
func (f *fixture) commitTrailers(dir, name, subject string, trailers ...vcs.Trailer) string {
	f.t.Helper()
	f.write(dir, name, name+"\n")
	f.git(dir, "add", "-A")
	f.git(dir, "commit", "-qm", vcs.Message{Subject: subject, Trailers: trailers}.Text())
	return f.git(dir, "rev-parse", "HEAD")
}

// TestTip pins what promote reads the run's branch tip with: the commit HEAD
// is on in the working tree it was handed, which for a run in its own
// worktree is not the commit the user's checkout is on.
func TestTip(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name string
		// build prepares a repository and returns the repository to ask, the
		// directory to ask about and the commit Tip must report.
		build   func(i int) (r *Repo, dir, want string)
		wantErr error
	}{
		{
			name: "the repository top reports the commit HEAD is on",
			build: func(i int) (*Repo, string, string) {
				top := f.repo("tip" + strconv.Itoa(i))
				return f.open(top), top, f.git(top, "rev-parse", "HEAD")
			},
		},
		{
			name: "a linked worktree reports its own HEAD, not the main tree's",
			build: func(i int) (*Repo, string, string) {
				top := f.repo("tip" + strconv.Itoa(i))
				wt := filepath.Join(f.root, "tip-linked"+strconv.Itoa(i))
				f.git(top, "branch", "side")
				f.git(top, "worktree", "add", "-q", wt, "side")
				f.write(wt, "w.txt", "w\n")
				f.git(wt, "add", "-A")
				f.git(wt, "commit", "-qm", "work on the side branch")
				want := f.git(wt, "rev-parse", "HEAD")
				if want == f.git(top, "rev-parse", "HEAD") {
					t.Fatalf("the worktree and the main tree are on the same commit, so the row proves nothing")
				}
				return f.open(top), wt, want
			},
		},
		{
			name: "a repository with no commits",
			build: func(i int) (*Repo, string, string) {
				top := f.emptyRepo("tip" + strconv.Itoa(i))
				return f.open(top), top, ""
			},
			wantErr: ErrNoCommits,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, dir, want := tt.build(i)
			got, err := r.Tip(t.Context(), dir)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Tip(%q) error = %v, want %v", dir, err, tt.wantErr)
				}
				if got != "" {
					t.Errorf("Tip(%q) = %q with an error, want the zero value", dir, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Tip(%q) error = %v", dir, err)
			}
			if got != want {
				t.Errorf("Tip(%q) = %q, want %q", dir, got, want)
			}
		})
	}
}

// carriesHistory builds the history every TestCarries row asks about and
// returns the repository top. It is one repository for every row because the
// rows differ only in the question, not in the history.
//
// The shapes that matter, in the order they are committed:
//
//   - a commit carrying both of a run's trailers;
//   - a commit carrying one of them, so a row can prove the two must meet on
//     one commit rather than anywhere in the history;
//   - a commit whose last paragraph holds prose before the trailer lines,
//     which git does not read as a trailer block;
//   - a branch that is never merged, whose commits the run's branch cannot
//     reach;
//   - a branch that is merged, whose commits it can.
func carriesHistory(f *fixture, name string) string {
	f.t.Helper()
	top := f.repo(name)

	f.commitTrailers(top, "both.txt", "belay: promote the winner",
		vcs.Trailer{Key: "Belay-Run", Value: "RUN-A"},
		vcs.Trailer{Key: "Belay-Candidate", Value: "cand-01"})
	f.commitTrailers(top, "one.txt", "belay: promote", vcs.Trailer{Key: "Belay-Run", Value: "RUN-B"})

	f.write(top, "buried.txt", "buried\n")
	f.git(top, "add", "-A")
	f.git(top, "commit", "-qm", "belay: promote\n\nprose that makes this no trailer block\nBelay-Run: RUN-BURIED\nBelay-Candidate: cand-buried")

	base := f.git(top, "rev-parse", "HEAD")
	unmerged := filepath.Join(f.root, name+"-unmerged")
	f.git(top, "branch", "unmerged")
	f.git(top, "worktree", "add", "-q", unmerged, "unmerged")
	f.commitTrailers(unmerged, "unmerged.txt", "belay: promote",
		vcs.Trailer{Key: "Belay-Run", Value: "RUN-UNMERGED"},
		vcs.Trailer{Key: "Belay-Candidate", Value: "cand-unmerged"})

	merged := filepath.Join(f.root, name+"-merged")
	f.git(top, "branch", "merged", base)
	f.git(top, "worktree", "add", "-q", merged, "merged")
	f.commitTrailers(merged, "merged.txt", "belay: promote",
		vcs.Trailer{Key: "Belay-Run", Value: "RUN-MERGED"},
		vcs.Trailer{Key: "Belay-Candidate", Value: "cand-merged"})
	f.git(top, "merge", "-q", "--no-ff", "-m", "belay: merge the winner", "merged")
	return top
}

// TestCarries is the question promote asks before it merges a winner: does
// the run's branch already hold a commit carrying both this run's trailer and
// the winner's?
//
// Two rows carry the whole reason the question is shaped that way. The two
// trailers must meet on ONE commit, because candidate IDs repeat from run to
// run and an earlier run's merged work would otherwise read as "already
// done". And a trailer only git reads counts: a line that looks like one in
// the middle of a paragraph is prose, and answering from message text would
// make it a promotion that never happened.
func TestCarries(t *testing.T) {
	f := newFixture(t)
	top := carriesHistory(f, "carries")
	r := f.open(top)

	run := func(v string) vcs.Trailer { return vcs.Trailer{Key: "Belay-Run", Value: v} }
	cand := func(v string) vcs.Trailer { return vcs.Trailer{Key: "Belay-Candidate", Value: v} }

	tests := []struct {
		name     string
		trailers []vcs.Trailer
		want     bool
		wantErr  error
	}{
		{
			name:     "both trailers on one commit",
			trailers: []vcs.Trailer{run("RUN-A"), cand("cand-01")},
			want:     true,
		},
		{
			name:     "the two trailers are on different commits",
			trailers: []vcs.Trailer{run("RUN-B"), cand("cand-01")},
		},
		{
			name:     "one trailer that is there",
			trailers: []vcs.Trailer{run("RUN-B")},
			want:     true,
		},
		{
			name:     "a trailer key is matched whatever its case",
			trailers: []vcs.Trailer{{Key: "belay-run", Value: "RUN-A"}, {Key: "BELAY-CANDIDATE", Value: "cand-01"}},
			want:     true,
		},
		{
			name:     "a trailer value is matched exactly",
			trailers: []vcs.Trailer{run("run-a"), cand("cand-01")},
		},
		{
			name:     "a commit only an unmerged branch can reach",
			trailers: []vcs.Trailer{run("RUN-UNMERGED"), cand("cand-unmerged")},
		},
		{
			name:     "a commit reached through a merge",
			trailers: []vcs.Trailer{run("RUN-MERGED"), cand("cand-merged")},
			want:     true,
		},
		{
			name:     "lines git does not read as a trailer block",
			trailers: []vcs.Trailer{run("RUN-BURIED"), cand("cand-buried")},
		},
		{
			name:    "no trailer to look for",
			wantErr: ErrInvalidTrailer,
		},
		{
			name:     "a key holding a comma, which git would read as the next atom option",
			trailers: []vcs.Trailer{{Key: "Belay-Run,valueonly", Value: "RUN-A"}},
			wantErr:  ErrInvalidTrailer,
		},
		{
			name:     "a key holding a parenthesis, which git would read as the end of the atom",
			trailers: []vcs.Trailer{{Key: "Belay-Run)x", Value: "RUN-A"}},
			wantErr:  ErrInvalidTrailer,
		},
		{
			name:     "a key holding a colon, which no trailer key can",
			trailers: []vcs.Trailer{{Key: "Belay-Run:", Value: "RUN-A"}},
			wantErr:  ErrInvalidTrailer,
		},
		{
			name:     "an empty key",
			trailers: []vcs.Trailer{{Value: "RUN-A"}},
			wantErr:  ErrInvalidTrailer,
		},
		{
			name:     "an empty value, which every commit without the trailer would match",
			trailers: []vcs.Trailer{{Key: "Belay-Run"}},
			wantErr:  ErrInvalidTrailer,
		},
		{
			name:     "a value holding a line break, which no trailer value can",
			trailers: []vcs.Trailer{run("RUN-A\nBelay-Candidate: cand-01")},
			wantErr:  ErrInvalidTrailer,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.Carries(t.Context(), top, tt.trailers...)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Carries(%v) error = %v, want %v", tt.trailers, err, tt.wantErr)
				}
				if got {
					t.Errorf("Carries(%v) = true with an error, want false", tt.trailers)
				}
				return
			}
			if err != nil {
				t.Fatalf("Carries(%v) error = %v", tt.trailers, err)
			}
			if got != tt.want {
				t.Errorf("Carries(%v) = %v, want %v", tt.trailers, got, tt.want)
			}
		})
	}
}

// TestCarriesAsksTheWorktreeItWasGiven proves the answer is about the branch
// checked out at dir and not about the repository's main checkout. A run
// works on its own branch in its own worktree, so an answer taken from the
// main tree would say "not promoted" for work that is on the run's branch,
// and promote would merge a winner it had already merged.
func TestCarriesAsksTheWorktreeItWasGiven(t *testing.T) {
	f := newFixture(t)
	top := f.repo("carries-worktree")
	wt := filepath.Join(f.root, "carries-worktree-run")
	f.git(top, "branch", "belay/run1")
	f.git(top, "worktree", "add", "-q", wt, "belay/run1")
	f.commitTrailers(wt, "work.txt", "belay: promote the winner",
		vcs.Trailer{Key: "Belay-Run", Value: "run1"},
		vcs.Trailer{Key: "Belay-Candidate", Value: "cand-01"})

	r := f.open(top)
	mark := []vcs.Trailer{{Key: "Belay-Run", Value: "run1"}, {Key: "Belay-Candidate", Value: "cand-01"}}

	got, err := r.Carries(t.Context(), wt, mark...)
	if err != nil {
		t.Fatalf("Carries(run worktree) error = %v", err)
	}
	if !got {
		t.Errorf("Carries(run worktree) = false, want true: the commit is on that branch")
	}
	got, err = r.Carries(t.Context(), top, mark...)
	if err != nil {
		t.Fatalf("Carries(main worktree) error = %v", err)
	}
	if got {
		t.Errorf("Carries(main worktree) = true, want false: the main branch never saw that commit")
	}
}

// TestCarriesReadsTheTrailersAtom pins how the answer is obtained: one
// `git log` whose --format asks git for the trailer values it parsed, with no
// --grep and nothing that reads the message text. A grep would match the same
// line in prose, in a quoted diff or in a revert's body.
func TestCarriesReadsTheTrailersAtom(t *testing.T) {
	f := newFixture(t)
	top := f.repo("carries-argv")
	rec := &recorder{next: f.run}
	r, err := Open(t.Context(), rec, top)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	rec.reset()
	if _, err := r.Carries(t.Context(), top, vcs.Trailer{Key: "Belay-Run", Value: "RUN-A"}); err != nil {
		t.Fatalf("Carries() error = %v", err)
	}

	var logs []bexec.Command
	for _, c := range rec.commands() {
		if subcommand(c) == "log" {
			logs = append(logs, c)
		}
	}
	if len(logs) != 1 {
		t.Fatalf("Carries ran %d git log commands, want 1: %v", len(logs), rec.commands())
	}
	argv := strings.Join(logs[0].Args, " ")
	for _, want := range []string{"--format=", "%(trailers:", "key=Belay-Run", "valueonly", "unfold", "-z"} {
		if !strings.Contains(argv, want) {
			t.Errorf("git log argv %q does not contain %q", argv, want)
		}
	}
	for _, unwanted := range []string{"--grep", "--fixed-strings", "%B", "%s", "%b"} {
		if strings.Contains(argv, unwanted) {
			t.Errorf("git log argv %q reads message text (%q)", argv, unwanted)
		}
	}
}

// TestCarriesRefusesAGitThatDoesNotExpandTheTrailersAtom is the floor check.
// A git that does not know an option of the %(trailers:...) atom prints the
// atom verbatim instead of failing, so the values would come back as the
// format string and every commit would read as "not promoted" — a wrong
// answer that looks like a right one. [MinVersion] is 2.38 and every option
// used here is older than that, so this can only be a binary that is not the
// git Open checked; either way it is refused, not read.
func TestCarriesRefusesAGitThatDoesNotExpandTheTrailersAtom(t *testing.T) {
	f := newFixture(t)
	top := f.repo("carries-old")
	working := f.open(top)

	// Only `git log` is answered staleley; everything else runs for real, so
	// the worktree checks and the HEAD lookup behave exactly as they do in
	// production and the atom is the only thing that is old.
	r, err := Open(t.Context(), &staleAtom{next: f.run}, top)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := working.Carries(t.Context(), top, vcs.Trailer{Key: "Belay-Run", Value: "RUN-A"}); err != nil {
		t.Fatalf("the real Carries must work for the stale one to mean anything: %v", err)
	}

	got, err := r.Carries(t.Context(), top, vcs.Trailer{Key: "Belay-Run", Value: "RUN-A"})
	if !errors.Is(err, ErrTooOld) {
		t.Fatalf("Carries() error = %v, want %v", err, ErrTooOld)
	}
	if got {
		t.Errorf("Carries() = true from a git that printed the format back, want false")
	}
}

// staleAtom is an Execer that answers `git log` the way a git too old for the
// %(trailers:...) atom does: with the atom printed back verbatim.
type staleAtom struct{ next Execer }

func (p *staleAtom) Run(ctx context.Context, c bexec.Command) (bexec.Result, error) {
	if subcommandOf(c.Args) != "log" {
		return p.next.Run(ctx, c)
	}
	var format string
	for _, a := range c.Args {
		if after, ok := strings.CutPrefix(a, "--format="); ok {
			format = after
		}
	}
	return bexec.Result{Stdout: format + "\x00"}, nil
}
