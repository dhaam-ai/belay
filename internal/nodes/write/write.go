package write

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/state"
)

// Node is belay's write node. It verifies the change the code node claims to
// have made against the workspace on disk, records it as a numbered patch
// artifact, and routes to the test node.
//
// It never modifies the workspace. See the package documentation for why that
// is the design rather than a limitation.
//
// The zero Node is usable and behaves exactly as New() with no options: the
// workspace root is derived from the run Layout and files are quoted up to
// DefaultMaxFileBytes. A *Node holds no mutable state and is safe for
// concurrent use, which best-of-N fanout requires of every registered node.
type Node struct {
	root         string
	maxFileBytes int64
}

// Node implements graph.Node. Asserted at compile time so a signature drift
// in the graph contract is a build failure here rather than a nil entry in
// the dispatcher's registry at runtime.
var _ graph.Node = (*Node)(nil)

// Option configures a Node.
type Option func(*Node)

// WithWorkspaceRoot pins the directory the node treats as the workspace,
// instead of deriving it from the run Layout.
//
// The default derivation reads the workspace out of the run directory's own
// path (see rootFromLayout), which is correct for an ordinary run. Wiring
// that puts a node's workspace somewhere else — a fanout candidate's isolated
// copy, a test fixture — must say so explicitly rather than hope the
// derivation guesses right, because the workspace root is the boundary every
// other safety property in this package is measured against.
func WithWorkspaceRoot(dir string) Option {
	return func(n *Node) { n.root = dir }
}

// WithMaxFileBytes caps how much of a single file the patch artifact quotes.
// A file over the cap is recorded by reference instead. Zero or negative
// selects DefaultMaxFileBytes.
func WithMaxFileBytes(limit int64) Option {
	return func(n *Node) { n.maxFileBytes = limit }
}

// New returns a write node configured by opts.
func New(opts ...Option) *Node {
	n := &Node{}
	for _, opt := range opts {
		opt(n)
	}
	return n
}

// Name returns graph.NodeWrite.
func (n *Node) Name() string { return graph.NodeWrite }

// Run verifies the change set and records it.
//
// # Outcomes
//
// A verified change set — including an empty one — is a success: Run writes
// the patch artifact, returns a state.Patch whose Code carries the artifact's
// run-relative path and the verified file list, and routes to
// graph.NodeTest. An empty change set means the agent proposed nothing; the
// artifact is zero bytes, and the test node then runs against the workspace
// exactly as it stands, which normally reproduces the previous test result.
// That repetition is the signal the fix loop needs in order to notice it is
// making no progress, so it is recorded plainly rather than dressed up as an
// error.
//
// A change set naming a path that cannot be proven to lie inside the
// workspace is a failure, not an error: Run returns
// (Result{Status: journal.StatusFailed}, nil) having written nothing. Per the
// graph contract a non-nil error means "no verdict was reachable", and a
// refusal is very much a verdict — the change set is untrustworthy and the
// run must stop.
//
// A non-nil error is reserved for the cases where Run genuinely could not
// decide: an underivable or unusable workspace root, an I/O failure, or a
// cancelled context.
//
// # Re-running
//
// Run is safe to re-run, which ADR-0002 requires because the dispatcher
// re-executes any node whose node_started record has no matching
// node_finished. It touches nothing in the workspace, and the artifact it
// writes is a deterministic function of the change set and the bytes on disk,
// so a second execution at the same step rewrites the same artifact with the
// same content and leaves the tree exactly as the first found it.
func (n *Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc == nil {
		return graph.Result{}, errors.New("write: nil RunContext")
	}
	if err := ctx.Err(); err != nil {
		return graph.Result{}, fmt.Errorf("write: %w", err)
	}

	root, rootPath, err := n.openWorkspace(rc)
	if err != nil {
		return graph.Result{}, err
	}
	defer func() { _ = root.Close() }()
	log := n.logger(rc).With(slog.String("workspace", rootPath))

	changes, patch, err := n.materialize(ctx, root, rootPath, rc.State.Code.ChangedFiles)
	if err != nil {
		if errors.Is(err, ErrRefused) {
			log.Warn("write: refused the change set", slog.String("error", err.Error()))
			return graph.Result{
				Status: journal.StatusFailed,
				Note:   sanitizeNote("refused the change set: " + err.Error()),
			}, nil
		}
		return graph.Result{}, err
	}

	name, err := artifactName(rc.Step)
	if err != nil {
		return graph.Result{}, err
	}
	rel, err := rc.WriteArtifact(name, patch)
	if err != nil {
		return graph.Result{}, fmt.Errorf("write: record %s: %w", name, err)
	}

	code := rc.State.Code
	code.LastDiff = rel
	code.ChangedFiles = paths(changes)

	present, absent := tally(changes)
	log.Info("write: recorded the change set",
		slog.Int("present", present),
		slog.Int("absent", absent),
		slog.Int("patch_bytes", len(patch)),
		slog.String("artifact", rel),
	)

	return graph.Result{
		Next:   graph.NodeTest,
		Status: journal.StatusOK,
		Patch:  state.Patch{Code: &code},
		Note:   sanitizeNote(note(present, absent, rel)),
	}, nil
}

// materialize verifies the claimed change set and renders its patch.
//
// The two steps are one operation because a refusal can come out of either:
// containment is decided when a path is inspected, and again when it is read,
// because the tree belongs to an agent process that may still be changing it.
// Keeping them together means Run has one place to recognise a refusal rather
// than two that must be remembered to stay in step.
func (n *Node) materialize(ctx context.Context, root *os.Root, rootPath string, claimed []string) ([]change, []byte, error) {
	changes, err := verify(ctx, root, rootPath, claimed)
	if err != nil {
		return nil, nil, err
	}
	patch, err := renderPatch(ctx, root, rootPath, changes, n.limit())
	if err != nil {
		return nil, nil, err
	}
	return changes, patch, nil
}

// openWorkspace resolves the directory this execution is bounded to and opens
// it as an os.Root, returning both the Root and its resolved path. The caller
// owns closing the Root.
func (n *Node) openWorkspace(rc *graph.RunContext) (*os.Root, string, error) {
	if strings.TrimSpace(n.root) != "" {
		return openRoot(n.root)
	}
	dir, err := rootFromLayout(rc.Layout)
	if err != nil {
		return nil, "", err
	}
	return openRoot(dir)
}

// limit returns the configured per-file quoting cap.
func (n *Node) limit() int64 {
	if n.maxFileBytes > 0 {
		return n.maxFileBytes
	}
	return DefaultMaxFileBytes
}

// logger returns rc's logger, or a discarding default when the dispatcher did
// not scope one. A node must not panic on a partially populated RunContext.
func (n *Node) logger(rc *graph.RunContext) *slog.Logger {
	if rc.Logger != nil {
		return rc.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// tally counts how many changes were found on disk and how many were not.
func tally(changes []change) (present, absent int) {
	for _, c := range changes {
		if c.Kind == changePresent {
			present++
			continue
		}
		absent++
	}
	return present, absent
}

// note composes the human-readable summary recorded in the journal.
func note(present, absent int, artifact string) string {
	if present == 0 && absent == 0 {
		return "no files changed; recorded an empty " + artifact
	}
	return fmt.Sprintf("verified %d changed file(s): %d present, %d deleted; recorded %s",
		present+absent, present, absent, artifact)
}

// maxNoteRunes bounds a Note. A refusal quotes a path that came from model
// output, and the journal is a line-oriented file a human reads.
const maxNoteRunes = 240

// sanitizeNote makes a Note safe to record.
//
// graph.Result documents Note as the node's own responsibility to redact, and
// a refusal's Note necessarily quotes a path an agent chose. Control
// characters are collapsed to spaces so a crafted path cannot forge structure
// in anything that renders the journal as text, invalid UTF-8 is replaced,
// and the result is bounded so one enormous path cannot dominate the record.
func sanitizeNote(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	count := 0
	for _, r := range s {
		if count >= maxNoteRunes {
			b.WriteString("...")
			break
		}
		switch {
		case r == utf8.RuneError:
			b.WriteRune('�')
		case unicode.IsControl(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
		count++
	}
	return strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
}
