// This file is the executable specification for the wc fixture. It does not
// implement wc — see main.go for the stub that must be replaced — it only
// describes, through table-driven cases, exactly what a correct
// implementation must do.
//
// Test cases are grouped into tiers of increasing difficulty, encoded in
// each subtest's name (tier1_bytes_only, tier2_lines_only, ...). A partial
// implementation should pass earlier tiers while later tiers still fail:
// -c (raw byte counting) requires no parsing of the input's structure at
// all, -l requires recognizing a single byte value, -w requires whitespace
// tokenization, and -m requires decoding UTF-8 rather than counting bytes.
// The stdin path (tier6, tier7) and the large-file path (tier8) are treated
// as their own hard cases because they change where input comes from, not
// just how it's counted. See GOAL.md for the full behavioral spec and
// README.md for a table mapping tiers to the required flags.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"
)

// wantCounts holds the four statistics wc can report for one input. Not
// every case uses every field — only the fields selected by a case's
// showL/showW/showM/showC flags are rendered into its expected output.
type wantCounts struct {
	lines, words, chars, bytes int
}

// formatExpected renders wantCounts into the exact output line specified by
// GOAL.md: each selected statistic right-justified in an 8-character field,
// in the fixed order lines, words, chars, bytes regardless of which flags
// were requested or the order they were given on the command line; a single
// space and the filename appended when filename is non-empty (input read
// from a named file); nothing appended when filename is empty (input read
// from standard input); one trailing newline always.
func formatExpected(c wantCounts, showL, showW, showM, showC bool, filename string) string {
	out := ""
	if showL {
		out += fmt.Sprintf("%8d", c.lines)
	}
	if showW {
		out += fmt.Sprintf("%8d", c.words)
	}
	if showM {
		out += fmt.Sprintf("%8d", c.chars)
	}
	if showC {
		out += fmt.Sprintf("%8d", c.bytes)
	}
	if filename != "" {
		out += " " + filename
	}
	return out + "\n"
}

// largeFixturePattern is repeated largeFixtureRepeat times to build the
// large-ish input required by GOAL.md. It is pure ASCII so its byte count
// and character count are identical, keeping the arithmetic below exact and
// easy to audit: wordsPerLine is the number of whitespace-delimited words
// in one copy of the pattern ("The quick brown fox jumps over the lazy
// dog." = 9 words), and the pattern contains exactly one newline.
const (
	largeFixturePattern = "The quick brown fox jumps over the lazy dog.\n"
	largeFixtureRepeat  = 7000
	largeWordsPerLine   = 9
)

// largeFixturePath is where writeLargeFixture materializes the generated
// large input. It lives under testdata/ so it can be referenced as an
// ordinary file-argument path (required by GOAL.md's "large-ish file"
// case), but fixtures/wc/.gitignore excludes it: it is regenerated,
// byte-for-byte identically, at the start of every test run rather than
// committed, per GOAL.md's determinism requirement.
const largeFixturePath = "testdata/generated/large.txt"

// writeLargeFixture (re)creates largeFixturePath and returns the exact
// counts implied by largeFixturePattern x largeFixtureRepeat. It registers
// its own cleanup so the generated directory never lingers after the test
// run, successful or not.
func writeLargeFixture(t *testing.T) wantCounts {
	t.Helper()

	dir := "testdata/generated"
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("cleaning up %s: %v", dir, err)
		}
	})

	var buf bytes.Buffer
	buf.Grow(len(largeFixturePattern) * largeFixtureRepeat)
	for i := 0; i < largeFixtureRepeat; i++ {
		buf.WriteString(largeFixturePattern)
	}
	if err := os.WriteFile(largeFixturePath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing %s: %v", largeFixturePath, err)
	}

	n := buf.Len()
	return wantCounts{
		lines: largeFixtureRepeat,
		words: largeWordsPerLine * largeFixtureRepeat,
		chars: n, // pure ASCII: one byte per character
		bytes: n,
	}
}

// mustReadFile reads a committed testdata file and fails the test
// immediately if it is missing, so a broken fixture is reported clearly
// instead of surfacing as a confusing mismatch deep in the table.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- path is always a hardcoded testdata literal from this file, never external input
	if err != nil {
		t.Fatalf("reading fixture %s: %v", path, err)
	}
	return b
}

// TestRun exercises run against every required scenario from GOAL.md.
// Cases are listed in increasing order of difficulty (see the package
// comment above); running `go test -v` shows the ramp directly in the
// subtest names.
func TestRun(t *testing.T) {
	// Exact counts for each committed testdata file, verified independently
	// (see fixtures/wc/README.md) rather than computed by the code under
	// test, so a buggy implementation can never accidentally agree with a
	// buggy expectation.
	var (
		cEmpty       = wantCounts{lines: 0, words: 0, chars: 0, bytes: 0}
		cNoTrailNL   = wantCounts{lines: 0, words: 2, chars: 11, bytes: 11}
		cTrailNL     = wantCounts{lines: 1, words: 2, chars: 12, bytes: 12}
		cCRLF        = wantCounts{lines: 3, words: 6, chars: 37, bytes: 37}
		cMultiWS     = wantCounts{lines: 2, words: 6, chars: 28, bytes: 28}
		cLeadTrailWS = wantCounts{lines: 2, words: 6, chars: 47, bytes: 47}
		// utf8_multibyte.txt: 43 bytes but only 24 characters. The gap is
		// real, not a typo: é and ï are 2-byte UTF-8 sequences, € and each
		// of 日 本 語 テ ス ト are 3-byte sequences, and 😀 is a 4-byte
		// sequence — 19 multi-byte characters account for the extra 19
		// bytes (43 - 24 = 19) beyond one byte per character.
		cUTF8 = wantCounts{lines: 2, words: 6, chars: 24, bytes: 43}
		// "foo\x00bar baz\n": the NUL byte is not whitespace, so it sits
		// inside the first word ("foo\x00bar") rather than splitting it.
		cNUL = wantCounts{lines: 1, words: 2, chars: 12, bytes: 12}
	)

	cLarge := writeLargeFixture(t)

	trailNLBytes := mustReadFile(t, "testdata/trailing_newline.txt")
	utf8Bytes := mustReadFile(t, "testdata/utf8_multibyte.txt")
	multiWSBytes := mustReadFile(t, "testdata/multi_space_tabs.txt")
	nulBytes := []byte("foo\x00bar baz\n")

	type tc struct {
		name string
		args []string // flags only; the file path (if any) is appended separately
		file string   // relative path under testdata/; "" means read from stdin
		in   []byte   // stdin content, used only when file == ""

		counts                     wantCounts
		showL, showW, showM, showC bool
	}

	cases := []tc{
		// --- tier1_bytes_only: -c is pure byte counting. No newline or
		// whitespace semantics are needed, so this tier is passable before
		// anything else works. ---
		{name: "tier1_bytes_only/empty_file", args: []string{"-c"}, file: "testdata/empty.txt", counts: cEmpty, showC: true},
		{name: "tier1_bytes_only/no_trailing_newline", args: []string{"-c"}, file: "testdata/no_trailing_newline.txt", counts: cNoTrailNL, showC: true},
		{name: "tier1_bytes_only/trailing_newline", args: []string{"-c"}, file: "testdata/trailing_newline.txt", counts: cTrailNL, showC: true},
		{name: "tier1_bytes_only/crlf", args: []string{"-c"}, file: "testdata/crlf.txt", counts: cCRLF, showC: true},
		{name: "tier1_bytes_only/utf8_multibyte", args: []string{"-c"}, file: "testdata/utf8_multibyte.txt", counts: cUTF8, showC: true},
		{name: "tier1_bytes_only/large_file", args: []string{"-c"}, file: largeFixturePath, counts: cLarge, showC: true},

		// --- tier2_lines_only: -l counts '\n' bytes. no_trailing_newline
		// is the classic gotcha: a file with content but no newline byte
		// has a line count of 0. ---
		{name: "tier2_lines_only/empty_file", args: []string{"-l"}, file: "testdata/empty.txt", counts: cEmpty, showL: true},
		{name: "tier2_lines_only/no_trailing_newline", args: []string{"-l"}, file: "testdata/no_trailing_newline.txt", counts: cNoTrailNL, showL: true},
		{name: "tier2_lines_only/trailing_newline", args: []string{"-l"}, file: "testdata/trailing_newline.txt", counts: cTrailNL, showL: true},
		{name: "tier2_lines_only/crlf", args: []string{"-l"}, file: "testdata/crlf.txt", counts: cCRLF, showL: true},
		{name: "tier2_lines_only/leading_trailing_whitespace", args: []string{"-l"}, file: "testdata/leading_trailing_whitespace.txt", counts: cLeadTrailWS, showL: true},

		// --- tier3_words_only: -w requires whitespace tokenization —
		// collapsing runs of spaces/tabs, ignoring leading/trailing
		// whitespace, and treating '\r' as whitespace rather than letting
		// it stick to the end of a CRLF line's last word. ---
		{name: "tier3_words_only/empty_file", args: []string{"-w"}, file: "testdata/empty.txt", counts: cEmpty, showW: true},
		{name: "tier3_words_only/multi_space_and_tabs", args: []string{"-w"}, file: "testdata/multi_space_tabs.txt", counts: cMultiWS, showW: true},
		{name: "tier3_words_only/leading_trailing_whitespace", args: []string{"-w"}, file: "testdata/leading_trailing_whitespace.txt", counts: cLeadTrailWS, showW: true},
		{name: "tier3_words_only/crlf", args: []string{"-w"}, file: "testdata/crlf.txt", counts: cCRLF, showW: true},

		// --- tier4_default_no_flags: no flags means -l -w -c together, in
		// that fixed output order — regardless of what order equivalent
		// explicit flags are given in. This is where formatting (field
		// width, spacing, filename) first has to be exactly right. ---
		{name: "tier4_default_no_flags/trailing_newline", args: nil, file: "testdata/trailing_newline.txt", counts: cTrailNL, showL: true, showW: true, showC: true},
		{name: "tier4_default_no_flags/empty_file", args: nil, file: "testdata/empty.txt", counts: cEmpty, showL: true, showW: true, showC: true},
		{name: "tier4_default_no_flags/crlf", args: nil, file: "testdata/crlf.txt", counts: cCRLF, showL: true, showW: true, showC: true},
		{name: "tier4_default_no_flags/multi_space_and_tabs", args: nil, file: "testdata/multi_space_tabs.txt", counts: cMultiWS, showL: true, showW: true, showC: true},
		{name: "tier4_default_no_flags/leading_trailing_whitespace", args: nil, file: "testdata/leading_trailing_whitespace.txt", counts: cLeadTrailWS, showL: true, showW: true, showC: true},
		{
			// Same expectation as trailing_newline above: explicit -w -c -l
			// in a scrambled order must print in the same lines/words/bytes
			// order as the flag-free default, proving output order is fixed
			// rather than mirroring argv order.
			name: "tier4_default_no_flags/explicit_flags_scrambled_order", args: []string{"-w", "-c", "-l"}, file: "testdata/trailing_newline.txt",
			counts: cTrailNL, showL: true, showW: true, showC: true,
		},

		// --- tier5_chars_multibyte: -m counts Unicode characters (code
		// points) after decoding UTF-8, not bytes. All test input is valid
		// UTF-8. ---
		{name: "tier5_chars_multibyte/empty_file", args: []string{"-m"}, file: "testdata/empty.txt", counts: cEmpty, showM: true},
		{name: "tier5_chars_multibyte/ascii_chars_equal_bytes", args: []string{"-m"}, file: "testdata/trailing_newline.txt", counts: cTrailNL, showM: true},
		{name: "tier5_chars_multibyte/utf8_chars_differ_from_bytes", args: []string{"-m"}, file: "testdata/utf8_multibyte.txt", counts: cUTF8, showM: true},
		{
			// The defining case: -c and -m on the same multi-byte input
			// must report two different numbers (43 bytes, 24 characters)
			// side by side in one line.
			name: "tier5_chars_multibyte/utf8_bytes_and_chars_together", args: []string{"-c", "-m"}, file: "testdata/utf8_multibyte.txt",
			counts: cUTF8, showM: true, showC: true,
		},
		{name: "tier5_chars_multibyte/utf8_lines_words_chars", args: []string{"-l", "-w", "-m"}, file: "testdata/utf8_multibyte.txt", counts: cUTF8, showL: true, showW: true, showM: true},

		// --- tier6_nul_byte_via_stdin: a NUL byte must not be mistaken for
		// a whitespace separator or a string terminator. Read from stdin
		// (see tier7) rather than committed to testdata/, so the fixture
		// never carries a raw NUL byte in a text-oriented git history. ---
		{name: "tier6_nul_byte_via_stdin/bytes", args: []string{"-c"}, in: nulBytes, counts: cNUL, showC: true},
		{name: "tier6_nul_byte_via_stdin/words", args: []string{"-w"}, in: nulBytes, counts: cNUL, showW: true},
		{name: "tier6_nul_byte_via_stdin/chars", args: []string{"-m"}, in: nulBytes, counts: cNUL, showM: true},
		{name: "tier6_nul_byte_via_stdin/default_combo", args: nil, in: nulBytes, counts: cNUL, showL: true, showW: true, showC: true},

		// --- tier7_stdin_input: reading input from standard input when no
		// file argument is given, and — just as important — NOT printing a
		// filename when there wasn't one. ---
		{name: "tier7_stdin_input/simple_default_no_filename_in_output", args: nil, in: trailNLBytes, counts: cTrailNL, showL: true, showW: true, showC: true},
		{name: "tier7_stdin_input/utf8_chars", args: []string{"-m"}, in: utf8Bytes, counts: cUTF8, showM: true},
		{name: "tier7_stdin_input/multi_space_words", args: []string{"-w"}, in: multiWSBytes, counts: cMultiWS, showW: true},

		// --- tier8_large_file_integration: a few-hundred-KB file read from
		// disk, all four fields at once. Exercises correctness and basic
		// scalability together rather than either in isolation. ---
		{name: "tier8_large_file_integration/default_combo", args: nil, file: largeFixturePath, counts: cLarge, showL: true, showW: true, showC: true},
		{name: "tier8_large_file_integration/all_four_flags", args: []string{"-l", "-w", "-m", "-c"}, file: largeFixturePath, counts: cLarge, showL: true, showW: true, showM: true, showC: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{}, c.args...)
			var stdin io.Reader = bytes.NewReader(nil)
			if c.file != "" {
				args = append(args, c.file)
			} else {
				stdin = bytes.NewReader(c.in)
			}

			var stdout, stderr bytes.Buffer
			code := run(args, stdin, &stdout, &stderr)

			if code != 0 {
				t.Errorf("run(%v) exit code = %d, want 0 (stderr: %q)", args, code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("run(%v) wrote to stderr, want silence on success: %q", args, stderr.String())
			}

			want := formatExpected(c.counts, c.showL, c.showW, c.showM, c.showC, c.file)
			if got := stdout.String(); got != want {
				t.Errorf("run(%v) stdout =\n%q\nwant:\n%q", args, got, want)
			}
		})
	}
}
