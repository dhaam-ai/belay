//go:build fixrate

package fixrate

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

// postRunLintTimeout bounds the harness's own post-run golangci-lint
// invocation.
const postRunLintTimeout = 2 * time.Minute

// runGolangciLint runs golangci-lint against dir using the .golangci.yml
// fixtures/seeded-bug/inject.go already wrote there, exactly as that
// package's own runGolangciLint does — see its doc comment. available is
// false, with an empty output and no error, when golangci-lint is not on
// PATH: lint verification is best-effort here too, for the same reason
// it is in the injector (go build/go test are the two checks every
// environment can run).
func runGolangciLint(ctx context.Context, dir string) (output string, available bool) {
	path, err := exec.LookPath("golangci-lint")
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, postRunLintTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "run", "--max-same-issues=0", "--max-issues-per-linter=0", "./...") //nolint:gosec // path from exec.LookPath; dir is this harness's own temp workspace
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run() // a non-zero exit means issues were found, which is expected, not a tool failure
	return out.String(), true
}

// lintStillFires reports whether output names both d's file and its
// linter, in golangci-lint's default text report format ("path/to/file.go
// ... (linterName)") — the identical check fixtures/seeded-bug/inject.go's
// own lintMatches performs, applied here to the POST-run output instead
// of the pre-run one.
func lintStillFires(output string, d InjectedDefect) bool {
	return strings.Contains(output, d.File) && strings.Contains(output, "("+d.Linter+")")
}
