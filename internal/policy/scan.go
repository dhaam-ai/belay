package policy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"github.com/dhaam-ai/belay/internal/glob"
)

// maxLineBytes is how much of one line is held in memory while a changed file
// is scanned.
//
// A line longer than this is a minified bundle or a generated blob, not
// something a person wrote, and holding an arbitrary one would let a single
// changed file decide how much memory belay uses. The cap is not silently
// applied to a content rule: scanChange refuses rather than matching half a
// line and reporting a pass, because "we could not look" must never be
// recorded as "we looked and it was fine".
const maxLineBytes = 1 << 20 // 1 MiB

// scanned is what one pass over the change's files found.
type scanned struct {
	// lines is how many lines the files the change touches hold together.
	lines int
	// content is every forbidden-content violation, in path order.
	content []Violation
}

// contentMatcher is one compiled ContentRule.
type contentMatcher struct {
	rule  ContentRule
	re    *regexp.Regexp
	paths glob.Set
}

// covers reports whether this rule applies to the changed file rel.
func (m contentMatcher) covers(rel string) bool {
	if m.paths.Len() == 0 {
		return true
	}
	_, hit := m.paths.Match(rel)
	return hit
}

// reason renders what a person is told when this rule fires.
func (m contentMatcher) reason() string {
	if r := strings.TrimSpace(m.rule.Reason); r != "" {
		return r
	}
	return fmt.Sprintf("it matches the policy.forbidden_content pattern %q", m.rule.Pattern)
}

// scanChange reads each changed file at most once and reports what the rules
// that need its contents found.
//
// Reading is skipped entirely unless a rule asks for it, so a policy of
// nothing but path globs costs no I/O at all — which matters because this
// runs on every pass of the fix loop, not once.
//
// Files are opened through an os.Root rooted at the workspace, for the reason
// the write node gives: a change set names paths an agent chose, and
// following a symbolic link out of the workspace would let a rule be
// satisfied, or a secret be read, from a file the run has nothing to do with.
func scanChange(ctx context.Context, p Policy, ch Change, files []string) (scanned, error) {
	matchers, err := compileContent(p.ForbiddenContent, ch.FoldCase)
	if err != nil {
		return scanned{}, err
	}
	countLines := p.MaxChangedLines > 0
	if !countLines && len(matchers) == 0 {
		return scanned{}, nil
	}

	root, err := os.OpenRoot(ch.Workspace)
	if err != nil {
		return scanned{}, fmt.Errorf("policy: open the workspace %s: %w", ch.Workspace, err)
	}
	defer func() { _ = root.Close() }()

	out := scanned{}
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return scanned{}, fmt.Errorf("policy: %w", err)
		}
		lines, hits, err := scanFile(root, rel, matchers)
		if err != nil {
			return scanned{}, err
		}
		out.lines += lines
		out.content = append(out.content, hits...)
	}
	return out, nil
}

// compileContent compiles every content rule, naming the setting a bad one
// came from.
func compileContent(rules []ContentRule, fold bool) ([]contentMatcher, error) {
	out := make([]contentMatcher, 0, len(rules))
	for _, rule := range rules {
		re, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return nil, fmt.Errorf("policy: policy.forbidden_content: %q is not a valid regular expression: %w",
				rule.Pattern, err)
		}
		paths, err := compileSet("policy.forbidden_content.paths", rule.Paths, fold)
		if err != nil {
			return nil, err
		}
		out = append(out, contentMatcher{rule: rule, re: re, paths: paths})
	}
	return out, nil
}

// scanFile counts rel's lines and reports every content rule it breaks.
//
// A path that is not there is a deletion, which is not an error: the write
// node records a deleted file in the change set, and a file that is gone has
// no lines and no content to match. It contributes nothing rather than
// stopping the gate.
//
// At most one violation is reported per rule per file. A pattern that matches
// forty lines of one file is one thing to fix, and forty copies of the same
// sentence would crowd the genuinely different violations out of both the
// journal note and the repair prompt.
func scanFile(root *os.Root, rel string, matchers []contentMatcher) (int, []Violation, error) {
	f, err := root.Open(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, nil, nil
	case err != nil:
		return 0, nil, fmt.Errorf("policy: read the changed file %s: %w", rel, err)
	}
	defer func() { _ = f.Close() }()

	// Statted after opening, through the handle, so what is measured is what
	// was opened: a directory or a device node in a change set has no lines
	// and no text, and is not something to read to the end of.
	info, err := f.Stat()
	if err != nil {
		return 0, nil, fmt.Errorf("policy: inspect the changed file %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return 0, nil, nil
	}

	var (
		lines  int
		hits   []Violation
		fired  = make(map[int]bool, len(matchers))
		reader = bufio.NewReader(f)
	)
	for {
		line, truncated, err := readLine(reader)
		if errors.Is(err, io.EOF) && line == "" {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, nil, fmt.Errorf("policy: read the changed file %s: %w", rel, err)
		}
		lines++
		if truncated && len(matchers) > 0 {
			return 0, nil, fmt.Errorf("policy: %s has a line longer than %d bytes, so belay cannot "+
				"check it against policy.forbidden_content; exclude it with the rule's paths, or "+
				"add it to policy.forbidden_paths", rel, maxLineBytes)
		}
		for i, m := range matchers {
			if fired[i] || !m.covers(rel) || !m.re.MatchString(line) {
				continue
			}
			fired[i] = true
			hits = append(hits, Violation{
				Rule: RuleForbiddenContent, Class: RuleForbiddenContent.Class(),
				Path: rel, Line: lines,
				Message: fmt.Sprintf("%s:%d is forbidden content: %s", rel, lines, m.reason()),
			})
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return lines, hits, nil
}

// readLine returns the next line without its terminator, and whether it was
// cut short at maxLineBytes.
//
// It reads through ReadSlice rather than ReadString so that the bytes held at
// once are the reader's buffer and not the whole line: the cap has to bound
// memory, and a function that assembles a 400 MiB line before deciding it is
// too long has not bounded anything. The rest of an over-long line is read
// and discarded, so the reader is always left at the start of the next one.
func readLine(r *bufio.Reader) (string, bool, error) {
	var (
		b         strings.Builder
		truncated bool
	)
	for {
		chunk, err := r.ReadSlice('\n')
		text := strings.TrimSuffix(string(chunk), "\n")
		if room := maxLineBytes - b.Len(); room >= len(text) {
			b.WriteString(text)
		} else {
			if room > 0 {
				b.WriteString(text[:room])
			}
			truncated = true
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return strings.TrimSuffix(b.String(), "\r"), truncated, err
	}
}
