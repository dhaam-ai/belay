package planarchive

import (
	"strings"
	"testing"
)

// TestSlug pins how a goal becomes the readable half of an archived plan's
// name. The goal is whatever a person typed, so every row here is something a
// person could type: punctuation a file name cannot hold, letters outside
// ASCII, a paragraph.
func TestSlug(t *testing.T) {
	tests := []struct {
		name string
		goal string
		want string
	}{
		{name: "an ordinary goal", goal: "add a --json flag to the CLI output", want: "add-a-json-flag-to-the-cli-output"},
		{name: "capitals and digits", goal: "Upgrade to the V2 client", want: "upgrade-to-the-v2-client"},
		{name: "surrounding space", goal: "  add tests  ", want: "add-tests"},
		{
			// Every one of these is a separator, never part of a name: a
			// slash would make a directory, and the rest are refused by
			// some file system or shell somewhere.
			name: "characters a file name cannot hold",
			goal: `a/b\c:d*e?f"g<h>i|j` + "\x00k",
			want: "a-b-c-d-e-f-g-h-i-j-k",
		},
		{name: "a path cannot climb out", goal: "../../etc/passwd", want: "etc-passwd"},
		{name: "a paragraph", goal: "fix the parser\n\nit drops the last line", want: "fix-the-parser-it-drops-the-last-line"},
		{
			// Letters outside ASCII become separators rather than being
			// transliterated, which would need a Unicode table: git and
			// macOS normalise non-ASCII names differently (NFC against NFD),
			// so the same plan could otherwise have two names.
			name: "letters outside ASCII",
			goal: "Fix naïve café.go",
			want: "fix-na-ve-caf-go",
		},
		{name: "empty", goal: "", want: "plan"},
		{name: "nothing ASCII", goal: "日本語", want: "plan"},
		{name: "only punctuation", goal: "!!!", want: "plan"},
		{
			name: "a long goal is cut at the last whole word",
			goal: "implement the new configuration loader with layered merging and strict validation of every key",
			want: "implement-the-new-configuration-loader-with",
		},
		{
			name: "exactly the longest a slug may be",
			goal: "abcdefghij abcdefghij abcdefghij abcdefghij abcd",
			want: "abcdefghij-abcdefghij-abcdefghij-abcdefghij-abcd",
		},
		{name: "one word longer than a slug may be", goal: strings.Repeat("a", 60), want: strings.Repeat("a", maxSlug)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Slug(tt.goal)
			if got != tt.want {
				t.Errorf("Slug(%q) = %q, want %q", tt.goal, got, tt.want)
			}
			if len(got) > maxSlug {
				t.Errorf("Slug(%q) is %d bytes, more than %d", tt.goal, len(got), maxSlug)
			}
		})
	}
}

// TestFilename pins the whole name: the run id first, so archived plans sort
// by when their run started and the id can be copied straight into `belay
// timeline`, then the slug.
func TestFilename(t *testing.T) {
	const runID = "20260905T142530Z-1f2e3d4c5b6a"
	if got, want := Filename(runID, "add a --json flag"), runID+"-add-a-json-flag.md"; got != want {
		t.Errorf("Filename = %q, want %q", got, want)
	}
	if got, want := Rel("docs/plans", runID, "add a --json flag"), "docs/plans/"+runID+"-add-a-json-flag.md"; got != want {
		t.Errorf("Rel = %q, want %q", got, want)
	}
}
