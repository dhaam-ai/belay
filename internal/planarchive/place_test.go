package planarchive

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const testRel = "docs/plans/20260905T142530Z-1f2e3d4c5b6a-add-tests.md"

var testPlan = []byte("## Steps\n\n1. Write the test.\n2. Make it pass.\n")

// readFile reads path, failing the test when it cannot.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // every path here is under the test's own t.TempDir()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// writeFile writes data at path, creating its directory.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// listTree returns every path under root, relative to it and sorted, so a
// test can say exactly what a call left behind.
func listTree(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, err := filepath.Rel(root, path); err == nil && rel != "." {
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	slices.Sort(paths)
	return paths
}

// TestPlaceCreatesTheFileAndItsDirectories is the ordinary case: nothing is
// there yet, not even docs/, and afterwards exactly the plan is, as an
// ordinary file of the repository rather than a private one of belay's.
func TestPlaceCreatesTheFileAndItsDirectories(t *testing.T) {
	root := t.TempDir()

	placed, err := Place(root, testRel, testPlan)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if !placed {
		t.Error("Place reported the plan was already there, on an empty workspace")
	}
	dest := filepath.Join(root, filepath.FromSlash(testRel))
	if got := readFile(t, dest); string(got) != string(testPlan) {
		t.Errorf("the placed file holds %q, want %q", got, testPlan)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// 0644 as the umask leaves it: a document of the repository, which a
	// person commits and other tools read, not one of belay's private run
	// files. The umask may take bits away; nothing may add them.
	if perm := info.Mode().Perm(); perm&0o600 != 0o600 || perm&^0o644 != 0 {
		t.Errorf("the placed file's mode is %v, want 0644 less the umask", perm)
	}
	want := []string{"docs", "docs/plans", testRel}
	if diff := slices.Compare(want, listTree(t, root)); diff != 0 {
		t.Errorf("the workspace holds %v, want exactly %v: no temporary file may be left behind", listTree(t, root), want)
	}
}

// TestPlaceNeverReplacesWhatIsThere covers every thing that can already be
// at the destination. The same bytes are a previous attempt's, and adopted;
// anything else is somebody's, and left exactly as it was.
func TestPlaceNeverReplacesWhatIsThere(t *testing.T) {
	tests := []struct {
		name       string
		arrange    func(t *testing.T, dest string)
		wantPlaced bool
		wantErr    error
	}{
		{
			name:    "the same plan, from an attempt that did not finish",
			arrange: func(t *testing.T, dest string) { writeFile(t, dest, testPlan) },
		},
		{
			name:    "a different file",
			arrange: func(t *testing.T, dest string) { writeFile(t, dest, []byte("somebody's own notes\n")) },
			wantErr: ErrOccupied,
		},
		{
			name: "a directory",
			arrange: func(t *testing.T, dest string) {
				if err := os.MkdirAll(dest, 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
			wantErr: ErrOccupied,
		},
		{
			// A link is never followed, not even to a file holding the
			// same bytes: writing through it would write wherever it leads.
			name: "a symbolic link to the same plan",
			arrange: func(t *testing.T, dest string) {
				target := filepath.Join(filepath.Dir(filepath.Dir(dest)), "elsewhere.md")
				writeFile(t, target, testPlan)
				if err := os.Symlink(target, dest); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			wantErr: ErrOccupied,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, filepath.FromSlash(testRel))
			if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			tt.arrange(t, dest)
			before := listTree(t, root)

			placed, err := Place(root, testRel, testPlan)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Place error = %v, want %v", err, tt.wantErr)
			}
			if placed != tt.wantPlaced {
				t.Errorf("placed = %v, want %v", placed, tt.wantPlaced)
			}
			if after := listTree(t, root); slices.Compare(before, after) != 0 {
				t.Errorf("Place changed the workspace from %v to %v", before, after)
			}
		})
	}
}

// TestPlaceStaysInsideTheWorkspace covers the ways a destination can lead
// out of the tree it was meant for. Configuration refuses an absolute path
// and a "..", and Place refuses them again rather than trusting that; only
// Place can see a symbolic link, because only the disk has one.
func TestPlaceStaysInsideTheWorkspace(t *testing.T) {
	tests := []struct {
		name    string
		rel     string
		arrange func(t *testing.T, root, outside string)
	}{
		{name: "climbing out", rel: "../escaped.md"},
		{name: "absolute", rel: "/tmp/escaped.md"},
		{
			name: "docs is a link out of the workspace",
			rel:  testRel,
			arrange: func(t *testing.T, root, outside string) {
				if err := os.Symlink(outside, filepath.Join(root, "docs")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
		},
		{
			name: "docs/plans is a link out of the workspace",
			rel:  testRel,
			arrange: func(t *testing.T, root, outside string) {
				if err := os.MkdirAll(filepath.Join(root, "docs"), 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.Symlink(outside, filepath.Join(root, "docs", "plans")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			root, outside := filepath.Join(base, "workspace"), filepath.Join(base, "outside")
			for _, dir := range []string{root, outside} {
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			}
			if tt.arrange != nil {
				tt.arrange(t, root, outside)
			}

			_, err := Place(root, tt.rel, testPlan)
			if !errors.Is(err, ErrOutside) {
				t.Fatalf("Place error = %v, want ErrOutside", err)
			}
			if got := listTree(t, outside); len(got) != 0 {
				t.Errorf("Place wrote outside the workspace: %v", got)
			}
			if got := listTree(t, base); slices.Contains(got, "escaped.md") {
				t.Errorf("Place wrote beside the workspace: %v", got)
			}
		})
	}
}

// TestPlaceIsInsideAWorkspaceReachedThroughALink is the other half of the
// rule: a link that stays inside the workspace is just a directory, and a
// workspace that is itself reached through a link (macOS's /var is one) is
// still the workspace.
func TestPlaceIsInsideAWorkspaceReachedThroughALink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(target, "documentation"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("documentation", filepath.Join(target, "docs")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	root := filepath.Join(base, "link")
	if err := os.Symlink(target, root); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := Place(root, testRel, testPlan); err != nil {
		t.Fatalf("Place: %v", err)
	}
	got := readFile(t, filepath.Join(target, "documentation", "plans", filepath.Base(testRel)))
	if string(got) != string(testPlan) {
		t.Errorf("the plan holds %q, want %q", got, testPlan)
	}
}

// TestPlaceClearsWhatACrashLeftBehind is Place re-run after dying part way.
// Its temporary file is its own by name, so it is removed rather than left
// in docs/ for a person, or `git add --all` in a branch run, to find.
func TestPlaceClearsWhatACrashLeftBehind(t *testing.T) {
	tests := []struct {
		name string
		// linked is whether the crash came after the temporary file was
		// linked into place, so the plan is already there too.
		linked     bool
		wantPlaced bool
	}{
		{name: "before the plan was linked into place", linked: false, wantPlaced: true},
		{name: "after the plan was linked into place", linked: true, wantPlaced: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, filepath.FromSlash(testRel))
			writeFile(t, tempPath(dest), testPlan[:7])
			if tt.linked {
				writeFile(t, dest, testPlan)
			}

			placed, err := Place(root, testRel, testPlan)
			if err != nil {
				t.Fatalf("Place: %v", err)
			}
			if placed != tt.wantPlaced {
				t.Errorf("placed = %v, want %v", placed, tt.wantPlaced)
			}
			if got := readFile(t, dest); string(got) != string(testPlan) {
				t.Errorf("the plan holds %q, want %q", got, testPlan)
			}
			if want := []string{"docs", "docs/plans", testRel}; slices.Compare(want, listTree(t, root)) != 0 {
				t.Errorf("the workspace holds %v, want exactly %v", listTree(t, root), want)
			}
		})
	}
}
