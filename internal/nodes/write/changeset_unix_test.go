//go:build unix

package write

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

// TestVerifyRefusesIrregularFiles covers the filesystem objects only a Unix
// host can create. A FIFO in a change set is not a code change, and reading
// one would block the node forever — which is precisely why classify refuses
// anything that is not a regular file rather than trying to render it.
func TestVerifyRefusesIrregularFiles(t *testing.T) {
	root, _ := newTree(t)
	fifo := filepath.Join(root, "afifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	got, err := verify(context.Background(), mustRoot(t, root), root, []string{"afifo"})
	if err == nil {
		t.Fatalf("verify(fifo) = %v, want a refusal", got)
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("verify(fifo) error = %v; want one wrapping ErrRefused", err)
	}
	if got != nil {
		t.Errorf("verify returned %v alongside a refusal; a refused set has no partial result", got)
	}
}
