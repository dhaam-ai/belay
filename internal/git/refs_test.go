//go:build unix

package git

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	bexec "github.com/dhaam-ai/belay/internal/exec"
)

// refCorpus is every string both ref tests below run. Each row says what the
// Go validator must decide and why, so a rule that changes has to change a
// reason with it.
var refCorpus = []struct {
	name string
	in   string
	ok   bool
	why  string
}{
	{name: "a candidate id", in: "cand-01-a1", ok: true},
	{name: "a run id", in: "20260918T120000Z-1f2e3d4c5b6a", ok: true},
	{name: "a run-qualified candidate id", in: "20260918T120000Z-1f2e3d4c5b6a-cand-01-a1", ok: true},
	{name: "an underscore and a digit", in: "fix_2", ok: true},
	{name: "a single dot inside", in: "v1.2.3", ok: true},
	{name: "non-ASCII, which git allows", in: "ünïcode", ok: true},
	{name: "lock without the dot", in: "xlock", ok: true},

	{name: "empty", in: "", why: "there is no such ref"},
	{name: "a single dot", in: ".", why: "a component may not begin with a dot"},
	{name: "two dots", in: "..", why: "a component may not contain .."},
	{name: "dots inside", in: "a..b", why: "a component may not contain .."},
	{name: "a leading dot", in: ".hidden", why: "a component may not begin with a dot"},
	{name: "a trailing dot", in: "trailing.", why: "a component may not end with a dot"},
	{name: "a .lock suffix", in: "x.lock", why: "git's own lock files end in .lock"},
	{name: "an uppercase .LOCK suffix", in: "x.LOCK", why: "a case-insensitive filesystem makes it the same file"},
	{name: "a slash", in: "a/b", why: "this validates one component, not a path"},
	{name: "a trailing slash", in: "a/", why: "this validates one component, not a path"},
	{name: "a space", in: "a b", why: "git forbids a space"},
	{name: "a tab", in: "a\tb", why: "git forbids a control character"},
	{name: "a newline", in: "a\nb", why: "git forbids a control character"},
	{name: "a NUL", in: "a\x00b", why: "exec would truncate the argument at the NUL"},
	{name: "a DEL", in: "a\x7fb", why: "git forbids DEL"},
	{name: "a tilde", in: "a~b", why: "git reads ~ as a revision suffix"},
	{name: "a caret", in: "a^b", why: "git reads ^ as a revision suffix"},
	{name: "a colon", in: "a:b", why: "git reads : as a refspec separator"},
	{name: "a question mark", in: "a?b", why: "git forbids ?"},
	{name: "a star", in: "a*b", why: "git forbids *"},
	{name: "an open bracket", in: "a[b", why: "git forbids ["},
	{name: "a backslash", in: "a\\b", why: "git forbids a backslash"},
	{name: "an at-brace", in: "a@{b", why: "git reads @{ as a reflog selector"},
	{name: "a bare at", in: "@", why: "@ alone is a refname git refuses"},
	{name: "a leading dash", in: "-lead", why: "git would read it as an option"},
	{name: "a leading double dash", in: "--lead", why: "git would read it as an option"},
	{name: "longer than a filename", in: strings.Repeat("a", 256), why: "the files ref backend stores it as a filename"},
}

// TestValidateRefComponent pins the rule set. Every reject row carries the
// reason the caller is shown, because a candidate id that git refuses must
// be refused before a branch is created, not after.
func TestValidateRefComponent(t *testing.T) {
	for _, tt := range refCorpus {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRefComponent(tt.in)
			if tt.ok {
				if err != nil {
					t.Fatalf("ValidateRefComponent(%q) error = %v, want nil", tt.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateRefComponent(%q) = nil, want an error: %s", tt.in, tt.why)
			}
			if !errors.Is(err, ErrInvalidRef) {
				t.Errorf("errors.Is(err, ErrInvalidRef) = false for %v", err)
			}
			if got := err.Error(); !strings.Contains(got, "git:") {
				t.Errorf("error %q does not name the package it came from", got)
			}
		})
	}
}

// stricterThanGit lists the corpus entries this package refuses although
// `git check-ref-format --branch belay/<s>` accepts them. Every one is a
// deliberate extra rule, and naming them here is what keeps the divergence a
// decision rather than a bug: a new divergence fails the test below.
var stricterThanGit = map[string]string{
	"a/b":                    "one component, so a slash would silently create a nested ref",
	"x.LOCK":                 "macOS is case-insensitive, so x.LOCK and x.lock are one file",
	"@":                      "git allows @ inside a path; belay refuses it everywhere, so no id is ever the string git reserves",
	"-lead":                  "a leading dash is read as an option by any git command given the name without a -- guard",
	"--lead":                 "a leading dash is read as an option by any git command given the name without a -- guard",
	strings.Repeat("a", 256): "longer than NAME_MAX, so the files ref backend cannot store it",
}

// TestValidateRefComponentAgreesWithGit runs the same corpus past the real
// `git check-ref-format`, in the one direction that can lose data: anything
// this package accepts, git must accept too. A rule this package is missing
// would let an id through to a command that creates a branch and fails
// halfway, which is how a run ends up with a branch and no worktree.
//
// The other direction is allowed to differ, and stricterThanGit says exactly
// where. The test pins that set, so adding a rule means saying why.
func TestValidateRefComponentAgreesWithGit(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	fired := make(map[string]bool, len(stricterThanGit))

	for _, tt := range refCorpus {
		t.Run(tt.name, func(t *testing.T) {
			if strings.ContainsRune(tt.in, 0) {
				t.Skip("a NUL cannot be passed to a process at all, so git has no verdict to compare")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, "git", "check-ref-format", "--branch", "belay/"+tt.in) //nolint:gosec // the corpus above is the argument, and it is a literal in this file
			cmd.Dir = dir
			gitOK := cmd.Run() == nil
			ourOK := ValidateRefComponent(tt.in) == nil

			switch {
			case ourOK && !gitOK:
				t.Errorf("ValidateRefComponent(%q) accepted a name `git check-ref-format --branch belay/%s` refuses", tt.in, tt.in)
			case !ourOK && gitOK:
				why, listed := stricterThanGit[tt.in]
				if !listed {
					t.Errorf("ValidateRefComponent(%q) refuses a name git accepts, and stricterThanGit does not say why", tt.in)
					return
				}
				fired[tt.in] = true
				t.Logf("stricter than git on purpose: %s", why)
			}
		})
	}

	// A row that never fires is a rule nobody is getting: either git
	// refuses the name too, or the corpus stopped covering it.
	for in := range stricterThanGit {
		if !fired[in] {
			t.Errorf("stricterThanGit claims %q is a divergence, but git refuses it too or the corpus never reached it", in)
		}
	}
}

// TestStricterThanGitHasNoStaleRows keeps the divergence list honest from the
// other side: a row for a name this package now accepts, or for a name the
// corpus no longer covers, would quietly stop being checked.
func TestStricterThanGitHasNoStaleRows(t *testing.T) {
	covered := make(map[string]bool, len(refCorpus))
	for _, tt := range refCorpus {
		covered[tt.in] = true
	}
	for in := range stricterThanGit {
		if !covered[in] {
			t.Errorf("stricterThanGit has a row for %q, which refCorpus does not cover", in)
		}
		if ValidateRefComponent(in) == nil {
			t.Errorf("stricterThanGit has a row for %q, which ValidateRefComponent now accepts", in)
		}
	}
}

// TestValidateBranch pins the rule for a whole branch name, which is what
// the methods that take one are given: every component must pass
// ValidateRefComponent, and the name must be a branch, not a ref.
func TestValidateBranch(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{name: "one component", in: "main", ok: true},
		{name: "the run branch", in: "belay/20260918T120000Z-1f2e3d4c5b6a", ok: true},
		{name: "a candidate branch", in: "belay/20260918T120000Z-1f2e3d4c5b6a-cand-01-a1", ok: true},
		{name: "several components", in: "team/feature/x", ok: true},

		{name: "empty", in: ""},
		{name: "a leading slash", in: "/x"},
		{name: "a trailing slash", in: "x/"},
		{name: "an empty component", in: "a//b"},
		{name: "a bad component", in: "belay/x.lock"},
		{name: "a leading dash", in: "-x"},
		{name: "HEAD, which git refuses as a branch", in: "HEAD"},
		{name: "a full ref instead of a branch", in: "refs/heads/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBranch(tt.in)
			switch {
			case tt.ok && err != nil:
				t.Errorf("validateBranch(%q) error = %v, want nil", tt.in, err)
			case !tt.ok && !errors.Is(err, ErrInvalidRef):
				t.Errorf("validateBranch(%q) error = %v, want one wrapping ErrInvalidRef", tt.in, err)
			}
		})
	}
}

// TestBranchTip pins the answer to "where is this branch?", including the
// two ways a name can be something other than a branch: absent, and shared
// with a tag. The tag must not answer for the branch.
func TestBranchTip(t *testing.T) {
	f := newFixture(t)
	top := f.repo("repo")
	r := f.open(top)
	first := f.git(top, "rev-parse", "HEAD")
	f.git(top, "tag", "dup", first)
	f.write(top, "a.txt", "second\n")
	f.git(top, "commit", "-qam", "second")
	second := f.git(top, "rev-parse", "HEAD")
	f.git(top, "branch", "dup", second)

	tests := []struct {
		name    string
		branch  string
		want    string
		wantOK  bool
		wantErr error
	}{
		{name: "an existing branch", branch: "main", want: second, wantOK: true},
		{name: "a branch sharing its name with a tag", branch: "dup", want: second, wantOK: true},
		{name: "no such branch", branch: "absent"},
		{name: "a name git would refuse", branch: "a..b", wantErr: ErrInvalidRef},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := r.BranchTip(t.Context(), tt.branch)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("BranchTip(%q) error = %v, want %v", tt.branch, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("BranchTip(%q) error = %v", tt.branch, err)
			}
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("BranchTip(%q) = %q, %t; want %q, %t", tt.branch, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestCreateBranchIsCreateOnly is the first half of the compare-and-swap
// pair. A branch that already exists may hold someone's work, so creating
// one by that name must fail, not move it: `git branch -f`, or update-ref
// without the all-zero old value, would silently repoint it.
func TestCreateBranchIsCreateOnly(t *testing.T) {
	f := newFixture(t)
	top := f.repo("repo")
	r := f.open(top)
	first := f.git(top, "rev-parse", "HEAD")
	f.write(top, "a.txt", "second\n")
	f.git(top, "commit", "-qam", "second")
	second := f.git(top, "rev-parse", "HEAD")

	if err := r.CreateBranch(t.Context(), "belay/run1", first); err != nil {
		t.Fatalf("CreateBranch() error = %v", err)
	}
	if got := f.git(top, "rev-parse", "refs/heads/belay/run1"); got != first {
		t.Fatalf("belay/run1 is at %s, want %s", got, first)
	}

	err := r.CreateBranch(t.Context(), "belay/run1", second)
	if !errors.Is(err, ErrBranchExists) {
		t.Fatalf("second CreateBranch() error = %v, want %v", err, ErrBranchExists)
	}
	if got := f.git(top, "rev-parse", "refs/heads/belay/run1"); got != first {
		t.Errorf("a refused CreateBranch moved belay/run1 from %s to %s", first, got)
	}
}

// TestCreateBranchResolvesItsCommit pins what "commit" may be: anything
// that names a commit, stored as the full object ID it names. Anything that
// does not name a commit is refused before a ref is written.
func TestCreateBranchResolvesItsCommit(t *testing.T) {
	f := newFixture(t)
	top := f.repo("repo")
	r := f.open(top)
	head := f.git(top, "rev-parse", "HEAD")
	tree := f.git(top, "rev-parse", "HEAD^{tree}")

	tests := []struct {
		name   string
		commit string
		ok     bool
	}{
		{name: "a full object id", commit: head, ok: true},
		{name: "an abbreviated object id", commit: head[:12], ok: true},
		{name: "a revision", commit: "HEAD", ok: true},
		{name: "a tree is not a commit", commit: tree},
		{name: "nothing by that name", commit: "no-such-revision"},
		{name: "an empty commit", commit: ""},
		{name: "something that looks like an option", commit: "--output=/tmp/x"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			branch := "resolve-" + strconv.Itoa(i)
			err := r.CreateBranch(t.Context(), branch, tt.commit)
			tip, exists, tipErr := r.BranchTip(t.Context(), branch)
			if tipErr != nil {
				t.Fatalf("BranchTip() error = %v", tipErr)
			}
			if !tt.ok {
				if err == nil || exists {
					t.Errorf("CreateBranch(%q) error = %v and the branch exists = %t; want an error and no branch", tt.commit, err, exists)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateBranch(%q) error = %v", tt.commit, err)
			}
			if tip != head {
				t.Errorf("the branch is at %q, want the full id %q", tip, head)
			}
		})
	}
}

// TestCreateBranchRefusesADirectoryFileConflict pins the ref-naming trap
// every candidate branch name is designed around. Refs are stored as paths,
// so refs/heads/belay/run1 and refs/heads/belay/run1/x cannot both exist,
// and neither can refs/heads/belay and refs/heads/belay/run1. A user with a
// branch named "belay" blocks every run branch; CreateBranch must say so,
// by name, before it writes anything.
func TestCreateBranchRefusesADirectoryFileConflict(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name     string
		existing string
		create   string
		wantErr  error
	}{
		{name: "a branch named after the parent directory", existing: "belay", create: "belay/run1", wantErr: ErrRefConflict},
		{name: "a branch named after a grandparent", existing: "team", create: "team/belay/run1", wantErr: ErrRefConflict},
		{name: "a branch nested under the new name", existing: "belay/run1/cand-01", create: "belay/run1", wantErr: ErrRefConflict},
		{name: "a sibling joined by a dash is no conflict", existing: "belay/run1", create: "belay/run1-cand-01"},
		{name: "a sibling in the same directory is no conflict", existing: "belay/run0", create: "belay/run1"},
		{name: "a longer name sharing a prefix is no conflict", existing: "belayx", create: "belay/run1"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			top := f.repo("df" + strconv.Itoa(i))
			r := f.open(top)
			head := f.git(top, "rev-parse", "HEAD")
			f.git(top, "branch", tt.existing)
			before := f.git(top, "for-each-ref")

			err := r.CreateBranch(t.Context(), tt.create, head)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("CreateBranch(%q) beside %q error = %v", tt.create, tt.existing, err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CreateBranch(%q) beside %q error = %v, want %v", tt.create, tt.existing, err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), "refs/heads/"+tt.existing) {
				t.Errorf("error %q does not name the branch in the way, refs/heads/%s", err, tt.existing)
			}
			if after := f.git(top, "for-each-ref"); after != before {
				t.Errorf("a refused CreateBranch changed the refs:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// TestDeleteBranchComparesAndSwaps is the second half of the pair. A branch
// is deleted only while it still points where the caller last saw it, so a
// branch someone moved in between survives.
//
// Three rows exist because of what update-ref really does, which the
// design assumed otherwise: `update-ref -d <ref> <zero-oid>` deletes the ref
// unconditionally, so an empty or zero expectation would have been a delete
// with no check at all; and update-ref deletes a branch checked out in a
// worktree, even the main one, leaving it on a branch with no commits,
// where `git branch -D` would have refused.
func TestDeleteBranchComparesAndSwaps(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name string
		// expect returns the commit the caller claims the branch is at,
		// given its real tip and another real commit.
		expect  func(tip, other string) string
		setup   func(top string)
		wantErr error
		// deleted says whether the branch must be gone afterwards.
		deleted bool
	}{
		{name: "the tip the caller saw", expect: func(tip, _ string) string { return tip }, deleted: true},
		{name: "a tip the branch has moved from", expect: func(_, other string) string { return other }, wantErr: ErrBranchMoved},
		{name: "the all-zero id, which update-ref reads as no check at all", expect: func(tip, _ string) string { return strings.Repeat("0", len(tip)) }, wantErr: ErrInvalidObjectID},
		{name: "an empty expectation", expect: func(string, string) string { return "" }, wantErr: ErrInvalidObjectID},
		{name: "an abbreviated id", expect: func(tip, _ string) string { return tip[:12] }, wantErr: ErrInvalidObjectID},
		{
			name:   "a branch checked out in a linked worktree",
			expect: func(tip, _ string) string { return tip },
			setup: func(top string) {
				f.git(top, "worktree", "add", "-q", filepath.Join(filepath.Dir(top), filepath.Base(top)+"-wt"), "doomed")
			},
			wantErr: ErrCheckedOut,
		},
		{
			name:    "the branch the main worktree is on",
			expect:  func(tip, _ string) string { return tip },
			setup:   func(top string) { f.git(top, "checkout", "-q", "doomed") },
			wantErr: ErrCheckedOut,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			top := f.repo("delete" + strconv.Itoa(i))
			r := f.open(top)
			other := f.git(top, "rev-parse", "HEAD")
			f.git(top, "checkout", "-q", "-b", "doomed")
			f.write(top, "a.txt", "on doomed\n")
			f.git(top, "commit", "-qam", "on doomed")
			tip := f.git(top, "rev-parse", "HEAD")
			f.git(top, "checkout", "-q", "main")
			if tt.setup != nil {
				tt.setup(top)
			}

			err := r.DeleteBranch(t.Context(), "doomed", tt.expect(tip, other))
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("DeleteBranch() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && err != nil {
				t.Errorf("DeleteBranch() error = %v", err)
			}
			got, exists, tipErr := r.BranchTip(t.Context(), "doomed")
			switch {
			case tipErr != nil:
				t.Fatalf("BranchTip() error = %v", tipErr)
			case tt.deleted && exists:
				t.Errorf("doomed still exists at %s", got)
			case !tt.deleted && !exists:
				t.Error("doomed was deleted, and a refused delete must leave it where it was")
			case !tt.deleted && got != tip:
				t.Errorf("doomed moved from %s to %s", tip, got)
			}
		})
	}

	t.Run("a branch that is already gone is not an error", func(t *testing.T) {
		top := f.repo("gone")
		r := f.open(top)
		tip := f.git(top, "rev-parse", "HEAD")
		if err := r.DeleteBranch(t.Context(), "never-existed", tip); err != nil {
			t.Errorf("DeleteBranch(<absent>) error = %v, want nil: it is gone, which is what was asked", err)
		}
	})
}

// TestContainedElsewhere answers the question a cleanup asks before it
// deletes a branch: would that lose a commit? It must never count the
// branch as containing itself, which for-each-ref --contains always lists.
func TestContainedElsewhere(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name  string
		setup func(top string)
		want  bool
	}{
		{
			name:  "a fresh branch at a commit main also has",
			setup: func(string) {},
			want:  true,
		},
		{
			name: "a branch holding a commit no other ref has",
			setup: func(top string) {
				commitOn(f, top, "cand", "unique\n")
			},
			want: false,
		},
		{
			name: "a branch whose work was merged into another branch",
			setup: func(top string) {
				commitOn(f, top, "cand", "merged\n")
				f.git(top, "checkout", "-q", "-b", "run")
				f.git(top, "merge", "-q", "--no-ff", "-m", "merge cand", "cand")
				f.git(top, "checkout", "-q", "main")
			},
			want: true,
		},
		{
			name: "a branch whose only other holder is a tag",
			setup: func(top string) {
				commitOn(f, top, "cand", "tagged\n")
				f.git(top, "tag", "kept", "cand")
			},
			want: true,
		},
		{
			name: "a branch whose only other holder is a remote-tracking ref",
			setup: func(top string) {
				commitOn(f, top, "cand", "pushed\n")
				f.git(top, "update-ref", "refs/remotes/origin/cand", "refs/heads/cand")
			},
			want: true,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			top := f.repo("contained" + strconv.Itoa(i))
			r := f.open(top)
			f.git(top, "branch", "cand")
			tt.setup(top)

			got, err := r.ContainedElsewhere(t.Context(), "cand")
			if err != nil {
				t.Fatalf("ContainedElsewhere() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ContainedElsewhere() = %t, want %t", got, tt.want)
			}
		})
	}

	t.Run("a branch that does not exist", func(t *testing.T) {
		r := f.open(f.repo("absent"))
		if _, err := r.ContainedElsewhere(t.Context(), "absent"); !errors.Is(err, ErrNoBranch) {
			t.Errorf("ContainedElsewhere(<absent>) error = %v, want %v", err, ErrNoBranch)
		}
	})
}

// commitOn adds one commit changing a.txt to body on branch, then goes back
// to main.
func commitOn(f *fixture, top, branch, body string) {
	f.t.Helper()
	f.git(top, "checkout", "-q", branch)
	f.write(top, "a.txt", body)
	f.git(top, "commit", "-qam", "on "+branch)
	f.git(top, "checkout", "-q", "main")
}

// TestCreateBranchLosesARaceCleanly scripts the window between the conflict
// check and the write: nothing is in the way when CreateBranch looks, and
// the branch exists by the time update-ref runs. The all-zero old value
// makes git refuse the write, and the refusal must still read as
// ErrBranchExists, not as an opaque exit status.
func TestCreateBranchLosesARaceCleanly(t *testing.T) {
	const oid = "0123456789abcdef0123456789abcdef01234567"
	rec := &recorder{reply: func(c bexec.Command) (bexec.Result, error) {
		switch subcommand(c) {
		case "rev-parse":
			return bexec.Result{Stdout: oid + "\n"}, nil
		case "for-each-ref":
			return bexec.Result{}, nil
		case "update-ref":
			return bexec.Result{ExitCode: 128}, &bexec.ExitError{Args: c.Args, Code: 128}
		}
		t.Fatalf("unexpected git %s", subcommand(c))
		return bexec.Result{}, nil
	}}
	r := &Repo{x: rec, top: t.TempDir()}

	err := r.CreateBranch(t.Context(), "belay/run1", oid)
	if !errors.Is(err, ErrBranchExists) {
		t.Fatalf("CreateBranch() error = %v, want %v", err, ErrBranchExists)
	}
	var update bexec.Command
	for _, c := range rec.commands() {
		if subcommand(c) == "update-ref" {
			update = c
		}
	}
	if n := len(update.Args); n < 2 || update.Args[n-1] != strings.Repeat("0", len(oid)) {
		t.Errorf("update-ref argv %q does not end with the all-zero old value that makes it create-only", update.Args)
	}
}
