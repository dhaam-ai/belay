// Package plandoc is belay's one reader and writer of a plan document: the
// Markdown file the plan node archives as plan.md, a human reads and edits
// at the approve gate, and the code node implements.
//
// A Step here is one step of that plan — an item of its numbered list. It is
// never a graph node; the glossary's word for those is node.
//
// # Losslessness is the whole point
//
// [Parse] keeps every byte it was handed. A Document is its source split
// into three contiguous regions — the bytes before the first step, each
// step's own bytes, and the bytes after the last one — so for a document
// nobody changed, [Document.Bytes] returns exactly what Parse read, and
// [Document.Parts] concatenates to the same thing.
//
// That is not tidiness. The approve gate re-fingerprints plan.md every time
// it runs and reports "The plan was edited" when the digest moved. A parser
// that normalized line endings, blank lines or numbering on the way through
// would make that note fire for a person who opened the plan and changed
// nothing, and the audit trail would record an edit that never happened. So
// nothing is normalized on the way in, and on the way out only the steps an
// edit actually touched are rewritten.
//
// # What counts as a step
//
// A step is an item of the first ordered list under the "## Steps" heading
// that internal/nodes/plan's prompt asks the agent for. Headings and list
// markers inside fenced code blocks are skipped, both when looking for the
// heading and when looking for the list, so a plan that quotes a plan does
// not get read as one. Everything else in the document — every other
// section, the prose before the list, the fenced block a step contains — is
// carried through untouched.
//
// [Step.Number] is the number the document writes, not the position in the
// list. They are the same in a conventional plan and differ in a lazily
// numbered one ("1. 1. 1.", which CommonMark renders as 1, 2, 3); reporting
// what is written is the only answer that stays true of a document this
// package promises not to renumber behind anyone's back. The position is the
// index into [Document.Steps], which is what every method here takes.
//
// # Editing
//
// [Document.Edit] rewrites one step and nothing else: not its number, and
// not a byte of any other step. [Document.Insert], [Document.Delete] and
// [Document.Move] change the list's shape, so they renumber it from its own
// start number, move a step's continuation lines to its new content column
// when its number gains or loses a digit (a sub-list left at column 3 under
// a "10." marker would escape the step), and space the steps the way the
// list was spaced. None of them touches the head or the tail.
//
// Every editing method reads its result back with Parse before returning
// it, and refuses with [ErrAmbiguousEdit] if the document would not read
// back as exactly the steps it meant to write. The rules above are designed
// so that never happens; the check is there so that a case nobody thought
// of fails loudly instead of quietly moving text from one step into
// another.
//
// # It has no opinion about whether a plan is any good
//
// ErrNoSteps does not mean "this plan is invalid". It means "I cannot edit
// this document's steps safely", and a plan it refuses — sub-headings
// instead of a list, bullets, no Steps section at all — is still a perfectly
// good plan for a human to read and for the code node to implement. Whether
// a plan may be approved is the approve gate's judgement and only the
// gate's; a second opinion living here would eventually disagree with it,
// and the disagreement would surface as a run that cannot be approved
// through one path and can through another. Callers show a refused document
// as text and turn the step-editing keys off.
//
// # What it deliberately does not do
//
//   - It does not render Markdown, and it is not a CommonMark parser. It
//     recognizes ATX headings, ordered list items and fenced code blocks,
//     which is what finding and rewriting the steps needs, and treats
//     everything else as opaque lines.
//   - It does not follow lazy continuation. A non-blank line indented less
//     than its item's content ends the list, and becomes document text. A
//     wrapped line a person did not indent therefore stays visible in the
//     plan but outside any step, rather than being silently absorbed into
//     one and re-indented the next time that step is edited.
//   - It does not read setext headings ("Steps" underlined with dashes), and
//     it does not expand tabs to spaces.
//   - It does not touch the filesystem, and it takes no lock. Reading and
//     writing plan.md belongs to the caller, which is also the only thing
//     that knows whether the file changed underneath it.
package plandoc

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Errors reported by this package. Each is a refusal to guess, never a
// verdict on the plan itself — see the package doc.
var (
	// ErrNoSteps reports that the document has no ordered list under a
	// "## Steps" heading, so there is nothing this package can edit. The
	// document is still readable, and still approvable.
	ErrNoSteps = errors.New("plandoc: no step list to edit")

	// ErrNotUTF8 reports bytes that are not valid UTF-8. Editing would have
	// to decode them into a Go string, which replaces every undecodable byte
	// with U+FFFD, so a single edit would corrupt the rest of the document
	// silently. Refusing leaves those bytes on disk exactly as they are.
	ErrNotUTF8 = errors.New("plandoc: plan is not valid UTF-8")

	// ErrNoSuchStep reports an index outside the document's steps.
	ErrNoSuchStep = errors.New("plandoc: no such step")

	// ErrEmptyStep reports step text that is empty or only whitespace. A
	// blank item would disappear from the rendered plan while still taking
	// up a number, so it is refused rather than written.
	ErrEmptyStep = errors.New("plandoc: a step cannot be empty")

	// ErrLastStep reports an attempt to delete a plan's only step. What
	// would be left is a Steps heading with nothing under it, which Parse
	// cannot read back as a list, so no later edit could add a step again.
	// Editing the step is the way to replace it.
	ErrLastStep = errors.New("plandoc: cannot delete the only step")

	// ErrAmbiguousEdit reports an edit whose result would not read back as
	// the steps it wrote, so it was not made. The case known to cause it is
	// a line just after the list, indented to a column the last step's
	// content does not reach until renumbering makes its marker a digit
	// narrower; the edit would silently pull that line into the step.
	ErrAmbiguousEdit = errors.New("plandoc: the edit would move text between steps")
)

// Step is one step of a plan, as a caller sees it.
type Step struct {
	// Number is the number the document writes for this step, which is not
	// necessarily its position: see the package doc on lazy numbering.
	Number int
	// Text is the step's content with the marker and the continuation
	// indentation removed, so the first line reads the same as the rest.
	// Lines are joined with "\n" whatever the document's own line ending is,
	// and a fenced block or sub-list the step contains is part of it.
	Text string
}

// step is one step as the document stores it: the exact bytes it occupies,
// plus the pieces needed to write it back with a different number.
type step struct {
	number int
	digits string // the number as written, so "01." survives a plain edit
	delim  byte   // '.' or ')'
	indent string // the whitespace before the marker
	pad    string // the whitespace between the delimiter and the content
	text   string // the de-indented content, "\n"-joined

	// raw is the item's bytes, from the start of its marker line through the
	// end of its last content line. gap is the blank lines separating it
	// from the next step, and is empty for the last one: whatever follows
	// the final step belongs to the tail.
	raw []byte
	gap []byte
}

// Document is a parsed plan document. The zero Document has no steps.
//
// Values are immutable: every editing method returns a new Document and
// leaves the receiver alone, which is what lets a caller keep the previous
// value as an undo entry without copying anything.
type Document struct {
	head  []byte
	steps []step
	tail  []byte

	eol      string // "\n" or "\r\n", whichever the document uses
	finalEOL bool   // whether the source ended with a line ending
	loose    bool   // whether a blank line separates the steps
	start    int    // the number the list starts at, for renumbering
	indent   string // the list's own marker indentation
	delim    byte   // the list's own delimiter
	pad      string // the list's own gap between delimiter and content
}

// Parse reads src as a plan document.
//
// It returns ErrNotUTF8 for bytes it cannot decode and ErrNoSteps when the
// document has no ordered list under a "## Steps" heading. Neither says the
// plan is unusable — see the package doc.
//
// Parse copies src. The returned Document never aliases the caller's slice,
// so a caller reading plan.md into a reused buffer cannot change a document
// it has already parsed.
func Parse(src []byte) (Document, error) {
	if !utf8.Valid(src) {
		return Document{}, ErrNotUTF8
	}
	buf := bytes.Clone(src)
	lines := splitLines(buf)

	heading := findStepsHeading(buf, lines)
	if heading < 0 {
		return Document{}, fmt.Errorf("%w: no %q heading", ErrNoSteps, "## Steps")
	}
	first := findFirstItem(buf, lines, heading)
	if first < 0 {
		return Document{}, fmt.Errorf("%w: the Steps section holds no ordered list", ErrNoSteps)
	}

	steps, tailAt := collectSteps(buf, lines, first)
	doc := Document{
		head:     buf[:lines[first].start],
		steps:    steps,
		tail:     buf[tailAt:],
		eol:      detectEOL(buf),
		finalEOL: len(buf) > 0 && buf[len(buf)-1] == '\n',
		start:    steps[0].number,
		indent:   steps[0].indent,
		delim:    steps[0].delim,
		pad:      steps[0].pad,
	}
	for _, s := range steps {
		if len(s.gap) > 0 {
			doc.loose = true
			break
		}
	}
	if doc.pad == "" {
		doc.pad = " "
	}
	return doc, nil
}

// Len returns the number of steps.
func (d Document) Len() int { return len(d.steps) }

// Steps returns the document's steps in order. The slice is freshly built on
// every call, so a caller may sort or truncate it without disturbing the
// document.
func (d Document) Steps() []Step {
	if len(d.steps) == 0 {
		return nil
	}
	out := make([]Step, len(d.steps))
	for i, s := range d.steps {
		out[i] = Step{Number: s.number, Text: s.text}
	}
	return out
}

// Bytes returns the whole document. For a document no edit has touched,
// these are the bytes Parse was given, exactly.
func (d Document) Bytes() []byte {
	size := len(d.head) + len(d.tail)
	for _, s := range d.steps {
		size += len(s.raw) + len(s.gap)
	}
	out := make([]byte, 0, size)
	out = append(out, d.head...)
	for _, s := range d.steps {
		out = append(out, s.raw...)
		out = append(out, s.gap...)
	}
	return append(out, d.tail...)
}

// Parts returns the document in three pieces: the bytes before the first
// step, one slice per step, and the bytes after the last one. A step's bytes
// run from its marker to the start of the next step, so they include the
// blank line a loose list separates them with.
//
// Concatenating head, every step and tail reproduces Bytes exactly. That
// invariant is what a caller rendering the document can rely on to point at
// a step without re-deriving where it starts. Every slice is a copy.
func (d Document) Parts() (head []byte, steps [][]byte, tail []byte) {
	head = bytes.Clone(d.head)
	tail = bytes.Clone(d.tail)
	if len(d.steps) == 0 {
		return head, nil, tail
	}
	steps = make([][]byte, len(d.steps))
	for i, s := range d.steps {
		part := make([]byte, 0, len(s.raw)+len(s.gap))
		part = append(part, s.raw...)
		steps[i] = append(part, s.gap...)
	}
	return head, steps, tail
}

// findStepsHeading returns the index of the line holding the "## Steps"
// heading, or -1. Fenced code blocks are skipped: a plan that quotes a plan
// has a "## Steps" line of its own, and reading the quoted one would make
// every step belong to a code sample.
func findStepsHeading(buf []byte, lines []span) int {
	var f fence
	for i, sp := range lines {
		text := body(buf, sp)
		if f.track(text) {
			continue
		}
		if isStepsHeading(text) {
			return i
		}
	}
	return -1
}

// findFirstItem returns the index of the first ordered list item in the
// Steps section, or -1. Prose between the heading and the list is allowed
// and becomes part of the document's head; a heading at the same level or
// above ends the section and the search.
func findFirstItem(buf []byte, lines []span, heading int) int {
	var f fence
	for i := heading + 1; i < len(lines); i++ {
		text := body(buf, lines[i])
		if f.track(text) {
			continue
		}
		if isSectionBreak(text) {
			return -1
		}
		if m, ok := parseMarker(text); ok && indentWidth(m.indent) <= maxMarkerIndent {
			return i
		}
	}
	return -1
}

// collectSteps reads the list that starts at line first, and returns its
// steps plus the offset the document's tail starts at.
//
// The tail starts at the end of the last step's last content line, so the
// blank lines between the list and whatever follows it belong to the tail
// rather than to the final step. That is what keeps deleting the last step
// from leaving a stray blank line behind.
func collectSteps(buf []byte, lines []span, first int) (steps []step, tailAt int) {
	m, _ := parseMarker(body(buf, lines[first]))
	list := m
	i := first
	for {
		last, next := itemExtent(buf, lines, i, m.col)
		s := step{
			number: m.number,
			digits: m.digits,
			delim:  m.delim,
			indent: m.indent,
			pad:    m.pad,
			text:   itemText(buf, lines, i, last, m.col),
			raw:    buf[lines[i].start:lines[last].end],
		}

		nextMarker, ok := nextItem(buf, lines, next, list)
		if !ok {
			steps = append(steps, s)
			return steps, lines[last].end
		}
		s.gap = buf[lines[last].end:lines[next].start]
		steps = append(steps, s)
		i, m = next, nextMarker
	}
}

// nextItem reports whether line at starts another item of the same list.
//
// A different delimiter starts a different list in CommonMark, and so does a
// marker indented far enough to nest, so both end this one.
func nextItem(buf []byte, lines []span, at int, list marker) (marker, bool) {
	if at >= len(lines) {
		return marker{}, false
	}
	m, ok := parseMarker(body(buf, lines[at]))
	if !ok || m.delim != list.delim || indentWidth(m.indent) > maxMarkerIndent {
		return marker{}, false
	}
	return m, true
}

// itemExtent returns the last line belonging to the item that starts at
// line i, and the first line after it that is not blank.
//
// A line indented to the item's content column or further continues it,
// whatever it looks like — that is how a sub-list, a fenced block, and the
// "1." inside that fenced block all stay inside the step they were written
// in. Blank lines are held over until the next non-blank line decides: the
// item continues if that line is indented, and ends otherwise.
func itemExtent(buf []byte, lines []span, i, col int) (last, next int) {
	last = i
	j := i + 1
	for j < len(lines) {
		text := body(buf, lines[j])
		if isBlank(text) {
			j++
			continue
		}
		if indentWidth(text) < col {
			break
		}
		last = j
		j++
	}
	return last, j
}

// itemText returns the item's content: the marker line with its marker
// removed, then every following line de-indented by the content column,
// joined with "\n". The document's own line ending is not part of the text —
// a caller editing a CRLF plan should not have to think about CR.
//
// Nor is a stray CR in front of that ending. It is not text anyone can see,
// and the writer, which ends every line with the document's own line
// ending, could not write it back as text anyway: a step moved or
// renumbered would read back different from the step that was moved.
func itemText(buf []byte, lines []span, first, last, col int) string {
	out := make([]string, 0, last-first+1)
	out = append(out, strings.TrimRight(markerContent(body(buf, lines[first])), "\r"))
	for i := first + 1; i <= last; i++ {
		text := body(buf, lines[i])
		if isBlank(text) {
			out = append(out, "")
			continue
		}
		out = append(out, strings.TrimRight(stripIndent(text, col), "\r"))
	}
	return joinLines(out)
}
