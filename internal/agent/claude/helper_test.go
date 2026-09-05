//go:build unix

package claude

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/belay-dev/belay/internal/exec"
	"github.com/belay-dev/belay/pkg/belay"
)

// No test in this package runs the real `claude` binary.
//
// Every invocation goes through Backend.Runner, and every test sets it to
// *fakeRunner, which replays bytes from testdata/ and starts no process at
// all. The one test that needs a real child process to prove redaction works
// end to end (TestAPIKeyRedactedByRealRunner) re-executes this test binary
// instead, via the helper mode below — the same technique internal/exec uses.
//
// Two independent things enforce this. The fake is the seam, and it records
// every command it was handed so a test can assert on argv without a process
// existing. Beyond that, `claude` is not on PATH in this environment, so even
// a bug that reached the real runner would fail with a missing toolchain
// rather than spend money.

const helperModeEnv = "BELAY_CLAUDE_HELPER"

// TestMain dispatches to helperMain when the harness re-executes this binary
// as a child process.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnv); mode != "" {
		helperMain(mode)
		return
	}
	os.Exit(m.Run())
}

// helperMain impersonates the CLI for the tests that need a real child.
func helperMain(mode string) {
	switch mode {
	case "leak":
		// Behave like an agent that prints its own credential: once into
		// the JSON result on stdout, once onto stderr. Both paths must be
		// redacted before they can reach a Result.
		key := os.Getenv(apiKeyEnv)
		fmt.Printf(`{"type":"result","subtype":"success","is_error":false,`+
			`"result":"the key is %s","session_id":"leak-session","num_turns":1,`+
			`"total_cost_usd":0.02,"usage":{"input_tokens":10,"output_tokens":5}}`+"\n", key)
		fmt.Fprintf(os.Stderr, "debug: %s=%s\n", apiKeyEnv, key)
	case "fail":
		fmt.Fprintln(os.Stderr, "error: unknown option '--allowedTool'")
		os.Exit(2)
	}
	os.Exit(0)
}

// testExe returns the absolute path of this test binary, used as the child.
func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// quietLogger returns a logger that discards everything, so a failing test
// reports an assertion rather than a wall of structured output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRunner is the subprocess seam standing in for *exec.Runner.
//
// It starts no process. Fn, when set, decides the outcome from the command;
// otherwise Result and Err are returned for every call. Every call is
// recorded.
type fakeRunner struct {
	Result exec.Result
	Err    error
	Fn     func(exec.Command) (exec.Result, error)

	mu    sync.Mutex
	calls []exec.Command
}

// Run implements Runner.
func (f *fakeRunner) Run(_ context.Context, c exec.Command) (exec.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()

	if f.Fn != nil {
		return f.Fn(c)
	}
	return f.Result, f.Err
}

// Calls returns every command Run received, in call order.
func (f *fakeRunner) Calls() []exec.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]exec.Command(nil), f.calls...)
}

// LastCall returns the most recent command, failing the test if there was none.
func (f *fakeRunner) LastCall(t *testing.T) exec.Command {
	t.Helper()
	calls := f.Calls()
	if len(calls) == 0 {
		t.Fatal("runner was never called")
	}
	return calls[len(calls)-1]
}

// newBackend returns a Backend wired to r. It is the only way the tests build
// a Backend, which is what keeps the real CLI out of the suite.
func newBackend(t *testing.T, r Runner) *Backend {
	t.Helper()
	if r == nil {
		t.Fatal("newBackend requires a Runner; a nil one would run the real claude CLI")
	}
	return &Backend{Runner: r, Logger: quietLogger()}
}

// okResult returns an exec.Result carrying stdout from a clean exit.
func okResult(stdout string) exec.Result {
	return exec.Result{Args: []string{"claude"}, ExitCode: 0, Stdout: stdout}
}

// fixture reads a hand-authored testdata file.
func fixture(t *testing.T, name string) string {
	t.Helper()
	// #nosec G304 -- name is a fixture filename written in this package's
	// own tests, not external input.
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// validRequest is a minimal request that passes validation.
func validRequest() belay.AgentRequest {
	return belay.AgentRequest{Prompt: "do the thing", WorkDir: "/repo"}
}

// almostEqual compares two dollar figures within a tolerance that float
// arithmetic on token counts cannot exceed.
func almostEqual(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	return d < eps && d > -eps
}
