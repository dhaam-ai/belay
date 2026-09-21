package planview

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// wide is a plan whose text is wider than it is long: CJK characters and
// emoji take two cells each, so a layout that measured bytes or runes would
// draw past the edge of the terminal.
const wide = `## Steps

1. 设置一个新的命令行标志，并且在帮助文本中清楚地说明它会做什么以及不会做什么事情。
2. Add tests 🧪🧪🧪 for the flag 🚩 and keep every existing test 🟢 green while doing it.
3. ` + "`" + `internal/cli/run.go` + "`" + ` gains a field whose name is deliberately very long: aVeryLongIdentifierWithNoBreaksAtAllThatMustStillFitOnTheScreen.
`

// longPlan has twelve steps, more than a small terminal can show at once.
func longPlan() string {
	var b strings.Builder
	b.WriteString("## Summary\n\nA plan with many steps.\n\n## Steps\n\n")
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&b, "%d. Step number %d, which says what to do in a sentence.\n", i, i)
	}
	b.WriteString("\n## Tests\n\nThe tests.\n")
	return b.String()
}

// assertFits fails for any line of the view wider than width cells, or for
// more lines than height. Width is measured both ways a terminal measures
// it — by grapheme and by wcwidth — because bubbletea's renderer starts with
// one and switches to the other when the terminal says it can.
func assertFits(t *testing.T, m Model, width, height int) {
	t.Helper()
	lines := strings.Split(m.View().Content, "\n")
	if len(lines) > height {
		t.Errorf("%dx%d: the view has %d lines", width, height, len(lines))
	}
	for i, l := range lines {
		if w, wc := ansi.StringWidth(l), ansi.StringWidthWc(l); w > width || wc > width {
			t.Errorf("%dx%d: line %d is %d cells wide (%d by wcwidth): %q", width, height, i, w, wc, ansi.Strip(l))
		}
	}
}

// TestViewFitsEveryTerminalSize draws the viewer at every size from 1x1 to
// 200x60. No size may panic, and nothing may be drawn past the edge: a line
// one cell too wide wraps, and every line below it lands one row lower than
// the renderer thinks it is.
func TestViewFitsEveryTerminalSize(t *testing.T) {
	for name, plan := range map[string]string{"canonical": canonical, "wide": wide} {
		opts := review()
		opts.Plan = []byte(plan)
		t.Run(name+" plan", func(t *testing.T) {
			t.Parallel()
			m := New(opts)
			for width := 1; width <= 200; width++ {
				for height := 1; height <= 60; height++ {
					m = send(t, m, tea.WindowSizeMsg{Width: width, Height: height})
					assertFits(t, m, width, height)
				}
			}
		})
		// And with the step editor open, which is drawn in the step's place,
		// and with the new-plan box and its explanation, which take the foot
		// of the page; both follow the window's size. Every size up to
		// 50x20, where anything that overflows overflows first, and a sample
		// of the larger ones.
		for key, what := range map[string]string{"e": "editor", "r": "new-plan box"} {
			t.Run(name+" plan with the "+what+" open", func(t *testing.T) {
				t.Parallel()
				m := typeText(t, keys(t, New(opts), key), "some words typed into the box")
				for width := 1; width <= 200; width = next(width, 50, 17) {
					for height := 1; height <= 60; height = next(height, 20, 8) {
						m = send(t, m, tea.WindowSizeMsg{Width: width, Height: height})
						assertFits(t, m, width, height)
					}
				}
			})
		}
	}
}

// next steps n by one up to dense, and by stride after it.
func next(n, dense, stride int) int {
	if n < dense {
		return n + 1
	}
	return n + stride
}

// TestSmallTerminalsSayWhatToDo: below 40 by 10 there is no room to read a
// plan in, so the whole screen says so, says the plan is safe, and says how
// to leave; and from 40 to 79 columns the key line is the short one.
func TestSmallTerminalsSayWhatToDo(t *testing.T) {
	for _, size := range [][2]int{{39, 24}, {80, 9}, {20, 5}} {
		got := screenText(open(t, review(), size[0], size[1]))
		flat := strings.Join(strings.Fields(got), " ")
		for _, want := range []string{"larger", "safe", "q leaves"} {
			if !strings.Contains(flat, want) {
				t.Errorf("%dx%d: the screen does not say %q:\n%s", size[0], size[1], want, got)
			}
		}
		if strings.Contains(got, "1. Add a dryRun") {
			t.Errorf("%dx%d: the plan is drawn on a screen too small to read it on", size[0], size[1])
		}
	}

	compact := screenText(open(t, review(), 60, 24))
	if strings.Contains(compact, "e edit") || !strings.Contains(compact, "q leave") {
		t.Errorf("at 60 columns the key line should be the short one:\n%s", compact)
	}
	full := screenText(open(t, review(), 80, 24))
	if !strings.Contains(full, "e edit") {
		t.Errorf("at 80 columns every editing key should be listed:\n%s", full)
	}
}

// TestResizeKeepsTheSelectedStepOnScreen: select a step near the end of a
// long plan and make the window smaller. The step must still be on the
// screen; keeping the old scroll position would leave the person looking at
// text they did not choose, with the cursor somewhere below the edge.
func TestResizeKeepsTheSelectedStepOnScreen(t *testing.T) {
	opts := review()
	opts.Plan = []byte(longPlan())
	m := open(t, opts, 80, 40)
	for range 9 {
		m = keys(t, m, "j")
	}
	if line := selectedLine(m); !strings.Contains(line, "10. Step number 10") {
		t.Fatalf("the selection is on %q, want step 10", line)
	}
	for _, height := range []int{20, 12, 10} {
		m = send(t, m, tea.WindowSizeMsg{Width: 80, Height: height})
		if line := selectedLine(m); !strings.Contains(line, "10. Step number 10") {
			t.Errorf("at height %d the selected step is off the screen:\n%s", height, screenText(m))
		}
	}
}

// hostile is a plan quoting text an agent copied out of a hostile
// repository: an OSC 52 clipboard write ended by BEL, a clear-screen, a C1
// control-sequence introducer, a NUL and a DEL.
const hostile = "## Steps\n\n" +
	"1. Copy \x1b]52;c;cm0gLXJmIH4K\x07 into the clipboard.\n" +
	"2. Clear \x1b[2J the screen, then \u009b31m colour it.\n" +
	"3. A NUL \x00 and a DEL \x7f in the text.\n"

// TestPlanTextCannotDriveTheTerminal: nothing the plan says reaches the
// terminal as a control character. Each is drawn as a visible stand-in, so
// the person can see something odd is in their plan without the terminal
// acting on it.
func TestPlanTextCannotDriveTheTerminal(t *testing.T) {
	for _, where := range []string{"plan", "goal"} {
		opts := review()
		if where == "plan" {
			opts.Plan = []byte(hostile)
		} else {
			opts.Goal = "make \x1b]52;c;Zm9v\x07 it \u009b2J work"
		}
		content := open(t, opts, 100, 24).View().Content
		for _, bad := range []string{"\x1b", "\x07", "\u009b", "\x00", "\x7f"} {
			if strings.Contains(content, bad) {
				t.Errorf("%s: the view carries %q to the terminal:\n%q", where, bad, content)
			}
		}
	}
	got := screenText(open(t, withPlan(hostile), 100, 24))
	for _, want := range []string{"␛]52;c;cm0gLXJmIH4K␇", "␛[2J", "<9B>31m", "␀", "␡"} {
		if !strings.Contains(got, want) {
			t.Errorf("the view does not show the stand-in %q:\n%s", want, got)
		}
	}
}

// withPlan is review() with plan as the plan.
func withPlan(plan string) Options {
	opts := review()
	opts.Plan = []byte(plan)
	return opts
}
