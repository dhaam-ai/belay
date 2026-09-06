//go:build unix

package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/detect"
	"github.com/dhaam-ai/belay/pkg/belay"
)

func TestNodeName(t *testing.T) {
	if got := NewNode(nil, quiet()).Name(); got != "node-test" {
		t.Errorf("Name() = %q, want %q", got, "node-test")
	}
}

func TestNodeDetect(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"package at the root", map[string]string{"package.json": `{"name":"x"}`}, true},
		{"package in a subdirectory is not claimed", map[string]string{"web/package.json": `{"name":"x"}`}, false},
		{"unparseable package.json still counts", map[string]string{"package.json": "{oops"}, true},
		{"go module", map[string]string{"go.mod": "module example.com/x\n"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewNode(nil, quiet()).Detect(writeFiles(t, tc.files)); got != tc.want {
				t.Errorf("Detect() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNodeScriptArgv is acceptance criterion 6. The four package managers
// disagree about how extra flags reach the script, and getting it wrong turns a
// reporter flag into an argument npm eats or a runner bun replaces. Sources are
// cited on the Node type.
func TestNodeScriptArgv(t *testing.T) {
	flags := []string{"--json", "--testLocationInResults"}
	tests := []struct {
		name  string
		pm    detect.PackageManager
		flags []string
		want  []string
	}{
		{
			name: "npm needs the -- separator",
			pm:   detect.PackageManagerNPM, flags: flags,
			want: []string{"npm", "run", "test", "--", "--json", "--testLocationInResults"},
		},
		{
			name: "yarn passes flags straight through",
			pm:   detect.PackageManagerYarn, flags: flags,
			want: []string{"yarn", "run", "test", "--json", "--testLocationInResults"},
		},
		{
			name: "pnpm passes flags straight through",
			pm:   detect.PackageManagerPNPM, flags: flags,
			want: []string{"pnpm", "run", "test", "--json", "--testLocationInResults"},
		},
		{
			name: "bun keeps the run keyword so bun test cannot hijack it",
			pm:   detect.PackageManagerBun, flags: flags,
			want: []string{"bun", "run", "test", "--json", "--testLocationInResults"},
		},
		{
			name: "an undetermined manager falls back to npm",
			pm:   detect.PackageManagerUnknown, flags: flags,
			want: []string{"npm", "run", "test", "--", "--json", "--testLocationInResults"},
		},
		{
			name: "npm omits the separator when there is nothing to pass",
			pm:   detect.PackageManagerNPM, flags: nil,
			want: []string{"npm", "run", "test"},
		},
		{
			name: "yarn with no flags",
			pm:   detect.PackageManagerYarn, flags: nil,
			want: []string{"yarn", "run", "test"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool, args := nodeScriptArgv(tc.pm, tc.flags)
			got := append([]string{tool}, args...)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("argv = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNodeArgvEndToEnd walks the same table through Test, so the argv assertion
// covers what the runner actually hands internal/exec rather than only the
// helper that builds it.
func TestNodeArgvEndToEnd(t *testing.T) {
	tests := []struct {
		name     string
		script   string
		devDeps  []string
		lockfile string
		want     []string
	}{
		{
			name: "jest under npm", script: "jest", devDeps: []string{"jest"},
			lockfile: "package-lock.json",
			want:     []string{"npm", "run", "test", "--", "--json", "--testLocationInResults"},
		},
		{
			name: "jest under yarn", script: "jest", devDeps: []string{"jest"},
			lockfile: "yarn.lock",
			want:     []string{"yarn", "run", "test", "--json", "--testLocationInResults"},
		},
		{
			name: "vitest under pnpm", script: "vitest", devDeps: []string{"vitest"},
			lockfile: "pnpm-lock.yaml",
			want:     []string{"pnpm", "run", "test", "--run", "--reporter=json", "--outputFile=/dev/stdout"},
		},
		{
			name: "vitest under bun", script: "vitest run", devDeps: []string{"vitest"},
			lockfile: "bun.lock",
			want:     []string{"bun", "run", "test", "--run", "--reporter=json", "--outputFile=/dev/stdout"},
		},
		{
			name: "node:test under npm", script: "node --test", lockfile: "package-lock.json",
			want: []string{"npm", "run", "test", "--", "--test-reporter=tap"},
		},
		{
			name:   "mocha gets no reporter flags it cannot honour",
			script: "mocha", devDeps: []string{"mocha"}, lockfile: "yarn.lock",
			want: []string{"yarn", "run", "test"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := nodePackageDir(t, tc.script, tc.devDeps, tc.lockfile)
			fake := execOK(nodeStdoutFor(t, tc.script))
			if _, err := NewNode(fake, quiet()).Test(context.Background(), dir); err != nil {
				t.Fatalf("Test() error = %v", err)
			}
			got := fake.argv(t)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("argv = %v, want %v", got, tc.want)
			}
		})
	}
}

// nodeStdoutFor returns output the runner selected for script will parse.
func nodeStdoutFor(t *testing.T, script string) string {
	t.Helper()
	switch {
	case strings.Contains(script, "jest"):
		return fixture(t, "jest-pass.json")
	case strings.Contains(script, "vitest"):
		return fixture(t, "vitest-pass.json")
	case strings.Contains(script, "--test"):
		return fixture(t, "nodetest-tap-fail.txt")
	default:
		return ""
	}
}

// TestNodeTestFixtures is the acceptance table over every captured JS fixture.
func TestNodeTestFixtures(t *testing.T) {
	tests := []struct {
		name     string
		script   string
		devDeps  []string
		lockfile string
		fixture  string
		exitCode int
		want     wantReport
		wantOK   bool
	}{
		{
			name: "jest passing", script: "jest", devDeps: []string{"jest"},
			lockfile: "package-lock.json", fixture: "jest-pass.json",
			want:   wantReport{total: 4, passed: 3, failed: 0},
			wantOK: true,
		},
		{
			name: "jest failing with locations", script: "jest", devDeps: []string{"jest"},
			lockfile: "package-lock.json", fixture: "jest-fail.json", exitCode: 1,
			want: wantReport{
				total: 4, passed: 1, failed: 2,
				failures: []belay.TestFailure{
					{Name: "add adds two numbers", File: "__tests__/math.test.js", Line: 4},
					{Name: "top level failure", File: "__tests__/math.test.js", Line: 13},
				},
			},
		},
		{
			name:   "vitest passing despite the trailing report notice",
			script: "vitest run", devDeps: []string{"vitest"},
			lockfile: "pnpm-lock.yaml", fixture: "vitest-pass.json",
			want:   wantReport{total: 4, passed: 3, failed: 0},
			wantOK: true,
		},
		{
			name:   "vitest failing recovers lines from the stack",
			script: "vitest run", devDeps: []string{"vitest"},
			lockfile: "pnpm-lock.yaml", fixture: "vitest-fail.json", exitCode: 1,
			want: wantReport{
				total: 4, passed: 1, failed: 2,
				failures: []belay.TestFailure{
					{Name: "add adds two numbers", File: "test/math.test.js", Line: 7},
					{Name: "top level failure", File: "test/math.test.js", Line: 16},
				},
			},
		},
		{
			name: "node:test TAP, one file", script: "node --test",
			lockfile: "package-lock.json", fixture: "nodetest-tap-fail.txt", exitCode: 1,
			want: wantReport{
				total: 3, passed: 1, failed: 1,
				failures: []belay.TestFailure{{
					Name: "broken", File: "t.test.js", Line: 4,
					Message: "Expected values to be strictly equal:\n\n2 !== 3",
				}},
			},
		},
		{
			name: "node:test TAP, several files", script: "node --test",
			lockfile: "package-lock.json", fixture: "nodetest-tap-multifile-fail.txt", exitCode: 1,
			want: wantReport{
				total: 3, passed: 2, failed: 1,
				failures: []belay.TestFailure{{
					Name: "a-bad", File: "a.test.js", Line: 4,
					Message: "Expected values to be strictly equal:\n\n'x' !== 'y'",
				}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := nodePackageDir(t, tc.script, tc.devDeps, tc.lockfile)
			// The fixtures were captured with the project root rewritten to
			// /repo. Substituting the real temporary directory back in is
			// what makes the File assertions exercise relativization rather
			// than just echoing whatever the tool printed.
			out := strings.ReplaceAll(fixture(t, tc.fixture), nodeFixtureRoot, dir)
			fake := execOK(out)
			if tc.exitCode != 0 {
				fake = execExit(out, tc.exitCode)
			}
			report, err := NewNode(fake, quiet()).Test(context.Background(), dir)
			if err != nil {
				t.Fatalf("Test() error = %v, want nil", err)
			}
			assertReport(t, report, tc.want)
			if got := report.OK(); got != tc.wantOK {
				t.Errorf("report.OK() = %v, want %v", got, tc.wantOK)
			}
			if IsBuildFailure(report) {
				t.Error("IsBuildFailure() = true; the Node runner never synthesizes one")
			}
		})
	}
}

// nodeFixtureRoot is the placeholder the captured JS fixtures use in place of
// the absolute project root the tools printed. See testdata/README.md.
const nodeFixtureRoot = "/repo"

// TestNodeFailingSuiteIsNotAnError is acceptance criterion 5 for Node.
func TestNodeFailingSuiteIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  string
		devDeps []string
		fixture string
	}{
		{"jest", "jest", []string{"jest"}, "jest-fail.json"},
		{"vitest", "vitest run", []string{"vitest"}, "vitest-fail.json"},
		{"node:test", "node --test", nil, "nodetest-tap-fail.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := nodePackageDir(t, tc.script, tc.devDeps, "package-lock.json")
			report, err := NewNode(execExit(fixture(t, tc.fixture), 1), quiet()).
				Test(context.Background(), dir)
			if err != nil {
				t.Fatalf("Test() on a failing suite returned error %v, want nil", err)
			}
			if report.Failed == 0 {
				t.Error("Test() on a failing suite reported no failures")
			}
			if report.OK() {
				t.Error("report.OK() = true for a failing suite")
			}
		})
	}
}

func TestNodeToolchainMissing(t *testing.T) {
	tests := []struct {
		name     string
		script   string
		devDeps  []string
		lockfile string
		wantTool string
	}{
		{"npm absent", "jest", []string{"jest"}, "package-lock.json", "npm"},
		{"pnpm absent", "jest", []string{"jest"}, "pnpm-lock.yaml", "pnpm"},
		{"yarn absent", "jest", []string{"jest"}, "yarn.lock", "yarn"},
		{"bun absent", "jest", []string{"jest"}, "bun.lock", "bun"},
		{"node absent when there is no test script", "", nil, "", "node"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := nodePackageDir(t, tc.script, tc.devDeps, tc.lockfile)
			if tc.script == "" {
				// A package with no test script but a node:test-shaped
				// dependency-free layout falls back to `node --test`.
				dir = writeFiles(t, map[string]string{
					"package.json": `{"name":"demo","scripts":{"other":"node --test"}}`,
				})
			}
			_, err := NewNode(execMissing(tc.wantTool), quiet()).Test(context.Background(), dir)
			if tc.script == "" {
				// Without a recognizable framework there is nothing to run,
				// which is a RunError rather than a missing toolchain.
				var runErr *RunError
				if !errors.As(err, &runErr) || !errors.Is(err, ErrNoCommand) {
					t.Fatalf("Test() error = %v, want *RunError wrapping ErrNoCommand", err)
				}
				return
			}
			assertToolchainMissing(t, err, tc.wantTool)
		})
	}
}

// TestNodeNoTestScriptFallsBackToNodeTest proves the one direct invocation the
// runner is willing to make without a package manager.
func TestNodeNoTestScriptFallsBackToNodeTest(t *testing.T) {
	plan, err := nodePlan(detect.NodeInfo{TestFramework: detect.TestFrameworkNodeTest})
	if err != nil {
		t.Fatalf("nodePlan() error = %v", err)
	}
	got := append([]string{plan.tool}, plan.args...)
	want := []string{"node", "--test", "--test-reporter=tap"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func TestNodePlanErrors(t *testing.T) {
	for _, framework := range []detect.TestFramework{
		detect.TestFrameworkUnknown,
		detect.TestFrameworkJest,
		detect.TestFrameworkVitest,
		detect.TestFrameworkMocha,
	} {
		t.Run(framework.String(), func(t *testing.T) {
			_, err := nodePlan(detect.NodeInfo{TestFramework: framework})
			if !errors.Is(err, ErrNoCommand) {
				t.Errorf("nodePlan() error = %v, want ErrNoCommand", err)
			}
		})
	}
}

// TestNodeCoarseReportForUnsupportedFramework pins the documented degradation:
// mocha has no machine-readable form, so the exit status is all there is.
func TestNodeCoarseReportForUnsupportedFramework(t *testing.T) {
	dir := nodePackageDir(t, "mocha", []string{"mocha"}, "yarn.lock")

	pass, err := NewNode(execOK("1 passing\n"), quiet()).Test(context.Background(), dir)
	if err != nil {
		t.Fatalf("Test() error = %v", err)
	}
	assertReport(t, pass, wantReport{total: 1, passed: 1})
	if !pass.OK() {
		t.Error("a zero-exit coarse run must report OK")
	}

	fail, err := NewNode(execExit("1 failing\n", 1), quiet()).Test(context.Background(), dir)
	if err != nil {
		t.Fatalf("Test() error = %v", err)
	}
	assertReport(t, fail, wantReport{
		total: 1, failed: 1,
		failures: []belay.TestFailure{{Name: "test-script"}},
	})
	if !strings.Contains(fail.Failures[0].Message, "no per-test detail") {
		t.Errorf("coarse failure message does not admit its limitation: %q", fail.Failures[0].Message)
	}
}

func TestNodeTestErrors(t *testing.T) {
	tests := []struct {
		name    string
		dir     func(t *testing.T) string
		fake    *fakeExec
		wantErr error
	}{
		{
			name:    "not a node package",
			dir:     func(t *testing.T) string { return writeFiles(t, map[string]string{"a.txt": "x"}) },
			fake:    execOK(""),
			wantErr: ErrNoProject,
		},
		{
			name: "jest produced no JSON document",
			dir: func(t *testing.T) string {
				return nodePackageDir(t, "jest", []string{"jest"}, "package-lock.json")
			},
			fake:    execExit("Cannot find module 'jest'\n", 1),
			wantErr: ErrNoCommand,
		},
		{
			name: "command could not start",
			dir: func(t *testing.T) string {
				return nodePackageDir(t, "jest", []string{"jest"}, "package-lock.json")
			},
			fake: execStartFailure(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewNode(tc.fake, quiet()).Test(context.Background(), tc.dir(t))
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

// TestParseJestJSONFileLoadFailure covers a file that never produced an
// assertion — a syntax error or a bad import. jest and vitest leave it out of
// numFailedTests, so a runner that only trusted the counters would report a
// clean run for a package that does not even parse.
func TestParseJestJSONFileLoadFailure(t *testing.T) {
	const doc = `{
	  "numTotalTests": 0, "numPassedTests": 0, "numFailedTests": 0,
	  "testResults": [
	    {"name": "/repo/src/broken.test.ts", "status": "failed",
	     "message": "SyntaxError: Unexpected token", "assertionResults": []}
	  ]
	}`
	report, err := parseJestJSON(doc, "/repo")
	if err != nil {
		t.Fatalf("parseJestJSON() error = %v", err)
	}
	assertReport(t, report, wantReport{
		total: 1, failed: 1,
		failures: []belay.TestFailure{{
			Name: "src/broken.test.ts", File: "src/broken.test.ts",
			Message: "SyntaxError: Unexpected token",
		}},
	})
	if report.OK() {
		t.Error("report.OK() = true for a file that failed to load")
	}
}

func TestFirstJSONObject(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"bare document", `{"a":1}`, `{"a":1}`, true},
		{"trailing notice", `{"a":1}JSON report written to /dev/stdout`, `{"a":1}`, true},
		{"leading banner", "corepack: downloading\n{\"a\":1}\n", `{"a":1}`, true},
		{"brace inside a string", `{"a":"} not the end","b":2}`, `{"a":"} not the end","b":2}`, true},
		{"escaped quote before a brace", `{"a":"\"}","b":2}`, `{"a":"\"}","b":2}`, true},
		{"nested objects", `{"a":{"b":{"c":1}}}x`, `{"a":{"b":{"c":1}}}`, true},
		{"no object", "no json here", "", false},
		{"unterminated", `{"a":1`, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := firstJSONObject(tc.input)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseTAPWithoutTrailer proves a truncated TAP stream still reports what
// it saw instead of reporting nothing.
func TestParseTAPWithoutTrailer(t *testing.T) {
	const stream = "TAP version 13\nok 1 - alpha\nnot ok 2 - beta\nok 3 - gamma\n"
	report, err := parseTAP(stream, "/repo")
	if err != nil {
		t.Fatalf("parseTAP() error = %v", err)
	}
	assertReport(t, report, wantReport{
		total: 3, passed: 2, failed: 1,
		failures: []belay.TestFailure{{Name: "beta"}},
	})
}

// TestParseTAPIgnoresDirectives keeps a "# SKIP"-directed result from being
// counted as a failure.
func TestParseTAPIgnoresDirectives(t *testing.T) {
	const stream = "TAP version 13\n" +
		"not ok 1 - deferred # SKIP not implemented\n" +
		"not ok 2 - real failure\n"
	report, err := parseTAP(stream, "/repo")
	if err != nil {
		t.Fatalf("parseTAP() error = %v", err)
	}
	if len(report.Failures) != 1 || report.Failures[0].Name != "real failure" {
		t.Errorf("failures = %+v, want only the undirected one", report.Failures)
	}
}

func TestJSAssertionName(t *testing.T) {
	tests := []struct {
		name string
		in   jestAssertion
		want string
	}{
		{"prefers the fully qualified name", jestAssertion{FullName: "suite case", Title: "case"}, "suite case"},
		{"falls back to the title", jestAssertion{Title: "case"}, "case"},
		{"neither", jestAssertion{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsAssertionName(tc.in); got != tc.want {
				t.Errorf("jsAssertionName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJSLine(t *testing.T) {
	const file = "/repo/src/thing.test.ts"
	tests := []struct {
		name string
		in   jestAssertion
		want int
	}{
		{
			name: "an explicit location wins",
			in:   jestAssertion{Location: &jestLocation{Line: 12}},
			want: 12,
		},
		{
			name: "a zero location is ignored in favour of the stack",
			in: jestAssertion{
				Location:        &jestLocation{Line: 0},
				FailureMessages: []string{"AssertionError\n    at " + file + ":31:5"},
			},
			want: 31,
		},
		{
			name: "the frame naming the test file wins over an earlier one",
			in: jestAssertion{FailureMessages: []string{
				"Error\n    at helper (/repo/node_modules/lib/index.js:99:1)\n    at " + file + ":7:23",
			}},
			want: 7,
		},
		{
			name: "with no frame in the test file the first frame is used",
			in: jestAssertion{FailureMessages: []string{
				"Error\n    at helper (/repo/node_modules/lib/index.js:99:1)",
			}},
			want: 99,
		},
		{name: "no location at all", in: jestAssertion{}, want: 0},
		{
			name: "a stack with no positions",
			in:   jestAssertion{FailureMessages: []string{"Error: something broke"}},
			want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsLine(tc.in, file); got != tc.want {
				t.Errorf("jsLine() = %d, want %d", got, tc.want)
			}
		})
	}
}
