//go:build unix

package lintgate

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// platformTags are build tags the toolchain itself satisfies for the machine
// the linter runs on. Listing one in .golangci.yml would say nothing:
// golangci-lint already builds for the host.
var platformTags = map[string]bool{
	"unix": true, "linux": true, "darwin": true, "windows": true,
	"amd64": true, "arm64": true, "cgo": true,
}

// TestGolangciLintSeesEverySuiteBehindABuildTag pins that belay's own
// .golangci.yml names every build tag its test suites are written behind.
//
// A tag the configuration does not name is a tag golangci-lint never parses:
// the files are not merely unchecked by one linter, they are outside the build
// it analyses, so nothing at all is reported about them — not even that gofmt
// would change them. The e2e and fix-rate suites sat that way from the day
// they were written.
//
// The tags are discovered from the suites rather than listed here, so a suite
// added behind a new tag fails this test instead of quietly going unlinted.
func TestGolangciLintSeesEverySuiteBehindABuildTag(t *testing.T) {
	root := filepath.Join("..", "..")

	var cfg struct {
		Run struct {
			BuildTags []string `yaml:"build-tags"`
		} `yaml:"run"`
	}
	raw, err := os.ReadFile(filepath.Join(root, ".golangci.yml")) //nolint:gosec // this repository's own config, at a fixed relative path
	if err != nil {
		t.Fatalf("read .golangci.yml: %v", err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode .golangci.yml: %v", err)
	}

	for _, tag := range suiteBuildTags(t, filepath.Join(root, "test")) {
		if !slices.Contains(cfg.Run.BuildTags, tag) {
			t.Errorf("the %q suite is behind a build tag .golangci.yml does not list, so golangci-lint "+
				"never reads a line of it; add %q to run.build-tags", tag, tag)
		}
	}
}

// suiteBuildTags returns every non-platform build tag the Go files under dir
// are written behind, sorted and each once.
//
// It reads the constraint as a set of names rather than evaluating the
// expression: what matters here is which tags have to be switched on for the
// linter to see the file at all, and a suite guards itself with a single tag.
func suiteBuildTags(t *testing.T, dir string) []string {
	t.Helper()
	var tags []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // a path from walking this repository's own test tree
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "//go:build ") {
				// Build constraints come before the package clause, so the
				// first one that is not a comment ends the search.
				if line != "" && !strings.HasPrefix(line, "//") {
					break
				}
				continue
			}
			for _, name := range strings.FieldsFunc(strings.TrimPrefix(line, "//go:build "),
				func(r rune) bool { return !isTagRune(r) }) {
				if name == "" || platformTags[name] || slices.Contains(tags, name) {
					continue
				}
				tags = append(tags, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	slices.Sort(tags)
	return tags
}

// isTagRune reports whether r can appear in a build tag name, so that the
// operators around the names (&&, ||, !, parentheses) split them apart.
func isTagRune(r rune) bool {
	return r == '.' || r == '_' || r == '/' ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
