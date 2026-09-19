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
