package cli

import (
	"io"
	"os"
	"strings"

	"github.com/dhaam-ai/belay/internal/planview"
)

// viewerTerminal is the answer to "may belay open the plan viewer here",
// and, when it may, what it opens on and how it should draw.
//
// The zero value is a refusal, which is what every caller that has not
// asked — and every test that builds a ui by hand — gets. Opening a
// full-screen viewer where nobody is watching is the one failure this whole
// feature could add to belay: a scripted run would stop, wait for a
// keypress that never comes, and hold the run open until something killed
// it.
type viewerTerminal struct {
	// ok reports whether the viewer may open.
	ok bool
	// reason is why it may not, in plain words, when ok is false.
	reason string
	// in and out are the terminal to read and to draw on. They are nil
	// unless ok.
	in, out *os.File
	// ascii reports that the terminal's locale is not UTF-8, so the viewer
	// must draw in ASCII, and locale names the setting that says so.
	ascii  bool
	locale string
}

// realTerminal is the check belay makes of a stream: the terminal driver's
// answer, not the file's mode. It is a variable so that a test can put a
// terminal where there is none.
var realTerminal = planview.IsTerminal

// detectViewerTerminal decides whether the plan viewer may open, and says
// why not when it may not.
//
// Every rule is a way of asking "is a person sitting in front of this, able
// to answer?", and all of them must hold:
//
//   - in and out are both terminals, by ioctl. Both, because the viewer
//     reads keys from one and draws on the other, and by ioctl because
//     /dev/null is a character device and a check on the file's mode — the
//     one belay's colour decision makes — takes it for a terminal.
//   - TERM is set and is not "dumb". A terminal that says it cannot render
//     escape sequences cannot show a full-screen page.
//   - CI is unset, empty, "0" or "false". A CI runner may well give a
//     process a pseudo-terminal, so the terminal check alone does not
//     settle it; anything else in CI means nobody is watching.
//   - --no-input was not given, which is how a person says so outright.
//
// In every other case belay does what it has always done: it prints and
// exits, and the run waits at its plan for `belay resume`.
func detectViewerTerminal(in io.Reader, out io.Writer, noInput bool,
	getenv func(string) string, isTerminal func(*os.File) bool) viewerTerminal {
	refuse := func(reason string) viewerTerminal { return viewerTerminal{reason: reason} }
	switch {
	case noInput:
		return refuse("--no-input was given")
	case isCI(getenv):
		return refuse("CI is set, so belay takes it that nobody is at the terminal")
	case getenv("TERM") == "":
		return refuse("TERM is not set, so belay cannot tell what this terminal can draw")
	case getenv("TERM") == "dumb":
		return refuse("TERM is \"dumb\", which is a terminal that cannot draw a full-screen page")
	}

	inFile, inOK := in.(*os.File)
	outFile, outOK := out.(*os.File)
	if !inOK || !outOK || !isTerminal(inFile) || !isTerminal(outFile) {
		return refuse("belay is not reading from and writing to a terminal")
	}

	term := viewerTerminal{ok: true, in: inFile, out: outFile}
	utf8, setting := utf8Locale(getenv)
	term.ascii, term.locale = !utf8, setting
	return term
}

// isCI reports whether the environment says this is a CI runner. A value
// that says "no" is taken at its word: somebody who exports CI=false has
// said this is not one.
func isCI(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("CI"))) {
	case "", "0", "false":
		return false
	default:
		return true
	}
}

// utf8Locale reports whether the terminal's locale is UTF-8, and, when it
// is not, names the setting that decided it.
//
// It reads the three variables POSIX gives, in the order POSIX gives them:
// LC_ALL overrides everything, LC_CTYPE settles which characters are which,
// and LANG is the fallback. The first one that is set decides; a locale
// nothing sets is the POSIX default, which is not UTF-8.
func utf8Locale(getenv func(string) string) (bool, string) {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		value := getenv(name)
		if value == "" {
			continue
		}
		// The codeset follows the dot: "en_US.UTF-8", "C.utf8".
		_, codeset, _ := strings.Cut(value, ".")
		codeset = strings.ToLower(strings.ReplaceAll(codeset, "-", ""))
		if codeset == "utf8" {
			return true, ""
		}
		return false, name + "=" + value
	}
	return false, ""
}
