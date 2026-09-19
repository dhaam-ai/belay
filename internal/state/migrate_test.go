package state

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
)

// releasedSchemaVersion is the schema_version every belay from v0.1.0 to
// v0.1.4 writes for both state.json and manifest.json, and the only one
// those binaries read. They predate the requires list, so a version they
// refuse is the only thing that can stop them resuming a run and dropping
// the fields they do not know.
const releasedSchemaVersion = 1

// schemaDocument is one of the two versioned run files, with just enough
// of a writer and a loader to exercise its schema version.
type schemaDocument struct {
	file string
	// v1 is a document exactly as released belay wrote it: a copy of this
	// file's golden at the v0.1.3 tag, unchanged from v0.1.0 to v0.1.4.
	v1 string
	// current is this file's schema version today.
	current int
	// writeNew saves the document a freshly started run gets.
	writeNew func(path string) error
	// resume loads path and saves what it loaded back over it, which is
	// what a resumed run does with each file before it changes anything.
	resume func(path string) error
}

func schemaDocuments() []schemaDocument {
	return []schemaDocument{
		{
			file:    "state.json",
			v1:      filepath.Join("testdata", "state.v1.json"),
			current: StateSchemaVersion,
			writeNew: func(path string) error {
				return SaveState(path, NewState("ship the widget"))
			},
			resume: func(path string) error {
				st, err := LoadState(path)
				if err != nil {
					return err
				}
				return SaveState(path, st)
			},
		},
		{
			file:    "manifest.json",
			v1:      filepath.Join("testdata", "manifest.v1.json"),
			current: ManifestSchemaVersion,
			writeNew: func(path string) error {
				now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				return SaveManifest(path, NewManifest(now, "run-1", "/work/repo", "ship the widget", config.Default()))
			},
			resume: func(path string) error {
				m, err := LoadManifest(path)
				if err != nil {
					return err
				}
				return SaveManifest(path, m)
			},
		},
	}
}

func readJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a test's own temporary or testdata path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return doc
}

// TestSave_WritesASchemaVersionReleasedBelayRefuses is what the schema
// bump is for. Every release so far reads version 1 and ignores keys it
// does not know, so a run this belay writes at version 1 is one those
// binaries would resume, then save without whatever they could not read.
// The document must also survive a resume byte for byte, so the new
// version is one this binary reads back as well as writes.
func TestSave_WritesASchemaVersionReleasedBelayRefuses(t *testing.T) {
	for _, d := range schemaDocuments() {
		t.Run(d.file, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), d.file)
			if err := d.writeNew(path); err != nil {
				t.Fatalf("write %s: %v", d.file, err)
			}
			written, err := os.ReadFile(path) //nolint:gosec // the test's own temporary path
			if err != nil {
				t.Fatalf("read %s: %v", d.file, err)
			}

			got, ok := readJSONObject(t, path)["schema_version"].(float64)
			if !ok {
				t.Fatalf("%s has no numeric schema_version: %s", d.file, written)
			}
			if int(got) <= releasedSchemaVersion {
				t.Errorf("%s written at schema_version %d; belay v0.1.x reads version %d, so it would resume this run and drop every field it does not know",
					d.file, int(got), releasedSchemaVersion)
			}

			if err := d.resume(path); err != nil {
				t.Fatalf("resume %s: %v", d.file, err)
			}
			resumed, err := os.ReadFile(path) //nolint:gosec // the test's own temporary path
			if err != nil {
				t.Fatalf("read %s after resume: %v", d.file, err)
			}
			if diff := cmp.Diff(string(written), string(resumed)); diff != "" {
				t.Errorf("%s changed across a resume (-written +resumed):\n%s", d.file, diff)
			}
		})
	}
}

// TestLoad_MigratesAVersion1Run is the other half of the bump: a run that
// released belay started must still load and resume here. Resuming saves
// it at the current version with a requires list, and every field the
// version-1 document carried must come through unchanged.
//
// The cases beyond the released shape cover a requires key at version 1,
// which only development builds between the requires list and the bump
// wrote. The migration may fill in a missing list, but it must never
// empty one: a capability a document names is still refused by name.
func TestLoad_MigratesAVersion1Run(t *testing.T) {
	cases := []struct {
		name string
		// requires is spliced into the version-1 document as its requires
		// value; empty leaves the key out, as every released belay did.
		requires string
		// key is the spelling requires is spliced under; empty means
		// "requires". encoding/json matches a field name
		// case-insensitively, so a document spelling it another way still
		// decodes into Requires — and the migration must not add a
		// lower-case empty list that would win that decode against it.
		key     string
		want    []any
		wantErr error
	}{
		{name: "as released belay wrote it, with no requires key", want: []any{}},
		{name: "with requires null", requires: `null`, want: []any{}},
		{name: "with requires empty", requires: `[]`, want: []any{}},
		{name: "requiring a capability this belay lacks", requires: `["from.the.future"]`, wantErr: ErrUnknownCapability},
		{
			name: "requiring a capability this belay lacks, spelled Requires",
			key:  "Requires", requires: `["from.the.future"]`, wantErr: ErrUnknownCapability,
		},
	}
	for _, d := range schemaDocuments() {
		for _, tt := range cases {
			t.Run(d.file+"/"+tt.name, func(t *testing.T) {
				v1 := readJSONObject(t, d.v1)
				if v1["schema_version"] != float64(releasedSchemaVersion) {
					t.Fatalf("fixture %s has schema_version %v, want %d", d.v1, v1["schema_version"], releasedSchemaVersion)
				}
				doc := maps.Clone(v1)
				if tt.requires != "" {
					var requires any
					if err := json.Unmarshal([]byte(tt.requires), &requires); err != nil {
						t.Fatalf("decode requires %s: %v", tt.requires, err)
					}
					key := tt.key
					if key == "" {
						key = "requires"
					}
					doc[key] = requires
				}
				path := writeJSONObject(t, d.file, doc)

				err := d.resume(path)
				if tt.wantErr != nil {
					if !errors.Is(err, tt.wantErr) {
						t.Fatalf("resume a version-1 %s requiring %s: error = %v, want one wrapping %v", d.file, tt.requires, err, tt.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("resume a version-1 %s: %v", d.file, err)
				}

				got := readJSONObject(t, path)
				if got["schema_version"] != float64(d.current) {
					t.Errorf("schema_version after resume = %v, want %d", got["schema_version"], d.current)
				}
				if diff := cmp.Diff(tt.want, got["requires"]); diff != "" {
					t.Errorf("requires after resume mismatch (-want +got):\n%s", diff)
				}
				for key, want := range v1 {
					if key == "schema_version" {
						continue
					}
					if diff := cmp.Diff(want, got[key]); diff != "" {
						t.Errorf("%s did not survive the migration (-version 1 +resumed):\n%s", key, diff)
					}
				}
			})
		}
	}
}

func writeJSONObject(t *testing.T, name string, doc map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	return writeRequiresDocument(t, name, string(raw))
}
