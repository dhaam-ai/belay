package termsafe

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// hostile is text an agent copied out of a hostile repository: a window
// title, an OSC 52 clipboard write ended by BEL, a cursor-up and an
// erase-line to hide the line above, a C1 control-sequence introducer, a
// NUL and a DEL.
const hostile = "\x1b]0;PWNED\x07\x1b]52;c;cm0gLXJmIH4K\x07\x1b[1A\x1b[2K\u009b2J\x00\x7f"

func TestStandIn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		r       rune
		ascii   bool
		want    string
		control bool
	}{
		{name: "ESC is its control picture", r: 0x1b, want: "␛", control: true},
		{name: "ESC in ASCII is caret notation", r: 0x1b, ascii: true, want: "^[", control: true},
		{name: "NUL", r: 0x00, want: "␀", control: true},
		{name: "NUL in ASCII", r: 0x00, ascii: true, want: "^@", control: true},
		{name: "BEL", r: 0x07, want: "␇", control: true},
		{name: "newline is a control too; callers keep it where they need it", r: '\n', want: "␊", control: true},
		{name: "DEL", r: 0x7f, want: "␡", control: true},
		{name: "DEL in ASCII", r: 0x7f, ascii: true, want: "^?", control: true},
		{name: "a C1 control is its code", r: 0x9b, want: "<9B>", control: true},
		{name: "a C1 control is its code in ASCII too", r: 0x85, ascii: true, want: "<85>", control: true},
		{name: "a letter is not a control", r: 'a'},
		{name: "a no-break space is not a control", r: 0xa0},
		{name: "a control picture is not a control", r: '␛'},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, control := StandIn(tt.r, tt.ascii)
			if got != tt.want || control != tt.control {
				t.Errorf("StandIn(%U, ascii %v) = %q, %v; want %q, %v", tt.r, tt.ascii, got, control, tt.want, tt.control)
			}
		})
	}
}

func TestText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text is untouched", in: "1. Add a dryRun field", want: "1. Add a dryRun field"},
		{name: "newlines and tabs are kept", in: "## Steps\n\n1.\tEdit main.go\n", want: "## Steps\n\n1.\tEdit main.go\n"},
		{name: "CRLF line endings read as LF", in: "one\r\ntwo\r\n", want: "one\ntwo\n"},
		{name: "a lone carriage return cannot overwrite the line", in: "safe\rEVIL", want: "safe␍EVIL"},
		{name: "every escape sequence is shown, never sent", in: hostile,
			want: "␛]0;PWNED␇␛]52;c;cm0gLXJmIH4K␇␛[1A␛[2K<9B>2J␀␡"},
		{name: "vertical tab and form feed are shown", in: "a\vb\fc", want: "a␋b␌c"},
		{name: "a byte that is not UTF-8 is the replacement character", in: "a\x9b[2Jb", want: "a�[2Jb"},
		{name: "letters beyond ASCII are kept", in: "café ☕ 東京", want: "café ☕ 東京"},
		{name: "empty stays empty", in: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Text(tt.in); got != tt.want {
				t.Errorf("Text(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text is untouched", in: "add a wc command", want: "add a wc command"},
		{name: "newlines collapse to spaces", in: "line one\nline two", want: "line one line two"},
		{name: "tabs and runs of spaces collapse", in: "a\t\t  b", want: "a b"},
		{name: "leading and trailing whitespace goes", in: "  hi  ", want: "hi"},
		{name: "a carriage return is whitespace, so it cannot overwrite the line", in: "overwrite\rme", want: "overwrite me"},
		{name: "a C1 next line is whitespace too", in: "a\u0085b", want: "a b"},
		{name: "every escape sequence is shown, never sent", in: "goal" + hostile,
			want: "goal␛]0;PWNED␇␛]52;c;cm0gLXJmIH4K␇␛[1A␛[2K<9B>2J␀␡"},
		{name: "a byte that is not UTF-8 is the replacement character", in: "a\x1b\x9bb", want: "a␛�b"},
		{name: "unicode is preserved", in: "café ☕", want: "café ☕"},
		{name: "empty stays empty", in: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Line(tt.in); got != tt.want {
				t.Errorf("Line(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNoControlCharacterGetsThrough runs every rune of the Basic
// Multilingual Plane through Text and Line, alone and between letters, and
// requires that the only control characters to come out are the newline and
// tab Text keeps.
func TestNoControlCharacterGetsThrough(t *testing.T) {
	t.Parallel()

	for r := rune(0); r <= 0xffff; r++ {
		in := "a" + string(r) + "b"
		for name, out := range map[string]string{"Text": Text(in), "Line": Line(in)} {
			if !utf8.ValidString(out) {
				t.Fatalf("%s(%q) = %q, which is not UTF-8", name, in, out)
			}
			kept := func(c rune) bool { return name == "Text" && (c == '\n' || c == '\t') }
			for _, c := range out {
				if unicode.IsControl(c) && !kept(c) {
					t.Fatalf("%s(%q) = %q carries the control character %U", name, in, out, c)
				}
			}
		}
	}
}

func TestJSON(t *testing.T) {
	t.Parallel()

	want := "goal" + hostile + "\u0085end"
	doc, err := json.Marshal(map[string]string{"goal": want})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := JSON(doc)

	for _, c := range string(got) {
		if unicode.IsControl(c) {
			t.Fatalf("JSON(%q) = %q still carries %U", doc, got, c)
		}
	}
	for _, escaped := range []string{`\u007f`, `\u009b`, `\u0085`} {
		if !strings.Contains(string(got), escaped) {
			t.Errorf("JSON(...) = %s, want %s escaped rather than replaced", got, escaped)
		}
	}
	var back map[string]string
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("JSON(...) = %s is not a JSON document any more: %v", got, err)
	}
	if back["goal"] != want {
		t.Errorf("a script reading JSON(...) gets %q, want the goal exactly as it was, %q", back["goal"], want)
	}
	if plain := []byte(`{"goal":"add tests"}`); string(JSON(plain)) != string(plain) {
		t.Errorf("JSON changed a document with nothing to escape: %s", JSON(plain))
	}
}
