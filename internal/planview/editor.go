package planview

import (
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// maxStepLines bounds how long a step may grow while it is typed. It is far
// beyond any plan's step; it is set only so that the editor's visible
// height, which follows the window, is not also taken as a limit on how
// many lines a step may have, which is what the textarea does without it.
const maxStepLines = 5000

// newEditor returns the text box a step is edited in, holding value, sized
// for the plan's column of the screen.
//
// Three of the textarea's defaults are turned off, each for a reason:
//
//   - Its clipboard keys. ctrl+v runs pbpaste, xclip or wl-paste, and
//     ctrl+shift+c runs pbcopy or xclip (bubbles v2.2.1 textarea.go, Paste
//     and CopySelection, through github.com/atotto/clipboard). A plan viewer
//     has no business starting subprocesses; the terminal's own paste, which
//     arrives as bracketed paste, still works.
//   - Its drawn cursor. The terminal's real cursor is used instead, which
//     needs no colour or attribute to be seen, and is the one a screen
//     reader follows.
//   - Its colours. The editor is drawn plainly; emphasis in the viewer is
//     kept for the selected step.
func (m Model) newEditor(value string) textarea.Model {
	ed := textarea.New()
	ed.ShowLineNumbers = false
	ed.Prompt = m.glyphs.bar + " "
	ed.KeyMap.Paste.SetEnabled(false)
	ed.KeyMap.CopySelection.SetEnabled(false)
	ed.SetVirtualCursor(false)
	ed.SetStyles(plainEditorStyles())
	ed.DynamicHeight = true
	ed.MinHeight = 1
	ed.MaxContentHeight = maxStepLines
	m.sizeEditor(&ed)
	ed.SetValue(value)
	ed.Focus()
	return ed
}

// sizeEditor fits ed to the plan's column and to the lines the screen has.
func (m Model) sizeEditor(ed *textarea.Model) {
	ed.MaxHeight = max(1, m.bodyHeight()-1)
	ed.SetWidth(max(1, m.width-gutterWidth))
}

// plainEditorStyles are textarea styles with no colour, and no attribute but
// the one selected text needs.
func plainEditorStyles() textarea.Styles {
	plain := textarea.StyleState{
		Base:             lipgloss.NewStyle(),
		CursorLine:       lipgloss.NewStyle(),
		CursorLineNumber: lipgloss.NewStyle(),
		EndOfBuffer:      lipgloss.NewStyle(),
		LineNumber:       lipgloss.NewStyle(),
		Placeholder:      lipgloss.NewStyle(),
		Prompt:           lipgloss.NewStyle(),
		// Reverse video is an attribute, not a colour (no-color.org allows
		// it), and without it text selected with shift and an arrow key
		// would be invisible.
		Selection: lipgloss.NewStyle().Reverse(true),
		Text:      lipgloss.NewStyle(),
	}
	return textarea.Styles{
		Focused: plain,
		Blurred: plain,
		Cursor:  textarea.CursorStyle{Shape: tea.CursorBlock},
	}
}
