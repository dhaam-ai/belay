//go:build unix

package lintgate

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/linter"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Python sources for the ruff tests. Each undefined name is pyflakes' F821,
// which internal/linter maps to Critical, so any one of them fails the
// default gate.
const (
	// ruffConfig selects pyflakes and nothing else, so a ruff release that
	// changes its default rule set cannot change what these tests see.
	ruffConfig = "[lint]\nselect = [\"F\"]\n"

	// pyDebt is committed code with a finding on line 5.
	pyDebt = "\"\"\"Committed with a finding in it.\"\"\"\n\n\ndef old():\n    return undefined_old\n"

	// pyEdit is appended to pyDebt, and adds a finding on line 9.
	pyEdit = "\n\ndef edited():\n    return undefined_edited\n"

	// pyAdded is a new file with a finding on line 5.
	pyAdded = "\"\"\"New code in a new file.\"\"\"\n\n\ndef added():\n    return undefined_added\n"

	// pyClean is a new file with nothing to report.
	pyClean = "\"\"\"Nothing to report.\"\"\"\n\n\ndef clean():\n    return 1\n"

	// pyBroken is committed code that does not parse, the way a
	// repository's test data sometimes deliberately does.
	pyBroken = "def broken(:\n    pass\n"
)

// The expected findings for the run most tests make: pyEdit appended to a
// committed pyDebt, plus pyAdded as a new file.
var (
	pyChanged = []string{"added.py:5 F821 critical", "debt.py:9 F821 critical"}
	pyAll     = []string{"added.py:5 F821 critical", "debt.py:5 F821 critical", "debt.py:9 F821 critical"}
)

// ruffAgentPrompt is what a skipped test tells its reader to ask a coding
// agent, so the test runs next time.
const ruffAgentPrompt = "Install ruff and git so both are on PATH, " +
	"then run `go test ./test/lintgate -run TestRuff -v` and confirm the tests pass instead of skipping."

// TestRuffScopesToChangedLines runs linter.Ruff, with its real internal/exec
// runner, the real ruff and the real git, over a project whose last commit
// already holds a finding. Only the lines a run changed may count toward the
// gate, the way golangci-lint's already do.
//
// A stub runner cannot stand in here: what this checks is belay reading git's
// own account of the change and matching it to ruff's own paths.
func TestRuffScopesToChangedLines(t *testing.T) {
	requireRuff(t)

	tests := []struct {
		name      string
		committed map[string]string
		change    map[string]string
		wantGate  belay.GateStatus
		want      []string
	}{
		{
			name:      "committed findings do not count and new ones do",
			committed: map[string]string{"debt.py": pyDebt},
			change:    map[string]string{"debt.py": pyDebt + pyEdit, "added.py": pyAdded},
			wantGate:  belay.GateFail,
			want:      pyChanged,
		},
		{
			name:      "a change with no findings passes despite committed ones",
			committed: map[string]string{"debt.py": pyDebt},
			change:    map[string]string{"clean.py": pyClean},
			wantGate:  belay.GatePass,
			want:      []string{},
		},
		{
			name:      "committed code that does not parse does not fail a change elsewhere",
			committed: map[string]string{"debt.py": pyDebt, "broken.py": pyBroken},
			change:    map[string]string{"clean.py": pyClean},
			wantGate:  belay.GatePass,
			want:      []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writePyProject(t, dir, tt.committed)
			commitAll(t, dir)
			writeFiles(t, dir, tt.change)

			check(t, lintRuff(t, dir), tt.wantGate, tt.want, false)
		})
	}

	t.Run("a subdirectory of a repository is scoped", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "svc")
		writePyProject(t, dir, map[string]string{"debt.py": pyDebt})
		commitAll(t, root)
		writeFiles(t, dir, map[string]string{"debt.py": pyDebt + pyEdit, "added.py": pyAdded})

		check(t, lintRuff(t, dir), belay.GateFail, pyChanged, false)
	})

	t.Run("a change already added to the index is still the run's", func(t *testing.T) {
		dir := t.TempDir()
		writePyProject(t, dir, map[string]string{"debt.py": pyDebt})
		commitAll(t, dir)
		writeFiles(t, dir, map[string]string{"debt.py": pyDebt + pyEdit, "added.py": pyAdded})
		runGit(t, dir, "add", "--all")

		check(t, lintRuff(t, dir), belay.GateFail, pyChanged, false)
	})

	// The last two cases pin documented limits of scoping by line, so the
	// documentation stays true.
	t.Run("a new file in a directory git ignores does not count", func(t *testing.T) {
		dir := t.TempDir()
		writePyProject(t, dir, map[string]string{"debt.py": pyDebt, ".gitignore": "gen/\n"})
		commitAll(t, dir)
		writeFiles(t, filepath.Join(dir, "gen"), map[string]string{"added.py": pyAdded})

		check(t, lintRuff(t, dir), belay.GatePass, []string{}, false)
	})

	t.Run("a renamed file counts in full", func(t *testing.T) {
		dir := t.TempDir()
		writePyProject(t, dir, map[string]string{"debt.py": pyDebt})
		commitAll(t, dir)
		runGit(t, dir, "mv", "debt.py", "moved.py")

		check(t, lintRuff(t, dir), belay.GateFail, []string{"moved.py:5 F821 critical"}, false)
	})
}

// TestRuffCountsEverythingGitCannotScope covers the directories where no
// commit holds the code. Every finding must count, and belay must say that it
// did not scope.
func TestRuffCountsEverythingGitCannotScope(t *testing.T) {
	requireRuff(t)
	base := map[string]string{"debt.py": pyDebt}
	change := map[string]string{"debt.py": pyDebt + pyEdit, "added.py": pyAdded}

	t.Run("outside a git repository", func(t *testing.T) {
		dir := t.TempDir()
		writePyProject(t, dir, base)
		writeFiles(t, dir, change)

		check(t, lintRuff(t, dir), belay.GateFail, pyAll, true)
	})

	t.Run("in a repository with no commits", func(t *testing.T) {
		dir := t.TempDir()
		writePyProject(t, dir, base)
		runGit(t, dir, "init", "--quiet")
		writeFiles(t, dir, change)

		check(t, lintRuff(t, dir), belay.GateFail, pyAll, true)
	})

	t.Run("in a directory the enclosing repository ignores", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{".gitignore": "ws/\n"})
		commitAll(t, root)
		dir := filepath.Join(root, "ws")
		writePyProject(t, dir, base)
		writeFiles(t, dir, change)

		check(t, lintRuff(t, dir), belay.GateFail, pyAll, true)
	})
}

// lintRuff runs linter.Ruff over dir, isolated from the machine running the
// test: ruff's cache is a fresh directory, and HOME is one too, so neither
// ruff nor the git belay runs reads the caller's configuration.
func lintRuff(t *testing.T, dir string) linted {
	t.Helper()
	t.Setenv("RUFF_CACHE_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	var logs bytes.Buffer
	gate := linter.NewRuff(linter.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	report, err := gate.Lint(t.Context(), dir)
	if err != nil {
		t.Fatalf("Lint: %v\nlogs:\n%s", err, logs.String())
	}
	warned := strings.Contains(logs.String(), "level=WARN msg=\"belay/linter: cannot scope ruff")
	return linted{report: report, warned: warned, logs: logs.String()}
}

// writePyProject writes a Python project configured by ruffConfig, holding
// files.
func writePyProject(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	all := map[string]string{"ruff.toml": ruffConfig}
	for name, content := range files {
		all[name] = content
	}
	writeFiles(t, dir, all)
}

// requireRuff skips t unless ruff and git both run. The skip message is meant
// to be acted on by whoever reads it, a person or a coding agent.
func requireRuff(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ruff", "--version").Output()
	if version := strings.TrimSpace(string(out)); err != nil || !strings.HasPrefix(version, "ruff ") {
		t.Skipf("skipping: needs ruff on PATH (version %q, error %v). "+
			"Install it with `pipx install ruff`, `uv tool install ruff` or `brew install ruff`, "+
			"or ask your coding agent: %q", version, err, ruffAgentPrompt)
	}
	// Run git rather than only finding it: on macOS without the command
	// line tools, /usr/bin/git exists and fails.
	if err := exec.CommandContext(ctx, "git", "--version").Run(); err != nil {
		t.Skipf("skipping: needs a working git on PATH (%v). "+
			"Install it from https://git-scm.com/downloads, "+
			"or ask your coding agent: %q", err, ruffAgentPrompt)
	}
}
