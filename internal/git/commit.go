//go:build unix

package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dhaam-ai/belay/internal/vcs"
)

// Sentinels for the operations that write commits. Match them with errors.Is.
var (
	// ErrForeignGitDir reports a working tree whose git directory is not
	// this repository's, which means something rewrote the .git file in it.
	// Nothing is committed: see verifyWorktree for what is checked and why.
	ErrForeignGitDir = errors.New("git: the working tree's git directory is not this repository's")

	// ErrDirtyTarget reports that Integrate was asked to merge into a
	// working tree with uncommitted changes in it. Merging would mix them
	// into the merge commit, and a conflict would leave them unrecoverable.
	//
	// It wraps vcs.ErrDirtyTarget because the node that has to act on it
	// cannot import this package: this one builds only on unix, and the
	// graph that hands a node its repository must stay free of build tags.
	// The node pauses the run on it rather than ending one a person is one
	// `git commit` away from finishing; see vcs.ErrDirtyTarget.
	ErrDirtyTarget = fmt.Errorf("%w: git: the working tree has uncommitted changes", vcs.ErrDirtyTarget)

	// ErrMergeInProgress reports a merge left half-done in the working tree
	// that is not the one belay was making, or one whose result has been
	// changed since. belay finishes its own interrupted merge; anything
	// else is a person's work in progress, and the error says how to finish
	// or abandon it.
	ErrMergeInProgress = errors.New("git: a merge is in progress in the working tree")

	// ErrInvalidMessage reports a commit message git would mangle: no
	// subject, or a newline where one would break the trailer block that
	// carries the run's ID.
	ErrInvalidMessage = errors.New("git: invalid commit message")
)

// Repo is the git implementation of the repository the promote node uses.
// This assertion is the seam: internal/vcs has no build tag and no git in
// it, so nodes and their tests can name a repository without starting one.
var _ vcs.Repository = (*Repo)(nil)

// Commit records every change in the working tree holding dir as one commit,
// and returns its ID.
//
// With nothing to commit it makes no commit and returns the commit the tree
// already has. That is what makes a promotion converge: the dispatcher
// re-runs a node whose start has no matching finish, so a run interrupted
// after the commit but before the journal record comes back here, finds its
// own work already committed, and reports the same ID rather than making a
// second, empty commit.
//
// What it records is the whole working tree, not the directory it was
// handed: a workspace can be a subdirectory of its worktree, and a change
// beside it is still the run's work. Everything `git status` shows goes in,
// with no curation — that tree is what the tests and the quality gate ran
// against — except .belay at the top, which is belay's own scratch space.
//
// The message is written exactly as vcs.Message.Text renders it. A message
// git would mangle is refused (ErrInvalidMessage) rather than silently
// reshaped, because the run's ID travels in the trailer block.
func (r *Repo) Commit(ctx context.Context, dir string, msg vcs.Message) (string, error) {
	if err := validateMessage(msg); err != nil {
		return "", err
	}
	if err := r.verifyWorktree(ctx, dir); err != nil {
		return "", err
	}
	if _, err := r.run(ctx, dir, "add", "--all", "--", ":/", ":(exclude,top).belay"); err != nil {
		return "", err
	}

	// diff --cached exits 1 when something is staged and 0 when nothing is.
	// Asking is what keeps a re-run from committing again; --allow-empty
	// would add a commit every time.
	_, err := r.run(ctx, dir, "diff", "--cached", "--quiet")
	code, exited := exitCode(err)
	switch {
	case err == nil:
		head, err := r.headAt(ctx, dir)
		return head.Commit, err
	case !exited || code != 1:
		return "", err
	}

	// --cleanup=whitespace rather than the default, which follows
	// commit.cleanup: a user configuring "strip" would have every line of a
	// body starting with # silently dropped.
	c := command(dir, "commit", "--quiet", "--no-verify", "--cleanup=whitespace", "--file=-")
	c.Stdin = strings.NewReader(msg.Text())
	if _, err := r.exec(ctx, c); err != nil {
		return "", err
	}
	head, err := r.headAt(ctx, dir)
	return head.Commit, err
}

// Integrate merges the work at from into the branch checked out at dir, as
// one merge commit carrying msg.
//
// It asks the repository what is already true rather than remembering what
// it did, so every outcome converges on a re-run:
//
//   - When dir's branch already contains from — because a previous attempt
//     merged it, or because a human did — nothing is committed and the
//     branch's current tip comes back.
//   - A merge that would conflict is not attempted. `merge-tree --write-tree`
//     answers "would this conflict?" without touching the working tree, so a
//     conflict returns an Integration naming the files, with the tree exactly
//     as it was and no MERGE_HEAD to clean up. That is a verdict for the
//     caller to pause on, not an error: a run whose candidate was chosen and
//     paid for must not end because two people edited one file.
//   - The merge itself is --no-ff, so a promotion is always a merge commit
//     even when it could fast-forward. The merge is the record of what
//     belay did.
//
// A working tree with uncommitted changes is refused (ErrDirtyTarget): they
// would be swept into the merge commit. The strategy is named explicitly so
// that the merge git performs is the one merge-tree predicted.
func (r *Repo) Integrate(ctx context.Context, dir, from string, msg vcs.Message) (vcs.Integration, error) {
	if err := validateMessage(msg); err != nil {
		return vcs.Integration{}, err
	}
	if err := r.verifyWorktree(ctx, dir); err != nil {
		return vcs.Integration{}, err
	}
	source, err := r.resolveCommit(ctx, from)
	if err != nil {
		return vcs.Integration{}, err
	}
	if err := r.resolveInterruptedMerge(ctx, dir, source); err != nil {
		return vcs.Integration{}, err
	}

	head, err := r.headAt(ctx, dir)
	if err != nil {
		return vcs.Integration{}, err
	}
	// Already contained: nothing to do, whoever did it.
	_, err = r.run(ctx, dir, "merge-base", "--is-ancestor", source, head.Commit)
	code, exited := exitCode(err)
	switch {
	case err == nil:
		return vcs.Integration{Tip: head.Commit}, nil
	case !exited || code != 1:
		return vcs.Integration{}, err
	}

	dirty, err := r.Status(ctx, dir)
	if err != nil {
		return vcs.Integration{}, err
	}
	if len(dirty) > 0 {
		return vcs.Integration{}, fmt.Errorf("%w: %s has %d uncommitted change(s), starting with %s",
			ErrDirtyTarget, dir, len(dirty), dirty[0])
	}

	_, conflicts, err := r.mergeTree(ctx, dir, head.Commit, source)
	if err != nil {
		return vcs.Integration{}, err
	}
	if len(conflicts) > 0 {
		return vcs.Integration{Tip: head.Commit, Conflicts: conflicts}, nil
	}

	_, err = r.run(ctx, dir, "merge", "--no-ff", "--no-log", "--no-edit", "--no-verify",
		"--strategy=ort", "-m", msg.Text(), source)
	if err != nil {
		// merge-tree said this merges cleanly, so a failure here is
		// something else: no identity configured, a full disk. Leave the
		// tree as it was rather than half-merged.
		return vcs.Integration{}, errors.Join(err, r.abortMerge(ctx, dir))
	}
	head, err = r.headAt(ctx, dir)
	return vcs.Integration{Tip: head.Commit}, err
}

// Changed returns the files that differ between the commits from and to, as
// paths from the repository's top.
//
// Rename detection is off: git would report a renamed file only under its
// new name, and both paths differ between the two commits. A caller listing
// what a run changed must see the file that is no longer there.
func (r *Repo) Changed(ctx context.Context, from, to string) ([]string, error) {
	fromOID, err := r.resolveCommit(ctx, from)
	if err != nil {
		return nil, err
	}
	toOID, err := r.resolveCommit(ctx, to)
	if err != nil {
		return nil, err
	}
	res, err := r.run(ctx, r.top, "diff", "--name-only", "--no-renames", "-z", fromOID, toOID)
	if err != nil {
		return nil, err
	}
	return splitNUL(res.Stdout), nil
}

// ErrInsideBelay reports a directory inside the .belay directory of the
// working tree around it: a copy belay made, which git sees as part of the
// repository it sits in and excludes.
var ErrInsideBelay = errors.New("git: the directory is inside the working tree's .belay directory")

// Changes returns every file in the working tree at dir, and below it, that
// differs from the commit base, as paths relative to dir: whatever is
// committed on top of base since, staged, edited or deleted, and every new
// file git does not ignore. A renamed file is listed under both its names.
// belay's own .belay directory is left out, at the top of the working tree
// and inside dir.
//
// It is the ground truth a run's change is judged by, and exists because the
// alternative is what the agent says it changed. An agent can misreport, by
// accident or on purpose, and a policy gate that judges the report is a gate
// the agent can walk past: it edited infra/main.tf and belay.yaml, reported
// only src/app.py, and both edits were committed. Asking git closes that,
// and it also answers in git's true names, so a report spelled
// "Infra/main.tf" on a case-insensitive disk cannot dodge a glob written
// "infra/**".
//
// What it lists is what Commit would record, measured against the run's
// base rather than HEAD: an agent that commits its own work cannot take the
// work out of the answer. A file git ignores is not listed, because a commit
// never carries it either.
//
// base must be a full object id. A branch name or an abbreviation is refused
// (ErrInvalidObjectID) rather than resolved, because a branch can have moved
// since the run recorded it, and every file between the two would vanish
// from the answer.
//
// dir is proven to be a working tree of this repository first, as every
// call handed a working tree is (see verifyWorktree): its .git file is the
// agent's to rewrite, and an answer read from another repository would be a
// false "nothing changed". So is a directory inside the .belay directory of
// the working tree around it (ErrInsideBelay), such as a copy belay made for
// a queued goal: git finds the person's repository a few directories up and
// excludes every file in the copy, which would also read as nothing changed;
// a .belay at any depth counts, since a run started in a subdirectory keeps
// its own there.
//
// The listings are -z output, so a name with a space or a newline in it is
// read as the name it is.
func (r *Repo) Changes(ctx context.Context, dir, base string) ([]string, error) {
	if !isObjectID(base) {
		return nil, fmt.Errorf("%w: a run's change is measured from a full commit id, got %q", ErrInvalidObjectID, base)
	}
	if err := r.verifyWorktree(ctx, dir); err != nil {
		return nil, err
	}
	res, err := r.run(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, err
	}
	if insideBelay(strings.TrimSpace(res.Stdout)) {
		return nil, fmt.Errorf("%w: %s", ErrInsideBelay, dir)
	}

	// "." scopes both listings to dir, and the two exclusions cover .belay
	// wherever a run keeps it: at the top for a run at the top, inside dir
	// for a run started in a subdirectory of the tree.
	scope := []string{"--", ".", ":(exclude,top).belay", ":(exclude).belay"}
	tracked, err := r.run(ctx, dir, append([]string{"diff", "--name-only", "-z", "--no-renames", "--relative", base}, scope...)...)
	if err != nil {
		return nil, err
	}
	untracked, err := r.run(ctx, dir, append([]string{"ls-files", "-z", "--others", "--exclude-standard"}, scope...)...)
	if err != nil {
		return nil, err
	}
	paths := append(splitNUL(tracked.Stdout), splitNUL(untracked.Stdout)...)
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

// insideBelay reports whether prefix, a directory's path within its working
// tree as rev-parse --show-prefix prints it, passes through a .belay
// directory at any depth: a run started in a subdirectory keeps its .belay
// there. Segments are compared without regard to case, because on the file
// systems macOS uses by default .Belay is the same directory.
func insideBelay(prefix string) bool {
	return slices.ContainsFunc(strings.Split(prefix, "/"), func(seg string) bool {
		return strings.EqualFold(seg, ".belay")
	})
}

// mergeTree reports the tree merging theirs into ours would produce and the
// files that conflict, without touching any working tree. It is how a
// conflict becomes a verdict instead of a mess someone has to clean up.
func (r *Repo) mergeTree(ctx context.Context, dir, ours, theirs string) (tree string, conflicts []string, err error) {
	res, err := r.run(ctx, dir, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", ours, theirs)
	code, exited := exitCode(err)
	switch {
	case err == nil, exited && code == 1:
		// 0 is a clean merge and 1 is a conflicted one; both print the
		// tree first. Anything else is a failure to answer at all.
	default:
		return "", nil, err
	}
	fields := splitNUL(res.Stdout)
	if len(fields) == 0 {
		return "", nil, fmt.Errorf("git: merge-tree printed no tree for %s into %s", theirs, ours)
	}
	conflicts = slices.Clone(fields[1:])
	slices.Sort(conflicts)
	return fields[0], slices.Compact(conflicts), nil
}

// resolveInterruptedMerge deals with a MERGE_HEAD found before a merge
// starts.
//
// belay's own merge can be interrupted between git writing MERGE_HEAD and
// git making the commit — a crash, a Ctrl-C — and the re-run has to get
// past it. But `git merge --abort` throws away whatever is in the tree, and
// the conflict pause deliberately invites a human to merge by hand in this
// very directory. Aborting theirs would delete a conflict resolution nobody
// can get back.
//
// So it only undoes a merge that is provably the one belay was making and
// that nobody has touched since: the same source, no unresolved paths, no
// edits of its own, and an index that still holds exactly what merging
// would produce. Anything else is refused with ErrMergeInProgress, which
// names the two commands that finish or abandon it.
func (r *Repo) resolveInterruptedMerge(ctx context.Context, dir, source string) error {
	res, err := r.run(ctx, dir, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	if code, exited := exitCode(err); exited && code == 1 {
		return nil // no merge in progress, which is the usual case
	}
	if err != nil {
		return err
	}

	refuse := fmt.Errorf("%w: %s is merging; finish it with `git commit` or abandon it with `git merge --abort`, then run belay again",
		ErrMergeInProgress, dir)
	if strings.TrimSpace(res.Stdout) != source {
		return refuse
	}
	ours, err := r.run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return err
	}
	tree, conflicts, err := r.mergeTree(ctx, dir, strings.TrimSpace(ours.Stdout), source)
	if err != nil {
		return err
	}
	// belay never starts a merge that conflicts, so a conflicted one is
	// someone else's, and its resolution is theirs.
	if len(conflicts) > 0 {
		return refuse
	}
	untouched, err := r.mergeIsUntouched(ctx, dir, tree)
	if err != nil {
		return err
	}
	if !untouched {
		return refuse
	}
	return r.abortMerge(ctx, dir)
}

// mergeIsUntouched reports whether the merge in progress in dir still holds
// exactly the result of merging, with nothing staged, edited or left over on
// top of it.
func (r *Repo) mergeIsUntouched(ctx context.Context, dir, tree string) (bool, error) {
	// diff-index compares the index with the merge's own tree, and
	// diff-files the working tree with the index. Both exit 1 on any
	// difference, which is the whole question.
	for _, args := range [][]string{
		{"diff-index", "--cached", "--quiet", tree, "--"},
		{"diff-files", "--quiet"},
	} {
		_, err := r.run(ctx, dir, args...)
		if code, exited := exitCode(err); exited && code == 1 {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	// An untracked file is not part of the merge, but committing it would
	// make it part of the commit, so it counts as touched.
	res, err := r.run(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z",
		"--", ":/", ":(exclude,top).belay")
	if err != nil {
		return false, err
	}
	return len(splitNUL(res.Stdout)) == 0, nil
}

// abortMerge undoes a merge in progress, returning the working tree to the
// commit it was on.
func (r *Repo) abortMerge(ctx context.Context, dir string) error {
	if _, err := r.run(ctx, dir, "rev-parse", "--verify", "--quiet", "MERGE_HEAD"); err != nil {
		if code, exited := exitCode(err); exited && code == 1 {
			return nil
		}
		return err
	}
	_, err := r.run(ctx, dir, "merge", "--abort")
	return err
}

// verifyWorktree refuses to write in a directory whose git directory is not
// this repository's own for that directory.
//
// This is the check that stands between a coding agent and the user's
// repository. The agent has write access to its workspace, and a worktree's
// .git is an ordinary file naming the git directory to use, so the agent can
// point it anywhere. Three redirects matter, and all three are refused here:
//
//   - At another repository, where the run's work would be committed.
//   - At this repository's main worktree. The common directory still
//     matches, so comparing that alone is not enough: git would stage the
//     agent's files into the user's own index and commit them onto the
//     branch the user has checked out. Verified against real git.
//   - At a git directory the agent builds inside its own workspace, whose
//     commondir names the real repository so the first check passes, while
//     its HEAD and index are the agent's to choose.
//
// So three things must hold: the common directory is this repository's; the
// git directory is either the common directory itself, for the main
// worktree, or an entry git keeps under <common>/worktrees, which is outside
// any workspace; and that entry names this directory back.
func (r *Repo) verifyWorktree(ctx context.Context, dir string) error {
	res, err := r.run(ctx, dir, "rev-parse", "--path-format=absolute",
		"--git-common-dir", "--absolute-git-dir", "--show-toplevel")
	if code, exited := exitCode(err); exited && code != 0 {
		return fmt.Errorf("%w: %s is not inside a git repository", ErrForeignGitDir, dir)
	}
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSuffix(res.Stdout, "\n"), "\n")
	if len(lines) != 3 {
		return fmt.Errorf("git: rev-parse printed %d lines for 3 questions: %q", len(lines), res.Stdout)
	}
	common, gitDir, top := lines[0], lines[1], lines[2]

	if common != r.commonDir {
		return fmt.Errorf("%w: %s uses the repository at %s, not %s", ErrForeignGitDir, dir, common, r.commonDir)
	}
	if gitDir == common {
		// dir claims to be the main working tree, so it must be the one
		// git lists first.
		list, err := r.Worktrees(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 || list[0].Path != top {
			return fmt.Errorf("%w: %s is not the main working tree of %s", ErrForeignGitDir, dir, common)
		}
		return nil
	}
	if filepath.Dir(gitDir) != filepath.Join(common, "worktrees") {
		return fmt.Errorf("%w: %s uses %s, which is not a worktree git registered", ErrForeignGitDir, dir, gitDir)
	}
	// The registration must name this directory back. Reading it is the
	// only way to tell a worktree's own entry from another worktree's.
	body, err := os.ReadFile(filepath.Join(gitDir, "gitdir")) //nolint:gosec // gitDir was just proven to be inside the repository's own worktrees directory.
	if err != nil {
		return fmt.Errorf("git: cannot read the worktree registration for %s: %w", dir, err)
	}
	if registered := resolvePath(strings.TrimSpace(string(body))); registered != filepath.Join(top, ".git") {
		return fmt.Errorf("%w: %s uses the git directory registered for %s", ErrForeignGitDir, dir, filepath.Dir(registered))
	}
	return nil
}

// validateMessage refuses a message git would mangle.
//
// vcs.Message.Text formats and does not validate, and says that refusing a
// bad message is the job of whoever commits it. This is that. A message with
// no subject is a commit nobody can identify in a log, and a newline inside a
// trailer splits the trailer block that carries the run's ID, which is how
// the promotion is traced back to the run that made it.
func validateMessage(msg vcs.Message) error {
	subject := strings.TrimSpace(msg.Subject)
	switch {
	case subject == "":
		return fmt.Errorf("%w: the subject is empty", ErrInvalidMessage)
	case strings.ContainsAny(subject, "\n\r"):
		return fmt.Errorf("%w: the subject holds a line break: %q", ErrInvalidMessage, msg.Subject)
	}
	for _, t := range msg.Trailers {
		switch {
		case strings.TrimSpace(t.Key) == "":
			return fmt.Errorf("%w: a trailer has no key", ErrInvalidMessage)
		case strings.ContainsAny(t.Key, "\n\r:"), strings.ContainsAny(t.Value, "\n\r"):
			return fmt.Errorf("%w: trailer %q: %q would not be read as one trailer", ErrInvalidMessage, t.Key, t.Value)
		}
	}
	return nil
}

// splitNUL returns the non-empty NUL-terminated fields of a -z listing.
func splitNUL(out string) []string {
	var fields []string
	for field := range strings.SplitSeq(out, "\x00") {
		if field != "" {
			fields = append(fields, field)
		}
	}
	return fields
}
