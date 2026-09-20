//go:build unix

package git

import (
	"context"
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

// validateBranch reports whether name is usable as a whole branch name, such
// as "belay/<run-id>": every slash-separated component must pass
// ValidateRefComponent, which also rules out an empty component (a leading,
// trailing or doubled slash).
//
// Two whole names are refused on top of that. "HEAD" is refused by git
// itself as a branch name. A name starting with "refs/" is accepted by git,
// which would create refs/heads/refs/heads/x; a caller passing one meant the
// branch x, and a silent double prefix is a branch nobody can find.
func validateBranch(name string) error {
	switch {
	case name == "HEAD":
		return fmt.Errorf("%w: %q is not a branch name", ErrInvalidRef, name)
	case strings.HasPrefix(name, "refs/"):
		return fmt.Errorf("%w: %q is a ref, and this takes a branch name without refs/heads/", ErrInvalidRef, name)
	}
	for component := range strings.SplitSeq(name, "/") {
		if err := ValidateRefComponent(component); err != nil {
			return fmt.Errorf("branch %q: %w", name, err)
		}
	}
	return nil
}

// BranchTip returns the commit the branch name points at, and false when
// there is no such branch.
//
// The full ref, refs/heads/<name>, is what is asked about. A bare name would
// go through git's revision lookup, where a tag of the same name wins, and a
// caller comparing tips before deleting a branch must be told the branch's
// tip and nothing else.
func (r *Repo) BranchTip(ctx context.Context, name string) (string, bool, error) {
	if err := validateBranch(name); err != nil {
		return "", false, err
	}
	res, err := r.run(ctx, r.top, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	if code, exited := exitCode(err); exited && code == 1 {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(res.Stdout), true, nil
}

// Branch sentinels. Match them with errors.Is.
var (
	// ErrBranchExists reports that CreateBranch found the branch already
	// there. It is never moved: an existing branch may hold someone's work.
	ErrBranchExists = errors.New("git: the branch already exists")

	// ErrRefConflict reports a branch name that cannot exist beside a branch
	// that already does, because refs are stored as paths: refs/heads/belay
	// and refs/heads/belay/run1 would need "belay" to be both a file and a
	// directory. The error names the branch in the way.
	ErrRefConflict = errors.New("git: the branch name conflicts with an existing branch")

	// ErrBranchMoved reports that DeleteBranch found the branch pointing
	// somewhere other than the caller expected, and left it alone.
	ErrBranchMoved = errors.New("git: the branch no longer points where it was expected to")

	// ErrCheckedOut reports that DeleteBranch was asked to delete a branch
	// some worktree has checked out.
	ErrCheckedOut = errors.New("git: the branch is checked out in a worktree")

	// ErrInvalidObjectID reports a value that had to be a full object ID
	// and was not.
	ErrInvalidObjectID = errors.New("git: not a full object id")
)

// CreateBranch creates the branch name at commit, and refuses when a branch
// by that name exists (ErrBranchExists) or when an existing branch's name
// makes it impossible (ErrRefConflict). It never moves an existing branch.
//
// commit may be anything that names a commit — a full or abbreviated object
// ID, a branch, HEAD — and is resolved first, so the branch records the full
// ID and something that names a tree or nothing at all is refused before a
// ref is written.
//
// The ref is written with `update-ref <ref> <new> <zero-id>`, whose all-zero
// old value means "only if it does not exist": git checks and writes under
// one lock, so a branch created by someone else in the meantime is refused,
// not overwritten. The conflict check before it is what turns git's refusal
// into an answer that names the branch in the way, without reading git's
// message.
func (r *Repo) CreateBranch(ctx context.Context, name, commit string) error {
	if err := validateBranch(name); err != nil {
		return err
	}
	oid, err := r.resolveCommit(ctx, commit)
	if err != nil {
		return err
	}
	if err := r.refConflict(ctx, name); err != nil {
		return err
	}
	zero := strings.Repeat("0", len(oid))
	_, err = r.run(ctx, r.top, "update-ref", "-m", "belay: create branch", "refs/heads/"+name, oid, zero)
	if err == nil {
		return nil
	}
	// The conflict check passed, so the likeliest cause is the branch
	// appearing in between. Say so when it did, rather than reading git's
	// message to find out.
	if _, ok, tipErr := r.BranchTip(ctx, name); tipErr == nil && ok {
		return fmt.Errorf("%w: %s", ErrBranchExists, name)
	}
	return err
}

// resolveCommit returns the full object ID of the commit rev names.
func (r *Repo) resolveCommit(ctx context.Context, rev string) (string, error) {
	if rev == "" {
		return "", errors.New("git: no commit given")
	}
	// --end-of-options keeps a rev that starts with a dash from being read
	// as an option; ^{commit} refuses anything that is not a commit, such
	// as a tree.
	res, err := r.run(ctx, r.top, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if code, exited := exitCode(err); exited && code == 1 {
		return "", fmt.Errorf("git: %q does not name a commit", rev)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// refConflict reports whether a branch that already exists makes name
// impossible: the branch itself, a branch named after one of its parent
// directories, or a branch nested under it.
//
// It asks for-each-ref once. A pattern matches a ref exactly or as a
// directory prefix up to a slash, so "refs/heads/<name>" finds the branch
// and everything under it, and each parent pattern finds the parent and
// its whole directory, of which only the parent itself is in the way.
// Ref names cannot hold a newline, so the list is split on newlines.
func (r *Repo) refConflict(ctx context.Context, name string) error {
	full := "refs/heads/" + name
	args := []string{"for-each-ref", "--format=%(refname)", full}
	parents := make(map[string]bool)
	for i := strings.Index(name, "/"); i >= 0; i = nextSlash(name, i) {
		parent := "refs/heads/" + name[:i]
		parents[parent] = true
		args = append(args, parent)
	}
	res, err := r.run(ctx, r.top, args...)
	if err != nil {
		return err
	}
	for ref := range strings.SplitSeq(strings.TrimSuffix(res.Stdout, "\n"), "\n") {
		switch {
		case ref == full:
			return fmt.Errorf("%w: %s", ErrBranchExists, name)
		case strings.HasPrefix(ref, full+"/"):
			return fmt.Errorf("%w: %s cannot be created while %s exists", ErrRefConflict, full, ref)
		case parents[ref]:
			return fmt.Errorf("%w: %s cannot be created while %s exists", ErrRefConflict, full, ref)
		}
	}
	return nil
}

// nextSlash returns the index of the next slash in s after index i, or -1.
func nextSlash(s string, i int) int {
	j := strings.Index(s[i+1:], "/")
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

// DeleteBranch deletes the branch name, but only while it still points at
// expect, the full object ID the caller last saw it at. A branch that has
// moved since is left alone (ErrBranchMoved): whatever moved it may be
// work. A branch that is already gone is not an error, so a cleanup that is
// run again converges.
//
// It refuses two things `git update-ref -d` would happily do, both found by
// running it:
//
//   - An expect that is not a full object ID (ErrInvalidObjectID). The
//     all-zero ID, like an empty one, tells update-ref to check nothing at
//     all, so either would make this an unconditional delete.
//   - A branch some worktree has checked out (ErrCheckedOut), the main one
//     included. update-ref is plumbing and does not look, and the worktree
//     is left on a branch that no longer exists, where every file shows as
//     a new one. `git branch -D` refuses the same thing.
func (r *Repo) DeleteBranch(ctx context.Context, name, expect string) error {
	if err := validateBranch(name); err != nil {
		return err
	}
	if !isObjectID(expect) {
		return fmt.Errorf("%w: deleting %s needs the full commit it is expected to point at, got %q", ErrInvalidObjectID, name, expect)
	}
	list, err := r.Worktrees(ctx)
	if err != nil {
		return err
	}
	for _, w := range list {
		if w.Branch == name {
			return fmt.Errorf("%w: %s is checked out at %s", ErrCheckedOut, name, w.Path)
		}
	}

	_, err = r.run(ctx, r.top, "update-ref", "-d", "refs/heads/"+name, expect)
	if err == nil {
		return nil
	}
	// update-ref exits 1 both when the branch is gone and when it moved.
	// Where the branch is now tells the two apart without reading the
	// message.
	tip, ok, tipErr := r.BranchTip(ctx, name)
	switch {
	case tipErr != nil:
		return err
	case !ok:
		return nil
	case tip != expect:
		return fmt.Errorf("%w: %s is at %s, not %s", ErrBranchMoved, name, tip, expect)
	default:
		return err
	}
}

// isObjectID reports whether s is a full SHA-1 or SHA-256 object ID other
// than the all-zero one, in the lowercase hex git prints.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	zero := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '0':
		case c >= '1' && c <= '9', c >= 'a' && c <= 'f':
			zero = false
		default:
			return false
		}
	}
	return !zero
}

// ContainedElsewhere reports whether any ref other than the branch name
// itself — another branch, a remote-tracking branch or a tag — contains the
// branch's tip. It is the question to ask before deleting a branch: when it
// is true, the branch holds no commit that deleting it would lose.
//
// It returns ErrNoBranch when there is no such branch. The branch never
// counts as containing itself, which is the one thing an implementation
// built on `for-each-ref --contains` gets wrong by default: the listing
// always includes the branch, so "contained" would be true for every
// branch, and a cleanup trusting it would delete unique work.
//
// Worktree HEADs and the reflog are deliberately not counted. A detached
// worktree that happens to hold the commits is not a place anyone would
// look for them, and a reflog entry expires.
func (r *Repo) ContainedElsewhere(ctx context.Context, name string) (bool, error) {
	tip, ok, err := r.BranchTip(ctx, name)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrNoBranch, name)
	}
	res, err := r.run(ctx, r.top, "for-each-ref", "--format=%(refname)", "--contains", tip,
		"refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return false, err
	}
	self := "refs/heads/" + name
	for ref := range strings.SplitSeq(strings.TrimSuffix(res.Stdout, "\n"), "\n") {
		if ref != "" && ref != self {
			return true, nil
		}
	}
	return false, nil
}
