# Goal: implement `wc`

Implement a command-line program, in this directory, that reports the
number of lines, words, characters, and bytes in its input — in the
spirit of the Unix `wc` utility.

Verification: `go test ./...`, run from this directory. That command
currently fails; your job is to make every test pass without changing
`wc_test.go`. The tests are the authoritative specification of expected
output — if anything below seems ambiguous, the test cases resolve the
ambiguity.

## Command-line interface

```
wc [-c] [-l] [-w] [-m] [file]
```

- `-c` — count bytes.
- `-l` — count lines.
- `-w` — count words.
- `-m` — count characters.
- Any combination of the four flags may be given. Flags precede the file
  argument.
- `file` is optional. When given, it is a path to read from. When
  omitted, read all of standard input instead.
- When no flags are given at all, behave as if `-l`, `-w`, and `-c` had
  all been given together (this matches real-world `wc`'s default). Note
  that `-m` is not part of this default; it only appears when requested
  explicitly.

Combined short flags (e.g. `-cl` as a single argument) are not required.
Multiple file arguments and a totals line are not required — exactly one
input source (one file, or standard input) per invocation.

## What each count means

- **Bytes** (`-c`): the exact number of bytes in the input, unmodified.
- **Lines** (`-l`): the number of newline (`\n`, byte value 10) characters
  in the input. This is a count of newline bytes, not a count of visual
  lines — input with content but no newline character at all has a line
  count of 0. For example, `"hello\nworld"` has a line count of 1 (one
  `\n`), while `"hello\nworld\n"` has a line count of 2.
- **Words** (`-w`): the number of words, where a word is a maximal run of
  characters containing no whitespace. Whitespace, for this purpose, is
  any of: space, tab, newline, carriage return, vertical tab, and form
  feed. A run of one or more consecutive whitespace characters separates
  words; leading and trailing whitespace do not produce empty words. A
  carriage return is whitespace like any other — it does not attach to
  the word before it.
- **Characters** (`-m`): the number of Unicode characters (code points)
  in the input, decoding the input as UTF-8. This can differ from the
  byte count when the input contains multi-byte UTF-8 characters — for
  example, a single 'é' character is one character but two bytes in
  UTF-8. All input used to test this program is valid UTF-8; you do not
  need to handle malformed byte sequences.

A byte with value 0 (NUL) is not whitespace and is not special-cased: it
is ordinary input data like any other byte or character.

## Output format

Print one line to standard output:

- For each requested count, print its decimal value right-justified in a
  field 8 characters wide (pad with leading spaces; a value with 8 or
  more digits is printed in full, unpadded). When more than one count is
  requested, the fields appear immediately adjacent to each other (the
  padding itself is the only separation) in this fixed order: lines,
  words, characters, bytes — regardless of the order the corresponding
  flags were given on the command line, and regardless of which subset
  was requested.
- If input was read from a named file, print a single space followed by
  that file path exactly as given on the command line, then a newline.
- If input was read from standard input (no file argument was given), do
  not print a filename or a trailing space — the line ends immediately
  after the last numeric field, followed by a newline.
- On success, print nothing to standard error, and exit with status 0.

Example: given a file `report.txt` containing 7 lines, 58 words, and 342
bytes, running the default (no flags) produces:

```
       7      58     342 report.txt
```

The same input piped through standard input with no flags produces:

```
       7      58     342
```

(no trailing space, no filename).

## Out of scope

The following are not required and are not tested: combined short flags,
multiple file operands, a totals line, handling of a missing or
unreadable file, handling of invalid/malformed UTF-8, and handling of
unrecognized flags. Focus on the behavior described above.

## Constraints

- Go standard library only. Do not add anything to `go.mod` — it must
  keep declaring zero dependencies.
- No network access, no external commands, no reliance on the local
  clock, locale, or timezone. The program's output must depend only on
  its input.
- The program must build with `go build .` from this directory.
