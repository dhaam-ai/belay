//go:build fixrate

package fixrate

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gitIn runs git in dir with a fixed identity and none of the caller's
// configuration, and returns what it printed.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{ //nolint:gosec // fixed binary; arguments are this file's own
		"-c", "user.name=fixrate test", "-c", "user.email=fixrate@example.invalid", "-c", "init.defaultBranch=main",
	}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeTree writes files, named relative to dir with slashes, creating dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// commitRepo makes dir a repository holding files in one commit, and returns
// that commit.
func commitRepo(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	writeTree(t, dir, files)
	gitIn(t, dir, "init", "--quiet")
	gitIn(t, dir, "add", "--all")
	gitIn(t, dir, "commit", "--quiet", "--message", "base")
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// TestCellPatch is the one artifact a cell hands the official harness: the
// change, as a patch git applies to the base the workspace started from. It
// must carry every kind of change an agent makes -- an edit, a new file it
// never added to the index, a deletion -- and nothing the repository
// ignores, and taking it must leave the workspace exactly as it was.
func TestCellPatch(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	base := commitRepo(t, ws, map[string]string{
		".gitignore":    "__pycache__/\n",
		"pkg/core.py":   "def resize(n, f):\n    return int(n * f)\n",
		"pkg/legacy.py": "OLD = True\n",
		"README.md":     "widget\n",
	})
	// The agent's change: an edit, a new file it never added, a deletion,
	// and a byproduct the repository ignores.
	writeTree(t, ws, map[string]string{
		"pkg/core.py":                  "def resize(n, f):\n    return int(n * f + 0.5)\n",
		"tests/test_resize.py":         "from pkg.core import resize\n\n\ndef test_half_up():\n    assert resize(3, 0.5) == 2\n",
		"pkg/__pycache__/core.cpython": "compiled",
	})
	if err := os.Remove(filepath.Join(ws, "pkg", "legacy.py")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	statusBefore := gitIn(t, ws, "status", "--porcelain")

	patch, err := cellPatch(t.Context(), ws, base, filepath.Join(t.TempDir(), "patch.index"))
	if err != nil {
		t.Fatalf("cellPatch() error = %v", err)
	}

	for _, want := range []string{"pkg/core.py", "tests/test_resize.py", "pkg/legacy.py", "+    return int(n * f + 0.5)"} {
		if !strings.Contains(patch, want) {
			t.Errorf("the patch does not carry %q:\n%s", want, patch)
		}
	}
	if strings.Contains(patch, "__pycache__") {
		t.Errorf("the patch carries a file the repository ignores:\n%s", patch)
	}
	if got := gitIn(t, ws, "status", "--porcelain"); got != statusBefore {
		t.Errorf("taking the patch changed the workspace's own git state:\nbefore:\n%s\nafter:\n%s", statusBefore, got)
	}

	// It applies to the base, as the harness will apply it.
	fresh := t.TempDir()
	gitIn(t, fresh, "clone", "--quiet", ws, ".")
	gitIn(t, fresh, "checkout", "--quiet", base)
	writeTree(t, fresh, map[string]string{"model.patch": patch})
	gitIn(t, fresh, "apply", "--verbose", "model.patch")
	status := gitIn(t, fresh, "status", "--porcelain", "--untracked-files=all")
	var files []string
	for _, line := range strings.Split(status, "\n") {
		files = append(files, strings.Join(strings.Fields(line), " "))
	}
	for _, want := range []string{"D pkg/legacy.py", "M pkg/core.py", "?? tests/test_resize.py"} {
		if !slices.Contains(files, want) {
			t.Errorf("after applying the patch to the base, git status lacks %q:\n%s", want, status)
		}
	}
}

// TestCellPatch_NoChangeIsEmpty: an arm that changed nothing has an empty
// patch, which the harness does not run and which cannot resolve.
func TestCellPatch_NoChangeIsEmpty(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	base := commitRepo(t, ws, map[string]string{"a.py": "x = 1\n"})
	patch, err := cellPatch(t.Context(), ws, base, filepath.Join(t.TempDir(), "patch.index"))
	if err != nil {
		t.Fatalf("cellPatch() error = %v", err)
	}
	if patch != "" {
		t.Errorf("cellPatch() = %q, want an empty patch for an untouched workspace", patch)
	}
}
