package code_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/belay-dev/belay/internal/nodes/code"
	"github.com/belay-dev/belay/pkg/belay"
	"github.com/google/go-cmp/cmp"
)

const sampleDiff = `diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -1,3 +1,7 @@
 package calc
+
+func Divide(a, b float64) float64 { return a / b }
`

// fence is spelled here rather than inlined because the raw strings these
// tests build cannot contain a backtick.
const fence = "```"

// artifacts lists the file names under the run's artifacts/ directory,
// sorted, so a test can assert on exactly what a run created.
func (f *fixture) artifacts(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.rc.Layout.ArtifactsDir())
	if err != nil {
		t.Fatalf("read artifacts dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// readArtifact reads one artifact by bare name.
func (f *fixture) readArtifact(t *testing.T, name string) string {
	t.Helper()
	data, err := f.rc.ReadArtifact(name)
	if err != nil {
		t.Fatalf("ReadArtifact(%q): %v", name, err)
	}
	return string(data)
}

func TestRunArchivesTheProposedDiff(t *testing.T) {
	reply := "Added Divide.\n\n" + fence + "diff\n" + sampleDiff + fence + "\n"
	f := newFixture(t, belay.AgentResponse{Text: reply, SessionID: "s1"})
	f.rc.Step = 3

	res, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := filepath.Join("artifacts", "diff-0003.patch")
	if got := res.Patch.Code.LastDiff; got != want {
		t.Fatalf("Patch.Code.LastDiff = %q, want %q", got, want)
	}
	if diff := cmp.Diff([]string{"calc.go"}, res.Patch.Code.ChangedFiles); diff != "" {
		t.Errorf("ChangedFiles mismatch (-want +got):\n%s", diff)
	}
	if got := f.readArtifact(t, "diff-0003.patch"); got != sampleDiff {
		t.Errorf("archived diff =\n%q\nwant\n%q", got, sampleDiff)
	}
	if diff := cmp.Diff([]string{"diff-0003.patch", "plan.md"}, f.artifacts(t)); diff != "" {
		t.Errorf("artifacts dir mismatch (-want +got):\n%s", diff)
	}
	if !strings.Contains(res.Note, "1 file") {
		t.Errorf("Note = %q, want it to report the changed-file count", res.Note)
	}
}

// A fix loop returns to the code node at a later step; attempt one's diff
// must still be on disk afterwards.
func TestRunNumbersDiffsSoAFixLoopDoesNotOverwrite(t *testing.T) {
	first := "attempt one\n\n" + fence + "diff\n" + sampleDiff + fence + "\n"
	secondDiff := strings.ReplaceAll(sampleDiff, "calc.go", "calc2.go")
	second := "attempt two\n\n" + fence + "diff\n" + secondDiff + fence + "\n"

	f := newFixture(t, belay.AgentResponse{Text: first, SessionID: "s1"})
	f.agent.Responses = []belay.AgentResponse{
		{Text: first, SessionID: "s1"},
		{Text: second, SessionID: "s1"},
	}

	f.rc.Step = 3
	res1, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run (step 3): %v", err)
	}

	// The dispatcher applies the patch and comes back later in the run.
	if err := res1.Patch.Apply(&f.rc.State); err != nil {
		t.Fatalf("Patch.Apply: %v", err)
	}
	f.rc.Step = 9
	f.rc.Attempt = 2
	res2, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run (step 9): %v", err)
	}

	if got, want := res2.Patch.Code.LastDiff, filepath.Join("artifacts", "diff-0009.patch"); got != want {
		t.Fatalf("second LastDiff = %q, want %q", got, want)
	}
	if diff := cmp.Diff([]string{"diff-0003.patch", "diff-0009.patch", "plan.md"}, f.artifacts(t)); diff != "" {
		t.Fatalf("attempt one's diff did not survive (-want +got):\n%s", diff)
	}
	if got := f.readArtifact(t, "diff-0003.patch"); got != sampleDiff {
		t.Errorf("diff-0003.patch was rewritten:\n%s", got)
	}
	if diff := cmp.Diff([]string{"calc2.go"}, res2.Patch.Code.ChangedFiles); diff != "" {
		t.Errorf("second ChangedFiles mismatch (-want +got):\n%s", diff)
	}
}

// Re-running the same step is what the dispatcher does after a crash
// inside a node; it must not accumulate a second copy of anything.
func TestRunIsIdempotentOnReRun(t *testing.T) {
	reply := "done\n\n" + fence + "diff\n" + sampleDiff + fence + "\n"
	f := newFixture(t, belay.AgentResponse{Text: reply, SessionID: "s1"})
	f.rc.Step = 4

	first, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run (first): %v", err)
	}
	beforeArtifacts := f.artifacts(t)
	beforePrompt := f.nodeFile(t, "prompt.txt")

	// The crash happened before the dispatcher persisted the patch, so
	// State is unchanged and Attempt increments.
	f.rc.Attempt = 2
	second, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run (re-run): %v", err)
	}

	if diff := cmp.Diff(beforeArtifacts, f.artifacts(t)); diff != "" {
		t.Errorf("re-running duplicated artifacts (-first +second):\n%s", diff)
	}
	if got := f.nodeFile(t, "prompt.txt"); got != beforePrompt {
		t.Error("prompt.txt changed across an identical re-run")
	}
	if diff := cmp.Diff(*first.Patch.Code, *second.Patch.Code); diff != "" {
		t.Errorf("re-run produced a different Code patch (-first +second):\n%s", diff)
	}
	if got := f.readArtifact(t, "diff-0004.patch"); got != sampleDiff {
		t.Errorf("diff-0004.patch content changed across a re-run:\n%s", got)
	}
}

// A reply with no diff must not clear the diff already on the blackboard:
// LastDiff means "the most recent diff", and that is still attempt one's.
func TestRunWithoutADiffCarriesTheLastOneForward(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "The plan needs no change.", SessionID: "s2"},
		func(f *fixture) {
			f.rc.State.Code.SessionID = "s1"
			f.rc.State.Code.LastDiff = "artifacts/diff-0003.patch"
			f.rc.State.Code.ChangedFiles = []string{"calc.go"}
		})

	res, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := res.Patch.Code.LastDiff; got != "artifacts/diff-0003.patch" {
		t.Errorf("LastDiff = %q, want the previous diff carried forward", got)
	}
	if diff := cmp.Diff([]string{"calc.go"}, res.Patch.Code.ChangedFiles); diff != "" {
		t.Errorf("ChangedFiles mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"plan.md"}, f.artifacts(t)); diff != "" {
		t.Errorf("an empty reply wrote a diff artifact (-want +got):\n%s", diff)
	}
	if !strings.Contains(res.Note, "proposed no change") {
		t.Errorf("Note = %q, want it to say no new change was proposed", res.Note)
	}
	if !strings.Contains(res.Note, "resumed session") {
		t.Errorf("Note = %q, want it to record that the session was resumed", res.Note)
	}
}

// The Patch must not share a backing array with the RunContext snapshot.
func TestRunPatchDoesNotAliasStateChangedFiles(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "no change"}, func(f *fixture) {
		f.rc.State.Code.ChangedFiles = []string{"calc.go"}
	})

	res, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res.Patch.Code.ChangedFiles[0] = "mutated.go"
	if f.rc.State.Code.ChangedFiles[0] != "calc.go" {
		t.Fatal("mutating the Patch reached back into the RunContext's State snapshot")
	}
}

// The prompt's continuation section is what stops a resumed session from
// re-proposing work it already did.
func TestPromptCarriesTheContinuationContext(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "ok"}, func(f *fixture) {
		f.rc.State.Code.SessionID = "s1"
		f.rc.State.Code.LastDiff = "artifacts/diff-0003.patch"
		f.rc.State.Code.ChangedFiles = []string{"calc.go", "calc_test.go"}
	})

	if _, err := code.New().Run(context.Background(), f.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	prompt := f.agent.Calls()[0].Prompt
	for _, want := range []string{"Continuing your earlier work", "artifacts/diff-0003.patch", "- calc.go", "- calc_test.go"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("continuation prompt is missing %q:\n%s", want, prompt)
		}
	}
}

func TestPromptOmitsContinuationOnAFirstCall(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "ok"})

	if _, err := code.New().Run(context.Background(), f.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prompt := f.agent.Calls()[0].Prompt; strings.Contains(prompt, "Continuing your earlier work") {
		t.Errorf("a first call sent a continuation section:\n%s", prompt)
	}
}

func TestExtractionAndChangedFiles(t *testing.T) {
	// The context lines here are the hazard: an unchanged fence inside a
	// Markdown file appears as " ```", which trims to a bare fence.
	markdownDiff := "diff --git a/README.md b/README.md\n" +
		"--- a/README.md\n" +
		"+++ b/README.md\n" +
		"@@ -1,4 +1,5 @@\n" +
		" # Title\n" +
		" " + fence + "go\n" +
		" fmt.Println()\n" +
		" " + fence + "\n" +
		"+trailing line\n"

	tests := []struct {
		name      string
		reply     string
		wantDiff  string
		wantFiles []string
	}{
		{
			name:      "fenced diff block",
			reply:     "Summary.\n\n" + fence + "diff\n" + sampleDiff + fence + "\n\nDone.",
			wantDiff:  sampleDiff,
			wantFiles: []string{"calc.go"},
		},
		{
			name:      "patch tag is accepted too",
			reply:     fence + "patch\n" + sampleDiff + fence,
			wantDiff:  sampleDiff,
			wantFiles: []string{"calc.go"},
		},
		{
			name:      "unfenced diff is still recovered",
			reply:     "Here is the change:\n\n" + sampleDiff,
			wantDiff:  sampleDiff,
			wantFiles: []string{"calc.go"},
		},
		{
			name:      "unterminated fence is still recovered",
			reply:     fence + "diff\n" + sampleDiff,
			wantDiff:  sampleDiff,
			wantFiles: []string{"calc.go"},
		},
		{
			name:      "a fence inside the diff body does not close the block",
			reply:     fence + "diff\n" + markdownDiff + fence + "\n",
			wantDiff:  markdownDiff,
			wantFiles: []string{"README.md"},
		},
		{
			name:      "no diff at all",
			reply:     "The plan requires no code change.",
			wantDiff:  "",
			wantFiles: []string{},
		},
		{
			name:      "empty fenced block falls through to the bare diff",
			reply:     fence + "diff\n" + fence + "\n\n" + sampleDiff,
			wantDiff:  sampleDiff,
			wantFiles: []string{"calc.go"},
		},
		{
			name: "rename records the new name, deletion records the removed file",
			reply: fence + "diff\n" +
				"diff --git a/old.go b/new.go\n" +
				"similarity index 100%\n" +
				"rename from old.go\n" +
				"rename to new.go\n" +
				"diff --git a/gone.go b/gone.go\n" +
				"deleted file mode 100644\n" +
				"--- a/gone.go\n" +
				"+++ /dev/null\n" +
				fence + "\n",
			wantFiles: []string{"new.go", "gone.go"},
		},
		{
			name: "a file is listed once even with two headers naming it",
			reply: fence + "diff\n" +
				"diff --git a/calc.go b/calc.go\n--- a/calc.go\n+++ b/calc.go\n@@ -1 +1 @@\n-a\n+b\n" +
				fence + "\n",
			wantFiles: []string{"calc.go"},
		},
		{
			name: "git diff --no-prefix headers resolve",
			reply: fence + "diff\n" +
				"diff --git main.go main.go\n" +
				"--- main.go\n" +
				"+++ main.go\n" +
				"@@ -1 +1 @@\n-a\n+b\n" +
				fence + "\n",
			wantFiles: []string{"main.go"},
		},
		{
			name: "a POSIX diff timestamp is stripped from the path",
			reply: "--- calc.go\t2026-01-01 00:00:00.000000000 +0000\n" +
				"+++ calc.go\t2026-01-02 00:00:00.000000000 +0000\n" +
				"@@ -1 +1 @@\n-a\n+b\n",
			wantFiles: []string{"calc.go"},
		},
		{
			name: "paths with spaces resolve",
			reply: fence + "diff\n" +
				"diff --git a/my dir/file.go b/my dir/file.go\n" +
				"--- a/my dir/file.go\n" +
				"+++ b/my dir/file.go\n" +
				"@@ -1 +1 @@\n-a\n+b\n" +
				fence + "\n",
			wantFiles: []string{"my dir/file.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, belay.AgentResponse{Text: tt.reply})
			f.rc.Step = 7

			res, err := code.New().Run(context.Background(), f.rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tt.wantDiff != "" {
				if got := f.readArtifact(t, "diff-0007.patch"); got != tt.wantDiff {
					t.Errorf("archived diff =\n%q\nwant\n%q", got, tt.wantDiff)
				}
			}
			if len(tt.wantFiles) == 0 {
				if got := res.Patch.Code.ChangedFiles; len(got) != 0 {
					t.Errorf("ChangedFiles = %v, want none", got)
				}
				return
			}
			if diff := cmp.Diff(tt.wantFiles, res.Patch.Code.ChangedFiles); diff != "" {
				t.Errorf("ChangedFiles mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
