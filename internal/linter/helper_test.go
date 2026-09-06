//go:build unix

package linter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dhaam-ai/belay/internal/exec"
)

// stubRunner is the CommandRunner every test in this package uses. It records
// the command it was handed and replays a scripted result without starting a
// process, which is what keeps the suite from ever invoking a real linter.
//
// Its return values mirror exec.Runner.Run exactly, because the adapters lean
// on those semantics: a Result comes back even on failure, and a non-zero exit
// is reported as *exec.ExitError rather than nil.
type stubRunner struct {
	stdout    string
	stderr    string
	exitCode  int
	truncated bool
	err       error

	mu    sync.Mutex
	calls []exec.Command
}

func (s *stubRunner) Run(_ context.Context, c exec.Command) (exec.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()

	res := exec.Result{
		Args:      append([]string{c.Path}, c.Args...),
		ExitCode:  s.exitCode,
		Stdout:    s.stdout,
		Stderr:    s.stderr,
		Truncated: s.truncated,
	}
	switch {
	case s.err != nil:
		return res, s.err
	case s.exitCode != 0:
		return res, &exec.ExitError{Args: res.Args, Code: s.exitCode, Stderr: s.stderr}
	default:
		return res, nil
	}
}

// lastCall returns the command the stub was most recently asked to run.
func (s *stubRunner) lastCall(t *testing.T) exec.Command {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("runner was never called")
	}
	return s.calls[len(s.calls)-1]
}

// callCount reports how many commands the stub was asked to run.
func (s *stubRunner) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// okRunner replays a clean exit-zero run printing stdout.
func okRunner(stdout string) *stubRunner {
	return &stubRunner{stdout: stdout}
}

// findingsRunner replays the run a linter performs when it has findings: the
// report on stdout, and a non-zero exit status.
func findingsRunner(stdout string, exitCode int) *stubRunner {
	return &stubRunner{stdout: stdout, exitCode: exitCode}
}

// missingRunner replays the tool not being installed, as exec reports it.
//
// It returns exec's own sentinel — the one that is NOT belay's — so every test
// built on it proves the adapter translates at the seam rather than passing
// the wrong sentinel through.
func missingRunner(tool string) *stubRunner {
	cause := errors.New(`exec: "` + tool + `": executable file not found in $PATH`)
	return &stubRunner{
		exitCode: -1,
		err:      &exec.ToolchainError{Tool: tool, Err: cause, Detail: cause.Error()},
	}
}

// quietLogger discards output so a failing test's diagnostics stay readable.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// readFixture returns the contents of testdata/name.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	// #nosec G304 -- the path is this package's own testdata directory.
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// writeFiles materializes a project tree for a Detect test. Keys are
// slash-separated paths relative to the returned directory.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}
