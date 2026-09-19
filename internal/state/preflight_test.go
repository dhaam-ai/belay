package state

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

// v1Run writes a run directory holding a version-1 manifest.json and a
// version-1 state.json — a run belay v0.1.x started — and returns its Store.
// Each document may be overridden key by key, which is how a row seeds a
// requires list or a version this belay cannot read.
func v1Run(t *testing.T, manifestKeys, stateKeys map[string]any) *Store {
	t.Helper()
	layout, err := NewLayout(t.TempDir(), "20260101T000000Z-c0ffee000000")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := os.MkdirAll(layout.RunDir(), dirPerm); err != nil {
		t.Fatalf("create run dir: %v", err)
	}
	for _, d := range schemaDocuments() {
		doc := maps.Clone(readJSONObject(t, d.v1))
		overrides := manifestKeys
		if d.file == "state.json" {
			overrides = stateKeys
		}
		maps.Copy(doc, overrides)
		copyJSONObject(t, doc, filepath.Join(layout.RunDir(), d.file))
	}
	return NewStore(layout)
}

// copyJSONObject writes doc to path as JSON.
func copyJSONObject(t *testing.T, doc map[string]any, path string) {
	t.Helper()
	written := writeJSONObject(t, filepath.Base(path), doc)
	raw, err := os.ReadFile(written) //nolint:gosec // the test's own temporary path
	if err != nil {
		t.Fatalf("read %s: %v", written, err)
	}
	if err := os.WriteFile(path, raw, filePerm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// onDiskVersion returns the schema_version recorded in the document at path,
// read raw: a loader would migrate it and report the version it produced
// rather than the one on disk.
func onDiskVersion(t *testing.T, path string) int {
	t.Helper()
	v, ok := readJSONObject(t, path)["schema_version"].(float64)
	if !ok {
		t.Fatalf("%s has no numeric schema_version", path)
	}
	return int(v)
}

// TestStorePreflight pins the order belay establishes a run in before it
// writes to it: refuse what it may not read, then bring the manifest up to
// the version it writes, and only then let the caller write anything.
//
// The manifest is upgraded first because every reader of a run — `belay
// runs`, `belay resume`, the dispatcher — reads manifest.json first. A crash
// after state.json was saved at the current version but before the manifest
// was leaves a released belay a manifest it still accepts over a state.json
// it does not, and it resumes the run and writes to it (ADR-0011). The other
// order leaves it the manifest version it refuses, which is the whole point
// of the bump.
func TestStorePreflight(t *testing.T) {
	tests := []struct {
		name         string
		manifestKeys map[string]any
		stateKeys    map[string]any
		wantErr      error
		// wantManifest is the schema version manifest.json is left at.
		wantManifest int
	}{
		{
			name:         "a version-1 run has its manifest saved at the current version",
			wantManifest: ManifestSchemaVersion,
		},
		{
			name:         "a run already at the current version is left as it is",
			manifestKeys: map[string]any{"schema_version": float64(ManifestSchemaVersion), "requires": []any{}},
			stateKeys:    map[string]any{"schema_version": float64(StateSchemaVersion), "requires": []any{}},
			wantManifest: ManifestSchemaVersion,
		},
		{
			name:         "state.json requiring a capability this belay lacks is refused, and nothing is written",
			stateKeys:    map[string]any{"requires": []any{"output.branch"}},
			wantErr:      ErrUnknownCapability,
			wantManifest: 1,
		},
		{
			name:         "state.json at a version this belay cannot read is refused, and nothing is written",
			stateKeys:    map[string]any{"schema_version": float64(99)},
			wantErr:      ErrUnsupportedStateVersion,
			wantManifest: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := v1Run(t, tt.manifestKeys, tt.stateKeys)
			before := readJSONObject(t, store.Layout().ManifestPath())

			err := store.Preflight()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Preflight() error = %v, want one wrapping %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Preflight(): %v", err)
			}

			if got := onDiskVersion(t, store.Layout().ManifestPath()); got != tt.wantManifest {
				t.Errorf("manifest.json is at schema version %d, want %d", got, tt.wantManifest)
			}
			// Nothing but the version and the requires list may change: a
			// preflight is not a lifecycle event, and rewriting a field here
			// would rewrite history.
			after := readJSONObject(t, store.Layout().ManifestPath())
			for key, want := range before {
				if key == "schema_version" || key == "requires" {
					continue
				}
				if got := after[key]; !equalJSON(got, want) {
					t.Errorf("Preflight changed %s: %v, want %v", key, got, want)
				}
			}
		})
	}
}

// TestStorePreflightWithoutAStateFile: a run directory with no state.json is
// not a run this belay can continue, and it must say so before it writes
// anything rather than after.
func TestStorePreflightWithoutAStateFile(t *testing.T) {
	store := v1Run(t, nil, nil)
	if err := os.Remove(store.Layout().StatePath()); err != nil {
		t.Fatalf("remove state.json: %v", err)
	}
	if err := store.Preflight(); err == nil {
		t.Fatal("Preflight() = nil for a run with no state.json")
	}
	if got := onDiskVersion(t, store.Layout().ManifestPath()); got != 1 {
		t.Errorf("manifest.json is at schema version %d; nothing may be written for a run belay cannot read", got)
	}
}

// equalJSON compares two decoded JSON values structurally. cmp.Diff would do
// as well; this keeps the comparison in one line at each use.
func equalJSON(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !equalJSON(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i, v := range av {
			if !equalJSON(v, bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
