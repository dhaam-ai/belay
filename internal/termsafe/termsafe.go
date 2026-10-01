// Package termsafe makes text belay did not write safe to print on a
// terminal: a goal, a plan, a note an agent's error left in the journal, a
// file name, a directory name read off disk.
//
// Any of those can carry an escape sequence, and a terminal acts on one
// wherever it appears. A plan quoting a hostile repository can set the
// window title, write the clipboard (OSC 52), move the cursor up and erase
// the line above it, or clear the screen — and it does so whether or not
// belay has colour switched on, because colour decides what belay adds, not
// what a string already holds. In an in-place run the agent can also write
// the run's own files under .belay, so a manifest, a journal or a queue log
// is no more trustworthy than the plan.
//
// Every control character — C0, DEL and C1, which is Unicode's Cc category —
// becomes a visible stand-in (StandIn), the promise ADR-0015 makes: a C0
// control or DEL its Unicode control picture (ESC is ␛), a C1 control its
// code in angle brackets. A stand-in rather than nothing, on purpose. The
// person reading sees that something odd is in their plan, and a name with
// a byte dropped out of it can read as a different, innocent name.
//
// There are two shapes of text, and so two functions. Text is a block, such
// as a plan, whose newlines and tabs are its layout and are kept. Line is a
// field inside one line, such as a goal in a table or a note under a row,
// where a newline would let the text forge a line of belay's own: there
// every run of whitespace becomes one space.
//
// What it deliberately does not do:
//
//   - It does not take a locale. Text and Line draw UTF-8 stand-ins, as
//     belay's own output draws →, — and █, so they assume a UTF-8 terminal
//     the way the rest of belay's line output does. The plan viewer, which
//     knows the locale, asks StandIn for ASCII instead and draws every other
//     letter beyond ASCII as ?.
//   - It does not touch format characters (Unicode Cf), such as the
//     bidirectional overrides. A terminal does not act on them as commands.
//   - It is not for JSON. A script reading belay's JSON must get the text as
//     it was, so JSON escapes the two kinds of control encoding/json leaves
//     raw, instead of replacing anything.
package termsafe

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// StandIn returns what is drawn in place of r, and reports whether r is a
// control character at all; for any other rune it returns "" and false.
//
// A C0 control or DEL is its Unicode control picture, or, when ascii is set
// for a terminal whose locale is not UTF-8, its caret notation (^[ for ESC,
// ^? for DEL). A C1 control is its code in angle brackets in either case,
// "<9B>", because a C1 control has no picture and no caret form.
//
// Newline and tab are control characters too. Whether they are kept is the
// caller's decision, which Text and Line each make.
func StandIn(r rune, ascii bool) (string, bool) {
	switch {
	case r < 0x20 && ascii:
		return "^" + string(r+'@'), true
	case r < 0x20:
		return string(0x2400 + r), true
	case r == 0x7f && ascii:
		return "^?", true
	case r == 0x7f:
		return "␡", true
	case r >= 0x80 && r < 0xa0:
		return fmt.Sprintf("<%02X>", r), true
	default:
		return "", false
	}
}

// Text returns s safe to print as a block of text: its newlines and tabs are
// kept, a Windows line ending reads as a plain newline, and every other
// control character is drawn as its stand-in. A byte that is not UTF-8 is
// drawn as the replacement character, never passed through, since on its own
// it could be a C1 control to a terminal that reads 8-bit controls.
func Text(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r' && strings.HasPrefix(s[i+1:], "\n"):
			// The newline after it is kept on its own turn.
		default:
			write(&b, r)
		}
	}
	return b.String()
}

// Line returns s safe to print inside a single line: every run of
// whitespace, newlines included, becomes one space, the ends are trimmed,
// and every other control character is drawn as its stand-in.
//
// A carriage return and a C1 next-line are whitespace, so they become a
// space too: neither can return the cursor to overwrite what is already on
// the line.
func Line(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			pendingSpace = b.Len() > 0
			continue
		}
		if pendingSpace {
			b.WriteByte(' ')
			pendingSpace = false
		}
		write(&b, r)
	}
	return b.String()
}

// write draws r: its stand-in when it is a control character, the
// replacement character when it stands for a byte that was not UTF-8, and
// r itself otherwise.
func write(b *strings.Builder, r rune) {
	if stand, ok := StandIn(r, false); ok {
		b.WriteString(stand)
		return
	}
	b.WriteRune(r)
}

// JSON returns doc, a document encoding/json produced, with DEL and every C1
// control escaped as \u007f and \u0080 to \u009f.
//
// encoding/json already escapes every C0 control, ESC among them, but writes
// DEL and C1 controls through as they are, and a terminal that honours C1
// reads U+009B as the start of a control sequence. Escaping them changes
// nothing a JSON parser reads: those characters can only appear inside a
// string, where the escape means the same character. So a script gets the
// text exactly, and the document is safe to print.
func JSON(doc []byte) []byte {
	var out []byte
	for i := 0; i < len(doc); {
		r, size := utf8.DecodeRune(doc[i:])
		if r == 0x7f || (r >= 0x80 && r < 0xa0) {
			if out == nil {
				out = append(make([]byte, 0, len(doc)+16), doc[:i]...)
			}
			out = fmt.Appendf(out, `\u%04x`, r)
		} else if out != nil {
			out = append(out, doc[i:i+size]...)
		}
		i += size
	}
	if out == nil {
		return doc
	}
	return out
}
