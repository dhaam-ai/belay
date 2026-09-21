package planview

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
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

// maxMessageLines bounds how much of the foot of the page one message may
// take, so that a long one cannot push the plan off a small screen.
const maxMessageLines = 3

// glyphs are the characters the viewer draws its own chrome with.
type glyphs struct {
	cursor string // beside the selected step
	rule   string // repeated to draw a horizontal line
	sep    string // between two key hints
	more   string // where text was cut short
	bar    string // down the left of the editor
}

// unicodeGlyphs are for a terminal whose locale is UTF-8.
var unicodeGlyphs = glyphs{cursor: "›", rule: "─", sep: " · ", more: "…", bar: "│"}

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
	// editor is true on the lines of the editor.
	editor bool
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

// bodyHeight is how many lines of the plan fit on the screen.
func (m Model) bodyHeight() int {
	return max(1, m.height-headerHeight-len(m.footer()))
}

// follow scrolls so that what the person is working on is on the screen:
// the editor while one is open, and otherwise the selected step — all of
// it when it fits, and its first line when it does not.
func (m *Model) follow() {
	lines := m.body()
	first, last := -1, -1
	for i, l := range lines {
		if (m.editorOpen() && l.editor) || (!m.editorOpen() && l.step == m.selected) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	height := m.bodyHeight()
	if first >= 0 {
		switch {
		case last-first+1 >= height, first < m.offset:
			m.offset = first
		case last >= m.offset+height:
			m.offset = last - height + 1
		}
	}
	m.offset = max(0, min(m.offset, len(lines)-height))
}

// editorOpen reports whether the editor is on the screen: while a step is
// being typed, and under the question asked before leaving it.
func (m Model) editorOpen() bool {
	return m.screen == editing || (m.screen == leaving && m.back == editing)
}

// body lays the plan out for the screen's width, with the editor in place
// of the step it is editing.
func (m Model) body() []line {
	width := max(1, m.width-gutterWidth)
	if m.stepsErr != nil {
		return layout(m.plan, width, -1)
	}
	head, steps, tail := m.doc.Parts()
	lines := layout(head, width, -1)
	for i, step := range steps {
		switch {
		case m.editorOpen() && m.adding && i == m.editAt:
			lines = append(lines, m.editorLines(-1)...)
		case m.editorOpen() && !m.adding && i == m.editAt:
			lines = append(lines, m.editorLines(i)...)
			continue
		}
		lines = append(lines, layout(step, width, i)...)
	}
	if m.editorOpen() && m.adding && m.editAt == len(steps) {
		lines = append(lines, m.editorLines(-1)...)
	}
	return append(lines, layout(tail, width, -1)...)
}

// editorLines is the editor, laid out as lines of the plan belonging to
// step i, or to no step while it holds a new one.
func (m Model) editorLines(i int) []line {
	var out []line
	for j, text := range strings.Split(m.editor.View(), "\n") {
		out = append(out, line{text: text, step: i, first: j == 0, editor: true})
	}
	return out
}

// View implements tea.Model.
func (m Model) View() tea.View {
	v := tea.NewView(strings.Join(m.screenLines(), "\n"))
	v.AltScreen = true
	v.Cursor = m.cursor()
	return v
}

// cursor places the terminal's own cursor where the editor's is, or hides
// it when nothing is being typed.
func (m Model) cursor() *tea.Cursor {
	if (m.screen != editing && m.screen != asking) || m.width < minWidth || m.height < minHeight {
		return nil
	}
	c := m.editor.Cursor()
	if c == nil {
		return nil
	}
	if m.screen == asking {
		footer, box := m.footerLines()
		placed := *c
		placed.Y += m.height - len(footer) + box
		return &placed
	}
	top := -1
	for i, l := range m.body() {
		if l.editor {
			top = i
			break
		}
	}
	row := top - m.offset + c.Y
	if top < 0 || row < 0 || row >= m.bodyHeight() {
		return nil
	}
	placed := *c
	placed.X += gutterWidth
	placed.Y = headerHeight + row
	return &placed
}

// screenLines draws every line of the page, top to bottom.
func (m Model) screenLines() []string {
	if m.width < minWidth || m.height < minHeight {
		return m.tooSmall()
	}
	out := m.header()
	lines := m.body()
	height := m.bodyHeight()
	for i := m.offset; i < m.offset+height; i++ {
		if i >= len(lines) {
			out = append(out, "")
			continue
		}
		out = append(out, m.gutter(lines[i])+lines[i].text)
	}
	return append(out, m.footer()...)
}

// gutter is what is drawn beside a line of the plan: the cursor beside the
// first line of what the person is working on — the editor while one is
// open, the selected step otherwise — and blank space beside everything
// else.
func (m Model) gutter(l line) string {
	here := l.first && l.step == m.selected
	if m.editorOpen() {
		here = l.first && l.editor
	}
	if here {
		return m.glyphs.cursor + " "
	}
	return strings.Repeat(" ", gutterWidth)
}

// header names the run and what was asked for, above a rule. Unsaved edits
// are marked in words, so the mark survives a terminal without colour.
func (m Model) header() []string {
	title := "Plan for run " + m.opts.RunID
	if m.dirty() {
		title += m.glyphs.sep + "edited"
	}
	return []string{
		fit(title, m.width, m.glyphs.more),
		fit("You asked for: "+m.opts.Goal, m.width, m.glyphs.more),
		strings.Repeat(m.glyphs.rule, m.width),
	}
}

// footer is the rule, the viewer's last message, and the key lines at the
// foot of the page.
func (m Model) footer() []string {
	lines, _ := m.footerLines()
	return lines
}

// block is a run of footer lines, and how readily it gives way when the
// screen is too short for the whole footer: blocks with a higher drop go
// first, and a block with drop 0 never goes.
type block struct {
	lines []string
	drop  int
	box   bool // the new-plan box
}

// footerLines is the footer, and the index in it of the new-plan box's
// first line, or -1 when the box is not open.
//
// On a short screen the footer gives way before the plan disappears
// entirely: the rule, then the explanations, then the notes, in that
// order, until the page fits. The question being asked, the box being typed
// into and the keys that answer it are never dropped.
func (m Model) footerLines() ([]string, int) {
	blocks := []block{{lines: []string{strings.Repeat(m.glyphs.rule, m.width)}, drop: 5}}
	if m.message != "" {
		msg := wrap(m.message, max(1, m.width))
		if len(msg) > maxMessageLines {
			msg = append(msg[:maxMessageLines-1], fit(strings.Join(msg[maxMessageLines-1:], " "), m.width, m.glyphs.more))
		}
		blocks = append(blocks, block{lines: msg, drop: 1})
	}
	blocks = append(blocks, m.screenBlocks()...)

	room := max(1, m.height-headerHeight-1)
	for total(blocks) > room {
		worst := -1
		for i, b := range blocks {
			if b.drop > 0 && (worst < 0 || b.drop > blocks[worst].drop) {
				worst = i
			}
		}
		if worst < 0 {
			break
		}
		blocks = append(blocks[:worst], blocks[worst+1:]...)
	}

	var out []string
	box := -1
	for _, b := range blocks {
		if b.box {
			box = len(out)
		}
		out = append(out, b.lines...)
	}
	return out, box
}

// total counts the lines in blocks.
func total(blocks []block) int {
	n := 0
	for _, b := range blocks {
		n += len(b.lines)
	}
	return n
}

// screenBlocks is what the foot of the page says on the screen the viewer
// is on: a question and its answers, or the keys that do something.
func (m Model) screenBlocks() []block {
	hints := func(h ...string) block {
		return block{lines: []string{fit(strings.Join(h, m.glyphs.sep), m.width, m.glyphs.more)}}
	}
	switch m.screen {
	case leaving:
		return []block{
			{lines: m.sentences("Your edits are not saved yet.")},
			hints("y save and leave", "n leave without saving", "esc stay"),
		}
	case conflict:
		return []block{
			{lines: m.sentences("The plan file has changed since belay opened it; it was edited somewhere else, " +
				"perhaps. Saving now would overwrite that change.")},
			hints("r read it again", "o overwrite it with what you see", "esc go back"),
		}
	case approving:
		out := []block{
			{lines: m.sentences("Approve this plan and continue the run? belay writes the code next.")},
			{lines: m.sentences(m.spent())},
		}
		if m.dirty() {
			out = append(out, block{lines: m.sentences("Your edits are saved first."), drop: 2})
		}
		return append(out, hints("y approve and continue", "n go back"))
	case asking:
		out := []block{
			{lines: m.sentences("Say what the new plan should do differently. belay runs the plan step again with " +
				"your words, and stops here again with the new plan."), drop: 4},
			{lines: m.sentences(m.spent()), drop: 2},
		}
		if m.dirty() {
			out = append(out, block{lines: m.sentences("Your edits are saved first, and kept with the plan they replace."), drop: 3})
		}
		return append(out,
			block{lines: strings.Split(m.editor.View(), "\n"), box: true},
			hints("enter asks for it", "esc goes back"))
	}
	var out []block
	if m.opts.Mode == ReadOnly && m.opts.ReadOnlyReason != "" {
		out = append(out, block{lines: m.sentences(m.opts.ReadOnlyReason), drop: 2})
	}
	keys := m.keyLines()
	for i, l := range keys {
		b := block{lines: []string{l}}
		if i == 0 && len(keys) > 1 {
			b.drop = 3 // a status line or the editing keys, which the second line repeats in part
		}
		out = append(out, b)
	}
	return out
}

// keyLines says which keys do something now.
func (m Model) keyLines() []string {
	sep := m.glyphs.sep
	hints := func(h ...string) string { return fit(strings.Join(h, sep), m.width, m.glyphs.more) }
	if m.screen == editing {
		what := fmt.Sprintf("Editing step %d of %d.", m.editAt+1, m.doc.Len())
		if m.adding {
			what = fmt.Sprintf("A new step %d.", m.editAt+1)
		}
		return []string{
			fit(what, m.width, m.glyphs.more),
			hints("ctrl+s keeps the edit", "esc throws it away"),
		}
	}
	switch {
	case m.opts.Mode == ReadOnly:
		return []string{hints("j k scroll", "q leave", "? all keys")}
	case m.stepsErr != nil:
		return []string{hints("c approve", "r new plan", "j k scroll", "q leave", "? all keys")}
	case m.width < fullWidth:
		return []string{hints("c approve", "r new plan", "q leave", "? all keys")}
	}
	return []string{
		hints("e edit", "a add", "d delete", "K J move", "u undo", "s save"),
		hints("c approve and continue", "r ask for a new plan", "q leave", "? all keys"),
	}
}

// tooSmall is the whole page on a terminal too small to read a plan in.
func (m Model) tooSmall() []string {
	msg := "Make this window larger to review the plan. Your plan is safe; q leaves."
	lines := wrap(msg, max(1, m.width))
	if len(lines) > m.height {
		lines = lines[:max(0, m.height)]
	}
	return lines
}

// sentences lays out each sentence on lines of its own, wrapped rather than
// cut: a question cut off at the edge of a narrow terminal is a question
// nobody can answer.
func (m Model) sentences(texts ...string) []string {
	var out []string
	for _, text := range texts {
		out = append(out, wrap(text, max(1, m.width))...)
	}
	return out
}

// spent says what the run has spent against its ceiling, marking an
// estimate as one.
func (m Model) spent() string {
	s := m.opts.Spend
	amount := fmt.Sprintf("$%.2f", s.SpentUSD)
	estimated := ""
	if s.Estimated {
		estimated = " (estimated)"
	}
	if s.LimitUSD <= 0 {
		return "Spent so far: " + amount + estimated + ", with no dollar ceiling set."
	}
	return fmt.Sprintf("Spent so far: %s of $%.2f%s.", amount, s.LimitUSD, estimated)
}
