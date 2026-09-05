package write

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestVerifyClassifiesAndOrders(t *testing.T) {
	root, _ := newTree(t)
	mustWrite(t, filepath.Join(root, "src", "main.go"), "package main\n")
	mustWrite(t, filepath.Join(root, "README.md"), "# hi\n")
	// #nosec G302 -- the executable bit is the point: classify must report
	// mode 0755 so renderPatch can emit "100755" rather than "100644". This
	// is a test fixture, not a file belay creates at runtime.
	if err := os.Chmod(filepath.Join(root, "src", "main.go"), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	got, err := verify(context.Background(), mustRoot(t, root), root, []string{
		"src/main.go",
		"README.md",
		"src/gone.go",   // deleted
		"./src/main.go", // duplicate after cleaning
		"src//main.go",  // duplicate after cleaning
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	want := []change{
		{Rel: "README.md", Abs: filepath.Join(root, "README.md"), Kind: changePresent, Mode: 0o600, Size: 5},
		{Rel: "src/gone.go", Abs: filepath.Join(root, "src", "gone.go"), Kind: changeAbsent},
		{Rel: "src/main.go", Abs: filepath.Join(root, "src", "main.go"), Kind: changePresent, Mode: 0o755, Size: 13},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("verify mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"README.md", "src/gone.go", "src/main.go"}, paths(got)); diff != "" {
		t.Errorf("paths mismatch (-want +got):\n%s", diff)
	}
}

func TestVerifyEmptyChangeSetIsNotAnError(t *testing.T) {
	root, _ := newTree(t)
	for _, tc := range []struct {
		name    string
		claimed []string
	}{
		{name: "nil", claimed: nil},
		{name: "empty", claimed: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := verify(context.Background(), mustRoot(t, root), root, tc.claimed)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("verify = %v, want empty", got)
			}
			if p := paths(got); p == nil {
				t.Error("paths(empty) = nil; must be non-nil so state.json holds [] and not null")
			}
		})
	}
}

func TestVerifyRefusesDirectories(t *testing.T) {
	root, _ := newTree(t)
	if err := os.MkdirAll(filepath.Join(root, "adir"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := verify(context.Background(), mustRoot(t, root), root, []string{"README.md", "adir"})
	if err == nil {
		t.Fatalf("verify(dir) = %v, want a refusal", got)
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("verify(dir) error = %v; want one wrapping ErrRefused", err)
	}
	if got != nil {
		t.Errorf("verify returned %v alongside a refusal; a refused set has no partial result", got)
	}
}

func TestVerifyRefusesTheWholeSetOnOneBadPath(t *testing.T) {
	root, _ := newTree(t)
	mustWrite(t, filepath.Join(root, "good.go"), "package good\n")

	got, err := verify(context.Background(), mustRoot(t, root), root, []string{"good.go", "../escape.go"})
	if err == nil {
		t.Fatalf("verify = %v, want a refusal", got)
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("verify error = %v; want one wrapping ErrRefused", err)
	}
	if got != nil {
		t.Errorf("verify returned %v; one bad path refuses the whole set, it does not filter it", got)
	}
}

func TestVerifyRespectsCancellation(t *testing.T) {
	root, _ := newTree(t)
	mustWrite(t, filepath.Join(root, "a.go"), "package a\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := verify(ctx, mustRoot(t, root), root, []string{"a.go"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("verify on a cancelled context = (%v, %v); want an error wrapping context.Canceled", got, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
