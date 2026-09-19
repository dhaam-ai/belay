package plandoc

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Edit replaces the text of step i, where i indexes Steps.
//
// Only that step's bytes change. Its number is kept exactly as written:
// renumbering on an edit would rewrite every step of a lazily numbered list
// to change one word in one of them, and the approve gate would then show a
// reviewer a plan that differs everywhere. The delimiter and the space after
// it are kept too.
//
// text is written in the document's own line ending, with every line after
// the first indented to the step's content column, so a sub-list or a
// fenced block typed into a step stays inside it. Blank lines before and
// after the text are dropped, and so is whitespace before its first line:
// the first would read as the gap to the next step, and the second would
// move the step's content column and push its other lines out of it.
//
// Text identical to the step's current Text returns the document unchanged,
// byte for byte — even for a step indented with tabs, which this package
// never writes itself. That is what lets a viewer commit whatever its editor
// hands back: saving a step nobody changed must not look like an edit to the
// approve gate.
//
// Otherwise it returns ErrNoSuchStep for an index outside Steps, ErrEmptyStep
// for text that is empty or only whitespace, and ErrNotUTF8 for text that is
// not valid UTF-8. The receiver is never modified.
func (d Document) Edit(i int, text string) (Document, error) {
	if err := d.checkIndex(i); err != nil {
		return Document{}, err
	}
	s := d.steps[i]
	if text == s.text {
		return d, nil
	}
	lines, err := tidy(text)
	if err != nil {
		return Document{}, err
	}
	s.text = joinLines(lines)
	s.raw = s.render(lines, d.eol, bytes.HasSuffix(s.raw, []byte("\n")))
	return d.replacing(i, s), nil
}

// checkIndex reports ErrNoSuchStep for an index outside Steps.
func (d Document) checkIndex(i int) error {
	if i < 0 || i >= len(d.steps) {
		return fmt.Errorf("%w: index %d, and the plan has %d step(s)", ErrNoSuchStep, i, len(d.steps))
	}
	return nil
}

// replacing returns a copy of d with step i replaced. The steps slice is
// copied, never written through, because the receiver may be an undo entry
// a caller is still holding.
func (d Document) replacing(i int, s step) Document {
	steps := make([]step, len(d.steps))
	copy(steps, d.steps)
	steps[i] = s
	d.steps = steps
	return d
}

// tidy validates step text a caller supplied and splits it into the lines
// render writes. See Edit for what it drops, and why.
//
// Whitespace-only lines become empty lines, which is how the reader sees
// them too, and CRs at the end of a line are removed: the writer adds the
// document's own line ending, and a CR left in front of it would be read
// back as part of that ending rather than as text.
func tidy(text string) ([]string, error) {
	if !utf8.ValidString(text) {
		return nil, ErrNotUTF8
	}
	if strings.TrimSpace(text) == "" {
		return nil, ErrEmptyStep
	}
	lines := strings.Split(text, "\n")
	for k, line := range lines {
		line = strings.TrimRight(line, "\r")
		if isBlank(line) {
			line = ""
		}
		lines[k] = line
	}
	for lines[0] == "" {
		lines = lines[1:]
	}
	for lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	lines[0] = strings.TrimLeft(lines[0], " \t")
	return lines, nil
}

// render writes lines back as the step's bytes: its marker, then the first
// line, then every other line indented to the content column. terminate
// says whether the last line gets a line ending, which it does unless this
// is the final line of a document that never had one.
//
// A marker with no pad (an item whose marker stood alone on its line) is
// given one space, which puts its content in the same column it had.
func (s step) render(lines []string, eol string, terminate bool) []byte {
	var b strings.Builder
	b.WriteString(s.indent)
	b.WriteString(s.digits)
	b.WriteByte(s.delim)
	if lines[0] != "" {
		pad := s.pad
		if pad == "" {
			pad = " "
		}
		b.WriteString(pad)
		b.WriteString(lines[0])
	}
	continuation := strings.Repeat(" ", s.col)
	for _, line := range lines[1:] {
		b.WriteString(eol)
		if line != "" {
			b.WriteString(continuation)
			b.WriteString(line)
		}
	}
	if terminate {
		b.WriteString(eol)
	}
	return []byte(b.String())
}
