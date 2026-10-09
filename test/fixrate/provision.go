//go:build fixrate

package fixrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// cellPatch returns the change in the workspace ws since the commit base, as
// a patch `git apply` takes -- the form the official harness applies a
// prediction in. index is a scratch index file, which cellPatch creates and
// removes: the workspace's own index is never read or written, so taking the
// patch cannot change what a resumed arm finds.
//
// The patch is the whole working tree against base: edits, deletions, and
// new files the agent never added to an index, but nothing the repository's
// own ignore rules exclude, so a compiled byproduct of a test run is not part
// of anyone's answer. It is taken with git's global and system configuration
// switched off, so a user's global excludes cannot drop an agent's new file
// and no user setting can change the patch's shape; file modes are ignored,
// because copying a repository out of a container is allowed to change them
// and an agent cannot. Binary changes are carried with --binary, which git
// apply understands.
//
// An empty string means the arm changed nothing.
func cellPatch(ctx context.Context, ws, base, index string) (patch string, err error) {
	defer func() {
		if rmErr := os.Remove(index); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
	}()
	git := func(args ...string) (string, error) {
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "git", append([]string{ //nolint:gosec // fixed binary; arguments are this function's own
			"-c", "core.fileMode=false", "-c", "core.autocrlf=false", "-c", "core.safecrlf=false",
		}, args...)...)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("fixrate: git %s in %s: %w: %s", args[0], ws, err, strings.TrimSpace(stderr.String()))
		}
		return stdout.String(), nil
	}
	if _, err := git("read-tree", base); err != nil {
		return "", err
	}
	if _, err := git("add", "--all", "--", "."); err != nil {
		return "", err
	}
	return git("diff", "--cached", "--binary", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
		"--src-prefix=a/", "--dst-prefix=b/", base, "--", ".")
}
