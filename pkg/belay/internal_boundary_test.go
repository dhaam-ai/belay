package belay_test

import (
	"bytes"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// TestNoInternalImports enforces belay's hard packaging rule: pkg/belay is
// the module's public, semver-stable contract and must not import anything
// — directly or transitively, including in its own tests — from the
// module's internal/ tree. An internal/ dependency would mean pkg/belay's
// own compatibility silently rode on code that carries no compatibility
// promise at all.
//
// Go's own internal-package visibility rule does not catch this on its
// own: it only stops packages OUTSIDE this module from importing
// internal/. Anything inside github.com/dhaam-ai/belay — including
// pkg/belay itself — is freely allowed by the compiler to import
// internal/, precisely because they share a module. This test is what
// actually enforces the boundary, by inspecting the real build graph with
// `go list` (golang.org/x/tools is not a declared dependency) rather than a
// textual grep a differently formatted import block could dodge.
func TestNoInternalImports(t *testing.T) {
	t.Parallel()

	goBin := findGoBin(t)

	modPath := strings.TrimSpace(runGoList(t, goBin, "-m"))
	if modPath == "" {
		t.Fatal("go list -m returned an empty module path")
	}
	internalPrefix := modPath + "/internal"

	// -test pulls in the packages' own test-only imports too, so a
	// violation hiding in a _test.go file inside pkg/belay is caught just
	// as reliably as one in production code.
	out := runGoList(t, goBin, "-deps", "-test", "-f", "{{.ImportPath}}", "./...")

	var violations []string
	for _, imp := range strings.Fields(out) {
		if imp == internalPrefix || strings.HasPrefix(imp, internalPrefix+"/") {
			violations = append(violations, imp)
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("pkg/belay (or a test within it) transitively imports the following internal/ package(s), which is forbidden — pkg/belay is belay's public contract and internal/ carries no compatibility promise:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// findGoBin locates the go tool on PATH. The test binary that runs
// TestNoInternalImports was itself produced by invoking `go`, so PATH
// carrying a resolvable "go" is the normal case; runtime.GOROOT() would be
// a fallback for the rare exception, but it is deprecated as of Go 1.24
// precisely because the baked-in path it returns is not guaranteed to
// point at a real "go" binary on the machine actually running the test, so
// this deliberately does not fall back to it — an environment where PATH
// has no "go" at all is not one this test can meaningfully run in anyway.
func findGoBin(t *testing.T) string {
	t.Helper()

	p, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not found on PATH; skipping internal/ boundary check")
	}
	return p
}

// runGoList runs `go list <args>` and returns its stdout, failing the test
// with stderr attached if the command does not exit cleanly.
func runGoList(t *testing.T, goBin string, args ...string) string {
	t.Helper()

	// goBin came from exec.LookPath above and args are this file's own
	// string-literal constants — never user input — so the subprocess
	// gosec flags by default is exactly the `go list` call this test is
	// for.
	cmd := exec.Command(goBin, append([]string{"list"}, args...)...) //nolint:gosec
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list %s failed: %v\nstderr:\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}
