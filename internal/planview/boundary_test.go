package planview

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// terminalLibraries are the import-path prefixes of the modules that draw
// and read a terminal. They arrived with this package and may go nowhere
// else.
var terminalLibraries = []string{"charm.land/", "github.com/charmbracelet/"}

// TestOnlyPlanviewImportsTheTerminalLibraries keeps the plan viewer's
// dependencies, and the viewer itself, where they belong.
//
// Two rules, both about the durability claim in the approve gate's package
// doc. The terminal libraries may be imported only here, so that removing
// the viewer is removing one package and nothing else starts drawing a
// terminal by accident. And this package may be imported only by the
// command layer, so that no node, no part of the dispatcher and nothing in
// pkg/belay can open a viewer and wait on a person: a gate that blocked on a
// terminal would make a paused run die with the session that started it.
//
// It reads every .go file in the module, tests and build-tagged files
// included, rather than asking `go list`, which would skip the files a build
// tag hides. The module's fixture projects are modules of their own and are
// left out.
func TestOnlyPlanviewImportsTheTerminalLibraries(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s is not the module root: %v", root, err)
	}
	const module = "github.com/dhaam-ai/belay"
	self := filepath.Join(root, "internal", "planview")
	importers := []string{filepath.Join(root, "internal", "cli"), self}

	importsTea := false
	fset := token.NewFileSet()
	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir // a module of its own, such as a fixture
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("parse %s: %v", path, err)
			return nil
		}
		dir := filepath.Dir(path)
		rel, _ := filepath.Rel(root, path) //nolint:errcheck // display only; path is under root
		for _, spec := range file.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Errorf("%s: unreadable import %s", rel, spec.Path.Value)
				continue
			}
			for _, lib := range terminalLibraries {
				if strings.HasPrefix(imp, lib) && dir != self {
					t.Errorf("%s imports %s; only internal/planview may import a terminal library", rel, imp)
				}
			}
			if imp == module+"/internal/planview" && !slices.Contains(importers, dir) {
				t.Errorf("%s imports internal/planview; only the command layer may open the viewer, "+
					"never a node, the dispatcher or pkg/belay", rel)
			}
			if dir == self && !strings.HasSuffix(path, "_test.go") && imp == "charm.land/bubbletea/v2" {
				importsTea = true
			}
		}
		return nil
	}
	if err := filepath.WalkDir(root, walk); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if !importsTea {
		t.Error("internal/planview imports no charm.land/bubbletea/v2, so the rules above hold vacuously")
	}
}
