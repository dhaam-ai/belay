//go:build unix

package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dhaam-ai/belay/internal/vcs"
)

// ErrInvalidTrailer reports a trailer [Repo.Carries] cannot look for: one
// that could not be written by [Repo.Commit] in the first place, or one whose
// key would change what git reads the --format atom to be.
//
// Recover the reason from the error text. A caller cannot do anything
// different for an empty value than for a key holding a comma, but whoever
// built the trailer needs to be told which rule they hit.
var ErrInvalidTrailer = errors.New("git: invalid trailer")

// Tip returns the commit the branch checked out at dir points to, and
// ErrNoCommits when that branch has none yet.
//
// dir names a working tree, not the repository: a run works on its own branch
// in its own worktree, and the commit the user's checkout is on is a
// different one. The directory is proven to be a working tree of this
// repository first (see verifyWorktree), for the same reason the writing
// calls do it — an agent can rewrite the .git file in the workspace it works
// in, and a tip read out of another repository would be recorded as this
// run's promotion.
func (r *Repo) Tip(ctx context.Context, dir string) (string, error) {
	if err := r.verifyWorktree(ctx, dir); err != nil {
		return "", err
	}
	head, err := r.headAt(ctx, dir)
	if err != nil {
		return "", err
	}
	return head.Commit, nil
}

// Carries reports whether any commit in the history of the branch checked out
// at dir carries every one of trailers, all of them on that one commit.
//
// It is the question a promotion asks before it merges: has this run's winner
// already been promoted? The trailers are matched together rather than one at
// a time because a candidate ID repeats from run to run, so an earlier run's
// merged work would answer "yes" to the candidate's trailer alone, and the
// merge this run still owes would be skipped.
//
// # It reads git's trailers, not the message
//
// The values come from `git log --format` with the %(trailers:...) atom, so
// what counts as a trailer is what git's own parser says is one. Searching
// the message text — a --grep, a scan of %B — would match the same line in a
// paragraph of prose, in a quoted diff, or in the body of a commit that
// reverted the promotion, and each of those would be read as a promotion that
// happened.
//
// The format asks for one field per trailer, values only and unfolded, with a
// record separator in front of each. So a commit with none of them costs a
// few bytes, and the whole history of a large repository still fits in the
// output this package captures. Two properties make the fields unambiguous: a
// trailer value cannot hold a line feed once it is unfolded, which is why the
// values of one key are read off the default line-feed separator; and a value
// holding the field separator itself would change the field count, which is
// checked on every record rather than assumed.
//
// Matching is git's on the key, which is case-insensitive, and exact on the
// value apart from surrounding whitespace, which is invisible in a commit
// message and must not decide the answer.
//
// A trailer that could not have been committed, or whose key would change
// where git reads the atom to end, is refused with ErrInvalidTrailer rather
// than asked about; see validateQuery.
func (r *Repo) Carries(ctx context.Context, dir string, trailers ...vcs.Trailer) (bool, error) {
	if err := validateQuery(trailers); err != nil {
		return false, err
	}
	// Tip proves the working tree is this repository's and resolves the
	// commit to walk from. Naming the commit rather than HEAD also keeps the
	// walk off whatever a later argument might have made HEAD mean.
	tip, err := r.Tip(ctx, dir)
	if err != nil {
		return false, err
	}

	atoms := make([]string, len(trailers))
	var format strings.Builder
	for i, t := range trailers {
		atoms[i] = trailersAtom(t.Key)
		format.WriteString(fieldSeparator)
		format.WriteString(atoms[i])
	}

	// -z ends each commit's record with a NUL, and `--` ends the revisions so
	// nothing here can be read as a path.
	res, err := r.run(ctx, dir, "log", "-z", "--format="+format.String(), tip, "--")
	if err != nil {
		return false, err
	}

	for record := range strings.SplitSeq(res.Stdout, "\x00") {
		// Every record begins with a separator, so the only empty one is
		// what follows the NUL that ends the last commit.
		if record == "" {
			continue
		}
		fields := strings.Split(record, fieldSeparator)
		if len(fields) != len(trailers)+1 || fields[0] != "" {
			return false, fmt.Errorf("git: cannot read the trailers of a commit in %s: %q", dir, record)
		}
		carries := true
		for i, t := range trailers {
			if fields[i+1] == atoms[i] {
				return false, fmt.Errorf("%w: this git printed %s back instead of expanding it, so it cannot answer what a commit's trailers are",
					ErrTooOld, atoms[i])
			}
			if !hasValue(fields[i+1], t.Value) {
				carries = false
				break
			}
		}
		if carries {
			return true, nil
		}
	}
	return false, nil
}

// fieldSeparator goes in front of every trailer's field in the --format, so a
// record has one more field than there are trailers and the first is always
// empty. Without the leading one, a single-trailer commit with no trailers
// would be an empty record, which is exactly what follows the last NUL.
//
// It is the ASCII record separator, which no commit message anyone writes
// holds. A value that held one anyway would make the record split into too
// many fields, which is refused rather than read.
const fieldSeparator = "\x1e"

// trailersAtom is the --format atom that prints the values of one trailer
// key, and nothing else.
//
// key= is matched case-insensitively by git and turns on "only", so
// non-trailer lines of the trailer block are left out. valueonly drops the
// key, whose case would otherwise have to be compared here. unfold joins a
// value split over several lines into one, which is what makes a line feed a
// safe separator between the values of one key.
//
// Every one of those options is older than [MinVersion], which Open refuses
// to run under, so the atom always expands in production. Carries checks that
// it did anyway: a git that does not know an option prints the atom back
// verbatim rather than failing.
func trailersAtom(key string) string {
	return "%(trailers:key=" + key + ",valueonly,unfold)"
}

// hasValue reports whether field, the values of one trailer key on one
// commit, holds want.
//
// git writes one value per line and ends the last one with a line feed too,
// so the split has an empty element at the end; an empty want is refused by
// validateQuery, which is what keeps that element from matching a commit that
// has no such trailer at all.
func hasValue(field, want string) bool {
	want = strings.TrimSpace(want)
	for value := range strings.SplitSeq(field, "\n") {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

// validateQuery refuses a trailer Carries must not ask git about.
//
// Two of the rules are about the atom, and both were found by running git.
// A key holding a comma makes git read the rest of the key as the next option
// of the atom, which it does not know, so the whole atom is printed back
// verbatim. A key holding ")" ends the atom early and the rest becomes
// literal text in the format. Either would turn the question into a different
// one.
//
// The rest are about trailers themselves. An empty key names nothing. A key
// holding a colon or a value holding a line break could not be one trailer in
// a commit message, and validateMessage refuses to commit either. And an
// empty value is refused because git prints an empty field both for a trailer
// whose value is empty and for a commit that has no such trailer at all, so
// asking for one would answer "yes" for every commit in the history.
func validateQuery(trailers []vcs.Trailer) error {
	if len(trailers) == 0 {
		return fmt.Errorf("%w: no trailer to look for", ErrInvalidTrailer)
	}
	for _, t := range trailers {
		switch {
		case strings.TrimSpace(t.Key) == "":
			return fmt.Errorf("%w: a trailer to look for has no key", ErrInvalidTrailer)
		case strings.ContainsAny(t.Key, ",)"):
			return fmt.Errorf("%w: key %q holds a character git would read as part of the format, not as the key", ErrInvalidTrailer, t.Key)
		case strings.ContainsAny(t.Key, "\n\r:"):
			return fmt.Errorf("%w: key %q could not be one trailer's key", ErrInvalidTrailer, t.Key)
		case strings.TrimSpace(t.Value) == "":
			return fmt.Errorf("%w: trailer %q has no value, which every commit without the trailer would match", ErrInvalidTrailer, t.Key)
		case strings.ContainsAny(t.Value, "\n\r"):
			return fmt.Errorf("%w: trailer %q: %q holds a line break, so it could not be one trailer's value", ErrInvalidTrailer, t.Key, t.Value)
		}
	}
	return nil
}
