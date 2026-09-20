//go:build unix

package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Sentinel errors. Match them with errors.Is.
//
// Which of belay's own sentinels each one wraps is the point of it. A caller
// choosing between branch output and editing in place needs to tell "this
// directory can never be used this way" (belay.ErrUnsupported) from "this
// one can, after the user does something" (neither), and from "git itself
// is the problem" (belay.ErrToolchainMissing, see ErrTooOld).
var (
	// ErrNotARepository reports a directory that is not inside a git
	// repository. It wraps belay.ErrUnsupported: nothing about the directory
	// will change that, short of the user running git init.
	ErrNotARepository = fmt.Errorf("%w: git: not inside a git repository", belay.ErrUnsupported)

	// ErrBare reports a directory with no working tree: a bare repository,
	// or the inside of a .git directory. It wraps belay.ErrUnsupported,
	// because belay edits files and there are none here to edit.
	ErrBare = fmt.Errorf("%w: git: no working tree here (a bare repository, or inside a .git directory)", belay.ErrUnsupported)

	// ErrNoCommits reports a repository whose HEAD names a branch with no
	// commits yet. It deliberately does NOT wrap belay.ErrUnsupported: one
	// commit fixes it, and telling a user "never" for a one-command fix is
	// worse than telling them nothing.
	ErrNoCommits = errors.New("git: the repository has no commits yet")

	// ErrTruncated reports that git printed more than this package captures.
	// The answer is refused rather than read short: a status list cut off
	// part way is a clean-looking subset of a dirty tree.
	ErrTruncated = errors.New("git: output was truncated")
)

// Limits every git command runs under.
const (
	// commandTimeout bounds one git command. It is generous because
	// `worktree add` checks out every tracked file, and a large repository
	// on a slow disk takes minutes; a hung credential helper is what the
	// bound is really for, and GIT_TERMINAL_PROMPT=0 already stops the
	// common case of that.
	commandTimeout = 10 * time.Minute

	// maxOutput caps captured stdout and stderr, per stream. It is far above
	// internal/exec's 1 MiB default because `status -uall` in a repository
	// whose build output is not ignored lists every file of it, and a list
	// cut short is refused (ErrTruncated), not used.
	maxOutput = 16 << 20
)

// Execer runs one git command. *exec.Runner satisfies it in production;
// tests substitute one that records the argv or scripts the answer.
type Execer interface {
	// Run executes c and returns its redacted result. It follows
	// exec.Runner.Run: a Result is returned even on failure, and a non-zero
	// exit is an *exec.ExitError.
	Run(ctx context.Context, c bexec.Command) (bexec.Result, error)
}

// The production Execer is internal/exec's Runner; this assertion keeps the
// seam honest if either signature moves.
var _ Execer = (*bexec.Runner)(nil)

// Repo is one git repository, as seen from the directory it was opened in.
//
// Everything a Repo reports about itself is fixed at Open: the top of the
// working tree, the common git directory every worktree shares, where the
// opened directory sits inside the tree, the git version and whether the
// repository has submodules. Methods that act on a working tree take its
// directory explicitly, because a repository with linked worktrees has
// several and the Repo does not guess which one is meant.
//
// A Repo holds no mutable state and is safe for concurrent use, though git
// itself serialises writers on its own lock files: two concurrent calls that
// write the same ref or index will see one of them fail, not wait.
type Repo struct {
	x          Execer
	top        string
	commonDir  string
	prefix     string
	version    Version
	submodules bool
}

// Head is what HEAD points at in one working tree.
type Head struct {
	// Commit is the full object ID HEAD resolves to.
	Commit string
	// Branch is the branch HEAD is on, without "refs/heads/", or "" when
	// HEAD is detached.
	Branch string
}

// Open opens the repository containing dir and learns what every later call
// relies on: the git version, the working tree's top, the common git
// directory and the opened directory's prefix within the tree.
//
// Every path it reports is absolute and has symlinks resolved, because git
// resolves them: on macOS a repository under /var/folders is reported under
// /private/var/folders, and a caller comparing paths must compare the same
// kind. The common directory in particular is asked for with
// --path-format=absolute; without it git answers ".git", relative to a
// directory the caller did not choose.
//
// Open refuses, in this order: a git it cannot use (ErrTooOld or
// ErrUnreadableVersion, both toolchain problems), a directory that is not in
// a repository (ErrNotARepository) and a directory with no working tree
// (ErrBare). A repository with no commits opens: whether that matters is the
// caller's question, which Head answers with ErrNoCommits.
func Open(ctx context.Context, x Execer, dir string) (*Repo, error) {
	r := &Repo{x: x}

	// The version comes first because it is about the tool, not the
	// directory: every other answer depends on git understanding the flags
	// it is asked with.
	res, err := r.run(ctx, "", "version")
	if errors.Is(err, bexec.ErrToolchainMissing) {
		// internal/exec has its own sentinel for a binary that cannot be
		// started; belay's callers match belay's.
		return nil, fmt.Errorf("%w: git: %w", belay.ErrToolchainMissing, err)
	}
	if err != nil {
		return nil, fmt.Errorf("git: cannot run git: %w", err)
	}
	if r.version, err = ParseVersion(res.Stdout); err != nil {
		return nil, err
	}
	if !r.version.AtLeast(MinVersion) {
		return nil, fmt.Errorf("%w: found git %s, and belay needs %s or newer", ErrTooOld, r.version, MinVersion)
	}

	// The common directory and whether dir is inside a working tree come
	// from one command that works in a bare repository too, so a bare
	// repository is told apart from "not a repository" by an answer rather
	// than by reading an error message.
	res, err = r.run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir", "--is-inside-work-tree")
	if code, exited := exitCode(err); exited && code != 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotARepository, dir)
	}
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(res.Stdout, "\n"), "\n")
	if len(lines) != 2 {
		return nil, fmt.Errorf("git: rev-parse printed %d lines for 2 questions: %q", len(lines), res.Stdout)
	}
	r.commonDir = lines[0]
	if lines[1] != "true" {
		return nil, fmt.Errorf("%w: %s", ErrBare, dir)
	}

	// --show-prefix prints an empty line at the top of the tree, so the
	// line count is checked, not the content.
	res, err = r.run(ctx, dir, "rev-parse", "--path-format=absolute", "--show-toplevel", "--show-prefix")
	if err != nil {
		return nil, err
	}
	lines = strings.Split(strings.TrimSuffix(res.Stdout, "\n"), "\n")
	if len(lines) != 2 {
		return nil, fmt.Errorf("git: rev-parse printed %d lines for 2 questions: %q", len(lines), res.Stdout)
	}
	r.top, r.prefix = lines[0], lines[1]

	// .gitmodules is looked for in the index, not on disk: an index entry
	// is what `git submodule` acts on, and it is the same answer from every
	// directory in the tree.
	res, err = r.run(ctx, r.top, "ls-files", "-z", "--", ".gitmodules")
	if err != nil {
		return nil, err
	}
	r.submodules = res.Stdout != ""
	return r, nil
}

// Top returns the absolute path of the working tree's top directory: the
// repository root, or a linked worktree's own root when Open was given one.
func (r *Repo) Top() string { return r.top }

// CommonDir returns the absolute path of the git directory every worktree of
// this repository shares, which is where branches and the worktree list
// live. For the main working tree it is <top>/.git.
func (r *Repo) CommonDir() string { return r.commonDir }

// Prefix returns the directory Open was given, relative to Top, with a
// trailing slash, or "" when Open was given the top itself. It is how a
// caller re-creates "the same place" inside another worktree of the same
// repository.
func (r *Repo) Prefix() string { return r.prefix }

// HasSubmodules reports whether the index holds a .gitmodules file.
//
// A fresh worktree is a faithful copy of HEAD only when there are no
// submodules: `worktree add` does not initialise them, so a candidate would
// start from a tree missing whole directories the user's checkout has.
func (r *Repo) HasSubmodules() bool { return r.submodules }

// Version returns the git version Open found. It is never older than
// MinVersion.
func (r *Repo) Version() Version { return r.version }

// Head returns what HEAD points at in the working tree Open was given, and
// ErrNoCommits when the branch HEAD names has no commits yet.
func (r *Repo) Head(ctx context.Context) (Head, error) {
	return r.headAt(ctx, r.top)
}

// headAt returns what HEAD points at in the working tree containing dir.
// HEAD is per worktree, so a caller acting on a linked worktree must ask
// there, not at Top.
func (r *Repo) headAt(ctx context.Context, dir string) (Head, error) {
	// HEAD^{commit} rather than HEAD: --verify --quiet exits 1 without a
	// word when it does not resolve, and in a repository whose HEAD names
	// an unborn branch that is the only reason it can fail.
	res, err := r.run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if code, exited := exitCode(err); exited && code == 1 {
		return Head{}, ErrNoCommits
	}
	if err != nil {
		return Head{}, err
	}
	head := Head{Commit: strings.TrimSpace(res.Stdout)}

	// symbolic-ref --quiet exits 1 without a word when HEAD is detached.
	// The full ref is asked for and "refs/heads/" stripped here, because
	// --short abbreviates to whatever is unambiguous, and a tag with the
	// branch's name makes that "heads/main".
	res, err = r.run(ctx, dir, "symbolic-ref", "--quiet", "HEAD")
	if code, exited := exitCode(err); exited && code == 1 {
		return head, nil
	}
	if err != nil {
		return Head{}, err
	}
	head.Branch = strings.TrimPrefix(strings.TrimSpace(res.Stdout), "refs/heads/")
	return head, nil
}

// Status returns the paths that differ from HEAD anywhere in the working
// tree containing dir — modified, deleted, added or untracked — relative to
// the tree's top. An empty result means the tree is clean.
//
// Three choices make the answer mean "would a fresh checkout of HEAD look
// different?", which is the question every caller is really asking:
//
//   - The whole tree is asked about, not only dir. A workspace can be a
//     subdirectory of its worktree, and a dirty file beside it is still a
//     dirty tree.
//   - .belay at the top of the tree is left out. belay writes it into every
//     repository it runs in, so counting it would make every tree dirty the
//     moment a run started.
//   - Untracked directories are listed file by file (-uall), so a path in
//     the answer is always a file a caller can name.
//
// Status never writes the index (see TestStatusDoesNotWriteTheIndex).
// Renames are reported as their two paths, never as one record with two
// fields, so the list cannot be misaligned by a rename.
func (r *Repo) Status(ctx context.Context, dir string) ([]string, error) {
	res, err := r.run(ctx, dir, "status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=all",
		"--", ":/", ":(exclude,top).belay")
	if err != nil {
		return nil, err
	}
	return parseStatus(res.Stdout)
}

// parseStatus reads `status --porcelain=v1 -z --no-renames`: records of the
// form "XY PATH", each ended by a NUL. The path is taken verbatim; with -z
// git neither quotes nor escapes it, so a name with a space or a newline in
// it is read as the name it is.
func parseStatus(out string) ([]string, error) {
	var paths []string
	for record := range strings.SplitSeq(out, "\x00") {
		if record == "" {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("git: cannot read status record %q", record)
		}
		paths = append(paths, record[3:])
	}
	return paths, nil
}

// hardening goes in front of every git argv this package builds. The package
// doc says what each flag closes; TestEveryCommandIsHardened proves every
// command carries all of them.
var hardening = []string{
	"--no-optional-locks",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "commit.gpgsign=false",
	"-c", "tag.gpgsign=false",
	"-c", "gc.auto=0",
}

// command builds every git argv this package runs. It is the one place an
// argv is assembled, so the hardening cannot be left off a new command.
//
// The environment adds only two variables to internal/exec's deny-by-default
// base set, which already carries HOME, so git still reads the user's own
// identity for the commits it makes.
func command(dir string, args ...string) bexec.Command {
	argv := make([]string, 0, len(hardening)+len(args))
	argv = append(argv, hardening...)
	argv = append(argv, args...)
	return bexec.Command{
		Path: "git",
		Args: argv,
		Dir:  dir,
		ExtraEnv: map[string]string{
			"GIT_TERMINAL_PROMPT": "0",
			"LC_ALL":              "C",
		},
		Timeout:   commandTimeout,
		MaxOutput: maxOutput,
	}
}

// run builds one git command with command and runs it in dir.
func (r *Repo) run(ctx context.Context, dir string, args ...string) (bexec.Result, error) {
	return r.exec(ctx, command(dir, args...))
}

// exec runs c, which must have been built by command.
//
// It checks that the working directory exists first. internal/exec reports
// a failed chdir as a missing toolchain, because both surface as ENOENT from
// the same system call, and "git is not installed" is the wrong thing to
// tell someone whose worktree directory was deleted.
//
// A result whose output was truncated is refused with ErrTruncated: every
// caller parses the output, and a list cut short is a wrong answer that
// looks like a right one.
func (r *Repo) exec(ctx context.Context, c bexec.Command) (bexec.Result, error) {
	if c.Dir != "" {
		info, err := os.Stat(c.Dir)
		switch {
		case err != nil:
			return bexec.Result{ExitCode: -1}, fmt.Errorf("git: cannot run git in %s: %w", c.Dir, err)
		case !info.IsDir():
			return bexec.Result{ExitCode: -1}, fmt.Errorf("git: cannot run git in %s: not a directory", c.Dir)
		}
	}
	res, err := r.x.Run(ctx, c)
	if err == nil && res.Truncated {
		return res, fmt.Errorf("%w: git %s printed more than %d bytes", ErrTruncated, subcommandOf(c.Args), maxOutput)
	}
	return res, err
}

// exitCode reports the exit status of a git that ran and exited non-zero.
// ok is false when err is nil or when git never ran at all, which callers
// must not mistake for an answer.
func exitCode(err error) (code int, ok bool) {
	var ee *bexec.ExitError
	if errors.As(err, &ee) {
		return ee.Code, true
	}
	return 0, false
}

// subcommandOf returns the git subcommand in args, skipping the global
// options command puts in front of it, for use in error messages.
func subcommandOf(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			return args[i]
		}
	}
	return ""
}
