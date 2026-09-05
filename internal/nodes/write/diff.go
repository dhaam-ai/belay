package write

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// DefaultMaxFileBytes is how much of a single file the patch artifact will
// quote before recording it by reference instead. A run directory is kept
// alongside a repository and read by humans; one generated file should not be
// able to turn it into the largest thing in the checkout.
const DefaultMaxFileBytes int64 = 1 << 20 // 1 MiB

// artifactName returns the name of the patch artifact for a node execution at
// step.
//
// Numbering by step, not by node name, is what stops a fix-loop retry from
// overwriting the record of the attempt before it: each execution occupies
// its own step, so diff-0007.patch stays exactly as attempt one wrote it once
// attempt two has written diff-0009.patch. The spelling matches the example
// in state.Code.LastDiff's own documentation.
func artifactName(step int) (string, error) {
	if step < 0 {
		return "", fmt.Errorf("write: step %d is negative", step)
	}
	return fmt.Sprintf("diff-%04d.patch", step), nil
}

// renderPatch renders changes as a git-style unified diff.
//
// # Why every present file is rendered as a new file
//
// belay keeps no snapshot of the workspace from before the agent ran, so this
// node has no baseline to subtract. Rather than invent a delta it cannot
// know, the artifact records what is actually on disk: each changed file is
// emitted against /dev/null, which is a true statement ("here is the content
// of every file the change set names") in a format existing tooling already
// reads. A deletion is emitted as a header with no hunk, because the removed
// content is exactly the thing no longer available to quote.
//
// The output is a function of the change set and the bytes on disk alone —
// no timestamps, no map iteration, no absolute paths — so two executions over
// an unchanged tree produce byte-identical artifacts. That determinism is not
// cosmetic: it is what makes a crash re-run of this node a no-op rather than
// a second, differently-worded record of the same event.
//
// An empty change set renders as zero bytes, which is both an honest record
// of "nothing was claimed" and a patch that applies cleanly as a no-op.
func renderPatch(ctx context.Context, root *os.Root, rootPath string, changes []change, maxBytes int64) ([]byte, error) {
	var buf bytes.Buffer
	for _, c := range changes {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("write: render patch: %w", err)
		}
		if err := renderChange(&buf, root, rootPath, c, maxBytes); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// renderChange writes one change's section of the patch.
func renderChange(buf *bytes.Buffer, root *os.Root, rootPath string, c change, maxBytes int64) error {
	fmt.Fprintf(buf, "diff --git a/%s b/%s\n", c.Rel, c.Rel)

	if c.Kind == changeAbsent {
		// The mode of a file that is gone is not knowable; 100644 is what git
		// itself records for a deletion whose blob it can no longer stat.
		fmt.Fprintf(buf, "deleted file mode 100644\n--- a/%s\n+++ /dev/null\n", c.Rel)
		return nil
	}

	fmt.Fprintf(buf, "new file mode %s\n", gitMode(c))

	if c.Size > maxBytes {
		fmt.Fprintf(buf, "Files /dev/null and b/%s differ (%d bytes exceeds the %d byte record limit)\n",
			c.Rel, c.Size, maxBytes)
		return nil
	}

	content, truncated, err := readCapped(root, c.Rel, maxBytes)
	if err != nil {
		if isEscape(err) {
			// The path passed every check in classify and stopped being
			// contained before it could be read. That is the swap the
			// os.Root layer exists to catch, and it is a refusal, not an
			// I/O failure: something changed the tree mid-execution.
			return refuseEscape(c.Rel, rootPath, "while reading it")
		}
		return err
	}
	if truncated {
		// The file grew between the stat in classify and the read here. The
		// artifact says so rather than quoting a prefix as if it were whole.
		fmt.Fprintf(buf, "Files /dev/null and b/%s differ (grew past the %d byte record limit while being read)\n",
			c.Rel, maxBytes)
		return nil
	}
	if bytes.IndexByte(content, 0) >= 0 {
		// git's own spelling, and its own heuristic: a NUL byte means the
		// content is not text and quoting it would corrupt the artifact.
		fmt.Fprintf(buf, "Binary files /dev/null and b/%s differ\n", c.Rel)
		return nil
	}

	fmt.Fprintf(buf, "--- /dev/null\n+++ b/%s\n", c.Rel)
	writeHunk(buf, string(content))
	return nil
}

// gitMode renders a file's mode the way a git patch header spells it.
func gitMode(c change) string {
	if c.Mode&0o111 != 0 {
		return "100755"
	}
	return "100644"
}

// writeHunk writes the single all-additions hunk for content.
//
// An empty file gets no hunk at all, which is what git emits for an empty new
// file: "@@ -0,0 +0,0 @@" is not a hunk any parser accepts.
func writeHunk(buf *bytes.Buffer, content string) {
	if content == "" {
		return
	}
	endsWithNewline := strings.HasSuffix(content, "\n")
	body := content
	if endsWithNewline {
		body = strings.TrimSuffix(body, "\n")
	}
	lines := strings.Split(body, "\n")

	fmt.Fprintf(buf, "@@ -0,0 +1,%d @@\n", len(lines))
	for _, line := range lines {
		buf.WriteString("+")
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	if !endsWithNewline {
		buf.WriteString("\\ No newline at end of file\n")
	}
}

// readCapped reads at most maxBytes from path, reporting whether the file
// held more than that.
//
// The cap is applied by the reader rather than trusted from the earlier stat:
// a file can grow between being classified and being read, and a node whose
// memory ceiling depends on a stale size is a node an agent can make allocate
// whatever it likes.
func readCapped(root *os.Root, path string, maxBytes int64) (content []byte, truncated bool, err error) {
	// Opened through the Root, so the kernel resolves path inside the
	// workspace and refuses any traversal that leaves it. That is what makes
	// this read safe even though the workspace may still be under an agent's
	// control: a file that turns into a symlink out of the tree between
	// classify and here is refused at open time, not followed.
	f, err := root.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("write: read %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("write: close %s: %w", path, cerr)
		}
	}()

	buf, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("write: read %s: %w", path, err)
	}
	if int64(len(buf)) > maxBytes {
		return nil, true, nil
	}
	return buf, false, nil
}
