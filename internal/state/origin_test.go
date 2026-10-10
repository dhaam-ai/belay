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

// TestManifest_SetGiveUpFloorRecordsTheCapabilityAForkNeeds: a fork of a fix loop
// that gave up runs with graph.give_up one above the rounds used, and the
// round is the fork's own decision, not anything its belay.yaml says. So it
// is pinned, and it is a control field: a belay that dropped it would resume
// the fork under belay.yaml's graph.give_up, and the fix node would give up
// again without calling the agent -- the very thing the fork was made to get
// past. Naming the capability turns that into a refusal that says to upgrade.
func TestManifest_SetGiveUpFloorRecordsTheCapabilityAForkNeeds(t *testing.T) {
	m := NewManifest(time.Now(), "fork-1", "/work", "goal", config.Default())

	m.SetGiveUpFloor(4)

	if m.GiveUpFloor != 4 {
		t.Errorf("GiveUpFloor = %d, want 4", m.GiveUpFloor)
	}
	if diff := cmp.Diff([]string{CapabilityForkGiveUp}, m.Requires); diff != "" {
		t.Errorf("Requires (-want +got):\n%s", diff)
	}
	m.SetGiveUpFloor(4)
	if diff := cmp.Diff([]string{CapabilityForkGiveUp}, m.Requires); diff != "" {
		t.Errorf("Requires after a second SetGiveUpFloor (-want +got):\n%s", diff)
	}

	// And a manifest that requires it loads: this belay knows the name.
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := SaveManifest(path, m); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	loaded, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest refused a manifest requiring %q: %v", CapabilityForkGiveUp, err)
	}
	if loaded.GiveUpFloor != 4 {
		t.Errorf("GiveUpFloor after a round trip = %d, want 4", loaded.GiveUpFloor)
	}
}

// TestManifestWithoutAGiveUpPinCarriesNone: every run that is not such a
// fork writes the manifest it wrote before the field existed.
func TestManifestWithoutAGiveUpPinCarriesNone(t *testing.T) {
	m := NewManifest(time.Now(), "run-1", "/work", "goal", config.Default())
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := fields["give_up_floor"]; ok {
		t.Errorf("a run with no give_up pin writes one: %s", raw)
	}
	if len(m.Requires) != 0 {
		t.Errorf("Requires = %v, want none", m.Requires)
	}
}

// TestPinnedConfigGivesAForkAtLeastItsRound: the pin is a floor, not an
// override. A resumed fork never has fewer rounds than it was given, but
// graph.give_up is a limit like the others, and raising it in belay.yaml and
// resuming still raises it.
func TestPinnedConfigGivesAForkAtLeastItsRound(t *testing.T) {
	tests := []struct {
		name       string
		pin, live  int
		wantGiveUp int
	}{
		{name: "no pin leaves graph.give_up live", pin: 0, live: 2, wantGiveUp: 2},
		{name: "the pin holds against the file", pin: 3, live: 2, wantGiveUp: 3},
		{name: "a raised graph.give_up still raises it", pin: 3, live: 5, wantGiveUp: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManifest(time.Now(), "fork-1", "/work", "goal", config.Default())
			if tt.pin > 0 {
				m.SetGiveUpFloor(tt.pin)
			}
			live := config.Default()
			live.Graph.GiveUp = tt.live
			if got := m.PinnedConfig(live).Graph.GiveUp; got != tt.wantGiveUp {
				t.Errorf("graph.give_up = %d, want %d", got, tt.wantGiveUp)
			}
			if got := m.GiveUpFor(tt.live); got != tt.wantGiveUp {
				t.Errorf("GiveUpFor(%d) = %d, want %d", tt.live, got, tt.wantGiveUp)
			}
		})
	}
}
