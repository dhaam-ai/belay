//go:build unix

package linter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// This file holds what every adapter that scopes its findings to a run's
// change asks git first: whether git can say what the change is at all.
//
// The question is the same whichever tool is linting. A run's change is the
// difference between the working tree and HEAD, so scoping needs a HEAD that
// holds the linted directory, and a directory whose new files git does not
// ignore. Where either is missing, scoping by HEAD would hide every finding
// the run adds, so the adapter must count every finding instead and say so.

// Bounds for the git commands the scope check runs. Each is quick, and the
// longest output is one directory level of names; the cap only has to leave
// that output non-empty.
const (
	gitCheckTimeout         = 30 * time.Second
	gitCheckMaxOutput int64 = 64 << 10
)

// unscopable returns why git cannot scope findings in dir to the run's
// change, or "" when it can.
//
// probe is a file name the check asks git's ignore rules about, standing in
// for a file the run adds to dir. It should carry the extension of the files
// the adapter lints, and a name no repository is expected to ignore, so that
// only the rules covering the directory decide.
func (b base) unscopable(ctx context.Context, dir, probe string) string {
	// ls-tree lists dir's entries in HEAD, relative to dir. It prints
	// nothing for a directory the repository ignores or has not committed,
	// and fails with no repository or no commit.
	res, err := b.git(ctx, dir, "ls-tree", "--name-only", "HEAD")
	switch {
	case err != nil:
		return err.Error()
	case strings.TrimSpace(res.Stdout) == "":
		return "HEAD holds nothing in this directory: git ignores it, or it is not committed yet"
	}

	// HEAD can hold a file in a directory whose other files git ignores,
	// such as ws/.gitkeep under "ws/*". git never reports an ignored file
	// as changed, so ask whether a new file here would be ignored.
	// check-ignore exits 0 when it would and 1 when it would not;
	// --no-index answers from the ignore rules alone.
	_, err = b.git(ctx, dir, "check-ignore", "--quiet", "--no-index", "--", probe)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return "git ignores new files in this directory"
	case errors.As(err, &exitErr) && exitErr.Code == 1:
		return ""
	default:
		return err.Error()
	}
}

// git runs one git command in dir, with the base environment only.
func (b base) git(ctx context.Context, dir string, args ...string) (exec.Result, error) {
	return b.gitCapped(ctx, dir, gitCheckMaxOutput, args...)
}

// gitCapped is git with a capture limit of the caller's choosing, for a
// command whose output grows with the change rather than with one directory
// listing.
func (b base) gitCapped(ctx context.Context, dir string, maxOutput int64, args ...string) (exec.Result, error) {
	return b.runner.Run(ctx, exec.Command{
		Path:      "git",
		Args:      args,
		Dir:       dir,
		Timeout:   min(gitCheckTimeout, b.timeout),
		MaxOutput: maxOutput,
	})
}

// gitDiffArgs is the command line a change's lines are read from: the working
// tree against HEAD, relative to the linted directory, with no context lines.
//
// Every flag pins something a repository or a user could otherwise set
// underneath the parser: colour, an external diff driver, a text conversion
// filter, rename detection and the path prefixes. Renames are reported as a
// deletion and an addition, so a moved file counts as new in full: that can
// only make the gate count more, never hide what a run moved.
var gitDiffArgs = []string{
	"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
	"--src-prefix=a/", "--dst-prefix=b/", "--unified=0", "--relative", "HEAD", "--", ".",
}

// gitUntrackedArgs lists the files a run created and has not added to the
// index, relative to the linted directory, leaving out the ones the
// repository ignores. -z prints each path verbatim, so none needs unquoting.
var gitUntrackedArgs = []string{"ls-files", "--others", "--exclude-standard", "-z", "--", "."}

// changes returns what a run changed in dir, or the reason git cannot say.
//
// A non-empty reason means the caller must count every finding. That covers
// everything unscopable reports, and three failures of its own: a git command
// that fails, one whose output was cut off at the capture limit, and a diff
// this parser cannot read. Each would otherwise yield a change set smaller
// than the change, and a finding the run made would vanish from the gate.
func (b base) changes(ctx context.Context, dir, probe string) (changeSet, string) {
	if reason := b.unscopable(ctx, dir, probe); reason != "" {
		return changeSet{}, reason
	}
	diff, err := b.gitCapped(ctx, dir, DefaultMaxOutput, gitDiffArgs...)
	switch {
	case err != nil:
		return changeSet{}, err.Error()
	case diff.Truncated:
		return changeSet{}, "git diff printed more than belay captures"
	}
	set, err := parseChangedLines(diff.Stdout)
	if err != nil {
		return changeSet{}, err.Error()
	}
	files, err := b.gitCapped(ctx, dir, DefaultMaxOutput, gitUntrackedArgs...)
	switch {
	case err != nil:
		return changeSet{}, err.Error()
	case files.Truncated:
		return changeSet{}, "git ls-files printed more than belay captures"
	}
	for _, path := range strings.Split(files.Stdout, "\x00") {
		if path != "" {
			set.whole[path] = true
		}
	}
	return set, ""
}

// lineSpan is the new-side lines one hunk added or rewrote, first to last
// inclusive.
type lineSpan struct{ first, last int }

// changeSet is a run's change in one directory, as git reports it. Paths are
// slash-separated and relative to the linted directory.
type changeSet struct {
	// lines holds, per file, the lines the change added or rewrote.
	lines map[string][]lineSpan
	// touched holds every file the diff names on its new side, including
	// one whose hunks only removed lines.
	touched map[string]bool
	// whole holds the files the run created and has not added to the
	// index, every line of which is new.
	whole map[string]bool
}

// newChangeSet returns an empty set, ready to fill.
func newChangeSet() changeSet {
	return changeSet{lines: map[string][]lineSpan{}, touched: map[string]bool{}, whole: map[string]bool{}}
}

// covers reports whether line of file is part of the change.
func (c changeSet) covers(file string, line int) bool {
	if c.whole[file] {
		return true
	}
	for _, s := range c.lines[file] {
		if line >= s.first && line <= s.last {
			return true
		}
	}
	return false
}

// touches reports whether the change touched file at all.
func (c changeSet) touches(file string) bool { return c.whole[file] || c.touched[file] }

// counts reports whether issue belongs to the change and so to the gate.
//
// A finding counts when it sits on a line the change added or rewrote. Two
// kinds of finding have no line that can be trusted to say where the change
// is, and count anywhere in a file the change touched: one with no line at
// all, and one saying the tool could not read or parse the file. A syntax
// error is reported where the parser gave up, which is often not where the
// edit that caused it is -- a deleted closing bracket is reported at the end
// of the file.
//
// The same two kinds do NOT count in a file the change left alone. That is a
// deliberate difference from golangci-lint, which reports a package that does
// not compile wherever it is: Go has a build step a broken file would stop,
// and Python does not. Python repositories commit files that do not parse on
// purpose, as test data for the code that parses them, and failing every run
// in such a repository would be failing it for something it did not do.
//
// A finding whose path belay could not place inside the linted directory
// (relPath left it absolute) cannot be matched against the change at all, so
// it counts. The alternative is a finding that vanishes from the gate because
// belay failed to read its path, which is the one failure this must not have.
func (c changeSet) counts(issue belay.Issue) bool {
	if filepath.IsAbs(issue.File) {
		return true
	}
	file := filepath.ToSlash(issue.File)
	if issue.Line <= 0 || unparsable(issue) {
		return c.touches(file)
	}
	return c.covers(file, issue.Line)
}

// unparsable reports whether issue says the tool could not read or parse the
// whole file, rather than finding something on one line of it: ruff's E9
// family (E902 for a file it cannot read, E999 for one it cannot parse), or a
// diagnostic whose message is a SyntaxError.
func unparsable(issue belay.Issue) bool {
	letters, digits := splitRuffCode(strings.ToUpper(strings.TrimSpace(issue.RuleID)))
	return (letters == "E" && strings.HasPrefix(digits, "9")) || strings.HasPrefix(issue.Message, "SyntaxError")
}

// scoped wraps parse so that only the issues set counts reach the report.
//
// The raw document stays the tool's own, so it still holds every finding the
// tool printed; the issues, the counts and the gate are the change's.
func scoped(parse parser, set changeSet) parser {
	return func(stdout []byte) ([]belay.Issue, json.RawMessage, error) {
		issues, raw, err := parse(stdout)
		if err != nil {
			return issues, raw, err
		}
		kept := make([]belay.Issue, 0, len(issues))
		for _, issue := range issues {
			if set.counts(issue) {
				kept = append(kept, issue)
			}
		}
		return kept, raw, nil
	}
}

// hunkHeader matches a unified diff hunk header, "@@ -a,b +c,d @@", in which
// either count may be left out to mean one line.
var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// parseChangedLines reads the zero-context unified diff gitDiffArgs prints
// into the lines each file gained.
//
// It walks each hunk by its counts rather than by the first character of a
// line, because an added line whose text begins "++ b/" reads as a file
// header otherwise. Anything it cannot account for is an error, never a
// smaller change: see changes.
func parseChangedLines(diff string) (changeSet, error) {
	set := newChangeSet()
	file, haveFile := "", false
	oldLeft, newLeft := 0, 0
	for line := range strings.Lines(diff) {
		line = strings.TrimSuffix(line, "\n")
		if oldLeft > 0 || newLeft > 0 {
			switch {
			case strings.HasPrefix(line, "\\"):
				// "\ No newline at end of file" belongs to the line above it.
			case strings.HasPrefix(line, "-") && oldLeft > 0:
				oldLeft--
			case strings.HasPrefix(line, "+") && newLeft > 0:
				newLeft--
			default:
				return changeSet{}, fmt.Errorf("belay/linter: a hunk in %q ended early at %q", file, line)
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			file, haveFile = "", false
		case strings.HasPrefix(line, "+++ "):
			path, err := diffPath(strings.TrimPrefix(line, "+++ "))
			if err != nil {
				return changeSet{}, err
			}
			file, haveFile = path, true
			if file != "" {
				set.touched[file] = true
			}
		case strings.HasPrefix(line, "@@"):
			if !haveFile {
				return changeSet{}, fmt.Errorf("belay/linter: a hunk before any file header: %q", line)
			}
			first, added, removed, err := parseHunkHeader(line)
			if err != nil {
				return changeSet{}, err
			}
			if file != "" && added > 0 {
				set.lines[file] = append(set.lines[file], lineSpan{first: first, last: first + added - 1})
			}
			oldLeft, newLeft = removed, added
		}
	}
	if oldLeft > 0 || newLeft > 0 {
		return changeSet{}, fmt.Errorf("belay/linter: the diff of %q ended inside a hunk", file)
	}
	return set, nil
}

// parseHunkHeader returns where a hunk's new-side lines start and how many
// lines it adds and removes.
func parseHunkHeader(line string) (first, added, removed int, err error) {
	m := hunkHeader.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, 0, fmt.Errorf("belay/linter: unreadable hunk header %q", line)
	}
	count := func(s string) (int, error) {
		if s == "" {
			return 1, nil
		}
		return strconv.Atoi(s)
	}
	if removed, err = count(m[2]); err != nil {
		return 0, 0, 0, fmt.Errorf("belay/linter: hunk header %q: %w", line, err)
	}
	if first, err = strconv.Atoi(m[3]); err != nil {
		return 0, 0, 0, fmt.Errorf("belay/linter: hunk header %q: %w", line, err)
	}
	if added, err = count(m[4]); err != nil {
		return 0, 0, 0, fmt.Errorf("belay/linter: hunk header %q: %w", line, err)
	}
	return first, added, removed, nil
}

// diffPath turns the name on a "+++ " line into a path relative to the
// linted directory, or "" for a file the change deleted.
//
// git quotes a name holding a byte outside printable ASCII, a quote or a
// backslash, with C-style escapes that strconv.Unquote reads, and follows a
// name holding a space with a tab, for GNU patch's sake.
func diffPath(name string) (string, error) {
	name = strings.TrimSuffix(name, "\t")
	if name == "/dev/null" {
		return "", nil
	}
	if strings.HasPrefix(name, `"`) {
		unquoted, err := strconv.Unquote(name)
		if err != nil {
			return "", fmt.Errorf("belay/linter: unreadable quoted path %s: %w", name, err)
		}
		name = unquoted
	}
	path, ok := strings.CutPrefix(name, "b/")
	if !ok {
		return "", fmt.Errorf("belay/linter: path %q lacks the b/ prefix gitDiffArgs asks for", name)
	}
	return path, nil
}
