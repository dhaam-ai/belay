//go:build unix

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/pkg/belay"
)

func TestGoName(t *testing.T) {
	if got := NewGo(nil, quiet()).Name(); got != "go-test" {
		t.Errorf("Name() = %q, want %q", got, "go-test")
	}
}

func TestGoDetect(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{
			name:  "module at the root",
			files: map[string]string{"go.mod": "module example.com/x\n\ngo 1.26\n"},
			want:  true,
		},
		{
			name:  "module in a subdirectory is not claimed",
			files: map[string]string{"backend/go.mod": "module example.com/x\n"},
			want:  false,
		},
		{
			name:  "go sources without go.mod",
			files: map[string]string{"main.go": "package main\n"},
			want:  false,
		},
		{
			name:  "node package",
			files: map[string]string{"package.json": `{"name":"x"}`},
			want:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewGo(nil, quiet()).Detect(writeFiles(t, tc.files)); got != tc.want {
				t.Errorf("Detect() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGoDetectMissingDirectory(t *testing.T) {
	if NewGo(nil, quiet()).Detect(writeFiles(t, nil) + "/nope") {
		t.Error("Detect() on a missing directory = true, want false")
	}
}

// TestGoTestFixtures is the acceptance table: every captured go test -json
// stream, with the exact counts and failures it must produce.
func TestGoTestFixtures(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		module    string
		exitCode  int
		want      wantReport
		wantOK    bool
		wantBuild bool
	}{
		{
			name:    "passing suite counts subtests and skips",
			fixture: "go-pass.jsonl",
			module:  "example.com/pass",
			want:    wantReport{total: 5, passed: 4, failed: 0},
			wantOK:  true,
		},
		{
			name:     "failing suite reports parent and subtest",
			fixture:  "go-fail.jsonl",
			module:   "example.com/fail",
			exitCode: 1,
			want: wantReport{
				total: 5, passed: 2, failed: 3,
				failures: []belay.TestFailure{
					{
						Name: "TestAdd", File: "lib_test.go", Line: 7,
						Message: "lib_test.go:7: Add(1,2) = -1, want 3",
					},
					{
						Name: "TestSub", File: "", Line: 0,
						Message: "test failed without producing output of its own",
					},
					{
						Name: "TestSub/neg", File: "lib_test.go", Line: 25,
						Message: "lib_test.go:25: nested boom: 42",
					},
				},
			},
		},
		{
			name:     "build error is one synthesized failure, not a test failure",
			fixture:  "go-build-error.jsonl",
			module:   "example.com/buildfail",
			exitCode: 1,
			want: wantReport{
				total: 1, passed: 0, failed: 1,
				failures: []belay.TestFailure{{
					Name: BuildFailureName, File: "lib.go", Line: 3,
					Message: "# example.com/buildfail [example.com/buildfail.test]\n" +
						"./lib.go:3:37: undefined: undefinedThing",
				}},
			},
			wantBuild: true,
		},
		{
			name:     "nested package failure gets a path relative to the module root",
			fixture:  "go-nested-fail.jsonl",
			module:   "example.com/nested",
			exitCode: 1,
			want: wantReport{
				total: 2, passed: 1, failed: 1,
				failures: []belay.TestFailure{{
					Name: "TestHalve", File: "internal/sub/sub_test.go", Line: 7,
					Message: "sub_test.go:7: Halve(8) = 16, want 4",
				}},
			},
		},
		{
			name:     "interleaved non-JSON output is skipped, not fatal",
			fixture:  "go-noisy-fail.jsonl",
			module:   "example.com/fail",
			exitCode: 1,
			want: wantReport{
				total: 5, passed: 2, failed: 3,
				failures: []belay.TestFailure{
					{Name: "TestAdd", File: "lib_test.go", Line: 7},
					{Name: "TestSub"},
					{Name: "TestSub/neg", File: "lib_test.go", Line: 25},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := fixture(t, tc.fixture)
			fake := execOK(out)
			if tc.exitCode != 0 {
				fake = execExit(out, tc.exitCode)
			}
			report, err := NewGo(fake, quiet()).Test(context.Background(), goModuleDir(t, tc.module))
			if err != nil {
				t.Fatalf("Test() error = %v, want nil", err)
			}
			assertReport(t, report, tc.want)
			if got := report.OK(); got != tc.wantOK {
				t.Errorf("report.OK() = %v, want %v", got, tc.wantOK)
			}
			if got := IsBuildFailure(report); got != tc.wantBuild {
				t.Errorf("IsBuildFailure() = %v, want %v", got, tc.wantBuild)
			}
		})
	}
}

// TestGoBuildErrorIsDistinguishableFromTestFailure is acceptance criterion 3
// stated directly: the two fixtures must not be confusable.
func TestGoBuildErrorIsDistinguishableFromTestFailure(t *testing.T) {
	dir := goModuleDir(t, "example.com/buildfail")
	build, err := NewGo(execExit(fixture(t, "go-build-error.jsonl"), 1), quiet()).
		Test(context.Background(), dir)
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	fail, err := NewGo(execExit(fixture(t, "go-fail.jsonl"), 1), quiet()).
		Test(context.Background(), goModuleDir(t, "example.com/fail"))
	if err != nil {
		t.Fatalf("failure fixture: %v", err)
	}

	if !IsBuildFailure(build) {
		t.Error("IsBuildFailure(build report) = false, want true")
	}
	if IsBuildFailure(fail) {
		t.Error("IsBuildFailure(test-failure report) = true, want false")
	}
	if build.Passed != 0 {
		t.Errorf("build report Passed = %d, want 0: no test ran", build.Passed)
	}
	if !strings.Contains(build.Failures[0].Message, "undefined: undefinedThing") {
		t.Errorf("build failure message lost the compiler diagnostic: %q", build.Failures[0].Message)
	}
	for _, f := range fail.Failures {
		if f.Name == BuildFailureName {
			t.Errorf("test-failure report contains a synthesized build failure: %+v", f)
		}
	}
}

// TestGoTestFailingSuiteIsNotAnError is acceptance criterion 5 for the Go
// runner: a red suite is a report, never a Go error.
func TestGoTestFailingSuiteIsNotAnError(t *testing.T) {
	report, err := NewGo(execExit(fixture(t, "go-fail.jsonl"), 1), quiet()).
		Test(context.Background(), goModuleDir(t, "example.com/fail"))
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

func TestGoTestToolchainMissing(t *testing.T) {
	_, err := NewGo(execMissing("go"), quiet()).
		Test(context.Background(), goModuleDir(t, "example.com/x"))
	assertToolchainMissing(t, err, "go")
}

func TestGoTestErrors(t *testing.T) {
	tests := []struct {
		name    string
		dir     func(t *testing.T) string
		fake    *fakeExec
		wantErr error
	}{
		{
			name:    "not a go module",
			dir:     func(t *testing.T) string { return writeFiles(t, map[string]string{"a.txt": "x"}) },
			fake:    execOK(""),
			wantErr: ErrNoProject,
		},
		{
			name:    "no events and a non-zero exit",
			dir:     func(t *testing.T) string { return goModuleDir(t, "example.com/x") },
			fake:    execExit("go: cannot find main module\n", 1),
			wantErr: ErrNoProject,
		},
		{
			name:    "command could not start",
			dir:     func(t *testing.T) string { return goModuleDir(t, "example.com/x") },
			fake:    execStartFailure(),
			wantErr: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewGo(tc.fake, quiet()).Test(context.Background(), tc.dir(t))
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

func TestGoTestArgv(t *testing.T) {
	fake := execOK(fixture(t, "go-pass.jsonl"))
	dir := goModuleDir(t, "example.com/pass")
	if _, err := NewGo(fake, quiet()).Test(context.Background(), dir); err != nil {
		t.Fatalf("Test() error = %v", err)
	}
	want := []string{"go", "test", "-json", "-count=1", "./..."}
	got := fake.argv(t)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", got, want)
	}
	if c := fake.only(t); c.Dir != dir {
		t.Errorf("Dir = %q, want %q", c.Dir, dir)
	}
}

// TestGoTestReportIsMarshalable pins the reason Raw holds a JSON string rather
// than the event stream verbatim: a TestReport is journaled as JSON, and a
// stream of objects in Raw would make the whole report fail to marshal.
func TestGoTestReportIsMarshalable(t *testing.T) {
	report, err := NewGo(execOK(fixture(t, "go-pass.jsonl")), quiet()).
		Test(context.Background(), goModuleDir(t, "example.com/pass"))
	if err != nil {
		t.Fatalf("Test() error = %v", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal(report) error = %v", err)
	}
	var round belay.TestReport
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("json.Unmarshal error = %v", err)
	}
	var raw string
	if err := json.Unmarshal(round.Raw, &raw); err != nil {
		t.Fatalf("Raw is not a JSON string: %v", err)
	}
	if !strings.Contains(raw, `"Action":"pass"`) {
		t.Error("Raw lost the original event stream")
	}
}

// TestParseGoTestJSONTruncated proves a cut-off stream reports what it saw
// rather than inventing outcomes for tests that never finished.
func TestParseGoTestJSONTruncated(t *testing.T) {
	full := fixture(t, "go-pass.jsonl")
	lines := strings.SplitAfter(full, "\n")
	truncated := strings.Join(lines[:6], "")

	report := parseGoTestJSON(strings.NewReader(truncated), "example.com/pass")
	if report.Failed != 0 {
		t.Errorf("Failed = %d, want 0: a truncated stream must not invent failures", report.Failed)
	}
	if report.Total >= 5 {
		t.Errorf("Total = %d, want fewer than the full run's 5", report.Total)
	}
}

func TestImportPathPackage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"test binary suffix stripped", "example.com/x [example.com/x.test]", "example.com/x"},
		{"external test package", "example.com/x_test [example.com/x.test]", "example.com/x_test"},
		{"plain import path", "example.com/x", "example.com/x"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := importPathPackage(tc.in); got != tc.want {
				t.Errorf("importPathPackage(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestGoBuildFailureWithoutDiagnostics covers a build-fail event whose output
// never arrived: the failure must still exist and say so, because the fix node
// branches on its presence.
func TestGoBuildFailureWithoutDiagnostics(t *testing.T) {
	const stream = `{"Action":"build-fail","ImportPath":"example.com/x [example.com/x.test]"}
{"Action":"fail","Package":"example.com/x","FailedBuild":"example.com/x [example.com/x.test]"}
`
	report := parseGoTestJSON(strings.NewReader(stream), "example.com/x")
	if !IsBuildFailure(report) {
		t.Fatal("IsBuildFailure() = false for a build-fail event")
	}
	assertReport(t, report, wantReport{
		total: 1, failed: 1,
		failures: []belay.TestFailure{{
			Name: BuildFailureName, Message: "build failed (no compiler output captured)",
		}},
	})
}

// TestGoPanicLocation covers the fallback location matcher: a panicking test
// prints its stack rather than a "file.go:12:" prefixed assertion.
func TestGoPanicLocation(t *testing.T) {
	const stream = `{"Action":"run","Package":"example.com/x","Test":"TestBoom"}
{"Action":"output","Package":"example.com/x","Test":"TestBoom","Output":"panic: runtime error: index out of range [3]\n"}
{"Action":"output","Package":"example.com/x","Test":"TestBoom","Output":"\tlib_test.go:14 +0x1c\n"}
{"Action":"fail","Package":"example.com/x","Test":"TestBoom"}
`
	report := parseGoTestJSON(strings.NewReader(stream), "example.com/x")
	assertReport(t, report, wantReport{
		total: 1, failed: 1,
		failures: []belay.TestFailure{{Name: "TestBoom", File: "lib_test.go", Line: 14}},
	})
}
