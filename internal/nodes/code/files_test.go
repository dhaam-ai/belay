package code_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/nodes/code"
)

// TestParseChangedFilesBlockIgnoresEveryDiff pins the one difference between
// the two parsers: the block-only one never salvages a path out of a diff.
//
// It is the parser the fix node calls, because a fix prompt quotes the failure
// it wants repaired and a failure is routinely printed as a unified diff. Each
// row here is a reply that would report something through ParseChangedFiles
// and must report nothing through this one.
func TestParseChangedFilesBlockIgnoresEveryDiff(t *testing.T) {
	// How testify prints a comparison, and how Go's own test output names a
	// temporary file: paths that are not repository-relative at all.
	const testifyDiff = "Error:          Not equal:\n" +
		"                Diff:\n" +
		"--- Expected\n+++ Actual\n@@ -1 +1 @@\n-5\n+4\n"
	const goldenDiff = "diff -u testdata/out.golden /tmp/TestRender123/out\n" +
		"--- /Users/me/repo/testdata/out.golden\n" +
		"+++ /tmp/TestRender123/out\n@@ -1 +1 @@\n-old\n+new\n"

	tests := []struct {
		name  string
		reply string
		want  []string
	}{
		{
			name:  "a block is read exactly as the shared rules read it",
			reply: "Summary.\n\n" + filesBlock("./calc.go", "calc.go", "internal/calc/calc_test.go"),
			want:  []string{"calc.go", "internal/calc/calc_test.go"},
		},
		{
			name:  "a bare diff reports nothing",
			reply: "Here is the change:\n\n" + sampleDiff,
		},
		{
			name:  "a fenced diff reports nothing",
			reply: "Summary.\n\n" + fence + "diff\n" + sampleDiff + fence + "\n",
		},
		{
			name:  "an empty block does not fall through to a diff",
			reply: fence + "changed-files\n" + fence + "\n\n" + sampleDiff,
		},
		{
			name:  "a quoted test failure reports nothing",
			reply: testifyDiff,
		},
		{
			name:  "a quoted golden-file diff reports nothing, and is not an error",
			reply: goldenDiff,
		},
		{
			name:  "the block wins, and the diff beside it is not added to it",
			reply: testifyDiff + "\n" + filesBlock("limiter.go"),
			want:  []string{"limiter.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := code.ParseChangedFilesBlock(tt.reply)
			if err != nil {
				t.Fatalf("ParseChangedFilesBlock: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseChangedFilesBlock (-want +got):\n%s", diff)
			}
		})
	}
}

// TestParseChangedFilesBlockStillRefusesTheBlocksOwnBadPaths: dropping the
// diff salvage does not soften the block. A path the prompt asked for and the
// agent got wrong is a false claim about the repository, and it is refused
// whole, as the code node refuses one.
func TestParseChangedFilesBlockStillRefusesTheBlocksOwnBadPaths(t *testing.T) {
	for _, path := range []string{"/etc/passwd", "../outside.go", `a\b.go`} {
		t.Run(path, func(t *testing.T) {
			got, err := code.ParseChangedFilesBlock(filesBlock("calc.go", path))
			if !errors.Is(err, code.ErrFileList) {
				t.Fatalf("err = %v, want one wrapping code.ErrFileList", err)
			}
			if got != nil {
				t.Errorf("a refused report returned %q; the set is refused whole", got)
			}
		})
	}
}
