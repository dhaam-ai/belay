package approve_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/nodes/approve"
)

// runTimeout is generous on purpose. The failure this guards against is an
// unbounded wait on a terminal, so anything short of "never" is a pass; a
// large bound just keeps a loaded CI machine from flaking.
const runTimeout = 30 * time.Second

// TestNeverBlocksOnStdin is the test the whole node exists to satisfy.
//
// belay's durability claim is that a paused run survives the session that
// started it: the human may resume tomorrow, from another machine, after a
// reboot. A gate that prompted would break that claim — the run would die
// with the SSH connection — so this node must reach its verdict with no
// terminal at all.
//
// Each case replaces os.Stdin with something that would punish a read. The
// hostile case is the open pipe: nothing is ever written to it and the write
// end stays open, so a single Read on it blocks forever. If the gate ever
// grows a prompt, this test hangs and then fails on the timeout.
func TestNeverBlocksOnStdin(t *testing.T) {
	tests := []struct {
		name  string
		stdin func(t *testing.T) *os.File
	}{
		{
			name: "stdin closed",
			stdin: func(t *testing.T) *os.File {
				t.Helper()
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatalf("os.Pipe: %v", err)
				}
				mustClose(t, w)
				mustClose(t, r)
				return r
			},
		},
		{
			name: "stdin at EOF",
			stdin: func(t *testing.T) *os.File {
				t.Helper()
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatalf("os.Pipe: %v", err)
				}
				mustClose(t, w)
				t.Cleanup(func() { _ = r.Close() })
				return r
			},
		},
		{
			name: "stdin is an open pipe nobody ever writes to (a read would block forever)",
			stdin: func(t *testing.T) *os.File {
				t.Helper()
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatalf("os.Pipe: %v", err)
				}
				t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
				return r
			},
		},
		{
			name:  "no stdin at all",
			stdin: func(*testing.T) *os.File { return nil },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// os.Stdin is process-global; these cases must not run in
			// parallel with each other.
			saved := os.Stdin
			t.Cleanup(func() { os.Stdin = saved })
			os.Stdin = tt.stdin(t)

			rc := newRC(t, planBody)

			type outcome struct {
				res graph.Result
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				res, err := approve.New().Run(context.Background(), rc)
				done <- outcome{res, err}
			}()

			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("Run: %v", got.err)
				}
				if want := journal.StatusPaused; got.res.Status != want {
					t.Errorf("Status = %v, want %v", got.res.Status, want)
				}
			case <-time.After(runTimeout):
				t.Fatalf("Run did not return within %s with stdin unreadable: the gate is blocking on a terminal, "+
					"which would make a paused run die with the session that started it", runTimeout)
			}
		})
	}
}

func mustClose(t *testing.T, f *os.File) {
	t.Helper()
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSourceNeverReferencesATerminal is the static half of the guarantee.
//
// The behavioural test above proves this build did not block. This one
// proves the package cannot start: it fails the moment someone adds an
// os.Stdin read, a fmt.Scan, or a terminal library, even if that code sits
// behind a branch no test happens to reach.
func TestSourceNeverReferencesATerminal(t *testing.T) {
	banned := []string{
		"os.Stdin",
		"fmt.Scan", "fmt.Scanln", "fmt.Scanf", "fmt.Fscan",
		"bufio.NewScanner", "bufio.NewReader",
		"golang.org/x/term", "term.ReadPassword", "term.IsTerminal",
		"survey.", "promptui.",
	}

	// go/parser.ParseDir is deprecated as of Go 1.25 and its suggested
	// replacement (golang.org/x/tools/go/packages) is not a dependency this
	// module carries, so the directory is walked directly.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	parsed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		parsed++

		// Comments legitimately mention os.Stdin to explain the rule, so
		// inspect the syntax tree rather than the raw bytes.
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if expr := ident.Name + "." + sel.Sel.Name; slices.Contains(banned, expr) {
				t.Errorf("%s references %s: the approve gate must never read a terminal", name, expr)
			}
			return true
		})

		for _, imp := range file.Imports {
			for _, b := range banned {
				if strings.Contains(imp.Path.Value, b) {
					t.Errorf("%s imports %s: the approve gate must never read a terminal", name, imp.Path.Value)
				}
			}
		}
	}

	if parsed == 0 {
		t.Fatal("parsed no source files; the guard would pass vacuously")
	}
}
