package plandoc_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/plandoc"
)

// read returns the bytes of a testdata fixture. The fixtures are read as
// bytes, never as strings, because two of them exist precisely for their
// bytes: crlf.md ends every line with CR LF and latin1.md is not UTF-8.
func read(t *testing.T, name string) []byte {
	t.Helper()
	// gosec's taint analysis cannot see that every name reaching here is a
	// literal in this file, so it reads the join as attacker-controlled.
	//nolint:gosec // G304: name is a fixture literal, under this package's own testdata.
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// parseable lists every fixture Parse must accept. The refusals live in
// TestParseRefusesWhatItCannotEditSafely.
var parseable = []string{
	"canonical.md", "nested.md", "paren.md", "lazy.md", "ten.md",
	"crlf.md", "loose.md", "one.md", "unicode.md", "fenced.md", "nofinal.md",
	"tailless.md", "tabs.md",
}

// TestParseFindsTheSteps pins what a step is: an item of the first ordered
// list under the "## Steps" heading prompt.go asks the agent for, with its
// number as the document writes it and its text de-indented to column 0.
//
// The awkward rows are the point. nested.md keeps a sub-list and a fenced
// code block inside their steps rather than ending a step at its first
// indented line; fenced.md hides a "## Steps" heading and a numbered list
// inside two fences, one of them before the real heading; ten.md crosses
// from one-digit to two-digit numbering, so the two content columns (3 and
// 4) both appear; and lazy.md reports the numbers 1, 1, 1 that are really
// written rather than the 1, 2, 3 a reader sees rendered.
func TestParseFindsTheSteps(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    []plandoc.Step
	}{
		{
			name:    "the shape the plan prompt asks for",
			fixture: "canonical.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Add a `dryRun` field to `runOptions` in `internal/cli/run.go`."},
				{Number: 2, Text: "Bind `--dry-run` to it in `newRunCmd`, with help text a first-time reader can act on."},
				{Number: 3, Text: "Return from `startRun` before `createRun` when the flag is set, after printing the resolved graph."},
			},
		},
		{
			name:    "a sub-list and a fenced block stay inside their step",
			fixture: "nested.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Add `Fold` to `internal/text/fold.go`:\n\n- it breaks on spaces only\n- it never splits a grapheme cluster"},
				{Number: 2, Text: "Call it from `internal/cli/timeline.go`:\n\n```go\nline := text.Fold(note, width)\n```\n\nKeep the existing colour handling exactly as it is."},
				{Number: 3, Text: "Document the wrapping rule in `docs/cli.md`."},
			},
		},
		{
			name:    "a paren-delimited list",
			fixture: "paren.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Read the manifest."},
				{Number: 2, Text: "Rebuild the registry from its pins."},
				{Number: 3, Text: "Refuse a run whose `requires` list this binary cannot honour."},
			},
		},
		{
			name:    "lazy numbering is reported as written",
			fixture: "lazy.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Read the manifest."},
				{Number: 1, Text: "Rebuild the registry from its pins."},
				{Number: 1, Text: "Refuse a newer schema."},
			},
		},
		{
			name:    "both content columns of a two-digit list",
			fixture: "ten.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Step one."},
				{Number: 2, Text: "Step two."},
				{Number: 3, Text: "Step three."},
				{Number: 4, Text: "Step four."},
				{Number: 5, Text: "Step five."},
				{Number: 6, Text: "Step six."},
				{Number: 7, Text: "Step seven."},
				{Number: 8, Text: "Step eight."},
				{Number: 9, Text: "Step nine, whose continuation line is indented to column 3:\n\nstill step nine."},
				{Number: 10, Text: "Step ten, whose continuation line is indented to column 4:\n\nstill step ten."},
				{Number: 11, Text: "Step eleven."},
			},
		},
		{
			name:    "CR LF line endings are not part of the text",
			fixture: "crlf.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Add a `dryRun` field to `runOptions` in `internal/cli/run.go`."},
				{Number: 2, Text: "Bind `--dry-run` to it in `newRunCmd`, with help text a first-time reader can act on."},
				{Number: 3, Text: "Return from `startRun` before `createRun` when the flag is set, after printing the resolved graph."},
			},
		},
		{
			name:    "a loose list separated by blank lines",
			fixture: "loose.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Read the manifest."},
				{Number: 2, Text: "Rebuild the registry from its pins."},
				{Number: 3, Text: "Refuse a newer schema."},
			},
		},
		{
			name:    "a plan with one step",
			fixture: "one.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Add an `Add(a, b int) int` function to src/app.go."},
			},
		},
		{
			name:    "a plan with no final newline",
			fixture: "nofinal.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Add an `Add(a, b int) int` function to src/app.go."},
			},
		},
		{
			name:    "a plan that ends with its last step",
			fixture: "tailless.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Read the manifest."},
				{Number: 2, Text: "Rebuild the registry from its pins."},
			},
		},
		{
			// "1." ends at column 2, so the tab after it stops at column 4,
			// not at column 4 past the marker: the content column is 4, and
			// a line indented four spaces continues the step.
			name:    "a tab after the marker is measured from where it starts",
			fixture: "tabs.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Read the manifest.\nIt holds the run's pinned wiring."},
				{Number: 2, Text: "Rebuild the registry from its pins."},
			},
		},
		{
			name:    "multi-byte text survives",
			fixture: "unicode.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Add `Width` to `internal/text/width.go` — it measures 幅, not bytes."},
				{Number: 2, Text: "Use it for the résumé line and the 🎉 banner, naïvely truncating nothing."},
				{Number: 3, Text: "Cover 한국어, ｆｕｌｌｗｉｄｔｈ and 👨‍👩‍👧‍👦 in the table."},
			},
		},
		{
			name:    "fences hide a heading and a list that are not steps",
			fixture: "fenced.md",
			want: []plandoc.Step{
				{Number: 1, Text: "Add the quoting helper to `internal/docs/quote.go`."},
				{Number: 2, Text: "Show the shape it must survive:\n\n```markdown\n## Steps\n\n1. A step inside a fence inside a step.\n```\n\nThe helper copies it verbatim."},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := plandoc.Parse(read(t, tt.fixture))
			if err != nil {
				t.Fatalf("Parse(%s) = error %v, want a document", tt.fixture, err)
			}
			if got := doc.Len(); got != len(tt.want) {
				t.Errorf("Len() = %d, want %d", got, len(tt.want))
			}
			if diff := cmp.Diff(tt.want, doc.Steps()); diff != "" {
				t.Errorf("Steps() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestParseRefusesWhatItCannotEditSafely pins the refusals. A parser that
// guessed here would rewrite prose it had misread the first time anyone
// edited a step, so every shape whose steps are not an ordered list under
// "## Steps" is refused outright and left to be read as plain text.
func TestParseRefusesWhatItCannotEditSafely(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    error
	}{
		{name: "sub-headings instead of a list", fixture: "headings.md", want: plandoc.ErrNoSteps},
		{name: "bullets instead of a numbered list", fixture: "bullets.md", want: plandoc.ErrNoSteps},
		{name: "no Steps section at all", fixture: "nosection.md", want: plandoc.ErrNoSteps},
		{name: "an empty plan", fixture: "empty.md", want: plandoc.ErrNoSteps},
		{name: "bytes that are not UTF-8", fixture: "latin1.md", want: plandoc.ErrNotUTF8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := plandoc.Parse(read(t, tt.fixture))
			if !errors.Is(err, tt.want) {
				t.Fatalf("Parse(%s) = error %v, want %v", tt.fixture, err, tt.want)
			}
		})
	}
}

// TestUnchangedDocumentIsByteIdentical is the invariant the approve gate
// depends on: a document nobody edited must come back byte for byte.
//
// The gate re-fingerprints plan.md on every pass and reports "The plan was
// edited" when the digest moved. If Parse and Bytes normalized so much as a
// line ending, merely opening a plan would make that note a lie.
func TestUnchangedDocumentIsByteIdentical(t *testing.T) {
	for _, fixture := range parseable {
		t.Run(fixture, func(t *testing.T) {
			src := read(t, fixture)
			doc, err := plandoc.Parse(src)
			if err != nil {
				t.Fatalf("Parse(%s) = error %v, want a document", fixture, err)
			}
			if got := doc.Bytes(); string(got) != string(src) {
				t.Errorf("Bytes() != source\n got: %q\nwant: %q", got, src)
			}

			head, steps, tail := doc.Parts()
			joined := append([]byte(nil), head...)
			for _, step := range steps {
				joined = append(joined, step...)
			}
			joined = append(joined, tail...)
			if string(joined) != string(src) {
				t.Errorf("head+steps+tail != source\n got: %q\nwant: %q", joined, src)
			}
			if len(steps) != doc.Len() {
				t.Errorf("Parts() returned %d step(s), Len() = %d", len(steps), doc.Len())
			}
		})
	}
}

// TestParseCopiesItsInput guards ownership: Parse must not keep the caller's
// slice. The CLI reads plan.md into a buffer it may reuse, and a Document
// that aliased it would change under the viewer's feet.
func TestParseCopiesItsInput(t *testing.T) {
	src := read(t, "canonical.md")
	doc, err := plandoc.Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	before := doc.Bytes()
	if len(before) != len(src) {
		t.Fatalf("Bytes() returned %d byte(s), want the fixture's %d", len(before), len(src))
	}

	for i := range src {
		src[i] = 'x'
	}

	if got := doc.Bytes(); string(got) != string(before) {
		t.Errorf("Bytes() changed when the caller's slice was overwritten\n got: %q\nwant: %q", got, before)
	}
}
