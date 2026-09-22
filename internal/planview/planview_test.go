package planview

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/dhaam-ai/belay/internal/plandoc"
)

// canonical is a plan in the shape internal/nodes/plan's prompt asks for.
const canonical = `## Summary

The CLI gains a --dry-run flag.

## Steps

1. Add a dryRun field to runOptions.
2. Bind --dry-run to it in newRunCmd.
3. Return from startRun before createRun when the flag is set.

## Tests

run_test.go covers the flag.
`

// press builds the key press a terminal would send for k, spelled the way
// bubbletea spells it: "j", "K", "?", "enter", "esc", "ctrl+s",
// "shift+up".
func press(k string) tea.KeyPressMsg {
	named := map[string]rune{
		"enter": tea.KeyEnter, "esc": tea.KeyEscape, "up": tea.KeyUp, "down": tea.KeyDown,
		"left": tea.KeyLeft, "right": tea.KeyRight, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown,
		"home": tea.KeyHome, "end": tea.KeyEnd, "backspace": tea.KeyBackspace, "tab": tea.KeyTab,
		"delete": tea.KeyDelete, "space": tea.KeySpace,
	}
	var mod tea.KeyMod
	parts := strings.Split(k, "+")
	for _, m := range parts[:len(parts)-1] {
		switch m {
		case "ctrl":
			mod |= tea.ModCtrl
		case "alt":
			mod |= tea.ModAlt
		case "shift":
			mod |= tea.ModShift
		}
	}
	last := parts[len(parts)-1]
	if code, ok := named[last]; ok {
		return tea.KeyPressMsg{Code: code, Mod: mod}
	}
	r, _ := utf8.DecodeRuneInString(last)
	if mod != 0 {
		return tea.KeyPressMsg{Code: r, Mod: mod}
	}
	lower := []rune(strings.ToLower(string(r)))[0]
	if lower != r {
		return tea.KeyPressMsg{Code: lower, ShiftedCode: r, Mod: tea.ModShift, Text: string(r)}
	}
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// send feeds msgs to m in order, as bubbletea would, and returns the model
// they leave behind.
func send(t *testing.T, m Model, msgs ...tea.Msg) Model {
	t.Helper()
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		var ok bool
		if m, ok = next.(Model); !ok {
			t.Fatalf("Update returned a %T, want a Model", next)
		}
	}
	return m
}

// keys presses each key in turn.
func keys(t *testing.T, m Model, ks ...string) Model {
	t.Helper()
	for _, k := range ks {
		m = send(t, m, press(k))
	}
	return m
}

// typeText types s one key at a time, with enter for a line break.
func typeText(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		if r == '\n' {
			m = send(t, m, press("enter"))
			continue
		}
		m = send(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return m
}

// saves records every call to Options.Save, and answers each with the next
// of errs, or nil once they run out.
type saves struct {
	plans      [][]byte
	overwrites []bool
	errs       []error
}

func (s *saves) save(plan []byte, overwrite bool) error {
	s.plans = append(s.plans, append([]byte(nil), plan...))
	s.overwrites = append(s.overwrites, overwrite)
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

// last is the plan most recently saved, or nil.
func (s *saves) last() []byte {
	if len(s.plans) == 0 {
		return nil
	}
	return s.plans[len(s.plans)-1]
}

// withSaves is review() with Save recorded in s.
func withSaves(s *saves) Options {
	opts := review()
	opts.Save = s.save
	return opts
}

// edited returns the canonical plan with step i's text replaced by text,
// exactly as plandoc writes it: what a save must hand over, byte for byte.
func edited(t *testing.T, i int, text string) []byte {
	t.Helper()
	doc, err := plandoc.Parse([]byte(canonical))
	if err != nil {
		t.Fatalf("parse the canonical plan: %v", err)
	}
	doc, err = doc.Edit(i, text)
	if err != nil {
		t.Fatalf("edit step %d: %v", i, err)
	}
	return doc.Bytes()
}

// open builds a viewer for opts on a terminal of width by height cells.
func open(t *testing.T, opts Options, width, height int) Model {
	t.Helper()
	return send(t, New(opts), tea.WindowSizeMsg{Width: width, Height: height})
}

// screenText is what the viewer draws, with the escape sequences that style
// it taken out.
func screenText(m Model) string { return ansi.Strip(m.View().Content) }

// selectedLine returns the line of the screen that carries the selection
// cursor, or "" when none does.
func selectedLine(m Model) string {
	for _, line := range strings.Split(screenText(m), "\n") {
		if strings.HasPrefix(line, "› ") {
			return line
		}
	}
	return ""
}

// review is the options every test starts from: the canonical plan, in the
// mode that lets a person change it.
func review() Options {
	return Options{
		RunID: "20260921T101500Z-1f2e3d4c5b6a",
		Goal:  "add a --dry-run flag",
		Path:  "/work/.belay/runs/20260921T101500Z-1f2e3d4c5b6a/artifacts/plan.md",
		Plan:  []byte(canonical),
		Mode:  Review,
	}
}

// TestViewShowsThePlanAndWhereItIs covers the first screen a person sees:
// which run this is, what they asked for, the plan itself, which step is
// selected, and the keys that do something. It is a full-screen view, so it
// takes the alternate screen and leaves the scrollback as it was.
func TestViewShowsThePlanAndWhereItIs(t *testing.T) {
	m := open(t, review(), 80, 24)
	got := screenText(m)
	for _, want := range []string{
		"20260921T101500Z-1f2e3d4c5b6a",
		"add a --dry-run flag",
		"## Summary",
		"1. Add a dryRun field to runOptions.",
		"3. Return from startRun before createRun when the flag is set.",
		"run_test.go covers the flag.",
		"e edit",
		"c approve",
		"q leave",
		"? all keys",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the viewer does not show %q:\n%s", want, got)
		}
	}
	if line := selectedLine(m); !strings.Contains(line, "1. Add a dryRun field") {
		t.Errorf("the selection is on %q, want the first step", line)
	}
	if !m.View().AltScreen {
		t.Error("the viewer draws over the scrollback instead of on the alternate screen")
	}
}

// TestSelectionMovesThroughTheSteps: j and k, and the arrow keys, choose a
// step, and stop at either end rather than wrapping round, which would move
// the cursor somewhere nobody was looking.
func TestSelectionMovesThroughTheSteps(t *testing.T) {
	tests := []struct {
		name string
		keys []string
		want string
	}{
		{name: "nothing pressed", keys: nil, want: "1. Add a dryRun field"},
		{name: "j", keys: []string{"j"}, want: "2. Bind --dry-run"},
		{name: "down twice", keys: []string{"down", "down"}, want: "3. Return from startRun"},
		{name: "past the last step", keys: []string{"j", "j", "j", "j"}, want: "3. Return from startRun"},
		{name: "back up", keys: []string{"j", "j", "k"}, want: "2. Bind --dry-run"},
		{name: "up from the first", keys: []string{"up"}, want: "1. Add a dryRun field"},
		{name: "end", keys: []string{"end"}, want: "3. Return from startRun"},
		{name: "home", keys: []string{"j", "j", "home"}, want: "1. Add a dryRun field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := keys(t, open(t, review(), 80, 24), tt.keys...)
			if line := selectedLine(m); !strings.Contains(line, tt.want) {
				t.Errorf("after %v the selection is on %q, want %q", tt.keys, line, tt.want)
			}
		})
	}
}

// TestEditingAStepChangesOnlyThatStep: e opens the selected step for
// editing, ctrl+s keeps the edit, and s saves. What is saved is plandoc's
// own rewrite of that one step -- every other byte of the document as it
// was -- because the approve gate fingerprints the file and a viewer that
// reformatted what it did not touch would report edits nobody made.
func TestEditingAStepChangesOnlyThatStep(t *testing.T) {
	var s saves
	m := keys(t, open(t, withSaves(&s), 80, 24), "j", "j", "e")
	m = typeText(t, m, " Say so in the output.")
	m = keys(t, m, "ctrl+s")

	if len(s.plans) != 0 {
		t.Fatal("keeping an edit saved the file; only s saves")
	}
	if got := screenText(m); !strings.Contains(got, "edited") {
		t.Errorf("an unsaved edit is not marked as edited:\n%s", got)
	}

	m = keys(t, m, "s")
	if len(s.plans) != 1 {
		t.Fatalf("s saved %d time(s), want once", len(s.plans))
	}
	want := edited(t, 2, "Return from startRun before createRun when the flag is set. Say so in the output.")
	if !bytes.Equal(s.last(), want) {
		t.Errorf("saved:\n%s\nwant:\n%s", s.last(), want)
	}
	if s.overwrites[0] {
		t.Error("an ordinary save asked to overwrite whatever is on disk")
	}
	got := screenText(m)
	if strings.Contains(got, "edited") {
		t.Errorf("the plan is still marked as edited after it was saved:\n%s", got)
	}
	if !strings.Contains(got, "saved") {
		t.Errorf("saving did not say it saved:\n%s", got)
	}
}

// TestEscapeThrowsAnEditAway: esc leaves the step as it was, and a plan
// nobody changed is not written at all.
func TestEscapeThrowsAnEditAway(t *testing.T) {
	var s saves
	m := keys(t, open(t, withSaves(&s), 80, 24), "e")
	m = typeText(t, m, " and more")
	m = keys(t, m, "esc", "s")
	if len(s.plans) != 0 {
		t.Errorf("a plan with no edits was saved: %q", s.last())
	}
	if line := selectedLine(m); !strings.HasSuffix(strings.TrimSpace(line), "1. Add a dryRun field to runOptions.") {
		t.Errorf("the step reads %q after esc, want it as it was", line)
	}
	if got := screenText(m); !strings.Contains(got, "nothing to save") {
		t.Errorf("s with nothing to save said nothing:\n%s", got)
	}
}

// TestKeepingAnUnchangedEditChangesNothing: opening a step and keeping it
// as it was is not an edit, so the plan is not marked edited.
func TestKeepingAnUnchangedEditChangesNothing(t *testing.T) {
	m := keys(t, open(t, review(), 80, 24), "e", "ctrl+s")
	if got := screenText(m); strings.Contains(got, "edited") {
		t.Errorf("a step kept exactly as it was marked the plan edited:\n%s", got)
	}
}

// TestUndoTakesBackTheLastChange: u undoes one change at a time, back to
// the plan as it was opened.
func TestUndoTakesBackTheLastChange(t *testing.T) {
	var s saves
	m := keys(t, open(t, withSaves(&s), 80, 24), "e")
	m = typeText(t, m, " One.")
	m = keys(t, m, "ctrl+s", "j", "e")
	m = typeText(t, m, " Two.")
	m = keys(t, m, "ctrl+s", "u", "s")
	if want := edited(t, 0, "Add a dryRun field to runOptions. One."); !bytes.Equal(s.last(), want) {
		t.Errorf("after one undo, saved:\n%s\nwant:\n%s", s.last(), want)
	}
	m = keys(t, m, "u", "s")
	if !bytes.Equal(s.last(), []byte(canonical)) {
		t.Errorf("after two undos, saved:\n%s\nwant the plan as it was opened", s.last())
	}
	m = keys(t, m, "u")
	if got := screenText(m); !strings.Contains(got, "nothing to undo") {
		t.Errorf("u with nothing left to undo said nothing:\n%s", got)
	}
}

// TestAnEmptyStepIsRefusedInWords: a step cannot be emptied, and the
// person is told so in a sentence, with the editor still open so they can
// type something or leave it with esc.
func TestAnEmptyStepIsRefusedInWords(t *testing.T) {
	m := keys(t, open(t, review(), 80, 24), "e")
	for range len("Add a dryRun field to runOptions.") {
		m = keys(t, m, "backspace")
	}
	m = keys(t, m, "ctrl+s")
	got := screenText(m)
	if !strings.Contains(got, "cannot be empty") {
		t.Errorf("emptying a step was not refused in words:\n%s", got)
	}
	if strings.Contains(got, "plandoc:") {
		t.Errorf("the refusal shows plandoc's error text rather than a sentence for a person:\n%s", got)
	}
	m = keys(t, m, "esc")
	if line := selectedLine(m); !strings.Contains(line, "1. Add a dryRun field to runOptions.") {
		t.Errorf("after esc the step reads %q, want it as it was", line)
	}
}

// TestTheEditorUsesTheTerminalsOwnCursor: while a step is open the
// terminal's cursor sits at the end of the text being typed, on the line
// the editor is drawn on, and while the plan is only being read there is no
// cursor at all. The real cursor needs no colour to be seen, and it is the
// one a screen reader follows.
func TestTheEditorUsesTheTerminalsOwnCursor(t *testing.T) {
	m := open(t, review(), 80, 24)
	if c := m.View().Cursor; c != nil {
		t.Errorf("a cursor is shown at %d,%d while nothing is being typed", c.X, c.Y)
	}
	m = keys(t, m, "e")
	c := m.View().Cursor
	if c == nil {
		t.Fatal("no cursor is shown while a step is being edited")
	}
	rows := strings.Split(screenText(m), "\n")
	if c.Y < 0 || c.Y >= len(rows) {
		t.Fatalf("the cursor is at row %d, off a screen of %d rows", c.Y, len(rows))
	}
	text := "Add a dryRun field to runOptions."
	row := rows[c.Y]
	if !strings.Contains(row, text) {
		t.Fatalf("the cursor is on row %d, %q, not on the line being edited", c.Y, row)
	}
	if want := strings.Index(row, text) + len(text); c.X != utf8.RuneCountInString(row[:want]) {
		t.Errorf("the cursor is at column %d, want %d, the end of the text", c.X, utf8.RuneCountInString(row[:want]))
	}
}

// parsed returns the canonical plan as plandoc reads it.
func parsed(t *testing.T) plandoc.Document {
	t.Helper()
	doc, err := plandoc.Parse([]byte(canonical))
	if err != nil {
		t.Fatalf("parse the canonical plan: %v", err)
	}
	return doc
}

// TestAddingAStepPutsItAfterTheSelection: a opens an empty editor below the
// selected step, and ctrl+s adds what was typed there as the next step,
// renumbering the ones after it the way plandoc does. The new step is then
// the selected one.
func TestAddingAStepPutsItAfterTheSelection(t *testing.T) {
	var s saves
	m := keys(t, open(t, withSaves(&s), 80, 24), "a")
	m = typeText(t, m, "Write the test first.")
	m = keys(t, m, "ctrl+s")
	if line := selectedLine(m); !strings.Contains(line, "2. Write the test first.") {
		t.Errorf("after adding, the selection is on %q, want the new step 2", line)
	}
	keys(t, m, "s")
	want, err := parsed(t).Insert(1, "Write the test first.")
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !bytes.Equal(s.last(), want.Bytes()) {
		t.Errorf("saved:\n%s\nwant:\n%s", s.last(), want.Bytes())
	}

	// esc adds nothing, and an empty new step is refused like an emptied one.
	var none saves
	m = keys(t, open(t, withSaves(&none), 80, 24), "a", "esc", "s")
	if len(none.plans) != 0 {
		t.Errorf("a step abandoned with esc was saved: %q", none.last())
	}
	m = keys(t, m, "a", "ctrl+s")
	if got := screenText(m); !strings.Contains(got, "cannot be empty") {
		t.Errorf("an empty new step was not refused in words:\n%s", got)
	}
}

// TestDeletingAStepCanBeUndone: d deletes the selected step at once — u
// puts it back, which is why it does not ask first — and says so.
func TestDeletingAStepCanBeUndone(t *testing.T) {
	var s saves
	m := keys(t, open(t, withSaves(&s), 80, 24), "j", "d")
	if got := screenText(m); !strings.Contains(got, "u puts it back") {
		t.Errorf("deleting a step did not say how to undo it:\n%s", got)
	}
	m = keys(t, m, "s")
	want, err := parsed(t).Delete(1)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !bytes.Equal(s.last(), want.Bytes()) {
		t.Errorf("saved:\n%s\nwant:\n%s", s.last(), want.Bytes())
	}
	m = keys(t, m, "u", "s")
	if !bytes.Equal(s.last(), []byte(canonical)) {
		t.Errorf("after undoing the delete, saved:\n%s\nwant the plan as it was", s.last())
	}
	if line := selectedLine(m); !strings.Contains(line, "2. Bind --dry-run") {
		t.Errorf("after undoing, the selection is on %q, want the step that came back", line)
	}
}

// TestMovingAStep: K and shift+up move the selected step up, J and
// shift+down move it down, and the selection travels with it.
func TestMovingAStep(t *testing.T) {
	doc := parsed(t)
	up, err := doc.Move(1, 0)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	down, err := doc.Move(1, 2)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	tests := []struct {
		name     string
		keys     []string
		want     []byte
		selected string
	}{
		{name: "K", keys: []string{"j", "K"}, want: up.Bytes(), selected: "1. Bind --dry-run"},
		{name: "shift+up", keys: []string{"j", "shift+up"}, want: up.Bytes(), selected: "1. Bind --dry-run"},
		{name: "J", keys: []string{"j", "J"}, want: down.Bytes(), selected: "3. Bind --dry-run"},
		{name: "shift+down", keys: []string{"j", "shift+down"}, want: down.Bytes(), selected: "3. Bind --dry-run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s saves
			m := keys(t, open(t, withSaves(&s), 80, 24), tt.keys...)
			if line := selectedLine(m); !strings.Contains(line, tt.selected) {
				t.Errorf("the selection is on %q, want %q", line, tt.selected)
			}
			keys(t, m, "s")
			if !bytes.Equal(s.last(), tt.want) {
				t.Errorf("saved:\n%s\nwant:\n%s", s.last(), tt.want)
			}
		})
	}

	// Moving the first step up, or the last down, changes nothing.
	var s saves
	m := keys(t, open(t, withSaves(&s), 80, 24), "K", "end", "J", "s")
	if len(s.plans) != 0 {
		t.Errorf("moving a step past the end of the list changed the plan: %q", s.last())
	}
	if got := screenText(m); !strings.Contains(got, "nothing to save") {
		t.Errorf("want the plan unchanged:\n%s", got)
	}
}

// TestPlandocsRefusalsAreSaidInWords: the two refusals a structural edit
// can meet — deleting a plan's only step, and an edit that would move text
// from one step into another — are said as sentences a person can act on,
// never as plandoc's error text, and the plan is left as it was.
func TestPlandocsRefusalsAreSaidInWords(t *testing.T) {
	one := review()
	one.Plan = []byte("## Steps\n\n1. The only step.\n")
	got := screenText(keys(t, open(t, one, 80, 24), "d"))
	if !strings.Contains(got, "at least one step") || strings.Contains(got, "plandoc:") {
		t.Errorf("deleting the only step was not refused in words:\n%s", got)
	}

	var b strings.Builder
	b.WriteString("## Steps\n\n")
	for n := 1; n <= 10; n++ {
		fmt.Fprintf(&b, "%d. Step %d.\n", n, n)
	}
	b.WriteString("   A line that belongs to no step.\n")
	ten := review()
	ten.Plan = []byte(b.String())
	ten.Path = "/work/plan.md"
	var s saves
	ten.Save = s.save
	m := keys(t, open(t, ten, 80, 24), "d")
	got = strings.Join(strings.Fields(screenText(m)), " ")
	if !strings.Contains(got, "text from one step into another") || !strings.Contains(got, "/work/plan.md") {
		t.Errorf("an ambiguous edit was not refused in words naming the file:\n%s", got)
	}
	keys(t, m, "s")
	if len(s.plans) != 0 {
		t.Errorf("a refused edit changed the plan: %q", s.last())
	}
}

// quits presses k and reports whether the viewer asked bubbletea to close.
func quits(t *testing.T, m Model, k string) (Model, bool) {
	t.Helper()
	next, cmd := m.Update(press(k))
	model, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned a %T, want a Model", next)
	}
	if cmd == nil {
		return model, false
	}
	_, quit := cmd().(tea.QuitMsg)
	return model, quit
}

// dirtyViewer is a viewer with one unsaved edit, saving into s.
func dirtyViewer(t *testing.T, s *saves) Model {
	t.Helper()
	m := keys(t, open(t, withSaves(s), 80, 24), "e")
	m = typeText(t, m, " Now.")
	return keys(t, m, "ctrl+s")
}

// TestLeavingNeverLosesEditsSilently: q and ctrl+c leave at once when
// there is nothing unsaved. With edits nobody saved they ask first — save
// and leave, leave without saving, or stay — and a second ctrl+c at that
// question leaves without saving, the way a second Ctrl-C always does in
// belay. Leaving decides nothing: the run stays paused at its plan.
func TestLeavingNeverLosesEditsSilently(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c"} {
		m, quit := quits(t, open(t, review(), 80, 24), k)
		if !quit {
			t.Errorf("%s with nothing unsaved did not leave", k)
		}
		if m.Decision() != (Decision{Action: ActionLeave}) {
			t.Errorf("%s decided %+v, want to leave the run as it is", k, m.Decision())
		}
	}

	tests := []struct {
		name      string
		keys      []string // after the key that asks
		wantQuit  bool
		wantSaves int
	}{
		{name: "y saves and leaves", keys: []string{"y"}, wantQuit: true, wantSaves: 1},
		{name: "n leaves without saving", keys: []string{"n"}, wantQuit: true, wantSaves: 0},
		{name: "esc stays", keys: []string{"esc"}, wantQuit: false, wantSaves: 0},
		{name: "a second ctrl+c leaves without saving", keys: []string{"ctrl+c"}, wantQuit: true, wantSaves: 0},
	}
	for _, ask := range []string{"q", "ctrl+c"} {
		for _, tt := range tests {
			t.Run(ask+" then "+tt.name, func(t *testing.T) {
				var s saves
				m, quit := quits(t, dirtyViewer(t, &s), ask)
				if quit {
					t.Fatalf("%s left without asking about unsaved edits", ask)
				}
				got := strings.Join(strings.Fields(screenText(m)), " ")
				for _, want := range []string{"not saved", "y save", "n leave without saving", "esc stay"} {
					if !strings.Contains(got, want) {
						t.Errorf("the question does not offer %q:\n%s", want, screenText(m))
					}
				}
				for _, k := range tt.keys {
					m, quit = quits(t, m, k)
				}
				if quit != tt.wantQuit {
					t.Errorf("left = %v, want %v", quit, tt.wantQuit)
				}
				if len(s.plans) != tt.wantSaves {
					t.Errorf("saved %d time(s), want %d", len(s.plans), tt.wantSaves)
				}
				if m.Decision() != (Decision{Action: ActionLeave}) {
					t.Errorf("leaving decided %+v, want to leave the run as it is", m.Decision())
				}
				if !tt.wantQuit && !strings.Contains(screenText(m), "edited") {
					t.Errorf("after staying, the edits are no longer marked:\n%s", screenText(m))
				}
			})
		}
	}
}

// TestCtrlCWhileTypingAsksToo: ctrl+c in the editor is leaving, not a
// character, and the words typed but not yet kept count as unsaved. esc at
// the question goes back to the editor with them intact; y keeps them and
// saves them.
func TestCtrlCWhileTypingAsksToo(t *testing.T) {
	var s saves
	m := keys(t, open(t, withSaves(&s), 80, 24), "e")
	m = typeText(t, m, " Typed.")
	m, quit := quits(t, m, "ctrl+c")
	if quit {
		t.Fatal("ctrl+c while typing left without asking")
	}
	if !strings.Contains(screenText(m), "Typed.") {
		t.Errorf("while the question is asked, the words it is asking about are not on the screen:\n%s", screenText(m))
	}
	m = keys(t, m, "esc")
	if !strings.Contains(screenText(m), "Typed.") {
		t.Fatalf("after esc the typing is gone:\n%s", screenText(m))
	}
	m, _ = quits(t, m, "ctrl+c")
	if _, quit = quits(t, m, "y"); !quit {
		t.Fatal("y did not leave")
	}
	want := edited(t, 0, "Add a dryRun field to runOptions. Typed.")
	if !bytes.Equal(s.last(), want) {
		t.Errorf("saved:\n%s\nwant the typing kept and saved:\n%s", s.last(), want)
	}
}

// flat is the screen's text with every run of whitespace made one space,
// so a sentence the page wrapped still reads as one.
func flat(m Model) string { return strings.Join(strings.Fields(screenText(m)), " ") }

// TestApprovingShowsWhatItMaySpendFirst: c does not approve; it asks, and
// the question says what the run has spent against its ceiling before the
// person commits it to spending more. y approves, n goes back.
func TestApprovingShowsWhatItMaySpendFirst(t *testing.T) {
	opts := review()
	opts.Spend = Spend{SpentUSD: 0.42, LimitUSD: 5}
	m, quit := quits(t, open(t, opts, 80, 24), "c")
	if quit {
		t.Fatal("c approved the plan on one key, without showing the cost")
	}
	for _, want := range []string{"Approve this plan", "$0.42 of $5.00", "y approve", "n go back"} {
		if !strings.Contains(flat(m), want) {
			t.Errorf("the question does not say %q:\n%s", want, screenText(m))
		}
	}
	if strings.Contains(flat(m), "estimated") {
		t.Errorf("a reported spend is called an estimate:\n%s", screenText(m))
	}
	approved, quit := quits(t, m, "y")
	if !quit || approved.Decision() != (Decision{Action: ActionApprove}) {
		t.Errorf("y: left = %v, decided %+v; want to leave having approved", quit, approved.Decision())
	}

	back, quit := quits(t, m, "n")
	if quit || back.Decision() != (Decision{Action: ActionLeave}) {
		t.Errorf("n: left = %v, decided %+v; want to go back having decided nothing", quit, back.Decision())
	}
	if !strings.Contains(flat(back), "c approve") {
		t.Errorf("n did not go back to the plan:\n%s", screenText(back))
	}

	opts.Spend.Estimated = true
	m = keys(t, open(t, opts, 80, 24), "c")
	if !strings.Contains(flat(m), "estimated") {
		t.Errorf("an estimated spend is not marked as one:\n%s", screenText(m))
	}
}

// TestApprovingSavesEditsFirst: the gate reads the plan file, so approving
// with edits nobody saved would approve the plan without them. They are
// saved first, and the question says they will be.
func TestApprovingSavesEditsFirst(t *testing.T) {
	var s saves
	m := keys(t, dirtyViewer(t, &s), "c")
	if !strings.Contains(flat(m), "saved first") {
		t.Errorf("the question does not say the edits are saved first:\n%s", screenText(m))
	}
	m, quit := quits(t, m, "y")
	if !quit || m.Decision().Action != ActionApprove {
		t.Fatalf("y: left = %v, decided %+v; want to approve", quit, m.Decision())
	}
	if want := edited(t, 0, "Add a dryRun field to runOptions. Now."); !bytes.Equal(s.last(), want) {
		t.Errorf("saved before approving:\n%s\nwant:\n%s", s.last(), want)
	}

	// A save that fails approves nothing.
	failing := saves{errs: []error{errors.New("disk full")}}
	m = keys(t, dirtyViewer(t, &failing), "c")
	m, quit = quits(t, m, "y")
	if quit || m.Decision().Action == ActionApprove {
		t.Errorf("a plan that could not be saved was approved (left = %v, %+v)", quit, m.Decision())
	}
	if !strings.Contains(flat(m), "disk full") {
		t.Errorf("the failed save is not explained:\n%s", screenText(m))
	}
}

// TestAPlanTheGateRefusesCannotBeApproved: the approve gate is the one
// judge of whether a plan can be approved, and the viewer asks it rather
// than forming an opinion of its own. A plan it refuses cannot be approved
// here, and the viewer says why in the gate's words.
func TestAPlanTheGateRefusesCannotBeApproved(t *testing.T) {
	tests := []struct {
		name string
		plan []byte
		why  string
	}{
		{name: "empty", plan: []byte(" \n\t\n"), why: "the plan is empty"},
		{name: "not UTF-8", plan: []byte("## Steps\n\n1. Caf\xe9.\n"), why: "not valid UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := review()
			opts.Plan = tt.plan
			m, quit := quits(t, open(t, opts, 80, 24), "c")
			if quit || m.Decision().Action == ActionApprove {
				t.Fatalf("a plan the gate refuses was approved (left = %v)", quit)
			}
			if !strings.Contains(flat(m), tt.why) {
				t.Errorf("the refusal does not give the gate's reason %q:\n%s", tt.why, screenText(m))
			}
			if m, quit = quits(t, m, "y"); quit || m.Decision().Action == ActionApprove {
				t.Error("y after the refusal approved the plan anyway")
			}
		})
	}
}

// TestABlockerStopsApprovingAndSaysWhy: when the command layer knows the
// run cannot continue from here — the agent it needs is not installed, say
// — approving is refused with that reason, instead of approving and then
// failing to start.
func TestABlockerStopsApprovingAndSaysWhy(t *testing.T) {
	opts := review()
	opts.Blocker = "belay cannot find the claude command line on PATH."
	m, quit := quits(t, open(t, opts, 80, 24), "c")
	if quit || m.Decision().Action == ActionApprove {
		t.Fatalf("approved a run that cannot continue (left = %v)", quit)
	}
	if !strings.Contains(flat(m), "cannot find the claude command line") {
		t.Errorf("the blocker is not said:\n%s", screenText(m))
	}
}

// TestAskingForANewPlan: r opens a box for what the new plan should do
// differently, above the spend so far, and enter asks for it. Nothing is
// asked of the agent here: the viewer closes with the request, and the
// command layer records it on the run the same way `belay plan
// --regenerate` does.
func TestAskingForANewPlan(t *testing.T) {
	opts := review()
	opts.Spend = Spend{SpentUSD: 0.42, LimitUSD: 5}
	m, quit := quits(t, open(t, opts, 80, 24), "r")
	if quit {
		t.Fatal("r asked for a new plan without asking what it should be")
	}
	for _, want := range []string{"what the new plan should do differently", "$0.42 of $5.00", "enter asks for it", "esc goes back"} {
		if !strings.Contains(flat(m), want) {
			t.Errorf("the box does not say %q:\n%s", want, screenText(m))
		}
	}
	m = typeText(t, m, "  fewer steps, and leave the parser alone  ")
	m, quit = quits(t, m, "enter")
	want := Decision{Action: ActionRegenerate, Guidance: "fewer steps, and leave the parser alone"}
	if !quit || m.Decision() != want {
		t.Errorf("enter: left = %v, decided %+v; want to leave with %+v", quit, m.Decision(), want)
	}

	// An empty request is not a request, and esc goes back.
	m, quit = quits(t, keys(t, open(t, opts, 80, 24), "r"), "enter")
	if quit || m.Decision().Action == ActionRegenerate {
		t.Errorf("an empty request was sent (left = %v, %+v)", quit, m.Decision())
	}
	if !strings.Contains(flat(m), "Say what") {
		t.Errorf("an empty request is not explained:\n%s", screenText(m))
	}
	m = keys(t, m, "esc")
	if m.Decision().Action != ActionLeave || !strings.Contains(flat(m), "c approve") {
		t.Errorf("esc did not go back to the plan:\n%s", screenText(m))
	}
}

// TestAskingForANewPlanSavesEditsFirst: the plan being replaced is kept
// beside the new one, and handed to the agent to revise, so edits nobody
// saved are saved first, or they would be in neither.
func TestAskingForANewPlanSavesEditsFirst(t *testing.T) {
	var s saves
	m := keys(t, dirtyViewer(t, &s), "r")
	if !strings.Contains(flat(m), "saved first") {
		t.Errorf("the box does not say the edits are saved first:\n%s", screenText(m))
	}
	m = typeText(t, m, "shorter")
	m, quit := quits(t, m, "enter")
	if !quit || m.Decision().Action != ActionRegenerate {
		t.Fatalf("enter: left = %v, decided %+v", quit, m.Decision())
	}
	if want := edited(t, 0, "Add a dryRun field to runOptions. Now."); !bytes.Equal(s.last(), want) {
		t.Errorf("saved before asking:\n%s\nwant:\n%s", s.last(), want)
	}
}

// TestABlockerStopsAskingForANewPlan: a new plan is a step of the run like
// any other, so a run that cannot continue cannot be asked for one.
func TestABlockerStopsAskingForANewPlan(t *testing.T) {
	opts := review()
	opts.Blocker = "belay cannot find the claude command line on PATH."
	m, quit := quits(t, open(t, opts, 80, 24), "r")
	if quit || strings.Contains(flat(m), "enter asks for it") {
		t.Fatalf("a run that cannot continue was offered a new plan (left = %v):\n%s", quit, screenText(m))
	}
	if !strings.Contains(flat(m), "cannot find the claude command line") {
		t.Errorf("the blocker is not said:\n%s", screenText(m))
	}
}

// TestTheNewPlanBoxUsesTheTerminalsOwnCursor: the cursor follows the words
// typed into the new-plan box at the foot of the page.
func TestTheNewPlanBoxUsesTheTerminalsOwnCursor(t *testing.T) {
	m := typeText(t, keys(t, open(t, review(), 80, 24), "r"), "fewer steps")
	c := m.View().Cursor
	if c == nil {
		t.Fatal("no cursor is shown in the new-plan box")
	}
	rows := strings.Split(screenText(m), "\n")
	if c.Y < 0 || c.Y >= len(rows) || !strings.Contains(rows[c.Y], "fewer steps") {
		t.Fatalf("the cursor is on row %d, not on the box holding the words typed", c.Y)
	}
	row := rows[c.Y]
	if want := utf8.RuneCountInString(row[:strings.Index(row, "fewer steps")+len("fewer steps")]); c.X != want {
		t.Errorf("the cursor is at column %d, want %d, the end of the words", c.X, want)
	}
}

// changedOnDisk is a plan somebody saved into the file from their editor
// while the viewer had it open.
const changedOnDisk = "## Steps\n\n1. What the person wrote in their editor.\n"

// conflicted returns a viewer with one unsaved edit whose first save finds
// the file changed on disk, and reloads changedOnDisk.
func conflicted(t *testing.T, s *saves) Model {
	t.Helper()
	s.errs = []error{ErrChangedOnDisk}
	opts := withSaves(s)
	opts.Reload = func() ([]byte, error) { return []byte(changedOnDisk), nil }
	m := keys(t, open(t, opts, 80, 24), "e")
	m = typeText(t, m, " Now.")
	return keys(t, m, "ctrl+s")
}

// TestASaveThatFindsTheFileChangedAsksWhatToDo: the person may have edited
// plan.md in their own editor while the viewer was open. Saving over that
// silently would throw their other edit away, so the viewer says what
// happened and offers the two ways out: read the file again (u then brings
// the viewer's edits back), or overwrite it with what is on the screen.
func TestASaveThatFindsTheFileChangedAsksWhatToDo(t *testing.T) {
	var s saves
	m := keys(t, conflicted(t, &s), "s")
	for _, want := range []string{"changed", "since belay opened it", "r read it again", "o overwrite", "esc go back"} {
		if !strings.Contains(flat(m), want) {
			t.Errorf("the conflict does not say %q:\n%s", want, screenText(m))
		}
	}

	overwritten := keys(t, m, "o")
	if len(s.overwrites) != 2 || !s.overwrites[1] {
		t.Fatalf("o did not save with overwrite (overwrites: %v)", s.overwrites)
	}
	if strings.Contains(flat(overwritten), "edited") {
		t.Errorf("after overwriting, the plan is still marked edited:\n%s", screenText(overwritten))
	}

	reloaded := keys(t, m, "r")
	if !strings.Contains(flat(reloaded), "What the person wrote in their editor.") {
		t.Errorf("r did not show the file as it is now:\n%s", screenText(reloaded))
	}
	if strings.Contains(flat(reloaded), "edited") {
		t.Errorf("the plan just read from disk is marked edited:\n%s", screenText(reloaded))
	}
	if !strings.Contains(flat(reloaded), "u brings back your edits") {
		t.Errorf("r did not say how to get the edits back:\n%s", screenText(reloaded))
	}
	undone := keys(t, reloaded, "u")
	if !strings.Contains(flat(undone), "Add a dryRun field to runOptions. Now.") || !strings.Contains(flat(undone), "edited") {
		t.Errorf("u after reading the file again did not bring the edits back:\n%s", screenText(undone))
	}

	before := len(s.plans)
	stayed := keys(t, m, "esc")
	if !strings.Contains(flat(stayed), "edited") || len(s.plans) != before {
		t.Errorf("esc changed something (saves: %d, then %d):\n%s", before, len(s.plans), screenText(stayed))
	}
}

// TestAConflictFinishesWhatTheSaveWasFor: a save made on the way to
// approving, leaving or asking for a new plan carries on with it once the
// person overwrites; reading the file again stops, so they can look at the
// plan they are now approving.
func TestAConflictFinishesWhatTheSaveWasFor(t *testing.T) {
	tests := []struct {
		name  string
		start func(t *testing.T, m Model) Model
		want  Decision
	}{
		{
			name:  "approving",
			start: func(t *testing.T, m Model) Model { return keys(t, m, "c", "y") },
			want:  Decision{Action: ActionApprove},
		},
		{
			name:  "leaving",
			start: func(t *testing.T, m Model) Model { return keys(t, m, "q", "y") },
			want:  Decision{Action: ActionLeave},
		},
		{
			name: "asking for a new plan",
			start: func(t *testing.T, m Model) Model {
				return keys(t, typeText(t, keys(t, m, "r"), "shorter"), "enter")
			},
			want: Decision{Action: ActionRegenerate, Guidance: "shorter"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s saves
			m := tt.start(t, conflicted(t, &s))
			if !strings.Contains(flat(m), "o overwrite") {
				t.Fatalf("the conflict was not reported:\n%s", screenText(m))
			}
			done, quit := quits(t, m, "o")
			if !quit || done.Decision() != tt.want {
				t.Errorf("o: left = %v, decided %+v; want to leave with %+v", quit, done.Decision(), tt.want)
			}
			stopped, quit := quits(t, m, "r")
			if quit || stopped.Decision() != (Decision{Action: ActionLeave}) {
				t.Errorf("r: left = %v, decided %+v; want to stay and decide nothing", quit, stopped.Decision())
			}
		})
	}
}

// headings is a plan whose steps are sub-headings rather than a numbered
// list: a perfectly good plan that plandoc will not guess the steps of.
const headings = `## Summary

Two steps, written as headings.

## Steps

### Step 1

Add the flag.

### Step 2

Test it.
`

// TestReadOnlyShowsThePlanAndSaysWhyNothingCanChange: a plan the person
// can read but not change — a run that is not waiting at its plan — says
// why on the page, answers every key that would change something with
// that reason, and never saves or decides anything.
func TestReadOnlyShowsThePlanAndSaysWhyNothingCanChange(t *testing.T) {
	var s saves
	opts := withSaves(&s)
	opts.Mode = ReadOnly
	opts.ReadOnlyReason = "Run 20260921T101500Z-1f2e3d4c5b6a is running its tests, so its plan can be read here but not changed."
	m := open(t, opts, 80, 24)
	if !strings.Contains(flat(m), "can be read here but not changed") {
		t.Errorf("the page does not say why the plan cannot be changed:\n%s", screenText(m))
	}
	if strings.Contains(flat(m), "e edit") || strings.Contains(flat(m), "c approve") {
		t.Errorf("a read-only page offers keys that change things:\n%s", screenText(m))
	}
	for _, k := range []string{"e", "a", "d", "K", "J", "u", "s", "c", "r"} {
		after, quit := quits(t, m, k)
		if quit || after.Decision() != (Decision{Action: ActionLeave}) {
			t.Errorf("%s in a read-only viewer: left = %v, decided %+v", k, quit, after.Decision())
		}
		if !strings.Contains(flat(after), "can be read here but not changed") {
			t.Errorf("%s did not say why:\n%s", k, screenText(after))
		}
		if strings.Contains(flat(after), "Editing step") || strings.Contains(flat(after), "enter asks for it") {
			t.Errorf("%s opened something in a read-only viewer:\n%s", k, screenText(after))
		}
	}
	if len(s.plans) != 0 {
		t.Errorf("a read-only viewer saved the plan %d time(s)", len(s.plans))
	}
	if _, quit := quits(t, m, "q"); !quit {
		t.Error("q did not leave a read-only viewer")
	}
}

// TestAPlanWithoutAStepListCanStillBeApproved: plandoc refuses to guess
// the steps of a plan written without a numbered list, so the step keys
// explain that instead of editing — but the plan is still a plan: it can be
// read, approved, and replaced with a new one.
func TestAPlanWithoutAStepListCanStillBeApproved(t *testing.T) {
	opts := review()
	opts.Plan = []byte(headings)
	m := open(t, opts, 80, 24)
	if !strings.Contains(flat(m), "### Step 2") || !strings.Contains(flat(m), "Test it.") {
		t.Errorf("the plan is not shown as it is:\n%s", screenText(m))
	}
	for _, k := range []string{"e", "a", "d", "K", "J"} {
		after := keys(t, m, k)
		if !strings.Contains(flat(after), "numbered list") || strings.Contains(flat(after), "plandoc:") {
			t.Errorf("%s did not explain in words why the steps cannot be edited:\n%s", k, screenText(after))
		}
	}
	approved, quit := quits(t, keys(t, m, "c"), "y")
	if !quit || approved.Decision().Action != ActionApprove {
		t.Errorf("a plan without a step list could not be approved (left = %v, %+v)", quit, approved.Decision())
	}
	asked, quit := quits(t, typeText(t, keys(t, m, "r"), "use a numbered list"), "enter")
	if !quit || asked.Decision().Action != ActionRegenerate {
		t.Errorf("a plan without a step list could not be replaced (left = %v, %+v)", quit, asked.Decision())
	}

	// A plan that is not UTF-8 is not edited either, and says why.
	opts.Plan = []byte("## Steps\n\n1. Caf\xe9.\n")
	if got := flat(keys(t, open(t, opts, 80, 24), "e")); !strings.Contains(got, "not valid UTF-8") {
		t.Errorf("editing a plan that is not UTF-8 was not refused in words:\n%s", got)
	}
}

// TestAPlanWithNoStepsScrollsLineByLine: with no steps to choose between,
// j and k scroll the page instead, so a long plan can be read to its end.
func TestAPlanWithNoStepsScrollsLineByLine(t *testing.T) {
	var b strings.Builder
	b.WriteString("## Summary\n\n")
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "Line %d of a plan written without a numbered list.\n", i)
	}
	opts := review()
	opts.Plan = []byte(b.String())
	m := open(t, opts, 80, 16)
	if strings.Contains(flat(m), "Line 40 ") {
		t.Fatalf("the whole plan fits, so this test proves nothing:\n%s", screenText(m))
	}
	for range 40 {
		m = keys(t, m, "j")
	}
	if !strings.Contains(flat(m), "Line 40 ") {
		t.Errorf("j never scrolled to the end of the plan:\n%s", screenText(m))
	}
	m = keys(t, m, "home")
	if !strings.Contains(flat(m), "## Summary") {
		t.Errorf("home did not go back to the top:\n%s", screenText(m))
	}
}

// TestTextThatIsNotUTF8IsRefusedOutLoud: in a terminal whose locale is not
// UTF-8, a key like é arrives as a byte the input decoder cannot read, and
// the editor would drop it without a word; pasted text that is not UTF-8
// would lose its letters the same way. The viewer takes neither, and says
// why, naming the locale, so the person is not left wondering where their
// typing went.
func TestTextThatIsNotUTF8IsRefusedOutLoud(t *testing.T) {
	opts := review()
	opts.Locale = "LANG=en_US.ISO8859-1"
	tests := []struct {
		name string
		msg  tea.Msg
		want string
	}{
		// What the decoder hands on for byte 0xE9 (é in Latin-1).
		{name: "a key the decoder could not read", msg: uv.UnknownEvent(byte(0xe9)), want: "LANG=en_US.ISO8859-1"},
		{name: "a key that arrived as a replacement character", msg: tea.KeyPressMsg{Code: utf8.RuneError, Text: "\uFFFD"}, want: "UTF-8"},
		{name: "a paste that is not UTF-8", msg: tea.PasteMsg{Content: "caf\xe9"}, want: "not valid UTF-8"},
	}
	for _, tt := range tests {
		for _, box := range []string{"e", "r"} {
			t.Run(tt.name+" in "+box, func(t *testing.T) {
				m := send(t, keys(t, open(t, opts, 80, 24), box), tt.msg)
				if !strings.Contains(flat(m), tt.want) {
					t.Errorf("the refusal does not say %q:\n%s", tt.want, screenText(m))
				}
				if strings.Contains(flat(m), "caf") || strings.Contains(flat(m), "\uFFFD") {
					t.Errorf("the text was taken anyway:\n%s", screenText(m))
				}
			})
		}
	}

	// A paste that is text goes in as typed, and an escape sequence the
	// decoder did not recognise is not a key anybody typed.
	m := send(t, keys(t, open(t, opts, 80, 24), "e"), tea.PasteMsg{Content: " Pasted."})
	if !strings.Contains(flat(m), "runOptions. Pasted.") {
		t.Errorf("a paste of ordinary text was not taken:\n%s", screenText(m))
	}
	m = send(t, keys(t, open(t, opts, 80, 24), "e"), uv.UnknownEvent("\x1b[?999;1$y"))
	if strings.Contains(flat(m), "UTF-8") {
		t.Errorf("an unrecognised escape sequence was reported as a key:\n%s", screenText(m))
	}
}

// TestTheEditorSaysWhatItCannotHold: the editor shows a tab as spaces and
// leaves control characters out, so a step holding either is not shown as
// the file holds it. Opening such a step says so before the person types;
// keeping it unchanged changes nothing; and a step they did change is saved
// as they saw it.
func TestTheEditorSaysWhatItCannotHold(t *testing.T) {
	const plan = "## Steps\n\n1. Keep\tthe tab.\n2. An ordinary step.\n"
	var s saves
	opts := withSaves(&s)
	opts.Plan = []byte(plan)
	m := keys(t, open(t, opts, 80, 24), "e")
	if got := flat(m); !strings.Contains(got, "tabs or control characters") {
		t.Errorf("opening a step with a tab does not say the editor cannot hold it:\n%s", screenText(m))
	}
	if got := flat(keys(t, m, "ctrl+s")); strings.Contains(got, "edited") {
		t.Errorf("keeping the step unchanged changed the plan:\n%s", got)
	}
	keys(t, typeText(t, m, " More."), "ctrl+s", "s")
	doc, err := plandoc.Parse([]byte(plan))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want, err := doc.Edit(0, "Keep    the tab. More.")
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !bytes.Equal(s.last(), want.Bytes()) {
		t.Errorf("saved:\n%q\nwant the step as it was shown and changed:\n%q", s.last(), want.Bytes())
	}

	if got := flat(keys(t, open(t, opts, 80, 24), "j", "e")); strings.Contains(got, "tabs or control characters") {
		t.Errorf("an ordinary step was said to hold tabs or control characters:\n%s", got)
	}
}

// TestEveryKeyIsListedUnderQuestionMark: the foot of the page has room for
// a few keys, so ? shows them all, with what each does in words. Anything
// that closes it goes back to the plan, and nothing there changes the plan.
func TestEveryKeyIsListedUnderQuestionMark(t *testing.T) {
	var s saves
	m := keys(t, open(t, withSaves(&s), 80, 24), "?")
	got := flat(m)
	for _, want := range []string{
		"j", "choose", "e", "edit", "a", "add", "d", "delete", "K", "move", "u", "undo",
		"s", "save", "c", "approve", "r", "new plan", "q", "leave", "ctrl+z", "?",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the keys screen does not mention %q:\n%s", want, screenText(m))
		}
	}
	for _, k := range []string{"esc", "?", "q"} {
		back := keys(t, m, k)
		if !strings.Contains(flat(back), "1. Add a dryRun field") || strings.Contains(flat(back), "ctrl+z") {
			t.Errorf("%s did not go back to the plan:\n%s", k, screenText(back))
		}
	}
	if keys(t, m, "d", "e", "a"); len(s.plans) != 0 {
		t.Error("a key pressed on the keys screen changed the plan")
	}
}

// TestCtrlZSuspendsTheViewer: ctrl+z puts belay in the background as any
// other program does, rather than being swallowed by raw mode.
func TestCtrlZSuspendsTheViewer(t *testing.T) {
	_, cmd := open(t, review(), 80, 24).Update(press("ctrl+z"))
	if cmd == nil {
		t.Fatal("ctrl+z did nothing")
	}
	if _, ok := cmd().(tea.SuspendMsg); !ok {
		t.Errorf("ctrl+z produced %#v, want a suspend", cmd())
	}
}
