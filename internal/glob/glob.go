// Package glob matches workspace-relative paths against the path globs a
// person writes in belay.yaml.
//
// It exists because path.Match, the obvious answer, cannot cross a separator:
// path.Match("**/*.go", "internal/nodes/main.go") is false, and no pattern it
// understands says "a Go file anywhere". A policy whose forbidden_paths could
// only name one directory level would be a guardrail with a hole in it, and a
// guardrail with a hole is worse than none, because somebody is relying on it.
//
// # The rules, and why they are the strict ones
//
// A pattern is slash-separated and anchored at the workspace root. It is
// matched segment by segment: "*", "?" and a character class match inside one
// segment, exactly as path.Match defines them, and "**" is the only thing that
// crosses a separator, standing for zero or more whole segments. So
// "infra/**" matches infra, infra/main.tf and infra/aws/main.tf, and nothing
// outside infra; "*.go" matches main.go and not internal/main.go.
//
// The alternative, gitignore's rule that a pattern with no slash matches a
// file of that name at any depth, was deliberately not taken. It makes the
// meaning of a pattern depend on whether it happens to contain a slash, which
// is the kind of rule people get wrong in the direction that matters here:
// "secrets.yaml" would protect every secrets.yaml in the tree, while
// "config/secrets.yaml" would protect exactly one, and nothing in the file
// says so. One rule that always reads the same way is worth the extra two
// characters of "**/secrets.yaml".
//
// # A pattern that cannot mean anything is refused
//
// Compile refuses an empty pattern, an absolute one, one containing "..", and
// one gluing "**" to other characters in a segment ("src**/x"). None of them
// can ever match a cleaned, workspace-relative path, so honouring them would
// mean a rule that silently matches nothing — the exact failure this package
// exists to prevent. They are refused where they are written, when the
// configuration loads, rather than where they would have fired.
package glob

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// ErrBadPattern reports a pattern belay cannot honour. Every error this
// package returns wraps it, so a caller collecting configuration problems can
// recognise one with errors.Is.
var ErrBadPattern = errors.New("glob: bad pattern")

// Pattern is one compiled path glob.
//
// The zero Pattern matches nothing and renders as the empty string. It is
// what a failed Set.Match returns, so a caller that ignores the second result
// gets a pattern that cannot be mistaken for one that fired.
type Pattern struct {
	raw  string
	segs []string
}

// Compile returns the compiled form of pattern, or an error wrapping
// ErrBadPattern. See the package documentation for what is refused.
func Compile(pattern string) (Pattern, error) {
	raw := strings.TrimSpace(pattern)
	if raw == "" {
		return Pattern{}, fmt.Errorf("%w: a pattern cannot be empty", ErrBadPattern)
	}
	if strings.HasPrefix(raw, "/") {
		return Pattern{}, fmt.Errorf("%w: %q is absolute, and belay matches paths relative to the workspace", ErrBadPattern, pattern)
	}
	// Split before cleaning, never after. path.Clean("src/../etc") is "etc",
	// so a cleaned pattern has already lost the ".." this refuses — and the
	// person who wrote it would have got a rule quietly aimed somewhere else.
	segs := make([]string, 0, strings.Count(raw, "/")+1)
	for _, seg := range strings.Split(strings.TrimSuffix(raw, "/"), "/") {
		switch seg {
		case "..":
			return Pattern{}, fmt.Errorf("%w: %q leaves the workspace, so it can never match a file belay would change", ErrBadPattern, pattern)
		case "", ".":
			// "./src" and "src//main.go" name the path "src/main.go" does.
			continue
		}
		segs = append(segs, seg)
	}
	if len(segs) == 0 {
		return Pattern{}, fmt.Errorf("%w: %q names no path", ErrBadPattern, pattern)
	}
	for _, seg := range segs {
		switch {
		case seg == "**":
			continue
		case strings.Contains(seg, "**"):
			return Pattern{}, fmt.Errorf("%w: %q glues ** to other characters in %q; ** stands for whole path segments, so write it on its own", ErrBadPattern, pattern, seg)
		}
		// path.Match reports a malformed character class only when it has a
		// name to match against, so it is asked here with one that cannot
		// short-circuit the scan.
		// Both errors are wrapped: a caller branching on ErrBadPattern is the
		// common case, and path.ErrBadPattern stays reachable for anything
		// that wants to know the segment itself was the malformed part.
		if _, err := path.Match(seg, ""); err != nil {
			return Pattern{}, fmt.Errorf("%w: %q: %w", ErrBadPattern, pattern, err)
		}
	}
	return Pattern{raw: raw, segs: segs}, nil
}

// MustCompile is Compile for a pattern fixed in belay's own source, and
// panics if it does not compile. It is never called on anything a person
// wrote.
func MustCompile(pattern string) Pattern {
	p, err := Compile(pattern)
	if err != nil {
		panic(err)
	}
	return p
}

// String returns the pattern as it was written, which is what a violation
// quotes back to the person who wrote it.
func (p Pattern) String() string { return p.raw }

// Match reports whether the workspace-relative path name matches p.
//
// name is cleaned first — a leading "./" and a trailing "/" change nothing —
// so a caller may pass a path exactly as some other part of belay recorded it
// without having to know which form that was.
func (p Pattern) Match(name string) bool {
	if len(p.segs) == 0 {
		return false
	}
	clean := strings.TrimSuffix(strings.TrimPrefix(path.Clean(strings.TrimSpace(name)), "./"), "/")
	if clean == "" || clean == "." {
		return false
	}
	return match(p.segs, strings.Split(clean, "/"))
}

// match reports whether the path segments in name satisfy the pattern
// segments in pat.
//
// The "**" case branches rather than looping greedily: a greedy "**" would
// have to know how many segments the rest of the pattern needs, which stops
// being knowable as soon as the rest contains another "**". Trying every
// split is O(segments²) on a pattern with one "**" and is bounded by path
// depth, which is small; correctness is worth more here than the saving.
func match(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if match(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		// The error is discarded because Compile already rejected every
		// segment path.Match cannot parse, and a segment cannot change
		// afterwards: Pattern's fields are unexported and never written
		// outside Compile.
		ok, _ := path.Match(pat[0], name[0])
		if !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// Set is an ordered group of patterns, which is what a configuration field
// holding a list of globs becomes.
//
// The zero Set matches nothing, which is what an unset policy field must do.
type Set struct{ patterns []Pattern }

// CompileAll compiles every pattern in patterns, in order, and fails on the
// first one it cannot.
//
// It fails rather than collecting, because the caller collecting
// configuration problems is internal/config, which reports the field and the
// first thing wrong with it; a list with two bad globs is one field to fix.
func CompileAll(patterns []string) (Set, error) {
	out := make([]Pattern, 0, len(patterns))
	for _, raw := range patterns {
		p, err := Compile(raw)
		if err != nil {
			return Set{}, err
		}
		out = append(out, p)
	}
	return Set{patterns: out}, nil
}

// Len returns how many patterns s holds.
func (s Set) Len() int { return len(s.patterns) }

// Match returns the first pattern in s that name matches, and whether one
// did.
//
// It returns the pattern rather than a bool because a violation has to name
// the rule that fired: "infra/aws/main.tf is forbidden by **/*.tf" is
// actionable, and "that path is forbidden" sends a person reading their whole
// belay.yaml to find out which line did it.
func (s Set) Match(name string) (Pattern, bool) {
	for _, p := range s.patterns {
		if p.Match(name) {
			return p, true
		}
	}
	return Pattern{}, false
}
