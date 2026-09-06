//go:build unix

package dircopy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestIsAncestorOf pins the prefix-confusion rule that the whole containment
// story rests on. The case that matters is "/a/bc" against "/a/b": a naive
// strings.HasPrefix says yes, and acting on that answer deletes a sibling
// directory that merely shares a name prefix with the root.
func TestIsAncestorOf(t *testing.T) {
	tests := []struct {
		name string
		root string
		path string
		want bool
	}{
		{"child", "/a/b", "/a/b/c", true},
		{"grandchild", "/a/b", "/a/b/c/d", true},
		{"prefix confusion: sibling sharing a name prefix", "/a/b", "/a/bc", false},
		{"prefix confusion: deeper sibling", "/a/b", "/a/bc/d", false},
		{"prefix confusion with trailing content", "/a/b", "/a/b.old/secrets", false},
		{"identical paths are not inside each other", "/a/b", "/a/b", false},
		{"identical after cleaning", "/a/b", "/a/b/", false},
		{"parent", "/a/b", "/a", false},
		{"traversal out and back is cleaned first", "/a/b", "/a/b/../c", false},
		{"traversal that lands back inside is allowed", "/a/b", "/a/b/x/../y", true},
		{"unrelated absolute path", "/a/b", "/etc/passwd", false},
		{"relative path refused", "/a/b", "a/b/c", false},
		{"relative root refused", "a/b", "/a/b/c", false},
		{"empty root", "", "/a/b", false},
		{"empty path", "/a/b", "", false},
		{"root filesystem contains everything", "/", "/etc", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAncestorOf(tt.root, tt.path); got != tt.want {
				t.Errorf("isAncestorOf(%q, %q) = %v, want %v", tt.root, tt.path, got, tt.want)
			}
		})
	}
}

// TestIsChildOf covers the stricter rule Destroy actually enforces: a
// candidate workspace is exactly one segment below the root, never deeper.
func TestIsChildOf(t *testing.T) {
	tests := []struct {
		name string
		root string
		path string
		want bool
	}{
		{"direct child", "/a/b", "/a/b/c", true},
		{"grandchild is not a direct child", "/a/b", "/a/b/c/d", false},
		{"prefix confusion", "/a/b", "/a/bc", false},
		{"root itself", "/a/b", "/a/b", false},
		{"outside", "/a/b", "/etc", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isChildOf(tt.root, tt.path); got != tt.want {
				t.Errorf("isChildOf(%q, %q) = %v, want %v", tt.root, tt.path, got, tt.want)
			}
		})
	}
}

// TestDestroyRefusesHostileWorkspaces is the test this package exists for.
//
// Every case hands Destroy a Workspace that names something it must not
// delete, and asserts two things: the call is refused with an error wrapping
// ErrRefused, and the thing it would have deleted is still on disk
// afterwards. The second assertion is the real one — an error return means
// nothing if the tree is already gone.
func TestDestroyRefusesHostileWorkspaces(t *testing.T) {
	tests := []struct {
		name string
		// build returns the hostile Workspace and the path that must survive.
		build func(t *testing.T, iso *Isolator, victim, candidate string) (belay.Workspace, string)
	}{
		{
			name: "parent traversal out of the candidate root",
			build: func(_ *testing.T, iso *Isolator, victim, _ string) (belay.Workspace, string) {
				escape := filepath.Join(iso.Root(), "..", filepath.Base(victim))
				return belay.Workspace{ID: "evil", Dir: escape, Ephemeral: true}, victim
			},
		},
		{
			name: "deep traversal out and into an unrelated tree",
			build: func(_ *testing.T, iso *Isolator, victim, _ string) (belay.Workspace, string) {
				escape := filepath.Join(iso.Root(), "cand", "..", "..", filepath.Base(victim))
				return belay.Workspace{ID: "evil", Dir: escape, Ephemeral: true}, victim
			},
		},
		{
			name: "absolute path outside the candidate root",
			build: func(_ *testing.T, _ *Isolator, victim, _ string) (belay.Workspace, string) {
				return belay.Workspace{ID: "evil", Dir: victim, Ephemeral: true}, victim
			},
		},
		{
			name: "sibling directory whose name shares the root's prefix",
			build: func(t *testing.T, iso *Isolator, _, _ string) (belay.Workspace, string) {
				// The /a/bc versus /a/b case, on a real filesystem.
				sibling := mkdirAll(t, iso.Root()+"-attacker")
				writeFile(t, filepath.Join(sibling, "keep.txt"), "keep", 0o600)
				return belay.Workspace{ID: "evil", Dir: sibling, Ephemeral: true}, sibling
			},
		},
		{
			name: "symlink inside the root pointing outside it",
			build: func(t *testing.T, iso *Isolator, victim, _ string) (belay.Workspace, string) {
				link := symlink(t, victim, filepath.Join(iso.Root(), "escape"))
				return belay.Workspace{ID: "evil", Dir: link, Ephemeral: true}, victim
			},
		},
		{
			name: "empty Dir",
			build: func(_ *testing.T, _ *Isolator, victim, _ string) (belay.Workspace, string) {
				return belay.Workspace{ID: "evil", Dir: "", Ephemeral: true}, victim
			},
		},
		{
			name: "workspace never produced by Create, marked non-ephemeral",
			build: func(_ *testing.T, _ *Isolator, _, candidate string) (belay.Workspace, string) {
				// belay.Workspace documents Ephemeral false as "this is the
				// user's own tree". That is the one directory this package
				// must never remove, even though the path is inside the root.
				return belay.Workspace{ID: "cand", Dir: candidate, Ephemeral: false}, candidate
			},
		},
		{
			name: "relative Dir",
			build: func(_ *testing.T, _ *Isolator, victim, _ string) (belay.Workspace, string) {
				return belay.Workspace{ID: "evil", Dir: "candidates/cand", Ephemeral: true}, victim
			},
		},
		{
			name: "grandchild of the root rather than a candidate",
			build: func(t *testing.T, _ *Isolator, _, candidate string) (belay.Workspace, string) {
				nested := mkdirAll(t, filepath.Join(candidate, "nested"))
				writeFile(t, filepath.Join(nested, "keep.txt"), "keep", 0o600)
				return belay.Workspace{ID: "evil", Dir: nested, Ephemeral: true}, nested
			},
		},
		{
			name: "the candidate root itself",
			build: func(_ *testing.T, iso *Isolator, _, _ string) (belay.Workspace, string) {
				return belay.Workspace{ID: "evil", Dir: iso.Root(), Ephemeral: true}, iso.Root()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iso := newIsolator(t)

			// A real bystander tree outside the root, and a real candidate
			// inside it, so every refusal has something concrete to protect.
			victim := mkdirAll(t, filepath.Join(t.TempDir(), "precious"))
			victimFile := writeFile(t, filepath.Join(victim, "thesis.txt"), "years of work", 0o600)
			candidate := mustCreate(t, iso, sampleTree(t), "cand")

			ws, mustSurvive := tt.build(t, iso, victim, candidate)

			err := iso.Destroy(context.Background(), ws)
			if err == nil {
				t.Fatalf("Destroy(%+v) returned nil; it must refuse", ws)
			}
			if !errors.Is(err, ErrRefused) {
				t.Errorf("Destroy error does not wrap ErrRefused: %v", err)
			}
			var ce *ContainmentError
			if !errors.As(err, &ce) {
				t.Errorf("Destroy error is not a *ContainmentError: %v", err)
			} else if ce.Op != "destroy" {
				t.Errorf("ContainmentError.Op = %q, want %q", ce.Op, "destroy")
			}

			requireExists(t, mustSurvive, "after a refused Destroy")
			requireExists(t, victim, "the bystander tree")
			requireExists(t, victimFile, "the bystander file")
			requireExists(t, candidate, "the legitimate candidate")
			requireExists(t, iso.Root(), "the candidate root")
		})
	}
}

// TestDestroyRemovesItsOwnCandidate is the positive half: what Create made,
// Destroy removes, leaving the root and its siblings alone.
func TestDestroyRemovesItsOwnCandidate(t *testing.T) {
	iso := newIsolator(t)
	src := sampleTree(t)

	first := mustCreate(t, iso, src, "cand-1")
	second := mustCreate(t, iso, src, "cand-2")

	if err := iso.Destroy(context.Background(), belay.Workspace{ID: "cand-1", Dir: first, Ephemeral: true}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	requireGone(t, first, "after Destroy")
	requireExists(t, second, "the sibling candidate")
	requireExists(t, iso.Root(), "the candidate root")

	if _, ok := iso.Stats("cand-1"); ok {
		t.Error("Stats still reports a destroyed workspace")
	}
	if _, ok := iso.Stats("cand-2"); !ok {
		t.Error("Stats forgot a workspace that was not destroyed")
	}
}

// TestDestroyIsIdempotentOnAMissingWorkspace covers the crash-resume path:
// belay.Isolator requires Destroy to work on a Workspace decoded from a run
// journal, and after a crash the directory may already be gone.
func TestDestroyIsIdempotentOnAMissingWorkspace(t *testing.T) {
	iso := newIsolator(t)
	dir := mustCreate(t, iso, sampleTree(t), "cand")
	ws := belay.Workspace{ID: "cand", Dir: dir, Ephemeral: true}

	if err := iso.Destroy(context.Background(), ws); err != nil {
		t.Fatalf("first Destroy: %v", err)
	}
	if err := iso.Destroy(context.Background(), ws); err != nil {
		t.Errorf("Destroy on an already-removed workspace = %v, want nil", err)
	}
}

// TestDestroyDoesNotFollowSymlinksInsideTheWorkspace checks the other half of
// the symlink story. A link that legitimately lives inside a workspace and
// points at something outside must be unlinked, not traversed: removing the
// workspace must not remove what the link pointed at.
func TestDestroyDoesNotFollowSymlinksInsideTheWorkspace(t *testing.T) {
	iso := newIsolator(t, WithSymlinkPolicy(SymlinkPreserveAll))

	outside := mkdirAll(t, filepath.Join(t.TempDir(), "outside"))
	outsideFile := writeFile(t, filepath.Join(outside, "keep.txt"), "keep me", 0o600)

	src := mkdirAll(t, filepath.Join(t.TempDir(), "repo"))
	writeFile(t, filepath.Join(src, "a.txt"), "a", 0o600)
	symlink(t, outside, filepath.Join(src, "escape-dir"))
	symlink(t, outsideFile, filepath.Join(src, "escape-file"))

	dir := mustCreate(t, iso, src, "cand")
	requireExists(t, filepath.Join(dir, "escape-dir"), "the preserved link")

	if err := iso.Destroy(context.Background(), belay.Workspace{ID: "cand", Dir: dir, Ephemeral: true}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	requireGone(t, dir, "the workspace")
	requireExists(t, outside, "the directory an internal symlink pointed at")
	requireExists(t, outsideFile, "the file an internal symlink pointed at")
	if got := readFile(t, outsideFile); got != "keep me" {
		t.Errorf("linked-to file content = %q, want %q", got, "keep me")
	}
}

// TestDestroyRefusesACancelledContext keeps the ctx contract honest.
func TestDestroyRefusesACancelledContext(t *testing.T) {
	iso := newIsolator(t)
	dir := mustCreate(t, iso, sampleTree(t), "cand")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := iso.Destroy(ctx, belay.Workspace{ID: "cand", Dir: dir, Ephemeral: true})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Destroy error = %v, want context.Canceled", err)
	}
	requireExists(t, dir, "after a cancelled Destroy")
}

// TestValidateID pins the id rules, which are what keep a destination a
// direct child of the root in the first place.
func TestValidateID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"ordinary candidate id", "cand-1", false},
		{"uuid-ish", "20260101T000000Z-abc123", false},
		{"dotfile-ish but harmless", ".hidden", false},
		{"empty", "", true},
		{"dot", ".", true},
		{"dotdot", "..", true},
		{"traversal", "../escape", true},
		{"traversal in the middle", "a/../../b", true},
		{"embedded dotdot", "cand..1", true},
		{"forward separator", "a/b", true},
		{"backslash separator", `a\b`, true},
		{"absolute", "/etc", true},
		{"NUL byte", "cand\x001", true},
		{"whitespace only", "   ", true},
		{"too long", strings.Repeat("a", maxIDLen+1), true},
		{"at the length limit", strings.Repeat("a", maxIDLen), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateID(tt.id)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateID(%q) error = %v, wantErr %v", tt.id, err, tt.wantErr)
			}
			if err == nil {
				return
			}
			if !errors.Is(err, ErrInvalidID) {
				t.Errorf("error does not wrap ErrInvalidID: %v", err)
			}
			var ide *IDError
			if !errors.As(err, &ide) {
				t.Errorf("error is not an *IDError: %v", err)
			}
		})
	}
}

// TestCreateRejectsHostileIDs proves the id rules are enforced at the door
// and that nothing is written outside the root when they are broken.
func TestCreateRejectsHostileIDs(t *testing.T) {
	iso := newIsolator(t)
	src := sampleTree(t)
	outside := filepath.Join(filepath.Dir(iso.Root()), "escaped")

	for _, id := range []string{"../escaped", "..", "/escaped", "a/b", ""} {
		t.Run(id, func(t *testing.T) {
			ws, err := iso.Create(context.Background(), src, id)
			if !errors.Is(err, ErrInvalidID) {
				t.Fatalf("Create(id=%q) error = %v, want ErrInvalidID", id, err)
			}
			if ws != (belay.Workspace{}) {
				t.Errorf("Create returned %+v on error, want the zero Workspace", ws)
			}
			requireGone(t, outside, "no directory may be created outside the root")
		})
	}
}

// TestCreateRejectsBadSources covers the source side of the same argument.
func TestCreateRejectsBadSources(t *testing.T) {
	iso := newIsolator(t)
	file := writeFile(t, filepath.Join(t.TempDir(), "a-file"), "x", 0o600)

	tests := []struct {
		name string
		src  string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"missing", filepath.Join(t.TempDir(), "nope")},
		{"a regular file, not a directory", file},
		{"NUL byte", "/tmp/a\x00b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := iso.Create(context.Background(), tt.src, "cand")
			if !errors.Is(err, ErrInvalidSource) {
				t.Fatalf("Create(src=%q) error = %v, want ErrInvalidSource", tt.src, err)
			}
		})
	}
}

// TestCreateRefusesANonEmptyDestination stops two trees being blended and
// handed to an agent as one clean checkout.
func TestCreateRefusesANonEmptyDestination(t *testing.T) {
	iso := newIsolator(t)
	src := sampleTree(t)

	occupied := mkdirAll(t, filepath.Join(iso.Root(), "cand"))
	stale := writeFile(t, filepath.Join(occupied, "leftover.txt"), "from a previous run", 0o600)

	_, err := iso.Create(context.Background(), src, "cand")
	if !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("Create onto a non-empty destination = %v, want ErrNotEmpty", err)
	}
	var ne *NotEmptyError
	if !errors.As(err, &ne) {
		t.Errorf("error is not a *NotEmptyError: %v", err)
	}

	requireExists(t, stale, "a refused Create must not touch the occupant")
	if got := readFile(t, stale); got != "from a previous run" {
		t.Errorf("occupant content = %q, want it untouched", got)
	}
	requireGone(t, filepath.Join(occupied, "go.mod"), "nothing from src may have been copied")
}

// TestCreateAcceptsAnExistingEmptyDestination is the flip side: a caller that
// pre-created the directory is not doing anything dangerous.
func TestCreateAcceptsAnExistingEmptyDestination(t *testing.T) {
	iso := newIsolator(t)
	mkdirAll(t, filepath.Join(iso.Root(), "cand"))

	dir := mustCreate(t, iso, sampleTree(t), "cand")
	requireExists(t, filepath.Join(dir, "go.mod"), "the copy")
}

// TestCreateRefusesConcurrentDuplicateIDs proves two candidates cannot both
// decide the same empty destination is theirs.
func TestCreateRefusesConcurrentDuplicateIDs(t *testing.T) {
	iso := newIsolator(t)
	src := sampleTree(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = iso.Create(context.Background(), src, "same")
		}()
	}
	close(start)
	wg.Wait()

	var failures int
	for _, err := range errs {
		if err != nil {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("got %d failures from two concurrent Creates with the same id, want exactly 1 (errs=%v)", failures, errs)
	}
}

// TestCreateIsConcurrencySafe exercises the fanout case the Isolator exists
// for: N candidates copied at once, each complete and independent.
func TestCreateIsConcurrencySafe(t *testing.T) {
	iso := newIsolator(t)
	src := sampleTree(t)

	const n = 6
	var wg sync.WaitGroup
	dirs := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ws, err := iso.Create(context.Background(), src, fmt.Sprintf("cand-%d", i))
			dirs[i], errs[i] = ws.Dir, err
		}()
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Create %d: %v", i, errs[i])
		}
		if got := readFile(t, filepath.Join(dirs[i], "go.mod")); got != "module example.com/x\n" {
			t.Errorf("candidate %d has wrong content %q", i, got)
		}
	}
}

// TestNewResolvesItsRoot checks that the containment boundary is stored
// resolved. On macOS t.TempDir() sits under a symlinked /var, so an
// unresolved root would compare against a different naming of the same
// directory than every path Create hands back.
func TestNewResolvesItsRoot(t *testing.T) {
	base := t.TempDir()
	realDir := mkdirAll(t, filepath.Join(base, "real"))
	link := symlink(t, realDir, filepath.Join(base, "link"))

	iso, err := New(link, WithLogger(quietLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resolvedReal, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if iso.Root() != resolvedReal {
		t.Errorf("Root() = %q, want the resolved %q", iso.Root(), resolvedReal)
	}

	dir := mustCreate(t, iso, sampleTree(t), "cand")
	if !isChildOf(iso.Root(), dir) {
		t.Errorf("Create returned %q, which is not a direct child of the resolved root %q", dir, iso.Root())
	}
	if err := iso.Destroy(context.Background(), belay.Workspace{ID: "cand", Dir: dir, Ephemeral: true}); err != nil {
		t.Errorf("Destroy of a workspace under a symlinked root: %v", err)
	}
}

// TestNewRejectsAnEmptyRoot keeps the zero-configuration mistake loud.
func TestNewRejectsAnEmptyRoot(t *testing.T) {
	for _, root := range []string{"", "   "} {
		if _, err := New(root); !errors.Is(err, ErrInvalidSource) {
			t.Errorf("New(%q) error = %v, want ErrInvalidSource", root, err)
		}
	}
}

// TestIsolatorImplementsBelayIsolator restates the compile-time assertion as
// a runtime one, so the interface belay.Isolator is what is exercised.
func TestIsolatorImplementsBelayIsolator(t *testing.T) {
	var iso belay.Isolator = newIsolator(t)

	ws, err := iso.Create(context.Background(), sampleTree(t), "cand")
	if err != nil {
		t.Fatalf("Create through the interface: %v", err)
	}
	if !ws.Ephemeral || ws.ID != "cand" || !filepath.IsAbs(ws.Dir) {
		t.Errorf("Workspace = %+v, want an ephemeral workspace with an absolute Dir", ws)
	}
	if err := iso.Destroy(context.Background(), ws); err != nil {
		t.Fatalf("Destroy through the interface: %v", err)
	}
	requireGone(t, ws.Dir, "after Destroy through the interface")
}

// TestErrorMessagesNameTheOffendingPath keeps the errors debuggable: a
// refusal a human cannot act on wastes the incident it was supposed to
// prevent.
func TestErrorMessagesNameTheOffendingPath(t *testing.T) {
	ce := &ContainmentError{Op: "destroy", Path: "/tmp/evil", Resolved: "/etc", Root: "/runs/candidates", Reason: "resolves outside the candidate root"}
	for _, want := range []string{"destroy", "/tmp/evil", "/etc", "/runs/candidates", "resolves outside"} {
		if !strings.Contains(ce.Error(), want) {
			t.Errorf("ContainmentError message %q does not mention %q", ce.Error(), want)
		}
	}
	if !errors.Is(ce, ErrRefused) {
		t.Error("ContainmentError does not wrap ErrRefused")
	}

	se := &SpaceError{Dir: "/runs/candidates/cand", NeedBytes: 100, FreeBytes: 10, ReserveBytes: 5}
	for _, want := range []string{"/runs/candidates/cand", "100", "10", "5"} {
		if !strings.Contains(se.Error(), want) {
			t.Errorf("SpaceError message %q does not mention %q", se.Error(), want)
		}
	}
	if !errors.Is(se, ErrInsufficientSpace) {
		t.Error("SpaceError does not wrap ErrInsufficientSpace")
	}

	if !errors.Is(&NotEmptyError{Dir: "/x"}, ErrNotEmpty) {
		t.Error("NotEmptyError does not wrap ErrNotEmpty")
	}
	if !errors.Is(&SourceError{Path: "/x"}, ErrInvalidSource) {
		t.Error("SourceError does not wrap ErrInvalidSource")
	}
}

// TestDefaultSkipIsACopy stops a caller mutating the package's own defaults.
func TestDefaultSkipIsACopy(t *testing.T) {
	first := DefaultSkip()
	if len(first) == 0 {
		t.Fatal("DefaultSkip returned nothing")
	}
	first[0] = "clobbered"
	if second := DefaultSkip(); second[0] == "clobbered" {
		t.Error("DefaultSkip hands out the package's own slice")
	}
}

// TestSymlinkPolicyString keeps log lines readable.
func TestSymlinkPolicyString(t *testing.T) {
	tests := map[SymlinkPolicy]string{
		SymlinkInternal:    "internal",
		SymlinkPreserveAll: "preserve-all",
		SymlinkSkip:        "skip",
		SymlinkPolicy(99):  "SymlinkPolicy(99)",
	}
	for policy, want := range tests {
		if got := policy.String(); got != want {
			t.Errorf("SymlinkPolicy(%d).String() = %q, want %q", int(policy), got, want)
		}
	}
}

// TestOwnedPathRefusesNonDirectories covers the remaining shape a Workspace
// can take inside the root: a plain file where a workspace should be.
func TestOwnedPathRefusesNonDirectories(t *testing.T) {
	iso := newIsolator(t)
	file := writeFile(t, filepath.Join(iso.Root(), "not-a-dir"), "x", 0o600)

	err := iso.Destroy(context.Background(), belay.Workspace{ID: "x", Dir: file, Ephemeral: true})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Destroy of a file = %v, want ErrRefused", err)
	}
	requireExists(t, file, "a refused Destroy")
}

// TestDestroyRefusesADirWithANULByte covers the last input-validation case.
func TestDestroyRefusesADirWithANULByte(t *testing.T) {
	iso := newIsolator(t)
	err := iso.Destroy(context.Background(), belay.Workspace{ID: "x", Dir: iso.Root() + "/a\x00b", Ephemeral: true})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Destroy with a NUL byte = %v, want ErrRefused", err)
	}
}

// requireNoEscapedFiles is a blunt backstop for the symlink tests: it walks
// the workspace and fails if anything in it resolves outside.
func requireNoEscapedFiles(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if path != dir && !isAncestorOf(dir, path) {
			t.Errorf("workspace contains %q, which is outside %q", path, dir)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", dir, err)
	}
}

// TestDestroyRefusesASymlinkedWorkspaceDir pins the branch that refuses a
// workspace Dir which is itself a symlink, by asserting the reason and not
// merely that something was refused.
//
// Without the reason assertion this case is satisfied by the "not a
// directory" fallback, and the symlink check could be deleted with every
// test still green.
func TestDestroyRefusesASymlinkedWorkspaceDir(t *testing.T) {
	iso := newIsolator(t)

	victim := mkdirAll(t, filepath.Join(t.TempDir(), "precious"))
	victimFile := writeFile(t, filepath.Join(victim, "thesis.txt"), "years of work", 0o600)
	link := symlink(t, victim, filepath.Join(iso.Root(), "escape"))

	err := iso.Destroy(context.Background(), belay.Workspace{ID: "evil", Dir: link, Ephemeral: true})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Destroy error = %v, want ErrRefused", err)
	}
	var ce *ContainmentError
	if !errors.As(err, &ce) {
		t.Fatalf("error is not a *ContainmentError: %v", err)
	}
	if !strings.Contains(ce.Reason, "symlink") {
		t.Errorf("ContainmentError.Reason = %q, want it to identify the symlink", ce.Reason)
	}
	requireExists(t, victim, "the directory the symlink pointed at")
	requireExists(t, victimFile, "the file inside it")
	requireExists(t, link, "the symlink itself was not followed and unlinked")
}

// TestDestroyRefusesAPathThatResolvesOutsideTheRoot covers the check no
// lexical rule can make, and the one reason this package resolves symlinks at
// all: the candidate root being swapped for a symlink after the Isolator
// resolved it.
//
// By name, "candidates/cand" is still a direct child of "candidates". On
// disk it is now somewhere else entirely. Only re-checking containment after
// EvalSymlinks catches that, which is why the check runs on the resolved
// path and not just the given one.
func TestDestroyRefusesAPathThatResolvesOutsideTheRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	iso, err := New(filepath.Join(base, "candidates"), WithLogger(quietLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	root := iso.Root()

	// A real tree elsewhere, holding a real candidate-shaped directory.
	elsewhere := mkdirAll(t, filepath.Join(base, "elsewhere"))
	victimDir := mkdirAll(t, filepath.Join(elsewhere, "cand"))
	victimFile := writeFile(t, filepath.Join(victimDir, "thesis.txt"), "years of work", 0o600)

	// Swap the candidate root itself for a symlink to that tree.
	if err := os.Remove(root); err != nil {
		t.Fatalf("Remove(%q): %v", root, err)
	}
	symlink(t, elsewhere, root)

	target := filepath.Join(root, "cand")
	// Everything lexical about this path still looks correct.
	if !isChildOf(root, target) {
		t.Fatalf("precondition: %q should look like a direct child of %q", target, root)
	}
	if info, err := os.Lstat(target); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("precondition: %q should look like an ordinary directory (err=%v)", target, err)
	}

	err = iso.Destroy(context.Background(), belay.Workspace{ID: "cand", Dir: target, Ephemeral: true})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Destroy error = %v, want ErrRefused", err)
	}
	var ce *ContainmentError
	if !errors.As(err, &ce) {
		t.Fatalf("error is not a *ContainmentError: %v", err)
	}
	if !strings.Contains(ce.Reason, "resolves outside") {
		t.Errorf("ContainmentError.Reason = %q, want it to name the resolution mismatch", ce.Reason)
	}

	requireExists(t, victimDir, "the tree behind the swapped root")
	requireExists(t, victimFile, "the file inside it")
	if got := readFile(t, victimFile); got != "years of work" {
		t.Errorf("content = %q, want it untouched", got)
	}
}
