//go:build unix

package linter

import (
	"strings"
	"testing"
)

// TestExplain pins what belay says when the lint gate has nothing to run.
//
// The CLI's blocker used to say "belay recognises no Go, Node or Python
// project" for every such directory. For a Python project that has simply
// never configured ruff -- most of them, and 467 of SWE-bench Verified's 500
// tasks by this roadmap's count -- that is false, and it points the reader at
// the wrong fix: belay recognised the project, and found no linter it runs.
func TestExplain(t *testing.T) {
	t.Parallel()

	const (
		pyproject = "[project]\nname = \"app\"\n"
		pkg       = `{"name": "app", "scripts": {"test": "jest"}}`
	)

	tests := []struct {
		name        string
		files       map[string]string
		wantMissing bool
		// Each fragment must appear in the problem or the fix.
		problemHas []string
		fixHas     []string
	}{
		{
			name:        "a python project that never configured ruff",
			files:       map[string]string{"pyproject.toml": pyproject},
			wantMissing: true,
			problemHas:  []string{"Python project", "ruff", "nothing"},
			fixHas:      []string{"[tool.ruff]", "pyproject.toml", "review.mode"},
		},
		{
			name:        "a python project with only a requirements file",
			files:       map[string]string{"requirements.txt": "requests\n"},
			wantMissing: true,
			problemHas:  []string{"Python project", "ruff"},
			fixHas:      []string{"ruff.toml", "review.mode"},
		},
		{
			name:        "a node project without eslint",
			files:       map[string]string{"package.json": pkg},
			wantMissing: true,
			problemHas:  []string{"Node project", "ESLint"},
			fixHas:      []string{"eslint", "review.mode"},
		},
		{
			name:        "a node and a python project, neither linted",
			files:       map[string]string{"package.json": pkg, "pyproject.toml": pyproject},
			wantMissing: true,
			problemHas:  []string{"Node project", "ESLint", "Python project", "ruff"},
			fixHas:      []string{"eslint", "[tool.ruff]", "review.mode"},
		},
		{
			name:        "nothing belay recognises at all",
			files:       map[string]string{"README.md": "hello\n"},
			wantMissing: true,
			problemHas:  []string{"recognises no Go, Node or Python project"},
			fixHas:      []string{"--workspace", "review.mode"},
		},
		{
			name:  "a go module is linted",
			files: map[string]string{"go.mod": "module example.com/app\n\ngo 1.26\n"},
		},
		{
			name:  "a python project that configures ruff is linted",
			files: map[string]string{"pyproject.toml": pyproject + "\n[tool.ruff]\n"},
		},
		{
			name:  "a node project with eslint is linted",
			files: map[string]string{"package.json": `{"name": "app", "devDependencies": {"eslint": "^9.0.0"}}`},
		},
		{
			name: "one linted project is enough",
			files: map[string]string{
				"package.json": pkg,
				"ruff.toml":    "line-length = 88\n",
				"setup.py":     "from setuptools import setup\n",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := writeFiles(t, tc.files)
			why, missing := Explain(dir)
			if missing != tc.wantMissing {
				t.Fatalf("Explain() missing = %v, want %v (%+v)", missing, tc.wantMissing, why)
			}
			if !missing {
				if why != (Absence{}) {
					t.Errorf("Explain() = %+v with nothing missing, want the zero Absence", why)
				}
				return
			}
			if !strings.Contains(why.Problem, dir) {
				t.Errorf("Problem = %q, want it to name the directory %q", why.Problem, dir)
			}
			for _, want := range tc.problemHas {
				if !strings.Contains(why.Problem, want) {
					t.Errorf("Problem = %q, want it to mention %q", why.Problem, want)
				}
			}
			for _, want := range tc.fixHas {
				if !strings.Contains(why.Fix, want) {
					t.Errorf("Fix = %q, want it to mention %q", why.Fix, want)
				}
			}
		})
	}
}

// TestExplain_NothingRecognisedKeepsTheCLIWording pins the one case whose
// wording does not change, so that the CLI can take every blocker from
// Explain without rewording the blocker people already see for a directory
// that is not a project at all.
func TestExplain_NothingRecognisedKeepsTheCLIWording(t *testing.T) {
	t.Parallel()

	dir := writeFiles(t, map[string]string{})
	why, missing := Explain(dir)
	if !missing {
		t.Fatal("Explain() missing = false for an empty directory")
	}
	wantProblem := `review.mode is "lint", but belay recognises no Go, Node or Python project in ` + dir + " to lint."
	wantFix := `point --workspace at the repository, or set review.mode to "sonar" in belay.yaml`
	if why.Problem != wantProblem {
		t.Errorf("Problem = %q\nwant %q", why.Problem, wantProblem)
	}
	if why.Fix != wantFix {
		t.Errorf("Fix = %q\nwant %q", why.Fix, wantFix)
	}
}
