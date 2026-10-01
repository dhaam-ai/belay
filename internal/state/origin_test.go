package state

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
)

// TestManifestWithoutOriginCarriesNoBlock: a run somebody started writes
// exactly the manifest.json it wrote before forks existed. A null "origin"
// on every manifest would change the bytes of every run belay writes, for a
// field that only a fork ever fills in.
func TestManifestWithoutOriginCarriesNoBlock(t *testing.T) {
	m := NewManifest(time.Now(), "run-1", "/work", "goal", config.Default())

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := keys["origin"]; ok {
		t.Errorf("an ordinary run's manifest carries an \"origin\" key: %s", data)
	}
}

// TestOriginRoundTripsAndRequiresNothing: a fork's manifest reads back as it
// was written, and declares no capability. Origin is a record of where the
// run came from; nothing the run does depends on it, so an older belay that
// dropped it would lose the record and change nothing about the run
// (ADR-0011), and a fork stays resumable by any belay that reads version 2.
func TestOriginRoundTripsAndRequiresNothing(t *testing.T) {
	m := NewManifest(time.Now().UTC().Truncate(time.Second), "fork-1", "/work/.belay/forks/fork-1", "goal", config.Default())
	m.Origin = &Origin{
		RunID: "run-1", Workspace: "/work", Tree: "/work", Node: "fix", Visit: 2, StartSeq: 17,
		InheritedUSD: 1.25, InheritedEstimated: true,
		Deviations: []string{"agent.models.fix: was \"sonnet\", now \"opus\""},
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := SaveManifest(path, m); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	got, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if diff := cmp.Diff(m.Origin, got.Origin); diff != "" {
		t.Errorf("Origin (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{}, got.Requires); diff != "" {
		t.Errorf("Requires (-want +got):\n%s", diff)
	}
	assertExactJSONKeys(t, got.Origin, []string{
		"run_id", "workspace", "tree", "node", "visit", "start_seq",
		"inherited_usd", "inherited_estimated", "deviations",
	})
}

func TestForksDir(t *testing.T) {
	got, err := ForksDir("/work/repo")
	if err != nil {
		t.Fatalf("ForksDir: %v", err)
	}
	if want := filepath.Join("/work/repo", ".belay", "forks"); got != want {
		t.Errorf("ForksDir = %q, want %q", got, want)
	}

	// A relative workspace resolves against whatever directory belay runs
	// in, which is the mistake NewLayout refuses too.
	if _, err := ForksDir("work/repo"); !errors.Is(err, ErrInvalidPathSegment) {
		t.Errorf("ForksDir(relative) error = %v, want ErrInvalidPathSegment", err)
	}
}
