package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// artifactLayout builds a run layout under a temporary directory.
func artifactLayout(t *testing.T) Layout {
	t.Helper()
	l, err := NewLayout(t.TempDir(), "20260905T101112Z-ab12cd34")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	return l
}

// TestReplaceArtifactWritesAndReplaces covers what a writer outside a
// dispatch needs: create the artifact, and replace it whole. Replacing
// matters more than creating — the plan is rewritten in place, and a write
// that only truncated as far as the new bytes reached would leave the tail
// of the old plan under the new one.
func TestReplaceArtifactWritesAndReplaces(t *testing.T) {
	l := artifactLayout(t)

	if err := ReplaceArtifact(l, "plan.md", []byte("# Plan\n\n1. The long first plan.\n")); err != nil {
		t.Fatalf("ReplaceArtifact: %v", err)
	}
	if err := ReplaceArtifact(l, "plan.md", []byte("# Plan\n\n1. Short.\n")); err != nil {
		t.Fatalf("ReplaceArtifact again: %v", err)
	}

	abs, err := l.ArtifactPath("plan.md")
	if err != nil {
		t.Fatalf("ArtifactPath: %v", err)
	}
	got, err := os.ReadFile(abs) //nolint:gosec // test-owned temp dir
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if want := "# Plan\n\n1. Short.\n"; string(got) != want {
		t.Errorf("artifact holds %q, want %q", got, want)
	}

	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("mode = %v, want %v: a run's artifacts can hold an agent's own words", perm, filePerm)
	}

	// Nothing but the artifact: an atomic write leaves no temporary file
	// behind for the next reader of artifacts/ to trip over.
	entries, err := os.ReadDir(l.ArtifactsDir())
	if err != nil {
		t.Fatalf("read artifacts dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "plan.md" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("artifacts dir holds %v, want only plan.md", names)
	}
}

// The artifacts directory belongs to the run, and a writer outside the
// dispatcher may reach the run before anything has created it.
func TestReplaceArtifactCreatesTheDirectory(t *testing.T) {
	l := artifactLayout(t)
	if _, err := os.Stat(l.ArtifactsDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test setup: artifacts dir already exists (%v)", err)
	}

	if err := ReplaceArtifact(l, "plan-replaced-1.md", []byte("# Plan\n")); err != nil {
		t.Fatalf("ReplaceArtifact: %v", err)
	}

	if _, err := os.Stat(filepath.Join(l.ArtifactsDir(), "plan-replaced-1.md")); err != nil {
		t.Errorf("artifact was not written: %v", err)
	}
}

// A name is one path component. Anything that could put a file outside the
// run's artifacts directory is refused rather than cleaned, exactly as
// Layout.ArtifactPath refuses it for every other writer.
//
// "." is not in this table although it resolves to the artifacts directory
// itself: validateSegment accepts it on purpose today, for every artifact
// writer there is, and reopening that belongs with whichever change gives
// an artifact name to a person to type.
func TestReplaceArtifactRefusesAnEscapingName(t *testing.T) {
	l := artifactLayout(t)

	for _, name := range []string{"../plan.md", "nested/plan.md", ""} {
		t.Run(name, func(t *testing.T) {
			err := ReplaceArtifact(l, name, []byte("# Plan\n"))
			if !errors.Is(err, ErrInvalidPathSegment) {
				t.Errorf("ReplaceArtifact(%q) = %v, want ErrInvalidPathSegment", name, err)
			}
		})
	}
}
