package detect

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestDetectEmptyDirectoryIsUnknown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	got, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect(%q) returned error: %v", dir, err)
	}
	if len(got) != 1 {
		t.Fatalf("Detect returned %d projects, want exactly 1: %+v", len(got), got)
	}

	project := got[0]
	if project.Kind != KindUnknown {
		t.Errorf("Kind = %v, want %v", project.Kind, KindUnknown)
	}
	if project.Root != dir {
		t.Errorf("Root = %q, want %q", project.Root, dir)
	}
	if project.Confidence != ConfidenceNone {
		t.Errorf("Confidence = %v, want %v", project.Confidence, ConfidenceNone)
	}
	if project.Go != nil || project.Node != nil || project.Python != nil {
		t.Errorf("unknown project carries detail: %+v", project)
	}
	if len(project.Markers) != 0 {
		t.Errorf("Markers = %v, want none", project.Markers)
	}
}

func TestDetectUnusableRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(file, []byte("module example.com/m\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		path   string
		target error
	}{
		{name: "missing", path: filepath.Join(dir, "nope"), target: fs.ErrNotExist},
		{name: "regular file", path: file, target: ErrNotDirectory},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Detect(tt.path)
			if err == nil {
				t.Fatalf("Detect(%q) = %+v, want error", tt.path, got)
			}
			if !errors.Is(err, tt.target) {
				t.Errorf("Detect(%q) error = %v, want it to wrap %v", tt.path, err, tt.target)
			}
			if got != nil {
				t.Errorf("Detect(%q) = %+v on error, want nil", tt.path, got)
			}
		})
	}
}

// TestWalkSkipsGeneratedDirectories proves the skip list by counting the
// directories actually read, not by timing the walk. The fixture holds a
// node_modules and a vendor tree that are deep enough to be expensive.
func TestWalkSkipsGeneratedDirectories(t *testing.T) {
	t.Parallel()

	root := filepath.Join("testdata", "skiplist")

	// testdata/skiplist itself plus src. node_modules and vendor are skipped
	// before they are read, so nothing below them is visited.
	const withSkipList = 2
	// Without the skip list, and still bounded by DefaultMaxDepth:
	// root(0); node_modules, src, vendor(1); left-pad, github.com(2);
	// dist, pkg(3).
	const withoutSkipList = 8

	scanned := &scanner{cfg: newConfig(nil)}
	scanned.walk(root, 0)
	if scanned.dirsRead != withSkipList {
		t.Errorf("read %d directories with the default skip list, want %d", scanned.dirsRead, withSkipList)
	}

	control := &scanner{cfg: newConfig([]Option{WithSkipDirs()})}
	control.walk(root, 0)
	if control.dirsRead != withoutSkipList {
		t.Errorf("read %d directories with skipping disabled, want %d", control.dirsRead, withoutSkipList)
	}
	if control.dirsRead <= scanned.dirsRead {
		t.Errorf("skipping read %d directories and not skipping read %d: the skip list did nothing",
			scanned.dirsRead, control.dirsRead)
	}
}

func TestWalkRespectsMaxDepth(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b", "c", "d", "e"), 0o750); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		options  []Option
		wantDirs int
	}{
		{name: "default depth", options: nil, wantDirs: 1 + DefaultMaxDepth},
		{name: "root only", options: []Option{WithMaxDepth(0)}, wantDirs: 1},
		{name: "one level", options: []Option{WithMaxDepth(1)}, wantDirs: 2},
		{name: "negative clamps to root only", options: []Option{WithMaxDepth(-5)}, wantDirs: 1},
		{name: "deeper than the tree", options: []Option{WithMaxDepth(99)}, wantDirs: 6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := &scanner{cfg: newConfig(tt.options)}
			s.walk(root, 0)
			if s.dirsRead != tt.wantDirs {
				t.Errorf("read %d directories, want %d", s.dirsRead, tt.wantDirs)
			}
		})
	}
}

// TestWalkDoesNotFollowSymlinks builds a tree whose links point at themselves,
// at an ancestor, and at the filesystem root. Following any of them would make
// the walk cyclic or unbounded, so the assertion is that Detect returns at all,
// and that it reads only the real directories.
func TestWalkDoesNotFollowSymlinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o750); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"self":   root,
		"parent": filepath.Dir(root),
		"escape": string(filepath.Separator),
		"loop":   filepath.Join(root, "real"),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(realDir, name)); err != nil {
			t.Skipf("symlinks unavailable on this platform: %v", err)
		}
	}

	done := make(chan []Project, 1)
	go func() {
		got, err := Detect(root)
		if err != nil {
			t.Errorf("Detect returned error: %v", err)
		}
		done <- got
	}()

	select {
	case got := <-done:
		if len(got) != 1 || got[0].Kind != KindUnknown {
			t.Errorf("Detect = %+v, want a single unknown project", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Detect did not terminate: a symbolic link was followed")
	}

	s := &scanner{cfg: newConfig(nil)}
	s.walk(root, 0)
	if want := 2; s.dirsRead != want {
		t.Errorf("read %d directories, want %d (the root and real/)", s.dirsRead, want)
	}
}

// TestSymlinkedMarkerIsIgnored pins the rule that only regular files count as
// markers. A link named go.mod could otherwise make Detect read a file outside
// the tree it was pointed at.
func TestSymlinkedMarkerIsIgnored(t *testing.T) {
	t.Parallel()

	outside := t.TempDir()
	target := filepath.Join(outside, "elsewhere.mod")
	if err := os.WriteFile(target, []byte("module example.com/outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	got, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if len(got) != 1 || got[0].Kind != KindUnknown {
		t.Errorf("Detect = %+v, want a single unknown project", got)
	}
}

func TestWithSkipDirsReplacesTheDefaults(t *testing.T) {
	t.Parallel()

	root := writeFiles(t, map[string]string{
		"node_modules/left-pad/package.json": `{"name":"left-pad"}`,
		"generated/package.json":             `{"name":"generated"}`,
		"app/package.json":                   `{"name":"app"}`,
	})

	got, err := Detect(root, WithSkipDirs("generated"))
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}

	var names []string
	for _, project := range got {
		names = append(names, project.Node.Name)
	}
	want := []string{"app", "left-pad"}
	if !slices.Equal(names, want) {
		t.Errorf("detected %v, want %v: the custom list replaces the default, it does not extend it", names, want)
	}
}
