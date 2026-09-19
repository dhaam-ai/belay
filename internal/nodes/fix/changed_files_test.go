package fix_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/nodes/code"
	"github.com/dhaam-ai/belay/internal/nodes/fix"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// changedFilesReply is a repair reply that ends in the fenced block the code
// node's prompt defines, naming paths one per line.
func changedFilesReply(paths ...string) string {
	var b strings.Builder
	b.WriteString("Repaired the limiter's off-by-one.\n\n```changed-files\n")
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString("```\n")
	return b.String()
}

// TestFixRecordsTheFilesItsRepairTouched pins what a repair adds to
// State.Code.ChangedFiles: the union of what was recorded and what the reply
// reports, sorted and without duplicates.
//
// Every row also runs the node a second time on the state its own patch
// produced, with the same reply. That is the re-run a crash between applying
// the patch and journalling node_finished leads to (ADR-0002), and it must
// land on the same list, not a longer or reordered one.
func TestFixRecordsTheFilesItsRepairTouched(t *testing.T) {
	tests := []struct {
		name  string
		prior []string
		reply string
		want  []string
	}{
		{
			name:  "a file the repair touched joins the files code touched",
			prior: []string{"a.go"},
			reply: changedFilesReply("b.go"),
			want:  []string{"a.go", "b.go"},
		},
		{
			name:  "a file already recorded is listed once",
			prior: []string{"a.go"},
			reply: changedFilesReply("c.go", "a.go"),
			want:  []string{"a.go", "c.go"},
		},
		{
			name:  "the same file spelled another way is listed once",
			prior: []string{"a.go"},
			reply: changedFilesReply("./a.go", "sub/../b.go"),
			want:  []string{"a.go", "b.go"},
		},
		{
			name:  "the union is sorted whatever order either list came in",
			prior: []string{"z.go", "a.go"},
			reply: changedFilesReply("m.go"),
			want:  []string{"a.go", "m.go", "z.go"},
		},
		{
			name:  "a report when nothing was recorded yet",
			prior: nil,
			reply: changedFilesReply("b.go"),
			want:  []string{"b.go"},
		},
		{
			name:  "a reply that reports nothing keeps what was recorded",
			prior: []string{"a.go", "b.go"},
			reply: "Repaired the limiter's off-by-one.",
			want:  []string{"a.go", "b.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := failingState(0)
			st.Code = state.Code{
				SessionID:    "sess-code-1",
				LastDiff:     "artifacts/diff-0004.patch",
				ChangedFiles: slices.Clone(tt.prior),
			}
			h := newHarness(t, st)
			h.agent.Responses = []belay.AgentResponse{{Text: tt.reply, SessionID: "sess-42"}}

			res, err := fix.New().Run(context.Background(), h.rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			// The RunContext's State is the dispatcher's snapshot, handed to
			// the node read-only. A union sorted in place would reorder it.
			if diff := cmp.Diff(tt.prior, h.rc.State.Code.ChangedFiles); diff != "" {
				t.Errorf("Run changed the RunContext's own ChangedFiles (-before +after):\n%s", diff)
			}

			applied := st
			applied.Code.ChangedFiles = slices.Clone(st.Code.ChangedFiles)
			if err := res.Patch.Apply(&applied); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if diff := cmp.Diff(tt.want, applied.Code.ChangedFiles); diff != "" {
				t.Errorf("ChangedFiles after the repair (-want +got):\n%s", diff)
			}
			// Patch.Code replaces Code wholesale, so the rest of it must be
			// carried through: fix resumes SessionID, and LastDiff names the
			// write node's record.
			if applied.Code.SessionID != "sess-code-1" || applied.Code.LastDiff != "artifacts/diff-0004.patch" {
				t.Errorf("Code = %+v, want SessionID and LastDiff as they were", applied.Code)
			}

			h.rc.State = applied
			again, err := fix.New().Run(context.Background(), h.rc)
			if err != nil {
				t.Fatalf("re-Run: %v", err)
			}
			if err := again.Patch.Apply(&applied); err != nil {
				t.Fatalf("re-Apply: %v", err)
			}
			if diff := cmp.Diff(tt.want, applied.Code.ChangedFiles); diff != "" {
				t.Errorf("ChangedFiles after re-running on the patched state (-want +got):\n%s", diff)
			}
		})
	}
}

// quotedTestifyDiff is a repair reply that quotes the test failure it was
// given: testify prints its comparison as a unified diff, and the fix prompt
// hands the agent that output and asks what was broken. The reply also shows
// the edit it made as a diff, in the same prose.
const quotedTestifyDiff = "The limiter admitted four requests, not five. The suite said:\n\n" +
	"Error:          Not equal:\n" +
	"                Diff:\n" +
	"--- Expected\n" +
	"+++ Actual\n" +
	"@@ -1 +1 @@\n" +
	"-5\n" +
	"+4\n" +
	"\n" +
	"So I raised the burst:\n\n" +
	"--- a/limiter.go\n" +
	"+++ b/limiter.go\n" +
	"@@ -7 +7 @@\n" +
	"-burst := 4\n" +
	"+burst := 5\n"

// quotedGoldenDiff is a repair reply that quotes a golden-file failure. The
// paths in it are absolute, because that is how `diff -u` and Go's own test
// output name a temporary directory.
const quotedGoldenDiff = "The golden file no longer matched:\n\n" +
	"diff -u testdata/out.golden /tmp/TestRender123/out\n" +
	"--- /Users/me/repo/testdata/out.golden\n" +
	"+++ /tmp/TestRender123/out\n" +
	"@@ -1 +1 @@\n" +
	"-old\n" +
	"+new\n" +
	"\n" +
	"I regenerated it.\n"

// TestFixReadsOnlyTheChangedFilesBlock pins that a repair's reply is read for
// the block the prompt asked for and nothing else.
//
// The code node's parser also salvages paths from any unified diff in a reply,
// which is right there: nothing in a code prompt quotes a diff. A fix prompt
// quotes the failure it wants repaired, so a diff in a repair's reply is as
// likely to be the failure as the repair — and reading it either invents a
// file the run never changed or, when the quoted paths are absolute, refuses
// the whole report and ends the run after the repair was made and paid for.
func TestFixReadsOnlyTheChangedFilesBlock(t *testing.T) {
	tests := []struct {
		name  string
		prior []string
		reply string
		want  []string
	}{
		{
			name:  "a quoted test failure is not a change report",
			prior: []string{"a.go"},
			reply: quotedTestifyDiff,
			want:  []string{"a.go"},
		},
		{
			name:  "a quoted golden-file diff is not a refusal",
			prior: []string{"a.go"},
			reply: quotedGoldenDiff,
			want:  []string{"a.go"},
		},
		{
			name:  "the block wins over a diff in the same reply",
			prior: []string{"a.go"},
			reply: quotedTestifyDiff + "\n" + changedFilesReply("limiter.go"),
			want:  []string{"a.go", "limiter.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := failingState(0)
			st.Code = state.Code{SessionID: "sess-code-1", ChangedFiles: slices.Clone(tt.prior)}
			h := newHarness(t, st)
			h.agent.Responses = []belay.AgentResponse{{Text: tt.reply, SessionID: "sess-42"}}

			res, err := fix.New().Run(context.Background(), h.rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			applied := st
			applied.Code.ChangedFiles = slices.Clone(st.Code.ChangedFiles)
			if err := res.Patch.Apply(&applied); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if diff := cmp.Diff(tt.want, applied.Code.ChangedFiles); diff != "" {
				t.Errorf("ChangedFiles after the repair (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFixRefusesAnUnusableChangedFilesReport: a reply naming a path outside
// the repository is refused whole, exactly as the code node refuses one,
// because a filtered report records a half-truth as fact. The exchange is
// still archived, since the edits it describes are on disk.
func TestFixRefusesAnUnusableChangedFilesReport(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "climbs out of the repository", path: "../outside.go"},
		{name: "absolute", path: "/etc/passwd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, failingState(0))
			h.agent.Responses = []belay.AgentResponse{{Text: changedFilesReply("b.go", tt.path)}}

			res, err := fix.New().Run(context.Background(), h.rc)
			if !errors.Is(err, code.ErrFileList) {
				t.Fatalf("err = %v, want one wrapping code.ErrFileList", err)
			}
			if diff := cmp.Diff(graph.Result{}, res); diff != "" {
				t.Errorf("Result on a refused report (-want +got):\n%s", diff)
			}
			if _, err := os.Stat(filepath.Join(h.nodeDir(t), "response.json")); err != nil {
				t.Errorf("response.json was not archived before the report was refused: %v", err)
			}
		})
	}
}
