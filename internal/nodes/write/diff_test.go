package write

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRenderPatch(t *testing.T) {
	root, _ := newTree(t)

	tests := []struct {
		name    string
		files   map[string]string
		exec    []string
		claimed []string
		want    string
	}{
		{
			name:    "empty change set renders zero bytes",
			claimed: nil,
			want:    "",
		},
		{
			name:    "a single text file",
			files:   map[string]string{"a.go": "package a\nvar X = 1\n"},
			claimed: []string{"a.go"},
			want: "diff --git a/a.go b/a.go\n" +
				"new file mode 100644\n" +
				"--- /dev/null\n" +
				"+++ b/a.go\n" +
				"@@ -0,0 +1,2 @@\n" +
				"+package a\n" +
				"+var X = 1\n",
		},
		{
			name:    "no trailing newline is marked",
			files:   map[string]string{"a.txt": "one\ntwo"},
			claimed: []string{"a.txt"},
			want: "diff --git a/a.txt b/a.txt\n" +
				"new file mode 100644\n" +
				"--- /dev/null\n" +
				"+++ b/a.txt\n" +
				"@@ -0,0 +1,2 @@\n" +
				"+one\n" +
				"+two\n" +
				"\\ No newline at end of file\n",
		},
		{
			name:    "an empty file gets a header and no hunk",
			files:   map[string]string{"empty.txt": ""},
			claimed: []string{"empty.txt"},
			want: "diff --git a/empty.txt b/empty.txt\n" +
				"new file mode 100644\n" +
				"--- /dev/null\n" +
				"+++ b/empty.txt\n",
		},
		{
			name:    "an executable file records mode 100755",
			files:   map[string]string{"run.sh": "#!/bin/sh\n"},
			exec:    []string{"run.sh"},
			claimed: []string{"run.sh"},
			want: "diff --git a/run.sh b/run.sh\n" +
				"new file mode 100755\n" +
				"--- /dev/null\n" +
				"+++ b/run.sh\n" +
				"@@ -0,0 +1,1 @@\n" +
				"+#!/bin/sh\n",
		},
		{
			name:    "a binary file is recorded by reference",
			files:   map[string]string{"blob.bin": "ab\x00cd"},
			claimed: []string{"blob.bin"},
			want: "diff --git a/blob.bin b/blob.bin\n" +
				"new file mode 100644\n" +
				"Binary files /dev/null and b/blob.bin differ\n",
		},
		{
			name:    "a deletion is a header with no hunk",
			claimed: []string{"gone.go"},
			want: "diff --git a/gone.go b/gone.go\n" +
				"deleted file mode 100644\n" +
				"--- a/gone.go\n" +
				"+++ /dev/null\n",
		},
		{
			name: "several files are rendered in sorted order",
			files: map[string]string{
				"z.go": "package z\n",
				"a.go": "package a\n",
			},
			claimed: []string{"z.go", "a.go"},
			want: "diff --git a/a.go b/a.go\n" +
				"new file mode 100644\n" +
				"--- /dev/null\n" +
				"+++ b/a.go\n" +
				"@@ -0,0 +1,1 @@\n" +
				"+package a\n" +
				"diff --git a/z.go b/z.go\n" +
				"new file mode 100644\n" +
				"--- /dev/null\n" +
				"+++ b/z.go\n" +
				"@@ -0,0 +1,1 @@\n" +
				"+package z\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(root, sanitize(tc.name))
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			sub, err := resolveRoot(dir)
			if err != nil {
				t.Fatalf("resolveRoot: %v", err)
			}
			for name, content := range tc.files {
				mustWrite(t, filepath.Join(sub, name), content)
			}
			for _, name := range tc.exec {
				// #nosec G302 -- the executable bit is what this case asserts.
				if err := os.Chmod(filepath.Join(sub, name), 0o755); err != nil {
					t.Fatalf("chmod: %v", err)
				}
			}

			changes, err := verify(context.Background(), mustRoot(t, sub), sub, tc.claimed)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			got, err := renderPatch(context.Background(), mustRoot(t, sub), sub, changes, DefaultMaxFileBytes)
			if err != nil {
				t.Fatalf("renderPatch: %v", err)
			}
			if diff := cmp.Diff(tc.want, string(got)); diff != "" {
				t.Errorf("renderPatch mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRenderPatchCapsLargeFiles(t *testing.T) {
	root, _ := newTree(t)
	mustWrite(t, filepath.Join(root, "big.txt"), strings.Repeat("x", 40)+"\n")

	changes, err := verify(context.Background(), mustRoot(t, root), root, []string{"big.txt"})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, err := renderPatch(context.Background(), mustRoot(t, root), root, changes, 10)
	if err != nil {
		t.Fatalf("renderPatch: %v", err)
	}
	want := "diff --git a/big.txt b/big.txt\n" +
		"new file mode 100644\n" +
		"Files /dev/null and b/big.txt differ (41 bytes exceeds the 10 byte record limit)\n"
	if diff := cmp.Diff(want, string(got)); diff != "" {
		t.Errorf("renderPatch mismatch (-want +got):\n%s", diff)
	}
}

func TestRenderPatchIsDeterministic(t *testing.T) {
	root, _ := newTree(t)
	mustWrite(t, filepath.Join(root, "a.go"), "package a\n")
	mustWrite(t, filepath.Join(root, "b.go"), "package b\n")

	claimed := []string{"b.go", "a.go", "missing.go"}
	changes, err := verify(context.Background(), mustRoot(t, root), root, claimed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	first, err := renderPatch(context.Background(), mustRoot(t, root), root, changes, DefaultMaxFileBytes)
	if err != nil {
		t.Fatalf("renderPatch: %v", err)
	}
	for i := range 5 {
		again, err := renderPatch(context.Background(), mustRoot(t, root), root, changes, DefaultMaxFileBytes)
		if err != nil {
			t.Fatalf("renderPatch %d: %v", i, err)
		}
		if diff := cmp.Diff(string(first), string(again)); diff != "" {
			t.Fatalf("renderPatch is not deterministic on run %d (-first +again):\n%s", i, diff)
		}
	}
}

func TestRenderPatchRespectsCancellation(t *testing.T) {
	root, _ := newTree(t)
	mustWrite(t, filepath.Join(root, "a.go"), "package a\n")
	changes, err := verify(context.Background(), mustRoot(t, root), root, []string{"a.go"})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := renderPatch(ctx, mustRoot(t, root), root, changes, DefaultMaxFileBytes); !errors.Is(err, context.Canceled) {
		t.Fatalf("renderPatch on a cancelled context = %v; want an error wrapping context.Canceled", err)
	}
}

func TestArtifactName(t *testing.T) {
	tests := []struct {
		step    int
		want    string
		wantErr bool
	}{
		{step: 0, want: "diff-0000.patch"},
		{step: 7, want: "diff-0007.patch"},
		{step: 1234, want: "diff-1234.patch"},
		{step: 99999, want: "diff-99999.patch"},
		{step: -1, wantErr: true},
	}
	for _, tc := range tests {
		got, err := artifactName(tc.step)
		if tc.wantErr {
			if err == nil {
				t.Errorf("artifactName(%d) = %q, want an error", tc.step, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("artifactName(%d): %v", tc.step, err)
			continue
		}
		if got != tc.want {
			t.Errorf("artifactName(%d) = %q, want %q", tc.step, got, tc.want)
		}
	}
}

// sanitize turns a subtest name into a single safe directory component.
func sanitize(name string) string {
	return strings.NewReplacer(" ", "-", "/", "-").Replace(name)
}
