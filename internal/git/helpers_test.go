//go:build unix

package git

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// agentPrompt is what a skipped test tells its reader to ask a coding agent,
// so the test runs next time instead of skipping again.
const agentPrompt = "Install git so it is on PATH and `git --version` prints 2.38 or newer, " +
	"then run `go test ./internal/git/... -v` and confirm the tests pass instead of skipping."

// requireGit skips the calling test unless a git new enough for this package
// is on PATH, and returns the version it found.
//
// This is a real-tool test package: a stub Execer can prove which argv was
// built, but only git itself can prove what git does with it, and every
// surprise this package is written against — a failed `worktree add -b`
// leaking its branch, `update-ref -d` with a zero old value deleting
// unconditionally, post-checkout firing on `worktree add` — was found by
// running the real binary. So when git is missing the tests skip rather than
// fake it, and the skip says how to make them run.
func requireGit(t *testing.T) Version {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	// Run git rather than only finding it: on macOS without the command line
	// tools, /usr/bin/git exists and fails.
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		t.Skipf("skipping: needs a working git on PATH (%v). "+
			"Install it with `xcode-select --install`, `brew install git` or from "+
			"https://git-scm.com/downloads, or ask your coding agent: %q", err, agentPrompt)
	}
	v, err := ParseVersion(string(out))
	if err != nil {
		t.Skipf("skipping: cannot read the git version from %q (%v). "+
			"Ask your coding agent: %q", strings.TrimSpace(string(out)), err, agentPrompt)
	}
	if !v.AtLeast(MinVersion) {
		t.Skipf("skipping: needs git %s or newer, found %s. "+
			"Upgrade it with `brew install git` or from https://git-scm.com/downloads, "+
			"or ask your coding agent: %q", MinVersion, v, agentPrompt)
	}
	return v
}
