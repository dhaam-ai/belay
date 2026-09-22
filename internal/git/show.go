//go:build unix

package git

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
)

// FileAt returns the contents of path, relative to the top of the
// repository, as commit holds it.
//
// It exists for `belay plan` on a finished branch run. Such a run's plan is
// committed on the run's branch and, once the run's worktree has been
// removed, is nowhere else on disk until the person takes the branch.
//
// commit must be a full object id, the kind a run records when it promotes
// (state.Promotion.Commit). A branch name or an abbreviation is refused
// rather than resolved: a branch may have moved on since the run, and would
// show somebody's later version of the file as the run's. path must be
// slash-separated and relative, with no ".." in it, so the question can only
// be about a file inside the repository.
//
// It reads the object with `git cat-file blob`, which prints the bytes the
// commit holds, with none of the filters or conversions a checkout applies.
func (r *Repo) FileAt(ctx context.Context, commit, file string) ([]byte, error) {
	if !isObjectID(commit) {
		return nil, fmt.Errorf("%w: %q is not a full commit id", ErrInvalidRef, commit)
	}
	if file == "" || path.IsAbs(file) || slices.Contains(strings.Split(file, "/"), "..") {
		return nil, fmt.Errorf("git: %q is not a path inside the repository", file)
	}
	res, err := r.run(ctx, r.top, "cat-file", "blob", commit+":"+file)
	if err != nil {
		return nil, fmt.Errorf("git: read %s at %s: %w", file, commit, err)
	}
	return []byte(res.Stdout), nil
}
