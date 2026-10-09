package write

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/graph"
)

// fakeChanges is a graph.ChangeSource answering from a fixed list.
type fakeChanges struct {
	paths []string
	err   error
	dirs  []string
}

func (f *fakeChanges) Changes(_ context.Context, dir string) ([]string, error) {
	f.dirs = append(f.dirs, dir)
	return f.paths, f.err
}

// The reviewer's evidence: an agent that edited infra/main.tf and belay.yaml
// and reported only src/app.py. The workspace's own answer is recorded
// beside the report, every file is verified and rendered into the diff, and
// the two the agent left out are named.
func TestRunRecordsWhatTheWorkspaceSaysChangedBesideTheReport(t *testing.T) {
	rc, root := newRC(t, 3, []string{"src/app.py"})
	mustWrite(t, filepath.Join(root, "src", "app.py"), "x = 1\n")
	mustWrite(t, filepath.Join(root, "infra", "main.tf"), "resource \"evil\" { backdoor = true }\n")
	mustWrite(t, filepath.Join(root, "belay.yaml"), "version: 1\npolicy: {}\n")
	source := &fakeChanges{paths: []string{"belay.yaml", "infra/main.tf", "src/app.py"}}
	rc.Changes = source

	got, err := New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Patch.Code == nil {
		t.Fatalf("Patch.Code is nil (note %q)", got.Note)
	}
	if diff := cmp.Diff([]string{"belay.yaml", "infra/main.tf", "src/app.py"}, got.Patch.Code.ChangedFiles); diff != "" {
		t.Errorf("Code.ChangedFiles (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"belay.yaml", "infra/main.tf"}, got.Patch.Code.Unreported); diff != "" {
		t.Errorf("Code.Unreported (-want +got):\n%s", diff)
	}
	if want := []string{rc.Workspace}; !cmp.Equal(source.dirs, want) {
		t.Errorf("the source was asked about %q, want the run's workspace %q", source.dirs, want)
	}
	if artifact := readArtifact(t, rc, got.Patch.Code.LastDiff); !strings.Contains(artifact, "backdoor = true") {
		t.Errorf("the diff artifact leaves out the unreported edit:\n%s", artifact)
	}
	for _, fragment := range []string{"2 the agent did not report", "belay.yaml", "infra/main.tf"} {
		if !strings.Contains(got.Note, fragment) {
			t.Errorf("Note %q never says %q", got.Note, fragment)
		}
	}
}

// A report spelled differently from the file's true name is the same file
// on a disk that folds case, and must not be recorded twice or let the true
// name pass as unreported. On a disk that does not fold case it names a
// different, absent file, and is recorded as the claim it is.
func TestRunFoldsAMisCasedReportIntoTheTrueName(t *testing.T) {
	rc, root := newRC(t, 3, []string{"Infra/main.tf"})
	mustWrite(t, filepath.Join(root, "infra", "main.tf"), "resource \"evil\" {}\n")
	rc.Changes = &fakeChanges{paths: []string{"infra/main.tf"}}
	_, err := os.Lstat(filepath.Join(root, "INFRA", "MAIN.TF"))
	folds := err == nil

	got, err := New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantFiles, wantUnreported := []string{"infra/main.tf"}, []string(nil)
	if !folds {
		wantFiles, wantUnreported = []string{"Infra/main.tf", "infra/main.tf"}, []string{"infra/main.tf"}
	}
	if diff := cmp.Diff(wantFiles, got.Patch.Code.ChangedFiles); diff != "" {
		t.Errorf("Code.ChangedFiles on a disk that folds case=%v (-want +got):\n%s", folds, diff)
	}
	if diff := cmp.Diff(wantUnreported, got.Patch.Code.Unreported); diff != "" {
		t.Errorf("Code.Unreported on a disk that folds case=%v (-want +got):\n%s", folds, diff)
	}
}

// A file reported unreported on an earlier pass is still unreported later,
// even though the fix loop has since carried it into the report it hands
// on: the record of what the agent left out must not shrink as the loop
// goes round.
func TestRunKeepsAFileUnreportedAcrossPasses(t *testing.T) {
	rc, root := newRC(t, 7, []string{"infra/main.tf", "src/app.py"})
	rc.State.Code.Unreported = []string{"infra/main.tf"}
	mustWrite(t, filepath.Join(root, "src", "app.py"), "x = 2\n")
	mustWrite(t, filepath.Join(root, "infra", "main.tf"), "resource \"evil\" {}\n")
	rc.Changes = &fakeChanges{paths: []string{"infra/main.tf", "src/app.py"}}

	got, err := New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if diff := cmp.Diff([]string{"infra/main.tf"}, got.Patch.Code.Unreported); diff != "" {
		t.Errorf("Code.Unreported (-want +got):\n%s", diff)
	}
}

// A source that cannot answer is no verdict, never a fall back to the
// agent's word: an answer the agent could make fail on purpose must not
// turn into its own report. A source that says the run keeps no record is
// the one case that falls back, and the note says so.
func TestRunWithAWorkspaceThatCannotOrDoesNotAnswer(t *testing.T) {
	t.Run("a failure is no verdict", func(t *testing.T) {
		rc, root := newRC(t, 3, []string{"src/app.py"})
		mustWrite(t, filepath.Join(root, "src", "app.py"), "x = 1\n")
		rc.Changes = &fakeChanges{err: errors.New("git: the working tree's git directory is not this repository's")}

		got, err := New().Run(context.Background(), rc)
		if err == nil {
			t.Fatalf("Run returned no error and %+v; a change nobody could check is not a verified one", got)
		}
		if got.Patch.Code != nil {
			t.Errorf("Patch.Code = %+v, want none: no verdict was reached", got.Patch.Code)
		}
	})
	t.Run("no record falls back to the report and says so", func(t *testing.T) {
		rc, root := newRC(t, 3, []string{"src/app.py"})
		mustWrite(t, filepath.Join(root, "src", "app.py"), "x = 1\n")
		rc.Changes = &fakeChanges{err: fmt.Errorf("%w: a fork's copy keeps none", graph.ErrUnrecorded)}

		got, err := New().Run(context.Background(), rc)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if diff := cmp.Diff([]string{"src/app.py"}, got.Patch.Code.ChangedFiles); diff != "" {
			t.Errorf("Code.ChangedFiles (-want +got):\n%s", diff)
		}
		if len(got.Patch.Code.Unreported) != 0 {
			t.Errorf("Code.Unreported = %q, want none: nothing checked the report", got.Patch.Code.Unreported)
		}
		if !strings.Contains(got.Note, "only the agent's report") {
			t.Errorf("Note %q never says the change is only what the agent reported", got.Note)
		}
	})
}
