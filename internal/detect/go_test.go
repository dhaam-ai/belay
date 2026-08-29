package detect

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"
)

func TestDetectGoModule(t *testing.T) {
	t.Parallel()

	root := filepath.Join("testdata", "go-simple")
	got, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Detect returned %d projects, want 1: %+v", len(got), got)
	}

	project := got[0]
	if project.Kind != KindGo {
		t.Errorf("Kind = %v, want %v", project.Kind, KindGo)
	}
	if project.Confidence != ConfidenceHigh {
		t.Errorf("Confidence = %v, want %v", project.Confidence, ConfidenceHigh)
	}
	if project.Root != root {
		t.Errorf("Root = %q, want %q", project.Root, root)
	}
	if want := []string{"go.mod"}; !slices.Equal(project.Markers, want) {
		t.Errorf("Markers = %v, want %v", project.Markers, want)
	}
	if len(project.Problems) != 0 {
		t.Errorf("Problems = %v, want none", project.Problems)
	}
	if project.Go == nil {
		t.Fatal("Go detail is nil")
	}
	if want := "github.com/belay-dev/fixture-service"; project.Go.Module != want {
		t.Errorf("Module = %q, want %q", project.Go.Module, want)
	}
	if want := "1.26.3"; project.Go.GoVersion != want {
		t.Errorf("GoVersion = %q, want %q", project.Go.GoVersion, want)
	}
}

// TestDetectGoModuleWithoutModuleLine pins the degradation contract: a marker
// that cannot be parsed still proves the kind, at a lower confidence, with the
// reason reported.
func TestDetectGoModuleWithoutModuleLine(t *testing.T) {
	t.Parallel()

	got, err := Detect(filepath.Join("testdata", "go-malformed"))
	if err != nil {
		t.Fatalf("Detect returned error: %v, want a degraded result", err)
	}
	if len(got) != 1 {
		t.Fatalf("Detect returned %d projects, want 1: %+v", len(got), got)
	}

	project := got[0]
	if project.Kind != KindGo {
		t.Errorf("Kind = %v, want %v", project.Kind, KindGo)
	}
	if project.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %v, want %v", project.Confidence, ConfidenceLow)
	}
	if project.Go == nil || project.Go.Module != "" {
		t.Errorf("Go = %+v, want an empty module path", project.Go)
	}
	if want := "1.24"; project.Go.GoVersion != want {
		t.Errorf("GoVersion = %q, want %q: the readable half of the file still counts", project.Go.GoVersion, want)
	}
	if len(project.Problems) != 1 {
		t.Fatalf("Problems = %v, want exactly one", project.Problems)
	}
	problem := project.Problems[0]
	if problem.File != "go.mod" {
		t.Errorf("Problem.File = %q, want %q", problem.File, "go.mod")
	}
	if !errors.Is(problem.Err, ErrMalformedMarker) {
		t.Errorf("Problem.Err = %v, want it to wrap ErrMalformedMarker", problem.Err)
	}
	if problem.String() == "" {
		t.Error("Problem.String() = empty")
	}
}

func TestParseGoMod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		content     string
		wantModule  string
		wantVersion string
	}{
		{
			name:        "canonical",
			content:     "module example.com/m\n\ngo 1.26.3\n",
			wantModule:  "example.com/m",
			wantVersion: "1.26.3",
		},
		{
			name:        "no trailing newline",
			content:     "module example.com/m\ngo 1.22",
			wantModule:  "example.com/m",
			wantVersion: "1.22",
		},
		{
			name:        "windows line endings",
			content:     "module example.com/m\r\n\r\ngo 1.22\r\n",
			wantModule:  "example.com/m",
			wantVersion: "1.22",
		},
		{
			name:        "leading tabs and extra spaces",
			content:     "\tmodule   example.com/m\n\tgo\t1.22\n",
			wantModule:  "example.com/m",
			wantVersion: "1.22",
		},
		{
			name:        "quoted operands",
			content:     "module \"example.com/m\"\ngo \"1.22\"\n",
			wantModule:  "example.com/m",
			wantVersion: "1.22",
		},
		{
			name:        "block form",
			content:     "module (\n\texample.com/m\n)\n\ngo 1.22\n",
			wantModule:  "example.com/m",
			wantVersion: "1.22",
		},
		{
			name:        "deprecation comment above the directive",
			content:     "// Deprecated: use example.com/m/v2\nmodule example.com/m // the real one\ngo 1.22\n",
			wantModule:  "example.com/m",
			wantVersion: "1.22",
		},
		{
			name:        "commented out module directive",
			content:     "// module example.com/m\ngo 1.22\n",
			wantModule:  "",
			wantVersion: "1.22",
		},
		{
			name:        "toolchain and require lines are ignored",
			content:     "module example.com/m\n\ngo 1.22\n\ntoolchain go1.26.3\n\nrequire example.com/dep v1.0.0\n",
			wantModule:  "example.com/m",
			wantVersion: "1.22",
		},
		{
			name:        "first module directive wins",
			content:     "module example.com/first\nmodule example.com/second\ngo 1.22\n",
			wantModule:  "example.com/first",
			wantVersion: "1.22",
		},
		{
			name:        "rc version",
			content:     "module example.com/m\ngo 1.21rc1\n",
			wantModule:  "example.com/m",
			wantVersion: "1.21rc1",
		},
		{
			name:        "empty file",
			content:     "",
			wantModule:  "",
			wantVersion: "",
		},
		{
			name:        "not a go.mod at all",
			content:     "{\n  \"name\": \"confused\"\n}\n",
			wantModule:  "",
			wantVersion: "",
		},
		{
			name:        "bare module keyword",
			content:     "module\ngo 1.22\n",
			wantModule:  "",
			wantVersion: "1.22",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			module, version := parseGoMod(tt.content)
			if module != tt.wantModule {
				t.Errorf("module = %q, want %q", module, tt.wantModule)
			}
			if version != tt.wantVersion {
				t.Errorf("go version = %q, want %q", version, tt.wantVersion)
			}
		})
	}
}

// TestDetectGoModUnreadable covers the other degradation path: the marker is
// there but cannot be read.
func TestDetectGoModUnreadable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := writeUnreadable(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("cannot create an unreadable file here: %v", err)
	}

	got, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect returned error: %v, want a degraded result", err)
	}
	if len(got) != 1 || got[0].Kind != KindGo {
		t.Fatalf("Detect = %+v, want one Go project", got)
	}
	if got[0].Confidence != ConfidenceLow {
		t.Errorf("Confidence = %v, want %v", got[0].Confidence, ConfidenceLow)
	}
	if len(got[0].Problems) != 1 || !errors.Is(got[0].Problems[0].Err, fs.ErrPermission) {
		t.Errorf("Problems = %v, want one permission error", got[0].Problems)
	}
}

func TestUnquoteToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "bare", input: "example.com/m", want: "example.com/m"},
		{name: "double quoted", input: `"example.com/m"`, want: "example.com/m"},
		{name: "back quoted", input: "`example.com/m`", want: "example.com/m"},
		{name: "unterminated quote", input: `"example.com/m`, want: "example.com/m"},
		{name: "invalid escape", input: `"example.com\qm"`, want: `example.com\qm`},
		{name: "empty", input: "", want: ""},
		{name: "single character", input: `"`, want: `"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := unquoteToken(tt.input); got != tt.want {
				t.Errorf("unquoteToken(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
