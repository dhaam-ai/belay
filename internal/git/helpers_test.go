//go:build unix

package git

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bexec "github.com/dhaam-ai/belay/internal/exec"
)

// agentPrompt is what a skipped test tells its reader to ask a coding agent,
// so the test runs next time instead of skipping again.
const agentPrompt = "Install git so it is on PATH and `git --version` prints 2.38 or newer, " +
	"then run `go test ./internal/git/... -v` and confirm the tests pass instead of skipping."

// requireGit skips the calling test unless a git new enough for this package
// is on PATH, and returns the version it found.
//
// This is a real-tool test package: a stub Execer can prove which argv was
// built, but only git itself can prove what git does with it, and every
// surprise this package is written against — a failed `worktree add -b`
// leaking its branch, `update-ref -d` with a zero old value deleting
// unconditionally, post-checkout firing on `worktree add` — was found by
// running the real binary. So when git is missing the tests skip rather than
// fake it, and the skip says how to make them run.
func requireGit(t *testing.T) Version {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	// Run git rather than only finding it: on macOS without the command line
	// tools, /usr/bin/git exists and fails.
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		t.Skipf("skipping: needs a working git on PATH (%v). "+
			"Install it with `xcode-select --install`, `brew install git` or from "+
			"https://git-scm.com/downloads, or ask your coding agent: %q", err, agentPrompt)
	}
	v, err := ParseVersion(string(out))
	if err != nil {
		t.Skipf("skipping: cannot read the git version from %q (%v). "+
			"Ask your coding agent: %q", strings.TrimSpace(string(out)), err, agentPrompt)
	}
	if !v.AtLeast(MinVersion) {
		t.Skipf("skipping: needs git %s or newer, found %s. "+
			"Upgrade it with `brew install git` or from https://git-scm.com/downloads, "+
			"or ask your coding agent: %q", MinVersion, v, agentPrompt)
	}
	return v
}

// gitConfig is the only git configuration a fixture repository sees.
//
// The developer's own ~/.gitconfig is kept out entirely: commit.gpgsign,
// init.defaultBranch, core.hooksPath and commit.template are all things a
// real developer sets, and any of them would make these tests pass or fail
// for a reason that has nothing to do with the code.
const gitConfig = "[user]\n\tname = Belay Test\n\temail = belay-test@example.invalid\n" +
	"[init]\n\tdefaultBranch = main\n" +
	"[commit]\n\tgpgsign = false\n" +
	"[advice]\n\tdetachedHead = false\n"

// fixture builds real git repositories under a temporary directory and opens
// them through a hermetic exec.Runner.
type fixture struct {
	t *testing.T
	// root is t.TempDir() with symlinks resolved, because git reports
	// resolved paths and macOS's /var/folders temp directory is a symlink
	// to /private/var/folders.
	root string
	home string
	run  *bexec.Runner
}

// newFixture returns a fixture whose git sees only the configuration in
// gitConfig, skipping the test when git is unusable.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireGit(t)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()) error = %v", err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", home, err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitConfig), 0o600); err != nil {
		t.Fatalf("WriteFile(.gitconfig) error = %v", err)
	}
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.Getenv("TMPDIR")}
	return &fixture{
		t:    t,
		root: root,
		home: home,
		run: &bexec.Runner{
			Logger:  slog.New(slog.DiscardHandler),
			Environ: func() []string { return slices.Clone(env) },
			Timeout: time.Minute,
		},
	}
}

// git runs one raw git command while building a fixture and returns its
// trimmed stdout.
//
// It deliberately does not go through the code under test: a fixture built
// by the thing being tested proves nothing when that thing is wrong.
func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // every argument comes from a test literal
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + f.home,
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.Getenv("TMPDIR"),
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("git %s in %q: %v: %s", strings.Join(args, " "), dir, err, stderr.String())
	}
	return strings.TrimRight(string(out), "\n")
}

// dir makes a directory under the fixture root and returns its path.
func (f *fixture) dir(name string) string {
	f.t.Helper()
	path := filepath.Join(f.root, name)
	if err := os.MkdirAll(path, 0o750); err != nil {
		f.t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
	return path
}

// write puts body at path relative to dir, creating parents.
func (f *fixture) write(dir, path, body string) string {
	f.t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		f.t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		f.t.Fatalf("WriteFile(%q) error = %v", full, err)
	}
	return full
}

// emptyRepo initialises a repository with no commits.
func (f *fixture) emptyRepo(name string) string {
	f.t.Helper()
	dir := f.dir(name)
	f.git(dir, "init", "-q")
	return dir
}

// repo initialises a repository with one commit holding a.txt and sub/b.txt.
func (f *fixture) repo(name string) string {
	f.t.Helper()
	dir := f.emptyRepo(name)
	f.write(dir, "a.txt", "a\n")
	f.write(dir, "sub/b.txt", "b\n")
	f.git(dir, "add", "-A")
	f.git(dir, "commit", "-qm", "first")
	return dir
}

// bareRepo initialises a repository with no working tree.
func (f *fixture) bareRepo(name string) string {
	f.t.Helper()
	dir := f.dir(name)
	f.git(dir, "init", "-q", "--bare")
	return dir
}

// open opens dir through the hermetic runner, failing the test on error.
func (f *fixture) open(dir string) *Repo {
	f.t.Helper()
	r, err := Open(f.t.Context(), f.run, dir)
	if err != nil {
		f.t.Fatalf("Open(%q) error = %v", dir, err)
	}
	return r
}

// recorder is an Execer that records every command it is handed. It proves
// which argv a method builds, which no real-git test can see.
//
// With next set it forwards each command to a real runner, so the recorded
// argv are the ones a real call against a real repository built. Without it
// it answers from reply and starts no process.
type recorder struct {
	mu   sync.Mutex
	cmds []bexec.Command
	// next, when set, runs every command for real after recording it.
	next Execer
	// reply answers one command when next is nil. A nil reply means an
	// empty success.
	reply func(c bexec.Command) (bexec.Result, error)
}

func (rec *recorder) Run(ctx context.Context, c bexec.Command) (bexec.Result, error) {
	rec.mu.Lock()
	rec.cmds = append(rec.cmds, c)
	rec.mu.Unlock()
	switch {
	case rec.next != nil:
		return rec.next.Run(ctx, c)
	case rec.reply != nil:
		return rec.reply(c)
	default:
		return bexec.Result{Args: append([]string{c.Path}, c.Args...)}, nil
	}
}

// commands returns what has been recorded so far.
func (rec *recorder) commands() []bexec.Command {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return slices.Clone(rec.cmds)
}

// reset forgets what has been recorded, so the next commands() answers for
// one call only.
func (rec *recorder) reset() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.cmds = nil
}

// subcommand returns the git subcommand c runs.
func subcommand(c bexec.Command) string { return subcommandOf(c.Args) }
