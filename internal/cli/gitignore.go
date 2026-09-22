package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/dhaam-ai/belay/internal/planarchive"
)

// belayGitignore is what the first run in a repository writes to
// .belay/.gitignore. The "*" matches everything in the directory, the file
// itself included, which is the convention tools that keep a scratch
// directory inside a repository follow; the comment is for the person who
// finds the file and wonders what wrote it.
const belayGitignore = "# Written by belay. Everything under .belay is run state, which is not\n" +
	"# meant to be committed; a finished run's plan is moved out, beside the code.\n" +
	"*\n"

// ignoreRunState makes git ignore everything under the workspace's .belay
// directory, by writing .belay/.gitignore, unless the file is already there.
//
// A run directory holds the agent's raw prompts and replies, a whole second
// checkout in a branch run's worktree, and files that change on every step,
// and none of it is meant to be committed. Until now that relied on each
// repository ignoring .belay by hand; the one file belay does mean for a
// commit, a finished run's plan, is moved out of .belay first (ADR-0016).
//
// A file already there is somebody's decision and is never replaced, not
// even when it does not ignore everything: a person who commits a cassette
// from .belay, say, has written exactly the rule they want. It is written
// with planarchive.Place for the same reason the plan is: no-clobber and
// atomic, so a crash can never leave a half-written .gitignore that every
// later run would then take for a person's own and leave broken.
//
// The recording agent.mode "record" saves is under .belay by default, so it
// is ignored too; one meant for a commit is recorded with --cassette to a
// path outside .belay.
func ignoreRunState(workspace string) error {
	_, err := planarchive.Place(workspace, ".belay/.gitignore", []byte(belayGitignore))
	if err == nil || errors.Is(err, planarchive.ErrOccupied) {
		return nil
	}
	return fmt.Errorf("cannot write %s: %w", filepath.Join(workspace, ".belay", ".gitignore"), err)
}
