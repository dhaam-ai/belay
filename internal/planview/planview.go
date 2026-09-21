// Package planview is belay's plan viewer: a full-screen terminal page where
// a person reads the plan a run stopped to show them, edits its steps, and
// then approves it, asks the agent for a different one, or leaves the run
// paused.
//
// It is the only package in belay that imports a terminal library
// (boundary_test.go enforces it), and only the command layer imports it.
package planview

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/dhaam-ai/belay/internal/plandoc"
)

// Mode says what a person may do in the viewer.
type Mode int

// The modes the viewer opens in.
const (
	// ReadOnly shows the plan, and lets the person scroll it and leave.
	ReadOnly Mode = iota
	// Review also lets them edit the steps, save, approve the plan and ask
	// for a new one.
	Review
)

// Options is what the viewer is given to show.
type Options struct {
	// RunID names the run the plan belongs to.
	RunID string
	// Goal is what the person asked for, exactly as they typed it.
	Goal string
	// Path is where the plan file is.
	Path string
	// Plan is the plan document, as it is on disk.
	Plan []byte
	// Mode says whether the person may change anything.
	Mode Mode
}

// Action is what the person decided to do with the run.
type Action int

// The decisions a person can leave the viewer with.
const (
	// ActionLeave leaves the run as it is: paused at its plan.
	ActionLeave Action = iota
)

// Decision is what the person decided, which the command layer carries out
// once the viewer has closed.
type Decision struct {
	// Action is what they decided.
	Action Action
	// Guidance is what they said a new plan should do differently.
	Guidance string
}

// Model is the viewer's state. It is a bubbletea model: every change to it
// arrives as a message through Update, which is what lets its tests drive it
// with key presses and no terminal at all.
type Model struct {
	opts   Options
	glyphs glyphs

	// doc is the plan as edited so far, and stepsErr is plandoc's refusal
	// to edit it, when it refused; doc is the zero Document then.
	doc      plandoc.Document
	stepsErr error
	// plan is the plan's bytes as they stand now.
	plan []byte

	selected      int
	offset        int
	width, height int

	decision Decision
}

var _ tea.Model = Model{}

// New returns a viewer showing what opts describes.
func New(opts Options) Model {
	m := Model{opts: opts, glyphs: unicodeGlyphs, plan: opts.Plan}
	m.doc, m.stepsErr = plandoc.Parse(opts.Plan)
	return m
}

// Decision returns what the person decided. It is ActionLeave until they
// decide something else.
func (m Model) Decision() Decision { return m.decision }

// Init implements tea.Model.
func (Model) Init() tea.Cmd { return nil }

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.follow()
	case tea.KeyPressMsg:
		m.browse(msg)
	}
	return m, nil
}

// browse answers a key pressed while the person is reading the plan.
func (m *Model) browse(msg tea.KeyPressMsg) {
	switch msg.String() {
	case "down", "j":
		m.selectStep(m.selected + 1)
	case "up", "k":
		m.selectStep(m.selected - 1)
	case "home", "g":
		m.selectStep(0)
		m.offset = 0
	case "end", "G":
		m.selectStep(m.doc.Len() - 1)
	}
}

// selectStep moves the selection to step i, held inside the plan's steps,
// and scrolls it into view.
func (m *Model) selectStep(i int) {
	m.selected = max(0, min(i, m.doc.Len()-1))
	m.follow()
}

// bodyHeight is how many lines of the plan fit on the screen.
func (m Model) bodyHeight() int {
	return max(1, m.height-headerHeight-len(m.footer()))
}

// follow scrolls so that the selected step is on the screen: all of it when
// it fits, and its first line when it does not.
func (m *Model) follow() {
	lines := m.body()
	first, last := -1, -1
	for i, l := range lines {
		if l.step == m.selected {
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

// body lays the plan out for the screen's width.
func (m Model) body() []line {
	width := max(1, m.width-gutterWidth)
	if m.stepsErr != nil {
		return layout(m.plan, width, -1)
	}
	head, steps, tail := m.doc.Parts()
	lines := layout(head, width, -1)
	for i, step := range steps {
		lines = append(lines, layout(step, width, i)...)
	}
	return append(lines, layout(tail, width, -1)...)
}

// View implements tea.Model.
func (m Model) View() tea.View {
	v := tea.NewView(strings.Join(m.screen(), "\n"))
	v.AltScreen = true
	return v
}

// screen draws every line of the page, top to bottom.
func (m Model) screen() []string {
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
// selected step's first line, and blank space beside everything else.
func (m Model) gutter(l line) string {
	if l.first && l.step == m.selected {
		return m.glyphs.cursor + " "
	}
	return strings.Repeat(" ", gutterWidth)
}

// header names the run and what was asked for, above a rule.
func (m Model) header() []string {
	return []string{
		fit("Plan for run "+m.opts.RunID, m.width, m.glyphs.more),
		fit("You asked for: "+m.opts.Goal, m.width, m.glyphs.more),
		strings.Repeat(m.glyphs.rule, m.width),
	}
}

// footer is the rule and the key lines at the foot of the page.
func (m Model) footer() []string {
	sep := m.glyphs.sep
	if m.width < fullWidth {
		return []string{
			strings.Repeat(m.glyphs.rule, m.width),
			fit(strings.Join([]string{"c approve", "r new plan", "q leave", "? all keys"}, sep), m.width, m.glyphs.more),
		}
	}
	return []string{
		strings.Repeat(m.glyphs.rule, m.width),
		fit(strings.Join([]string{"e edit", "a add", "d delete", "K J move", "u undo", "s save"}, sep), m.width, m.glyphs.more),
		fit(strings.Join([]string{"c approve and continue", "r ask for a new plan", "q leave", "? all keys"}, sep),
			m.width, m.glyphs.more),
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
