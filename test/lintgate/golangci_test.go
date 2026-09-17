//go:build unix

package lintgate

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/linter"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Sources for TestGolangCIScopesToChangedLines. Each unchecked os.Remove is an
// errcheck finding, which internal/linter maps to Major, so any one of them
// fails the default gate.
const (
	// scopeConfig enables errcheck and nothing else. Because it sits in the
	// linted directory, it also keeps a .golangci.yml in the home directory
	// of whoever runs the test from changing the result.
	scopeConfig = "version: \"2\"\nlinters:\n  default: none\n  enable:\n    - errcheck\n"

	// scopeDebt is committed code with a finding on line 7.
	scopeDebt = "package scoped\n\nimport \"os\"\n\n// Old was committed with a finding in it.\nfunc Old() {\n\tos.Remove(\"old\")\n}\n"

	// scopeEdit is appended to scopeDebt, and adds a finding on line 12.
	scopeEdit = "\n// Edited is new code in a committed file.\nfunc Edited() {\n\tos.Remove(\"edited\")\n}\n"

	// scopeAdded is a new file with a finding on line 7.
	scopeAdded = "package scoped\n\nimport \"os\"\n\n// Added is new code in a new file.\nfunc Added() {\n\tos.Remove(\"added\")\n}\n"

	// scopeClean is a new file with nothing to report.
	scopeClean = "package scoped\n\n// Clean is new code with nothing to report.\nfunc Clean() int { return 1 }\n"

	// scopeBroken is committed code that does not compile.
	scopeBroken = "package scoped\n\n// Broken does not compile: x is declared and not used.\nfunc Broken() { x := 1 }\n"
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
	requireGolangCIV2(t)
	requireGit(t)

	tests := []struct {
		name string
		// repo commits base to a new git repository. Without it the
		// directory is not a repository at all, like a fanout candidate,
		// which internal/isolate/dircopy copies without .git.
		repo     bool
		base     map[string]string
		change   map[string]string
		wantGate belay.GateStatus
		// want is every finding as "file:line rule severity", sorted.
		want []string
	}{
		{
			name:     "committed findings do not count and new ones do",
			repo:     true,
			base:     map[string]string{"debt.go": scopeDebt},
			change:   map[string]string{"debt.go": scopeDebt + scopeEdit, "added.go": scopeAdded},
			wantGate: belay.GateFail,
			want:     []string{"added.go:7 errcheck major", "debt.go:12 errcheck major"},
		},
		{
			name:     "a change with no findings passes despite committed ones",
			repo:     true,
			base:     map[string]string{"debt.go": scopeDebt},
			change:   map[string]string{"clean.go": scopeClean},
			wantGate: belay.GatePass,
			want:     []string{},
		},
		{
			name:     "outside a git repository every finding counts",
			repo:     false,
			base:     map[string]string{"debt.go": scopeDebt},
			change:   map[string]string{"debt.go": scopeDebt + scopeEdit, "added.go": scopeAdded},
			wantGate: belay.GateFail,
			want:     []string{"added.go:7 errcheck major", "debt.go:12 errcheck major", "debt.go:7 errcheck major"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := lint(t, tt.repo, tt.base, tt.change)
			if report.Gate != tt.wantGate {
				t.Errorf("Gate = %s, want %s (%s)", report.Gate, tt.wantGate, report.Summary)
			}
			if diff := cmp.Diff(tt.want, findings(report.Issues)); diff != "" {
				t.Errorf("findings mismatch (-want +got):\n%s", diff)
			}
		})
	}

	// golangci-lint reports a package that does not compile wherever the
	// error is, and where it attributes the error is its own business, so
	// this case checks the rule and severity rather than a position.
	t.Run("committed code that does not compile still fails", func(t *testing.T) {
		report := lint(t, true,
			map[string]string{"debt.go": scopeDebt, "broken.go": scopeBroken},
			map[string]string{"clean.go": scopeClean})
		if report.Gate != belay.GateFail {
			t.Errorf("Gate = %s, want %s (%s)", report.Gate, belay.GateFail, report.Summary)
		}
		if !slices.ContainsFunc(report.Issues, func(i belay.Issue) bool {
			return i.RuleID == "typecheck" && i.Severity == belay.SeverityBlocker
		}) {
			t.Errorf("no typecheck blocker among findings %v", findings(report.Issues))
		}
	})
}

// lint builds the directory the review node would lint, runs linter.GolangCI
// over it, and returns the report.
//
// base is written first and, when repo is set, committed as the only commit
// of a new repository. change is then written on top and left uncommitted,
// which is where a run's edits are when the gate runs.
func lint(t *testing.T, repo bool, base, change map[string]string) belay.QualityReport {
	t.Helper()

	// A cache per workspace. golangci-lint caches results by package
	// content, so two directories holding the same module can be handed
	// each other's findings, with paths into the wrong directory.
	t.Setenv("GOLANGCI_LINT_CACHE", t.TempDir())

	dir := t.TempDir()
	files := map[string]string{
		"go.mod":        "module example.com/scoped\n\ngo 1.22\n",
		".golangci.yml": scopeConfig,
	}
	maps.Copy(files, base)
	writeFiles(t, dir, files)
	if repo {
		runGit(t, dir, "init", "--quiet")
		runGit(t, dir, "add", "--all")
		runGit(t, dir, "commit", "--quiet", "--message", "base")
	}
	writeFiles(t, dir, change)

	gate := linter.NewGolangCI(linter.WithLogger(slog.New(slog.DiscardHandler)))
	report, err := gate.Lint(t.Context(), dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	return report
}

// writeFiles writes each file, named relative to dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
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

// runGit runs git in dir, away from the caller's git configuration and from
// every inherited GIT_* variable. A GIT_DIR set by a hook that runs the tests
// would otherwise point these commands at the repository under test.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	args = append([]string{"-c", "user.name=belay test", "-c", "user.email=belay-test@example.invalid"}, args...)
	cmd := exec.Command("git", args...) //nolint:gosec // fixed binary; args are this file's own constants
	cmd.Dir = dir
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "GIT_")
	}), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// requireGolangCIV2 skips t unless golangci-lint v2 is on PATH. The message
// is meant to be acted on by whoever reads it, a person or a coding agent.
func requireGolangCIV2(t *testing.T) {
	t.Helper()
	out, err := exec.Command("golangci-lint", "version", "--short").Output()
	if version := strings.TrimSpace(string(out)); err != nil || !strings.HasPrefix(version, "2.") {
		t.Skipf("skipping: needs golangci-lint v2 on PATH (version %q, error %v). "+
			"Install it with `brew install golangci-lint` or "+
			"`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`, "+
			"or ask your coding agent: %q", version, err, agentPrompt)
	}
}

// requireGit skips t unless git is on PATH, in the same terms as
// requireGolangCIV2.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("skipping: needs git on PATH (%v). "+
			"Install it from https://git-scm.com/downloads, "+
			"or ask your coding agent: %q", err, agentPrompt)
	}
}
