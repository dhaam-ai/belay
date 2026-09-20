//go:build unix

package git

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Remote answers "where would this branch go?" without pushing anything, so
// belay can refuse a pull request it could not open before it creates a
// branch, rather than after.
//
// It follows the same order git does when it pushes: what the branch itself
// is configured with, then remote.pushDefault, then the one remote there is.
// It deliberately does not guess between two remotes belay has no basis to
// choose from — pushing someone's work to the wrong fork is not a mistake
// belay gets to make on their behalf.
func TestRemote(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name    string
		setup   func(top string)
		branch  string
		want    string
		wantErr error
	}{
		{
			name:    "no remote at all",
			setup:   func(string) {},
			branch:  "main",
			wantErr: ErrNoRemote,
		},
		{
			name:   "the only remote there is",
			setup:  func(top string) { f.git(top, "remote", "add", "upstream", "https://example.invalid/x.git") },
			branch: "main", want: "upstream",
		},
		{
			name: "the branch's own remote wins",
			setup: func(top string) {
				f.git(top, "remote", "add", "origin", "https://example.invalid/a.git")
				f.git(top, "remote", "add", "fork", "https://example.invalid/b.git")
				f.git(top, "config", "branch.main.remote", "fork")
			},
			branch: "main", want: "fork",
		},
		{
			name: "remote.pushDefault next",
			setup: func(top string) {
				f.git(top, "remote", "add", "origin", "https://example.invalid/a.git")
				f.git(top, "remote", "add", "fork", "https://example.invalid/b.git")
				f.git(top, "config", "remote.pushDefault", "fork")
			},
			branch: "main", want: "fork",
		},
		{
			name: "two remotes and nothing saying which",
			setup: func(top string) {
				f.git(top, "remote", "add", "origin", "https://example.invalid/a.git")
				f.git(top, "remote", "add", "fork", "https://example.invalid/b.git")
			},
			branch: "main", want: "origin",
		},
		{
			name: "two remotes, neither called origin",
			setup: func(top string) {
				f.git(top, "remote", "add", "one", "https://example.invalid/a.git")
				f.git(top, "remote", "add", "two", "https://example.invalid/b.git")
			},
			branch: "main", wantErr: ErrAmbiguousRemote,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			top := f.repo(strings.ReplaceAll(tt.name, " ", "-"))
			tt.setup(top)
			repo := f.open(top)

			got, err := repo.Remote(t.Context(), tt.branch)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Remote() = %q, %v, want an error wrapping %v", got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Remote() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Remote() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Push sends one branch to one remote, under exactly that name, and never
// forces.
//
// The refspec is written out in full because push.default, a push refspec
// configured on the remote, and a branch's own upstream can each redirect a
// bare `git push <remote> <branch>` somewhere else. Forcing is what would
// discard somebody else's commits, and belay pushing a run's branch has no
// business doing that -- a rejected push is a thing for a person to look at.
func TestPushSendsExactlyTheBranchAndNeverForces(t *testing.T) {
	f := newFixture(t)
	top := f.repo("source")
	bare := f.bareRepo("remote")
	f.git(top, "remote", "add", "origin", bare)
	f.git(top, "branch", "belay/run-1")
	repo := f.open(top)

	rec := &recorder{next: f.run}
	recorded := f.open(top)
	recorded.x = rec

	if err := recorded.Push(t.Context(), "origin", "belay/run-1"); err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if got := f.git(bare, "rev-parse", "belay/run-1"); got != f.git(top, "rev-parse", "belay/run-1") {
		t.Errorf("the remote's belay/run-1 is %q, want the local branch's commit", got)
	}

	var argv []string
	for _, c := range rec.commands() {
		if subcommandOf(c.Args) == "push" {
			argv = c.Args
		}
	}
	if argv == nil {
		t.Fatal("Push ran no git push")
	}
	for _, forbidden := range []string{"--force", "-f", "--force-with-lease", "--force-if-includes"} {
		if slices.Contains(argv, forbidden) {
			t.Errorf("push argv %v contains %q; belay never overwrites a remote branch", argv, forbidden)
		}
	}
	for _, arg := range argv {
		if strings.HasPrefix(arg, "+") {
			t.Errorf("push argv %v has a forcing refspec %q", argv, arg)
		}
	}
	if !slices.Contains(argv, "refs/heads/belay/run-1:refs/heads/belay/run-1") {
		t.Errorf("push argv %v does not spell the refspec out; push.default could redirect it", argv)
	}

	// Pushing the same branch again is not an error: it is already there.
	if err := repo.Push(t.Context(), "origin", "belay/run-1"); err != nil {
		t.Errorf("Push() a second time = %v, want nil: the branch is already where it was asked to go", err)
	}
}

