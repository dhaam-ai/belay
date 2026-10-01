//go:build unix

package linter

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dhaam-ai/belay/internal/exec"
)

// This file holds what every adapter that scopes its findings to a run's
// change asks git first: whether git can say what the change is at all.
//
// The question is the same whichever tool is linting. A run's change is the
// difference between the working tree and HEAD, so scoping needs a HEAD that
// holds the linted directory, and a directory whose new files git does not
// ignore. Where either is missing, scoping by HEAD would hide every finding
// the run adds, so the adapter must count every finding instead and say so.

// Bounds for the git commands the scope check runs. Each is quick, and the
// longest output is one directory level of names; the cap only has to leave
// that output non-empty.
const (
	gitCheckTimeout         = 30 * time.Second
	gitCheckMaxOutput int64 = 64 << 10
)

// unscopable returns why git cannot scope findings in dir to the run's
// change, or "" when it can.
//
// probe is a file name the check asks git's ignore rules about, standing in
// for a file the run adds to dir. It should carry the extension of the files
// the adapter lints, and a name no repository is expected to ignore, so that
// only the rules covering the directory decide.
func (b base) unscopable(ctx context.Context, dir, probe string) string {
	// ls-tree lists dir's entries in HEAD, relative to dir. It prints
	// nothing for a directory the repository ignores or has not committed,
	// and fails with no repository or no commit.
	res, err := b.git(ctx, dir, "ls-tree", "--name-only", "HEAD")
	switch {
	case err != nil:
		return err.Error()
	case strings.TrimSpace(res.Stdout) == "":
		return "HEAD holds nothing in this directory: git ignores it, or it is not committed yet"
	}

	// HEAD can hold a file in a directory whose other files git ignores,
	// such as ws/.gitkeep under "ws/*". git never reports an ignored file
	// as changed, so ask whether a new file here would be ignored.
	// check-ignore exits 0 when it would and 1 when it would not;
	// --no-index answers from the ignore rules alone.
	_, err = b.git(ctx, dir, "check-ignore", "--quiet", "--no-index", "--", probe)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return "git ignores new files in this directory"
	case errors.As(err, &exitErr) && exitErr.Code == 1:
		return ""
	default:
		return err.Error()
	}
}

// git runs one git command in dir, with the base environment only.
func (b base) git(ctx context.Context, dir string, args ...string) (exec.Result, error) {
	return b.runner.Run(ctx, exec.Command{
		Path:      "git",
		Args:      args,
		Dir:       dir,
		Timeout:   min(gitCheckTimeout, b.timeout),
		MaxOutput: gitCheckMaxOutput,
	})
}
