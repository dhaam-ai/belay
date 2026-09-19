//go:build unix

package git

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
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
