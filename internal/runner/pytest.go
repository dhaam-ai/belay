//go:build unix

package runner

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/dhaam-ai/belay/internal/detect"
	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// PytestName is the [belay.TestRunner.Name] of the Python runner.
const PytestName = "pytest"

// pytestTool is the binary a human is told to install when it is absent.
const pytestTool = "pytest"

// pytestEnvAllow are the parent variables pytest needs beyond exec's base set.
//
// PYTEST_ADDOPTS is deliberately absent: it injects arbitrary flags into every
// pytest invocation, including ones that change the output this runner parses
// (-p, --tb, -q), so an operator's shell setting could silently turn every
// belay test node unparseable.
var pytestEnvAllow = []string{
	"CI",
	"PYTHONDONTWRITEBYTECODE",
	"PYTHONPATH",
	"VIRTUAL_ENV",
}

// pytest's documented exit codes. See
// https://docs.pytest.org/en/stable/reference/exit-codes.html
const (
	pytestOK          = 0 // all collected tests passed
	pytestTestsFailed = 1 // tests were collected and run, some failed
	pytestNoTests     = 5 // no tests were collected
)

// Pytest runs `pytest --tb=short -q -rfE` and parses pytest's own summary.
//
// # Why text, not JSON
//
// pytest ships no machine-readable report in its core distribution. The
// popular ones — pytest-json-report, pytest-report-log — are third-party
// plugins, and a test runner that only works if the target repository happens
// to have installed an extra dependency is worse than one that parses text
// reliably. Core pytest does ship --junit-xml, but it writes to a file rather
// than a stream, which would mean creating and cleaning a temporary path on
// every test node for a marginal gain.
//
// So this runner parses two stable, documented surfaces instead:
//
//   - the short test summary that -rfE requests, which prints one
//     "FAILED <nodeid> - <message>" or "ERROR <nodeid>" line per problem;
//   - the final counts line, "2 failed, 2 passed, 1 skipped in 0.02s".
//
// # The honest limitation
//
// Line numbers come from the --tb=short traceback body, matched back to a test
// by its section header. That match is by test name, so two tests with the
// same name in different files can be attributed to the wrong location, and a
// failure whose traceback pytest chose not to print carries Line 0. File is
// always right: it comes from the nodeid, not the traceback. If exact
// locations matter more than working without plugins, configure a custom
// runner that invokes pytest with the reporter of your choice.
//
// # Collection errors
//
// A module that will not import is pytest's analogue of a compile error, but
// it is reported per file, so it surfaces as an ordinary [belay.TestFailure]
// named after the file rather than as a synthesized [BuildFailureName] entry —
// the file name is more actionable than a package-wide "build" marker.
// [IsBuildFailure] is therefore false for it. pytest exits 2 in that case; the
// runner still returns a report rather than an error, because the fix node can
// act on "no module named foo".
type Pytest struct {
	base
}

// NewPytest returns a Python test runner.
//
// A nil Execer uses a real *exec.Runner; a nil logger uses slog.Default.
func NewPytest(x Execer, logger *slog.Logger) *Pytest {
	return &Pytest{base: newBase(x, logger)}
}

// Name implements [belay.TestRunner].
func (p *Pytest) Name() string { return PytestName }

// Detect implements [belay.TestRunner]. It reports whether dir itself holds a
// Python project marker — pyproject.toml, setup.py, requirements.txt or
// tox.ini — since that is where pytest will be invoked.
func (p *Pytest) Detect(dir string) bool {
	_, ok := projectAt(dir, detect.KindPython)
	return ok
}

// Test runs pytest in dir and returns the parsed report.
//
// A missing `pytest` binary is an error satisfying
// errors.Is(err, [belay.ErrToolchainMissing]). Tests that run and fail are not
// an error: the report carries Failed > 0 and nil. Exit code 5 ("no tests
// collected") returns an empty report and nil, which [belay.TestReport.OK]
// treats as not-passing. Any other exit code with no parseable summary is a
// [*RunError].
func (p *Pytest) Test(ctx context.Context, dir string) (belay.TestReport, error) {
	project, ok := projectAt(dir, detect.KindPython)
	if !ok {
		return belay.TestReport{}, &RunError{Runner: PytestName, ExitCode: -1, Err: ErrNoProject}
	}
	if project.Python != nil && !project.Python.HasPytest {
		p.log.LogAttrs(ctx, slog.LevelWarn,
			"belay/runner: python project does not declare pytest; running it anyway",
			slog.String("dir", dir))
	}

	res, err := p.run(ctx, PytestName, pytestTool, bexec.Command{
		Path:     pytestTool,
		Args:     []string{"--tb=short", "-q", "-rfE"},
		Dir:      dir,
		EnvAllow: pytestEnvAllow,
	})
	if err != nil {
		return belay.TestReport{}, err
	}

	report := parsePytest(res.Stdout)
	report.Duration = res.Duration
	report.Raw = rawString(res.Stdout)

	if report.Total == 0 && res.ExitCode != pytestOK &&
		res.ExitCode != pytestTestsFailed && res.ExitCode != pytestNoTests {
		return belay.TestReport{}, &RunError{
			Runner:   PytestName,
			Args:     res.Args,
			ExitCode: res.ExitCode,
			Stderr:   res.Stderr,
			Err:      ErrNoProject,
		}
	}
	p.log.LogAttrs(ctx, slog.LevelInfo, "belay/runner: pytest complete",
		slog.String("dir", dir), slog.Int("total", report.Total),
		slog.Int("passed", report.Passed), slog.Int("failed", report.Failed))
	return report, nil
}

// pytestSummaryLine matches the counts line pytest prints last, such as
// "2 failed, 2 passed, 1 skipped in 0.02s". The leading and trailing "=" runs
// are optional because -q drops them.
var pytestSummaryLine = regexp.MustCompile(`(?m)^=*\s*((?:\d+ [a-z]+(?:, )?)+)\s*in [\d.]+s`)

// pytestCount matches one "<n> <outcome>" pair inside the counts line.
var pytestCount = regexp.MustCompile(`(\d+) ([a-z]+)`)

// pytestShortSummary matches one line of the short test summary that -rfE
// requests: "FAILED tests/test_x.py::test_y - assert 1 == 2", or an ERROR line
// which may carry no message at all.
var pytestShortSummary = regexp.MustCompile(`(?m)^(FAILED|ERROR) (\S+)(?: - (.*))?$`)

// pytestSection matches a --tb=short section header, "____ test_name ____".
var pytestSection = regexp.MustCompile(`(?m)^_{2,} (.+?) _{2,}$`)

// pytestTraceLocation matches the "path/to/file.py:12: in test_name" line
// --tb=short opens each frame with.
var pytestTraceLocation = regexp.MustCompile(`(?m)^(\S+\.py):(\d+): `)

// parsePytest builds a report from pytest's stdout.
//
// Counts come from the summary line, which is authoritative. Failures come
// from the short summary, which names each problem exactly once. The two are
// parsed independently on purpose: a run whose traceback formatting changes
// still gets correct counts, and a run whose summary line is missing still
// gets its failures listed.
func parsePytest(stdout string) belay.TestReport {
	var report belay.TestReport
	counts := parsePytestCounts(stdout)
	for outcome, n := range counts {
		switch outcome {
		case "passed":
			report.Total += n
			report.Passed += n
		case "failed", "error", "errors":
			report.Total += n
			report.Failed += n
		case "skipped", "xfailed", "xpassed", "deselected":
			report.Total += n
		}
	}

	locations := parsePytestLocations(stdout)
	for _, m := range pytestShortSummary.FindAllStringSubmatch(stdout, -1) {
		nodeID, message := m[2], strings.TrimSpace(m[3])
		failure := belay.TestFailure{
			Name:    nodeID,
			File:    pytestFileOf(nodeID),
			Message: message,
		}
		if line, ok := locations[pytestTestName(nodeID)]; ok {
			failure.Line = line
		}
		if failure.Message == "" {
			failure.Message = m[1] + " " + nodeID
		}
		report.Failures = append(report.Failures, failure)
	}

	// The summary line is the source of truth for Failed, but a run
	// interrupted before it printed one still lists its problems. Trust the
	// listing in that case rather than reporting zero failures.
	if report.Failed == 0 && len(report.Failures) > 0 {
		report.Failed = len(report.Failures)
		report.Total += report.Failed
	}
	sortFailures(report.Failures)
	return report
}

// parsePytestCounts reads the final counts line into outcome/number pairs.
//
// The last matching line wins: a run that prints an interim summary — pytest-xdist,
// a plugin banner — must not shadow the real one at the end.
func parsePytestCounts(stdout string) map[string]int {
	matches := pytestSummaryLine.FindAllStringSubmatch(stdout, -1)
	if len(matches) == 0 {
		return nil
	}
	last := matches[len(matches)-1][1]
	counts := make(map[string]int)
	for _, m := range pytestCount.FindAllStringSubmatch(last, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		counts[m[2]] += n
	}
	return counts
}

// parsePytestLocations maps a test name to the first source line in its
// --tb=short traceback.
//
// Sections are delimited by "____ name ____" headers, so the mapping is by
// test name rather than by nodeid — that is all the header carries. Two tests
// of the same name in different files collide; the first one seen wins, and
// the resulting Line may point at the wrong file's copy. See the type doc.
func parsePytestLocations(stdout string) map[string]int {
	headers := pytestSection.FindAllStringSubmatchIndex(stdout, -1)
	if len(headers) == 0 {
		return nil
	}
	locations := make(map[string]int, len(headers))
	for i, h := range headers {
		name := strings.TrimSpace(stdout[h[2]:h[3]])
		end := len(stdout)
		if i+1 < len(headers) {
			end = headers[i+1][0]
		}
		if _, seen := locations[name]; seen {
			continue
		}
		if m := pytestTraceLocation.FindStringSubmatch(stdout[h[1]:end]); m != nil {
			if n, err := strconv.Atoi(m[2]); err == nil {
				locations[name] = n
			}
		}
	}
	return locations
}

// pytestFileOf extracts the file part of a nodeid such as
// "tests/test_x.py::TestClass::test_y".
func pytestFileOf(nodeID string) string {
	file, _, _ := strings.Cut(nodeID, "::")
	return file
}

// pytestTestName extracts the final component of a nodeid, which is what a
// traceback section header shows.
func pytestTestName(nodeID string) string {
	if i := strings.LastIndex(nodeID, "::"); i >= 0 {
		return nodeID[i+2:]
	}
	return nodeID
}
