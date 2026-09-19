package plandoc

import "strings"

// The line-level rules this package recognizes. They are a deliberately
// small subset of CommonMark: enough to find the step list and put it back,
// and no more. See the package doc for what is left out.
const (
	// maxMarkerIndent is how far a list marker may be indented and still
	// belong to the document's own top-level list. CommonMark allows three
	// spaces before a block start; four would make it indented code, and
	// more than the item's content column would nest it inside the step
	// above.
	maxMarkerIndent = 3
	// tabWidth is the column a tab advances to a multiple of. CommonMark
	// fixes it at 4. Tabs are measured with it and never rewritten as
	// spaces.
	tabWidth = 4
	// minFence is the shortest run of backticks or tildes that opens a
	// fenced code block.
	minFence = 3
	// maxDigits is how many digits an ordered list marker may have.
	maxDigits = 9
)

// span is a line's half-open byte range in the document, including its line
// terminator. Lines are kept as ranges rather than strings so that every
// region of the document stays a slice of the one buffer Parse copied, which
// is what makes an untouched document byte-identical on the way out.
type span struct{ start, end int }

// splitLines returns one span per line. A final line with no terminator is a
// line: a plan that does not end with a newline must round-trip too.
func splitLines(buf []byte) []span {
	var out []span
	start := 0
	for i, b := range buf {
		if b == '\n' {
			out = append(out, span{start: start, end: i + 1})
			start = i + 1
		}
	}
	if start < len(buf) {
		out = append(out, span{start: start, end: len(buf)})
	}
	return out
}

// body returns the line's text without its terminator, so that a CRLF
// document and an LF one are read by exactly the same rules.
func body(buf []byte, s span) string {
	text := string(buf[s.start:s.end])
	text = strings.TrimSuffix(text, "\n")
	return strings.TrimSuffix(text, "\r")
}

// detectEOL returns the line ending the document uses, which is the one its
// first line ends with. A document with no line ending at all is written
// back with "\n".
func detectEOL(buf []byte) string {
	i := strings.IndexByte(string(buf), '\n')
	if i > 0 && buf[i-1] == '\r' {
		return "\r\n"
	}
	return "\n"
}

// isBlank reports whether the line holds nothing but whitespace.
func isBlank(text string) bool { return strings.TrimSpace(text) == "" }

// indentWidth returns the display column the line's first non-whitespace
// character sits at, expanding tabs the way CommonMark does.
func indentWidth(text string) int {
	return advance(0, text[:len(text)-len(strings.TrimLeft(text, " \t"))])
}

// advance returns the column reached by writing ws starting at column col.
//
// A tab stops at the next multiple of tabWidth from where it actually
// starts, which is why the column has to be threaded through: the tab in
// "1.\tfoo" starts at column 2 and is two columns wide, not four.
func advance(col int, ws string) int {
	for _, r := range ws {
		if r == '\t' {
			col += tabWidth - col%tabWidth
			continue
		}
		col++
	}
	return col
}

// stripIndent removes up to col columns of leading whitespace, which is how
// a continuation line is brought back to column 0 for editing. Indentation
// past col is the step's own structure — a nested list, a fenced block — and
// is kept.
func stripIndent(text string, col int) string {
	consumed := 0
	for i, r := range text {
		if consumed >= col {
			return text[i:]
		}
		switch r {
		case ' ':
			consumed++
		case '\t':
			consumed += tabWidth - consumed%tabWidth
		default:
			return text[i:]
		}
	}
	return ""
}

// joinLines joins content lines with "\n". Step text is always LF-joined,
// whatever the document's own line ending is; the eol is re-applied when the
// step is written back.
func joinLines(lines []string) string { return strings.Join(lines, "\n") }

// isStepsHeading reports whether the line is the level-2 "Steps" heading
// that internal/nodes/plan's prompt asks the agent for.
//
// The match is case-insensitive and tolerates an ATX closing sequence
// ("## Steps ##"), because both are the same heading to every Markdown
// renderer and a human editing a plan should not have to know which spelling
// belay's parser happens to prefer. It is not loosened further: "## Steps to
// take" is a different heading, and guessing that it meant this one would
// put the parser in charge of prose it cannot read.
func isStepsHeading(text string) bool {
	if indentWidth(text) > maxMarkerIndent {
		return false
	}
	rest := strings.TrimLeft(text, " \t")
	if !strings.HasPrefix(rest, "##") {
		return false
	}
	rest = rest[2:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return false
	}
	return strings.EqualFold(headingText(rest), "steps")
}

// isSectionBreak reports whether the line starts a heading at the Steps
// heading's own level or above, which is where the Steps section ends.
func isSectionBreak(text string) bool {
	if indentWidth(text) > maxMarkerIndent {
		return false
	}
	rest := strings.TrimLeft(text, " \t")
	hashes := len(rest) - len(strings.TrimLeft(rest, "#"))
	if hashes == 0 || hashes > 2 {
		return false
	}
	rest = rest[hashes:]
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}

// headingText returns an ATX heading's text with its optional closing run of
// '#' removed. CommonMark only treats that run as a closing sequence when a
// space precedes it, which is what keeps a heading reading "## C#" intact.
func headingText(rest string) string {
	text := strings.TrimSpace(rest)
	trimmed := strings.TrimRight(text, "#")
	if trimmed == text {
		return text
	}
	if trimmed == "" || strings.HasSuffix(trimmed, " ") || strings.HasSuffix(trimmed, "\t") {
		return strings.TrimSpace(trimmed)
	}
	return text
}

// marker is an ordered list item's marker, as the document writes it.
type marker struct {
	indent string // the whitespace before the digits
	digits string // the number as written
	delim  byte   // '.' or ')'
	pad    string // the whitespace between the delimiter and the content
	col    int    // the display column the content starts at
	number int
}

// parseMarker reads an ordered list marker from the start of a line.
//
// CommonMark's rules, because a plan is read by other Markdown tools too:
// at most nine digits, '.' or ')' after them, and the marker must be
// followed by whitespace or end the line. Content that starts more than four
// columns after the delimiter is an indented code block inside the item, so
// only the first space counts towards the content column.
func parseMarker(text string) (marker, bool) {
	indent := text[:len(text)-len(strings.TrimLeft(text, " \t"))]
	rest := text[len(indent):]

	digits := rest[:len(rest)-len(strings.TrimLeft(rest, "0123456789"))]
	if digits == "" || len(digits) > maxDigits {
		return marker{}, false
	}
	rest = rest[len(digits):]
	if rest == "" || (rest[0] != '.' && rest[0] != ')') {
		return marker{}, false
	}
	delim := rest[0]
	rest = rest[1:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return marker{}, false
	}

	pad := rest[:len(rest)-len(strings.TrimLeft(rest, " \t"))]
	markerEnd := indentWidth(indent) + len(digits) + 1
	width := advance(markerEnd, pad) - markerEnd
	switch {
	case pad == "":
		// An item whose marker is alone on its line: its content column is
		// where a single space would have put it.
		width = 1
	case width > tabWidth, len(pad) == len(rest):
		// Deeper padding, or a marker with nothing after it, both mean the
		// content column is one past the delimiter.
		pad, width = pad[:1], 1
	}

	return marker{
		indent: indent,
		digits: digits,
		delim:  delim,
		pad:    pad,
		col:    markerEnd + width,
		number: atoi(digits),
	}, true
}

// markerContent returns what the marker line says after its marker.
func markerContent(text string) string {
	m, ok := parseMarker(text)
	if !ok {
		return text
	}
	return text[len(m.indent)+len(m.digits)+1+len(m.pad):]
}

// atoi converts a run of digits that parseMarker has already validated. It
// saturates instead of overflowing: nine digits cannot overflow an int on
// any platform belay builds for, and a saturating conversion still could not
// produce a wrong number here.
func atoi(digits string) int {
	n := 0
	for _, r := range digits {
		n = n*10 + int(r-'0')
	}
	return n
}

// fence tracks whether the scanner is inside a fenced code block. Steps,
// headings and list markers inside one are text, not structure: a plan that
// shows what a plan looks like contains both, and reading them would make
// the parser edit a code sample.
type fence struct {
	open bool
	char byte
	size int
}

// track advances the tracker by one line and reports whether that line is
// part of a fenced code block, opening fence and closing fence included.
func (f *fence) track(text string) bool {
	if f.open {
		if f.closedBy(text) {
			f.open = false
		}
		return true
	}
	char, size, ok := opensFence(text)
	if !ok {
		return false
	}
	f.open, f.char, f.size = true, char, size
	return true
}

// closedBy reports whether the line closes the open fence: at least as many
// of the same character, and nothing else on the line.
func (f *fence) closedBy(text string) bool {
	char, size, ok := opensFence(text)
	return ok && char == f.char && size >= f.size &&
		strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(text), string(char))) == ""
}

// opensFence reports whether the line is a code fence, and returns the
// character it is made of and how long it is.
func opensFence(text string) (char byte, size int, ok bool) {
	if indentWidth(text) > maxMarkerIndent {
		return 0, 0, false
	}
	rest := strings.TrimLeft(text, " \t")
	if rest == "" {
		return 0, 0, false
	}
	char = rest[0]
	if char != '`' && char != '~' {
		return 0, 0, false
	}
	size = len(rest) - len(strings.TrimLeft(rest, string(char)))
	if size < minFence {
		return 0, 0, false
	}
	// A backtick fence's info string may not contain a backtick, which is
	// what keeps inline code (`like this`) from opening a block.
	if char == '`' && strings.Contains(rest[size:], "`") {
		return 0, 0, false
	}
	return char, size, true
}
