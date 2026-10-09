# ADR-0012: Git worktrees for isolation, and a run's result on its own branch

## Status
Accepted

## Context

Until this slice, a belay run edited the directory it was started in. The agent wrote into the user's checkout, the tests ran there, the quality gate judged it there, and belay stopped. Whatever it had done was sitting in the working tree as uncommitted changes, mixed in with anything the person had been doing themselves. The README's advice was to start from a clean `git status`, which nothing enforced and nobody could enforce: belay had no idea whether the tree was clean.

That is a bad deal in three separate ways.

**It is not reviewable.** An agent's change is exactly the kind of change that has to be read before it is kept. Reading it means diffing it against what was there before, and the only "before" was the person's own working tree, which the run had already overwritten.

**It is not recoverable.** A run that went wrong left its damage in the one tree the person was working in. `git checkout .` would take their own work with it.

**It cannot be proposed.** The thing everybody wants from an agent that writes code is a pull request. A pull request needs a branch and a commit, and belay made neither.

Meanwhile ADR-0004 had chosen directory copies for fanout isolation, against the lead engineer's recommendation of git worktrees, on the grounds that copies work on non-git targets and are trivially understandable. The cost it accepted was O(N × repo size) disk and a manual merge of the winner. With a run's own result now going onto a branch, that second cost stops being a trade and becomes a contradiction: a candidate in a directory copy has no branch, so there is nothing to merge, and the whole point of choosing a winner is to deliver it.

Slice 1b built `internal/git` and the worktree isolator against real git and found several things that shaped this decision, listed below with the rest.

## Decision

A run in a git repository delivers its result on its own branch. `output.mode` (`auto`, `branch`, `in_place`) chooses; `auto` is the default and picks `branch` whenever the workspace can take one. A branch run creates `belay/<run-id>` at the commit `HEAD` is on, checks it out in a git worktree under `.belay/runs/<run-id>/worktree`, points the run's `state.Layout` at that worktree so every node works there, and commits once — in the new `promote` node, after the quality gate has passed. The user's checkout is never written to. `belay run --pr` additionally pushes that branch and opens a **draft** pull request through `gh`.

Fanout candidates are git worktrees on sibling branches, `belay/<run-id>-<candidate-id>`, selected by `fanout.isolator` (`auto`, `worktree`, `dircopy`), with `auto` following the output mode. Directory copies remain, as the fallback for targets that are not git working trees.

This supersedes ADR-0004 for git targets. ADR-0004's non-git decision still stands.

### Worktrees over directory copies, with dircopy kept

A worktree shares the repository's object database, so making one costs a checkout of the tree rather than a copy of the tree *and* its history. It has a HEAD, so a candidate's work can be committed, compared and merged; a directory copy's cannot. `internal/isolate/dircopy` deliberately does not copy `.git`, which means a candidate there cannot run git at all — it cannot even see what it changed.

But `git worktree add` needs a git repository, and belay is not only for git repositories. A bare Python directory is a target belay should work on, and ADR-0004 was right about that. So dircopy stays, and it is chosen by `auto` for exactly the cases that need it. ADR-0004's claim that worktrees are "nearly free" is corrected here: they are cheap in *history*, not in *tree*. A worktree of a 500 MB checkout still writes 500 MB of files. What it does not do is duplicate the object database.

### The git 2.38 floor, and why

`Integrate` never runs `git merge` speculatively. It asks `git merge-tree --write-tree` whether a merge would conflict, and only then performs the merge. That subcommand's `--write-tree` mode — the one that reports conflicts without touching a working tree, index or HEAD — arrived in git 2.38.

The alternative was to run `git merge`, see what happened, and `git merge --abort` on conflict. That leaves conflict markers in files, `MERGE_HEAD` on disk, and a window in which a crash strands a working tree half merged. Slice 1b confirmed all three against real git. A version floor is a smaller cost than a tree belay can leave in a state it cannot describe, so `Open` refuses an older git with `ErrTooOld`, which wraps `belay.ErrToolchainMissing`, and `output.mode: auto` falls back to editing in place and says why.

### The gitlink binding check, and the main-worktree attack

A linked worktree's `.git` is not a directory. It is a one-line file — `gitdir: /path/to/repo/.git/worktrees/<name>` — sitting in a tree the agent has write access to. An agent that rewrote that line would point it at another repository's git directory, and belay's next `git -C <dir> commit` would faithfully commit into whatever repository the file named.

So `Commit` and `Integrate` verify, before they write anything, that the worktree they were handed is bound to the repository they were opened against, and refuse with `ErrForeignGitDir` otherwise.

Slice 1b found the sharper version of this while writing `Destroy`. The worktree isolator derives both the branch and the directory it removes from `Workspace.ID` plus values fixed when the isolator was constructed, and only *checks* `ws.Dir` against that derivation. It never trusts it. The reason is that a `belay.Workspace` round-trips through `state.json`, which is a file an agent can write to: a `Workspace` naming the repository's own main worktree, or a branch called `main`, would otherwise have driven `git worktree remove` and `git branch -D` at the user's checkout and their work. `RemoveWorktree` additionally refuses the main worktree outright — git lists it first, always — and refuses a worktree the user has locked rather than forcing past a lock that means "leave this alone".

### A merge left in progress is a pause, not an error

When the winner does not merge cleanly, promote returns a paused Result naming the conflicting files, the candidate's branch and the run's worktree, and touches nothing else. A person settles it there by hand and runs `belay resume`, and promote — which asks the repository what is already done rather than remembering anything — finds their merge and finishes.

The failure mode that forced a second rule: a person can stop half way through that merge, with the conflicts resolved and nothing committed. The repository then refuses to commit or merge over changes it did not make (`vcs.ErrDirtyTarget`; sweeping them into belay's commit is exactly what must not happen). Reported as an error that ends the run, it would have cost them everything the run had spent, one `git commit` from the finish. So that refusal is also a pause, and `vcs.ErrDirtyTarget` lives in the types package both sides share, because the node cannot import `internal/git` — that package builds only on unix, and `internal/graph`, which hands a node its repository, must stay free of build tags.

Debris from a crash is handled without asking a human: an interrupted merge whose result is still exactly what belay's own `merge-tree` predicted is aborted and redone. One that has been touched since is `ErrMergeInProgress`, which is somebody's work in progress and not belay's to discard.

### Why "-" and not "/" separates the candidate suffix

A candidate's branch is `belay/<run-id>-<candidate-id>`, not `belay/<run-id>/<candidate-id>`. Git stores loose refs as files, so `refs/heads/belay/<run-id>` is a *file*, and `refs/heads/belay/<run-id>/<candidate-id>` would need that same path to be a *directory*. Git refuses the second with a D/F conflict while the first exists. Since the run's own branch is created before any candidate's, the slash form is not a style choice that happens to look nicer — it cannot be created at all. Verified on git 2.54.

### ChangedFiles can come from git, which a reported list is not

The write node records what the agent *said* it changed. That is a claim, checked for presence but not for completeness, and a real agent re-invoked after a crash may summarise its work without repeating the list — which silently drops a file from the run's record.

In branch mode there is ground truth: `git diff --name-only` between the run's base commit and the branch's tip is what the run actually changed, whoever reported what. `promote` already measures its `Promotion.Files` that way. Taking the write node's `ChangedFiles` from git in branch mode is the same idea one node earlier, and it is left as a follow-up rather than done here only because it changes a node in-place runs share.

### Commits are the user's own identity

belay configures no author and no committer, so git uses whatever the person's own git is configured with, and adds a `Belay-Run: <run-id>` trailer (plus `Belay-Candidate` on a candidate's). No bot identity, no signing. A commit belay makes is a commit that person is accountable for, attributed to them, and the trailer is what connects it back to the run, its journal and its timeline. `promote` matches **both** trailers when it asks whether a winner was already merged, because candidate ids repeat between runs and an earlier run's merge would otherwise read as this run's.

### The pull request is a draft, and belay does not choose its base

`--pr` opens a draft: belay wrote the change, and whether it is ready for review is a person's to say. belay passes no `--base` to `gh pr create`, so gh uses the repository's default branch or the merge base configured for the branch — the same answer the person's own `gh pr create` would give. The alternative, passing the branch that was checked out when the run started, requires mapping a local branch onto a remote-tracking name that may not exist, and getting it wrong proposes a merge into the wrong branch.

## Consequences

### Positive
- **The user's checkout is never written to.** This is the claim the slice exists to make, and it is asserted end to end against a real repository, twice: through the CLI's own seams and through the whole node graph.
- **A run's result is reviewable and recoverable.** `git log -p <base>..belay/<run-id>` is the diff. Deleting the branch is the undo.
- **A run can be proposed.** One commit on one branch is what a pull request is made of.
- **A fanout winner can be delivered.** Merging a branch is a merge; copying a directory back was never going to be one.
- **Uncommitted work is safe from belay by construction**, not by advice nothing enforced. belay warns that such work is not *part of* a branch run, which is the surprise that remains.
- **The run's own worktree is not an Isolator workspace.** `Destroy` promises to release everything `Create` allocated, and the run branch is the deliverable. The CLI creates it with the same `internal/git` primitives, and nothing can release it.

### Negative
- **Disk.** A worktree is a full checkout of the tree. A branch run costs one; a fanout run costs N more. Only a completed run's worktree is removed automatically — a paused or failed one is where the person finishes, so it stays until they are done with it.
- **A second git version to support.** Everything from 2.38 works; below it, branch output is unavailable and `auto` falls back.
- **Uncommitted work is not in the run.** A branch starts at a commit. Somebody with an hour of uncommitted work expects belay to build on it, and it does not. belay warns, with a count, before spending anything.
- **A crash during run creation can orphan a branch.** `createRun` writes the manifest last, so a crash after the branch exists leaves one no run names. The manifest write failing is undone; a process killed between the two cannot be. A run directory without a manifest is already understood by every reader; an orphan `belay/*` branch is not, and `docs/troubleshooting.md` says how to find and remove them.
- **Two ways to run.** Branch and in-place both exist, both are supported, and every report, next-step and troubleshooting entry has to be right for both.
- **`gh` is a third-party dependency for `--pr`.** It is checked for before the run starts rather than after it has finished and spent its budget, and everything works without it.

### Follow-ups
- ~~Take the write node's `ChangedFiles` from `git diff` in branch mode, rather than from what the agent reported.~~ Done 2026-10-09: the write node records the agent's report together with what git says changed since the run's base, and the policy gate judges that; promote commits only what was recorded. See ADR-0014.
- Consider moving `Tip` and `Carries` onto `vcs.Repository` now that `internal/git` has both; `promote` consumes them through an unexported optional interface, which was a way of not breaking the `internal/git` lane mid-flight rather than a decision about the contract.
- Fanout is still on no route belay walks: nothing routes into it, and no candidate is executed (ADR-0019 is reserved for that). `join → promote` is wired and proven at node level and against real git; `review → promote` is live in every branch-mode run.

## Alternatives Considered

**Keep editing in place and commit afterwards.** belay would run as it always had, then `git add -A && git commit`. It commits whatever else was in the tree, cannot tell the person's work from the agent's, and leaves nothing to undo with — the damage is already in the branch they were on. It also breaks the quality gate: `golangci-lint --new-from-rev=HEAD` scopes findings to uncommitted changes, so committing earlier would hide the run's own changes from the gate meant to judge them.

**A clone per run instead of a worktree.** A clone is independent, which sounds safer, and costs a copy of the whole object database — the exact cost ADR-0004 was criticised for. It also puts the run's commits in a repository the user cannot see from theirs, so delivering the result means adding a remote and fetching. A worktree's branch is already in their repository.

**A temporary branch on the user's own checkout, switching back at the end.** No second tree, so no disk cost. It moves their HEAD, it fails outright if their tree is dirty, and a crash leaves them on a branch they never asked to be on. Ctrl-C at the wrong moment is not a thing that should require a person to know what `git switch -` does.

**`git stash` around the run.** Rejected on sight. A stash is a hiding place with no owner: a crash leaves the person's work in a stack they did not know was there, and `stash pop` can conflict.

**Passing `--base` to `gh pr create`.** Considered, and settled the other way. The run records the branch that was checked out when it started, but turning that into the remote-tracking name a pull request needs is guesswork when the branch has no upstream — and guessing wrong proposes a merge into the wrong branch.

## Revisit If
- Repositories large enough that a worktree checkout is itself a cost worth avoiding become common. `git worktree add --no-checkout` plus a sparse checkout is the next step, at the cost of tests that do not see the whole tree.
- A second version control system needs the same treatment. `vcs.Repository` moves to `pkg/belay` at that point, and not before: ADR-0005 records what exporting an interface with one implementation is worth.
- Hosting other than GitHub needs `--pr`. The step is one function behind one interface, and nothing about branch output depends on it.
- Submodules become worth supporting. They are excluded today because a new worktree does not populate them, so the run's tests would fail for a reason that has nothing to do with the change.
