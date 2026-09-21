package planview

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
