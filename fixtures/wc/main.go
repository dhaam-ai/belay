// Command wc counts lines, words, characters, and bytes in its input, in
// the spirit of the Unix wc utility.
//
// This file is an intentionally-unimplemented stub: it builds and runs
// cleanly, but run below has not been written yet, so every behavior
// described in GOAL.md currently fails. See GOAL.md for the full
// specification and wc_test.go for the exact expected behavior — the
// implementation must satisfy those tests, but is otherwise free to
// restructure this file (or add others) however it likes, as long as the
// package remains buildable with `go build .` and run keeps this exact
// signature so wc_test.go continues to compile against it.
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run parses args (flags followed by an optional input file path), reads
// the selected input — the named file, or standard input when no file is
// given — and writes the requested counts to stdout. It returns a process
// exit code (0 on success).
//
// NOT YET IMPLEMENTED. See GOAL.md for the behavior run must have, and
// wc_test.go for the exact cases it must satisfy.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	_, _ = fmt.Fprintln(stderr, "wc: not implemented")
	return 1
}
