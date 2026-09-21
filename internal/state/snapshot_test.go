package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func snapshotStore(t *testing.T) *Store {
	t.Helper()
	layout, err := NewLayout(t.TempDir(), "20260101T000000Z-0123456789ab")
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	return NewStore(layout)
}

// sampleSnapshot is a snapshot of a blackboard with something in it, so a
// round trip that dropped the blackboard would show.
func sampleSnapshot(seq uint64, node string, attempts int) Snapshot {
	st := NewState("count each repair once")
	st.Fix.Attempts = attempts
	st.Code.ChangedFiles = []string{"a.go"}
	return Snapshot{
		SchemaVersion: SnapshotSchemaVersion, RunID: "20260101T000000Z-0123456789ab",
		Node: node, Seq: seq, Step: 4,
		TakenAt:         time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		WorkspaceDigest: "tree1:sha256:ab", State: st,
	}
}

func TestSnapshotPath(t *testing.T) {
	t.Parallel()

	layout, err := NewLayout("/repos/app", "run-1")
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}

	tests := []struct {
		name   string
		layout Layout
		seq    uint64
		node   string
		want   string // empty: an error wrapping ErrInvalidPathSegment
	}{
		{name: "zero-padded seq, then the node", layout: layout, seq: 7, node: "fix",
			want: "/repos/app/.belay/runs/run-1/snapshots/00000007-fix.json"},
		{name: "a seq wider than the padding keeps every digit", layout: layout, seq: 123456789, node: "test",
			want: "/repos/app/.belay/runs/run-1/snapshots/123456789-test.json"},
		{name: "a node that climbs out", layout: layout, seq: 7, node: "../x"},
		{name: "a node with a separator", layout: layout, seq: 7, node: "a/b"},
		{name: "no node", layout: layout, seq: 7, node: ""},
		// No journal record has Seq 0, so a snapshot keyed on it names no
		// execution at all.
		{name: "seq zero", layout: layout, seq: 0, node: "fix"},
		{name: "a zero Layout", layout: Layout{}, seq: 7, node: "fix"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := tt.layout.SnapshotPath(tt.seq, tt.node)
			if tt.want == "" {
				if !errors.Is(err, ErrInvalidPathSegment) {
					t.Fatalf("SnapshotPath(%d, %q) = %q, %v; want ErrInvalidPathSegment", tt.seq, tt.node, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SnapshotPath(%d, %q) error = %v", tt.seq, tt.node, err)
			}
			if got != tt.want {
				t.Errorf("SnapshotPath(%d, %q) = %q, want %q", tt.seq, tt.node, got, tt.want)
			}
		})
	}
}

func TestSaveSnapshotOnceRoundTrips(t *testing.T) {
	t.Parallel()

	store := snapshotStore(t)
	want := sampleSnapshot(7, "fix", 2)

	written, err := store.SaveSnapshotOnce(want)
	if err != nil {
		t.Fatalf("SaveSnapshotOnce() error = %v", err)
	}
	if !written {
		t.Fatal("SaveSnapshotOnce() on a fresh key reported written = false")
	}

	path, err := store.Layout().SnapshotPath(7, "fix")
	if err != nil {
		t.Fatalf("SnapshotPath() error = %v", err)
	}
	got, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot() error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("snapshot round trip mismatch (-want +got):\n%s", diff)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("snapshot permissions = %v, want %v: it holds the same blackboard state.json does", perm, filePerm)
	}
}

// TestSaveSnapshotOnceKeepsTheFirst is what makes a snapshot safe to take
// again. A re-run after a crash may reach the point where the snapshot is
// taken with state.json already carrying its crashed attempt's patch; the
// snapshot that attempt took first is the truth, and must survive.
func TestSaveSnapshotOnceKeepsTheFirst(t *testing.T) {
	t.Parallel()

	store := snapshotStore(t)
	first, second := sampleSnapshot(7, "fix", 0), sampleSnapshot(7, "fix", 1)

	if written, err := store.SaveSnapshotOnce(first); err != nil || !written {
		t.Fatalf("first SaveSnapshotOnce() = %v, %v; want true, nil", written, err)
	}
	written, err := store.SaveSnapshotOnce(second)
	if err != nil {
		t.Fatalf("second SaveSnapshotOnce() error = %v", err)
	}
	if written {
		t.Error("second SaveSnapshotOnce() reported written = true over an existing snapshot")
	}

	path, err := store.Layout().SnapshotPath(7, "fix")
	if err != nil {
		t.Fatalf("SnapshotPath() error = %v", err)
	}
	got, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot() error = %v", err)
	}
	if got.State.Fix.Attempts != 0 {
		t.Errorf("snapshot holds Fix.Attempts = %d, want 0: the second save overwrote the first", got.State.Fix.Attempts)
	}
}

func TestLoadSnapshotOfAnExecutionWithNoneIsNotExist(t *testing.T) {
	t.Parallel()

	store := snapshotStore(t)
	path, err := store.Layout().SnapshotPath(7, "fix")
	if err != nil {
		t.Fatalf("SnapshotPath() error = %v", err)
	}
	if _, err := LoadSnapshot(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("LoadSnapshot() of a missing snapshot = %v, want an error wrapping fs.ErrNotExist", err)
	}
}

// writeRawSnapshot writes doc as the snapshot for (7, "fix") and returns its
// path, for the documents SaveSnapshotOnce would never write.
func writeRawSnapshot(t *testing.T, doc map[string]any) string {
	t.Helper()
	store := snapshotStore(t)
	path, err := store.Layout().SnapshotPath(7, "fix")
	if err != nil {
		t.Fatalf("SnapshotPath() error = %v", err)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

// stateDocument is a state.json-shaped map for the raw documents below.
func stateDocument(t *testing.T, st State) map[string]any {
	t.Helper()
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return doc
}

func TestLoadSnapshotRejectsAnUnknownSchemaVersion(t *testing.T) {
	t.Parallel()

	path := writeRawSnapshot(t, map[string]any{
		"schema_version": SnapshotSchemaVersion + 1, "node": "fix", "seq": 7,
		"state": stateDocument(t, NewState("g")),
	})
	if _, err := LoadSnapshot(path); !errors.Is(err, ErrUnsupportedSnapshotVersion) {
		t.Errorf("LoadSnapshot() = %v, want an error wrapping ErrUnsupportedSnapshotVersion", err)
	}
}

// TestLoadSnapshotReadsTheBlackboardAsStateJSONIsRead: the blackboard inside
// a snapshot is what a re-run of the node is given, so it is held to the
// same rules as state.json. A requirement this belay does not know is a
// refusal (ADR-0011), and an older blackboard is migrated, never decoded as
// though it were current.
func TestLoadSnapshotReadsTheBlackboardAsStateJSONIsRead(t *testing.T) {
	t.Parallel()

	t.Run("a capability this belay lacks", func(t *testing.T) {
		t.Parallel()
		st := stateDocument(t, NewState("g"))
		st["requires"] = []string{"from.the.future"}
		path := writeRawSnapshot(t, map[string]any{
			"schema_version": SnapshotSchemaVersion, "node": "fix", "seq": 7, "state": st,
		})
		var capErr *CapabilityError
		if _, err := LoadSnapshot(path); !errors.As(err, &capErr) {
			t.Fatalf("LoadSnapshot() = %v, want a *CapabilityError", err)
		}
	})

	t.Run("a version-1 blackboard", func(t *testing.T) {
		t.Parallel()
		st := stateDocument(t, NewState("g"))
		st["schema_version"] = 1
		delete(st, "requires")
		path := writeRawSnapshot(t, map[string]any{
			"schema_version": SnapshotSchemaVersion, "node": "fix", "seq": 7, "state": st,
		})
		got, err := LoadSnapshot(path)
		if err != nil {
			t.Fatalf("LoadSnapshot() error = %v", err)
		}
		if got.State.SchemaVersion != StateSchemaVersion || got.State.Requires == nil {
			t.Errorf("snapshot blackboard = version %d, requires %v; want it migrated to version %d with requires []",
				got.State.SchemaVersion, got.State.Requires, StateSchemaVersion)
		}
	})
}
