package ndjson

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

// dirNames lists the names in dir, sorted, so a test can say exactly what a
// call left behind.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestCreateNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing *string // nil: nothing is at the path beforehand
		lines    []string
		wantErr  error
		want     string // the file's content afterwards
	}{
		{
			name:  "every line is written, each with its newline",
			lines: []string{`{"n":1}`, `{"n":2}`},
			want:  "{\"n\":1}\n{\"n\":2}\n",
		},
		{
			name: "no lines makes an empty file",
			want: "",
		},
		{
			name:     "a file already there is refused and left as it was",
			existing: ptr("old\n"),
			lines:    []string{"new"},
			wantErr:  ErrExists,
			want:     "old\n",
		},
		{
			// Empty is not absent: a caller that created the file and
			// crashed before writing to it still owns it.
			name:     "an empty file already there is refused too",
			existing: ptr(""),
			lines:    []string{"new"},
			wantErr:  ErrExists,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, "log.ndjson")
			if tt.existing != nil {
				if err := os.WriteFile(path, []byte(*tt.existing), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}
			lines := make([][]byte, len(tt.lines))
			for i, l := range tt.lines {
				lines[i] = []byte(l)
			}

			err := CreateNew(path, lines, 0o600)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CreateNew() error = %v, want %v", err, tt.wantErr)
			}

			// #nosec G304 -- path is under the test's own temp directory.
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("file = %q, want %q", got, tt.want)
			}
			if names := dirNames(t, dir); len(names) != 1 || names[0] != "log.ndjson" {
				t.Errorf("directory holds %q afterwards, want only log.ndjson: nothing temporary may be left behind", names)
			}
		})
	}
}

// TestCreateNewSetsThePermissionAsked: the file is created with exactly perm,
// whatever the process umask would have made of it.
func TestCreateNewSetsThePermissionAsked(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "log.ndjson")
	if err := CreateNew(path, [][]byte{[]byte("a")}, 0o640); err != nil {
		t.Fatalf("CreateNew() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("permission = %v, want %v", got, fs.FileMode(0o640))
	}
}

// TestCreateNewNeverWritesThroughASymlink: a link at the path counts as
// something being there, even one that points nowhere. Following it would let
// whoever planted it choose where belay writes.
func TestCreateNewNeverWritesThroughASymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "log.ndjson")
	target := filepath.Join(dir, "elsewhere")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	if err := CreateNew(path, [][]byte{[]byte("a")}, 0o600); !errors.Is(err, ErrExists) {
		t.Fatalf("CreateNew() over a dangling symlink error = %v, want ErrExists", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lstat(target) error = %v, want the link's target still not to exist", err)
	}
}

// TestCreateNewHasOneWinner: of several callers creating the same path at
// once, exactly one succeeds and the file holds exactly what it wrote. This is
// the property create-if-absent exists for, and the one a check-then-rename
// would lose.
func TestCreateNewHasOneWinner(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "log.ndjson")
	const callers = 8

	var (
		wg      sync.WaitGroup
		errs    = make([]error, callers)
		release = make(chan struct{})
	)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-release
			errs[i] = CreateNew(path, [][]byte{[]byte(fmt.Sprint(i))}, 0o600)
		}()
	}
	close(release)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner >= 0:
			t.Fatalf("callers %d and %d both created %s", winner, i, path)
		case err == nil:
			winner = i
		case !errors.Is(err, ErrExists):
			t.Errorf("caller %d error = %v, want nil or ErrExists", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no caller created the file")
	}
	// #nosec G304 -- path is under the test's own temp directory.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if want := fmt.Sprintf("%d\n", winner); string(got) != want {
		t.Errorf("file = %q, want the winner's %q", got, want)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Errorf("directory holds %q afterwards, want only log.ndjson", names)
	}
}

// TestCreateNewDoesNotMakeDirectories: a missing directory is the caller's
// mistake to see, not one to paper over by creating whatever the path names.
func TestCreateNewDoesNotMakeDirectories(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing")
	err := CreateNew(filepath.Join(missing, "log.ndjson"), [][]byte{[]byte("a")}, 0o600)
	if err == nil || errors.Is(err, ErrExists) {
		t.Fatalf("CreateNew() in a missing directory error = %v, want an error other than ErrExists", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lstat(%s) error = %v, want the directory still not to exist", missing, err)
	}
}
