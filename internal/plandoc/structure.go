package plandoc

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Insert adds a step reading text so that it becomes Steps()[at]; at may
// be Len(), which appends. Every step after it is renumbered.
//
// The new step is written with the list's own delimiter, indentation and
// padding, in the document's line ending, and text is tidied the way Edit
// tidies it. It returns ErrNoSuchStep for a position outside 0..Len(),
// ErrEmptyStep and ErrNotUTF8 as Edit does, and ErrNoSteps for a Document
// with no list to add to — the zero Document — rather than inventing a
// delimiter and an indentation the document never chose.
func (d Document) Insert(at int, text string) (Document, error) {
	if len(d.steps) == 0 {
		return Document{}, fmt.Errorf("%w: there is no list to add a step to", ErrNoSteps)
	}
	if at < 0 || at > len(d.steps) {
		return Document{}, fmt.Errorf("%w: cannot insert at %d, and the plan has %d step(s)", ErrNoSuchStep, at, len(d.steps))
	}
	lines, err := tidy(text)
	if err != nil {
		return Document{}, err
	}
	fresh := step{indent: d.indent, delim: d.delim, pad: d.pad, text: joinLines(lines)}
	return d.relaid(slices.Insert(slices.Clone(d.steps), at, fresh))
}

// Delete removes Steps()[i] and renumbers the steps after it.
//
// It refuses to remove the only step, with ErrLastStep: what would be left
// is a Steps heading with no list under it, which Parse itself cannot read
// back as a document with steps, so nothing could add one again.
func (d Document) Delete(i int) (Document, error) {
	if err := d.checkIndex(i); err != nil {
		return Document{}, err
	}
	if len(d.steps) == 1 {
		return Document{}, ErrLastStep
	}
	return d.relaid(slices.Delete(slices.Clone(d.steps), i, i+1))
}

// Move moves Steps()[from] so that it becomes Steps()[to], and renumbers
// the list. Moving a step to where it already is returns the document
// unchanged, byte for byte: it is not a reason to renumber a lazily
// numbered list.
func (d Document) Move(from, to int) (Document, error) {
	if err := d.checkIndex(from); err != nil {
		return Document{}, err
	}
	if err := d.checkIndex(to); err != nil {
		return Document{}, err
	}
	if from == to {
		return d, nil
	}
	moved := d.steps[from]
	steps := slices.Delete(slices.Clone(d.steps), from, from+1)
	return d.relaid(slices.Insert(steps, to, moved))
}

// relaid returns d with steps as its list, laid out again.
//
// Steps are numbered from the list's own start number. A step whose number
// is already right keeps its bytes; any other step is written again from
// its text, which is what moves its continuation lines to the new content
// column when its number gains or loses a digit. Steps are then spaced one
// blank line apart in a loose list and zero apart in a tight one: a list
// whose shape changed has no other spacing that would be right for every
// step in it.
//
// The step that ends up last is the only one left without a line ending,
// and only when the document never ended with one. Head and tail are not
// touched. The result is read back before it is returned; see readBack.
func (d Document) relaid(steps []step) (Document, error) {
	last := len(steps) - 1
	unterminated := len(d.tail) == 0 && !d.finalEOL
	var gap []byte
	if d.loose {
		gap = []byte(d.eol)
	}

	for k, s := range steps {
		number := d.start + k
		terminate := k != last || !unterminated
		if digits := strconv.Itoa(number); s.digits != digits || s.indent != d.indent {
			s.number, s.digits, s.indent = number, digits, d.indent
			s = s.rewritten(strings.Split(s.text, "\n"), d.eol, terminate)
		} else {
			s.raw = terminated(s.raw, terminate, d.eol)
		}
		s.gap = nil
		if k != last {
			s.gap = gap
		}
		steps[k] = s
	}

	d.steps = steps
	return d.readBack()
}

// terminated returns raw with a line ending at its end when want is true,
// and without one when it is false. raw is never written through: it may
// share its array with the document the caller still holds.
func terminated(raw []byte, want bool, eol string) []byte {
	has := bytes.HasSuffix(raw, []byte("\n"))
	switch {
	case want && !has:
		out := make([]byte, 0, len(raw)+len(eol))
		out = append(out, raw...)
		return append(out, eol...)
	case !want && has:
		return bytes.TrimSuffix(bytes.TrimSuffix(raw, []byte("\n")), []byte("\r"))
	}
	return raw
}

// readBack parses d's bytes and returns d only if they read back as exactly
// the head, steps and tail d holds; otherwise it returns ErrAmbiguousEdit.
//
// Every rule above exists to make this check pass, so it is not expected to
// fire — but a line after the list indented just short of the last step's
// content column is text an edit can pull into that step without anyone
// asking, and a check that reads the result back catches that and any case
// nobody has thought of yet with one rule. A plan is a few kilobytes, so the
// second parse costs nothing a person would notice.
func (d Document) readBack() (Document, error) {
	again, err := Parse(d.Bytes())
	if err != nil {
		return Document{}, fmt.Errorf("%w: the edited plan no longer parses: %w", ErrAmbiguousEdit, err)
	}
	if !sameLayout(d, again) {
		return Document{}, fmt.Errorf("%w: the edited plan reads back as different steps", ErrAmbiguousEdit)
	}
	return d, nil
}

// sameLayout reports whether two documents split into the same regions and
// read the same steps out of them.
func sameLayout(a, b Document) bool {
	if len(a.steps) != len(b.steps) || !bytes.Equal(a.head, b.head) || !bytes.Equal(a.tail, b.tail) {
		return false
	}
	for i, s := range a.steps {
		t := b.steps[i]
		if s.number != t.number || s.text != t.text || !bytes.Equal(s.raw, t.raw) || !bytes.Equal(s.gap, t.gap) {
			return false
		}
	}
	return true
}
