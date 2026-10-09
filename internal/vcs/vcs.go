// Package vcs names what belay needs from version control to promote a run's
// change: commit a workspace, integrate one line of work into another, list
// the files two commits differ in, and list what a working tree changed.
//
// # Types only
//
// This package holds the Repository contract and the plain data it passes,
// and nothing else. It starts no process and has no build tag. Nothing
// implements Repository yet: it lands ahead of its first implementation, a
// git one, so the node that promotes a change and the package that drives
// git can be built against it at the same time. The one behaviour here is
// Message.Text, because every implementation must write a commit message
// the same way.
//
// # Why it is not in pkg/belay
//
// pkg/belay is the only package with a compatibility promise, and an
// interface exported there with one implementation is a seam no second
// implementation has tested. ADR-0005 records that cost for AgentBackend:
// with only Claude Code behind it, "CLI-agnostic" is a design promise, not a
// proven fact. Repository's only planned implementation is git, so exporting
// it would promise that same untested seam. It moves to pkg/belay when a
// second version control system needs it, and not before.
//
// # Why it is neither internal/graph nor the git package
//
// It is not in internal/graph because it is shaped like git (commits,
// trailers, merging one branch into another), and the graph knows nothing
// about version control. It is not the git package itself because the graph
// is to hand every node a Repository on its run context, and the git package
// will run git through internal/exec, which builds only on unix. internal/graph
// has no build tag and must not gain one to name a type. A node's tests also
// need to fake a repository without starting git. An interface in a package
// with no build tag and no dependencies serves both.
package vcs

import (
	"context"
	"errors"
	"strings"
)

// ErrDirtyTarget reports a working tree with changes the caller did not make
// and has not committed, which the operation would otherwise sweep into a
// commit of its own.
//
// It is here, and not only in the package that drives git, for one reason:
// the node that promotes a run's change has to tell this apart from every
// other failure, and it cannot import that package. Promoting is the step a
// person lands in when they finish a conflicting merge by hand in the run's
// worktree, and stopping half way -- conflicts resolved, nothing committed --
// is ordinary. Reported as an error it ends the run, and a run that has ended
// cannot be resumed; recognised here it pauses, and `belay resume` finishes
// the moment they commit.
//
// An implementation returns an error wrapping it, with the directory and what
// is uncommitted, so errors.Is answers the question and the message still
// says which tree.
var ErrDirtyTarget = errors.New("vcs: the working tree has uncommitted changes")

// Repository is the version control a promotion needs.
//
// Every method must converge when it is run again: the dispatcher re-runs a
// node whose start has no matching finish, and a node that promotes a change
// learns what was already done by asking the repository, not by remembering.
type Repository interface {
	// Commit records every change in the working tree at dir as one commit
	// with msg as its message, and returns that commit's ID. When dir has
	// nothing to commit, Commit makes no commit and returns the commit dir
	// already has checked out, so a re-run does not commit twice.
	Commit(ctx context.Context, dir string, msg Message) (string, error)

	// Integrate brings the work on from into the branch checked out at dir,
	// as one commit with msg as its message. When that branch already
	// contains from, it makes no commit and returns the branch's tip.
	//
	// A conflict is not an error: Integrate returns an Integration whose
	// Conflicts names the conflicting files and leaves dir unchanged, so a
	// human can decide what to keep. An error means Integrate could not tell
	// whether the work integrates at all.
	Integrate(ctx context.Context, dir, from string, msg Message) (Integration, error)

	// Changed returns the files that differ between the commits from and
	// to, as paths relative to the repository's top directory.
	Changed(ctx context.Context, from, to string) ([]string, error)

	// Changes returns every file in the working tree at dir, and below it,
	// that differs from the commit base, committed or not, as paths
	// relative to dir: everything a commit of that tree would carry beyond
	// base. base must be a full commit id.
	//
	// It is what lets promote commit exactly what the run recorded and
	// judged, and refuse a change no step of the run accounted for.
	Changes(ctx context.Context, dir, base string) ([]string, error)
}

// Message is a commit message, held as its parts so every Repository writes
// it the same way. Text is the only way to turn it into the text a commit
// carries.
type Message struct {
	// Subject is the message's first line.
	Subject string
	// Body is free prose after the subject, and may be empty.
	Body string
	// Trailers are the key-value lines at the end of the message, in the
	// order they are written.
	Trailers []Trailer
}

// Trailer is one "Key: Value" line in the trailer block git reads at the end
// of a commit message, such as a run's ID.
type Trailer struct {
	// Key names the trailer.
	Key string
	// Value is the trailer's value.
	Value string
}

// Integration is what Repository.Integrate did.
type Integration struct {
	// Tip is the commit the branch at dir points to afterwards. When
	// Conflicts is not empty nothing was integrated and Tip is the tip the
	// branch already had.
	Tip string
	// Conflicts lists the files that could not be integrated, and is empty
	// when the work integrated cleanly.
	Conflicts []string
}

// Text returns m as the text of a commit message: the subject, a blank line,
// the body, a blank line, and one "Key: Value" line per trailer. An empty
// body or an empty trailer list leaves out its paragraph and the blank line
// before it, and the text ends without a newline. git commit's default
// message cleanup adds the final newline a commit message carries.
//
// Subject and Body are trimmed of surrounding whitespace first, so a Body
// that is only whitespace counts as empty, and the layout holds however the
// caller built the parts: one blank line between two paragraphs and none at
// the end. Trailers go last because git reads trailers only from a message's
// last paragraph.
//
// Trailers are written in the order given, not sorted: the caller chooses
// their order, and the same Message always gives the same text.
//
// Text formats and does not validate. It does not refuse an empty Subject,
// or a Key or Value holding a newline, which would break the trailer block.
// Refusing such a Message is the job of the Repository asked to commit it.
func (m Message) Text() string {
	paragraphs := []string{strings.TrimSpace(m.Subject)}
	if body := strings.TrimSpace(m.Body); body != "" {
		paragraphs = append(paragraphs, body)
	}
	if len(m.Trailers) > 0 {
		lines := make([]string, len(m.Trailers))
		for i, t := range m.Trailers {
			lines[i] = t.Key + ": " + t.Value
		}
		paragraphs = append(paragraphs, strings.Join(lines, "\n"))
	}
	return strings.Join(paragraphs, "\n\n")
}
