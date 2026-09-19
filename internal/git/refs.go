//go:build unix

package git

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidRef reports a name that cannot be one component of a git ref.
//
// Recover the reason from the error text: a caller cannot do anything
// different for a trailing dot than for a space, but the person who typed
// the id needs to be told which rule they hit.
var ErrInvalidRef = errors.New("git: invalid ref component")

// maxRefComponent is the longest component this package will build a ref
// from. git itself has no limit, but the files ref backend stores a ref as a
// file whose name is the component, and NAME_MAX is 255 on every filesystem
// belay runs on. A longer name fails inside git with an errno, after the
// caller has already been told the id was fine.
const maxRefComponent = 255

// refForbidden are the characters git refuses anywhere in a ref name, plus
// the backslash. Each has a meaning to git's revision parser that would turn
// a name into a different request: "x~1" is a commit, "x^" is a parent,
// "a:b" is a refspec.
const refForbidden = " ~^:?*[\\"

// ValidateRefComponent reports whether s is usable as ONE component of a ref
// name — the "cand-01-a1" in refs/heads/belay/<run>-cand-01-a1.
//
// It is a pure-Go mirror of the rules `git check-ref-format` applies, and it
// exists so a bad id is refused before anything is created. Without it the
// first thing that notices is git itself, halfway through a sequence that
// has already made a branch, and unwinding that is strictly harder than not
// starting.
//
// The rules it shares with git: a component may not be empty, begin with a
// dot, end with a dot, contain "..", contain "@{", or contain a space, a
// control character, DEL, or any of ~ ^ : ? * [ \.
//
// It is deliberately stricter than git in four places, and
// TestValidateRefComponentAgreesWithGit pins every one of them:
//
//   - A slash is refused. git would accept it and quietly create a nested
//     ref, which is the D/F conflict this naming scheme exists to avoid.
//   - ".lock" is refused case-insensitively. git compares exactly, but
//     macOS's default filesystem does not, so "x.LOCK" and git's own
//     "x.lock" are one file there.
//   - "@" alone is refused. git only refuses it as a whole refname, but a
//     component that is the one name git reserves is not worth the saving.
//   - A leading "-" is refused, because any git command handed the name
//     without a "--" guard would read it as an option.
//
// It returns an error wrapping ErrInvalidRef naming the rule that was hit.
func ValidateRefComponent(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%w: a ref component may not be empty", ErrInvalidRef)
	case len(s) > maxRefComponent:
		return fmt.Errorf("%w: %d bytes is longer than the %d a ref file name may be", ErrInvalidRef, len(s), maxRefComponent)
	case strings.HasPrefix(s, "."):
		return fmt.Errorf("%w: %q begins with a dot", ErrInvalidRef, s)
	case strings.HasSuffix(s, "."):
		return fmt.Errorf("%w: %q ends with a dot", ErrInvalidRef, s)
	case strings.Contains(s, ".."):
		return fmt.Errorf("%w: %q contains %q", ErrInvalidRef, s, "..")
	case strings.HasSuffix(strings.ToLower(s), ".lock"):
		return fmt.Errorf("%w: %q ends with .lock, which is how git names its own lock files", ErrInvalidRef, s)
	case strings.Contains(s, "@{"):
		return fmt.Errorf("%w: %q contains %q, which git reads as a reflog selector", ErrInvalidRef, s, "@{")
	case s == "@":
		return fmt.Errorf("%w: %q is a name git reserves", ErrInvalidRef, s)
	case strings.HasPrefix(s, "-"):
		return fmt.Errorf("%w: %q begins with a dash, which git would read as an option", ErrInvalidRef, s)
	case strings.Contains(s, "/"):
		return fmt.Errorf("%w: %q contains a slash, and this is one ref component, not a path", ErrInvalidRef, s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c < 0x20 || c == 0x7f:
			return fmt.Errorf("%w: %q contains the control character %#x at byte %d", ErrInvalidRef, s, c, i)
		case strings.IndexByte(refForbidden, c) >= 0:
			return fmt.Errorf("%w: %q contains %q, which git forbids in a ref name", ErrInvalidRef, s, string(c))
		}
	}
	return nil
}
