//go:build fixrate

package fixrate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// TestRunBelay_ManifestIsWrittenLast pins the ordering Wave 0b established
// everywhere else: state.json is written before manifest.json, so a run
// whose creation fails part-way is never discoverable.
//
// The manifest is what makes a run findable — internal/cli's createRun
// says so at the point it writes one ("Only now, with the run findable"),
// and every reader of a run reads the manifest first (see
// state.Store.Preflight). A manifest sitting beside a missing state.json
// therefore advertises a run that cannot be loaded, which `belay runs`
// lists and `belay resume` then fails on.
//
// The test makes the state.json write fail deterministically by putting a
// directory where the file belongs: state.SaveState writes atomically
// (temp file, then rename), and no rename can replace a directory. With
// the writes in the wrong order the manifest is already on disk by then,
// which is exactly what this asserts against.
func TestRunBelay_ManifestIsWrittenLast(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	runDir := filepath.Join(workspace, ".belay", "runs", belayRunID)
	// A directory exactly where state.json belongs: the atomic rename
	// that would create the file cannot succeed.
	if err := os.MkdirAll(filepath.Join(runDir, "state.json"), 0o750); err != nil {
		t.Fatalf("seeding an unwritable state.json path: %v", err)
	}

	_, err := runBelay(t.Context(), workspace, &belaytest.FakeAgent{}, nil, nil)
	if err == nil {
		t.Fatal("runBelay() = nil error, want the state.json write to fail")
	}

	manifestPath := filepath.Join(runDir, "manifest.json")
	if _, statErr := os.Stat(manifestPath); statErr == nil {
		t.Errorf("runBelay() left %s on disk after failing to write state.json; "+
			"the manifest must be written LAST so a half-made run is never discoverable", manifestPath)
	}
}
