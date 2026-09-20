//go:build unix

package worktree

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
	"github.com/dhaam-ai/belay/internal/git"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// agentPrompt is what a skipped test tells its reader to ask a coding agent,
// so the test runs next time instead of skipping again.
const agentPrompt = "Install git so it is on PATH and `git --version` prints 2.38 or newer, " +
	"then run `go test ./internal/isolate/worktree/... -v` and confirm the tests pass instead of skipping."

// requireGit skips the calling test unless a git new enough for internal/git
// is on PATH.
//
// Everything this package does happens inside git: a branch that cannot exist
// beside another, a `worktree add` that refuses an occupied directory, a
// `branch -d` that judges merge status against the wrong HEAD. A faked git
// would prove only that the fake agrees with the code. So when git is missing
// these tests skip, and the skip says how to make them run.
func requireGit(t *testing.T) {
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
	v, err := git.ParseVersion(string(out))
	if err != nil {
		t.Skipf("skipping: cannot read the git version from %q (%v). "+
			"Ask your coding agent: %q", strings.TrimSpace(string(out)), err, agentPrompt)
	}
	if !v.AtLeast(git.MinVersion) {
		t.Skipf("skipping: needs git %s or newer, found %s. "+
			"Upgrade it with `brew install git` or from https://git-scm.com/downloads, "+
			"or ask your coding agent: %q", git.MinVersion, v, agentPrompt)
	}
}

// gitConfig is the only git configuration a fixture repository sees. The
// developer's own ~/.gitconfig is kept out entirely: commit.gpgsign,
// init.defaultBranch and core.hooksPath are all things a real developer sets,
// and any of them would make these tests pass or fail for a reason that has
// nothing to do with the code.
const gitConfig = "[user]\n\tname = Belay Test\n\temail = belay-test@example.invalid\n" +
	"[init]\n\tdefaultBranch = main\n" +
	"[commit]\n\tgpgsign = false\n" +
	"[advice]\n\tdetachedHead = false\n"

// fixture builds real git repositories under a temporary directory and opens
// them through a hermetic exec.Runner.
type fixture struct {
	t *testing.T
	// root is t.TempDir() with symlinks resolved, because git reports
	// resolved paths and macOS's /var/folders temp directory is a symlink to
	// /private/var/folders.
	root string
	home string
	run  *bexec.Runner
}

// newFixture returns a fixture whose git sees only gitConfig.
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

// git runs one raw git command and returns its trimmed stdout. It
// deliberately does not go through the code under test: a fixture built by
// the thing being tested proves nothing when that thing is wrong.
func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := f.tryGit(dir, args...)
	if err != nil {
		f.t.Fatalf("git %s in %q: %v", strings.Join(args, " "), dir, err)
	}
	return out
}

// tryGit runs one raw git command and returns its trimmed stdout, or the
// error and stderr when it fails.
func (f *fixture) tryGit(dir string, args ...string) (string, error) {
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
		return strings.TrimRight(string(out), "\n"), &gitError{err: err, stderr: stderr.String()}
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// gitError carries a failed raw git command's stderr into the test's message.
type gitError struct {
	err    error
	stderr string
}

func (e *gitError) Error() string { return e.err.Error() + ": " + strings.TrimSpace(e.stderr) }

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

// open opens dir through the hermetic runner.
func (f *fixture) open(dir string) *git.Repo {
	f.t.Helper()
	return f.openWith(f.run, dir)
}

// openWith opens dir through x, for the tests that inject a failure.
func (f *fixture) openWith(x git.Execer, dir string) *git.Repo {
	f.t.Helper()
	r, err := git.Open(f.t.Context(), x, dir)
	if err != nil {
		f.t.Fatalf("git.Open(%q) error = %v", dir, err)
	}
	return r
}

// testNamespace is the run branch a candidate's branch hangs off in these
// tests, shaped like the real one: belay/<run-id>.
const testNamespace = "belay/20260918T120000Z-1f2e3d4c5b6a"

// newIsolator returns an Isolator over the repository at dir, rooted where a
// real run puts its candidates: .belay/candidates inside the repository. That
// placement is the production one and it is the interesting one — a worktree
// nested inside the repository is exactly what would make the source look
// dirty if .belay were not excluded.
func (f *fixture) newIsolator(dir string, opts ...Option) *Isolator {
	f.t.Helper()
	root := filepath.Join(dir, ".belay", "candidates")
	iso, err := New(f.open(dir), root, testNamespace, append([]Option{WithLogger(quiet())}, opts...)...)
	if err != nil {
		f.t.Fatalf("New(%q, %q) error = %v", root, testNamespace, err)
	}
	return iso
}

// mustCreate creates a workspace and fails the test if it cannot.
func (f *fixture) mustCreate(iso *Isolator, src, id string) belay.Workspace {
	f.t.Helper()
	ws, err := iso.Create(f.t.Context(), src, id)
	if err != nil {
		f.t.Fatalf("Create(%q, %q) error = %v", src, id, err)
	}
	return ws
}

// branches returns every branch in the repository at dir, sorted.
func (f *fixture) branches(dir string) []string {
	f.t.Helper()
	out := f.git(dir, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if out == "" {
		return nil
	}
	list := strings.Split(out, "\n")
	slices.Sort(list)
	return list
}

// worktreePaths returns every path `git worktree list` reports for the
// repository at dir.
func (f *fixture) worktreePaths(dir string) []string {
	f.t.Helper()
	var paths []string
	for _, line := range strings.Split(f.git(dir, "worktree", "list", "--porcelain"), "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths
}

// exists reports whether path is there at all, without following a symlink.
func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}

// quiet returns a logger that discards, so the package's info and warn lines
// stay out of test output while every call site is still exercised.
func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// capture is a slog handler that keeps every record, so a test can assert
// that a warning a person has to act on really names the command they need.
type capture struct {
	mu      sync.Mutex
	records []string
}

// logger returns a logger writing into c.
func (c *capture) logger() *slog.Logger { return slog.New(c) }

// Enabled implements slog.Handler.
func (c *capture) Enabled(context.Context, slog.Level) bool { return true }

// Handle implements slog.Handler.
func (c *capture) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, b.String())
	return nil
}

// WithAttrs implements slog.Handler.
func (c *capture) WithAttrs([]slog.Attr) slog.Handler { return c }

// WithGroup implements slog.Handler.
func (c *capture) WithGroup(string) slog.Handler { return c }

// find returns the first record holding want, or "".
func (c *capture) find(want string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if strings.Contains(r, want) {
			return r
		}
	}
	return ""
}

// messages returns every record captured so far.
func (c *capture) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.records)
}

// promoteBrancher is the optional interface internal/nodes/promote looks for
// on a run's isolator, restated here so that a change to either signature
// stops compiling. promote cannot import this package — the nodes stay free
// of the unix build tag internal/exec brings — so nothing but a pair of
// assertions holds the two ends of that seam together.
type promoteBrancher interface {
	Branch(id string) (string, error)
}

var _ promoteBrancher = (*Isolator)(nil)

// readFile reads a file a test built under t.TempDir(), in one place, so the
// gosec exemption for a path that is a variable is argued once rather than at
// every call site.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // G304: every path here is built by the fixture under t.TempDir().
}
