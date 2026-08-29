package detect

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestDetectNodePackage covers the two facts the runner adapter cannot build a
// command line without: which package manager runs the script, and which
// framework the script invokes.
func TestDetectNodePackage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		fixture        string
		wantManager    PackageManager
		wantVersion    string
		wantFramework  TestFramework
		wantScript     string
		wantMarkers    []string
		wantConfidence Confidence
		wantESLint     bool
	}{
		{
			name:           "npm lockfile with jest",
			fixture:        "node-npm",
			wantManager:    PackageManagerNPM,
			wantFramework:  TestFrameworkJest,
			wantScript:     "jest --ci",
			wantMarkers:    []string{"package.json", "package-lock.json"},
			wantConfidence: ConfidenceHigh,
			wantESLint:     true,
		},
		{
			name:           "yarn lockfile with vitest",
			fixture:        "node-yarn",
			wantManager:    PackageManagerYarn,
			wantFramework:  TestFrameworkVitest,
			wantScript:     "vitest run --coverage",
			wantMarkers:    []string{"package.json", "yarn.lock"},
			wantConfidence: ConfidenceHigh,
		},
		{
			name:           "pnpm lockfile with the built-in runner",
			fixture:        "node-pnpm",
			wantManager:    PackageManagerPNPM,
			wantFramework:  TestFrameworkNodeTest,
			wantScript:     "node --test test/",
			wantMarkers:    []string{"package.json", "pnpm-lock.yaml"},
			wantConfidence: ConfidenceHigh,
		},
		{
			name:           "bun lockfile with mocha",
			fixture:        "node-bun",
			wantManager:    PackageManagerBun,
			wantFramework:  TestFrameworkMocha,
			wantScript:     "mocha --reporter spec",
			wantMarkers:    []string{"package.json", "bun.lock"},
			wantConfidence: ConfidenceHigh,
		},
		{
			name:           "packageManager field with no lockfile",
			fixture:        "node-corepack",
			wantManager:    PackageManagerPNPM,
			wantVersion:    "9.1.0",
			wantFramework:  TestFrameworkVitest,
			wantScript:     "cross-env NODE_ENV=test ./node_modules/.bin/vitest run",
			wantMarkers:    []string{"package.json"},
			wantConfidence: ConfidenceHigh,
			wantESLint:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Detect(filepath.Join("testdata", tt.fixture))
			if err != nil {
				t.Fatalf("Detect returned error: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("Detect returned %d projects, want 1: %+v", len(got), got)
			}

			project := got[0]
			if project.Kind != KindNode {
				t.Fatalf("Kind = %v, want %v", project.Kind, KindNode)
			}
			if project.Confidence != tt.wantConfidence {
				t.Errorf("Confidence = %v, want %v", project.Confidence, tt.wantConfidence)
			}
			if !slices.Equal(project.Markers, tt.wantMarkers) {
				t.Errorf("Markers = %v, want %v", project.Markers, tt.wantMarkers)
			}
			if len(project.Problems) != 0 {
				t.Errorf("Problems = %v, want none", project.Problems)
			}
			if project.Node == nil {
				t.Fatal("Node detail is nil")
			}
			if project.Node.PackageManager != tt.wantManager {
				t.Errorf("PackageManager = %v, want %v", project.Node.PackageManager, tt.wantManager)
			}
			if project.Node.PackageManagerVersion != tt.wantVersion {
				t.Errorf("PackageManagerVersion = %q, want %q", project.Node.PackageManagerVersion, tt.wantVersion)
			}
			if project.Node.TestFramework != tt.wantFramework {
				t.Errorf("TestFramework = %v, want %v", project.Node.TestFramework, tt.wantFramework)
			}
			if project.Node.TestScript != tt.wantScript {
				t.Errorf("TestScript = %q, want %q", project.Node.TestScript, tt.wantScript)
			}
			if project.Node.HasESLint != tt.wantESLint {
				t.Errorf("HasESLint = %v, want %v", project.Node.HasESLint, tt.wantESLint)
			}
		})
	}
}

// TestDetectNodeMalformedPackageJSON pins the degradation contract for invalid
// JSON: still a Node project, lower confidence, the parse error reported, and
// the package manager still recovered from the lockfile beside it.
func TestDetectNodeMalformedPackageJSON(t *testing.T) {
	t.Parallel()

	got, err := Detect(filepath.Join("testdata", "node-malformed"))
	if err != nil {
		t.Fatalf("Detect returned error: %v, want a degraded result", err)
	}
	if len(got) != 1 {
		t.Fatalf("Detect returned %d projects, want 1: %+v", len(got), got)
	}

	project := got[0]
	if project.Kind != KindNode {
		t.Errorf("Kind = %v, want %v", project.Kind, KindNode)
	}
	if project.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %v, want %v", project.Confidence, ConfidenceLow)
	}
	if project.Node == nil {
		t.Fatal("Node detail is nil")
	}
	if project.Node.PackageManager != PackageManagerYarn {
		t.Errorf("PackageManager = %v, want %v from yarn.lock", project.Node.PackageManager, PackageManagerYarn)
	}
	if project.Node.Name != "" || project.Node.TestScript != "" {
		t.Errorf("Node = %+v, want no fields recovered from unparsable JSON", project.Node)
	}

	if len(project.Problems) != 1 {
		t.Fatalf("Problems = %v, want exactly one", project.Problems)
	}
	problem := project.Problems[0]
	if problem.File != "package.json" {
		t.Errorf("Problem.File = %q, want %q", problem.File, "package.json")
	}
	if !errors.Is(problem.Err, ErrMalformedMarker) {
		t.Errorf("Problem.Err = %v, want it to wrap ErrMalformedMarker", problem.Err)
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(problem.Err, &syntaxErr) {
		t.Errorf("Problem.Err = %v, want it to carry the json.SyntaxError", problem.Err)
	}
}

func TestFrameworkFromScript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		script string
		want   TestFramework
	}{
		{name: "empty", script: "", want: TestFrameworkUnknown},
		{name: "bare jest", script: "jest", want: TestFrameworkJest},
		{name: "jest with flags", script: "jest --ci --coverage", want: TestFrameworkJest},
		{name: "vitest run", script: "vitest run", want: TestFrameworkVitest},
		{name: "mocha", script: "mocha --reporter spec", want: TestFrameworkMocha},
		{name: "node built-in runner", script: "node --test", want: TestFrameworkNodeTest},
		{name: "node runner with a glob", script: "node --test \"**/*.test.js\"", want: TestFrameworkNodeTest},
		{name: "node runner with a concurrency flag", script: "node --test=4", want: TestFrameworkNodeTest},
		{name: "bin path", script: "./node_modules/.bin/vitest run", want: TestFrameworkVitest},
		{name: "npx wrapper", script: "npx jest", want: TestFrameworkJest},
		{name: "env wrapper", script: "cross-env NODE_ENV=test jest", want: TestFrameworkJest},
		{name: "chained commands", script: "tsc --noEmit && vitest run", want: TestFrameworkVitest},
		{name: "config path is not an invocation", script: "echo --config=jest.config.js", want: TestFrameworkUnknown},
		{name: "test flag without node", script: "tap --test", want: TestFrameworkUnknown},
		{name: "indirection through the package manager", script: "pnpm run test:unit", want: TestFrameworkUnknown},
		{name: "placeholder script", script: "echo \"Error: no test specified\" && exit 1", want: TestFrameworkUnknown},
		{name: "first framework named wins", script: "jest && vitest", want: TestFrameworkJest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := frameworkFromScript(tt.script); got != tt.want {
				t.Errorf("frameworkFromScript(%q) = %v, want %v", tt.script, got, tt.want)
			}
		})
	}
}

func TestFrameworkForFallsBackToDependencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		script  string
		devDeps map[string]string
		deps    map[string]string
		want    TestFramework
	}{
		{
			name:    "script names nothing so devDependencies decide",
			script:  "run-tests.sh",
			devDeps: map[string]string{"vitest": "^1.6.0"},
			want:    TestFrameworkVitest,
		},
		{
			name:   "runtime dependencies are the last resort",
			script: "",
			deps:   map[string]string{"mocha": "^10.4.0"},
			want:   TestFrameworkMocha,
		},
		{
			name:    "the script outranks the dependencies",
			script:  "vitest run",
			devDeps: map[string]string{"jest": "^29.7.0"},
			want:    TestFrameworkVitest,
		},
		{
			name:    "unrelated dependencies",
			script:  "",
			devDeps: map[string]string{"typescript": "^5.5.0"},
			want:    TestFrameworkUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := frameworkFor(tt.script, tt.devDeps, tt.deps); got != tt.want {
				t.Errorf("frameworkFor(%q, %v, %v) = %v, want %v", tt.script, tt.devDeps, tt.deps, got, tt.want)
			}
		})
	}
}

func TestParsePackageManagerField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		field       string
		want        PackageManager
		wantVersion string
	}{
		{name: "empty", field: "", want: PackageManagerUnknown},
		{name: "name and version", field: "pnpm@8.6.0", want: PackageManagerPNPM, wantVersion: "8.6.0"},
		{
			name:        "with integrity hash",
			field:       "yarn@3.2.3+sha224.953c8233f7a92884eee2de69a1b92d1f2ec1655e66d08071ba9a02fa",
			want:        PackageManagerYarn,
			wantVersion: "3.2.3",
		},
		{name: "npm", field: "npm@10.8.1", want: PackageManagerNPM, wantVersion: "10.8.1"},
		{name: "bun", field: "bun@1.1.20", want: PackageManagerBun, wantVersion: "1.1.20"},
		{name: "no version", field: "pnpm", want: PackageManagerPNPM},
		{name: "surrounding whitespace", field: "  yarn@4.3.1 ", want: PackageManagerYarn, wantVersion: "4.3.1"},
		{name: "uppercase", field: "PNPM@9.0.0", want: PackageManagerPNPM, wantVersion: "9.0.0"},
		{name: "unrecognized tool", field: "rush@5.0.0", want: PackageManagerUnknown},
		{name: "the literal word unknown", field: "unknown@1.0.0", want: PackageManagerUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, version := parsePackageManagerField(tt.field)
			if got != tt.want {
				t.Errorf("parsePackageManagerField(%q) = %v, want %v", tt.field, got, tt.want)
			}
			if version != tt.wantVersion {
				t.Errorf("parsePackageManagerField(%q) version = %q, want %q", tt.field, version, tt.wantVersion)
			}
		})
	}
}

// TestDetectNodeConflictingEvidence covers the two ways a package can describe
// two different package managers at once. Both are reported, neither is fatal.
func TestDetectNodeConflictingEvidence(t *testing.T) {
	t.Parallel()

	t.Run("two lockfiles", func(t *testing.T) {
		t.Parallel()

		root := writeFiles(t, map[string]string{
			"package.json":      `{"name":"conflicted"}`,
			"yarn.lock":         "# yarn lockfile v1\n",
			"package-lock.json": `{"lockfileVersion":3}`,
		})

		got := detectOne(t, root)
		if got.Node.PackageManager != PackageManagerNPM {
			t.Errorf("PackageManager = %v, want %v: the first lockfile in canonical order wins",
				got.Node.PackageManager, PackageManagerNPM)
		}
		if want := []string{"package.json", "package-lock.json", "yarn.lock"}; !slices.Equal(got.Markers, want) {
			t.Errorf("Markers = %v, want %v", got.Markers, want)
		}
		if len(got.Problems) != 1 || !errors.Is(got.Problems[0].Err, ErrMalformedMarker) {
			t.Errorf("Problems = %v, want one malformed-marker problem", got.Problems)
		}
		if got.Confidence != ConfidenceHigh {
			t.Errorf("Confidence = %v, want %v: the package.json itself parsed", got.Confidence, ConfidenceHigh)
		}
	})

	t.Run("field disagrees with the lockfile", func(t *testing.T) {
		t.Parallel()

		root := writeFiles(t, map[string]string{
			"package.json": `{"name":"conflicted","packageManager":"pnpm@9.1.0"}`,
			"yarn.lock":    "# yarn lockfile v1\n",
		})

		got := detectOne(t, root)
		if got.Node.PackageManager != PackageManagerPNPM {
			t.Errorf("PackageManager = %v, want %v: the declared field wins",
				got.Node.PackageManager, PackageManagerPNPM)
		}
		if len(got.Problems) != 1 || !errors.Is(got.Problems[0].Err, ErrMalformedMarker) {
			t.Errorf("Problems = %v, want one malformed-marker problem", got.Problems)
		}
	})

	t.Run("no evidence at all falls back to npm", func(t *testing.T) {
		t.Parallel()

		root := writeFiles(t, map[string]string{"package.json": `{"name":"bare"}`})

		got := detectOne(t, root)
		if got.Node.PackageManager != PackageManagerNPM {
			t.Errorf("PackageManager = %v, want %v", got.Node.PackageManager, PackageManagerNPM)
		}
		if got.Node.TestFramework != TestFrameworkUnknown {
			t.Errorf("TestFramework = %v, want %v", got.Node.TestFramework, TestFrameworkUnknown)
		}
		if len(got.Problems) != 0 {
			t.Errorf("Problems = %v, want none", got.Problems)
		}
	})
}

func TestDetectNodeUnreadablePackageJSON(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := writeUnreadable(filepath.Join(root, "package.json")); err != nil {
		t.Skipf("cannot create an unreadable file here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	project := detectOne(t, root)
	if project.Kind != KindNode {
		t.Fatalf("Kind = %v, want %v", project.Kind, KindNode)
	}
	if project.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %v, want %v", project.Confidence, ConfidenceLow)
	}
	if project.Node.PackageManager != PackageManagerPNPM {
		t.Errorf("PackageManager = %v, want %v from the lockfile beside it",
			project.Node.PackageManager, PackageManagerPNPM)
	}
	if len(project.Problems) != 1 || !errors.Is(project.Problems[0].Err, fs.ErrPermission) {
		t.Errorf("Problems = %v, want one permission error", project.Problems)
	}
}

func TestDetectNodeESLintAsRuntimeDependency(t *testing.T) {
	t.Parallel()

	root := writeFiles(t, map[string]string{
		"package.json": `{"name":"plugin","dependencies":{"eslint":"^9.9.0"}}`,
	})

	if project := detectOne(t, root); !project.Node.HasESLint {
		t.Error("HasESLint = false, want true: eslint is a runtime dependency here")
	}
}
