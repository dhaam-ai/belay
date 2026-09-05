package graph_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/state"
)

type stubNode struct{ name string }

func (s stubNode) Name() string { return s.name }
func (s stubNode) Run(context.Context, *graph.RunContext) (graph.Result, error) {
	return graph.Result{Next: graph.End, Status: journal.StatusOK}, nil
}

// journal derives a resume point and names the entry node itself, while graph
// owns the node vocabulary. Nothing but this test makes the two agree, and a
// silent divergence would send every fresh run to a node that is not
// registered.
func TestJournalFirstNodeMatchesGraphEntryPoint(t *testing.T) {
	if journal.FirstNode != graph.NodePlan {
		t.Fatalf("journal.FirstNode = %q, graph.NodePlan = %q; the resume path and the registry must name the same entry node",
			journal.FirstNode, graph.NodePlan)
	}
}

func TestResultValidate(t *testing.T) {
	tests := []struct {
		name string
		res  graph.Result
		want error
	}{
		{"ok with next", graph.Result{Next: graph.NodeCode, Status: journal.StatusOK}, nil},
		{"ok with End", graph.Result{Next: graph.End, Status: journal.StatusOK}, nil},
		{"ok without next", graph.Result{Status: journal.StatusOK}, graph.ErrInvalidResult},
		{"unset status", graph.Result{Next: graph.NodeCode}, graph.ErrInvalidResult},
		{"failed needs no next", graph.Result{Status: journal.StatusFailed}, nil},
		{"paused needs no next", graph.Result{Status: journal.StatusPaused}, nil},
		{"aborted needs no next", graph.Result{Status: journal.StatusAborted}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.res.Validate()
			if tt.want == nil && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("Validate() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestResultDone(t *testing.T) {
	if !(graph.Result{Next: graph.End, Status: journal.StatusOK}).Done() {
		t.Error("OK+End should be Done")
	}
	if (graph.Result{Next: graph.End, Status: journal.StatusFailed}).Done() {
		t.Error("a failed result is never Done, even routed to End")
	}
	if (graph.Result{Next: graph.NodeTest, Status: journal.StatusOK}).Done() {
		t.Error("OK with a real Next is not Done")
	}
}

func TestRegistry(t *testing.T) {
	r := graph.NewRegistry()

	if err := r.Register(stubNode{graph.NodePlan}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Register(stubNode{graph.NodePlan}); !errors.Is(err, graph.ErrDuplicateNode) {
		t.Fatalf("duplicate Register = %v, want ErrDuplicateNode", err)
	}
	if err := r.Register(stubNode{""}); !errors.Is(err, graph.ErrInvalidNodeName) {
		t.Fatalf("empty name = %v, want ErrInvalidNodeName", err)
	}
	if err := r.Register(stubNode{graph.End}); !errors.Is(err, graph.ErrInvalidNodeName) {
		t.Fatalf("End as a node name = %v, want ErrInvalidNodeName", err)
	}
	if _, err := r.Get("nope"); !errors.Is(err, graph.ErrUnknownNode) {
		t.Fatalf("Get(unregistered) = %v, want ErrUnknownNode", err)
	}
	if _, err := r.Get(graph.NodePlan); err != nil {
		t.Fatalf("Get(registered): %v", err)
	}

	r.MustRegister(stubNode{graph.NodeCode}, stubNode{graph.NodeTest})
	if got, want := r.Len(), 3; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}
	names := r.Names()
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("Names() not sorted: %v", names)
		}
	}
}

// Every canonical name must be distinct and registrable; a copy-paste
// collision would make one node silently unreachable.
func TestCanonicalNodeNamesAreDistinctAndRegistrable(t *testing.T) {
	all := []string{
		graph.NodePlan, graph.NodeApprove, graph.NodeCode, graph.NodeWrite,
		graph.NodeTest, graph.NodeFix, graph.NodeReview, graph.NodeFanout, graph.NodeJoin,
	}
	r := graph.NewRegistry()
	for _, n := range all {
		if err := r.Register(stubNode{n}); err != nil {
			t.Fatalf("Register(%q): %v", n, err)
		}
	}
	if got, want := r.Len(), len(all); got != want {
		t.Fatalf("registered %d of %d names; some collide: %v", got, want, r.Names())
	}
}

func newRC(t *testing.T) *graph.RunContext {
	t.Helper()
	ws := t.TempDir()
	layout, err := state.NewLayout(ws, "2026-08-31T00-00-00Z-test")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	return &graph.RunContext{Layout: layout, NodeName: graph.NodePlan, Step: 1, Attempt: 1}
}

func TestWriteAndReadArtifact(t *testing.T) {
	rc := newRC(t)
	rel, err := rc.WriteArtifact("plan.md", []byte("# plan\n"))
	if err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	if filepath.IsAbs(rel) {
		t.Fatalf("WriteArtifact returned %q; State stores run-relative paths", rel)
	}
	if want := filepath.Join("artifacts", "plan.md"); rel != want {
		t.Fatalf("rel = %q, want %q", rel, want)
	}
	got, err := rc.ReadArtifact("plan.md")
	if err != nil {
		t.Fatalf("ReadArtifact: %v", err)
	}
	if string(got) != "# plan\n" {
		t.Fatalf("round trip = %q", got)
	}
}

// Layout already refuses escaping names; this pins that RunContext does not
// route around it.
func TestArtifactPathEscapesAreRefused(t *testing.T) {
	rc := newRC(t)
	for _, bad := range []string{"../escape.md", "/etc/passwd", "a/b.md", ""} {
		if _, err := rc.WriteArtifact(bad, []byte("x")); err == nil {
			t.Errorf("WriteArtifact(%q) succeeded; want refusal", bad)
		}
	}
}

func TestWriteNodeFile(t *testing.T) {
	rc := newRC(t)
	if err := rc.WriteNodeFile("prompt.txt", []byte("hello")); err != nil {
		t.Fatalf("WriteNodeFile: %v", err)
	}
	dir, err := rc.Layout.NodeDir(rc.Step, rc.NodeName)
	if err != nil {
		t.Fatalf("NodeDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "prompt.txt")); err != nil {
		t.Fatalf("node file not written: %v", err)
	}
	if err := rc.WriteNodeFile("../escape.txt", []byte("x")); err == nil {
		t.Error("WriteNodeFile accepted a path segment; want refusal")
	}
}
