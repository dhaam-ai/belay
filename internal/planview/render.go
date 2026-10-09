package planview

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dhaam-ai/belay/internal/termsafe"
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

// glyphs are the characters the viewer draws its own chrome with, and
// whether everything it draws must be ASCII.
type glyphs struct {
	cursor string // beside the selected step
	rule   string // repeated to draw a horizontal line
	sep    string // between two key hints
	more   string // where text was cut short
	bar    string // down the left of the editor
	ascii  bool   // nothing but ASCII may reach the terminal
}

// unicodeGlyphs are for a terminal whose locale is UTF-8.
var unicodeGlyphs = glyphs{cursor: "›", rule: "─", sep: " · ", more: "…", bar: "│"}

// asciiGlyphs are for a terminal whose locale is not UTF-8. There, any byte
// from 0x80 up is not part of a letter: a Latin-1 terminal reads 0x80 to
// 0x9F as C1 controls, so the continuation byte of a UTF-8 arrow can start a
// control sequence. Nothing but ASCII is drawn.
var asciiGlyphs = glyphs{cursor: ">", rule: "-", sep: " | ", more: "...", bar: "|", ascii: true}

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

// tabWidth is how many spaces a tab in the plan is drawn as: what the
// editor turns a tab into, so a step reads the same open or closed.
const tabWidth = 4

// clean makes text from outside belay safe to draw: the plan, the goal, and
// anything an error quotes. Every control character becomes the visible
// stand-in internal/termsafe gives it rather than reaching the terminal,
// where an ESC or a C1 byte starts a sequence the terminal acts on —
// writing the clipboard (OSC 52), clearing the screen — and a plan can quote
// such text from the repository it was written about. A C0 control or DEL
// becomes its Unicode control picture (ESC is ␛), a C1 control its code in
// angle brackets, and a tab spaces. The rule is termsafe's, shared with
// every other place belay prints text it did not write, so the viewer and
// `belay plan --no-input` show a plan the same way.
//
// Outside a UTF-8 locale the stand-ins are ASCII — caret notation, ^[ for
// ESC — and every letter beyond ASCII is drawn as ?, for the reason
// asciiGlyphs gives.
func (g glyphs) clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		stand, control := termsafe.StandIn(r, g.ascii)
		switch {
		case r == '\t':
			b.WriteString(strings.Repeat(" ", tabWidth))
		case control:
			b.WriteString(stand)
		case r >= 0x80 && g.ascii:
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// layout lays text out in lines no wider than width: every line of text,
// made safe to draw with clean and wrapped at width.
func layout(text []byte, width, step int, clean func(string) string) []line {
	var out []line
	parts := strings.Split(string(text), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	for i, p := range parts {
		p = clean(strings.TrimSuffix(p, "\r"))
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
	clean := m.glyphs.clean
	if m.stepsErr != nil {
		return layout(m.plan, width, -1, clean)
	}
	head, steps, tail := m.doc.Parts()
	lines := layout(head, width, -1, clean)
	for i, step := range steps {
		switch {
		case m.editorOpen() && m.adding && i == m.editAt:
			lines = append(lines, m.editorLines(-1)...)
		case m.editorOpen() && !m.adding && i == m.editAt:
			lines = append(lines, m.editorLines(i)...)
			continue
		}
		lines = append(lines, layout(step, width, i, clean)...)
	}
	if m.editorOpen() && m.adding && m.editAt == len(steps) {
		lines = append(lines, m.editorLines(-1)...)
	}
	return append(lines, layout(tail, width, -1, clean)...)
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
		text := m.gutter(lines[i]) + lines[i].text
		if !m.editorOpen() && lines[i].step == m.selected && m.doc.Len() > 0 {
			text = m.emph(text)
		}
		out = append(out, text)
	}
	return append(out, m.footer()...)
}

// emph emphasises s, in bold, when colour is allowed. The viewer emphasises
// two things only, as belay does everywhere (ADR-0010): the step the person
// is on, and what the run has spent. Neither carries meaning by emphasis
// alone — the cursor glyph marks the step, and the spend is in words — so a
// terminal without it loses nothing.
func (m Model) emph(s string) string {
	if !m.opts.Color || s == "" {
		return s
	}
	return "\x1b[1m" + s + "\x1b[22m"
}

// cost is the spend, laid out and emphasised.
func (m Model) cost() []string {
	lines := m.sentences(m.spent())
	for i, l := range lines {
		lines[i] = m.emph(l)
	}
	return lines
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
	title := "Plan for run " + m.glyphs.clean(m.opts.RunID)
	if m.dirty() {
		title += m.glyphs.sep + "edited"
	}
	return []string{
		fit(title, m.width, m.glyphs.more),
		fit("You asked for: "+m.glyphs.clean(m.opts.Goal), m.width, m.glyphs.more),
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
		msg := wrap(m.glyphs.clean(m.message), max(1, m.width))
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
	case keyList:
		var lines []string
		for _, k := range keyHelp {
			if k.change && m.opts.Mode == ReadOnly {
				continue
			}
			lines = append(lines, fit(fmt.Sprintf("%-12s %s", k.key, k.does), m.width, m.glyphs.more))
		}
		return append([]block{{lines: lines}}, hints("esc goes back"))
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
			{lines: m.cost()},
		}
		if m.dirty() {
			out = append(out, block{lines: m.sentences("Your edits are saved first."), drop: 2})
		}
		return append(out, hints("y approve and continue", "n go back"))
	case asking:
		out := []block{
			{lines: m.sentences("Say what the new plan should do differently. belay runs the plan step again with " +
				"your words, and stops here again with the new plan."), drop: 4},
			{lines: m.cost(), drop: 2},
		}
		if m.dirty() {
			out = append(out, block{lines: m.sentences("Your edits are saved first, and kept with the plan they replace."), drop: 3})
		}
		return append(out,
			block{lines: strings.Split(m.editor.View(), "\n"), box: true},
			hints("enter asks for it", "esc goes back"))
	}
	var out []block
	if m.screen == editing && m.altered {
		out = append(out, block{drop: 2, lines: m.sentences("This step holds tabs or control characters, which the " +
			"editor shows as spaces or leaves out. Keep it unchanged and nothing changes; change it and it is saved as shown.")})
	}
	if m.glyphs.ascii {
		out = append(out, block{lines: m.sentences(m.localeNotice()), drop: 2})
	}
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
		out = append(out, wrap(m.glyphs.clean(text), max(1, m.width))...)
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

// localeNotice says why the viewer is drawing in ASCII, naming the setting
// that made the locale what it is, so the person knows what to change.
func (m Model) localeNotice() string {
	locale := "This terminal's locale"
	if m.opts.Locale != "" {
		locale += " (" + m.opts.Locale + ")"
	}
	return locale + " is not UTF-8, so belay shows letters outside ASCII as ?, and they cannot be typed here."
}

// keyHelp is every key the viewer answers to, and what it does, as the ?
// screen lists them. change marks the ones a read-only viewer does not
// offer.
var keyHelp = []struct {
	key    string
	does   string
	change bool
}{
	{key: "j k, arrows", does: "choose a step, or scroll a plan that has none"},
	{key: "pgup pgdown", does: "scroll a page at a time"},
	{key: "home end", does: "go to the first or the last step"},
	{key: "e, enter", does: "edit the selected step (ctrl+s keeps it, esc throws it away)", change: true},
	{key: "a", does: "add a step after the selected one", change: true},
	{key: "d", does: "delete the selected step", change: true},
	{key: "K J", does: "move the selected step up or down", change: true},
	{key: "u", does: "undo the last change", change: true},
	{key: "s", does: "save the plan to its file", change: true},
	{key: "c", does: "approve the plan and continue the run", change: true},
	{key: "r", does: "ask the agent for a new plan", change: true},
	{key: "q, ctrl+c", does: "leave, and keep the run waiting at its plan"},
	{key: "ctrl+z", does: "put belay in the background"},
	{key: "?", does: "show or hide this list"},
}
