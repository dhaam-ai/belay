package detect

import (
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// ignoreProblems keeps the golden table readable. Problems carry wrapped
// errors, which compare by identity rather than by message; the degradation
// tests in go_test.go and node_test.go assert on them directly.
var ignoreProblems = cmpopts.IgnoreFields(Project{}, "Problems")

// TestDetectTrees is the golden table over the synthetic fixtures in testdata.
// It pins the whole result of Detect, ordering included.
func TestDetectTrees(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fixture string
		want    []Project
	}{
		{
			name:    "a directory with no markers is unknown",
			fixture: "empty",
			want: []Project{
				{Kind: KindUnknown, Root: filepath.Join("testdata", "empty"), Confidence: ConfidenceNone},
			},
		},
		{
			name:    "pure go",
			fixture: "go-simple",
			want: []Project{{
				Kind:       KindGo,
				Root:       filepath.Join("testdata", "go-simple"),
				Markers:    []string{"go.mod"},
				Confidence: ConfidenceHigh,
				Go:         &GoInfo{Module: "github.com/belay-dev/fixture-service", GoVersion: "1.26.3"},
			}},
		},
		{
			name:    "go.mod with no module directive",
			fixture: "go-malformed",
			want: []Project{{
				Kind:       KindGo,
				Root:       filepath.Join("testdata", "go-malformed"),
				Markers:    []string{"go.mod"},
				Confidence: ConfidenceLow,
				Go:         &GoInfo{GoVersion: "1.24"},
			}},
		},
		{
			name:    "pure node with an npm lockfile",
			fixture: "node-npm",
			want: []Project{{
				Kind:       KindNode,
				Root:       filepath.Join("testdata", "node-npm"),
				Markers:    []string{"package.json", "package-lock.json"},
				Confidence: ConfidenceHigh,
				Node: &NodeInfo{
					Name:           "npm-fixture",
					PackageManager: PackageManagerNPM,
					TestFramework:  TestFrameworkJest,
					TestScript:     "jest --ci",
					HasESLint:      true,
				},
			}},
		},
		{
			name:    "pure node with a yarn lockfile",
			fixture: "node-yarn",
			want: []Project{{
				Kind:       KindNode,
				Root:       filepath.Join("testdata", "node-yarn"),
				Markers:    []string{"package.json", "yarn.lock"},
				Confidence: ConfidenceHigh,
				Node: &NodeInfo{
					Name:           "yarn-fixture",
					PackageManager: PackageManagerYarn,
					TestFramework:  TestFrameworkVitest,
					TestScript:     "vitest run --coverage",
				},
			}},
		},
		{
			name:    "pure node with a pnpm lockfile",
			fixture: "node-pnpm",
			want: []Project{{
				Kind:       KindNode,
				Root:       filepath.Join("testdata", "node-pnpm"),
				Markers:    []string{"package.json", "pnpm-lock.yaml"},
				Confidence: ConfidenceHigh,
				Node: &NodeInfo{
					Name:           "pnpm-fixture",
					PackageManager: PackageManagerPNPM,
					TestFramework:  TestFrameworkNodeTest,
					TestScript:     "node --test test/",
				},
			}},
		},
		{
			name:    "pure node with a bun lockfile",
			fixture: "node-bun",
			want: []Project{{
				Kind:       KindNode,
				Root:       filepath.Join("testdata", "node-bun"),
				Markers:    []string{"package.json", "bun.lock"},
				Confidence: ConfidenceHigh,
				Node: &NodeInfo{
					Name:           "bun-fixture",
					PackageManager: PackageManagerBun,
					TestFramework:  TestFrameworkMocha,
					TestScript:     "mocha --reporter spec",
				},
			}},
		},
		{
			name:    "malformed package.json still names the kind",
			fixture: "node-malformed",
			want: []Project{{
				Kind:       KindNode,
				Root:       filepath.Join("testdata", "node-malformed"),
				Markers:    []string{"package.json", "yarn.lock"},
				Confidence: ConfidenceLow,
				Node:       &NodeInfo{PackageManager: PackageManagerYarn},
			}},
		},
		{
			name:    "python declared by pyproject.toml",
			fixture: "python-pyproject",
			want: []Project{{
				Kind:       KindPython,
				Root:       filepath.Join("testdata", "python-pyproject"),
				Markers:    []string{"pyproject.toml"},
				Confidence: ConfidenceHigh,
				Python:     &PythonInfo{HasPytest: true, HasRuff: true},
			}},
		},
		{
			name:    "python declared by setup.py",
			fixture: "python-setup",
			want: []Project{{
				Kind:       KindPython,
				Root:       filepath.Join("testdata", "python-setup"),
				Markers:    []string{"setup.py"},
				Confidence: ConfidenceHigh,
				Python:     &PythonInfo{HasPytest: true},
			}},
		},
		{
			name:    "python implied by requirements.txt",
			fixture: "python-requirements",
			want: []Project{{
				Kind:       KindPython,
				Root:       filepath.Join("testdata", "python-requirements"),
				Markers:    []string{"requirements.txt"},
				Confidence: ConfidenceMedium,
				Python:     &PythonInfo{HasPytest: true, HasRuff: true},
			}},
		},
		{
			name:    "python implied by tox.ini",
			fixture: "python-tox",
			want: []Project{{
				Kind:       KindPython,
				Root:       filepath.Join("testdata", "python-tox"),
				Markers:    []string{"tox.ini"},
				Confidence: ConfidenceMedium,
				Python:     &PythonInfo{HasPytest: true},
			}},
		},
		{
			name:    "a go service with a frontend beside it",
			fixture: "monorepo-go-node",
			want: []Project{
				{
					Kind:       KindGo,
					Root:       filepath.Join("testdata", "monorepo-go-node"),
					Markers:    []string{"go.mod"},
					Confidence: ConfidenceHigh,
					Go:         &GoInfo{Module: "github.com/belay-dev/fixture-monorepo", GoVersion: "1.26.3"},
				},
				{
					Kind:       KindNode,
					Root:       filepath.Join("testdata", "monorepo-go-node", "web"),
					Markers:    []string{"package.json", "package-lock.json"},
					Confidence: ConfidenceHigh,
					Node: &NodeInfo{
						Name:           "fixture-monorepo-web",
						PackageManager: PackageManagerNPM,
						TestFramework:  TestFrameworkJest,
						TestScript:     "jest",
					},
				},
			},
		},
		{
			name:    "projects two levels down, ranked by confidence then path",
			fixture: "monorepo-nested",
			want: []Project{
				{
					Kind:       KindGo,
					Root:       filepath.Join("testdata", "monorepo-nested", "services", "api"),
					Markers:    []string{"go.mod"},
					Confidence: ConfidenceHigh,
					Go:         &GoInfo{Module: "github.com/belay-dev/fixture-nested/services/api", GoVersion: "1.26.3"},
				},
				{
					Kind:       KindNode,
					Root:       filepath.Join("testdata", "monorepo-nested", "services", "web"),
					Markers:    []string{"package.json", "pnpm-lock.yaml"},
					Confidence: ConfidenceHigh,
					Node: &NodeInfo{
						Name:           "fixture-nested-web",
						PackageManager: PackageManagerPNPM,
						TestFramework:  TestFrameworkVitest,
						TestScript:     "vitest run",
					},
				},
				{
					Kind:       KindPython,
					Root:       filepath.Join("testdata", "monorepo-nested", "tools", "scripts"),
					Markers:    []string{"requirements.txt"},
					Confidence: ConfidenceMedium,
					Python:     &PythonInfo{HasPytest: true, HasRuff: true},
				},
			},
		},
		{
			name:    "three kinds in one directory are ordered by kind",
			fixture: "polyglot-same-dir",
			want: []Project{
				{
					Kind:       KindGo,
					Root:       filepath.Join("testdata", "polyglot-same-dir"),
					Markers:    []string{"go.mod"},
					Confidence: ConfidenceHigh,
					Go:         &GoInfo{Module: "github.com/belay-dev/fixture-polyglot", GoVersion: "1.26.3"},
				},
				{
					Kind:       KindNode,
					Root:       filepath.Join("testdata", "polyglot-same-dir"),
					Markers:    []string{"package.json"},
					Confidence: ConfidenceHigh,
					Node: &NodeInfo{
						Name:           "fixture-polyglot",
						PackageManager: PackageManagerNPM,
						TestFramework:  TestFrameworkNodeTest,
						TestScript:     "node --test",
					},
				},
				{
					Kind:       KindPython,
					Root:       filepath.Join("testdata", "polyglot-same-dir"),
					Markers:    []string{"pyproject.toml"},
					Confidence: ConfidenceHigh,
					Python:     &PythonInfo{HasPytest: true},
				},
			},
		},
		{
			name:    "generated directories are not searched",
			fixture: "skiplist",
			want: []Project{{
				Kind:       KindNode,
				Root:       filepath.Join("testdata", "skiplist"),
				Markers:    []string{"package.json"},
				Confidence: ConfidenceHigh,
				Node: &NodeInfo{
					Name:           "skiplist-fixture",
					PackageManager: PackageManagerNPM,
					TestFramework:  TestFrameworkVitest,
					TestScript:     "vitest run",
				},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Detect(filepath.Join("testdata", tt.fixture))
			if err != nil {
				t.Fatalf("Detect returned error: %v", err)
			}
			if diff := cmp.Diff(tt.want, got, ignoreProblems); diff != "" {
				t.Errorf("Detect mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDetectOrderingIsStable runs the same scan repeatedly. os.ReadDir sorts
// its entries, but nothing guarantees that two projects at different depths
// are appended in a stable relative order, so the sort is what callers and
// golden tests rely on.
func TestDetectOrderingIsStable(t *testing.T) {
	t.Parallel()

	root := filepath.Join("testdata", "monorepo-nested")
	first, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if len(first) < 3 {
		t.Fatalf("fixture yielded %d projects, want at least 3 to make ordering meaningful", len(first))
	}

	want := render(first)
	for i := range 100 {
		got, err := Detect(root)
		if err != nil {
			t.Fatalf("Detect returned error on run %d: %v", i, err)
		}
		if diff := cmp.Diff(want, render(got)); diff != "" {
			t.Fatalf("run %d differs from run 0 (-run0 +run%d):\n%s", i, i, diff)
		}
	}
}

func TestDetectRanksByConfidenceThenPath(t *testing.T) {
	t.Parallel()

	root := writeFiles(t, map[string]string{
		"z-app/go.mod":           "module example.com/z\ngo 1.26.3\n",
		"a-app/requirements.txt": "pytest\n",
		"m-app/package.json":     `{"name":"m"}`,
		"a-app/nested/setup.py":  "setup()\n",
		"b-lib/tox.ini":          "[tox]\n",
	})

	got, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}

	var lines []string
	for _, project := range got {
		rel, err := filepath.Rel(root, project.Root)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%v %v %s", project.Confidence, project.Kind, filepath.ToSlash(rel)))
	}

	// Equal confidence ties break on the path, so a-app/nested sorts ahead of
	// m-app and m-app ahead of z-app, regardless of kind or walk order.
	wantOrder := []string{
		"high python a-app/nested",
		"high node m-app",
		"high go z-app",
		"medium python a-app",
		"medium python b-lib",
	}
	if diff := cmp.Diff(wantOrder, lines); diff != "" {
		t.Errorf("ordering mismatch (-want +got):\n%s", diff)
	}
}

func TestDetectMaxDepthBoundsTheSearch(t *testing.T) {
	t.Parallel()

	root := writeFiles(t, map[string]string{
		"a/b/c/d/go.mod": "module example.com/deep\ngo 1.26.3\n",
	})

	shallow, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if len(shallow) != 1 || shallow[0].Kind != KindUnknown {
		t.Errorf("Detect = %+v, want unknown: the module is below DefaultMaxDepth", shallow)
	}

	deep, err := Detect(root, WithMaxDepth(4))
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if len(deep) != 1 || deep[0].Kind != KindGo {
		t.Fatalf("Detect = %+v, want one Go project", deep)
	}
	if want := filepath.Join(root, "a", "b", "c", "d"); deep[0].Root != want {
		t.Errorf("Root = %q, want %q", deep[0].Root, want)
	}
}

func TestDefaultSkipDirsIsACopy(t *testing.T) {
	t.Parallel()

	first := DefaultSkipDirs()
	if len(first) == 0 {
		t.Fatal("DefaultSkipDirs() is empty")
	}
	first[0] = "mutated"

	second := DefaultSkipDirs()
	if second[0] == "mutated" {
		t.Error("DefaultSkipDirs() returned a view of the package's own slice")
	}
}

// render flattens a result to a comparable string, problems included.
func render(projects []Project) string {
	var out strings.Builder
	for _, project := range projects {
		fmt.Fprintf(&out, "%v\t%v\t%s\t%s", project.Kind, project.Confidence,
			filepath.ToSlash(project.Root), strings.Join(project.Markers, ","))
		for _, problem := range project.Problems {
			fmt.Fprintf(&out, "\t%s", problem)
		}
		out.WriteByte('\n')
	}
	return out.String()
}

// TestPackageStaysCheap guards the promise in the package documentation: no
// process execution, no network, standard library only. It reads the package
// source rather than trusting review.
func TestPackageStaysCheap(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), "detect.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing detect.go: %v", err)
	}

	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquoting import %s: %v", spec.Path.Value, err)
		}
		first, _, _ := strings.Cut(imported, "/")
		switch {
		case strings.Contains(first, "."):
			t.Errorf("imports %q: detection must depend on the standard library only", imported)
		case imported == "os/exec":
			t.Errorf("imports %q: detection must never execute a process", imported)
		case first == "net":
			t.Errorf("imports %q: detection must never open a network connection", imported)
		}
	}
}
