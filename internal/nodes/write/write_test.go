package write

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
)

func TestNameIsTheCanonicalNodeName(t *testing.T) {
	if got := New().Name(); got != graph.NodeWrite {
		t.Errorf("Name() = %q, want %q", got, graph.NodeWrite)
	}
	if got := (&Node{}).Name(); got != graph.NodeWrite {
		t.Errorf("zero Node Name() = %q, want %q", got, graph.NodeWrite)
	}
}

func TestRunRecordsTheChangeSetAndRoutesToTest(t *testing.T) {
	rc, root := newRC(t, 7, []string{"src/main.go", "README.md", "src/gone.go"})
	mustWrite(t, filepath.Join(root, "src", "main.go"), "package main\n")
	mustWrite(t, filepath.Join(root, "README.md"), "# belay\n")

	got, err := New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Result.Validate: %v", err)
	}
	if got.Next != graph.NodeTest {
		t.Errorf("Next = %q, want %q", got.Next, graph.NodeTest)
	}
	if got.Status != journal.StatusOK {
		t.Errorf("Status = %v, want %v", got.Status, journal.StatusOK)
	}
	if got.Usage != (belay.Usage{}) {
		t.Errorf("Usage = %+v, want zero: this node calls no agent", got.Usage)
	}
	if got.Patch.Code == nil {
		t.Fatal("Patch.Code is nil; the node must report what it verified")
	}
	if got.Patch.Test != nil || got.Patch.Plan != nil || got.Patch.Review != nil ||
		got.Patch.Fix != nil || got.Patch.Candidates != nil || got.Patch.History != nil {
		t.Errorf("Patch touches sections outside Code: %+v", got.Patch)
	}

	wantRel := filepath.Join("artifacts", "diff-0007.patch")
	if got.Patch.Code.LastDiff != wantRel {
		t.Errorf("Code.LastDiff = %q, want %q", got.Patch.Code.LastDiff, wantRel)
	}
	if got.Patch.Code.SessionID != "sess-1" {
		t.Errorf("Code.SessionID = %q, want it carried through unchanged", got.Patch.Code.SessionID)
	}
	want := []string{"README.md", "src/gone.go", "src/main.go"}
	if diff := cmp.Diff(want, got.Patch.Code.ChangedFiles); diff != "" {
		t.Errorf("Code.ChangedFiles mismatch (-want +got):\n%s", diff)
	}

	artifact := readArtifact(t, rc, got.Patch.Code.LastDiff)
	for _, fragment := range []string{
		"diff --git a/README.md b/README.md",
		"+# belay",
		"diff --git a/src/gone.go b/src/gone.go\ndeleted file mode 100644",
		"+package main",
	} {
		if !strings.Contains(artifact, fragment) {
			t.Errorf("artifact does not contain %q:\n%s", fragment, artifact)
		}
	}

	// The patch it applies to state must round-trip onto a State.
	st := state.NewState(rc.Goal)
	if err := got.Patch.Apply(&st); err != nil {
		t.Fatalf("Patch.Apply: %v", err)
	}
	if diff := cmp.Diff(want, st.Code.ChangedFiles); diff != "" {
		t.Errorf("applied State.Code.ChangedFiles mismatch (-want +got):\n%s", diff)
	}
}

func TestRunEmptyChangeIsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changed []string
	}{
		{name: "nil change set", changed: nil},
		{name: "empty change set", changed: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc, root := newRC(t, 3, tc.changed)
			mustWrite(t, filepath.Join(root, "untouched.go"), "package untouched\n")
			before := hashTree(t, root)

			got, err := New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.Status != journal.StatusOK || got.Next != graph.NodeTest {
				t.Fatalf("Run = {Status:%v Next:%q}, want {ok %q}", got.Status, got.Next, graph.NodeTest)
			}
			if got.Patch.Code == nil {
				t.Fatal("Patch.Code is nil")
			}
			if len(got.Patch.Code.ChangedFiles) != 0 {
				t.Errorf("ChangedFiles = %v, want empty", got.Patch.Code.ChangedFiles)
			}
			if got.Patch.Code.ChangedFiles == nil {
				t.Error("ChangedFiles is nil; an empty change must serialize as [] and not null")
			}
			if body := readArtifact(t, rc, got.Patch.Code.LastDiff); body != "" {
				t.Errorf("artifact = %q, want an empty patch", body)
			}
			if !strings.Contains(got.Note, "no files changed") {
				t.Errorf("Note = %q, want it to say plainly that nothing changed", got.Note)
			}
			if after := hashTree(t, root); after != before {
				t.Error("the workspace changed while recording an empty change set")
			}
		})
	}
}

func TestRunIsSafeToReRun(t *testing.T) {
	rc, root := newRC(t, 7, []string{"src/main.go", "docs/notes.md", "src/deleted.go"})
	mustWrite(t, filepath.Join(root, "src", "main.go"), "package main\n\nfunc main() {}\n")
	mustWrite(t, filepath.Join(root, "docs", "notes.md"), "notes\nno trailing newline")

	node := New()
	first, err := node.Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	afterFirstTree := hashTree(t, root)
	firstArtifact := readArtifact(t, rc, first.Patch.Code.LastDiff)

	// The dispatcher re-runs a node whose node_started has no matching
	// node_finished: same step, same state, same everything.
	second, err := node.Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	afterSecondTree := hashTree(t, root)

	if afterSecondTree != afterFirstTree {
		t.Errorf("the workspace tree differs after a re-run:\n first:  %s\n second: %s",
			afterFirstTree, afterSecondTree)
	}
	if diff := cmp.Diff(first, second); diff != "" {
		t.Errorf("re-running produced a different Result (-first +second):\n%s", diff)
	}
	secondArtifact := readArtifact(t, rc, second.Patch.Code.LastDiff)
	if diff := cmp.Diff(firstArtifact, secondArtifact); diff != "" {
		t.Errorf("re-running produced a different artifact (-first +second):\n%s", diff)
	}
	if second.Patch.Code.LastDiff != first.Patch.Code.LastDiff {
		t.Errorf("re-running wrote a second artifact %q alongside %q; a step numbers one record, not two",
			second.Patch.Code.LastDiff, first.Patch.Code.LastDiff)
	}
}

func TestRunNumbersArtifactsPerStep(t *testing.T) {
	rc, root := newRC(t, 7, []string{"a.go"})
	mustWrite(t, filepath.Join(root, "a.go"), "package a\n")

	node := New()
	attempt1, err := node.Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	body1 := readArtifact(t, rc, attempt1.Patch.Code.LastDiff)

	// The fix loop edited the file and the dispatcher advanced the step.
	mustWrite(t, filepath.Join(root, "a.go"), "package a\n\nvar Fixed = true\n")
	rc.Step = 9
	rc.Attempt = 2
	attempt2, err := node.Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("attempt 2: %v", err)
	}

	if attempt1.Patch.Code.LastDiff == attempt2.Patch.Code.LastDiff {
		t.Fatalf("both attempts wrote %q; a fix-loop retry must not overwrite the previous record",
			attempt1.Patch.Code.LastDiff)
	}
	if !artifactExists(t, rc, "diff-0007.patch") {
		t.Error("attempt 1's artifact is gone after attempt 2")
	}
	if got := readArtifact(t, rc, attempt1.Patch.Code.LastDiff); got != body1 {
		t.Errorf("attempt 1's artifact changed after attempt 2:\n-%s\n+%s", body1, got)
	}
	body2 := readArtifact(t, rc, attempt2.Patch.Code.LastDiff)
	if !strings.Contains(body2, "+var Fixed = true") {
		t.Errorf("attempt 2's artifact does not record the new content:\n%s", body2)
	}
}

func TestRunRefusesEscapingChangeSets(t *testing.T) {
	tests := []struct {
		name string
		// claim builds the change-set entry, given the workspace root.
		claim func(root string) string
		// target names the absolute path the claim would have reached.
		target func(root string) string
	}{
		{
			name:   "parent traversal",
			claim:  func(string) string { return "../ESCAPED" },
			target: func(root string) string { return filepath.Join(filepath.Dir(root), "ESCAPED") },
		},
		{
			name:   "absolute path",
			claim:  func(root string) string { return filepath.Join(filepath.Dir(root), "ESCAPED") },
			target: func(root string) string { return filepath.Join(filepath.Dir(root), "ESCAPED") },
		},
		{
			name:   "symlink pointing out of the tree",
			claim:  func(string) string { return "leak.txt" },
			target: func(root string) string { return filepath.Join(filepath.Dir(root), "outside", "ESCAPED") },
		},
		{
			name:   "path under a directory symlinked out of the tree",
			claim:  func(string) string { return "link/ESCAPED" },
			target: func(root string) string { return filepath.Join(filepath.Dir(root), "outside", "ESCAPED") },
		},
		{
			name:   "sibling directory sharing a string prefix with the root",
			claim:  func(string) string { return "prefix-link/ESCAPED" },
			target: func(root string) string { return root + "-evil/ESCAPED" },
		},
		{
			name:   "belay's own run directory",
			claim:  func(string) string { return ".belay/runs/" + testRunID + "/state.json" },
			target: func(root string) string { return filepath.Join(root, ".belay", "runs", testRunID, "state.json") },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc, root := newRC(t, 4, nil)
			// A real tree outside the workspace, and the links that reach it.
			outside := filepath.Join(filepath.Dir(root), "outside")
			if err := os.MkdirAll(outside, 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			mustWrite(t, filepath.Join(outside, "secret.txt"), "secret\n")
			prefixSibling := root + "-evil"
			if err := os.MkdirAll(prefixSibling, 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			if err := os.Symlink(prefixSibling, filepath.Join(root, "prefix-link")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "leak.txt")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			mustWrite(t, filepath.Join(root, "honest.go"), "package honest\n")
			rc.State.Code.ChangedFiles = []string{"honest.go", tc.claim(root)}

			beforeWorkspace := hashTree(t, root)
			beforeOutside := hashTree(t, outside)

			got, err := New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run returned an error %v; a refusal is a verdict, not an inability to reach one", err)
			}
			if got.Status != journal.StatusFailed {
				t.Fatalf("Status = %v, want %v", got.Status, journal.StatusFailed)
			}
			if verr := got.Validate(); verr != nil {
				t.Errorf("Result.Validate on a refusal: %v; the dispatcher must be able to act on it", verr)
			}
			if got.Done() {
				t.Error("a refusal reported Done; a refused run has not succeeded")
			}
			if got.Patch.Code != nil {
				t.Errorf("Patch.Code = %+v, want nil: a refused change set is not recorded as fact", got.Patch.Code)
			}
			if got.Note == "" {
				t.Error("Note is empty; a refusal must say what it refused")
			}
			if artifactExists(t, rc, "diff-0004.patch") {
				t.Error("an artifact was written for a refused change set")
			}

			// Nothing was created, in the workspace or outside it.
			target := tc.target(root)
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("refused target %q exists after the refusal (err=%v)", target, err)
			}
			if after := hashTree(t, root); after != beforeWorkspace {
				t.Error("the workspace tree changed during a refusal")
			}
			if after := hashTree(t, outside); after != beforeOutside {
				t.Error("the tree outside the workspace changed during a refusal")
			}
		})
	}
}

func TestRunCancellationLeavesNothingBehind(t *testing.T) {
	// The thresholds pick the stage the cancellation lands in: Run checks
	// ctx.Err once on entry, verify once per claimed path, renderPatch once
	// per change. With three claimed files that is entry at 1, verification
	// at 2 through 4, and rendering at 5 through 7.
	tests := []struct {
		name string
		ctx  func() context.Context
		// wantStage is a fragment of the error the stage that noticed the
		// cancellation wraps it with. Asserting it is what keeps the
		// thresholds honest: if the number of ctx.Err checks ever shifts,
		// the cancellation lands somewhere else and this test says so
		// instead of quietly passing without reaching the stage it names.
		wantStage string
	}{
		{
			name: "cancelled before Run",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantStage: "write: context canceled",
		},
		{
			name:      "cancelled while verifying the change set",
			ctx:       func() context.Context { return cancelAfter(2) },
			wantStage: "write: verify change set:",
		},
		{
			name:      "cancelled while rendering the patch",
			ctx:       func() context.Context { return cancelAfter(5) },
			wantStage: "write: render patch:",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc, root := newRC(t, 5, []string{"a.go", "b.go", "c.go"})
			mustWrite(t, filepath.Join(root, "a.go"), "package a\n")
			mustWrite(t, filepath.Join(root, "b.go"), "package b\n")
			mustWrite(t, filepath.Join(root, "c.go"), "package c\n")
			before := hashTree(t, root)

			got, err := New().Run(tc.ctx(), rc)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run = (%+v, %v); want an error wrapping context.Canceled", got, err)
			}
			if !strings.Contains(err.Error(), tc.wantStage) {
				t.Fatalf("Run error = %q; want it to come from %q — the cancellation did not land in the stage this case exists to cover",
					err, tc.wantStage)
			}
			if got.Patch.Code != nil || got.Next != "" {
				t.Errorf("Run returned a usable Result %+v alongside a cancellation", got)
			}
			if after := hashTree(t, root); after != before {
				t.Errorf("the workspace tree changed under cancellation:\n before: %s\n after:  %s", before, after)
			}
			if artifactExists(t, rc, "diff-0005.patch") {
				t.Error("an artifact was written despite cancellation")
			}
		})
	}
}

func TestRunUsesAnExplicitWorkspaceRoot(t *testing.T) {
	rc, derived := newRC(t, 2, []string{"pinned.go"})
	pinned := t.TempDir()
	mustWrite(t, filepath.Join(pinned, "pinned.go"), "package pinned\n")
	mustWrite(t, filepath.Join(derived, "pinned.go"), "package wrong\n")

	got, err := New(WithWorkspaceRoot(pinned)).Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body := readArtifact(t, rc, got.Patch.Code.LastDiff)
	if !strings.Contains(body, "+package pinned") {
		t.Errorf("the pinned workspace root was not used:\n%s", body)
	}
	if strings.Contains(body, "+package wrong") {
		t.Errorf("the derived workspace root was used despite an explicit one:\n%s", body)
	}
}

func TestRunErrorsWhenTheWorkspaceCannotBeEstablished(t *testing.T) {
	tests := []struct {
		name string
		node *Node
		rc   *graph.RunContext
	}{
		{
			name: "underivable layout",
			node: New(),
			rc:   &graph.RunContext{Layout: state.Layout{}, NodeName: graph.NodeWrite, Step: 1, Attempt: 1},
		},
		{
			name: "pinned root does not exist",
			node: New(WithWorkspaceRoot(filepath.Join(t.TempDir(), "missing"))),
			rc:   &graph.RunContext{NodeName: graph.NodeWrite, Step: 1, Attempt: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.node.Run(context.Background(), tc.rc)
			if !errors.Is(err, ErrWorkspace) {
				t.Fatalf("Run = (%+v, %v); want an error wrapping ErrWorkspace", got, err)
			}
		})
	}
}

func TestRunRejectsANilRunContext(t *testing.T) {
	if _, err := New().Run(context.Background(), nil); err == nil {
		t.Fatal("Run(nil) = nil error; want a clear failure rather than a panic")
	}
}

func TestWithMaxFileBytes(t *testing.T) {
	rc, root := newRC(t, 1, []string{"big.txt"})
	mustWrite(t, filepath.Join(root, "big.txt"), strings.Repeat("y", 100)+"\n")

	got, err := New(WithMaxFileBytes(16)).Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body := readArtifact(t, rc, got.Patch.Code.LastDiff)
	if !strings.Contains(body, "exceeds the 16 byte record limit") {
		t.Errorf("WithMaxFileBytes was not honoured:\n%s", body)
	}
	if diff := cmp.Diff([]string{"big.txt"}, got.Patch.Code.ChangedFiles); diff != "" {
		t.Errorf("an over-limit file must still be reported as changed (-want +got):\n%s", diff)
	}
}

func TestSanitizeNote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "verified 2 files", want: "verified 2 files"},
		{name: "newlines collapse", in: "refused\n\"a\"\nb", want: `refused "a" b`},
		{name: "carriage return collapses", in: "a\rb", want: "a b"},
		{name: "tabs collapse", in: "a\tb", want: "a b"},
		{name: "trimmed", in: "  spaced  ", want: "spaced"},
		{name: "long input is bounded", in: strings.Repeat("z", 400), want: strings.Repeat("z", maxNoteRunes) + "..."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeNote(tc.in); got != tc.want {
				t.Errorf("sanitizeNote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNodeRegistersWithTheDispatcher(t *testing.T) {
	registry := graph.NewRegistry()
	node := New()
	if err := registry.Register(node); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := registry.Get(graph.NodeWrite)
	if err != nil {
		t.Fatalf("Get(%q): %v", graph.NodeWrite, err)
	}
	if got != graph.Node(node) {
		t.Errorf("Get(%q) returned a different node than was registered", graph.NodeWrite)
	}
	// A node routing to a name the registry cannot resolve is a wiring bug
	// that only shows up at runtime, so the destination is checked here too.
	if _, err := registry.Get(graph.NodeTest); err == nil {
		t.Skip("the test node is registered in this build; nothing to assert about the route")
	} else if !errors.Is(err, graph.ErrUnknownNode) {
		t.Fatalf("Get(%q) error = %v; want ErrUnknownNode", graph.NodeTest, err)
	}
}
