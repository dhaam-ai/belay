//go:build unix

package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/pkg/belay"
)

func TestPytestName(t *testing.T) {
	if got := NewPytest(nil, quiet()).Name(); got != "pytest" {
		t.Errorf("Name() = %q, want %q", got, "pytest")
	}
}

func TestPytestDetect(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"pyproject", map[string]string{"pyproject.toml": "[project]\nname='x'\n"}, true},
		{"setup.py", map[string]string{"setup.py": "from setuptools import setup\n"}, true},
		{"requirements", map[string]string{"requirements.txt": "pytest\n"}, true},
		{"tox.ini", map[string]string{"tox.ini": "[tox]\n"}, true},
		{"nested project is not claimed", map[string]string{"svc/pyproject.toml": "[project]\n"}, false},
		{"bare python sources", map[string]string{"main.py": "print(1)\n"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewPytest(nil, quiet()).Detect(writeFiles(t, tc.files)); got != tc.want {
				t.Errorf("Detect() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPytestFixtures is the acceptance table over the captured pytest output.
func TestPytestFixtures(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		exitCode int
		want     wantReport
		wantOK   bool
	}{
		{
			name:    "passing run counts the skip toward Total only",
			fixture: "pytest-pass.txt",
			want:    wantReport{total: 5, passed: 4, failed: 0},
			wantOK:  true,
		},
		{
			name:    "failing run lists each failure with file and line",
			fixture: "pytest-fail.txt", exitCode: 1,
			want: wantReport{
				total: 5, passed: 2, failed: 2,
				failures: []belay.TestFailure{
					{
						Name: "tests/test_math.py::test_add", File: "tests/test_math.py", Line: 9,
						Message: "assert -1 == 3",
					},
					{
						Name: "tests/test_math.py::test_raises", File: "tests/test_math.py", Line: 27,
						Message: "ValueError: boom",
					},
				},
			},
		},
		{
			name:    "collection error is a failure naming the file, not a build failure",
			fixture: "pytest-collect-error.txt", exitCode: 2,
			want: wantReport{
				total: 1, passed: 0, failed: 1,
				failures: []belay.TestFailure{{
					Name: "bad/test_broken.py", File: "bad/test_broken.py",
					Message: "ERROR bad/test_broken.py",
				}},
			},
		},
		{
			name:    "no tests collected is an empty report, not an error",
			fixture: "pytest-no-tests.txt", exitCode: 5,
			want: wantReport{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := fixture(t, tc.fixture)
			fake := execOK(out)
			if tc.exitCode != 0 {
				fake = execExit(out, tc.exitCode)
			}
			report, err := NewPytest(fake, quiet()).Test(context.Background(), pythonProjectDir(t))
			if err != nil {
				t.Fatalf("Test() error = %v, want nil", err)
			}
			assertReport(t, report, tc.want)
			if got := report.OK(); got != tc.wantOK {
				t.Errorf("report.OK() = %v, want %v", got, tc.wantOK)
			}
			if IsBuildFailure(report) {
				t.Error("IsBuildFailure() = true; the pytest runner never synthesizes one")
			}
		})
	}
}

// TestPytestFailingSuiteIsNotAnError is acceptance criterion 5 for pytest.
func TestPytestFailingSuiteIsNotAnError(t *testing.T) {
	report, err := NewPytest(execExit(fixture(t, "pytest-fail.txt"), 1), quiet()).
		Test(context.Background(), pythonProjectDir(t))
	if err != nil {
		t.Fatalf("Test() on a failing suite returned error %v, want nil", err)
	}
	if report.Failed == 0 {
		t.Fatal("Test() on a failing suite reported no failures")
	}
	if report.OK() {
		t.Error("report.OK() = true for a failing suite")
	}
}

func TestPytestToolchainMissing(t *testing.T) {
	_, err := NewPytest(execMissing("pytest"), quiet()).
		Test(context.Background(), pythonProjectDir(t))
	assertToolchainMissing(t, err, "pytest")
}

func TestPytestArgv(t *testing.T) {
	fake := execOK(fixture(t, "pytest-pass.txt"))
	dir := pythonProjectDir(t)
	if _, err := NewPytest(fake, quiet()).Test(context.Background(), dir); err != nil {
		t.Fatalf("Test() error = %v", err)
	}
	want := "pytest --tb=short -q -rfE"
	if got := strings.Join(fake.argv(t), " "); got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if c := fake.only(t); c.Dir != dir {
		t.Errorf("Dir = %q, want %q", c.Dir, dir)
	}
	for _, banned := range []string{"PYTEST_ADDOPTS"} {
		for _, allowed := range fake.only(t).EnvAllow {
			if allowed == banned {
				t.Errorf("EnvAllow contains %q, which can rewrite the flags being parsed", banned)
			}
		}
	}
}

func TestPytestTestErrors(t *testing.T) {
	tests := []struct {
		name    string
		dir     func(t *testing.T) string
		fake    *fakeExec
		wantErr error
	}{
		{
			name:    "not a python project",
			dir:     func(t *testing.T) string { return writeFiles(t, map[string]string{"a.txt": "x"}) },
			fake:    execOK(""),
			wantErr: ErrNoProject,
		},
		{
			name:    "internal error with nothing parseable",
			dir:     pythonProjectDir,
			fake:    execExit("INTERNALERROR> RuntimeError\n", 3),
			wantErr: ErrNoProject,
		},
		{
			name: "command could not start",
			dir:  pythonProjectDir,
			fake: execStartFailure(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPytest(tc.fake, quiet()).Test(context.Background(), tc.dir(t))
			var runErr *RunError
			if !errors.As(err, &runErr) {
				t.Fatalf("Test() error = %v (%T), want *RunError", err, err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(err, %v) = false; err = %v", tc.wantErr, err)
			}
			if errors.Is(err, belay.ErrToolchainMissing) {
				t.Error("a run failure must not masquerade as a missing toolchain")
			}
		})
	}
}

// TestParsePytestCounts pins the arithmetic across the outcome vocabulary
// pytest can print, including the xfail forms the fixtures do not exercise.
func TestParsePytestCounts(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   wantReport
	}{
		{
			name:   "every outcome at once",
			stdout: "2 failed, 3 passed, 1 skipped, 1 xfailed, 1 xpassed, 2 errors in 1.20s\n",
			want:   wantReport{total: 10, passed: 3, failed: 4},
		},
		{
			name:   "decorated summary line still parses",
			stdout: "=========== 1 failed, 1 passed in 0.10s ============\n",
			want:   wantReport{total: 2, passed: 1, failed: 1},
		},
		{
			name:   "an interim summary does not shadow the final one",
			stdout: "1 passed in 0.01s\nrerunning\n3 failed, 1 passed in 0.05s\n",
			want:   wantReport{total: 4, passed: 1, failed: 3},
		},
		{
			name:   "no summary at all",
			stdout: "something went very wrong\n",
			want:   wantReport{},
		},
		{
			name: "failures listed without a summary are still counted",
			stdout: "short test summary info\n" +
				"FAILED tests/test_a.py::test_one - boom\n",
			want: wantReport{
				total: 1, failed: 1,
				failures: []belay.TestFailure{
					{Name: "tests/test_a.py::test_one", File: "tests/test_a.py", Message: "boom"},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertReport(t, parsePytest(tc.stdout), tc.want)
		})
	}
}

// TestParsePytestLocationMissing documents the honest limitation: a failure
// pytest reported without a traceback carries Line 0 rather than a guess.
func TestParsePytestLocationMissing(t *testing.T) {
	report := parsePytest("FAILED tests/test_a.py::test_one - boom\n1 failed in 0.01s\n")
	if len(report.Failures) != 1 {
		t.Fatalf("got %d failures, want 1", len(report.Failures))
	}
	if got := report.Failures[0].Line; got != 0 {
		t.Errorf("Line = %d, want 0 when no traceback was printed", got)
	}
	if got := report.Failures[0].File; got != "tests/test_a.py" {
		t.Errorf("File = %q, want it recovered from the nodeid", got)
	}
}
