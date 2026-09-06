//go:build unix

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestExecSentinelsAreDistinct states the trap this package exists to avoid.
// If this ever fails, the translation seam has become unnecessary — and if it
// keeps passing, every runner must keep translating.
func TestExecSentinelsAreDistinct(t *testing.T) {
	raw := error(&bexec.ToolchainError{Tool: "go", Err: os.ErrNotExist, Detail: "not found"})
	if !errors.Is(raw, bexec.ErrToolchainMissing) {
		t.Fatal("exec.ToolchainError no longer satisfies exec.ErrToolchainMissing")
	}
	if errors.Is(raw, belay.ErrToolchainMissing) {
		t.Fatal("exec's error now satisfies belay's sentinel; the seam would be dead code")
	}
}

func TestToolchainError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantNil  bool
		wantTool string
	}{
		{
			name:     "exec toolchain error is translated",
			err:      &bexec.ToolchainError{Tool: "go", Err: os.ErrNotExist, Detail: "d"},
			wantTool: "pytest",
		},
		{
			name: "wrapped exec toolchain error is still found",
			err: errors.Join(errors.New("context"),
				&bexec.ToolchainError{Tool: "go", Err: os.ErrNotExist, Detail: "d"}),
			wantTool: "pytest",
		},
		{name: "exit error is not a toolchain problem", err: &bexec.ExitError{Code: 1}, wantNil: true},
		{name: "start error is not a toolchain problem", err: &bexec.StartError{Err: os.ErrPermission}, wantNil: true},
		{name: "timeout is not a toolchain problem", err: &bexec.TimeoutError{Timeout: time.Second}, wantNil: true},
		{name: "context cancellation is not a toolchain problem", err: context.Canceled, wantNil: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := toolchainError("pytest", tc.err)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("toolchainError() = %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, belay.ErrToolchainMissing) {
				t.Fatalf("translated error does not satisfy belay.ErrToolchainMissing: %v", got)
			}
			var te *belay.ToolchainError
			if !errors.As(got, &te) || te.Tool != tc.wantTool {
				t.Fatalf("translated error = %v, want *belay.ToolchainError with Tool %q", got, tc.wantTool)
			}
			if !errors.Is(got, os.ErrNotExist) {
				t.Error("translation dropped the underlying lookup failure")
			}
		})
	}
}

// TestToolchainMissingAcrossRunners is acceptance criterion 4 as one table:
// every runner must surface belay's sentinel, naming the binary a human has to
// install, when its own binary is absent.
func TestToolchainMissingAcrossRunners(t *testing.T) {
	tests := []struct {
		name string
		tool string
		make func(t *testing.T, x Execer) (belay.TestRunner, string)
	}{
		{
			name: "go", tool: "go",
			make: func(t *testing.T, x Execer) (belay.TestRunner, string) {
				return NewGo(x, quiet()), goModuleDir(t, "example.com/x")
			},
		},
		{
			name: "node", tool: "npm",
			make: func(t *testing.T, x Execer) (belay.TestRunner, string) {
				return NewNode(x, quiet()), nodePackageDir(t, "jest", []string{"jest"}, "package-lock.json")
			},
		},
		{
			name: "pytest", tool: "pytest",
			make: func(t *testing.T, x Execer) (belay.TestRunner, string) {
				return NewPytest(x, quiet()), pythonProjectDir(t)
			},
		},
		{
			name: "custom", tool: "make",
			make: func(t *testing.T, x Execer) (belay.TestRunner, string) {
				return NewCustom("make test", x, quiet()), writeFiles(t, nil)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner, dir := tc.make(t, execMissing(tc.tool))
			report, err := runner.Test(context.Background(), dir)
			assertToolchainMissing(t, err, tc.tool)
			if report.Total != 0 || report.Failed != 0 {
				t.Errorf("report = %+v, want the zero report; a missing toolchain is not a test result", report)
			}
		})
	}
}

// TestToolchainMissingThroughRealExecRunner exercises the same criterion
// through the production *exec.Runner rather than the fake, proving the
// translation survives the real error type. TestMain has emptied PATH, so the
// lookup fails before any process is created: no toolchain is invoked.
func TestToolchainMissingThroughRealExecRunner(t *testing.T) {
	if os.Getenv("PATH") != "" {
		t.Fatal("TestMain should have emptied PATH; refusing to risk a real invocation")
	}
	tests := []struct {
		name string
		tool string
		make func(t *testing.T) (belay.TestRunner, string)
	}{
		{
			name: "go", tool: "go",
			make: func(t *testing.T) (belay.TestRunner, string) {
				return NewGo(nil, quiet()), goModuleDir(t, "example.com/x")
			},
		},
		{
			name: "pytest", tool: "pytest",
			make: func(t *testing.T) (belay.TestRunner, string) {
				return NewPytest(nil, quiet()), pythonProjectDir(t)
			},
		},
		{
			name: "node", tool: "npm",
			make: func(t *testing.T) (belay.TestRunner, string) {
				return NewNode(nil, quiet()), nodePackageDir(t, "jest", []string{"jest"}, "package-lock.json")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner, dir := tc.make(t)
			_, err := runner.Test(context.Background(), dir)
			assertToolchainMissing(t, err, tc.tool)
		})
	}
}

// TestFailingRunIsNeverAnError is acceptance criterion 7 as one table across
// all four runners: a suite that ran and failed returns (report, nil).
func TestFailingRunIsNeverAnError(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T) (belay.TestRunner, string)
	}{
		{
			name: "go",
			make: func(t *testing.T) (belay.TestRunner, string) {
				return NewGo(execExit(fixture(t, "go-fail.jsonl"), 1), quiet()),
					goModuleDir(t, "example.com/fail")
			},
		},
		{
			name: "go build error",
			make: func(t *testing.T) (belay.TestRunner, string) {
				return NewGo(execExit(fixture(t, "go-build-error.jsonl"), 1), quiet()),
					goModuleDir(t, "example.com/buildfail")
			},
		},
		{
			name: "node jest",
			make: func(t *testing.T) (belay.TestRunner, string) {
				return NewNode(execExit(fixture(t, "jest-fail.json"), 1), quiet()),
					nodePackageDir(t, "jest", []string{"jest"}, "package-lock.json")
			},
		},
		{
			name: "node vitest",
			make: func(t *testing.T) (belay.TestRunner, string) {
				return NewNode(execExit(fixture(t, "vitest-fail.json"), 1), quiet()),
					nodePackageDir(t, "vitest run", []string{"vitest"}, "pnpm-lock.yaml")
			},
		},
		{
			name: "node:test",
			make: func(t *testing.T) (belay.TestRunner, string) {
				return NewNode(execExit(fixture(t, "nodetest-tap-fail.txt"), 1), quiet()),
					nodePackageDir(t, "node --test", nil, "package-lock.json")
			},
		},
		{
			name: "pytest",
			make: func(t *testing.T) (belay.TestRunner, string) {
				return NewPytest(execExit(fixture(t, "pytest-fail.txt"), 1), quiet()),
					pythonProjectDir(t)
			},
		},
		{
			name: "custom",
			make: func(t *testing.T) (belay.TestRunner, string) {
				return NewCustom("make test", execExit("boom\n", 1), quiet()), writeFiles(t, nil)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner, dir := tc.make(t)
			report, err := runner.Test(context.Background(), dir)
			if err != nil {
				t.Fatalf("Test() returned error %v; a failing run is a report, not an error", err)
			}
			if report.Failed == 0 {
				t.Errorf("report = %+v, want Failed > 0", report)
			}
			if report.OK() {
				t.Error("report.OK() = true for a failing run")
			}
			if errors.Is(err, belay.ErrToolchainMissing) {
				t.Error("a failing run must never look like a missing toolchain")
			}
		})
	}
}

// TestPassingRunIsOKAcrossRunners is the other half: a green suite is OK and
// carries no failures.
func TestPassingRunIsOKAcrossRunners(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T) (belay.TestRunner, string)
	}{
		{"go", func(t *testing.T) (belay.TestRunner, string) {
			return NewGo(execOK(fixture(t, "go-pass.jsonl")), quiet()), goModuleDir(t, "example.com/pass")
		}},
		{"node jest", func(t *testing.T) (belay.TestRunner, string) {
			return NewNode(execOK(fixture(t, "jest-pass.json")), quiet()),
				nodePackageDir(t, "jest", []string{"jest"}, "package-lock.json")
		}},
		{"node vitest", func(t *testing.T) (belay.TestRunner, string) {
			return NewNode(execOK(fixture(t, "vitest-pass.json")), quiet()),
				nodePackageDir(t, "vitest run", []string{"vitest"}, "yarn.lock")
		}},
		{"pytest", func(t *testing.T) (belay.TestRunner, string) {
			return NewPytest(execOK(fixture(t, "pytest-pass.txt")), quiet()), pythonProjectDir(t)
		}},
		{"custom", func(t *testing.T) (belay.TestRunner, string) {
			return NewCustom("make test", execOK("ok\n"), quiet()), writeFiles(t, nil)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner, dir := tc.make(t)
			report, err := runner.Test(context.Background(), dir)
			if err != nil {
				t.Fatalf("Test() error = %v, want nil", err)
			}
			if !report.OK() {
				t.Errorf("report.OK() = false for a passing run: %+v", report)
			}
			if len(report.Failures) != 0 {
				t.Errorf("failures = %+v, want none", report.Failures)
			}
			if report.Duration == 0 {
				t.Error("Duration = 0; the report should carry the command's wall time")
			}
			var raw string
			if err := json.Unmarshal(report.Raw, &raw); err != nil {
				t.Errorf("Raw is not a JSON string: %v", err)
			}
		})
	}
}

func TestIsBuildFailure(t *testing.T) {
	tests := []struct {
		name   string
		report belay.TestReport
		want   bool
	}{
		{name: "empty report", report: belay.TestReport{}},
		{
			name: "assertion failures only",
			report: belay.TestReport{Failures: []belay.TestFailure{
				{Name: "TestThing"}, {Name: "TestOther"},
			}},
		},
		{
			name: "build failure alone",
			report: belay.TestReport{Failures: []belay.TestFailure{
				{Name: BuildFailureName},
			}},
			want: true,
		},
		{
			name: "build failure alongside real failures",
			report: belay.TestReport{Failures: []belay.TestFailure{
				{Name: BuildFailureName}, {Name: "TestThing"},
			}},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsBuildFailure(tc.report); got != tc.want {
				t.Errorf("IsBuildFailure() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSelect(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.Test
		dir      func(t *testing.T) string
		wantName string
		wantErr  error
	}{
		{
			name: "explicit go", cfg: config.Test{Runner: config.TestRunnerGo},
			dir: func(t *testing.T) string { return writeFiles(t, nil) }, wantName: GoName,
		},
		{
			name: "explicit node", cfg: config.Test{Runner: config.TestRunnerNode},
			dir: func(t *testing.T) string { return writeFiles(t, nil) }, wantName: NodeName,
		},
		{
			name: "explicit python", cfg: config.Test{Runner: config.TestRunnerPython},
			dir: func(t *testing.T) string { return writeFiles(t, nil) }, wantName: PytestName,
		},
		{
			name: "custom with a command",
			cfg:  config.Test{Runner: config.TestRunnerCustom, CustomCmd: "make test"},
			dir:  func(t *testing.T) string { return writeFiles(t, nil) }, wantName: CustomName,
		},
		{
			name:    "custom without a command",
			cfg:     config.Test{Runner: config.TestRunnerCustom},
			dir:     func(t *testing.T) string { return writeFiles(t, nil) },
			wantErr: ErrNoCommand,
		},
		{
			name: "auto finds a go module", cfg: config.Test{Runner: config.TestRunnerAuto},
			dir:      func(t *testing.T) string { return goModuleDir(t, "example.com/x") },
			wantName: GoName,
		},
		{
			name: "auto finds a node package", cfg: config.Test{Runner: config.TestRunnerAuto},
			dir: func(t *testing.T) string {
				return nodePackageDir(t, "jest", []string{"jest"}, "package-lock.json")
			},
			wantName: NodeName,
		},
		{
			name: "auto finds a python project", cfg: config.Test{Runner: config.TestRunnerAuto},
			dir:      pythonProjectDir,
			wantName: PytestName,
		},
		{
			name: "auto prefers go in a polyglot root", cfg: config.Test{Runner: config.TestRunnerAuto},
			dir: func(t *testing.T) string {
				return writeFiles(t, map[string]string{
					"go.mod":       "module example.com/x\n\ngo 1.26\n",
					"package.json": `{"name":"x"}`,
				})
			},
			wantName: GoName,
		},
		{
			name: "auto finds nothing", cfg: config.Test{Runner: config.TestRunnerAuto},
			dir:     func(t *testing.T) string { return writeFiles(t, map[string]string{"a.txt": "x"}) },
			wantErr: ErrNoProject,
		},
		{
			name:    "unknown runner name",
			cfg:     config.Test{Runner: config.TestRunner("rustc")},
			dir:     func(t *testing.T) string { return writeFiles(t, nil) },
			wantErr: belay.ErrUnsupported,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Select(tc.cfg, tc.dir(t), execOK(""), quiet())
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Select() error = %v, want one wrapping %v", err, tc.wantErr)
				}
				if got != nil {
					t.Errorf("Select() runner = %v, want nil on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Select() error = %v, want nil", err)
			}
			if got.Name() != tc.wantName {
				t.Errorf("Select() = %q, want %q", got.Name(), tc.wantName)
			}
		})
	}
}

func TestRelTo(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		path string
		want string
	}{
		{"absolute inside dir", "/repo", "/repo/src/a.ts", "src/a.ts"},
		{"already relative", "/repo", "src/a.ts", "src/a.ts"},
		{"empty", "/repo", "", ""},
		{"absolute outside dir stays absolute", "/repo", "/other/a.ts", "/other/a.ts"},
		{"parent of dir stays absolute", "/repo/pkg", "/repo/a.ts", "/repo/a.ts"},
		{"dir itself", "/repo", "/repo", "."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := relTo(tc.dir, tc.path); got != tc.want {
				t.Errorf("relTo(%q, %q) = %q, want %q", tc.dir, tc.path, got, tc.want)
			}
		})
	}
}

func TestJoinPackageFile(t *testing.T) {
	tests := []struct {
		name   string
		module string
		pkg    string
		file   string
		want   string
	}{
		{"root package", "example.com/m", "example.com/m", "a_test.go", "a_test.go"},
		{"nested package", "example.com/m", "example.com/m/internal/sub", "a_test.go",
			filepath.Join("internal", "sub", "a_test.go")},
		{"outside the module", "example.com/m", "other.com/x", "a_test.go", "a_test.go"},
		{"unknown module", "", "example.com/m/sub", "a_test.go", "a_test.go"},
		{"no file", "example.com/m", "example.com/m", "", ""},
		{"absolute file is left alone", "example.com/m", "example.com/m/sub", "/abs/a_test.go", "/abs/a_test.go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinPackageFile(tc.module, tc.pkg, tc.file); got != tc.want {
				t.Errorf("joinPackageFile(%q, %q, %q) = %q, want %q",
					tc.module, tc.pkg, tc.file, got, tc.want)
			}
		})
	}
}

func TestRawStringIsAlwaysValidJSON(t *testing.T) {
	tests := []string{
		"",
		`{"Action":"pass"}` + "\n" + `{"Action":"fail"}`,
		"plain text with \"quotes\" and \\backslashes\\",
		"invalid utf-8: \xff\xfe",
		"control chars: \x00\x01\n\t",
	}
	for _, input := range tests {
		var decoded string
		raw := rawString(input)
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Errorf("rawString(%q) produced invalid JSON %q: %v", input, raw, err)
		}
	}
}

func TestExitStatusReport(t *testing.T) {
	pass := exitStatusReport("make test", "coarse", bexec.Result{ExitCode: 0, Duration: time.Second})
	if !pass.OK() || pass.Total != 1 || pass.Passed != 1 {
		t.Errorf("passing coarse report = %+v, want Total 1, Passed 1 and OK", pass)
	}
	fail := exitStatusReport("make test", "coarse", bexec.Result{
		ExitCode: 2, Stdout: "out\n", Stderr: "err\n", TimedOut: true,
	})
	if fail.OK() || fail.Failed != 1 || len(fail.Failures) != 1 {
		t.Fatalf("failing coarse report = %+v, want exactly one failure", fail)
	}
	msg := fail.Failures[0].Message
	for _, want := range []string{"status 2", "timed out", "no per-test detail", "out", "err"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message %q does not mention %q", msg, want)
		}
	}
}

func TestRunErrorMessage(t *testing.T) {
	err := &RunError{
		Runner: "go-test",
		Err:    ErrNoProject,
		Stderr: "line one\nline two\n",
	}
	msg := err.Error()
	for _, want := range []string{"go-test", "could not run tests", "line two"} {
		if !strings.Contains(msg, want) {
			t.Errorf("RunError.Error() = %q, want it to mention %q", msg, want)
		}
	}
	if !errors.Is(err, ErrNoProject) {
		t.Error("RunError does not unwrap to its cause")
	}
}
