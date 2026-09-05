package write

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/belay-dev/belay/internal/state"
)

// escapeTarget is the sentinel content an escape test would write if the gate
// let it through, and the name of the file it would land in.
const escapeTargetName = "ESCAPED"

// newTree builds a workspace root with a sibling directory next to it, so a
// test can aim at a real place outside the tree rather than at a path that
// merely looks like one.
//
// It returns the resolved workspace root and the resolved sibling root. Both
// come back symlink-resolved because macOS puts t.TempDir under /var, which
// is itself a symlink to /private/var: comparing an unresolved root against a
// resolved path is a false negative waiting to happen.
func newTree(t *testing.T) (root, sibling string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "ws")
	sibling = filepath.Join(base, "outside")
	for _, d := range []string{root, sibling, filepath.Join(root, "src")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	resolvedRoot, err := resolveRoot(root)
	if err != nil {
		t.Fatalf("resolveRoot(%s): %v", root, err)
	}
	resolvedSibling, err := resolveRoot(sibling)
	if err != nil {
		t.Fatalf("resolveRoot(%s): %v", sibling, err)
	}
	return resolvedRoot, resolvedSibling
}

func TestSafeJoinRefusesEscapes(t *testing.T) {
	root, sibling := newTree(t)

	// A directory symlink inside the workspace pointing at the sibling tree.
	// Anything under it is lexically innocent and materially outside.
	if err := os.Symlink(sibling, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// A file symlink inside the workspace pointing at a file outside it.
	outsideFile := filepath.Join(sibling, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// The "/a/bc" vs "/a/b" case, materialised: a real sibling directory
	// whose absolute path shares a string prefix with the workspace root but
	// no path prefix. A naive strings.HasPrefix containment check calls this
	// contained; segment-wise comparison does not.
	prefixSibling := root + "-evil"
	if err := os.MkdirAll(prefixSibling, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", prefixSibling, err)
	}

	tests := []struct {
		name string
		rel  string
		// target is the absolute path the claim would have reached had the
		// gate let it through. The test asserts it was not created.
		target string
	}{
		{
			name:   "parent traversal",
			rel:    "../" + escapeTargetName,
			target: filepath.Join(filepath.Dir(root), escapeTargetName),
		},
		{
			name:   "parent traversal mid-path",
			rel:    "src/../../" + escapeTargetName,
			target: filepath.Join(filepath.Dir(root), escapeTargetName),
		},
		{
			name:   "deep parent traversal",
			rel:    "../../../../../../../../etc/" + escapeTargetName,
			target: filepath.Join("/etc", escapeTargetName),
		},
		{
			name:   "absolute path",
			rel:    filepath.Join(sibling, escapeTargetName),
			target: filepath.Join(sibling, escapeTargetName),
		},
		{
			name:   "absolute path at root",
			rel:    "/" + escapeTargetName,
			target: "/" + escapeTargetName,
		},
		{
			name:   "symlinked directory out of the tree",
			rel:    "link/" + escapeTargetName,
			target: filepath.Join(sibling, escapeTargetName),
		},
		{
			name:   "symlinked file out of the tree",
			rel:    "leak.txt",
			target: filepath.Join(root, "leak.txt"),
		},
		{
			name:   "sibling sharing a string prefix with the root",
			rel:    "../" + filepath.Base(prefixSibling) + "/" + escapeTargetName,
			target: filepath.Join(prefixSibling, escapeTargetName),
		},
		{
			name:   "belay's own run directory",
			rel:    ".belay/runs/x/state.json",
			target: filepath.Join(root, ".belay", "runs", "x", "state.json"),
		},
		{
			name:   "belay directory itself",
			rel:    ".belay",
			target: filepath.Join(root, ".belay"),
		},
		{
			name:   "empty path",
			rel:    "",
			target: root,
		},
		{
			name:   "whitespace only",
			rel:    "   ",
			target: filepath.Join(root, "   "),
		},
		{
			name:   "NUL byte",
			rel:    "src/a\x00b",
			target: filepath.Join(root, "src", "a\x00b"),
		},
		{
			name:   "backslash separator",
			rel:    `..\` + escapeTargetName,
			target: filepath.Join(filepath.Dir(root), escapeTargetName),
		},
		{
			name:   "current directory",
			rel:    ".",
			target: root,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeJoin(root, tc.rel)
			if err == nil {
				t.Fatalf("safeJoin(%q, %q) = %q, want a refusal", root, tc.rel, got)
			}
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("safeJoin(%q) error = %v; want one wrapping ErrRefused", tc.rel, err)
			}
			var ce *ContainmentError
			if !errors.As(err, &ce) {
				t.Fatalf("safeJoin(%q) error = %v; want a *ContainmentError", tc.rel, err)
			}
			if ce.Path != tc.rel {
				t.Errorf("ContainmentError.Path = %q, want %q", ce.Path, tc.rel)
			}
			if ce.Reason == "" {
				t.Error("ContainmentError.Reason is empty; a refusal must say why")
			}
			assertNotCreated(t, tc.target)
		})
	}
}

// assertNotCreated fails if path exists, unless it is one of the directories
// the fixture deliberately created — the point is that the refusal did not
// materialise anything new.
func assertNotCreated(t *testing.T, path string) {
	t.Helper()
	if strings.HasSuffix(path, escapeTargetName) || strings.HasSuffix(path, "state.json") {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused path %q exists after the refusal (err=%v); nothing may be created on a refusal", path, err)
		}
	}
}

func TestSafeJoinAcceptsContainedPaths(t *testing.T) {
	root, _ := newTree(t)
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name string
		rel  string
		want string
	}{
		{name: "existing file", rel: "src/main.go", want: filepath.Join(root, "src", "main.go")},
		{name: "missing file is a deletion, not an escape", rel: "src/gone.go", want: filepath.Join(root, "src", "gone.go")},
		{name: "missing file under a missing directory", rel: "a/b/c.go", want: filepath.Join(root, "a", "b", "c.go")},
		{name: "dot slash prefix is cleaned", rel: "./src/main.go", want: filepath.Join(root, "src", "main.go")},
		{name: "doubled separators are cleaned", rel: "src//main.go", want: filepath.Join(root, "src", "main.go")},
		{name: "dot segment mid-path is cleaned", rel: "src/./main.go", want: filepath.Join(root, "src", "main.go")},
		{name: "a name merely starting with a dot-dot prefix", rel: "..data/x.go", want: filepath.Join(root, "..data", "x.go")},
		{name: "a name containing .belay as a prefix", rel: ".belayrc", want: filepath.Join(root, ".belayrc")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeJoin(root, tc.rel)
			if err != nil {
				t.Fatalf("safeJoin(%q) = %v, want it accepted", tc.rel, err)
			}
			if got != tc.want {
				t.Errorf("safeJoin(%q) = %q, want %q", tc.rel, got, tc.want)
			}
		})
	}
}

func TestIsWithin(t *testing.T) {
	tests := []struct {
		name string
		root string
		path string
		want bool
	}{
		{name: "child", root: "/a/b", path: "/a/b/c", want: true},
		{name: "grandchild", root: "/a/b", path: "/a/b/c/d", want: true},
		{name: "string prefix sibling is not inside", root: "/a/b", path: "/a/bc", want: false},
		{name: "string prefix sibling with children", root: "/a/b", path: "/a/bc/d", want: false},
		{name: "root is not inside itself", root: "/a/b", path: "/a/b", want: false},
		{name: "parent is not inside child", root: "/a/b/c", path: "/a/b", want: false},
		{name: "unrelated", root: "/a/b", path: "/x/y", want: false},
		{name: "relative root", root: "a/b", path: "/a/b/c", want: false},
		{name: "relative path", root: "/a/b", path: "a/b/c", want: false},
		{name: "empty root", root: "", path: "/a", want: false},
		{name: "empty path", root: "/a", path: "", want: false},
		{name: "uncleaned but contained", root: "/a/b", path: "/a/b/./c", want: true},
		{name: "uncleaned and escaping", root: "/a/b", path: "/a/b/../c", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWithin(tc.root, tc.path); got != tc.want {
				t.Errorf("isWithin(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
			}
		})
	}
}

func TestResolveRootRejectsUnusable(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name string
		dir  string
	}{
		{name: "empty", dir: ""},
		{name: "whitespace", dir: "   "},
		{name: "NUL byte", dir: filepath.Join(base, "a\x00b")},
		{name: "missing", dir: filepath.Join(base, "nope")},
		{name: "not a directory", dir: file},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRoot(tc.dir)
			if err == nil {
				t.Fatalf("resolveRoot(%q) = %q, want an error", tc.dir, got)
			}
			if !errors.Is(err, ErrWorkspace) {
				t.Fatalf("resolveRoot(%q) error = %v; want one wrapping ErrWorkspace", tc.dir, err)
			}
			var we *WorkspaceError
			if !errors.As(err, &we) {
				t.Fatalf("resolveRoot(%q) error = %v; want a *WorkspaceError", tc.dir, err)
			}
		})
	}
}

// The workspace now arrives on the RunContext rather than being reconstructed
// from the run directory's shape, and Layout stores it rather than discarding
// it -- so the two must still agree, and an unset value must refuse rather
// than resolve to whatever directory belay is standing in.
func TestWorkspaceComesFromTheContext(t *testing.T) {
	ws := t.TempDir()
	resolved, err := resolveRoot(ws)
	if err != nil {
		t.Fatalf("resolveRoot: %v", err)
	}
	layout, err := state.NewLayout(ws, "20260101T000000Z-abcdef123456")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if got := layout.WorkspaceDir(); got != ws {
		t.Errorf("Layout.WorkspaceDir() = %q, want %q", got, ws)
	}
	got, err := resolveRoot(layout.WorkspaceDir())
	if err != nil {
		t.Fatalf("resolveRoot(WorkspaceDir): %v", err)
	}
	if got != resolved {
		t.Errorf("resolved workspace = %q, want %q", got, resolved)
	}
}

func TestUnsetWorkspaceIsRefused(t *testing.T) {
	if _, err := resolveRoot(""); err == nil {
		t.Fatal("resolveRoot(\"\") = nil error, want ErrWorkspace")
	} else if !errors.Is(err, ErrWorkspace) {
		t.Fatalf("resolveRoot(\"\") error = %v; want one wrapping ErrWorkspace", err)
	}
}

// TestOsRootRefusesEscapesIndependently proves the second containment layer
// holds on its own.
//
// It bypasses safeJoin entirely and hands classify and readCapped the very
// paths a lexical bug would have let through. Everything is still refused,
// this time by the kernel, which is the point: the guarantee does not rest on
// safeJoin being right. It is also the only defence against the window
// safeJoin cannot close — the workspace belongs to an agent process that may
// still be running, and a path checked as a regular file can become a symlink
// out of the tree before it is read.
func TestOsRootRefusesEscapesIndependently(t *testing.T) {
	root, sibling := newTree(t)
	outsideFile := filepath.Join(sibling, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink(sibling, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	osRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = osRoot.Close() })

	escapes := []string{
		"../outside/secret.txt",
		"link/secret.txt",
		"leak.txt/../secret.txt",
	}
	for _, rel := range escapes {
		t.Run("classify "+rel, func(t *testing.T) {
			got, cerr := classify(osRoot, root, rel, filepath.Join(root, rel), rel)
			if cerr == nil {
				t.Fatalf("classify(%q) = %+v, want a refusal from the Root", rel, got)
			}
			if !errors.Is(cerr, ErrRefused) {
				t.Fatalf("classify(%q) error = %v; want one wrapping ErrRefused", rel, cerr)
			}
		})
		t.Run("readCapped "+rel, func(t *testing.T) {
			content, _, rerr := readCapped(osRoot, rel, DefaultMaxFileBytes)
			if rerr == nil {
				t.Fatalf("readCapped(%q) = %q, want a refusal from the Root", rel, content)
			}
			if strings.Contains(string(content), "secret") {
				t.Fatalf("readCapped(%q) leaked content from outside the workspace", rel)
			}
		})
	}

	// The same Root reads a contained file without complaint, so the refusals
	// above are about containment and not about the Root being unusable.
	mustWrite(t, filepath.Join(root, "src", "ok.go"), "package ok\n")
	content, truncated, err := readCapped(osRoot, "src/ok.go", DefaultMaxFileBytes)
	if err != nil || truncated {
		t.Fatalf("readCapped(src/ok.go) = (%q, %v, %v), want the file read cleanly", content, truncated, err)
	}
	if string(content) != "package ok\n" {
		t.Errorf("readCapped(src/ok.go) = %q, want %q", content, "package ok\n")
	}
}
