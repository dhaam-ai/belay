package plandoc_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/plandoc"
)

// parseString parses an inline document, for the edge cases too small or
// too strange to deserve a fixture of their own.
func parseString(t *testing.T, src string) plandoc.Document {
	t.Helper()
	doc, err := plandoc.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse(%q) = error %v, want a document", src, err)
	}
	return doc
}

// TestStructuralEditsRenumber pins Insert, Delete and Move. Each renumbers
// the list from its own start number, re-indents continuation lines to the
// new content column (a step going from "9." to "10." moves its sub-lines
// from column 3 to column 4, or a sub-list would escape it), and puts every
// step one blank line apart in a loose list and zero apart in a tight one.
// The head and the tail are never touched.
func TestStructuralEditsRenumber(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		edit    func(plandoc.Document) (plandoc.Document, error)
		want    []plandoc.Step
		// wantParts is the bytes of the steps named by index, gap included,
		// as Parts returns them. Steps it does not name are checked through
		// want alone.
		wantParts map[int]string
	}{
		{
			name:    "an insert at the top renumbers every step",
			fixture: "canonical.md",
			edit: func(d plandoc.Document) (plandoc.Document, error) {
				return d.Insert(0, "Read `internal/cli/run.go` first.")
			},
			want: []plandoc.Step{
				{Number: 1, Text: "Read `internal/cli/run.go` first."},
				{Number: 2, Text: "Add a `dryRun` field to `runOptions` in `internal/cli/run.go`."},
				{Number: 3, Text: "Bind `--dry-run` to it in `newRunCmd`, with help text a first-time reader can act on."},
				{Number: 4, Text: "Return from `startRun` before `createRun` when the flag is set, after printing the resolved graph."},
			},
			wantParts: map[int]string{
				0: "1. Read `internal/cli/run.go` first.\n",
				1: "2. Add a `dryRun` field to `runOptions` in `internal/cli/run.go`.\n",
				3: "4. Return from `startRun` before `createRun` when the flag is set, after printing the resolved graph.\n",
			},
		},
		{
			name:    "an insert past the last step appends",
			fixture: "canonical.md",
			edit: func(d plandoc.Document) (plandoc.Document, error) {
				return d.Insert(3, "Mention the flag in `docs/cli.md`.")
			},
			want: []plandoc.Step{
				{Number: 1, Text: "Add a `dryRun` field to `runOptions` in `internal/cli/run.go`."},
				{Number: 2, Text: "Bind `--dry-run` to it in `newRunCmd`, with help text a first-time reader can act on."},
				{Number: 3, Text: "Return from `startRun` before `createRun` when the flag is set, after printing the resolved graph."},
				{Number: 4, Text: "Mention the flag in `docs/cli.md`."},
			},
			wantParts: map[int]string{3: "4. Mention the flag in `docs/cli.md`.\n"},
		},
		{
			name:    "a loose list stays loose",
			fixture: "loose.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Insert(1, "Check the schema version.") },
			want: []plandoc.Step{
				{Number: 1, Text: "Read the manifest."},
				{Number: 2, Text: "Check the schema version."},
				{Number: 3, Text: "Rebuild the registry from its pins."},
				{Number: 4, Text: "Refuse a newer schema."},
			},
			wantParts: map[int]string{
				0: "1. Read the manifest.\n\n",
				1: "2. Check the schema version.\n\n",
				2: "3. Rebuild the registry from its pins.\n\n",
				3: "4. Refuse a newer schema.\n",
			},
		},
		{
			name:    "deleting the first step renumbers the rest",
			fixture: "canonical.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Delete(0) },
			want: []plandoc.Step{
				{Number: 1, Text: "Bind `--dry-run` to it in `newRunCmd`, with help text a first-time reader can act on."},
				{Number: 2, Text: "Return from `startRun` before `createRun` when the flag is set, after printing the resolved graph."},
			},
			wantParts: map[int]string{
				0: "1. Bind `--dry-run` to it in `newRunCmd`, with help text a first-time reader can act on.\n",
				1: "2. Return from `startRun` before `createRun` when the flag is set, after printing the resolved graph.\n",
			},
		},
		{
			name:    "deleting the last step of a loose list leaves no blank line behind",
			fixture: "loose.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Delete(2) },
			want: []plandoc.Step{
				{Number: 1, Text: "Read the manifest."},
				{Number: 2, Text: "Rebuild the registry from its pins."},
			},
			wantParts: map[int]string{
				0: "1. Read the manifest.\n\n",
				1: "2. Rebuild the registry from its pins.\n",
			},
		},
		{
			name:    "a step moved down",
			fixture: "canonical.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Move(0, 2) },
			want: []plandoc.Step{
				{Number: 1, Text: "Bind `--dry-run` to it in `newRunCmd`, with help text a first-time reader can act on."},
				{Number: 2, Text: "Return from `startRun` before `createRun` when the flag is set, after printing the resolved graph."},
				{Number: 3, Text: "Add a `dryRun` field to `runOptions` in `internal/cli/run.go`."},
			},
			wantParts: map[int]string{2: "3. Add a `dryRun` field to `runOptions` in `internal/cli/run.go`.\n"},
		},
		{
			name:    "a step moved up",
			fixture: "canonical.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Move(2, 0) },
			want: []plandoc.Step{
				{Number: 1, Text: "Return from `startRun` before `createRun` when the flag is set, after printing the resolved graph."},
				{Number: 2, Text: "Add a `dryRun` field to `runOptions` in `internal/cli/run.go`."},
				{Number: 3, Text: "Bind `--dry-run` to it in `newRunCmd`, with help text a first-time reader can act on."},
			},
			wantParts: map[int]string{0: "1. Return from `startRun` before `createRun` when the flag is set, after printing the resolved graph.\n"},
		},
		{
			name:    "lazy numbering becomes sequential once the list changes shape",
			fixture: "lazy.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Insert(3, "Log the refusal.") },
			want: []plandoc.Step{
				{Number: 1, Text: "Read the manifest."},
				{Number: 2, Text: "Rebuild the registry from its pins."},
				{Number: 3, Text: "Refuse a newer schema."},
				{Number: 4, Text: "Log the refusal."},
			},
			wantParts: map[int]string{1: "2. Rebuild the registry from its pins.\n", 3: "4. Log the refusal.\n"},
		},
		{
			name:    "the delimiter is kept",
			fixture: "paren.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Delete(1) },
			want: []plandoc.Step{
				{Number: 1, Text: "Read the manifest."},
				{Number: 2, Text: "Refuse a run whose `requires` list this binary cannot honour."},
			},
			wantParts: map[int]string{1: "2) Refuse a run whose `requires` list this binary cannot honour.\n"},
		},
		{
			name:    "nine becomes ten and its continuation moves to column 4",
			fixture: "ten.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Insert(0, "Step zero.") },
			want: []plandoc.Step{
				{Number: 1, Text: "Step zero."},
				{Number: 2, Text: "Step one."},
				{Number: 3, Text: "Step two."},
				{Number: 4, Text: "Step three."},
				{Number: 5, Text: "Step four."},
				{Number: 6, Text: "Step five."},
				{Number: 7, Text: "Step six."},
				{Number: 8, Text: "Step seven."},
				{Number: 9, Text: "Step eight."},
				{Number: 10, Text: "Step nine, whose continuation line is indented to column 3:\n\nstill step nine."},
				{Number: 11, Text: "Step ten, whose continuation line is indented to column 4:\n\nstill step ten."},
				{Number: 12, Text: "Step eleven."},
			},
			wantParts: map[int]string{
				9:  "10. Step nine, whose continuation line is indented to column 3:\n\n    still step nine.\n\n",
				10: "11. Step ten, whose continuation line is indented to column 4:\n\n    still step ten.\n\n",
				11: "12. Step eleven.\n",
			},
		},
		{
			name:    "ten becomes nine and its continuation moves back to column 3",
			fixture: "ten.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Delete(0) },
			want: []plandoc.Step{
				{Number: 1, Text: "Step two."},
				{Number: 2, Text: "Step three."},
				{Number: 3, Text: "Step four."},
				{Number: 4, Text: "Step five."},
				{Number: 5, Text: "Step six."},
				{Number: 6, Text: "Step seven."},
				{Number: 7, Text: "Step eight."},
				{Number: 8, Text: "Step nine, whose continuation line is indented to column 3:\n\nstill step nine."},
				{Number: 9, Text: "Step ten, whose continuation line is indented to column 4:\n\nstill step ten."},
				{Number: 10, Text: "Step eleven."},
			},
			wantParts: map[int]string{
				7: "8. Step nine, whose continuation line is indented to column 3:\n\n   still step nine.\n\n",
				8: "9. Step ten, whose continuation line is indented to column 4:\n\n   still step ten.\n\n",
				9: "10. Step eleven.\n",
			},
		},
		{
			name:    "CR LF steps are written with CR LF",
			fixture: "crlf.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Insert(1, "Check the flag.") },
			want: []plandoc.Step{
				{Number: 1, Text: "Add a `dryRun` field to `runOptions` in `internal/cli/run.go`."},
				{Number: 2, Text: "Check the flag."},
				{Number: 3, Text: "Bind `--dry-run` to it in `newRunCmd`, with help text a first-time reader can act on."},
				{Number: 4, Text: "Return from `startRun` before `createRun` when the flag is set, after printing the resolved graph."},
			},
			wantParts: map[int]string{
				1: "2. Check the flag.\r\n",
				2: "3. Bind `--dry-run` to it in `newRunCmd`, with help text a first-time reader can act on.\r\n",
			},
		},
		{
			name:    "a plan with no final newline still has none after an append",
			fixture: "tailless.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Insert(2, "Refuse a newer schema.") },
			want: []plandoc.Step{
				{Number: 1, Text: "Read the manifest."},
				{Number: 2, Text: "Rebuild the registry from its pins."},
				{Number: 3, Text: "Refuse a newer schema."},
			},
			wantParts: map[int]string{
				1: "2. Rebuild the registry from its pins.\n",
				2: "3. Refuse a newer schema.",
			},
		},
		{
			name:    "the unterminated last step gains a newline when it stops being last",
			fixture: "tailless.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Move(1, 0) },
			want: []plandoc.Step{
				{Number: 1, Text: "Rebuild the registry from its pins."},
				{Number: 2, Text: "Read the manifest."},
			},
			wantParts: map[int]string{
				0: "1. Rebuild the registry from its pins.\n",
				1: "2. Read the manifest.",
			},
		},
		{
			name:    "a fenced block and a sub-list move with their steps",
			fixture: "nested.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Move(1, 0) },
			want: []plandoc.Step{
				{Number: 1, Text: "Call it from `internal/cli/timeline.go`:\n\n```go\nline := text.Fold(note, width)\n```\n\nKeep the existing colour handling exactly as it is."},
				{Number: 2, Text: "Add `Fold` to `internal/text/fold.go`:\n\n- it breaks on spaces only\n- it never splits a grapheme cluster"},
				{Number: 3, Text: "Document the wrapping rule in `docs/cli.md`."},
			},
			wantParts: map[int]string{
				0: "1. Call it from `internal/cli/timeline.go`:\n\n   ```go\n   line := text.Fold(note, width)\n   ```\n\n   Keep the existing colour handling exactly as it is.\n\n",
				1: "2. Add `Fold` to `internal/text/fold.go`:\n\n   - it breaks on spaces only\n   - it never splits a grapheme cluster\n\n",
				2: "3. Document the wrapping rule in `docs/cli.md`.\n",
			},
		},
		{
			name:    "one step becomes two",
			fixture: "one.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Insert(1, "Cover it in src/app_test.go.") },
			want: []plandoc.Step{
				{Number: 1, Text: "Add an `Add(a, b int) int` function to src/app.go."},
				{Number: 2, Text: "Cover it in src/app_test.go."},
			},
			wantParts: map[int]string{1: "2. Cover it in src/app_test.go.\n"},
		},
		{
			name:    "a tab after the marker is kept",
			fixture: "tabs.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Insert(0, "Check the schema.") },
			want: []plandoc.Step{
				{Number: 1, Text: "Check the schema."},
				{Number: 2, Text: "Read the manifest.\nIt holds the run's pinned wiring."},
				{Number: 3, Text: "Rebuild the registry from its pins."},
			},
			wantParts: map[int]string{
				0: "1.\tCheck the schema.\n",
				1: "2.\tRead the manifest.\n    It holds the run's pinned wiring.\n",
			},
		},
		{
			name:    "multi-byte steps move intact",
			fixture: "unicode.md",
			edit:    func(d plandoc.Document) (plandoc.Document, error) { return d.Move(2, 0) },
			want: []plandoc.Step{
				{Number: 1, Text: "Cover 한국어, ｆｕｌｌｗｉｄｔｈ and 👨‍👩‍👧‍👦 in the table."},
				{Number: 2, Text: "Add `Width` to `internal/text/width.go` — it measures 幅, not bytes."},
				{Number: 3, Text: "Use it for the résumé line and the 🎉 banner, naïvely truncating nothing."},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, src := parse(t, tt.fixture)

			got, err := tt.edit(doc)
			if err != nil {
				t.Fatalf("edit = error %v", err)
			}

			if diff := cmp.Diff(tt.want, got.Steps()); diff != "" {
				t.Errorf("Steps() after the edit mismatch (-want +got):\n%s", diff)
			}
			head, _, tail := doc.Parts()
			gotHead, gotSteps, gotTail := got.Parts()
			if string(gotHead) != string(head) {
				t.Errorf("head changed\n got: %q\nwant: %q", gotHead, head)
			}
			if string(gotTail) != string(tail) {
				t.Errorf("tail changed\n got: %q\nwant: %q", gotTail, tail)
			}
			for i, want := range tt.wantParts {
				if i >= len(gotSteps) {
					t.Errorf("step %d missing; the edited document has %d", i, len(gotSteps))
					continue
				}
				if string(gotSteps[i]) != want {
					t.Errorf("step %d bytes\n got: %q\nwant: %q", i, gotSteps[i], want)
				}
			}
			if string(doc.Bytes()) != string(src) {
				t.Errorf("the edit changed the document it was called on")
			}
			reparses(t, got)
		})
	}
}

// TestMoveInPlaceChangesNothing pins the structural no-op: moving a step to
// where it already is must not renumber a lazy list or respace a loose one.
func TestMoveInPlaceChangesNothing(t *testing.T) {
	for _, fixture := range parseable {
		t.Run(fixture, func(t *testing.T) {
			doc, src := parse(t, fixture)
			for i := range doc.Len() {
				got, err := doc.Move(i, i)
				if err != nil {
					t.Fatalf("Move(%d, %d) = error %v", i, i, err)
				}
				if string(got.Bytes()) != string(src) {
					t.Errorf("Move(%d, %d) changed the document\n got: %q\nwant: %q", i, i, got.Bytes(), src)
				}
			}
		})
	}
}

// TestStructuralEditsRefuse pins the refusals, and that a refused edit
// leaves the document alone.
func TestStructuralEditsRefuse(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		edit    func(plandoc.Document) (plandoc.Document, error)
		want    error
	}{
		{name: "insert before the first position", fixture: "canonical.md", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Insert(-1, "x") }, want: plandoc.ErrNoSuchStep},
		{name: "insert past the end", fixture: "canonical.md", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Insert(4, "x") }, want: plandoc.ErrNoSuchStep},
		{name: "insert an empty step", fixture: "canonical.md", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Insert(0, " \n ") }, want: plandoc.ErrEmptyStep},
		{name: "insert text that is not UTF-8", fixture: "canonical.md", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Insert(0, "\xff") }, want: plandoc.ErrNotUTF8},
		{name: "delete before the first step", fixture: "canonical.md", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Delete(-1) }, want: plandoc.ErrNoSuchStep},
		{name: "delete past the last step", fixture: "canonical.md", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Delete(3) }, want: plandoc.ErrNoSuchStep},
		{name: "delete the only step", fixture: "one.md", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Delete(0) }, want: plandoc.ErrLastStep},
		{name: "move from before the first step", fixture: "canonical.md", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Move(-1, 0) }, want: plandoc.ErrNoSuchStep},
		{name: "move from past the last step", fixture: "canonical.md", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Move(3, 0) }, want: plandoc.ErrNoSuchStep},
		{name: "move to past the last step", fixture: "canonical.md", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Move(0, 3) }, want: plandoc.ErrNoSuchStep},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, src := parse(t, tt.fixture)
			_, err := tt.edit(doc)
			if !errors.Is(err, tt.want) {
				t.Fatalf("edit = error %v, want %v", err, tt.want)
			}
			if string(doc.Bytes()) != string(src) {
				t.Errorf("a refused edit changed the document it was called on")
			}
		})
	}
}

// TestStructuralEditsKeepEveryCharacter pins that normalizing the spacing
// between steps only ever removes spacing. A line between two steps that
// holds a no-break space is not blank to CommonMark, which counts only
// spaces and tabs, and dropping it would delete something a person typed.
// A stray CR before a line ending is the opposite case: it is part of no
// line's text, so a step rewritten without it must still read back as the
// same step.
func TestStructuralEditsKeepEveryCharacter(t *testing.T) {
	tests := []struct {
		name string
		src  string
		keep string
	}{
		{
			name: "a line holding only a no-break space",
			src:  "## Steps\n\n1. Read the manifest.\n \n2. Rebuild the registry.\n",
			keep: " ",
		},
		{
			name: "a stray CR before a line ending",
			src:  "## Steps\n\n1. Read the manifest.\r\r\n2. Rebuild the registry.\n",
			keep: "Rebuild the registry.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := parseString(t, tt.src)
			got, err := doc.Insert(0, "Check the schema.")
			if err != nil {
				t.Fatalf("Insert = error %v", err)
			}
			if !strings.Contains(string(got.Bytes()), tt.keep) {
				t.Errorf("Insert dropped %q\ndocument: %q", tt.keep, got.Bytes())
			}
			reparses(t, got)
		})
	}
}

// TestStructuralEditsNeverMoveTextBetweenSteps pins the read-back check.
//
// The line after this list is indented three columns: outside step "10.",
// whose content starts at column 4, but inside a step "9.", whose content
// starts at column 3. Deleting a step would renumber "10." to "9." and pull
// that line into it — a change to what the plan says that nobody asked for.
// Every edit is read back before it is returned, and one that would not
// read back as the steps it wrote is refused.
func TestStructuralEditsNeverMoveTextBetweenSteps(t *testing.T) {
	var b strings.Builder
	b.WriteString("## Steps\n\n")
	for n := 1; n <= 10; n++ {
		fmt.Fprintf(&b, "%d. Step %d.\n", n, n)
	}
	b.WriteString("   A line that belongs to no step.\n")
	src := b.String()

	doc := parseString(t, src)
	if doc.Len() != 10 {
		t.Fatalf("fixture has %d steps, want 10:\n%s", doc.Len(), src)
	}

	_, err := doc.Delete(0)
	if !errors.Is(err, plandoc.ErrAmbiguousEdit) {
		t.Fatalf("Delete(0) = error %v, want %v", err, plandoc.ErrAmbiguousEdit)
	}
	if string(doc.Bytes()) != src {
		t.Errorf("a refused edit changed the document it was called on")
	}

	// Growing the list keeps the last step's content at column 4 or beyond,
	// so the same line stays outside it and the edit goes through.
	got, err := doc.Insert(0, "Step zero.")
	if err != nil {
		t.Fatalf("Insert(0) = error %v", err)
	}
	reparses(t, got)
}

// TestZeroDocumentHasNothingToEdit pins the zero value. It has no step list,
// so there is no delimiter or indentation to write a new step with, and
// Insert says so rather than inventing a list the document never had.
func TestZeroDocumentHasNothingToEdit(t *testing.T) {
	var doc plandoc.Document
	if doc.Len() != 0 || doc.Steps() != nil || len(doc.Bytes()) != 0 {
		t.Fatalf("zero Document: Len()=%d Steps()=%v Bytes()=%q, want nothing", doc.Len(), doc.Steps(), doc.Bytes())
	}

	tests := []struct {
		name string
		edit func(plandoc.Document) (plandoc.Document, error)
		want error
	}{
		{name: "insert", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Insert(0, "x") }, want: plandoc.ErrNoSteps},
		{name: "edit", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Edit(0, "x") }, want: plandoc.ErrNoSuchStep},
		{name: "delete", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Delete(0) }, want: plandoc.ErrNoSuchStep},
		{name: "move", edit: func(d plandoc.Document) (plandoc.Document, error) { return d.Move(0, 0) }, want: plandoc.ErrNoSuchStep},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.edit(doc); !errors.Is(err, tt.want) {
				t.Fatalf("%s on the zero Document = error %v, want %v", tt.name, err, tt.want)
			}
		})
	}
}
