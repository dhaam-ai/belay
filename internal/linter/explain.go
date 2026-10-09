//go:build unix

package linter

import (
	"fmt"

	"github.com/dhaam-ai/belay/internal/detect"
)

// Absence says why no adapter in this package would lint a directory, in the
// two halves a blocker needs: what belay found, and what to change.
type Absence struct {
	// Problem says what belay found, and why that leaves the lint gate with
	// nothing to run.
	Problem string
	// Fix says what to change, as a lowercase imperative clause.
	Fix string
}

// lintModeNote opens every Problem, because Explain is only ever the answer
// to one question: review.mode asked for a lint gate, and there is none.
const lintModeNote = `review.mode is "lint", `

// sonarFix is the way out every Fix ends with: a quality gate that needs no
// linter configured in the repository.
const sonarFix = `set review.mode to "sonar" in belay.yaml`

// Explain says why none of GolangCI, ESLint and Ruff would lint dir. missing
// is false, with a zero Absence, when one of them would, and there is nothing
// to explain.
//
// It exists because belay used to give one reason for every such directory,
// that it "recognises no Go, Node or Python project" there. For the commonest
// case that is false: belay recognised a Python project, the same one its test
// runner would run pytest in, and found no ruff configuration. ruff only lints
// a project that declares or configures it, so a reader told the project was
// not recognised looks for the wrong fix. Explain names what was found and
// the one change that gives the gate something to run.
//
// Whether an adapter would lint dir is decided by the adapters' own Detect
// methods, the ones the CLI chooses by, so this cannot drift from them. It
// reads the directory and starts no process.
func Explain(dir string) (why Absence, missing bool) {
	for _, l := range []interface{ Detect(string) bool }{NewGolangCI(), NewESLint(), NewRuff()} {
		if l.Detect(dir) {
			return Absence{}, false
		}
	}

	var node, python bool
	for _, p := range projects(dir) {
		switch p.Kind {
		case detect.KindNode:
			node = true
		case detect.KindPython:
			python = true
		case detect.KindGo, detect.KindUnknown:
		}
	}

	const (
		eslintFix = "add eslint to devDependencies with an eslint.config.js"
		ruffFix   = "add a [tool.ruff] table to pyproject.toml, or a ruff.toml, to lint with ruff " +
			"(an empty one uses ruff's default rules, and belay counts only the lines a run changes)"
	)
	switch {
	case node && python:
		return Absence{
			Problem: fmt.Sprintf(lintModeNote+"and belay found a Node project and a Python project in %s, "+
				"but neither configures the linter belay runs for it (ESLint for Node, ruff for Python), "+
				"so the lint gate has nothing to run.", dir),
			Fix: eslintFix + "; or " + ruffFix + "; or " + sonarFix,
		}, true
	case python:
		return Absence{
			Problem: fmt.Sprintf(lintModeNote+"and belay found a Python project in %s, "+
				"but nothing in it configures ruff, the linter belay runs for Python, "+
				"so the lint gate has nothing to run.", dir),
			Fix: ruffFix + "; or " + sonarFix,
		}, true
	case node:
		return Absence{
			Problem: fmt.Sprintf(lintModeNote+"and belay found a Node project in %s, "+
				"but it neither depends on nor configures ESLint, the linter belay runs for Node, "+
				"so the lint gate has nothing to run.", dir),
			Fix: eslintFix + "; or " + sonarFix,
		}, true
	default:
		// Word for word what the CLI has always said here, so taking every
		// blocker from Explain changes nothing for a directory that holds
		// no project at all.
		return Absence{
			Problem: fmt.Sprintf(lintModeNote+"but belay recognises no Go, Node or Python project in %s to lint.", dir),
			Fix:     "point --workspace at the repository, or " + sonarFix,
		}, true
	}
}
