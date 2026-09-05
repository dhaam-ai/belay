//go:build unix

package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/belay-dev/belay/internal/detect"
	bexec "github.com/belay-dev/belay/internal/exec"
	"github.com/belay-dev/belay/pkg/belay"
)

// GoName is the [belay.TestRunner.Name] of the Go runner.
const GoName = "go-test"

// goTool is the binary a human is told to install when it is absent.
const goTool = "go"

// goEnvAllow are the parent variables the Go toolchain needs beyond exec's
// base set (HOME, LANG, PATH, TERM, TMPDIR).
//
// These locate caches, the module proxy and private-module policy — the
// difference between a build that reuses a warm cache and one that re-downloads
// the world, or cannot download at all. GOFLAGS is deliberately absent: it can
// inject flags into `go test` and change the very output this runner parses.
var goEnvAllow = []string{
	"CGO_ENABLED",
	"GOCACHE",
	"GOMODCACHE",
	"GONOSUMDB",
	"GOPATH",
	"GOPRIVATE",
	"GOPROXY",
	"GOROOT",
	"GOSUMDB",
	"GOTOOLCHAIN",
}

// Go runs `go test -json -count=1 ./...` and parses the test2json event stream.
//
// # Why -count=1
//
// Go's test cache is sound and replays the whole -json stream on a hit, so
// this is not a correctness workaround. It is a deliberate trade of time for a
// fresh observation: a test node in an autonomous loop is a verification gate,
// and having the suite actually execute after an agent edited the tree is
// worth more than the seconds the cache would save.
//
// # Build failures
//
// A package that does not compile is not a failing test, and the fix node
// needs to know which it is looking at. `go test -json` reports this as
// build-output and build-fail events (Go 1.24 and later) plus a package-level
// fail carrying FailedBuild. All of that collapses into one synthesized
// [belay.TestFailure] named [BuildFailureName] whose Message is the compiler
// output, recognizable with [IsBuildFailure]. When one package of a module
// fails to build while others test cleanly, the report carries both: the real
// failures and the synthesized build one.
//
// # What gets counted
//
// Every test that reaches a terminal event is counted, parents and subtests
// alike, exactly as `go test -json` reports them. A failing subtest therefore
// contributes two failures — "TestThing/case" and its parent "TestThing" —
// because both genuinely failed and dropping the parent would lose a failure
// in the case where a parent asserts something of its own after its subtests
// pass. Skipped tests count toward Total and neither Passed nor Failed.
//
// # File paths
//
// `go test` reports only base file names ("thing_test.go:42"). The Go runner
// rebuilds a path relative to the tested directory by trimming the module path
// off the package's import path, using the module name from internal/detect.
// A package outside the main module — a nested module, a replace directive
// pointing elsewhere — cannot be resolved that way, and its failures carry the
// base name alone.
type Go struct {
	base
}

// NewGo returns a Go test runner.
//
// A nil Execer uses a real *exec.Runner; a nil logger uses slog.Default.
func NewGo(x Execer, logger *slog.Logger) *Go {
	return &Go{base: newBase(x, logger)}
}

// Name implements [belay.TestRunner].
func (g *Go) Name() string { return GoName }

// Detect implements [belay.TestRunner]. It reports whether dir itself is a Go
// module root — a go.mod in dir, not in a subdirectory, because that is where
// `go test ./...` has to run.
func (g *Go) Detect(dir string) bool {
	_, ok := projectAt(dir, detect.KindGo)
	return ok
}

// Test runs the module's tests and returns the parsed report.
//
// A missing `go` binary is an error satisfying
// errors.Is(err, [belay.ErrToolchainMissing]). A suite that runs and fails is
// not an error: the report carries Failed > 0 and nil. A `go test` that exits
// non-zero having produced no parseable events at all — no module, an
// unreadable directory — is a [*RunError], because nothing was learned about
// the code.
func (g *Go) Test(ctx context.Context, dir string) (belay.TestReport, error) {
	project, ok := projectAt(dir, detect.KindGo)
	if !ok {
		return belay.TestReport{}, &RunError{Runner: GoName, ExitCode: -1, Err: ErrNoProject}
	}
	module := ""
	if project.Go != nil {
		module = project.Go.Module
	}

	res, err := g.run(ctx, GoName, goTool, bexec.Command{
		Path:     goTool,
		Args:     []string{"test", "-json", "-count=1", "./..."},
		Dir:      dir,
		EnvAllow: goEnvAllow,
	})
	if err != nil {
		return belay.TestReport{}, err
	}

	report := parseGoTestJSON(strings.NewReader(res.Stdout), module)
	report.Duration = res.Duration
	report.Raw = rawString(res.Stdout)

	if report.Total == 0 && res.ExitCode != 0 {
		return belay.TestReport{}, &RunError{
			Runner:   GoName,
			Args:     res.Args,
			ExitCode: res.ExitCode,
			Stderr:   res.Stderr,
			Err:      fmt.Errorf("go test produced no test events: %w", ErrNoProject),
		}
	}
	g.log.LogAttrs(ctx, slog.LevelInfo, "belay/runner: go test complete",
		slog.String("dir", dir), slog.Int("total", report.Total),
		slog.Int("passed", report.Passed), slog.Int("failed", report.Failed),
		slog.Bool("build_failure", IsBuildFailure(report)))
	return report, nil
}

// goEvent is one test2json event.
//
// The field set is the documented one from cmd/test2json, plus ImportPath and
// FailedBuild, which Go 1.24 added for build failures. Unknown fields are
// ignored, so a newer toolchain that adds more cannot break the parse.
type goEvent struct {
	Action      string `json:"Action"`
	Package     string `json:"Package"`
	Test        string `json:"Test"`
	Output      string `json:"Output"`
	ImportPath  string `json:"ImportPath"`
	FailedBuild string `json:"FailedBuild"`
}

// goTestState is one test's accumulated events.
type goTestState struct {
	pkg     string
	name    string
	outcome string
	output  strings.Builder
}

// goParser folds a test2json event stream into a report.
//
// It is a struct rather than a pile of parameters because the stream carries
// two independent kinds of state — per-test accumulation and whole-build
// failure — and threading both through a free function made the signature
// longer than the logic.
type goParser struct {
	// module is the main module path, used to turn import paths back into
	// directories. Empty when go.mod had no module directive.
	module string
	// tests indexes accumulating state by package and test name.
	tests map[string]*goTestState
	// order preserves first-seen order so the pre-sort report is stable.
	order []*goTestState
	// buildOut accumulates compiler diagnostics across build-output events.
	buildOut strings.Builder
	// buildPkg is the import path of the first package that failed to build.
	buildPkg string
	// buildFailed records that any package failed to build.
	buildFailed bool
}

// parseGoTestJSON reads a test2json event stream and builds a report.
//
// The stream is a sequence of JSON objects, one per line, not a single
// document, so it is parsed line by line. Lines that are not JSON objects are
// skipped rather than treated as errors: `go test` interleaves plain text on
// stdout in some situations ("go: downloading ..."), and a toolchain note must
// not be able to void a whole test run. Lines are read with a bufio.Reader
// rather than a Scanner because a single failure message can exceed a
// Scanner's token limit, and a truncated stream would silently lose tests.
//
// module is the main module path; it may be empty, in which case failures
// carry the base file name go test reported.
func parseGoTestJSON(r io.Reader, module string) belay.TestReport {
	p := &goParser{module: module, tests: make(map[string]*goTestState)}
	buf := bufio.NewReader(r)
	for {
		line, err := buf.ReadString('\n')
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "{") {
			var ev goEvent
			if json.Unmarshal([]byte(trimmed), &ev) == nil {
				p.apply(&ev)
			}
		}
		if err != nil {
			break
		}
	}
	return p.report()
}

// apply folds one event into the accumulating state.
func (p *goParser) apply(ev *goEvent) {
	switch ev.Action {
	case "build-output":
		if p.buildPkg == "" {
			p.buildPkg = importPathPackage(ev.ImportPath)
		}
		p.buildOut.WriteString(ev.Output)
		return
	case "build-fail":
		p.buildFailed = true
		return
	}

	if ev.Test == "" {
		// A package-level event. The only one carrying information the
		// per-test events do not is a fail naming the build that broke,
		// which a toolchain older than Go 1.24 reports without a
		// build-fail action.
		if ev.Action == "fail" && ev.FailedBuild != "" {
			p.buildFailed = true
			if p.buildPkg == "" {
				p.buildPkg = importPathPackage(ev.FailedBuild)
			}
		}
		return
	}

	key := ev.Package + "\t" + ev.Test
	t, ok := p.tests[key]
	if !ok {
		t = &goTestState{pkg: ev.Package, name: ev.Test}
		p.tests[key] = t
		p.order = append(p.order, t)
	}
	switch ev.Action {
	case "output":
		t.output.WriteString(ev.Output)
	case "pass", "fail", "skip":
		t.outcome = ev.Action
	}
}

// report turns the accumulated state into a TestReport.
//
// A test with no terminal event — the stream was cut off, the process was
// killed — is counted as nothing at all rather than guessed at, so a truncated
// run reports fewer tests instead of inventing outcomes.
func (p *goParser) report() belay.TestReport {
	var report belay.TestReport
	for _, t := range p.order {
		switch t.outcome {
		case "pass":
			report.Total++
			report.Passed++
		case "skip":
			report.Total++
		case "fail":
			report.Total++
			report.Failed++
			report.Failures = append(report.Failures, goFailure(t, p.module))
		}
	}
	if p.buildFailed {
		report.Total++
		report.Failed++
		report.Failures = append(report.Failures,
			goBuildFailure(p.buildOut.String(), p.buildPkg, p.module))
	}
	sortFailures(report.Failures)
	return report
}

// importPathPackage strips the test-binary suffix Go appends to the import
// path of a failing build, turning
// "example.com/x [example.com/x.test]" into "example.com/x".
func importPathPackage(importPath string) string {
	if i := strings.Index(importPath, " ["); i >= 0 {
		return importPath[:i]
	}
	return importPath
}

// goFileLine matches the "file.go:12: message" form go test emits for
// t.Error/t.Fatal, anchored to the start of a line so a file name mentioned
// inside a message body does not win over the real location.
var goFileLine = regexp.MustCompile(`(?m)^[\t ]*([\w.+\-/]+\.go):(\d+): `)

// goFileLineAnywhere is the fallback for panics and stack traces, where the
// location appears mid-line preceded by a tab.
var goFileLineAnywhere = regexp.MustCompile(`([\w.+\-/]+\.go):(\d+)`)

// goBuildFileLine matches a compiler diagnostic, which carries a column too:
// "./thing.go:3:37: undefined: x".
var goBuildFileLine = regexp.MustCompile(`(?m)^(\.?/?[\w.+\-/]+\.go):(\d+):(?:\d+:)? `)

// goFailure converts one failed test into a TestFailure.
func goFailure(t *goTestState, module string) belay.TestFailure {
	message := cleanGoOutput(t.output.String())
	if message == "" {
		// A parent test whose only failure was a subtest's produces no
		// output of its own. Say so rather than handing the fix node an
		// empty string it cannot act on.
		message = "test failed without producing output of its own"
	}
	file, line := locateGo(message)
	return belay.TestFailure{
		Name:    t.name,
		File:    joinPackageFile(module, t.pkg, file),
		Line:    line,
		Message: message,
	}
}

// goBuildFailure synthesizes the single failure that stands for a broken
// build.
func goBuildFailure(output, pkg, module string) belay.TestFailure {
	message := strings.TrimSpace(output)
	if message == "" {
		message = "build failed (no compiler output captured)"
	}
	var (
		file string
		line int
	)
	if m := goBuildFileLine.FindStringSubmatch(message); m != nil {
		file = strings.TrimPrefix(m[1], "./")
		line, _ = strconv.Atoi(m[2])
	}
	return belay.TestFailure{
		Name:    BuildFailureName,
		File:    joinPackageFile(module, pkg, file),
		Line:    line,
		Message: message,
	}
}

// locateGo finds the first source location in a failure message.
func locateGo(message string) (file string, line int) {
	m := goFileLine.FindStringSubmatch(message)
	if m == nil {
		m = goFileLineAnywhere.FindStringSubmatch(message)
	}
	if m == nil {
		return "", 0
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return m[1], 0
	}
	return m[1], n
}

// joinPackageFile rebuilds a path relative to the tested directory from the
// package's import path and the base file name go test reported.
//
// It returns file unchanged when the package is not inside the main module, or
// when the caller already has an absolute or multi-segment path — the fallback
// is a base name, which is still the most useful thing available.
func joinPackageFile(module, pkg, file string) string {
	if file == "" || module == "" || pkg == "" || filepath.IsAbs(file) {
		return file
	}
	if pkg == module {
		return file
	}
	rest, ok := strings.CutPrefix(pkg, module+"/")
	if !ok {
		return file
	}
	return filepath.Join(filepath.FromSlash(rest), file)
}

// cleanGoOutput strips the framing `go test` wraps around a test's own output.
//
// "=== RUN", "=== PAUSE", "=== CONT", "=== NAME" and the trailing
// "--- FAIL:" banner say nothing the report does not already carry in Name and
// Failed, and they crowd out the assertion message in a prompt.
func cleanGoOutput(output string) string {
	lines := strings.Split(output, "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		trimmed := strings.TrimLeft(l, " \t")
		switch {
		case strings.HasPrefix(trimmed, "=== "):
			continue
		case strings.HasPrefix(trimmed, "--- FAIL: "),
			strings.HasPrefix(trimmed, "--- PASS: "),
			strings.HasPrefix(trimmed, "--- SKIP: "):
			continue
		}
		kept = append(kept, l)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
