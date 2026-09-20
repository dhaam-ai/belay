package plandoc_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/plandoc"
)

// edgeSeeds are documents that sit on the parser's edges, beyond the shapes
// the fixtures cover: a marker alone on its line, a list starting at 0, a
// number one short of too many digits, an unclosed fence, a line after the
// list indented just short of the last step, and whitespace CommonMark does
// not count as blank.
var edgeSeeds = []string{
	"## Steps\n\n1.\n   Marker alone.\n2. Next.\n",
	"## Steps\n\n0. Zero-based.\n1. One.\n",
	"## Steps\n\n999999998. Big.\n999999999. Bigger.\n",
	"## Steps\n\n1. Opens a fence:\n\n   ```\n   never closed\n2. Still a step.\n",
	"## Steps\n\n9. Nine.\n10. Ten.\n   Just short of step ten.\n",
	"## Steps\n\n1. a\n \n2. b\n",
	"## Steps\n\n1. a\r\r\n2. b\r\n",
	"## Steps\n\n1.      Indented code after the marker.\n2.\t\t\tTabs after the marker.\n",
	"Intro.\n\n## steps ##\n\nSome prose first.\n\n  3) Indented and paren-delimited.\n  4) Next.\n\n# Next section\n",
	"## Steps\n\n1. a\n\n\n\n2. b\n\n\n\n## Tests\n",
}

// seeds returns every parseable fixture and every edge seed.
func seeds(f *testing.F) [][]byte {
	f.Helper()
	var out [][]byte
	for _, fixture := range parseable {
		src, err := readFixture(fixture)
		if err != nil {
			f.Fatalf("read fixture: %v", err)
		}
		out = append(out, src)
	}
	for _, s := range edgeSeeds {
		out = append(out, []byte(s))
	}
	return out
}

// FuzzParseRoundTrip pins losslessness for any input at all. Parse either
// refuses with one of its two documented errors, or returns a document whose
// Bytes are the input and whose Parts concatenate to it — and on which every
// no-op edit really is a no-op.
func FuzzParseRoundTrip(f *testing.F) {
	for _, src := range seeds(f) {
		f.Add(src)
	}

	f.Fuzz(func(t *testing.T, src []byte) {
		doc, err := plandoc.Parse(src)
		if err != nil {
			if !errors.Is(err, plandoc.ErrNoSteps) && !errors.Is(err, plandoc.ErrNotUTF8) {
				t.Fatalf("Parse = error %v, want ErrNoSteps or ErrNotUTF8", err)
			}
			return
		}
		if doc.Len() == 0 || len(doc.Steps()) != doc.Len() {
			t.Fatalf("Parse succeeded with Len()=%d and %d Steps()", doc.Len(), len(doc.Steps()))
		}
		if got := doc.Bytes(); !bytes.Equal(got, src) {
			t.Fatalf("Bytes() != source\n got: %q\nwant: %q", got, src)
		}
		head, steps, tail := doc.Parts()
		if len(steps) != doc.Len() {
			t.Fatalf("Parts() returned %d step(s), Len() = %d", len(steps), doc.Len())
		}
		if joined := bytes.Join(append(append([][]byte{head}, steps...), tail), nil); !bytes.Equal(joined, src) {
			t.Fatalf("head+steps+tail != source\n got: %q\nwant: %q", joined, src)
		}

		for i, s := range doc.Steps() {
			same, err := doc.Edit(i, s.Text)
			if err != nil || !bytes.Equal(same.Bytes(), src) {
				t.Fatalf("Edit(%d, its own text) = %q, %v; want the source unchanged", i, same.Bytes(), err)
			}
			moved, err := doc.Move(i, i)
			if err != nil || !bytes.Equal(moved.Bytes(), src) {
				t.Fatalf("Move(%d, %d) = %q, %v; want the source unchanged", i, i, moved.Bytes(), err)
			}
		}
	})
}

// texts are step texts chosen to break a careless writer: a fenced block
// holding a heading and a marker, CR LF, a tab, blank lines around the
// text, and multi-byte runes. The fuzzer's own string is used as well.
var texts = []string{
	"Rewritten.",
	"First line.\n\n- a sub-bullet\n- another",
	"Quote a plan:\n\n```markdown\n## Steps\n1. not a step\n```",
	"CR LF text.\r\nSecond line.\r\n",
	"\n\n  Leading blank lines and spaces.\n\n",
	"\tTabbed.\n\tStill tabbed.",
	"幅 and 👨‍👩‍👧‍👦 and résumé.",
	"2. Looks like a marker.\n10) So does this.",
}

// FuzzEditSequencesStayParseable drives random sequences of Edit, Insert,
// Delete and Move over the fixtures, and checks after every edit that went
// through: the head and the tail are untouched, the document reads back as
// the steps it reports, an Edit changed no other step's bytes, and a
// structural edit left the list numbered from its own start.
//
// A refusal must be one this package documents, and ErrAmbiguousEdit only
// for a document with a known cause (see knownAmbiguity). If the fuzzer
// finds a third cause, that is a writer bug to fix, not a refusal to allow.
func FuzzEditSequencesStayParseable(f *testing.F) {
	for i, src := range seeds(f) {
		f.Add(src, []byte{0, byte(i), 1, 1, 0, 2, 2, 1, 0, 3, 2, 0, 1, 3, byte(i)}, "Fuzzed step text.")
	}

	f.Fuzz(func(t *testing.T, src, ops []byte, fuzzed string) {
		doc, err := plandoc.Parse(src)
		if err != nil {
			return
		}
		start := doc.Steps()[0].Number

		for ; len(ops) >= 3; ops = ops[3:] {
			n := doc.Len()
			op, a, b := ops[0]%4, int(ops[1]), int(ops[2])
			text := fuzzed
			if pick := b % (len(texts) + 1); pick < len(texts) {
				text = texts[pick]
			}

			var next plandoc.Document
			var name string
			switch op {
			case 0:
				name = "Edit"
				next, err = doc.Edit(a%n, text)
			case 1:
				name = "Insert"
				next, err = doc.Insert(a%(n+1), text)
			case 2:
				name = "Delete"
				next, err = doc.Delete(a % n)
			default:
				name = "Move"
				next, err = doc.Move(a%n, b%n)
			}

			if err != nil {
				checkRefusal(t, name, doc, start, err)
				continue
			}
			checkEdited(t, name, doc, next)
			if op == 0 {
				checkOnlyOneStepChanged(t, doc, next, a%n)
			} else if op != 3 || a%n != b%n {
				checkNumbering(t, name, next, start)
			}
			doc = next
		}
	})
}

// checkRefusal fails the test unless err is a refusal the operation
// documents for this document.
func checkRefusal(t *testing.T, name string, doc plandoc.Document, start int, err error) {
	t.Helper()
	switch {
	case errors.Is(err, plandoc.ErrEmptyStep), errors.Is(err, plandoc.ErrNotUTF8):
		if name == "Delete" || name == "Move" {
			t.Fatalf("%s = error %v, which only text can cause", name, err)
		}
	case errors.Is(err, plandoc.ErrLastStep):
		if name != "Delete" || doc.Len() != 1 {
			t.Fatalf("%s on %d step(s) = error %v", name, doc.Len(), err)
		}
	case errors.Is(err, plandoc.ErrAmbiguousEdit):
		if name == "Edit" || !knownAmbiguity(doc, start) {
			t.Fatalf("%s = error %v with no known cause\ndocument: %q", name, err, doc.Bytes())
		}
	default:
		t.Fatalf("%s = error %v, which it never documents", name, err)
	}
}

// checkEdited fails the test unless next kept before's head and tail and
// reads back as exactly the steps it reports.
func checkEdited(t *testing.T, name string, before, next plandoc.Document) {
	t.Helper()
	head, _, tail := before.Parts()
	gotHead, _, gotTail := next.Parts()
	if !bytes.Equal(gotHead, head) || !bytes.Equal(gotTail, tail) {
		t.Fatalf("%s changed the head or the tail\nbefore: %q\n after: %q", name, before.Bytes(), next.Bytes())
	}
	again, err := plandoc.Parse(next.Bytes())
	if err != nil {
		t.Fatalf("%s left a document Parse refuses (%v): %q", name, err, next.Bytes())
	}
	if diff := cmp.Diff(next.Steps(), again.Steps()); diff != "" {
		t.Fatalf("%s left a document that reads back differently (-reported +reparsed):\n%s", name, diff)
	}
}

// checkOnlyOneStepChanged fails the test if an Edit of step i changed any
// other step's bytes, or the number of steps.
func checkOnlyOneStepChanged(t *testing.T, before, next plandoc.Document, i int) {
	t.Helper()
	_, was, _ := before.Parts()
	_, is, _ := next.Parts()
	if len(was) != len(is) {
		t.Fatalf("Edit(%d) changed the number of steps from %d to %d", i, len(was), len(is))
	}
	for j := range was {
		if j != i && !bytes.Equal(was[j], is[j]) {
			t.Fatalf("Edit(%d) changed step %d\nbefore: %q\n after: %q", i, j, was[j], is[j])
		}
	}
}

// checkNumbering fails the test unless the list is numbered start, start+1,
// and so on, which every structural edit leaves it.
func checkNumbering(t *testing.T, name string, doc plandoc.Document, start int) {
	t.Helper()
	for k, s := range doc.Steps() {
		if s.Number != start+k {
			t.Fatalf("after %s, step %d is numbered %d, want %d", name, k, s.Number, start+k)
		}
	}
}

// knownAmbiguity reports whether doc has a shape a structural edit is
// documented to be refused for: a line right after the list indented at
// least three columns, which is where the narrowest possible step content
// starts, or a list whose numbers could outgrow nine digits.
func knownAmbiguity(doc plandoc.Document, start int) bool {
	const maxNumber = 999_999_999
	_, _, tail := doc.Parts()
	return firstIndent(tail) >= 3 || start+doc.Len() > maxNumber
}

// firstIndent returns the indentation, in columns, of the first line of b
// that is not blank, or -1 when there is none. Tabs stop at multiples of 4,
// as in CommonMark.
func firstIndent(b []byte) int {
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Trim(line, " \t\r") == "" {
			continue
		}
		col := 0
		for _, r := range line {
			switch r {
			case ' ':
				col++
			case '\t':
				col += 4 - col%4
			default:
				return col
			}
		}
		return col
	}
	return -1
}
