// Package planview is belay's plan viewer: a full-screen terminal page where
// a person reads the plan a run stopped to show them, edits its steps, and
// then approves it, asks the agent for a different one, or leaves the run
// paused.
//
// It is the only package in belay that imports a terminal library
// (boundary_test.go enforces it), and only the command layer imports it.
package planview

import (
	"bytes"
	"errors"
	"fmt"

	"charm.land/bubbles/v2/textarea"
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
	// Path is where the plan file is, so the person can open it elsewhere.
	Path string
	// Plan is the plan document, as it is on disk.
	Plan []byte
	// Mode says whether the person may change anything.
	Mode Mode
	// Save writes plan to the plan file. The viewer never writes a file
	// itself: this is the only way anything it does reaches the disk.
	Save func(plan []byte, overwrite bool) error
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

// screen is what the viewer is doing: which keys mean what, and what the
// foot of the page says.
type screen int

const (
	// reading is the plan itself, and the keys that act on it.
	reading screen = iota
	// editing is a step open in the editor.
	editing
)

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
	// plan is the plan's bytes as they stand now, and disk is what the file
	// held when it was last read or written. They differ exactly when there
	// are edits nobody has saved.
	plan []byte
	disk []byte
	// undo holds the documents before each change, most recent last.
	undo []plandoc.Document

	selected      int
	offset        int
	width, height int

	screen screen
	// editor is the text box of the step being edited, editAt is the step,
	// and shown is what the box held when it opened, as it shows it. adding
	// is true when the box holds a new step, which goes in at editAt.
	editor textarea.Model
	editAt int
	shown  string
	adding bool

	// message is the viewer's answer to the last key, when it has one.
	message string

	decision Decision
}

var _ tea.Model = Model{}

// New returns a viewer showing what opts describes.
func New(opts Options) Model {
	m := Model{opts: opts, glyphs: unicodeGlyphs, plan: opts.Plan, disk: opts.Plan}
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
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = size.Width, size.Height
		if m.screen == editing {
			m.sizeEditor(&m.editor)
		}
		m.follow()
		return m, nil
	}
	if m.screen == editing {
		return m.edit(msg)
	}
	if key, ok := msg.(tea.KeyPressMsg); ok {
		m.message = ""
		m.read(key)
	}
	return m, nil
}

// read answers a key pressed while the person is reading the plan.
func (m *Model) read(msg tea.KeyPressMsg) {
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
	case "e", "enter":
		m.openEditor()
	case "a":
		m.openNewStep()
	case "d":
		m.deleteStep()
	case "K", "shift+up":
		m.moveStep(-1)
	case "J", "shift+down":
		m.moveStep(+1)
	case "u":
		m.undoChange()
	case "s":
		m.save()
	}
}

// edit answers a message while a step is open in the editor: ctrl+s keeps
// the edit, esc throws it away, and everything else is typing.
func (m Model) edit(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "ctrl+s":
			m.keepEdit()
			return m, nil
		case "esc":
			m.closeEditor()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(msg)
	m.follow()
	return m, cmd
}

// selectStep moves the selection to step i, held inside the plan's steps,
// and scrolls it into view.
func (m *Model) selectStep(i int) {
	m.selected = max(0, min(i, m.doc.Len()-1))
	m.follow()
}

// openEditor opens the selected step in the editor.
func (m *Model) openEditor() {
	steps := m.doc.Steps()
	if len(steps) == 0 {
		return
	}
	m.screen = editing
	m.editAt = m.selected
	m.adding = false
	m.editor = m.newEditor(steps[m.selected].Text)
	m.shown = m.editor.Value()
	m.follow()
}

// openNewStep opens an empty editor for a step to go in after the selected
// one.
func (m *Model) openNewStep() {
	if m.doc.Len() == 0 {
		return
	}
	m.screen = editing
	m.editAt = m.selected + 1
	m.adding = true
	m.editor = m.newEditor("")
	m.shown = ""
	m.follow()
}

// deleteStep deletes the selected step. It does not ask first: u puts the
// step back, and a question before every delete is a question people learn
// to answer without reading.
func (m *Model) deleteStep() {
	if m.doc.Len() == 0 {
		return
	}
	doc, err := m.doc.Delete(m.selected)
	if err != nil {
		m.message = m.refusal(err)
		return
	}
	number := m.selected + 1
	m.change(doc)
	m.message = fmt.Sprintf("Deleted step %d; u puts it back.", number)
}

// moveStep moves the selected step by delta places, and the selection with
// it. A step already at that end of the list stays where it is.
func (m *Model) moveStep(delta int) {
	to := m.selected + delta
	if m.doc.Len() == 0 || to < 0 || to >= m.doc.Len() {
		return
	}
	doc, err := m.doc.Move(m.selected, to)
	if err != nil {
		m.message = m.refusal(err)
		return
	}
	m.selected = to
	m.change(doc)
}

// keepEdit puts the editor's text into the plan, through plandoc, and closes
// the editor. A refusal leaves the editor open, with the reason said.
//
// Text the person did not change is not an edit, even when it differs from
// the step: the editor shows some characters differently from how the file
// holds them, and writing its version back would change a step nobody
// touched.
func (m *Model) keepEdit() {
	text := m.editor.Value()
	if !m.adding && text == m.shown {
		m.closeEditor()
		return
	}
	var (
		doc plandoc.Document
		err error
	)
	if m.adding {
		doc, err = m.doc.Insert(m.editAt, text)
	} else {
		doc, err = m.doc.Edit(m.editAt, text)
	}
	if err != nil {
		m.message = m.refusal(err)
		return
	}
	m.selected = m.editAt
	m.change(doc)
	m.closeEditor()
}

// closeEditor goes back to reading the plan.
func (m *Model) closeEditor() {
	m.screen = reading
	m.editor = textarea.Model{}
	m.follow()
}

// change makes doc the plan, keeping the document it replaces for undo.
func (m *Model) change(doc plandoc.Document) {
	m.undo = append(m.undo, m.doc)
	m.doc = doc
	m.plan = doc.Bytes()
	m.selectStep(m.selected)
}

// undoChange puts back the document before the last change.
func (m *Model) undoChange() {
	if len(m.undo) == 0 {
		m.message = "There is nothing to undo."
		return
	}
	m.doc = m.undo[len(m.undo)-1]
	m.undo = m.undo[:len(m.undo)-1]
	m.plan = m.doc.Bytes()
	m.selectStep(m.selected)
	m.message = "Undone."
}

// dirty reports whether there are edits nobody has saved.
func (m Model) dirty() bool { return !bytes.Equal(m.plan, m.disk) }

// save writes the plan through Options.Save, and says what happened.
func (m *Model) save() {
	if !m.dirty() {
		m.message = "There is nothing to save: the plan file already holds what you see."
		return
	}
	if m.opts.Save == nil {
		m.message = "belay cannot save this plan from here."
		return
	}
	if err := m.opts.Save(m.plan, false); err != nil {
		m.message = "belay could not save the plan: " + err.Error()
		return
	}
	m.disk = m.plan
	m.message = "belay saved the plan to " + m.opts.Path + "."
}

// refusal says, for a person, why plandoc would not make a change.
func (m Model) refusal(err error) string {
	switch {
	case errors.Is(err, plandoc.ErrEmptyStep):
		return "A step cannot be empty. Type what it should say, or press esc to leave it as it was."
	case errors.Is(err, plandoc.ErrLastStep):
		return "A plan needs at least one step, so belay will not delete its only one. Press e to rewrite it instead."
	case errors.Is(err, plandoc.ErrAmbiguousEdit):
		return "belay did not make that change: it would have moved text from one step into another. " +
			"Make it in the plan file instead: " + m.opts.Path
	case errors.Is(err, plandoc.ErrNotUTF8):
		return "That text is not valid UTF-8, so belay cannot put it in the plan."
	default:
		return "belay could not make that change: " + err.Error()
	}
}
