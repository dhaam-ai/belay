package detect

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

// writeUnreadable creates a file the current process cannot open. It reports an
// error when that is impossible, which is the case when the tests run as root.
func writeUnreadable(path string) error {
	if current, err := user.Current(); err == nil && current.Uid == "0" {
		return os.ErrPermission
	}
	if err := os.WriteFile(path, []byte("module example.com/m\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o000)
}

// writeFiles builds a throwaway directory holding the named files and returns
// its path.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// detectOne runs Detect and asserts that it found exactly one project.
func detectOne(t *testing.T, root string) Project {
	t.Helper()

	got, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect(%q) returned error: %v", root, err)
	}
	if len(got) != 1 {
		t.Fatalf("Detect(%q) returned %d projects, want 1: %+v", root, len(got), got)
	}
	return got[0]
}
