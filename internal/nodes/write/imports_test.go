package write

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// allowedImports is everything this package's non-test source is permitted to
// import.
//
// The list is the enforcement mechanism for a claim the package documentation
// makes: the write node performs no network access, starts no process, and
// invokes no agent. Those are properties of its import graph, so they are
// asserted against the import graph rather than argued in prose. Nothing here
// can reach net/http, os/exec, internal/exec, an agent adapter, or a
// container runtime — and adding one becomes a deliberate act that fails this
// test rather than a quiet line at the top of a file.
var allowedImports = map[string]bool{
	"bytes":         true,
	"context":       true,
	"errors":        true,
	"fmt":           true,
	"io":            true,
	"io/fs":         true,
	"log/slog":      true,
	"os":            true,
	"path/filepath": true,
	"sort":          true,
	"strings":       true,
	"unicode":       true,
	"unicode/utf8":  true,
	// syscall is permitted for the Errno type alone — it is how an os.Root
	// containment refusal is told apart from a kernel failure, since the
	// refusal is the only error of that shape that is not an Errno. The
	// package is otherwise capable of exec and sockets, so
	// TestSyscallIsUsedOnlyForErrno pins the usage down to that one name.
	"syscall": true,
	"github.com/dhaam-ai/belay/internal/graph":   true,
	"github.com/dhaam-ai/belay/internal/journal": true,
	"github.com/dhaam-ai/belay/internal/state":   true,
}

// sourceFiles returns every non-test .go file in the package directory.
//
// The files are enumerated directly rather than through a package loader, so
// a file excluded from this build by a //go:build constraint is still
// checked. A guard that only inspected the current platform's files would
// have nothing to say about an import added under a tag, which is precisely
// where an unreviewed one would be easiest to miss.
func sourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("found no non-test source files; the guards below would pass vacuously")
	}
	return out
}

// parseSources parses every source file with the given mode.
func parseSources(t *testing.T, mode parser.Mode) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, name := range sourceFiles(t) {
		file, err := parser.ParseFile(fset, name, nil, mode)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = file
	}
	return files
}

func TestPackageImportsNothingThatCanCallOut(t *testing.T) {
	var offenders []string
	for name, file := range parseSources(t, parser.ImportsOnly) {
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote %s: %v", name, imp.Path.Value, err)
			}
			if !allowedImports[path] {
				offenders = append(offenders, name+": "+path)
			}
		}
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("disallowed import %s\n\tthe write node must not be able to reach the network, "+
			"a subprocess, or an agent backend; if this import is genuinely needed, "+
			"add it to allowedImports and say why in the package documentation", o)
	}
}

// syscallAllowedNames are the syscall identifiers this package may use.
//
// The import allowlist alone would not be enough here: unlike every other
// entry on it, syscall can start a process and open a socket. Pinning the
// usage to a single type keeps the guarantee the package documentation makes
// — no network, no subprocess — true rather than merely likely.
var syscallAllowedNames = map[string]bool{"Errno": true}

func TestSyscallIsUsedOnlyForErrno(t *testing.T) {
	found := 0
	for name, file := range parseSources(t, 0) {
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "syscall" {
				return true
			}
			found++
			if !syscallAllowedNames[sel.Sel.Name] {
				t.Errorf("%s: syscall.%s is not permitted; this package may reference "+
					"syscall.Errno and nothing else, because anything more would let it "+
					"start a process or open a socket", name, sel.Sel.Name)
			}
			return true
		})
	}
	if found == 0 {
		t.Error("found no syscall usage; either the allowlist entry is stale or this guard stopped looking")
	}
}
