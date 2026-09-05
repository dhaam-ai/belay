package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateSegment(t *testing.T) {
	tests := []struct {
		name    string
		segment string
		wantErr bool
	}{
		{"empty", "", true},
		{"parent traversal", "../etc", true},
		{"parent traversal suffix", "foo/../../etc", true},
		{"bare dotdot", "..", true},
		{"absolute unix path", "/etc/passwd", true},
		{"embedded separator", "a/b", true},
		{"embedded backslash", `a\b`, true},
		{"embedded NUL", "a\x00b", true},
		{"ordinary run id", "20260101T000000Z-abc123", false},
		{"ordinary name", "plan.md", false},
		{"single dot is not a traversal but is still an empty-ish name", ".", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSegment("test segment", tt.segment)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSegment(%q) error = %v, wantErr %v", tt.segment, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidPathSegment) {
				t.Errorf("error does not wrap ErrInvalidPathSegment: %v", err)
			}
			var pse *PathSegmentError
			if err != nil && !errors.As(err, &pse) {
				t.Errorf("error is not a *PathSegmentError: %v", err)
			}
		})
	}
}

func TestJoinSafe(t *testing.T) {
	base := filepath.Join(t.TempDir(), "run")

	t.Run("ordinary segment stays inside base", func(t *testing.T) {
		got, err := joinSafe("kind", base, "plan.md")
		if err != nil {
			t.Fatalf("joinSafe: %v", err)
		}
		want := filepath.Join(base, "plan.md")
		if got != want {
			t.Errorf("joinSafe = %q, want %q", got, want)
		}
	})

	t.Run("traversal rejected", func(t *testing.T) {
		if _, err := joinSafe("kind", base, "../escape"); err == nil {
			t.Fatal("joinSafe did not reject a traversal segment")
		}
	})
}

// TestAtomicWriteFile_CrashSafety proves that stopping atomicWriteFileUpTo
// at any prefix of its write sequence — simulating a crash exactly there —
// never leaves the target path holding a truncated or otherwise torn file:
// it holds either its complete old content or its complete new content,
// and nothing in between.
func TestAtomicWriteFile_CrashSafety(t *testing.T) {
	stages := []struct {
		name  string
		stage atomicWriteStage
		// wantNew is true if, once atomicWriteFileUpTo returns having
		// stopped at this stage, the target path must hold the new
		// content; false if it must still hold the old content.
		wantNew bool
	}{
		{"stop before temp file created", stageTempCreated, false},
		{"stop after data written to temp", stageDataWritten, false},
		{"stop after temp fsynced", stageTempSynced, false},
		{"stop after temp closed", stageTempClosed, false},
		{"stop after rename", stageRenamed, true},
		{"stop after directory fsync (full success)", stageDirSynced, true},
	}

	for _, tt := range stages {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")

			oldContent := []byte(`{"v":"old"}`)
			if err := atomicWriteFile(path, oldContent, filePerm); err != nil {
				t.Fatalf("seed old content: %v", err)
			}

			newContent := []byte(`{"v":"new-` + tt.name + `"}`)
			if err := atomicWriteFileUpTo(path, newContent, filePerm, tt.stage); err != nil {
				t.Fatalf("atomicWriteFileUpTo: %v", err)
			}

			got, err := os.ReadFile(path) //nolint:gosec // fixed test path
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if !json.Valid(got) {
				t.Fatalf("file is not valid JSON after stopping at %v: %q", tt.stage, got)
			}

			want := oldContent
			if tt.wantNew {
				want = newContent
			}
			if string(got) != string(want) {
				t.Errorf("after stopping at %v: got %q, want %q", tt.stage, got, want)
			}
		})
	}
}

// TestAtomicWriteFile_FreshFile proves the same crash-prefix property when
// there is no pre-existing file at path: any stage before the rename must
// leave path absent, never a zero-length or partial file.
func TestAtomicWriteFile_FreshFile(t *testing.T) {
	stages := []struct {
		stage      atomicWriteStage
		wantExists bool
	}{
		{stageTempCreated, false},
		{stageDataWritten, false},
		{stageTempSynced, false},
		{stageTempClosed, false},
		{stageRenamed, true},
		{stageDirSynced, true},
	}

	for _, tt := range stages {
		dir := t.TempDir()
		path := filepath.Join(dir, "fresh.json")
		content := []byte(`{"fresh":true}`)

		if err := atomicWriteFileUpTo(path, content, filePerm, tt.stage); err != nil {
			t.Fatalf("stage %v: %v", tt.stage, err)
		}

		got, err := os.ReadFile(path) //nolint:gosec // fixed test path
		switch {
		case tt.wantExists && err != nil:
			t.Fatalf("stage %v: expected file to exist, read error: %v", tt.stage, err)
		case tt.wantExists:
			if string(got) != string(content) {
				t.Errorf("stage %v: got %q, want %q", tt.stage, got, content)
			}
		case !tt.wantExists && err == nil:
			t.Fatalf("stage %v: expected no file, but read %q", tt.stage, got)
		case !tt.wantExists && !os.IsNotExist(err):
			t.Fatalf("stage %v: unexpected read error: %v", tt.stage, err)
		}
	}
}

// TestAtomicWriteFile_NoPartialReads runs many writes concurrently with a
// reader that continuously reads the same path, and asserts every read
// that finds the file at all sees a complete, valid, self-consistent JSON
// document — never a torn write. This is the property atomicWriteFile
// relies on os.Rename's same-filesystem atomicity to provide; a naive
// "open path, write, close" implementation would fail this test.
func TestAtomicWriteFile_NoPartialReads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	type doc struct {
		Seq     int    `json:"seq"`
		Payload string `json:"payload"`
	}

	const writes = 200
	var stop atomic.Bool
	var readErr atomic.Value // holds string
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			data, err := os.ReadFile(path) //nolint:gosec // fixed test path
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				readErr.Store(err.Error())
				return
			}
			var d doc
			if err := json.Unmarshal(data, &d); err != nil {
				readErr.Store("torn read: " + err.Error() + ": " + string(data))
				return
			}
			// The payload's declared length must match its actual length:
			// a read that landed mid-rename on a filesystem without atomic
			// rename semantics could otherwise still parse as valid JSON
			// while holding bytes from two different writes.
			if len(d.Payload) != d.Seq {
				readErr.Store("inconsistent document (payload/seq mismatch): " + string(data))
				return
			}
		}
	}()

	for i := 1; i <= writes; i++ {
		payload := strings.Repeat("x", i)
		d := doc{Seq: i, Payload: payload}
		data, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := atomicWriteFile(path, data, filePerm); err != nil {
			t.Fatalf("atomicWriteFile %d: %v", i, err)
		}
	}
	stop.Store(true)
	wg.Wait()

	if v := readErr.Load(); v != nil {
		t.Fatalf("reader observed a bad file: %s", v)
	}
}

func TestAtomicWriteFile_CreatesParentDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "state.json")
	if err := atomicWriteFile(path, []byte(`{}`), filePerm); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}
}

func TestAtomicWriteFile_SetsRequestedPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := atomicWriteFile(path, []byte(`{}`), 0o640); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("permissions = %v, want %v", got, os.FileMode(0o640))
	}
}

func TestAtomicWriteFile_LeavesNoTempFileOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := atomicWriteFile(path, []byte(`{}`), filePerm); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory contains %v, want only state.json", names)
	}
}

func TestWriteJSONAndReadFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.json")

	type doc struct {
		Name string    `json:"name"`
		At   time.Time `json:"at"`
	}
	want := doc{Name: "belay", At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}

	if err := writeJSON(path, want, filePerm); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	raw, err := readFile(path)
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	var got doc
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.At.Equal(want.At) || got.Name != want.Name {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestReadFile_MissingFile(t *testing.T) {
	_, err := readFile(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
