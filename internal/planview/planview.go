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
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/plandoc"
)

// ErrChangedOnDisk is what Options.Save returns when the plan file no
// longer holds what the viewer last read from it or wrote to it.
var ErrChangedOnDisk = errors.New("planview: the plan file changed since the viewer read it")

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
	// ReadOnlyReason says why the person may not, in ReadOnly mode.
	ReadOnlyReason string
	// Save writes plan to the plan file. The viewer never writes a file
	// itself: this is the only way anything it does reaches the disk.
	Save func(plan []byte, overwrite bool) error
	// Reload reads the plan file again.
	Reload func() ([]byte, error)
	// Blocker is why the run cannot continue from here.
	Blocker string
	// Spend is what the run has spent, and may spend.
	Spend Spend
	// Color allows emphasis.
	Color bool
	// ASCII draws with ASCII characters only.
	ASCII bool
	// Locale names the setting that makes the locale what it is.
	Locale string
}

// Spend is what a run has spent, and the most it may.
type Spend struct {
	// SpentUSD is what the run has spent so far.
	SpentUSD float64
	// LimitUSD is its ceiling.
	LimitUSD float64
	// Estimated is true when some of SpentUSD is an estimate.
	Estimated bool
}

// Action is what the person decided to do with the run.
type Action int

// The decisions a person can leave the viewer with.
const (
	// ActionLeave leaves the run as it is: paused at its plan.
	ActionLeave Action = iota
	// ActionApprove approves the plan as saved, and continues the run.
	ActionApprove
	// ActionRegenerate asks the agent for a new plan.
	ActionRegenerate
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
	// leaving is the question asked before unsaved edits are left behind.
	leaving
	// approving is the question asked before the plan is approved.
	approving
	// asking is the box for what a new plan should do differently.
	asking
	// conflict is the question asked when a save finds the plan file
	// changed on disk.
	conflict
	// keyList is the list of every key and what it does.
	keyList
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
	// altered is true when the editor shows the step differently from how
	// the file holds it: tabs as spaces, control characters left out.
	altered bool

	// message is the viewer's answer to the last key, when it has one.
	message string
	// back is the screen the leaving question returns to on esc.
	back screen
	// pending is what a save that met a conflict was for: the decision the
	// viewer closes with once the person overwrites, or nil for a save.
	pending *Decision

	decision Decision
	// closing is set once the person has decided, and the viewer is to
	// close.
	closing bool
}

var _ tea.Model = Model{}

// New returns a viewer showing what opts describes.
func New(opts Options) Model {
	m := Model{opts: opts, glyphs: unicodeGlyphs, plan: opts.Plan, disk: opts.Plan}
	if opts.ASCII {
		m.glyphs = asciiGlyphs
	}
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
	var cmd tea.Cmd
	switch key, isKey := msg.(tea.KeyPressMsg); {
	case isKey && key.String() == "ctrl+z" && m.screen != editing && m.screen != asking:
		// Suspending belongs to the shell, not to the viewer: without this
		// raw mode would swallow it.
		return m, tea.Suspend
	case m.screen == keyList:
		if isKey {
			m.answerKeys(key)
		}
	case m.screen == editing:
		m, cmd = m.edit(msg)
	case m.screen == asking:
		m, cmd = m.ask(msg)
	case !isKey:
	case m.screen == leaving:
		m.answerLeaving(key)
	case m.screen == approving:
		m.answerApproving(key)
	case m.screen == conflict:
		m.answerConflict(key)
	default:
		m.message = ""
		m.read(key)
	}
	if m.closing {
		return m, tea.Quit
	}
	return m, cmd
}

// read answers a key pressed while the person is reading the plan.
func (m *Model) read(msg tea.KeyPressMsg) {
	k := msg.String()
	switch k {
	case "down", "j":
		m.moveDown(+1)
		return
	case "up", "k":
		m.moveDown(-1)
		return
	case "pgdown", "space":
		m.scroll(m.bodyHeight())
		return
	case "pgup":
		m.scroll(-m.bodyHeight())
		return
	case "home", "g":
		m.selectStep(0)
		m.offset = 0
		return
	case "end", "G":
		m.selectStep(m.doc.Len() - 1)
		m.scroll(len(m.body()))
		return
	case "q", "ctrl+c":
		m.leave()
		return
	case "?":
		m.screen = keyList
		return
	}
	if !m.mayChange(k) {
		return
	}
	switch k {
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
		m.saveFor(nil)
	case "c":
		m.askToApprove()
	case "r":
		m.openAsking()
	}
}

// changeKeys are the keys that change the plan or decide what happens to
// the run, and stepKeys the ones among them that act on a single step.
var (
	changeKeys = map[string]bool{
		"e": true, "enter": true, "a": true, "d": true, "K": true, "shift+up": true,
		"J": true, "shift+down": true, "u": true, "s": true, "c": true, "r": true,
	}
	stepKeys = map[string]bool{
		"e": true, "enter": true, "a": true, "d": true, "K": true, "shift+up": true, "J": true, "shift+down": true,
	}
)

// mayChange reports whether key k may act, and when it may not, says why:
// nothing can change in a read-only viewer, and the steps of a plan
// plandoc could not read cannot be edited one by one.
func (m *Model) mayChange(k string) bool {
	switch {
	case !changeKeys[k]:
		return false
	case m.opts.Mode == ReadOnly:
		m.message = m.opts.ReadOnlyReason
		return false
	case stepKeys[k] && m.stepsErr != nil:
		m.message = m.stepsRefusal()
		return false
	}
	return true
}

// stepsRefusal says why the steps of this plan cannot be edited here, and
// what can still be done with it.
func (m Model) stepsRefusal() string {
	if errors.Is(m.stepsErr, plandoc.ErrNotUTF8) {
		return "This plan is not valid UTF-8 text, so belay will not edit it here: a change would damage the rest of it. " +
			"Fix it in " + m.opts.Path + ", or ask for a new plan with r."
	}
	return "belay cannot tell this plan's steps apart, so it cannot edit them here: they are not a numbered list " +
		"under a \"## Steps\" heading. You can still approve the plan with c, or ask for a new one with r."
}

// moveDown moves the selection by delta steps, or, in a plan with no steps
// to choose between, scrolls the page by delta lines.
func (m *Model) moveDown(delta int) {
	if m.doc.Len() == 0 {
		m.scroll(delta)
		return
	}
	m.selectStep(m.selected + delta)
}

// scroll moves the page by n lines, leaving the selection where it is.
func (m *Model) scroll(n int) {
	m.offset = max(0, min(m.offset+n, len(m.body())-m.bodyHeight()))
}

// openAsking opens the box for what a new plan should do differently.
//
// The viewer asks nothing of the agent. It closes with the request, and
// the command layer records it on the run exactly as `belay plan
// --regenerate` does, so the new plan is written by the plan node inside
// the dispatcher: journalled, charged to the budget, and resumable.
func (m *Model) openAsking() {
	if m.opts.Blocker != "" {
		m.message = m.opts.Blocker
		return
	}
	m.screen = asking
	m.editor = m.newGuidanceBox()
	m.follow()
}

// ask answers a message while the new-plan box is open: enter asks, esc
// goes back, ctrl+c is leaving, and everything else is typing.
func (m Model) ask(msg tea.Msg) (Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "enter":
			m.submitAsking()
			return m, nil
		case "esc":
			m.closeEditor()
			return m, nil
		case "ctrl+c":
			m.closeEditor()
			m.leave()
			return m, nil
		}
	}
	if why := m.unreadable(msg); why != "" {
		m.message = why
		return m, nil
	}
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(msg)
	m.follow()
	return m, cmd
}

// submitAsking closes the viewer with a request for a new plan, saving any
// edits first: the plan being replaced is kept beside the new one and given
// to the agent to revise, and unsaved edits would be in neither.
func (m *Model) submitAsking() {
	guidance := strings.TrimSpace(m.editor.Value())
	if guidance == "" {
		m.message = "Say what the new plan should do differently; belay gives your words to the agent as you typed them."
		return
	}
	m.closeEditor()
	m.saveFor(&Decision{Action: ActionRegenerate, Guidance: guidance})
}

// askToApprove asks the person to confirm the approval, showing what the
// run has spent — or says why the plan cannot be approved from here.
//
// Whether the plan can be approved is the approve gate's judgement, and
// only the gate's: the viewer asks approve.CheckPlan, the rule the gate
// itself applies when the run continues, and repeats its reason. A second
// opinion here would sooner or later disagree with the gate, and a plan
// would be approvable through one path and not through another.
func (m *Model) askToApprove() {
	if m.opts.Blocker != "" {
		m.message = m.opts.Blocker
		return
	}
	if err := approve.CheckPlan(m.plan); err != nil {
		m.message = "belay cannot approve this plan: " + gateWords(err) +
			". Press r to ask for a new plan, or write one into " + m.opts.Path + "."
		return
	}
	m.screen = approving
}

// answerApproving answers the approval question: y saves any edits and
// approves, n and esc go back, and ctrl+c is leaving.
func (m *Model) answerApproving(key tea.KeyPressMsg) {
	switch key.String() {
	case "y":
		m.screen = reading
		m.saveFor(&Decision{Action: ActionApprove})
	case "n", "esc":
		m.screen = reading
	case "ctrl+c":
		m.screen = reading
		m.leave()
	}
}

// gateWords is the approve gate's reason for refusing a plan, without the
// package's name in front of it.
func gateWords(err error) string {
	return strings.TrimPrefix(err.Error(), "approve: ")
}

// edit answers a message while a step is open in the editor: ctrl+s keeps
// the edit, esc throws it away, ctrl+c is leaving, and everything else is
// typing.
func (m Model) edit(msg tea.Msg) (Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "ctrl+s":
			m.keepEdit()
			return m, nil
		case "esc":
			m.closeEditor()
			return m, nil
		case "ctrl+c":
			m.leave()
			return m, nil
		}
	}
	if why := m.unreadable(msg); why != "" {
		m.message = why
		return m, nil
	}
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(msg)
	m.follow()
	return m, cmd
}

// leave closes the viewer, deciding nothing, or asks first when there are
// edits nobody has saved.
func (m *Model) leave() {
	if !m.unsaved() {
		m.closing = true
		return
	}
	m.back = m.screen
	m.screen = leaving
}

// unsaved reports whether leaving now would lose anything: an edit made to
// the plan and not saved, or words typed into the editor and not yet kept.
func (m Model) unsaved() bool {
	typing := m.screen == editing && (m.editor.Value() != m.shown)
	return m.dirty() || typing
}

// answerLeaving answers the question asked before unsaved edits are left:
// y saves them first, n and a second ctrl+c leave them, and esc goes back.
func (m *Model) answerLeaving(key tea.KeyPressMsg) {
	switch key.String() {
	case "y":
		m.screen = m.back
		if m.screen == editing {
			if m.keepEdit(); m.screen == editing {
				return // refused; the reason is on the screen
			}
		}
		m.saveFor(&Decision{Action: ActionLeave})
	case "n", "ctrl+c":
		m.closing = true
	case "esc":
		m.screen = m.back
		m.follow()
	}
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
	if m.glyphs.ascii && !isASCII(steps[m.selected].Text) {
		// The editor draws the step's own bytes, which this terminal would
		// show as something else, and saves what it shows.
		m.message = fmt.Sprintf("This terminal cannot show every letter of step %d, so belay will not open it here. "+
			"Edit it in the plan file instead: %s", m.selected+1, m.opts.Path)
		return
	}
	m.screen = editing
	m.editAt = m.selected
	m.adding = false
	m.editor = m.newEditor(steps[m.selected].Text)
	m.shown = m.editor.Value()
	m.altered = m.shown != steps[m.selected].Text
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
	m.altered = false
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
	m.stepsErr = nil // only a document plandoc read is ever kept for undo
	m.plan = m.doc.Bytes()
	m.selectStep(m.selected)
	m.message = "Undone."
}

// dirty reports whether there are edits nobody has saved.
func (m Model) dirty() bool { return !bytes.Equal(m.plan, m.disk) }

// saveFor saves the plan when it has edits nobody saved, and then closes
// the viewer with decision, when there is one. A save that finds the file
// changed on disk asks the person what to do, and finishes the decision
// only if they choose to overwrite.
func (m *Model) saveFor(decision *Decision) {
	if !m.dirty() {
		if decision == nil {
			m.message = "There is nothing to save: the plan file already holds what you see."
		}
		m.finish(decision)
		return
	}
	if m.opts.Save == nil {
		m.message = "belay cannot save this plan from here."
		return
	}
	err := m.opts.Save(m.plan, false)
	switch {
	case errors.Is(err, ErrChangedOnDisk):
		m.pending = decision
		m.screen = conflict
		return
	case err != nil:
		m.message = "belay could not save the plan: " + err.Error()
		return
	}
	m.saved()
	m.finish(decision)
}

// saved records that the file now holds the plan.
func (m *Model) saved() {
	m.disk = m.plan
	m.message = "belay saved the plan to " + m.opts.Path + "."
}

// finish closes the viewer with decision, when there is one.
func (m *Model) finish(decision *Decision) {
	if decision == nil {
		return
	}
	m.decision = *decision
	m.closing = true
}

// answerConflict answers the question asked when a save found the plan
// file changed on disk: o overwrites it and carries on with what the save
// was for, r reads it again, and esc goes back with the edits unsaved.
func (m *Model) answerConflict(key tea.KeyPressMsg) {
	pending := m.pending
	switch key.String() {
	case "o":
		m.screen, m.pending = reading, nil
		if err := m.opts.Save(m.plan, true); err != nil {
			m.message = "belay could not save the plan: " + err.Error()
			return
		}
		m.saved()
		m.finish(pending)
	case "r":
		m.screen, m.pending = reading, nil
		m.reload()
	case "esc":
		m.screen, m.pending = reading, nil
	case "ctrl+c":
		m.screen, m.pending = reading, nil
		m.leave()
	}
}

// reload reads the plan file again, keeping the viewer's edits for undo so
// that reading the file is never the way they are lost.
func (m *Model) reload() {
	if m.opts.Reload == nil {
		m.message = "belay cannot read the plan file again from here."
		return
	}
	data, err := m.opts.Reload()
	if err != nil {
		m.message = "belay could not read the plan file again: " + err.Error()
		return
	}
	if m.stepsErr == nil {
		m.undo = append(m.undo, m.doc)
	}
	m.plan, m.disk = data, data
	m.doc, m.stepsErr = plandoc.Parse(data)
	m.selectStep(m.selected)
	m.message = "belay read the plan file again; u brings back your edits."
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

// isASCII reports whether s is nothing but ASCII.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// unreadable says why msg must not reach the editor, or returns "" when it
// may: text that did not arrive as UTF-8.
//
// The editor would not refuse it, it would lose it: it drops the
// replacement character a bad byte decodes to, so a key like é typed in a
// Latin-1 terminal, or a pasted "café" that is not UTF-8, would vanish or
// arrive without its letters, and nobody would be told. Such a key does not
// reach the editor as a key at all: bubbletea's decoder hands on an
// ultraviolet UnknownEvent holding the byte it could not read, which is
// told apart here from the unknown escape sequences it also reports that
// way.
func (m Model) unreadable(msg tea.Msg) string {
	locale := ""
	if m.opts.Locale != "" {
		locale = " This terminal's locale is " + m.opts.Locale + "."
	}
	switch msg := msg.(type) {
	case uv.UnknownEvent:
		if unreadableByte(string(msg)) {
			return "belay could not read that key as UTF-8 text, so it was not typed." + locale
		}
	case tea.KeyPressMsg:
		if !isText(msg.Text) {
			return "belay could not read that key as UTF-8 text, so it was not typed." + locale
		}
	case tea.PasteMsg:
		if !isText(msg.Content) {
			return "belay did not paste that: it is not valid UTF-8 text, and parts of it would have been lost." + locale
		}
	}
	return ""
}

// isText reports whether s is valid UTF-8 with no replacement characters.
func isText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, utf8.RuneError)
}

// unreadableByte reports whether an UnknownEvent holds the one byte of a
// key the decoder could not read as UTF-8: a byte from 0x80 up, which the
// decoder hands on either raw or as the rune of the same value.
func unreadableByte(s string) bool {
	if len(s) == 1 {
		return s[0] >= 0x80
	}
	r, size := utf8.DecodeRuneInString(s)
	return size == len(s) && r >= 0x80 && r <= 0xff
}

// answerKeys answers a key pressed on the list of keys: esc, ? and q go
// back to the plan, ctrl+c leaves, and nothing else does anything.
func (m *Model) answerKeys(key tea.KeyPressMsg) {
	switch key.String() {
	case "esc", "?", "q":
		m.screen = reading
	case "ctrl+c":
		m.screen = reading
		m.leave()
	}
}
