package code

import (
	"path/filepath"
	"strings"
)

// changeSet resolves the set of repository-relative paths the reply reports
// as changed.
//
// # Precedence
//
// The explicit changed-files block is the primary source and the only one
// the prompt asks for. The diff fallback exists because a backend that
// volunteers a unified diff has still told us which files it touched, and
// harvesting those paths is better than recording nothing and letting the
// write node verify an empty set — but it is a salvage path, not a supported
// contract, and it is never consulted while a usable block is present:
//
//  1. The first non-empty fenced block tagged changedFilesTag wins outright.
//  2. Otherwise, paths are recovered from a unified diff in the reply, fenced
//     or bare (see extractDiff).
//  3. Otherwise the reply reports no change, which is a valid outcome: the
//     caller carries the previously recorded change forward rather than
//     zeroing it.
//
// A block that is present but holds no entries is treated as absent, so the
// fallback still runs. That mirrors fencedDiff, which also steps over an
// empty block rather than reading it as a report — an empty block is a
// formatting accident far more often than it is a considered claim, and the
// honest reading is "this reply did not answer", not "this reply said none".
//
// Every path, from either source, goes through the same validation. Paths
// recovered from a diff are not trusted more than declared ones: both end up
// in state.Code.ChangedFiles, and the write node verifies both identically.
//
// The returned slice is nil when nothing was reported, so the caller can
// distinguish "reported nothing" from "reported an empty set".
func changeSet(text string) ([]string, error) {
	if raw, ok := changedFilesBlock(text); ok {
		paths, err := cleanPaths(raw, sourceBlock)
		if err != nil {
			return nil, err
		}
		if len(paths) > 0 {
			return paths, nil
		}
	}
	diff := extractDiff(text)
	if diff == "" {
		return nil, nil
	}
	return cleanPaths(changedFiles(diff), sourceDiff)
}

// Where a claimed path came from, for the error message. A malformed entry
// in a block the prompt explicitly asked for is a different diagnosis from
// one harvested out of a volunteered diff, and a post-mortem should not have
// to guess which happened.
const (
	sourceBlock = "the " + changedFilesTag + " block"
	sourceDiff  = "the unified diff in the reply"
)

// changedFilesBlock returns the raw lines of the first non-empty fenced block
// tagged changedFilesTag, and whether one was found.
//
// Only the first is read. A reply carrying two such blocks has not followed
// an instruction that says "exactly one", and concatenating them would invent
// a change set neither block claims.
func changedFilesBlock(text string) ([]string, bool) {
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		if !isChangedFilesFenceOpen(lines[i]) {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if isFenceClose(lines[j]) {
				end = j
				break
			}
		}
		if body := lines[i+1 : end]; len(body) > 0 {
			return body, true
		}
		i = end
	}
	return nil, false
}

// isChangedFilesFenceOpen reports whether line opens a changed-files block.
//
// Leading whitespace is tolerated, matching isDiffFenceOpen: an indented
// opening fence is a common model formatting habit. The tag comparison is
// case-insensitive for the same reason.
func isChangedFilesFenceOpen(line string) bool {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, fence) {
		return false
	}
	info := strings.ToLower(strings.TrimSpace(strings.TrimLeft(s, "`")))
	return info == changedFilesTag
}

// cleanPaths validates and normalizes claimed paths, rejecting the whole set
// if any single entry is not a repository-relative path.
//
// Refusing the set rather than dropping the offending entry matches the write
// node, which refuses a change set on one bad path for the same reason: a
// path that escapes the workspace is not noise to be filtered but evidence
// that the report does not describe what happened, and a run continuing on a
// filtered version of it would record a half-truth as fact. Failing here also
// fails earlier and more legibly than letting write's containment check
// produce the refusal several steps later.
//
// Blank lines are skipped rather than rejected. They are whitespace a model
// left between entries, not claims about files — unlike, say, "/etc/passwd"
// or "../escape.go", which are claims, and wrong ones.
//
// Order of first appearance is preserved and duplicates are dropped, so the
// result is a set rather than a tally. The result is never nil for a non-empty
// input, because "zero changed files" is a fact worth recording as an empty
// list rather than a null.
func cleanPaths(raw []string, source string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, entry := range raw {
		path, ok, err := cleanPath(entry, source)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out, nil
}

// unquote strips one matching pair of surrounding quotes.
//
// The prompt tells the agent not to quote paths, and a quoted path is
// nonetheless a common habit rather than a false claim about a file — so it
// is normalized, in the same spirit as trimming whitespace. What that must
// not do is launder an empty entry: `""` unquotes to "", which cleanPath
// then refuses rather than skipping, because unlike a blank line it is an
// entry that asserts a file with no name.
func unquote(s string) string {
	for _, q := range []string{`"`, "'"} {
		if len(s) >= 2 && strings.HasPrefix(s, q) && strings.HasSuffix(s, q) {
			return strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	return s
}

// cleanPath validates one claimed entry.
//
// It reports (path, true, nil) for a usable path, ("", false, nil) for a line
// that is only whitespace and carries no claim, and an error for an entry
// that claims something the write node would refuse.
//
// The rules are deliberately a subset of write.safeJoin's, checked here so
// the failure names the reply that produced it. This node cannot repeat
// write's later checks — whether the path is a regular file, whether a
// symlink resolves out of the tree — because those are facts about the disk
// at the moment write reads it, not about the string.
func cleanPath(entry, source string) (string, bool, error) {
	trimmed := unquote(strings.TrimSpace(entry))
	if trimmed == "" {
		// Nothing but whitespace, or a pair of empty quotes. The first is
		// formatting between entries and carries no claim; the second is an
		// entry that claims a file with no name, and unquote has already
		// reduced it to the same thing. Both are skipped only when the raw
		// line was blank — a quoted empty string is refused below.
		if strings.TrimSpace(entry) == "" {
			return "", false, nil
		}
		return "", false, &FileListError{Path: entry, Source: source, Reason: "is empty"}
	}

	refuse := func(reason string) (string, bool, error) {
		return "", false, &FileListError{Path: entry, Source: source, Reason: reason}
	}
	switch {
	case strings.ContainsRune(trimmed, 0):
		return refuse("contains a NUL byte")
	case filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/"):
		return refuse("is an absolute path, but paths must be repository-relative")
	case strings.Contains(trimmed, `\`):
		// A backslash is a path separator on one of Go's target platforms
		// and a legal filename byte on the others, so the same string names
		// different files depending on where it is read. write refuses it;
		// refusing it here says so against the reply that wrote it.
		return refuse("contains a backslash")
	}

	// Clean before inspecting segments so "a/./b" and "a//b" normalize, and
	// so a traversal that only cancels out ("a/../b") is accepted as the "b"
	// it denotes rather than refused for containing "..".
	path := filepath.ToSlash(filepath.Clean(trimmed))
	switch {
	case path == "." || path == "":
		return refuse("does not name a file")
	case path == ".." || strings.HasPrefix(path, "../"):
		return refuse(`escapes the repository root with a ".." segment`)
	}
	return path, true, nil
}
