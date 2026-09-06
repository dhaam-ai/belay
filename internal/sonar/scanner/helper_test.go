//go:build unix

package scanner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dhaam-ai/belay/internal/exec"
)

// testToken is the SONAR_TOKEN value every test injects.
//
// It is shaped like a real SonarQube user token (the squ_ prefix and length
// exec's redactor keys on) and is deliberately distinctive, because the whole
// point of TestTokenNeverLeaks is to grep for these bytes in everything the
// Scanner produces. The Execer is a stub, so exec's redactor never runs and a
// leak would show up verbatim rather than as a marker — which is the strictest
// version of the check.
//
//nolint:gosec // synthetic value; there is no such SonarQube instance.
const testToken = "squ_belaytesttoken0123456789abcdef01234567"

// testHostURL is the SONAR_HOST_URL value every test injects.
const testHostURL = "https://sonar.example.test"

// testProjectKey is the ReviewRequest.ProjectKey every test uses.
const testProjectKey = "belay-demo"

// TestMain empties PATH for the whole test binary in the default build. See
// isolatePATH, which is false only under the sonardocker tag.
func TestMain(m *testing.M) {
	if isolatePATH {
		if err := os.Setenv("PATH", ""); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

// stubExecer is the Execer every test uses. It records the commands it was
// handed and replays a scripted result without starting a process.
//
// Its return values mirror exec.Runner.Run exactly, because classify leans on
// those semantics: a Result comes back even on failure, and a non-zero exit is
// reported as *exec.ExitError rather than nil.
type stubExecer struct {
	stdout    string
	stderr    string
	exitCode  int
	truncated bool
	timedOut  bool
	err       error

	mu    sync.Mutex
	calls []exec.Command
}

func (s *stubExecer) Run(_ context.Context, c exec.Command) (exec.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()

	res := exec.Result{
		Args:      append([]string{c.Path}, c.Args...),
		ExitCode:  s.exitCode,
		Stdout:    s.stdout,
		Stderr:    s.stderr,
		Truncated: s.truncated,
		TimedOut:  s.timedOut,
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
func (s *stubExecer) lastCall(t *testing.T) exec.Command {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("execer was never called")
	}
	return s.calls[len(s.calls)-1]
}

// callCount reports how many commands the stub was asked to run.
func (s *stubExecer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// scanRunner replays a scanner that ran to completion with the given output
// and exit status.
func scanRunner(stdout string, exitCode int) *stubExecer {
	return &stubExecer{stdout: stdout, exitCode: exitCode}
}

// missingRunner replays the binary not being installed, as exec reports it.
//
// It returns exec's own sentinel — the one that is NOT belay's — so every test
// built on it proves the Scanner translates at the seam rather than passing
// the wrong sentinel through.
func missingRunner(tool string) *stubExecer {
	cause := errors.New(`exec: "` + tool + `": executable file not found in $PATH`)
	return &stubExecer{
		exitCode: -1,
		err:      &exec.ToolchainError{Tool: tool, Err: cause, Detail: cause.Error()},
	}
}

// timeoutRunner replays belay's own deadline elapsing and the process group
// being killed, as exec reports it.
func timeoutRunner(stdout string) *stubExecer {
	return &stubExecer{
		stdout:   stdout,
		exitCode: 128 + 9,
		timedOut: true,
		err:      &exec.TimeoutError{Args: []string{"docker"}, Timeout: DefaultQualityGateTimeout},
	}
}

// testEnv returns a lookup that reports SONAR_TOKEN and SONAR_HOST_URL set,
// and everything else unset.
func testEnv() func(string) (string, bool) {
	return envWith(map[string]string{EnvToken: testToken, EnvHostURL: testHostURL})
}

// envWith returns a lookup backed by vars.
func envWith(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// newTestScanner returns a Scanner wired to e, with the test environment and a
// silent logger, plus whatever extra options a test needs.
func newTestScanner(e Execer, opts ...Option) *Scanner {
	base := []Option{WithExecer(e), WithLogger(quietLogger()), WithLookupEnv(testEnv())}
	return New(append(base, opts...)...)
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

// TestNoRealSubprocessInEnabledTests fails if a default-tagged test file in
// this package builds a real subprocess runner.
//
// ADR 0008 promises the suite runs without SonarQube or Docker. TestMain's
// empty PATH enforces that at run time; this enforces it at review time, so
// the violation is named rather than showing up as a confusing lookup error.
// The Docker-requiring test lives behind the sonardocker build tag and is
// exempt.
func TestNoRealSubprocessInEnabledTests(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		// #nosec G304 -- the path is this package's own directory listing.
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(src)
		if strings.Contains(text, "//go:build unix && sonardocker") {
			continue
		}
		for _, banned := range []string{"exec.New(", "osexec.", "os/exec"} {
			if strings.Contains(text, banned) {
				t.Errorf("%s references %q; tests must reach subprocesses only through the Execer stub", name, banned)
			}
		}
	}
}
