# ADR-0004: Plain Directory Copies for Fanout Isolation, Behind an Isolator Interface

## Status
Accepted

## Context
When fanout spawns N candidates, each must work in isolation so they don't interfere with each other's file mutations (writes to go.mod, package.json, source files, etc.). How do we isolate?

Two primary approaches were debated:

1. **Git worktrees**: Create a separate working tree per candidate (git worktree add). Native, lightweight, uses Git's own primitives. Merging the winning candidate's code back is a branch merge, not a manual operation.

2. **Directory copies**: Copy the entire repository directory for each candidate. Simple, works on non-Git targets (bare directories, svn checkouts). No external tool required.

The lead engineer recommended git worktrees for elegance and merge automation. The team chose directory copies anyway, accepting the costs.

## Decision
Fanout isolation uses plain recursive directory copies (cp -r) per candidate, behind an `Isolator` interface. Each candidate gets its own writable directory tree. When isolation is complete, results are merged manually (code from winning candidate is copied back to the original tree).

## Consequences

### Positive
- **Works on non-Git targets**: A user with a bare Python directory (not a git repo) can still run fanout. Isolator is not bound to Git.
- **Trivially understandable**: No Git plumbing, no worktree subcommands. `cp -r src dst` is self-explanatory.
- **No external tool dependency**: Directory copy is built into the OS (cp, xcopy, or Go's filepath.Walk). No `git` binary requirement.
- **Interface allows future swaps**: Isolator is exported; containers or other schemes can drop in later.

### Negative
- **Slow for large repos**: Copying a 500 MB repository N times means O(N × 500 MB) of I/O. Worktrees share the .git object database, so they are nearly free.
- **Disk space**: If a user runs 5 fanout candidates, they need 5 copies of the repo on disk. A 1 GB repo means 5 GB overhead (plus the original). For embedded/IoT use cases, this is painful.
- **Manual merge of winning code**: After candidates finish, the winning one's changes must be copied back into the original tree. Worktrees would make this a branch merge (git merge), automatically handling conflicts and partial changes. Directory copy means: find changed files, copy them, manually check for conflicts.
- **No delta-sync**: If a candidate modifies one 50 KB file in a 500 MB repo, we still copy the whole 500 MB to archive it. Worktrees would only store the delta.

### Follow-ups
- Implement Isolator interface in `pkg/isolator/` as an abstract type so future implementations (worktrees, containers) can coexist.
- Document the merge-back algorithm in `pkg/graph/merge.go`: iterate winning candidate's tree, find files changed relative to the original, copy them back.
- Add a cost note in the README: "Fanout uses O(N × repo_size) disk. For large repos and high N, consider using a container runtime or running fewer candidates."
- Implement a `belay cleanup` command to delete orphaned isolation directories after runs complete (not left behind).

## Alternatives Considered
- **Git worktrees**: Recommended by the lead engineer for elegance and merge automation. Not chosen because: team wanted to keep Isolator agnostic to version control, and directory copy is simpler to understand and debug in its first implementation. Trade-off accepted knowingly.

## Revisit If
- Users report disk pressure from large fanout runs (> 3 candidates on > 500 MB repos).
- A second run with the same repo shows measurable slowdown from repeated copies (profile first; caching might help).
- A user requests automatic merge of winning code without manual inspection (merging is currently manual).
- A container runtime isolator is contributed and benchmarks show >10x faster isolation.

## References
- Challenge #134 phase 4: "Fanout Execution"
- pkg/isolator/isolator.go (T46: Isolator interface)
- pkg/graph/merge.go (T46: merge winning candidate back)
