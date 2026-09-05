package write

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/state"
)

// testRunID is a fixed, well-formed run ID, so every test's Layout — and
// therefore every derived workspace root — is deterministic.
const testRunID = "20260101T000000Z-0123456789ab"

// newRC returns a RunContext over a fresh workspace, together with that
// workspace's resolved root.
//
// The Layout is built the way state.NewLayout builds a real one, so the
// node's default workspace derivation (rootFromLayout) is exercised rather
// than bypassed: a test that pinned the root with WithWorkspaceRoot would
// never notice the derivation breaking.
func newRC(t *testing.T, step int, changed []string) (*graph.RunContext, string) {
	t.Helper()
	ws := t.TempDir()
	root, err := resolveRoot(ws)
	if err != nil {
		t.Fatalf("resolveRoot(%s): %v", ws, err)
	}
	layout, err := state.NewLayout(ws, testRunID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	return &graph.RunContext{
		Goal:     "make the tests pass",
		State:    state.State{Code: state.Code{SessionID: "sess-1", ChangedFiles: changed}},
		Layout:   layout,
		NodeName: graph.NodeWrite,
		Step:     step,
		Attempt:  1,
	}, root
}

// mustRoot opens dir as an os.Root for a test that exercises one of the
// package's internals directly, and closes it when the test ends.
func mustRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// hashTree fingerprints every entry under root: its relative path, its type
// and permission bits, and — for a regular file — its content.
//
// It is the instrument the re-run and cancellation tests are built on. A
// weaker check (comparing file lists, or mtimes) would pass for a node that
// rewrote a file with different content, which is exactly the failure those
// tests exist to catch. Symlinks are hashed by their target text rather than
// followed, so a link swapped for a copy is a difference.
//
// belay's own .belay/ directory is excluded. It sits inside the workspace and
// holds the run's artifacts and control state, which legitimately change every
// time a node runs; including it would make "the tree is unchanged" a claim no
// correct node could ever satisfy. What these tests assert is that the user's
// source tree is untouched — the artifacts are checked separately and by name.
func hashTree(t *testing.T, root string) string {
	t.Helper()
	type entry struct{ line string }
	var entries []entry

	// Walked through an fs.FS rooted at root rather than with
	// filepath.WalkDir: the walk never follows a symlink out of the tree,
	// and every read is resolved relative to the root rather than by
	// re-opening an absolute path that could have changed underneath the
	// walk. The fixture that measures containment should not itself be
	// escapable.
	fsys := os.DirFS(root)
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == belayDirName {
			return fs.SkipDir
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		line := fmt.Sprintf("%s\x00%s\x00%o", path, info.Mode().Type(), info.Mode().Perm())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, lerr := fs.ReadLink(fsys, path)
			if lerr != nil {
				return lerr
			}
			line += "\x00link:" + target
		case info.Mode().IsRegular():
			data, rerr := fs.ReadFile(fsys, path)
			if rerr != nil {
				return rerr
			}
			sum := sha256.Sum256(data)
			line += "\x00" + hex.EncodeToString(sum[:])
		}
		entries = append(entries, entry{line: line})
		return nil
	})
	if err != nil {
		t.Fatalf("hashTree(%s): %v", root, err)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].line < entries[j].line })
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.line))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readArtifact reads the patch artifact recorded at a run-relative path.
func readArtifact(t *testing.T, rc *graph.RunContext, rel string) string {
	t.Helper()
	// #nosec G304 -- rel is the path the node under test just returned,
	// joined onto a run directory the test created under t.TempDir.
	data, err := os.ReadFile(filepath.Join(rc.Layout.RunDir(), rel))
	if err != nil {
		t.Fatalf("read artifact %s: %v", rel, err)
	}
	return string(data)
}

// artifactExists reports whether a named artifact is present in the run's
// artifacts directory.
func artifactExists(t *testing.T, rc *graph.RunContext, name string) bool {
	t.Helper()
	path, err := rc.Layout.ArtifactPath(name)
	if err != nil {
		t.Fatalf("ArtifactPath(%s): %v", name, err)
	}
	_, err = os.Lstat(path)
	return err == nil
}

// countingCtx reports itself cancelled once Err has been consulted more than
// a set number of times.
//
// Cancelling a real context from another goroutine races the work it is meant
// to interrupt, so a test built that way either cancels too early to reach the
// interesting code or too late to interrupt it, and flakes either way. This
// makes the cancellation land at an exact, chosen point: Run checks ctx.Err
// once on entry, verify checks it once per claimed path, and renderPatch once
// per change, so a threshold picks the stage precisely.
type countingCtx struct {
	context.Context
	limit int
	calls atomic.Int64
	done  chan struct{}
}

// cancelAfter returns a context that reports Canceled from the (limit+1)-th
// call to Err.
func cancelAfter(limit int) *countingCtx {
	return &countingCtx{Context: context.Background(), limit: limit, done: make(chan struct{})}
}

// Err implements context.Context.
func (c *countingCtx) Err() error {
	if c.calls.Add(1) > int64(c.limit) {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
		return context.Canceled
	}
	return nil
}

// Done implements context.Context.
func (c *countingCtx) Done() <-chan struct{} { return c.done }
