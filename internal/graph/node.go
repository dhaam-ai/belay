// Package graph defines the contract every belay node implements and the
// registry the dispatcher resolves node names through.
//
// The central invariant, from ADR 0002, is that a node is a pure function of
// its inputs: it reads a State snapshot and its adapters, and it returns a
// Result describing what changed and where to go next. It never writes
// state.json, manifest.json, or the journal. The dispatcher owns all three.
//
// That is what makes crash recovery correct rather than hopeful. Because a
// node persists nothing, re-running one after a crash cannot half-apply an
// update, and every node is a table-testable (state, adapters) -> Result
// function with no filesystem in the way.
//
// This package deliberately contains types only. The dispatcher lives in
// internal/graph/dispatcher.go and the nodes in internal/nodes/*.
package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
)

// End is the Result.Next value that terminates a run successfully. It is not
// a registrable node name; Registry.Register rejects it.
const End = "END"

// The canonical node names. Every node's Name method and every Result.Next
// must use these constants rather than a string literal, because the
// dispatcher resolves Next through Registry by exact match: a node that calls
// itself "Code" while another routes to "code" produces an unknown-node error
// at runtime, not a compile error.
const (
	NodePlan    = "plan"
	NodeApprove = "approve"
	NodeCode    = "code"
	NodeWrite   = "write"
	NodeTest    = "test"
	NodeFix     = "fix"
	NodeReview  = "review"
	NodeFanout  = "fanout"
	NodeJoin    = "join"
)

// Errors returned by this package.
var (
	// ErrUnknownNode means Result.Next named a node the Registry has no
	// entry for. It is always a wiring bug, never a user error.
	ErrUnknownNode = errors.New("graph: unknown node")
	// ErrDuplicateNode means two nodes registered under one name.
	ErrDuplicateNode = errors.New("graph: duplicate node name")
	// ErrInvalidNodeName means a name was empty or collided with End.
	ErrInvalidNodeName = errors.New("graph: invalid node name")
	// ErrInvalidResult means a node returned a Result the dispatcher
	// cannot act on — see Result.Validate.
	ErrInvalidResult = errors.New("graph: invalid result")
)

// Result is what a node returns: where to go next, how it ended, what changed,
// and what it cost.
//
// A node returns (Result, nil) for every outcome it understands, including
// failure. A non-nil error means the node could not reach a verdict at all —
// its adapter was unreachable, its input was malformed. The distinction
// matters: a failing test is a Result routing to NodeFix, whereas a missing Go
// toolchain is an error, and collapsing the two makes the fix loop try to
// repair a machine that is merely missing software.
type Result struct {
	// Next is the name of the node to run next, or End to finish the run.
	// Ignored when Status is not StatusOK.
	Next string

	// Status is how this node ended. StatusPaused stops the dispatcher
	// cleanly and requires `belay resume` to continue; StatusFailed and
	// StatusAborted end the run.
	Status journal.Status

	// Patch is the state mutation the dispatcher applies and persists.
	// The zero Patch is valid and means "nothing changed".
	Patch state.Patch

	// Usage is the token and USD cost attributable to this node, for the
	// budget ledger and the timeline. Zero for nodes that call no agent.
	Usage belay.Usage

	// Note is a short human-readable reason, surfaced in the journal and
	// the timeline. It must already be redacted — internal/exec redacts
	// captured subprocess output, but a note a node composes itself is
	// the node's own responsibility.
	Note string
}

// Validate reports whether the dispatcher can act on r.
func (r Result) Validate() error {
	if r.Status == journal.StatusUnknown {
		return fmt.Errorf("%w: Status is unset", ErrInvalidResult)
	}
	if r.Status == journal.StatusOK && r.Next == "" {
		return fmt.Errorf("%w: Status is OK but Next is empty (use graph.End to finish)", ErrInvalidResult)
	}
	return nil
}

// Done reports whether r ends the run successfully.
func (r Result) Done() bool { return r.Status == journal.StatusOK && r.Next == End }

// Node is one step of the graph.
//
// Implementations must be safe to re-run: the dispatcher re-executes a node
// whose node_started record has no matching node_finished, because that is how
// a crash inside a node presents on resume.
type Node interface {
	// Name returns the node's canonical name, one of the Node* constants.
	// It must be constant for the lifetime of the value.
	Name() string

	// Run executes one step. It must respect ctx cancellation, and must
	// not write state.json, manifest.json, or the journal.
	Run(ctx context.Context, rc *RunContext) (Result, error)
}

// RunContext is everything a node is allowed to see. The dispatcher builds one
// per node execution.
//
// State is a snapshot taken before the node ran. Mutating it has no effect —
// changes are expressed through Result.Patch, which is what keeps persistence
// in one place.
type RunContext struct {
	// Goal is the run's objective, as given to `belay run`.
	Goal string

	// State is a read-only snapshot. Return changes via Result.Patch.
	State state.State

	// Config is the run's resolved configuration.
	Config config.Config

	// Layout resolves every path inside the run directory. Nodes must use
	// it rather than joining paths themselves.
	Layout state.Layout

	// NodeName is the name of the executing node, and Step its position in
	// the run. Attempt is 1 on a first execution and increments when the
	// dispatcher re-runs a node after a crash or a fix-loop retry.
	NodeName string
	Step     int
	Attempt  int

	// Logger is scoped to this node execution.
	Logger *slog.Logger

	// Adapters. Any of these may be nil when the configuration does not
	// select one — a run with review.mode "lint" has no Reviewer. A node
	// that needs a nil adapter must return a clear error rather than
	// panicking.
	Agent    belay.AgentBackend
	Runner   belay.TestRunner
	Linter   belay.Linter
	Reviewer belay.Reviewer
	Isolator belay.Isolator
}

// WriteArtifact writes a run artifact — plan.md, review.json, a diff — and
// returns its path relative to the run directory, suitable for storing in
// State. Artifacts are the one thing a node may write directly; they are
// content, not control state, so a partially written artifact cannot corrupt
// resume the way a partially written state.json could.
func (rc *RunContext) WriteArtifact(name string, data []byte) (string, error) {
	abs, err := rc.Layout.ArtifactPath(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return "", fmt.Errorf("graph: create artifacts dir: %w", err)
	}
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		return "", fmt.Errorf("graph: write artifact %q: %w", name, err)
	}
	rel, err := filepath.Rel(rc.Layout.RunDir(), abs)
	if err != nil {
		return "", fmt.Errorf("graph: relativize artifact %q: %w", name, err)
	}
	return rel, nil
}

// ReadArtifact reads an artifact previously written by WriteArtifact. It is
// how a resumed node recovers work from before a crash, and how the approve
// node reads a plan a human edited by hand.
func (rc *RunContext) ReadArtifact(name string) ([]byte, error) {
	abs, err := rc.Layout.ArtifactPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs) //nolint:gosec // path is constrained by Layout.ArtifactPath
	if err != nil {
		return nil, fmt.Errorf("graph: read artifact %q: %w", name, err)
	}
	return data, nil
}

// WriteNodeFile records per-execution evidence — the prompt sent, the raw
// response, captured stdout — under this node's own directory, so a failed run
// can be diagnosed after the fact.
func (rc *RunContext) WriteNodeFile(name string, data []byte) error {
	dir, err := rc.Layout.NodeDir(rc.Step, rc.NodeName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("graph: create node dir: %w", err)
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("%w: node file %q must be a bare filename", state.ErrInvalidPathSegment, name)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		return fmt.Errorf("graph: write node file %q: %w", name, err)
	}
	return nil
}

// Registry maps node names to implementations. It is populated once at wiring
// time and read concurrently thereafter; Register must not be called after the
// dispatcher starts.
type Registry struct {
	nodes map[string]Node
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{nodes: make(map[string]Node)} }

// Register adds n. It rejects an empty name, the reserved End value, and any
// name already taken — a silent overwrite would make the graph depend on
// registration order.
func (r *Registry) Register(n Node) error {
	name := n.Name()
	if name == "" || name == End {
		return fmt.Errorf("%w: %q", ErrInvalidNodeName, name)
	}
	if _, dup := r.nodes[name]; dup {
		return fmt.Errorf("%w: %q", ErrDuplicateNode, name)
	}
	r.nodes[name] = n
	return nil
}

// MustRegister is Register for wiring code that cannot proceed on failure.
func (r *Registry) MustRegister(nodes ...Node) {
	for _, n := range nodes {
		if err := r.Register(n); err != nil {
			panic(err)
		}
	}
}

// Get returns the node registered under name.
func (r *Registry) Get(name string) (Node, error) {
	n, ok := r.nodes[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %v)", ErrUnknownNode, name, r.Names())
	}
	return n, nil
}

// Names returns every registered name, sorted, so error messages and tests are
// deterministic.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.nodes))
	for name := range r.nodes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Len reports how many nodes are registered.
func (r *Registry) Len() int { return len(r.nodes) }
