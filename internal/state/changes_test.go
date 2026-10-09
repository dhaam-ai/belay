package state

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
)

// Where a run learns what its workspace changed is decided once, when the
// run is created, and read back on every resume: a run started against a
// commit must not be judged against something else after a restart.
func TestAManifestKeepsWhereItsChangeIsReadFrom(t *testing.T) {
	t.Parallel()

	for _, pin := range []Changes{
		{From: ChangesFromGit, Base: "0123456789abcdef0123456789abcdef01234567", Record: true},
		{From: ChangesFromRecord},
		{From: ChangesReported, Why: "a fork's copy keeps no record of where its source run began"},
	} {
		t.Run(string(pin.From), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "manifest.json")
			m := NewManifest(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "run-1", "/work", "goal", config.Default())
			m.SetChanges(pin)
			if err := SaveManifest(path, m); err != nil {
				t.Fatalf("SaveManifest: %v", err)
			}
			got, err := LoadManifest(path)
			if err != nil {
				t.Fatalf("LoadManifest: %v", err)
			}
			if got.Changes == nil {
				t.Fatal("the manifest lost where its change is read from")
			}
			if diff := cmp.Diff(pin, *got.Changes); diff != "" {
				t.Errorf("Changes after a round trip (-want +got):\n%s", diff)
			}
			// Not a capability: see Changes for why.
			if len(got.Requires) != 0 {
				t.Errorf("Requires = %v, want none", got.Requires)
			}
		})
	}
}

// The file record lives in the run directory, beside the manifest, and a
// zero Layout has nowhere to put one.
func TestLayoutNamesTheFileRecord(t *testing.T) {
	t.Parallel()
	layout, err := NewLayout("/work", "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if got, want := layout.FileRecordPath(), filepath.Join(layout.RunDir(), "file-record.json"); got != want {
		t.Errorf("FileRecordPath = %q, want %q", got, want)
	}
	if got := (Layout{}).FileRecordPath(); got != "" {
		t.Errorf("Layout{}.FileRecordPath = %q, want empty", got)
	}
}
