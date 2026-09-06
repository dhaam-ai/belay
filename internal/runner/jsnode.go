//go:build unix

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/dhaam-ai/belay/internal/detect"
	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// NodeName is the [belay.TestRunner.Name] of the Node runner.
const NodeName = "node-test"

// nodeScript is the package.json script every package manager is asked to run.
const nodeScript = "test"

// nodeTool is the binary used when a package has no test script and the
// built-in runner is the only option.
const nodeTool = "node"

// nodeEnvAllow are the parent variables the Node toolchains need beyond exec's
// base set.
//
// NODE_OPTIONS is deliberately absent: it injects arbitrary flags into every
// node process, including ones that write to stdout and would corrupt the JSON
// or TAP document this runner parses.
var nodeEnvAllow = []string{
	"CI",
	"COREPACK_ENABLE_STRICT",
	"NODE_ENV",
	"NODE_PATH",
	"npm_config_registry",
}

// Node runs a package's tests through its own package manager and parses the
// framework's machine-readable output.
//
// # Package manager argument pass-through
//
// This is the part that silently breaks polyglot runners, because the four
// package managers do not agree on how extra flags reach the underlying
// script. belay uses the explicit "run" form everywhere and adds npm's
// separator only for npm:
//
//	npm     npm run test -- --json          # -- is required
//	yarn    yarn run test --json            # no separator
//	pnpm    pnpm run test --json            # no separator
//	bun     bun run test --json             # no separator, and "run" is load-bearing
//
// npm parses "-"-prefixed arguments itself unless they follow "--"
// (https://docs.npmjs.com/cli/v10/commands/npm-run-script: "Use `--` to pass
// `-`-prefixed flags and options which would otherwise be parsed by npm").
// yarn passes everything after the script name straight through
// (https://classic.yarnpkg.com/en/docs/cli/run), as does pnpm ("Any arguments
// after the command's name are added to the executed script",
// https://pnpm.io/cli/run). Bun is the trap: `bun test` runs Bun's own
// built-in test runner and ignores the package's test script entirely, so the
// "run" keyword is mandatory rather than stylistic
// (https://bun.com/docs/cli/run).
//
// A package with no detected package manager is run with npm, which is the
// same fallback internal/detect documents.
//
// # Framework dispatch
//
// jest and vitest both emit the jest JSON document, so one parser serves both:
//
//	jest       --json --testLocationInResults
//	vitest     --run --reporter=json --outputFile=/dev/stdout
//	node:test  --test-reporter=tap
//
// vitest needs --outputFile because since vitest 5 the json reporter writes to
// a file under .vitest/ by default and prints only a notice to stdout; naming
// /dev/stdout puts the document back on the stream in both old and new
// versions. --run is passed because a test script of plain "vitest" would
// otherwise be free to enter watch mode and never return.
//
// mocha and an unrecognized framework have no agreed machine-readable form, so
// they are run with no extra flags and reported from the exit status alone:
// Total 1, and one failure carrying the output tail. That is a real
// degradation and it is documented on the report's failure message, but it is
// better than refusing to test the package.
//
// # No test script
//
// A package.json with no "test" script can still be tested if the framework is
// node:test, which needs no package manager (`node --test --test-reporter=tap`).
// For every other framework the runner has nothing to run and returns
// [ErrNoCommand] rather than guessing at a binary under node_modules.
type Node struct {
	base
}

// NewNode returns a Node test runner.
//
// A nil Execer uses a real *exec.Runner; a nil logger uses slog.Default.
func NewNode(x Execer, logger *slog.Logger) *Node {
	return &Node{base: newBase(x, logger)}
}

// Name implements [belay.TestRunner].
func (n *Node) Name() string { return NodeName }

// Detect implements [belay.TestRunner]. It reports whether dir itself holds a
// package.json, since that is where the package manager will be invoked.
func (n *Node) Detect(dir string) bool {
	_, ok := projectAt(dir, detect.KindNode)
	return ok
}

// Test runs the package's tests and returns the parsed report.
//
// A missing package manager or node binary is an error satisfying
// errors.Is(err, [belay.ErrToolchainMissing]), naming the program that was
// actually absent ("pnpm", not "node"). Tests that run and fail are not an
// error: the report carries Failed > 0 and nil.
func (n *Node) Test(ctx context.Context, dir string) (belay.TestReport, error) {
	project, ok := projectAt(dir, detect.KindNode)
	if !ok || project.Node == nil {
		return belay.TestReport{}, &RunError{Runner: NodeName, ExitCode: -1, Err: ErrNoProject}
	}
	plan, err := nodePlan(*project.Node)
	if err != nil {
		return belay.TestReport{}, &RunError{Runner: NodeName, ExitCode: -1, Err: err}
	}

	res, err := n.run(ctx, NodeName, plan.tool, bexec.Command{
		Path:     plan.tool,
		Args:     plan.args,
		Dir:      dir,
		EnvAllow: nodeEnvAllow,
	})
	if err != nil {
		return belay.TestReport{}, err
	}

	// A nil parser means the framework has no machine-readable output and
	// the exit status is all there is. See nodeFramework.
	report := exitStatusReport(nodeScript+" script", nodeCoarseFailureName, res)
	if plan.parse != nil {
		parsed, perr := plan.parse(res.Stdout, dir)
		if perr != nil {
			return belay.TestReport{}, &RunError{
				Runner:   NodeName,
				Args:     res.Args,
				ExitCode: res.ExitCode,
				Stderr:   res.Stderr,
				Err:      perr,
			}
		}
		report = parsed
	}
	report.Duration = res.Duration
	report.Raw = rawString(res.Stdout)

	n.log.LogAttrs(ctx, slog.LevelInfo, "belay/runner: node test complete",
		slog.String("dir", dir), slog.String("package_manager", project.Node.PackageManager.String()),
		slog.String("framework", project.Node.TestFramework.String()),
		slog.Int("total", report.Total), slog.Int("passed", report.Passed),
		slog.Int("failed", report.Failed))
	return report, nil
}

// nodeCoarseFailureName is the failure name used when the framework has no
// machine-readable output and only the exit status is available.
const nodeCoarseFailureName = "test-script"

// nodeParser turns a runner's stdout into a report. dir is the directory the
// command ran in, used to relativize absolute file paths.
type nodeParser func(stdout, dir string) (belay.TestReport, error)

// nodeCommand is a resolved decision about what to run and how to read it.
type nodeCommand struct {
	// tool is the program to execute — a package manager, or node itself.
	tool string
	// args are the arguments after the program name.
	args []string
	// parse reads the tool's stdout. Nil when the framework has no
	// machine-readable output and only the exit status is available.
	parse nodeParser
}

// nodePlan resolves a detected package into a command.
//
// It is separated from Test so the argument-construction rules — the part that
// differs per package manager and is easiest to get wrong — can be tested
// exhaustively without a subprocess or a filesystem.
func nodePlan(info detect.NodeInfo) (nodeCommand, error) {
	flags, parse := nodeFramework(info.TestFramework)

	if info.TestScript == "" {
		if info.TestFramework != detect.TestFrameworkNodeTest {
			return nodeCommand{}, fmt.Errorf(
				"package has no %q script and framework %s has no default invocation: %w",
				nodeScript, info.TestFramework, ErrNoCommand)
		}
		return nodeCommand{
			tool:  nodeTool,
			args:  append([]string{"--test"}, flags...),
			parse: parse,
		}, nil
	}

	tool, args := nodeScriptArgv(info.PackageManager, flags)
	return nodeCommand{tool: tool, args: args, parse: parse}, nil
}

// nodeFramework maps a detected framework to the flags that make it emit a
// machine-readable report and the parser that reads it.
//
// A nil parser means no such form exists — mocha, or a framework internal/detect
// could not name — and the caller falls back to the exit status.
func nodeFramework(f detect.TestFramework) (flags []string, parse nodeParser) {
	switch f {
	case detect.TestFrameworkJest:
		return []string{"--json", "--testLocationInResults"}, parseJestJSON
	case detect.TestFrameworkVitest:
		return []string{"--run", "--reporter=json", "--outputFile=/dev/stdout"}, parseJestJSON
	case detect.TestFrameworkNodeTest:
		return []string{"--test-reporter=tap"}, parseTAP
	default:
		return nil, nil
	}
}

// nodeScriptArgv builds the argv that runs the package's test script with
// flags, honoring each package manager's pass-through rules. See the [Node]
// documentation for the sources.
func nodeScriptArgv(pm detect.PackageManager, flags []string) (tool string, args []string) {
	switch pm {
	case detect.PackageManagerYarn, detect.PackageManagerPNPM, detect.PackageManagerBun:
		// Everything after the script name reaches the script unchanged.
		// For bun, the "run" keyword is what keeps `bun test` from
		// hijacking the invocation with its own built-in runner.
		args = make([]string, 0, 2+len(flags))
		args = append(args, "run", nodeScript)
		args = append(args, flags...)
		return pm.String(), args
	default:
		// npm, and the documented fallback for an undetermined manager.
		// npm consumes "-"-prefixed arguments itself unless they follow
		// "--", which is only emitted when there is something to pass.
		args = make([]string, 0, 3+len(flags))
		args = append(args, "run", nodeScript)
		if len(flags) > 0 {
			args = append(args, "--")
			args = append(args, flags...)
		}
		return detect.PackageManagerNPM.String(), args
	}
}

// jestDocument is the JSON document jest's --json emits and vitest's json
// reporter reproduces. Only the fields belay needs are declared.
type jestDocument struct {
	NumTotalTests   int              `json:"numTotalTests"`
	NumPassedTests  int              `json:"numPassedTests"`
	NumFailedTests  int              `json:"numFailedTests"`
	NumPendingTests int              `json:"numPendingTests"`
	TestResults     []jestFileResult `json:"testResults"`
}

// jestFileResult is one test file's outcome.
type jestFileResult struct {
	Name             string          `json:"name"`
	Status           string          `json:"status"`
	Message          string          `json:"message"`
	AssertionResults []jestAssertion `json:"assertionResults"`
}

// jestAssertion is one test case's outcome.
type jestAssertion struct {
	FullName        string        `json:"fullName"`
	Title           string        `json:"title"`
	Status          string        `json:"status"`
	FailureMessages []string      `json:"failureMessages"`
	Location        *jestLocation `json:"location"`
}

// jestLocation is the source position --testLocationInResults adds. jest
// documents Line as 1-indexed and Column as 0-indexed; belay only uses Line.
type jestLocation struct {
	Line int `json:"line"`
}

// parseJestJSON reads the jest-compatible JSON document produced by both jest
// and vitest.
//
// Counts come from the document's own num* fields, which are authoritative and
// consistent with belay's arithmetic: numTotalTests already includes pending
// tests, so Total - Passed - Failed is the skipped count without any
// adjustment.
//
// A file that failed to load produces no assertion results at all — a syntax
// error, a missing import. jest and vitest report that as a failed file with a
// message and no assertions, and neither counts it in numFailedTests, so it is
// added explicitly. Without that, a package whose test file does not parse
// would report a clean zero-failure run.
func parseJestJSON(stdout, dir string) (belay.TestReport, error) {
	raw, ok := firstJSONObject(stdout)
	if !ok {
		return belay.TestReport{}, fmt.Errorf(
			"%w: no JSON report found in test output", ErrNoCommand)
	}
	var doc jestDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return belay.TestReport{}, fmt.Errorf("parse JSON test report: %w", err)
	}

	report := belay.TestReport{
		Total:  doc.NumTotalTests,
		Passed: doc.NumPassedTests,
		Failed: doc.NumFailedTests,
	}
	for _, file := range doc.TestResults {
		rel := relTo(dir, file.Name)
		failed := 0
		for _, a := range file.AssertionResults {
			if a.Status != "failed" {
				continue
			}
			failed++
			report.Failures = append(report.Failures, belay.TestFailure{
				Name:    jsAssertionName(a),
				File:    rel,
				Line:    jsLine(a, file.Name),
				Message: strings.TrimSpace(strings.Join(a.FailureMessages, "\n")),
			})
		}
		if failed == 0 && file.Status == "failed" {
			report.Total++
			report.Failed++
			report.Failures = append(report.Failures, belay.TestFailure{
				Name:    rel,
				File:    rel,
				Message: strings.TrimSpace(file.Message),
			})
		}
	}
	sortFailures(report.Failures)
	return report, nil
}

// jsAssertionName prefers the fully qualified name, which includes the
// describe() blocks a bare title omits.
func jsAssertionName(a jestAssertion) string {
	if a.FullName != "" {
		return a.FullName
	}
	return a.Title
}

// jsStackLocation matches a "path/file.ts:12:34" position inside a stack trace
// or a vitest failure message.
var jsStackLocation = regexp.MustCompile(`([^\s():]+\.[cm]?[jt]sx?):(\d+):(\d+)`)

// jsLine resolves a failure's line number.
//
// jest fills Location when --testLocationInResults is passed. vitest does not
// emit Location at all, so the line is recovered from the first stack frame
// that names the test's own file, falling back to the first frame of any file
// — which is still inside the project far more often than not.
func jsLine(a jestAssertion, file string) int {
	if a.Location != nil && a.Location.Line > 0 {
		return a.Location.Line
	}
	var fallback int
	for _, msg := range a.FailureMessages {
		for _, m := range jsStackLocation.FindAllStringSubmatch(msg, -1) {
			n, err := strconv.Atoi(m[2])
			if err != nil {
				continue
			}
			if m[1] == file {
				return n
			}
			if fallback == 0 {
				fallback = n
			}
		}
	}
	return fallback
}

// firstJSONObject extracts the first complete top-level JSON object from s.
//
// Both jest and vitest can print a line of their own around the document — a
// "JSON report written to ..." notice, a corepack banner, a deprecation
// warning — so the document is located rather than assumed to be the whole
// stream. Scanning tracks string and escape state so a brace inside a failure
// message cannot end the object early.
func firstJSONObject(s string) (json.RawMessage, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil, false
	}
	depth, inString, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Braces inside strings are data, not structure.
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return json.RawMessage(s[start : i+1]), true
			}
		}
	}
	return nil, false
}

// tapPlanLine matches an "ok"/"not ok" assertion line at any nesting depth.
var tapPlanLine = regexp.MustCompile(`^(\s*)(not ok|ok)\s+(\d+)\s*-?\s*(.*)$`)

// tapCount matches a trailer count such as "# fail 1".
var tapCount = regexp.MustCompile(`(?m)^\s*# (tests|pass|fail|skipped|todo) (\d+)\s*$`)

// tapDirective matches the "# SKIP"/"# TODO" suffix that marks a result as not
// a real outcome.
var tapDirective = regexp.MustCompile(`#\s*(SKIP|TODO)\b`)

// tapLocation matches the YAML "location: 'file:line:col'" key node:test emits
// for a failure.
var tapLocation = regexp.MustCompile(`^location:\s*'(.+):(\d+):(\d+)'\s*$`)

// parseTAP reads the TAP 13 stream `node --test --test-reporter=tap` produces.
//
// Counts come from the trailer ("# tests", "# pass", "# fail"), which node
// emits once at the end and which already reconciles nested subtests. When the
// trailer is absent — a killed process, a truncated capture — the ok/not ok
// lines are counted instead, so a partial stream still reports what it saw
// rather than reporting nothing.
//
// Each failure's YAML diagnostic block supplies the source location and the
// assertion text; both are optional, and a failure missing them still gets a
// name from its TAP description.
func parseTAP(stdout, dir string) (belay.TestReport, error) {
	lines := strings.Split(stdout, "\n")
	var (
		report   belay.TestReport
		seenOK   int
		seenFail int
		current  *belay.TestFailure
		indent   int
		block    []string
	)

	flush := func() {
		if current == nil {
			return
		}
		applyTAPDiagnostics(current, block, dir)
		report.Failures = append(report.Failures, *current)
		current, block = nil, nil
	}

	for _, line := range lines {
		if current != nil && (strings.TrimSpace(line) == "" || lineIndent(line) > indent) {
			// Part of this failure's YAML diagnostic block. Blank lines
			// belong to it too: they are interior newlines of a literal
			// error message, and dropping them would splice two
			// unrelated assertion lines together.
			block = append(block, line)
			continue
		}
		m := tapPlanLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		flush()
		name := strings.TrimSpace(m[4])
		if tapDirective.MatchString(name) {
			continue // a skip or todo directive, not an outcome
		}
		if m[2] == "ok" {
			seenOK++
			continue
		}
		seenFail++
		indent = lineIndent(line)
		current = &belay.TestFailure{Name: name}
	}
	flush()

	counts := tapCounts(stdout)
	if total, ok := counts["tests"]; ok {
		report.Total = total
		report.Passed = counts["pass"]
		report.Failed = counts["fail"]
	} else {
		report.Total = seenOK + seenFail
		report.Passed = seenOK
		report.Failed = seenFail
	}
	sortFailures(report.Failures)
	return report, nil
}

// tapCounts reads the trailer counts.
func tapCounts(stdout string) map[string]int {
	matches := tapCount.FindAllStringSubmatch(stdout, -1)
	if len(matches) == 0 {
		return nil
	}
	counts := make(map[string]int, len(matches))
	for _, m := range matches {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		counts[m[1]] = n
	}
	return counts
}

// lineIndent counts the leading whitespace of a line, which is how TAP nests
// a diagnostic block under its result and how YAML delimits a literal scalar.
func lineIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

// applyTAPDiagnostics folds a failure's YAML block into the failure.
//
// The block is read line by line rather than with a YAML parser: belay's
// dependency set is cobra, yaml.v3 and go-cmp, and the two keys that matter
// here — a single-quoted location and a literal "error: |-" block — do not
// justify threading a document parser through a streaming reader.
//
// The literal block ends by indentation, per YAML's own rule, not by looking
// for a line that appears to be a key. node:test error messages routinely end
// in a colon ("Expected values to be strictly equal:"), so a key-shaped
// heuristic would truncate the message to nothing on the most common failure
// there is.
func applyTAPDiagnostics(f *belay.TestFailure, block []string, dir string) {
	var message []string
	literalIndent := -1
	for _, raw := range block {
		line := strings.TrimSpace(raw)
		if literalIndent >= 0 {
			if line == "" || lineIndent(raw) > literalIndent {
				message = append(message, line)
				continue
			}
			literalIndent = -1
		}
		if m := tapLocation.FindStringSubmatch(line); m != nil {
			f.File = relTo(dir, m[1])
			if n, err := strconv.Atoi(m[2]); err == nil {
				f.Line = n
			}
			continue
		}
		rest, ok := strings.CutPrefix(line, "error:")
		if !ok {
			continue
		}
		switch rest = strings.TrimSpace(rest); rest {
		case "|-", "|", ">-", ">":
			literalIndent = lineIndent(raw)
		default:
			message = append(message, strings.Trim(rest, `'"`))
		}
	}
	f.Message = strings.TrimSpace(strings.Join(message, "\n"))
}
