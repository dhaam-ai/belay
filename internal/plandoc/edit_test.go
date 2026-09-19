package plandoc_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/plandoc"
)

// parse reads a fixture and parses it, failing the test on any error.
func parse(t *testing.T, fixture string) (plandoc.Document, []byte) {
	t.Helper()
	src := read(t, fixture)
	doc, err := plandoc.Parse(src)
	if err != nil {
		t.Fatalf("Parse(%s) = error %v, want a document", fixture, err)
	}
	return doc, src
}

// reparses checks that got's bytes read back as the same steps, which is
// what a caller saving the document and opening it again will see.
func reparses(t *testing.T, got plandoc.Document) {
	t.Helper()
	again, err := plandoc.Parse(got.Bytes())
	if err != nil {
		t.Fatalf("Parse(edited document) = error %v\ndocument: %q", err, got.Bytes())
	}
	if diff := cmp.Diff(got.Steps(), again.Steps()); diff != "" {
		t.Errorf("edited document re-parses to different steps (-edited +reparsed):\n%s\ndocument: %q", diff, got.Bytes())
	}
}

// TestEditRewritesOnlyItsOwnStep pins what an edit may touch: the edited
// step's own bytes, and nothing else. The head, the tail and every other
// step come back byte for byte, the number is left as written (a one-word
// change must not renumber a lazily numbered list), and continuation lines
// are indented to the step's own content column in the document's own line
// ending.
func TestEditRewritesOnlyItsOwnStep(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		index    int
		text     string
		wantStep string // the edited step's bytes, as Parts returns them
	}{
		{
			name:     "one line in a tight list",
			fixture:  "canonical.md",
			index:    1,
			text:     "Bind `--dry-run` to it in `newRunCmd`.",
			wantStep: "2. Bind `--dry-run` to it in `newRunCmd`.\n",
		},
		{
			name:     "a lazily numbered list keeps its numbers",
			fixture:  "lazy.md",
			index:    2,
			text:     "Refuse a newer schema, and say which version wrote it.",
			wantStep: "1. Refuse a newer schema, and say which version wrote it.\n",
		},
		{
			name:     "a paren-delimited list keeps its delimiter",
			fixture:  "paren.md",
			index:    0,
			text:     "Read the manifest first.",
			wantStep: "1) Read the manifest first.\n",
		},
		{
			name:     "a loose list keeps the blank line after the step",
			fixture:  "loose.md",
			index:    0,
			text:     "Read the manifest.\n\nIt holds the run's pinned wiring.",
			wantStep: "1. Read the manifest.\n\n   It holds the run's pinned wiring.\n\n",
		},
		{
			name:     "CR LF is written back as CR LF",
			fixture:  "crlf.md",
			index:    0,
			text:     "Add a `dryRun` field.\nSet it from the flag.",
			wantStep: "1. Add a `dryRun` field.\r\n   Set it from the flag.\r\n",
		},
		{
			name:     "a two-digit step indents to column 4",
			fixture:  "ten.md",
			index:    9,
			text:     "Step ten, rewritten.\n\n- a sub-bullet that must stay inside it",
			wantStep: "10. Step ten, rewritten.\n\n    - a sub-bullet that must stay inside it\n\n",
		},
		{
			name:     "a fenced block inside the new text stays inside the step",
			fixture:  "fenced.md",
			index:    0,
			text:     "Add the quoting helper:\n\n```markdown\n## Steps\n1. not a step\n```",
			wantStep: "1. Add the quoting helper:\n\n   ```markdown\n   ## Steps\n   1. not a step\n   ```\n",
		},
		{
			name:     "trailing blank lines in the new text are dropped",
			fixture:  "canonical.md",
			index:    0,
			text:     "Add a `dryRun` field.\n\n\n",
			wantStep: "1. Add a `dryRun` field.\n",
		},
		{
			name:     "multi-byte text is written as it was typed",
			fixture:  "unicode.md",
			index:    2,
			text:     "Cover 한국어 and 👨‍👩‍👧‍👦 only.",
			wantStep: "3. Cover 한국어 and 👨‍👩‍👧‍👦 only.\n",
		},
		{
			name:     "the last line of a plan gains no newline its author never wrote",
			fixture:  "tailless.md",
			index:    1,
			text:     "Rebuild the registry.",
			wantStep: "2. Rebuild the registry.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, src := parse(t, tt.fixture)

			got, err := doc.Edit(tt.index, tt.text)
			if err != nil {
				t.Fatalf("Edit(%d) = error %v", tt.index, err)
			}

			head, steps, tail := doc.Parts()
			gotHead, gotSteps, gotTail := got.Parts()
			if len(gotSteps) != len(steps) {
				t.Fatalf("Edit changed the number of steps from %d to %d", len(steps), len(gotSteps))
			}
			if string(gotSteps[tt.index]) != tt.wantStep {
				t.Errorf("edited step bytes\n got: %q\nwant: %q", gotSteps[tt.index], tt.wantStep)
			}
			if string(gotHead) != string(head) {
				t.Errorf("head changed\n got: %q\nwant: %q", gotHead, head)
			}
			if string(gotTail) != string(tail) {
				t.Errorf("tail changed\n got: %q\nwant: %q", gotTail, tail)
			}
			for i := range steps {
				if i != tt.index && string(gotSteps[i]) != string(steps[i]) {
					t.Errorf("step %d changed\n got: %q\nwant: %q", i, gotSteps[i], steps[i])
				}
			}
			if want, gotNumber := doc.Steps()[tt.index].Number, got.Steps()[tt.index].Number; gotNumber != want {
				t.Errorf("edited step's Number = %d, want %d (an edit must not renumber)", gotNumber, want)
			}
			if string(doc.Bytes()) != string(src) {
				t.Errorf("Edit changed the document it was called on")
			}
			reparses(t, got)
		})
	}
}

// TestEditWithTheSameTextChangesNothing pins the no-op: saving a step
// nobody changed must leave the document byte for byte as it was, for every
// step of every fixture. A viewer opens the editor on a step's Text and
// commits whatever comes back; if that round trip moved a byte, the approve
// gate would record an edit the person never made.
func TestEditWithTheSameTextChangesNothing(t *testing.T) {
	for _, fixture := range parseable {
		t.Run(fixture, func(t *testing.T) {
			doc, src := parse(t, fixture)
			for i, s := range doc.Steps() {
				got, err := doc.Edit(i, s.Text)
				if err != nil {
					t.Fatalf("Edit(%d, its own text) = error %v", i, err)
				}
				if string(got.Bytes()) != string(src) {
					t.Errorf("Edit(%d, its own text) changed the document\n got: %q\nwant: %q", i, got.Bytes(), src)
				}
			}
		})
	}
}

// TestRetypingAStepRestoresItsBytes checks the writer against the reader:
// change a step, then type its original text back, and the document must be
// exactly what it was. Every fixture is written the way the plan prompt and
// a careful human write Markdown, so this is where a writer that indents,
// terminates or spaces a step differently from how the reader measured it
// shows up. The no-op test above cannot catch that, because an unchanged
// step is never rewritten at all.
func TestRetypingAStepRestoresItsBytes(t *testing.T) {
	for _, fixture := range parseable {
		t.Run(fixture, func(t *testing.T) {
			doc, src := parse(t, fixture)
			for i, s := range doc.Steps() {
				changed, err := doc.Edit(i, "A placeholder the person typed first.")
				if err != nil {
					t.Fatalf("Edit(%d, placeholder) = error %v", i, err)
				}
				restored, err := changed.Edit(i, s.Text)
				if err != nil {
					t.Fatalf("Edit(%d, original text) = error %v", i, err)
				}
				if string(restored.Bytes()) != string(src) {
					t.Errorf("step %d: retyping the original text did not restore the document\n got: %q\nwant: %q",
						i, restored.Bytes(), src)
				}
			}
		})
	}
}

// TestEditRefuses pins the refusals. Each would otherwise write something
// the author did not mean: a blank item that still takes a number, or text
// that a later decode turns into U+FFFD.
func TestEditRefuses(t *testing.T) {
	tests := []struct {
		name  string
		index int
		text  string
		want  error
	}{
		{name: "an index before the first step", index: -1, text: "x", want: plandoc.ErrNoSuchStep},
		{name: "an index past the last step", index: 3, text: "x", want: plandoc.ErrNoSuchStep},
		{name: "empty text", index: 0, text: "", want: plandoc.ErrEmptyStep},
		{name: "whitespace only", index: 0, text: " \n\t\n", want: plandoc.ErrEmptyStep},
		{name: "text that is not UTF-8", index: 0, text: "r\xe9sum\xe9", want: plandoc.ErrNotUTF8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, src := parse(t, "canonical.md")
			_, err := doc.Edit(tt.index, tt.text)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Edit(%d, %q) = error %v, want %v", tt.index, tt.text, err, tt.want)
			}
			if string(doc.Bytes()) != string(src) {
				t.Errorf("a refused Edit changed the document it was called on")
			}
		})
	}
}
