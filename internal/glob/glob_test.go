package glob_test

import (
	"errors"
	"testing"

	"github.com/dhaam-ai/belay/internal/glob"
)

func TestCompileRefusesPatternsNobodyCouldHaveMeant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pattern string
	}{
		{name: "empty", pattern: ""},
		{name: "blank", pattern: "   "},
		{name: "absolute", pattern: "/etc/passwd"},
		{name: "escapes the workspace", pattern: "../secrets/**"},
		{name: "dot dot in the middle", pattern: "src/../etc"},
		{name: "unclosed character class", pattern: "src/[a-.go"},
		{name: "a doubled star glued to other characters", pattern: "src**/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := glob.Compile(tt.pattern); err == nil {
				t.Fatalf("glob.Compile(%q) = nil error; a pattern belay cannot honour must be refused where it is written, not silently matched against nothing", tt.pattern)
			} else if !errors.Is(err, glob.ErrBadPattern) {
				t.Fatalf("glob.Compile(%q) error = %v; want one wrapping glob.ErrBadPattern", tt.pattern, err)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{name: "an exact path", pattern: "go.mod", path: "go.mod", want: true},
		{name: "a different path", pattern: "go.mod", path: "go.sum", want: false},
		{name: "a star stays inside one segment", pattern: "*.go", path: "main.go", want: true},
		{name: "a star does not cross a separator", pattern: "*.go", path: "internal/main.go", want: false},
		{name: "a doubled star crosses separators", pattern: "**/*.go", path: "internal/nodes/main.go", want: true},
		{name: "a doubled star matches no segments at all", pattern: "**/*.go", path: "main.go", want: true},
		{name: "a directory prefix", pattern: "infra/**", path: "infra/aws/main.tf", want: true},
		{name: "a directory prefix matches the directory itself", pattern: "infra/**", path: "infra", want: true},
		{name: "a directory prefix does not match a sibling", pattern: "infra/**", path: "infrastructure/main.tf", want: false},
		{name: "a doubled star in the middle", pattern: "src/**/*_test.go", path: "src/a/b/x_test.go", want: true},
		{name: "a question mark is one character", pattern: "v?.go", path: "v1.go", want: true},
		{name: "a question mark is not two", pattern: "v?.go", path: "v10.go", want: false},
		{name: "a character class", pattern: "v[0-9].go", path: "v7.go", want: true},
		{name: "a leading ./ is the same path", pattern: "./go.mod", path: "go.mod", want: true},
		{name: "the path is cleaned before matching", pattern: "src/main.go", path: "./src/main.go", want: true},
		{name: "a trailing slash on the path", pattern: "infra", path: "infra/", want: true},
		{name: "a pattern is anchored at the workspace root", pattern: "workflows/*", path: ".github/workflows/ci.yml", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, err := glob.Compile(tt.pattern)
			if err != nil {
				t.Fatalf("glob.Compile(%q): %v", tt.pattern, err)
			}
			if got := p.Match(tt.path); got != tt.want {
				t.Errorf("glob.Compile(%q).Match(%q) = %t, want %t", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

func TestPatternStringIsWhatWasWritten(t *testing.T) {
	t.Parallel()

	p, err := glob.Compile("infra/**")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := p.String(); got != "infra/**" {
		t.Errorf("Pattern.String() = %q, want %q: a refusal quotes the pattern the person wrote", got, "infra/**")
	}
}

func TestMatchAnyReportsTheFirstPatternThatMatched(t *testing.T) {
	t.Parallel()

	set, err := glob.CompileAll([]string{"docs/**", "**/*.tf"})
	if err != nil {
		t.Fatalf("CompileAll: %v", err)
	}
	got, ok := set.Match("infra/aws/main.tf")
	if !ok {
		t.Fatalf("Set.Match(%q) = _, false; want the second pattern to match", "infra/aws/main.tf")
	}
	if got.String() != "**/*.tf" {
		t.Errorf("Set.Match matched %q, want %q: a violation must name the rule that fired", got, "**/*.tf")
	}
	if _, ok := set.Match("internal/main.go"); ok {
		t.Errorf("Set.Match(%q) matched; want no match", "internal/main.go")
	}
}

// A folded pattern matches without regard to case, for a disk on which two
// names that differ only in case are one file. It quotes the pattern as it
// was written, because a violation quotes it back to the person who wrote
// it.
func TestAFoldedPatternMatchesAnyCase(t *testing.T) {
	t.Parallel()

	set, err := glob.CompileAll([]string{"infra/**", "**/Secrets.YAML", "src/[A-C]*.go"})
	if err != nil {
		t.Fatalf("CompileAll: %v", err)
	}
	folded := set.Fold()
	for _, tc := range []struct {
		name        string
		exact, fold bool
		quoted      string
	}{
		{name: "INFRA/evil.tf", exact: false, fold: true, quoted: "infra/**"},
		{name: "infra/main.tf", exact: true, fold: true, quoted: "infra/**"},
		{name: "config/secrets.yaml", exact: false, fold: true, quoted: "**/Secrets.YAML"},
		{name: "SRC/b.GO", exact: false, fold: true, quoted: "src/[A-C]*.go"},
		{name: "src/d.go", exact: false, fold: false},
		{name: "infrastructure/x", exact: false, fold: false},
	} {
		if _, hit := set.Match(tc.name); hit != tc.exact {
			t.Errorf("Match(%q) = %v, want %v", tc.name, hit, tc.exact)
		}
		p, hit := folded.Match(tc.name)
		if hit != tc.fold {
			t.Errorf("folded Match(%q) = %v, want %v", tc.name, hit, tc.fold)
		}
		if hit && p.String() != tc.quoted {
			t.Errorf("folded Match(%q) quotes %q, want the pattern as written, %q", tc.name, p.String(), tc.quoted)
		}
	}
	if set.Len() != folded.Len() {
		t.Errorf("Fold changed the number of patterns: %d, want %d", folded.Len(), set.Len())
	}
}
