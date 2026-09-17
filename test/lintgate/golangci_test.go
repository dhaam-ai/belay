//go:build unix

package lintgate

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/linter"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Sources for the tests below. Each unchecked os.Remove is an errcheck
// finding, which internal/linter maps to Major, so any one of them fails the
// default gate.
const (
	// scopeConfig enables errcheck and nothing else.
	scopeConfig = "version: \"2\"\nlinters:\n  default: none\n  enable:\n    - errcheck\n"

	// scopeDebt is committed code with a finding on line 7.
	scopeDebt = "package scoped\n\nimport \"os\"\n\n// Old was committed with a finding in it.\nfunc Old() {\n\tos.Remove(\"old\")\n}\n"

	// scopeEdit is appended to scopeDebt, and adds a finding on line 12.
	scopeEdit = "\n// Edited is new code in a committed file.\nfunc Edited() {\n\tos.Remove(\"edited\")\n}\n"

	// scopeAdded is a new file with a finding on line 7.
	scopeAdded = "package scoped\n\nimport \"os\"\n\n// Added is new code in a new file.\nfunc Added() {\n\tos.Remove(\"added\")\n}\n"

	// scopeBranch is committed on a branch before the run, with a finding on
	// line 7.
	scopeBranch = "package scoped\n\nimport \"os\"\n\n// Branch was committed on a branch before the run.\nfunc Branch() {\n\tos.Remove(\"branch\")\n}\n"

	// scopeClean is a new file with nothing to report.
	scopeClean = "package scoped\n\n// Clean is new code with nothing to report.\nfunc Clean() int { return 1 }\n"

	// scopeBroken is committed code that does not compile.
	scopeBroken = "package scoped\n\n// Broken does not compile: x is declared and not used.\nfunc Broken() { x := 1 }\n"

	// scopeDo, scopeDoFails and scopeUse change a signature so that a line
	// the change never touched, line 5 of scopeUse, gains a finding.
	scopeDo      = "package scoped\n\n// Do cannot fail.\nfunc Do() {}\n"
	scopeDoFails = "package scoped\n\n// Do can fail now.\nfunc Do() error { return nil }\n"
	scopeUse     = "package scoped\n\n// Use calls Do.\nfunc Use() {\n\tDo()\n}\n"
)

// The expected findings for the run most tests make: scopeEdit appended to a
// committed scopeDebt, plus scopeAdded as a new file.
var (
	changed = []string{"added.go:7 errcheck major", "debt.go:12 errcheck major"}
	all     = []string{"added.go:7 errcheck major", "debt.go:12 errcheck major", "debt.go:7 errcheck major"}
)

// agentPrompt is what a skipped test tells its reader to ask a coding agent,
// so the test runs next time.
const agentPrompt = "Install golangci-lint v2 and git so both are on PATH, " +
	"then run `go test ./test/lintgate -v` and confirm the tests pass instead of skipping."

// TestGolangCIScopesToChangedLines runs linter.GolangCI, with its real
// internal/exec runner and the real golangci-lint, over a module whose last
// commit already holds a finding. Only the lines a run changed may count
// toward the gate.
//
// A stub runner cannot stand in here: what this checks is golangci-lint
// calling git inside the environment internal/exec builds for it.
func TestGolangCIScopesToChangedLines(t *testing.T) {
	goEnv := requireTools(t)

	tests := []struct {
		name     string
		change   map[string]string
		wantGate belay.GateStatus
		want     []string
	}{
		{
			name:     "committed findings do not count and new ones do",
			change:   map[string]string{"debt.go": scopeDebt + scopeEdit, "added.go": scopeAdded},
			wantGate: belay.GateFail,
			want:     changed,
		},
		{
			name:     "a change with no findings passes despite committed ones",
			change:   map[string]string{"clean.go": scopeClean},
			wantGate: belay.GatePass,
			want:     []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeModule(t, dir, map[string]string{"debt.go": scopeDebt})
			commitAll(t, dir)
			writeFiles(t, dir, tt.change)

			check(t, lint(t, goEnv, dir), tt.wantGate, tt.want, false)
		})
	}

	t.Run("a subdirectory of a repository is scoped", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "svc")
		writeModule(t, dir, map[string]string{"debt.go": scopeDebt})
		commitAll(t, root)
		writeFiles(t, dir, map[string]string{"debt.go": scopeDebt + scopeEdit, "added.go": scopeAdded})

		check(t, lint(t, goEnv, dir), belay.GateFail, changed, false)
	})

	// golangci-lint reports a package that does not compile wherever the
	// error is, and where it attributes the error is its own business, so
	// this case checks the rule and severity rather than a position.
	t.Run("committed code that does not compile still fails", func(t *testing.T) {
		dir := t.TempDir()
		writeModule(t, dir, map[string]string{"debt.go": scopeDebt, "broken.go": scopeBroken})
		commitAll(t, dir)
		writeFiles(t, dir, map[string]string{"clean.go": scopeClean})

		got := lint(t, goEnv, dir)
		if got.report.Gate != belay.GateFail {
			t.Errorf("Gate = %s, want %s (%s)", got.report.Gate, belay.GateFail, got.report.Summary)
		}
		if !slices.ContainsFunc(got.report.Issues, func(i belay.Issue) bool {
			return i.RuleID == "typecheck" && i.Severity == belay.SeverityBlocker
		}) {
			t.Errorf("no typecheck blocker among findings %v", findings(got.report.Issues))
		}
	})

	// The last two cases pin documented limits of scoping by line, so the
	// documentation stays true.
	t.Run("a finding on a line the change did not touch does not count", func(t *testing.T) {
		dir := t.TempDir()
		writeModule(t, dir, map[string]string{"do.go": scopeDo, "use.go": scopeUse})
		commitAll(t, dir)
		writeFiles(t, dir, map[string]string{"do.go": scopeDoFails})

		check(t, lint(t, goEnv, dir), belay.GatePass, []string{}, false)
	})

	t.Run("a new file in a directory git ignores does not count", func(t *testing.T) {
		dir := t.TempDir()
		writeModule(t, dir, map[string]string{"debt.go": scopeDebt, ".gitignore": "gen/\n"})
		commitAll(t, dir)
		writeFiles(t, filepath.Join(dir, "gen"), map[string]string{
			"added.go": strings.Replace(scopeAdded, "package scoped", "package gen", 1),
		})

		check(t, lint(t, goEnv, dir), belay.GatePass, []string{}, false)
	})

	t.Run("a new file whose name git ignores does not count", func(t *testing.T) {
		dir := t.TempDir()
		writeModule(t, dir, map[string]string{"debt.go": scopeDebt, ".gitignore": "*_gen.go\n"})
		commitAll(t, dir)
		writeFiles(t, dir, map[string]string{"added_gen.go": scopeAdded})

		check(t, lint(t, goEnv, dir), belay.GatePass, []string{}, false)
	})

	// lib is a repository of its own but a package of the module, so
	// golangci-lint lints it and the enclosing repository's git never
	// lists its files.
	t.Run("a change inside a nested repository does not count", func(t *testing.T) {
		dir := t.TempDir()
		lib := filepath.Join(dir, "lib")
		writeFiles(t, lib, map[string]string{"lib.go": "package lib\n\n// Lib does nothing.\nfunc Lib() {}\n"})
		commitAll(t, lib)
		writeModule(t, dir, map[string]string{"debt.go": scopeDebt})
		commitAll(t, dir)
		writeFiles(t, lib, map[string]string{
			"added.go": strings.Replace(scopeAdded, "package scoped", "package lib", 1),
		})

		check(t, lint(t, goEnv, dir), belay.GatePass, []string{}, false)
	})
}

// TestGolangCIIgnoresRepositoryScopeSettings checks that a repository's own
// issues.* settings cannot change what the gate counts, scoped or not. Left
// alone, some would count findings committed before the run, and others would
// hide findings that should count.
func TestGolangCIIgnoresRepositoryScopeSettings(t *testing.T) {
	goEnv := requireTools(t)
	change := map[string]string{"debt.go": scopeDebt + scopeEdit, "added.go": scopeAdded}

	for _, setting := range []string{
		"new: true",
		"new-from-rev: HEAD~1",
		"new-from-merge-base: main",
		"new-from-patch: empty.patch",
		"whole-files: true",
	} {
		config := scopeConfig + "issues:\n  " + setting + "\n"
		t.Run(setting, func(t *testing.T) {
			t.Run("scoped", func(t *testing.T) {
				dir := t.TempDir()
				writeModule(t, dir, map[string]string{"debt.go": scopeDebt, "empty.patch": "", ".golangci.yml": config})
				commitAll(t, dir)
				runGit(t, dir, "checkout", "--quiet", "-b", "feature")
				writeFiles(t, dir, map[string]string{"branch.go": scopeBranch})
				commitAll(t, dir)
				writeFiles(t, dir, change)

				check(t, lint(t, goEnv, dir), belay.GateFail, changed, false)
			})

			// An ignored workspace in a repository with two commits on a
			// branch, so every setting has history to act on.
			t.Run("unscoped", func(t *testing.T) {
				root := t.TempDir()
				writeFiles(t, root, map[string]string{".gitignore": "ws/\n", "one.txt": "one\n"})
				commitAll(t, root)
				runGit(t, root, "checkout", "--quiet", "-b", "feature")
				writeFiles(t, root, map[string]string{"two.txt": "two\n"})
				commitAll(t, root)
				dir := filepath.Join(root, "ws")
				writeModule(t, dir, map[string]string{"debt.go": scopeDebt, "empty.patch": "", ".golangci.yml": config})
				writeFiles(t, dir, change)

				check(t, lint(t, goEnv, dir), belay.GateFail, all, true)
			})
		})
	}
}

// TestGolangCICountsEverythingGitCannotScope covers the directories where no
// commit holds the code. Every finding must count, and belay must say that it
// did not scope. Two of these layouts are inside a repository that ignores
// them, where scoping by HEAD would hide every finding.
func TestGolangCICountsEverythingGitCannotScope(t *testing.T) {
	goEnv := requireTools(t)
	base := map[string]string{"debt.go": scopeDebt}
	change := map[string]string{"debt.go": scopeDebt + scopeEdit, "added.go": scopeAdded}

	t.Run("outside a git repository", func(t *testing.T) {
		dir := t.TempDir()
		writeModule(t, dir, base)
		writeFiles(t, dir, change)

		check(t, lint(t, goEnv, dir), belay.GateFail, all, true)
	})

	t.Run("in a repository with no commits", func(t *testing.T) {
		dir := t.TempDir()
		writeModule(t, dir, base)
		runGit(t, dir, "init", "--quiet")
		writeFiles(t, dir, change)

		check(t, lint(t, goEnv, dir), belay.GateFail, all, true)
	})

	t.Run("in a directory the enclosing repository ignores", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{".gitignore": "ws/\n"})
		commitAll(t, root)
		dir := filepath.Join(root, "ws")
		writeModule(t, dir, base)
		writeFiles(t, dir, change)

		check(t, lint(t, goEnv, dir), belay.GateFail, all, true)
	})

	// A negated rule keeps one file in HEAD, so HEAD is not empty here, but
	// git ignores every file the run adds.
	t.Run("in an ignored directory that holds a committed file", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{".gitignore": "ws/*\n!ws/.gitkeep\n"})
		writeFiles(t, filepath.Join(root, "ws"), map[string]string{".gitkeep": ""})
		commitAll(t, root)
		dir := filepath.Join(root, "ws")
		writeModule(t, dir, base)
		writeFiles(t, dir, change)

		check(t, lint(t, goEnv, dir), belay.GateFail, all, true)
	})

	// internal/state puts candidates at .belay/runs/<run>/candidates/<id>,
	// inside the workspace, and a workspace usually ignores .belay.
	t.Run("in a fanout candidate under an ignored .belay", func(t *testing.T) {
		root := t.TempDir()
		writeModule(t, root, base)
		writeFiles(t, root, map[string]string{".gitignore": ".belay/\n"})
		commitAll(t, root)
		dir := filepath.Join(root, ".belay", "runs", "r1", "candidates", "c1", "workspace")
		writeModule(t, dir, base)
		writeFiles(t, dir, change)

		check(t, lint(t, goEnv, dir), belay.GateFail, all, true)
	})
}

// linted is one GolangCI run: its report, and whether it logged a warning.
type linted struct {
	report belay.QualityReport
	warned bool
	logs   string
}

// lint runs linter.GolangCI over dir.
//
// It isolates the run from the machine running the test. golangci-lint gets
// a fresh cache, because it caches results by package content, so two
// directories holding the same module can be handed each other's findings
// with paths into the wrong directory. HOME is a fresh directory, so neither
// the git that golangci-lint runs nor golangci-lint itself reads the
// caller's configuration, and the Go settings that would otherwise follow
// HOME are pinned to the caller's values in goEnv.
func lint(t *testing.T, goEnv map[string]string, dir string) linted {
	t.Helper()
	for name, value := range goEnv {
		t.Setenv(name, value)
	}
	t.Setenv("GOLANGCI_LINT_CACHE", t.TempDir())
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "off")
	t.Setenv("HOME", t.TempDir())

	var logs bytes.Buffer
	gate := linter.NewGolangCI(linter.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	report, err := gate.Lint(t.Context(), dir)
	if err != nil {
		t.Fatalf("Lint: %v\nlogs:\n%s", err, logs.String())
	}
	warned := strings.Contains(logs.String(), "level=WARN msg=\"belay/linter: cannot scope golangci-lint")
	return linted{report: report, warned: warned, logs: logs.String()}
}

// check compares a run with what the test expects: the gate, every finding as
// "file:line rule severity" in sorted order, and whether belay warned that it
// could not scope the findings.
func check(t *testing.T, got linted, wantGate belay.GateStatus, want []string, wantWarn bool) {
	t.Helper()
	if got.report.Gate != wantGate {
		t.Errorf("Gate = %s, want %s (%s)", got.report.Gate, wantGate, got.report.Summary)
	}
	if diff := cmp.Diff(want, findings(got.report.Issues)); diff != "" {
		t.Errorf("findings mismatch (-want +got):\n%s", diff)
	}
	if got.warned != wantWarn {
		t.Errorf("warned = %v, want %v; logs:\n%s", got.warned, wantWarn, got.logs)
	}
}

// findings renders issues as sorted "file:line rule severity" strings.
func findings(issues []belay.Issue) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, fmt.Sprintf("%s:%d %s %s", i.File, i.Line, i.RuleID, i.Severity))
	}
	slices.Sort(out)
	return out
}

// writeModule writes a module named after dir, configured by scopeConfig,
// holding files.
func writeModule(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	all := map[string]string{
		"go.mod":        "module example.com/" + filepath.Base(dir) + "\n\ngo 1.22\n",
		".golangci.yml": scopeConfig,
	}
	maps.Copy(all, files)
	writeFiles(t, dir, all)
}

// writeFiles writes each file, named relative to dir, creating dir if needed.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// commitAll commits everything under root, creating the repository first if
// there is none.
func commitAll(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		runGit(t, root, "init", "--quiet")
	}
	runGit(t, root, "add", "--all")
	runGit(t, root, "commit", "--quiet", "--message", "commit")
}

// runGit runs git in dir, away from the caller's git configuration and from
// every inherited GIT_* variable. A GIT_DIR set by a hook that runs the tests
// would otherwise point these commands at the repository under test.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	args = append([]string{
		"-c", "user.name=belay test",
		"-c", "user.email=belay-test@example.invalid",
		"-c", "init.defaultBranch=main",
	}, args...)
	cmd := exec.Command("git", args...) //nolint:gosec // fixed binary; args are this file's own constants
	cmd.Dir = dir
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "GIT_")
	}), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// requireTools skips t unless golangci-lint v2, git and go all run, and
// returns the Go settings lint pins. The skip message is meant to be acted on
// by whoever reads it, a person or a coding agent.
func requireTools(t *testing.T) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "golangci-lint", "version", "--short").Output()
	if version := strings.TrimSpace(string(out)); err != nil || !strings.HasPrefix(version, "2.") {
		t.Skipf("skipping: needs golangci-lint v2 on PATH (version %q, error %v). "+
			"Install it with `brew install golangci-lint` or "+
			"`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`, "+
			"or ask your coding agent: %q", version, err, agentPrompt)
	}
	// Run git rather than only finding it: on macOS without the command
	// line tools, /usr/bin/git exists and fails.
	if err := exec.CommandContext(ctx, "git", "--version").Run(); err != nil {
		t.Skipf("skipping: needs a working git on PATH (%v). "+
			"Install it from https://git-scm.com/downloads, "+
			"or ask your coding agent: %q", err, agentPrompt)
	}

	names := []string{"GOCACHE", "GOMODCACHE", "GOPATH"}
	out, err = exec.CommandContext(ctx, "go", append([]string{"env"}, names...)...).Output() //nolint:gosec // fixed binary and arguments
	if err != nil {
		t.Skipf("skipping: needs go on PATH (%v)", err)
	}
	values := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(values) != len(names) {
		t.Fatalf("go env printed %d lines for %d names: %q", len(values), len(names), out)
	}
	env := make(map[string]string, len(names))
	for i, name := range names {
		env[name] = values[i]
	}
	return env
}
