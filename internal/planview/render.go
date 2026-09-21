package planview

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Size limits. Below minWidth by minHeight the plan cannot be read usefully,
// and the viewer says so instead of drawing a page nobody can follow. From
// minWidth to fullWidth the key line is the short one.
const (
	minWidth  = 40
	minHeight = 10
	fullWidth = 80
)

// gutterWidth is the column the selection cursor is drawn in, beside every
// line of the plan.
const gutterWidth = 2

// headerHeight is the header's line count: whose plan, what was asked for,
// and a rule under them.
const headerHeight = 3

// glyphs are the characters the viewer draws its own chrome with.
type glyphs struct {
	cursor string // beside the selected step
	rule   string // repeated to draw a horizontal line
	sep    string // between two key hints
	more   string // where text was cut short
}

// unicodeGlyphs are for a terminal whose locale is UTF-8.
var unicodeGlyphs = glyphs{cursor: "›", rule: "─", sep: " · ", more: "…"}

// line is one line of the plan as the viewer lays it out: its text, already
// made safe to print and cut to fit, and the step it belongs to.
type line struct {
	text string
	// step is the index of the step the line belongs to, or -1 for the text
	// around the steps.
	step int
	// first is true on a step's first line, which is the one the cursor is
	// drawn beside.
	first bool
}

// layout lays text out in lines no wider than width: every line of text,
// wrapped at width.
func layout(text []byte, width, step int) []line {
	var out []line
	parts := strings.Split(string(text), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	for i, p := range parts {
		p = strings.TrimSuffix(p, "\r")
		for j, w := range wrap(p, width) {
			out = append(out, line{text: w, step: step, first: step >= 0 && i == 0 && j == 0})
		}
	}
	return out
}

// wrap breaks s into lines of at most width cells, measured by display
// width, never by bytes or runes: a line of CJK text is twice as wide as it
// is long.
func wrap(s string, width int) []string {
	if s == "" {
		return []string{""}
	}
	return strings.Split(ansi.Wrap(s, width, ""), "\n")
}

// fit cuts s to at most width cells, marking the cut.
func fit(s string, width int, more string) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, more)
}
