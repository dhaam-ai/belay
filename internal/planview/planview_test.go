package planview

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
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
