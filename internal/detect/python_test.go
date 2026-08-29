package detect

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"
)

// TestDetectPythonProject covers one fixture per marker file, and the two
// facts the runner and linter adapters ask for.
func TestDetectPythonProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		fixture        string
		wantMarkers    []string
		wantConfidence Confidence
		wantPytest     bool
		wantRuff       bool
	}{
		{
			name:           "pyproject.toml",
			fixture:        "python-pyproject",
			wantMarkers:    []string{"pyproject.toml"},
			wantConfidence: ConfidenceHigh,
			wantPytest:     true,
			wantRuff:       true,
		},
		{
			name:           "setup.py",
			fixture:        "python-setup",
			wantMarkers:    []string{"setup.py"},
			wantConfidence: ConfidenceHigh,
			wantPytest:     true,
		},
		{
			name:           "requirements.txt is only a secondary marker",
			fixture:        "python-requirements",
			wantMarkers:    []string{"requirements.txt"},
			wantConfidence: ConfidenceMedium,
			wantPytest:     true,
			wantRuff:       true,
		},
		{
			name:           "tox.ini is only a secondary marker",
			fixture:        "python-tox",
			wantMarkers:    []string{"tox.ini"},
			wantConfidence: ConfidenceMedium,
			wantPytest:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			project := detectOne(t, filepath.Join("testdata", tt.fixture))
			if project.Kind != KindPython {
				t.Fatalf("Kind = %v, want %v", project.Kind, KindPython)
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
			if project.Python == nil {
				t.Fatal("Python detail is nil")
			}
			if project.Python.HasPytest != tt.wantPytest {
				t.Errorf("HasPytest = %v, want %v", project.Python.HasPytest, tt.wantPytest)
			}
			if project.Python.HasRuff != tt.wantRuff {
				t.Errorf("HasRuff = %v, want %v", project.Python.HasRuff, tt.wantRuff)
			}
		})
	}
}

func TestDetectPythonEvidence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		files       map[string]string
		wantMarkers []string
		wantPytest  bool
		wantRuff    bool
	}{
		{
			name: "every marker at once",
			files: map[string]string{
				"pyproject.toml":   "[project]\nname = \"all\"\n",
				"setup.py":         "from setuptools import setup\n",
				"requirements.txt": "pytest==8.2.0\n",
				"tox.ini":          "[tox]\n",
			},
			wantMarkers: []string{"pyproject.toml", "setup.py", "requirements.txt", "tox.ini"},
			wantPytest:  true,
		},
		{
			name:        "standalone ruff config",
			files:       map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n", "ruff.toml": "line-length = 100\n"},
			wantMarkers: []string{"pyproject.toml"},
			wantRuff:    true,
		},
		{
			name:        "hidden ruff config",
			files:       map[string]string{"setup.py": "setup()\n", ".ruff.toml": "line-length = 100\n"},
			wantMarkers: []string{"setup.py"},
			wantRuff:    true,
		},
		{
			name:        "commented out dependencies do not count",
			files:       map[string]string{"requirements.txt": "# pytest==8.2.0\n# ruff==0.5.5\nhttpx==0.27.0\n"},
			wantMarkers: []string{"requirements.txt"},
		},
		{
			name:        "a name embedded in another word does not count",
			files:       map[string]string{"requirements.txt": "mypytest==1.0.0\ngruffalo==2.0.0\n"},
			wantMarkers: []string{"requirements.txt"},
		},
		{
			name:        "pytest plugins imply pytest",
			files:       map[string]string{"requirements.txt": "pytest-asyncio==0.23.7\n"},
			wantMarkers: []string{"requirements.txt"},
			wantPytest:  true,
		},
		{
			name:        "empty markers",
			files:       map[string]string{"requirements.txt": ""},
			wantMarkers: []string{"requirements.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			project := detectOne(t, writeFiles(t, tt.files))
			if project.Kind != KindPython {
				t.Fatalf("Kind = %v, want %v", project.Kind, KindPython)
			}
			if !slices.Equal(project.Markers, tt.wantMarkers) {
				t.Errorf("Markers = %v, want %v", project.Markers, tt.wantMarkers)
			}
			if project.Python.HasPytest != tt.wantPytest {
				t.Errorf("HasPytest = %v, want %v", project.Python.HasPytest, tt.wantPytest)
			}
			if project.Python.HasRuff != tt.wantRuff {
				t.Errorf("HasRuff = %v, want %v", project.Python.HasRuff, tt.wantRuff)
			}
		})
	}
}

func TestDetectPythonUnreadableMarker(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := writeUnreadable(filepath.Join(root, "pyproject.toml")); err != nil {
		t.Skipf("cannot create an unreadable file here: %v", err)
	}

	project := detectOne(t, root)
	if project.Kind != KindPython {
		t.Fatalf("Kind = %v, want %v", project.Kind, KindPython)
	}
	if project.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %v, want %v", project.Confidence, ConfidenceLow)
	}
	if len(project.Problems) != 1 || !errors.Is(project.Problems[0].Err, fs.ErrPermission) {
		t.Errorf("Problems = %v, want one permission error", project.Problems)
	}
}

func TestMentionsName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "bare name", text: "pytest\n", want: true},
		{name: "pinned version", text: "pytest==8.2.0\n", want: true},
		{name: "toml table", text: "[tool.pytest.ini_options]\n", want: true},
		{name: "tox section", text: "[pytest]\n", want: true},
		{name: "quoted in a list", text: "dev = [\"pytest>=8.2\"]\n", want: true},
		{name: "hyphenated plugin", text: "pytest-cov==5.0.0\n", want: true},
		{name: "underscored plugin", text: "pytest_asyncio\n", want: true},
		{name: "indented", text: "deps =\n    pytest>=8.2\n", want: true},
		{name: "prefixed by a letter", text: "mypytest\n", want: false},
		{name: "suffixed by a letter", text: "pytestify\n", want: false},
		{name: "suffixed by a digit", text: "pytest2\n", want: false},
		{name: "commented out", text: "# pytest==8.2.0\n", want: false},
		{name: "trailing comment on another dependency", text: "httpx  # not pytest\n", want: false},
		{name: "absent", text: "[project]\nname = \"x\"\n", want: false},
		{name: "empty", text: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := mentionsName(tt.text, "pytest"); got != tt.want {
				t.Errorf("mentionsName(%q, \"pytest\") = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}
