//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveBlocksAPythonProjectWithoutRuff pins the lint gate's blocker
// for the commonest case it fires on: a Python project that never configured
// ruff. belay recognises such a project -- its test runner would run pytest
// there -- so the blocker must not say it recognises no project, and must
// name the change that gives the gate something to run.
func TestResolveBlocksAPythonProjectWithoutRuff(t *testing.T) {
	ws := t.TempDir()
	pyproject := "[project]\nname = \"app\"\n\n[tool.pytest.ini_options]\n"
	if err := os.WriteFile(filepath.Join(ws, "pyproject.toml"), []byte(pyproject), 0o600); err != nil {
		t.Fatalf("write pyproject.toml: %v", err)
	}

	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := blockerText(plan)
	for _, want := range []string{"found a Python project", "configures ruff", "[tool.ruff]", `review.mode to "sonar"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the lint blocker does not say %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "recognises no Go, Node or Python project") {
		t.Errorf("the lint blocker says belay recognises no project, but it found a Python one; got:\n%s", got)
	}
}
