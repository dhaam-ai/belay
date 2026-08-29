//go:build unix

package exec

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestTokenize pins the contract that makes shell injection structurally
// impossible: every construct a shell would act on survives as literal
// argument text. Tokenize removes quoting and nothing else.
func TestTokenize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   \t\n ", nil},
		{"simple", "go test ./...", []string{"go", "test", "./..."}},
		{"collapses runs of spaces", "go   test", []string{"go", "test"}},
		{"tabs and newlines split", "go\ttest\n./...", []string{"go", "test", "./..."}},
		{"leading and trailing space", "  go test  ", []string{"go", "test"}},

		{"command substitution is literal", "echo $(whoami)", []string{"echo", "$(whoami)"}},
		{"backticks are literal", "echo `id`", []string{"echo", "`id`"}},
		{"variable is literal", "echo $HOME", []string{"echo", "$HOME"}},
		{"braced variable is literal", "echo ${IFS}", []string{"echo", "${IFS}"}},
		{"and operator is literal", "a && b", []string{"a", "&&", "b"}},
		{"or operator is literal", "a || b", []string{"a", "||", "b"}},
		{"semicolon is literal", "a; b", []string{"a;", "b"}},
		{"pipe is literal", "a | b", []string{"a", "|", "b"}},
		{"redirect is literal", "a > out.txt", []string{"a", ">", "out.txt"}},
		{"append redirect is literal", "a >> out.txt", []string{"a", ">>", "out.txt"}},
		{"heredoc is literal", "a << EOF", []string{"a", "<<", "EOF"}},
		{"background is literal", "a &", []string{"a", "&"}},
		{"subshell parens are literal", "(id)", []string{"(id)"}},
		{"glob is literal", "ls *.go", []string{"ls", "*.go"}},
		{"brace expansion is literal", "ls {a,b}", []string{"ls", "{a,b}"}},
		{"tilde is literal", "ls ~/src", []string{"ls", "~/src"}},
		{"comment marker is literal", "go test # nope", []string{"go", "test", "#", "nope"}},
		{"newline does not chain", "a\nb", []string{"a", "b"}},

		{"single quotes group", "go test -run 'Test A'", []string{"go", "test", "-run", "Test A"}},
		{"double quotes group", `go test -run "Test A"`, []string{"go", "test", "-run", "Test A"}},
		{"single quotes keep metacharacters", `echo '$(id) && x'`, []string{"echo", "$(id) && x"}},
		{"double quotes keep metacharacters", `echo "$(id) && x"`, []string{"echo", "$(id) && x"}},
		{"empty single quotes are an argument", "cmd '' x", []string{"cmd", "", "x"}},
		{"empty double quotes are an argument", `cmd "" x`, []string{"cmd", "", "x"}},
		{"adjacent quotes join", `cmd a"b"c`, []string{"cmd", "abc"}},
		{"quote inside word", `cmd -run='Test A'`, []string{"cmd", "-run=Test A"}},

		{"backslash escapes space", `cmd a\ b`, []string{"cmd", "a b"}},
		{"backslash escapes quote", `cmd \"x\"`, []string{"cmd", `"x"`}},
		{"backslash escapes dollar", `cmd \$(id)`, []string{"cmd", "$(id)"}},
		{"backslash escapes backslash", `cmd a\\b`, []string{"cmd", `a\b`}},
		{"backslash in double quotes escapes quote", `cmd "a\"b"`, []string{"cmd", `a"b`}},
		{"backslash in double quotes is otherwise literal", `cmd "C:\temp\new"`, []string{"cmd", `C:\temp\new`}},
		{"backslash in single quotes is literal", `cmd 'a\b'`, []string{"cmd", `a\b`}},

		{"utf8 survives", "echo héllo→", []string{"echo", "héllo→"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Tokenize(tc.in)
			if err != nil {
				t.Fatalf("Tokenize(%q): unexpected error: %v", tc.in, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Tokenize(%q) mismatch (-want +got):\n%s", tc.in, diff)
			}
		})
	}
}

func TestTokenizeErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"unterminated single quote", "echo 'abc", ErrUnterminatedQuote},
		{"unterminated double quote", `echo "abc`, ErrUnterminatedQuote},
		{"trailing backslash", `echo abc\`, ErrUnterminatedQuote},
		{"null byte", "echo a\x00b", ErrNullByte},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Tokenize(tc.in); !errors.Is(err, tc.want) {
				t.Errorf("Tokenize(%q) error = %v, want %v", tc.in, err, tc.want)
			}
		})
	}
}

func TestParseCommandLine(t *testing.T) {
	t.Parallel()
	got, err := ParseCommandLine(`"/usr/local/bin/my tool" test -run 'A B'`)
	if err != nil {
		t.Fatalf("ParseCommandLine: %v", err)
	}
	if got.Path != "/usr/local/bin/my tool" {
		t.Errorf("Path = %q", got.Path)
	}
	if diff := cmp.Diff([]string{"test", "-run", "A B"}, got.Args); diff != "" {
		t.Errorf("Args mismatch (-want +got):\n%s", diff)
	}
}

func TestParseCommandLineEmpty(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "   ", "\t\n"} {
		if _, err := ParseCommandLine(in); !errors.Is(err, ErrEmptyCommand) {
			t.Errorf("ParseCommandLine(%q) error = %v, want ErrEmptyCommand", in, err)
		}
	}
}
