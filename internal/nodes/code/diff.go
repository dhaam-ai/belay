package code

import (
	"fmt"
	"strings"
)

// diffArtifactName returns the artifact name for the diff proposed at
// step.
//
// Numbering by graph.RunContext.Step rather than by a fixed name is what
// keeps a fix loop's history intact: every pass through the code node runs
// at a later step, so attempt one's diff is still on disk when attempt two
// writes its own. Zero padding keeps the artifacts directory sorted the
// way the run actually happened.
func diffArtifactName(step int) string { return fmt.Sprintf("diff-%04d.patch", step) }

// extractDiff pulls the unified diff out of an agent's reply, or returns
// "" if the reply contains none.
//
// The prompt asks for a fenced block tagged "diff", which is what a
// cooperative backend produces; the bare-diff fallback exists because a
// model that forgets the fence has still done the work, and throwing that
// away would cost a paid invocation to recover something already in hand.
func extractDiff(text string) string {
	if block, ok := fencedDiff(text); ok {
		return block
	}
	return bareDiff(text)
}

// fencedDiff returns the contents of the first non-empty ```diff (or
// ```patch) block in text.
func fencedDiff(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		if !isDiffFenceOpen(lines[i]) {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if isFenceClose(lines[j]) {
				end = j
				break
			}
		}
		if block, ok := joinBlock(lines[i+1 : end]); ok {
			return block, true
		}
		i = end
	}
	return "", false
}

// isDiffFenceOpen reports whether line opens a diff block.
//
// Leading whitespace is tolerated here but not in isFenceClose, and the
// asymmetry is deliberate: an indented opening fence is a common model
// formatting habit, whereas every line inside a unified diff body carries
// a leading marker (' ', '+', '-', '@'), so only a closing fence can start
// with a backtick in column zero. Trimming the closer too would let a diff
// that patches a Markdown file terminate its own block early.
func isDiffFenceOpen(line string) bool {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, diffFence) {
		return false
	}
	info := strings.ToLower(strings.TrimSpace(strings.TrimLeft(s, "`")))
	return info == "diff" || info == "patch"
}

// isFenceClose reports whether line is a closing fence in column zero.
func isFenceClose(line string) bool {
	if !strings.HasPrefix(line, diffFence) {
		return false
	}
	return strings.TrimSpace(strings.TrimLeft(line, "`")) == ""
}

// bareDiff returns text from its first unified-diff header onward, for a
// reply that emitted a diff without fencing it.
func bareDiff(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		isGitHeader := strings.HasPrefix(line, "diff --git ")
		isUnifiedHeader := strings.HasPrefix(line, "--- ") &&
			i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ")
		if isGitHeader || isUnifiedHeader {
			block, _ := joinBlock(lines[i:])
			return block
		}
	}
	return ""
}

// joinBlock rejoins lines, reporting false if they hold nothing but
// whitespace. The result always ends in exactly one newline, because a
// patch without a trailing newline is one `git apply` refuses.
func joinBlock(lines []string) (string, bool) {
	block := strings.TrimRight(strings.Join(lines, "\n"), " \t\n")
	if strings.TrimSpace(block) == "" {
		return "", false
	}
	return block + "\n", true
}

// changedFiles lists, in first-appearance order, the workspace-relative
// paths diff touches. The result is never nil: zero changed files is a
// fact worth recording as an empty list rather than a null.
//
// It reads the b-side of every header, so a rename records its new name
// and a deletion still records the file it removed (its "+++" line is
// /dev/null, but its "diff --git" header is not).
func changedFiles(diff string) []string {
	out := make([]string, 0, 4)
	seen := make(map[string]struct{})
	add := func(path string, ok bool) {
		if !ok {
			return
		}
		if _, dup := seen[path]; dup {
			return
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}

	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			add(gitHeaderPath(line))
		case strings.HasPrefix(line, "+++ "):
			add(unifiedHeaderPath(strings.TrimPrefix(line, "+++ ")))
		}
	}
	return out
}

// gitHeaderPath returns the b-side path of a "diff --git a/x b/x" header.
func gitHeaderPath(line string) (string, bool) {
	rest := strings.TrimPrefix(line, "diff --git ")
	// Split on the " b/" separator rather than on whitespace so a path
	// containing a space still resolves.
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return normalizeDiffPath(rest[i+1:])
	}
	// git diff --no-prefix emits no a/ and b/ markers; the b-side is
	// then simply the last field.
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", false
	}
	return normalizeDiffPath(fields[len(fields)-1])
}

// unifiedHeaderPath returns the path from the body of a "+++ " header,
// dropping the timestamp POSIX diff appends after a tab.
func unifiedHeaderPath(s string) (string, bool) {
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	return normalizeDiffPath(s)
}

// normalizeDiffPath strips a header path down to a workspace-relative
// path, reporting false for the /dev/null placeholder a creation or
// deletion uses on one side.
func normalizeDiffPath(p string) (string, bool) {
	p = strings.Trim(strings.TrimSpace(p), `"`)
	// Exactly one "b/" is removed: the a/ and b/ convention means a real
	// file named b/main.go appears in a header as b/b/main.go.
	p = strings.TrimPrefix(p, "b/")
	if p == "" || p == "/dev/null" {
		return "", false
	}
	return p, true
}
