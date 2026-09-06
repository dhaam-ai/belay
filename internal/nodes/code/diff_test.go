package code_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/nodes/code"
	"github.com/dhaam-ai/belay/pkg/belay"
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

// filesBlock builds the machine-readable block the prompt asks the agent
// for, from the same tag the node parses.
func filesBlock(paths ...string) string {
	return fence + "changed-files\n" + strings.Join(paths, "\n") + "\n" + fence + "\n"
}

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

// The happy path for the new contract: the reply's file list becomes
// ChangedFiles verbatim, and the prose is archived under a name that says
// what it is.
func TestRunArchivesTheAgentSummary(t *testing.T) {
	reply := "Added Divide to the calculator.\n\n" + filesBlock("calc.go", "calc_test.go")
	f := newFixture(t, belay.AgentResponse{Text: reply, SessionID: "s1"})
	f.rc.Step = 3

	res, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if diff := cmp.Diff([]string{"calc.go", "calc_test.go"}, res.Patch.Code.ChangedFiles); diff != "" {
		t.Errorf("ChangedFiles mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"plan.md", "summary-0003.md"}, f.artifacts(t)); diff != "" {
		t.Errorf("artifacts dir mismatch (-want +got):\n%s", diff)
	}
	if got := f.readArtifact(t, "summary-0003.md"); !strings.Contains(got, "Added Divide") {
		t.Errorf("summary artifact does not hold the agent's prose:\n%s", got)
	}
	// The write node owns LastDiff: it names the materialization record,
	// and this node has no patch to put there.
	if got := res.Patch.Code.LastDiff; got != "" {
		t.Errorf("LastDiff = %q; the code node must leave the diff record to the write node", got)
	}
	if !strings.Contains(res.Note, "2 file") {
		t.Errorf("Note = %q, want it to report the changed-file count", res.Note)
	}
}

// A fix loop returns to the code node at a later step; attempt one's
// summary must still be on disk afterwards.
func TestRunNumbersSummariesSoAFixLoopDoesNotOverwrite(t *testing.T) {
	first := "attempt one\n\n" + filesBlock("calc.go")
	second := "attempt two\n\n" + filesBlock("calc.go", "calc2.go")

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

	if diff := cmp.Diff([]string{"plan.md", "summary-0003.md", "summary-0009.md"}, f.artifacts(t)); diff != "" {
		t.Fatalf("attempt one's summary did not survive (-want +got):\n%s", diff)
	}
	if got := f.readArtifact(t, "summary-0003.md"); !strings.Contains(got, "attempt one") {
		t.Errorf("summary-0003.md was rewritten:\n%s", got)
	}
	if diff := cmp.Diff([]string{"calc.go", "calc2.go"}, res2.Patch.Code.ChangedFiles); diff != "" {
		t.Errorf("second ChangedFiles mismatch (-want +got):\n%s", diff)
	}
}

// Re-running the same step is what the dispatcher does after a crash
// inside a node; it must not accumulate a second copy of anything.
func TestRunIsIdempotentOnReRun(t *testing.T) {
	reply := "done\n\n" + filesBlock("calc.go")
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
	if got := f.readArtifact(t, "summary-0004.md"); !strings.Contains(got, "done") {
		t.Errorf("summary-0004.md content changed across a re-run:\n%s", got)
	}
}

// A reply that reports nothing must not clear the change already on the
// blackboard. The edits from the earlier pass are still on disk, and
// LastDiff still names the record the write node made of them.
func TestRunWithoutAReportCarriesTheLastChangeForward(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "The plan needs no change.", SessionID: "s2"},
		func(f *fixture) {
			f.rc.State.Code.SessionID = "s1"
			f.rc.State.Code.LastDiff = "artifacts/diff-0004.patch"
			f.rc.State.Code.ChangedFiles = []string{"calc.go"}
		})
	f.rc.Step = 5

	res, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := res.Patch.Code.LastDiff; got != "artifacts/diff-0004.patch" {
		t.Errorf("LastDiff = %q, want the previous record carried forward", got)
	}
	if diff := cmp.Diff([]string{"calc.go"}, res.Patch.Code.ChangedFiles); diff != "" {
		t.Errorf("ChangedFiles mismatch (-want +got):\n%s", diff)
	}
	if !strings.Contains(res.Note, "reported no change") {
		t.Errorf("Note = %q, want it to say no change was reported", res.Note)
	}
	if !strings.Contains(res.Note, "resumed session") {
		t.Errorf("Note = %q, want it to record that the session was resumed", res.Note)
	}
	// The prose is still evidence even when it reports nothing: it is the
	// agent's account of why it changed nothing.
	if diff := cmp.Diff([]string{"plan.md", "summary-0005.md"}, f.artifacts(t)); diff != "" {
		t.Errorf("artifacts dir mismatch (-want +got):\n%s", diff)
	}
}

// An explicitly empty block is not a claim that the set is empty; it is a
// reply that did not answer, and it must not erase a recorded change.
func TestRunTreatsAnEmptyBlockAsNoReport(t *testing.T) {
	reply := "Nothing to do.\n\n" + fence + "changed-files\n" + fence + "\n"
	f := newFixture(t, belay.AgentResponse{Text: reply}, func(f *fixture) {
		f.rc.State.Code.ChangedFiles = []string{"calc.go"}
	})

	res, err := code.New().Run(context.Background(), f.rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if diff := cmp.Diff([]string{"calc.go"}, res.Patch.Code.ChangedFiles); diff != "" {
		t.Errorf("an empty block erased the recorded change (-want +got):\n%s", diff)
	}
}

// The changed-files block is the primary source, and this is its contract.
func TestChangedFilesBlockIsTheSourceOfTruth(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		want  []string
	}{
		{
			name:  "one path per line",
			reply: "Summary.\n\n" + filesBlock("calc.go", "internal/calc/calc_test.go"),
			want:  []string{"calc.go", "internal/calc/calc_test.go"},
		},
		{
			name:  "surrounding prose is ignored",
			reply: "I edited calc.go and also README.md.\n\n" + filesBlock("calc.go"),
			want:  []string{"calc.go"},
		},
		{
			name:  "blank lines between entries are formatting, not entries",
			reply: fence + "changed-files\n\ncalc.go\n\n\ncalc_test.go\n\n" + fence + "\n",
			want:  []string{"calc.go", "calc_test.go"},
		},
		{
			name:  "surrounding whitespace on an entry is trimmed",
			reply: fence + "changed-files\n  calc.go  \n\tcalc_test.go\n" + fence + "\n",
			want:  []string{"calc.go", "calc_test.go"},
		},
		{
			name:  "paths are cleaned",
			reply: filesBlock("./calc.go", "internal//calc/calc.go", "a/../calc_test.go"),
			want:  []string{"calc.go", "internal/calc/calc.go", "calc_test.go"},
		},
		{
			name:  "a duplicate is listed once, in first-appearance order",
			reply: filesBlock("b.go", "a.go", "b.go", "./b.go"),
			want:  []string{"b.go", "a.go"},
		},
		{
			name:  "an indented opening fence is tolerated",
			reply: "  " + fence + "changed-files\ncalc.go\n" + fence + "\n",
			want:  []string{"calc.go"},
		},
		{
			name:  "the tag is matched case-insensitively",
			reply: fence + "Changed-Files\ncalc.go\n" + fence + "\n",
			want:  []string{"calc.go"},
		},
		{
			name:  "an unterminated block is still read",
			reply: fence + "changed-files\ncalc.go\ncalc_test.go",
			want:  []string{"calc.go", "calc_test.go"},
		},
		{
			name:  "only the first block is read",
			reply: filesBlock("calc.go") + "\nand also\n\n" + filesBlock("other.go"),
			want:  []string{"calc.go"},
		},
		{
			name: "the block beats a diff in the same reply",
			reply: "Summary.\n\n" + fence + "diff\n" + sampleDiff + fence + "\n\n" +
				filesBlock("calc.go", "calc_test.go"),
			want: []string{"calc.go", "calc_test.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, belay.AgentResponse{Text: tt.reply})

			res, err := code.New().Run(context.Background(), f.rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if diff := cmp.Diff(tt.want, res.Patch.Code.ChangedFiles); diff != "" {
				t.Errorf("ChangedFiles mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// An entry the write node would refuse is refused here instead, where the
// error can name the reply that produced it.
func TestRunRejectsUnusablePaths(t *testing.T) {
	tests := []struct {
		name       string
		reply      string
		wantPath   string
		wantReason string
	}{
		{
			name:       "absolute path",
			reply:      filesBlock("calc.go", "/etc/passwd"),
			wantPath:   "/etc/passwd",
			wantReason: "absolute",
		},
		{
			name:       "parent traversal",
			reply:      filesBlock("../escape.go"),
			wantPath:   "../escape.go",
			wantReason: "escapes the repository root",
		},
		{
			name:       "traversal below a directory",
			reply:      filesBlock("internal/../../escape.go"),
			wantPath:   "internal/../../escape.go",
			wantReason: "escapes the repository root",
		},
		{
			name:       "an entry that names no file",
			reply:      filesBlock("calc.go", "."),
			wantPath:   ".",
			wantReason: "does not name a file",
		},
		{
			name:       "a quoted empty entry",
			reply:      filesBlock(`""`),
			wantPath:   `""`,
			wantReason: "is empty",
		},
		{
			name:       "backslash separators",
			reply:      filesBlock(`internal\calc\calc.go`),
			wantPath:   `internal\calc\calc.go`,
			wantReason: "backslash",
		},
		{
			name:       "a bad path recovered from the diff fallback",
			reply:      "Here it is:\n\ndiff --git a/x b//etc/passwd\n--- a/x\n+++ /etc/passwd\n",
			wantPath:   "/etc/passwd",
			wantReason: "absolute",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, belay.AgentResponse{Text: tt.reply})

			res, err := code.New().Run(context.Background(), f.rc)
			if err == nil {
				t.Fatalf("Run succeeded on an unusable path. Result = %+v", res)
			}
			if !errors.Is(err, code.ErrFileList) {
				t.Errorf("errors.Is(err, ErrFileList) = false: %v", err)
			}
			var fe *code.FileListError
			if !errors.As(err, &fe) {
				t.Fatalf("errors.As(*code.FileListError) = false; got %v", err)
			}
			if fe.Path != tt.wantPath {
				t.Errorf("FileListError.Path = %q, want the entry exactly as the reply wrote it (%q)",
					fe.Path, tt.wantPath)
			}
			if tt.wantReason != "" && !strings.Contains(fe.Reason, tt.wantReason) {
				t.Errorf("FileListError.Reason = %q, want it to mention %q", fe.Reason, tt.wantReason)
			}
			if fe.Source == "" {
				t.Error("FileListError.Source is empty; a post-mortem cannot tell block from diff fallback")
			}
			// The reply is still evidence, and it is the only record of what
			// the agent believes it did to the workspace.
			if _, statErr := os.Stat(filepath.Join(
				f.rc.Layout.ArtifactsDir(), "summary-0003.md")); statErr != nil {
				t.Errorf("the summary was not archived before the rejection: %v", statErr)
			}
		})
	}
}

// The fallback: a reply that volunteers a diff instead of the block has
// still said which files it touched, and those paths are better than none.
func TestChangedFilesFallsBackToADiff(t *testing.T) {
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
		wantFiles []string
	}{
		{
			name:      "fenced diff block",
			reply:     "Summary.\n\n" + fence + "diff\n" + sampleDiff + fence + "\n\nDone.",
			wantFiles: []string{"calc.go"},
		},
		{
			name:      "patch tag is accepted too",
			reply:     fence + "patch\n" + sampleDiff + fence,
			wantFiles: []string{"calc.go"},
		},
		{
			name:      "unfenced diff is still recovered",
			reply:     "Here is the change:\n\n" + sampleDiff,
			wantFiles: []string{"calc.go"},
		},
		{
			name:      "unterminated fence is still recovered",
			reply:     fence + "diff\n" + sampleDiff,
			wantFiles: []string{"calc.go"},
		},
		{
			name:      "a fence inside the diff body does not close the block",
			reply:     fence + "diff\n" + markdownDiff + fence + "\n",
			wantFiles: []string{"README.md"},
		},
		{
			name:      "no diff and no block at all",
			reply:     "The plan requires no code change.",
			wantFiles: []string{},
		},
		{
			name:      "empty fenced block falls through to the bare diff",
			reply:     fence + "diff\n" + fence + "\n\n" + sampleDiff,
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
// redoing work whose edits are already on disk.
func TestPromptCarriesTheContinuationContext(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "ok"}, func(f *fixture) {
		f.rc.State.Code.SessionID = "s1"
		f.rc.State.Code.LastDiff = "artifacts/diff-0004.patch"
		f.rc.State.Code.ChangedFiles = []string{"calc.go", "calc_test.go"}
	})

	if _, err := code.New().Run(context.Background(), f.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	prompt := f.agent.Calls()[0].Prompt
	for _, want := range []string{
		"Continuing your earlier work",
		"still on disk",
		"- calc.go",
		"- calc_test.go",
		"cumulative set",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("continuation prompt is missing %q:\n%s", want, prompt)
		}
	}
	// The patch-era continuation told the agent its earlier diff "is replaced
	// by this one, not added to it", and to re-emit the complete diff. Under
	// in-place editing that invites it to redo work already on disk.
	for _, banned := range []string{"replaced by this one", "complete diff"} {
		if strings.Contains(prompt, banned) {
			t.Errorf("continuation prompt still carries the patch-era instruction %q:\n%s", banned, prompt)
		}
	}
}

// The continuation turns on the session alone. Under in-place editing the
// earlier edits are on disk whether or not any artifact recorded them, so
// gating on an archived diff would let a resumed session start over.
func TestPromptContinuesOnASessionWithNoArchivedDiff(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "ok"}, func(f *fixture) {
		f.rc.State.Code.SessionID = "s1"
		f.rc.State.Code.LastDiff = ""
		f.rc.State.Code.ChangedFiles = nil
	})

	if _, err := code.New().Run(context.Background(), f.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	prompt := f.agent.Calls()[0].Prompt
	if !strings.Contains(prompt, "Continuing your earlier work") {
		t.Errorf("a resumed session with no archived diff got no continuation section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "no files were recorded") {
		t.Errorf("continuation prompt does not handle an empty file list:\n%s", prompt)
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

// The prompt must ask for what the parser actually reads, and must no
// longer forbid the editing this node depends on.
func TestPromptAsksForTheFileListAndNotAPatch(t *testing.T) {
	f := newFixture(t, belay.AgentResponse{Text: "ok"})

	if _, err := code.New().Run(context.Background(), f.rc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	req := f.agent.Calls()[0]
	for _, want := range []string{"changed-files", "one path per line", "load-bearing"} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, req.Prompt)
		}
	}
	for _, banned := range []string{"must not create, modify, or delete", "Your only output is a patch"} {
		if strings.Contains(req.SystemPrompt, banned) {
			t.Errorf("system prompt still forbids editing (%q):\n%s", banned, req.SystemPrompt)
		}
	}
	for _, want := range []string{"do not run tests", "do not commit"} {
		if !strings.Contains(req.SystemPrompt, want) {
			t.Errorf("system prompt dropped the constraint %q:\n%s", want, req.SystemPrompt)
		}
	}
}
