//go:build unix

package git

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Sentinels for publishing a branch. Match them with errors.Is.
var (
	// ErrNoRemote reports a repository with no remote to push to.
	ErrNoRemote = errors.New("git: the repository has no remote")

	// ErrAmbiguousRemote reports several remotes with nothing saying which
	// one a branch belongs to. It is deliberately not a guess: pushing
	// somebody's work to the wrong fork is not a mistake belay gets to make
	// on their behalf.
	ErrAmbiguousRemote = errors.New("git: several remotes, and nothing says which one to push to")
)

// Remote returns the remote the branch called name would be pushed to,
// without pushing anything.
//
// It exists so that belay can refuse a pull request it could not open before
// it creates a branch rather than after, and it follows the order git itself
// follows: the branch's own branch.<name>.remote, then remote.pushDefault,
// then the single remote when there is only one, then "origin".
//
// Several remotes with nothing saying which, and no "origin" among them, is
// ErrAmbiguousRemote rather than a guess. Pushing somebody's work to the
// wrong fork is not a mistake belay gets to make on their behalf, and the
// caller can tell them exactly which setting names the one they meant.
//
// name is not required to exist: a branch belay is about to create has no
// configuration of its own, and the answer is then the repository's.
func (r *Repo) Remote(ctx context.Context, name string) (string, error) {
	if name != "" {
		if err := validateBranch(name); err != nil {
			return "", err
		}
		if value, ok, err := r.configValue(ctx, "branch."+name+".remote"); err != nil {
			return "", err
		} else if ok {
			return value, nil
		}
	}
	if value, ok, err := r.configValue(ctx, "remote.pushDefault"); err != nil {
		return "", err
	} else if ok {
		return value, nil
	}

	list, err := r.remotes(ctx)
	switch {
	case err != nil:
		return "", err
	case len(list) == 0:
		return "", fmt.Errorf("%w: add one with `git remote add origin <url>`", ErrNoRemote)
	case len(list) == 1:
		return list[0], nil
	case slices.Contains(list, "origin"):
		// git's own fallback, and the name every clone gets.
		return "origin", nil
	}
	return "", fmt.Errorf("%w: %s; set remote.pushDefault, or branch.%s.remote, to the one you mean",
		ErrAmbiguousRemote, strings.Join(list, ", "), name)
}

// Push sends branch to remote under exactly that name, and never forces.
//
// The refspec is spelled out in full because push.default, a push refspec
// configured on the remote and a branch's own upstream can each redirect a
// bare `git push <remote> <branch>` somewhere else — including, with
// push.default set to "matching", to branches nobody asked about.
//
// Nothing here forces. Forcing is what discards somebody else's commits, and
// a run's branch has no business doing that: a rejected push is a thing for a
// person to look at, not for belay to overrule. Pushing a branch the remote
// already has at the same commit is not an error; git reports it as
// up to date, which is the state the caller asked for.
//
// --atomic makes the push all-or-nothing, and no upstream is configured:
// belay has not been asked to change what the person's own `git push` does.
func (r *Repo) Push(ctx context.Context, remote, branch string) error {
	if strings.TrimSpace(remote) == "" {
		return fmt.Errorf("%w: no remote given", ErrNoRemote)
	}
	if strings.HasPrefix(remote, "-") {
		// A remote name is a positional argument; one starting with "-"
		// would be read as a flag.
		return fmt.Errorf("git: remote %q is not a usable name", remote)
	}
	if err := validateBranch(branch); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	_, err := r.run(ctx, r.top, "push", "--atomic", "--no-verify", "--", remote, ref+":"+ref)
	return err
}

// remotes lists the repository's remotes, in git's own order.
func (r *Repo) remotes(ctx context.Context) ([]string, error) {
	res, err := r.run(ctx, r.top, "remote")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// configValue reads one configuration key, reporting whether it is set.
// git exits 1 for a key that is not set, which is an answer, not a failure.
func (r *Repo) configValue(ctx context.Context, key string) (string, bool, error) {
	res, err := r.run(ctx, r.top, "config", "--get", key)
	if code, exited := exitCode(err); err != nil {
		if exited && code == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	value := strings.TrimSpace(res.Stdout)
	return value, value != "", nil
}
