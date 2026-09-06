package belay_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/pkg/belay"
)

func TestWorkspaceJSONRoundTrip(t *testing.T) {
	t.Parallel()

	tests := map[string]belay.Workspace{
		"zero value": {},
		"ephemeral copy": {
			ID:        "run-42-candidate-0",
			Dir:       "/tmp/belay/run-42/candidate-0",
			Ephemeral: true,
		},
		"no-isolation passthrough": {
			ID:        "run-7",
			Dir:       "/home/user/repo",
			Ephemeral: false,
		},
	}

	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			encoded, err := json.Marshal(want)
			if err != nil {
				t.Fatalf("json.Marshal returned error: %v", err)
			}
			var got belay.Workspace
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("round trip through %s changed the value (-want +got):\n%s", encoded, diff)
			}
		})
	}
}

// TestWorkspaceIsPlainData is a compile-time-flavored sanity check for the
// design rationale documented on Workspace: it must decode from JSON alone,
// with no side channel, since a resumed run reconstructs it from the
// journal rather than from the Isolator that originally created it.
func TestWorkspaceIsPlainData(t *testing.T) {
	t.Parallel()

	const journalEntry = `{"id":"run-1","dir":"/work/run-1","ephemeral":true}`

	var ws belay.Workspace
	if err := json.Unmarshal([]byte(journalEntry), &ws); err != nil {
		t.Fatalf("json.Unmarshal(%s) returned error: %v", journalEntry, err)
	}

	want := belay.Workspace{ID: "run-1", Dir: "/work/run-1", Ephemeral: true}
	if diff := cmp.Diff(want, ws); diff != "" {
		t.Errorf("decoding a bare journal entry produced a different Workspace (-want +got):\n%s", diff)
	}
}
