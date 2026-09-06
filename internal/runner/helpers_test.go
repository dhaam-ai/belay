//go:build unix

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestMain empties PATH for the whole test binary.
//
// This is the mechanical guarantee behind the claim that no test in this
// package invokes a real toolchain. Every runner test injects a fakeExec, so
// nothing should reach os/exec in the first place; emptying PATH means that if
// one ever did — a refactor that forgets the seam, a nil Execer defaulting to
// a real *exec.Runner — the lookup fails instead of quietly shelling out to
// whatever go, node or pytest happens to be installed on the machine running
// the suite. It also lets the toolchain-missing tests exercise a real
// *exec.Runner without starting a process: exec.LookPath fails before fork.
//
// Nothing in the test binary needs PATH after it has started, so this costs
// nothing.
func TestMain(m *testing.M) {
	if err := os.Setenv("PATH", ""); err != nil {
		panic("runner tests: cannot clear PATH: " + err.Error())
	}
	os.Exit(m.Run())
}

// quiet returns a logger that discards everything, so a table test's output
// is its assertions rather than the runner's own INFO lines.
func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeExec is the Execer every runner test injects. It replays a fixture and
// records the command it was asked to run, without starting a process.
type fakeExec struct {
	stdout   string
	stderr   string
	exitCode int
	duration time.Duration
	err      error

	// calls records every command, so tests can assert the argv a runner
	// built without observing its side effects.
	calls []bexec.Command
}

// Run implements Execer.
func (f *fakeExec) Run(_ context.Context, c bexec.Command) (bexec.Result, error) {
	f.calls = append(f.calls, c)
	res := bexec.Result{
		Args:     append([]string{c.Path}, c.Args...),
		ExitCode: f.exitCode,
		Stdout:   f.stdout,
		Stderr:   f.stderr,
		Duration: f.duration,
	}
	return res, f.err
}

// only returns the single command the fake was asked to run, failing the test
// if it was not asked for exactly one.
func (f *fakeExec) only(t *testing.T) bexec.Command {
	t.Helper()
	if len(f.calls) != 1 {
		t.Fatalf("Run called %d times, want 1", len(f.calls))
	}
	return f.calls[0]
}

// argv renders the single recorded command as a program-first slice.
func (f *fakeExec) argv(t *testing.T) []string {
	t.Helper()
	c := f.only(t)
	return append([]string{c.Path}, c.Args...)
}

// execOK returns a fake whose command exits zero with the given stdout.
func execOK(stdout string) *fakeExec {
	return &fakeExec{stdout: stdout, duration: 42 * time.Millisecond}
}

// execExit returns a fake whose command ran and exited non-zero, which is what
// internal/exec reports as *ExitError. This is the shape of a failing test
// suite, and no runner may treat it as a Go error.
func execExit(stdout string, code int) *fakeExec {
	return &fakeExec{
		stdout:   stdout,
		exitCode: code,
		duration: 42 * time.Millisecond,
		err:      &bexec.ExitError{Args: []string{"tool"}, Code: code},
	}
}

// execMissing returns a fake whose binary could not be found, which is what
// internal/exec reports as its own *ToolchainError. Translating this into
// belay's sentinel is the seam every runner has to get right.
func execMissing(tool string) *fakeExec {
	return &fakeExec{
		exitCode: -1,
		err: &bexec.ToolchainError{
			Tool:   tool,
			Err:    os.ErrNotExist,
			Detail: `exec: "` + tool + `": executable file not found in $PATH`,
		},
	}
}

// execStartFailure returns a fake whose command could not start for a reason
// that is not a missing binary — an unusable working directory, say. That is
// the "could not run the tests at all" case.
func execStartFailure() *fakeExec {
	return &fakeExec{
		exitCode: -1,
		err:      &bexec.StartError{Tool: "tool", Err: os.ErrPermission, Detail: "permission denied"},
	}
}

// fixture reads a testdata file. See testdata/README.md for provenance.
func fixture(t *testing.T, name string) string {
	t.Helper()
	//nolint:gosec // name is a literal fixture name chosen by the test itself.
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// writeFiles creates a temporary directory containing the named files.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// goModuleDir returns a directory that internal/detect recognizes as a Go
// module with the given module path.
func goModuleDir(t *testing.T, module string) string {
	t.Helper()
	return writeFiles(t, map[string]string{
		"go.mod": "module " + module + "\n\ngo 1.26\n",
	})
}

// pythonProjectDir returns a directory internal/detect recognizes as a Python
// project that declares pytest.
func pythonProjectDir(t *testing.T) string {
	t.Helper()
	return writeFiles(t, map[string]string{
		"pyproject.toml": "[project]\nname = \"demo\"\n\n[tool.pytest.ini_options]\ntestpaths = [\"tests\"]\n",
	})
}

// nodePackageDir returns a directory internal/detect recognizes as a Node
// package. testScript and devDeps drive framework detection; lockfile, when
// non-empty, drives package-manager detection.
func nodePackageDir(t *testing.T, testScript string, devDeps []string, lockfile string) string {
	t.Helper()
	pkg := map[string]any{"name": "demo", "version": "1.0.0"}
	if testScript != "" {
		pkg["scripts"] = map[string]string{"test": testScript}
	}
	if len(devDeps) > 0 {
		deps := make(map[string]string, len(devDeps))
		for _, d := range devDeps {
			deps[d] = "^1.0.0"
		}
		pkg["devDependencies"] = deps
	}
	encoded, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		t.Fatalf("encode package.json: %v", err)
	}
	files := map[string]string{"package.json": string(encoded)}
	if lockfile != "" {
		files[lockfile] = "# fixture lockfile\n"
	}
	return writeFiles(t, files)
}

// wantReport is the subset of a TestReport a table test asserts on. Duration
// and Raw are excluded: one is wall-clock, the other is the whole captured
// stream, and neither says anything about the parse.
type wantReport struct {
	total    int
	passed   int
	failed   int
	failures []belay.TestFailure
}

// assertReport compares a report against the expected counts and failures.
func assertReport(t *testing.T, got belay.TestReport, want wantReport) {
	t.Helper()
	if got.Total != want.total || got.Passed != want.passed || got.Failed != want.failed {
		t.Errorf("counts = total %d, passed %d, failed %d; want total %d, passed %d, failed %d",
			got.Total, got.Passed, got.Failed, want.total, want.passed, want.failed)
	}
	if len(got.Failures) != len(want.failures) {
		t.Fatalf("got %d failures, want %d:\n got: %+v\nwant: %+v",
			len(got.Failures), len(want.failures), got.Failures, want.failures)
	}
	for i, w := range want.failures {
		g := got.Failures[i]
		if g.Name != w.Name || g.File != w.File || g.Line != w.Line {
			t.Errorf("failure %d = {Name:%q File:%q Line:%d}; want {Name:%q File:%q Line:%d}",
				i, g.Name, g.File, g.Line, w.Name, w.File, w.Line)
		}
		if w.Message != "" && g.Message != w.Message {
			t.Errorf("failure %d message =\n%q\nwant\n%q", i, g.Message, w.Message)
		}
	}
}

// assertToolchainMissing is acceptance criterion 4, factored so every runner
// asserts exactly the same thing: the error must satisfy belay's sentinel, not
// internal/exec's, and must carry the tool name a human has to install.
func assertToolchainMissing(t *testing.T, err error, tool string) {
	t.Helper()
	if err == nil {
		t.Fatal("Test() error = nil, want a missing-toolchain error")
	}
	if !errors.Is(err, belay.ErrToolchainMissing) {
		t.Fatalf("errors.Is(err, belay.ErrToolchainMissing) = false; err = %v (%T)", err, err)
	}
	var te *belay.ToolchainError
	if !errors.As(err, &te) {
		t.Fatalf("errors.As(err, **belay.ToolchainError) = false; err = %v (%T)", err, err)
	}
	if te.Tool != tool {
		t.Errorf("ToolchainError.Tool = %q, want %q", te.Tool, tool)
	}
	var runErr *RunError
	if errors.As(err, &runErr) {
		t.Error("a missing toolchain must not also be a *RunError")
	}
}
