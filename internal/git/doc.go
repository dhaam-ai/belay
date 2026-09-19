//go:build unix

// Package git is the only place in belay that starts the git binary.
//
// It exists so that the run branch, the run's worktree, the candidate
// worktrees and the promotion commit are all built by one piece of code with
// one set of rules, rather than by whichever node happened to need git next.
// Everything belay asks of version control goes through a [Repo], and a [Repo]
// is the implementation of [vcs.Repository] that the promote node consumes.
//
// # Why the git CLI and not a Go library
//
// go-git is the obvious alternative and it is deliberately not used. The
// repositories belay runs against are the user's own working repositories,
// with whatever their git has put there: partial clones, sparse checkouts,
// LFS pointers, alternates, worktrees, a commit-graph, reftable or files
// backends. git reads all of that because git wrote it; a reimplementation
// reads the subset its authors reached. Being wrong about a user's repository
// is not a degraded feature here, it is a lost commit. The cost accepted in
// exchange is that belay depends on a binary it does not ship, must parse that
// binary's output, and cannot run on Windows.
//
// # One command constructor, hardened
//
// Every git this package starts has its argv built in one function, which
// puts the same global options in front of every subcommand. Each closes a
// hole that is only closed if it is on every command, which is why there is
// one constructor and a test (TestEveryCommandIsHardened) that records the
// argv of every method against a real repository:
//
//   - -c core.hooksPath=/dev/null. A repository's hooks are code, and belay
//     never agreed to run them on the user's behalf. post-checkout fires on
//     `git worktree add` and pre-commit on `git commit`; both were observed
//     firing without this flag. The quality gate stands in for pre-commit,
//     and the user's CI checks the pull request.
//   - --no-optional-locks. A plain `git status` refreshes the stat data in
//     .git/index and writes it back, so `belay run --dry-run` would modify
//     the repository it promised only to inspect.
//   - -c commit.gpgsign=false and -c tag.gpgsign=false. internal/exec passes
//     a child no environment it did not ask for, so no gpg-agent socket and
//     no SSH signing state reach git, and a repository configured to sign
//     would hang on a passphrase prompt nobody can see.
//   - -c core.fsmonitor=false. The setting can name a command, which git
//     would start on every status.
//   - -c gc.auto=0. An automatic garbage collection belay happened to
//     trigger would run for minutes inside a node's timeout.
//
// The environment gets two variables on top of internal/exec's
// deny-by-default base set: GIT_TERMINAL_PROMPT=0, because a credential
// prompt in a run nobody is watching is a hang, and LC_ALL=C, so that the
// only git output a human reads — an error message — reads the way git's
// documentation shows it. HOME is already in the base set, so git reads the
// user's own identity: commits belay makes are the user's commits.
//
// What the flags do not stop is configuration that runs code during the
// operations themselves: a clean or smudge filter (git-lfs), an
// includeIf, core.sshCommand for a push. That is no new trust: by the time
// belay commits, the test node has already run the repository's own code.
//
// # Parsing
//
// Output is read only in forms git promises not to change: -z and
// --porcelain listings, and exit codes. Message text never decides anything
// here, because it is translated and it changes between releases. A bare
// repository is told apart from "not a repository" by asking
// --is-inside-work-tree in a command that works in both, not by reading
// "fatal: this operation must be run in a work tree"; a detached HEAD is
// symbolic-ref's exit status 1, not the word "detached".
//
// Output that was cut off at the capture limit is refused, not read
// ([ErrTruncated]): every listing here answers a yes-or-no question, and the
// first half of a list of dirty files looks like a shorter list.
//
// # Platform
//
// This package builds on unix only, because it runs every command through
// internal/exec, which needs Setpgid to kill a process group. internal/vcs
// exists precisely so that internal/graph and the nodes can name a repository
// without inheriting that build tag.
//
// # The minimum git version
//
// [MinVersion] is 2.38, the release that added `merge-tree --write-tree`.
// That command is how [Repo.Integrate] answers "would this merge conflict?"
// without touching a working tree: with it, a conflicting promotion is a
// verdict the node reports, and the user's files are exactly as they were.
// Without it the only way to find out is to run the merge, which leaves
// MERGE_HEAD and conflict markers in the tree for a human to clean up. belay
// refuses to do that, so an older git is a missing toolchain ([ErrTooOld])
// and the run falls back to editing in place.
package git
