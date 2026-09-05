# Adapters & Extension Points

Belay is built around pluggable adapters that interact with external services. This document describes each adapter interface and how to implement custom adapters.

## Overview

Adapters are injected into the dispatcher at runtime. They abstract away differences between services (Claude, Codex, linters, etc.), allowing the graph to remain backend-agnostic.

The main adapter interfaces are:

1. **AgentBackend**: LLM-based code generation (Claude Code, Codex, Gemini)
2. **Reviewer**: Code quality assessment (linters, SonarQube, AI)
3. **TestRunner**: Test execution and reporting
4. **CodeFormatter**: Code formatting and cleanup

## AgentBackend Interface

**Purpose**: LLM-based code generation. Invoke(ctx, AgentRequest) → (AgentResponse, error) is the only method (see `pkg/belay/agent.go`).

**Method Signature**:
```go
type AgentBackend interface {
    Name() string
    Invoke(ctx context.Context, req AgentRequest) (AgentResponse, error)
}
```

**AgentRequest** contains:
- `Prompt`: the task (e.g., "implement the Login handler")
- `SystemPrompt`: optional instruction override (augments or replaces the backend's default)
- `WorkDir`: absolute path to the repository (isolated Workspace.Dir)
- `AllowedTools`: optional tool restrictions (nil = backend's default set)
- `MaxTurns`: optional turn limit (0 = backend's default)
- `Model`: optional model selector (e.g., "claude-opus-4"; empty = backend's default)
- `SessionID`: optional session ID to resume a prior conversation (if supported)
- `MCPConfig`: optional backend-specific MCP configuration (JSON, backend-specific, never parsed by belay)

**AgentResponse** contains:
- `Text`: human-readable output (e.g., a summary of changes; never empty after success)
- `SessionID`: session identifier for resumption (if the backend supports sessions)
- `Usage`: token count and USD cost for this invocation
- `Turns`: number of agent turns taken (0 if the backend does not report it)
- `Raw`: backend's unparsed output (e.g., raw JSON; never read by belay)

**Concurrency**: An AgentBackend may be called concurrently by the same dispatcher (one call per node, but fanout candidates run in parallel). Adapters that cannot safely serve concurrent Invoke calls must serialize internally and document the constraint.

**Error Handling** (see `pkg/belay/errors.go`):
- `ErrBudgetExceeded`: The backend tracked its own budget and refuses to proceed (rare; belay usually tracks budget itself)
- `ErrUnsupported`: The call was valid but unsupported (e.g., SessionID on a backend with no session support)
- Other errors: Network failures, parse errors, timeouts. Wrapped errors should preserve the underlying context (e.g., wrapping ctx.Err() on cancellation)
- An AgentResponse with partial output may be returned alongside an error (e.g., to communicate a backend-defined budget constraint via errors.As)

## Linter Interface

**Purpose**: Auto-detectable static analysis (golangci-lint, eslint, ruff, etc.). Two methods (see `pkg/belay/linter.go`):

```go
type Linter interface {
    Name() string
    Detect(dir string) bool
    Lint(ctx context.Context, dir string) (QualityReport, error)
}
```

- `Detect(dir)`: Side-effect-free probe: is this a project this linter knows how to check? Go linter checks for go.mod, JavaScript checks for package.json, etc.
- `Lint(ctx, dir)`: Run the tool and return a QualityReport.

**Key Contract**:
- A **missing toolchain** (binary not on PATH) returns `(zero QualityReport, error wrapping belay.ErrToolchainMissing)`
- A **successful run** always returns `(QualityReport, nil)`, regardless of whether issues were found (finding issues is not an error)
- **QualityReport.Gate** is computed by the adapter based on its own thresholds; a Linter does not accept a FailOn parameter (unlike Reviewer)
- **All fields of QualityReport are always present** in JSON output (Issues is never nil, even if empty)

**Implementation Notes** (see `internal/linter/` for Go, Node, Python examples):
- Severity translation is critical: map the tool's severity vocabulary (golangci-lint linter names, eslint integers, ruff rule codes) to belay.Severity
- Use explicit mapping tables (not heuristics) with documented rationales (see golangciSeverities in `internal/linter/golangci.go`)
- Parse the tool's output (usually JSON) and convert to Issue objects with RuleID, Severity, File, Line, Message

## Reviewer Interface

**Purpose**: Holistic quality assessment with caller-controlled thresholds. One method (see `pkg/belay/quality.go`):

```go
type Reviewer interface {
    Review(ctx context.Context, req ReviewRequest) (QualityReport, error)
}
```

**ReviewRequest**:
- `WorkDir`: absolute path to the repository
- `ChangedFiles`: list of relative paths to focus on (a whole-project reviewer may ignore it)
- `ProjectKey`: optional project identifier (for SonarQube and similar)
- `FailOn`: minimum Severity that causes Gate = GateFail (e.g., SeverityMajor)

**Key Contract**:
- Gate is GatePass iff Counts.AtOrAbove(FailOn) == 0
- If the review cannot run at all (toolchain missing, network down, parse error), return error and optionally a GateError report
- If the review runs but the verdict is unknown (tool timed out mid-analysis), return GateError and nil error
- Like Linter, **all QualityReport fields are always present** on every path, including errors (Issues is never nil)

**SonarQube Implementation** (see `internal/review/sonar.go`):
- **Server mode**: Connect to an existing SonarQube server (requires SONAR_TOKEN environment variable for authentication)
- **Docker mode**: Spawn SonarQube in Docker (requires docker binary; slower, heavier setup)
- Fetch project quality gate status and issue list
- Map SonarQube severity (blocker, critical, major, minor, info) to belay.Severity
- **Trap**: SonarQube's own quality-gate status field uses the string "ERROR" to mean "gate failed" (belay.GateFail); it is not belay.GateError (which means "verdict unknown")

## TestRunner Interface

**Purpose**: Auto-detectable test execution and reporting. Two methods (see `pkg/belay/runner.go`):

```go
type TestRunner interface {
    Name() string
    Detect(dir string) bool
    Test(ctx context.Context, dir string) (TestReport, error)
}
```

- `Detect(dir)`: Side-effect-free probe: does this look like a project this runner can test? (check for go.mod, package.json with "test" script, pytest.ini, etc.)
- `Test(ctx, dir)`: Run the test suite and return a TestReport

**TestReport** contains:
- `Total`: number of tests discovered and executed
- `Passed`: number of passing tests
- `Failed`: number of failing tests (synthesized as 1 if the toolchain fails to build/compile)
- `Failures`: list of TestFailure objects describing each failure
- `Duration`: wall-clock time the test run took
- `Raw`: unparsed tool output (JSON from `go test -json`, pytest JSON report, etc.)

**OK()** method: Returns true iff Total > 0 and Failed == 0. A suite that found zero tests is deliberately not OK (likely misconfiguration).

**Key Contract**:
- A **missing toolchain** returns `(zero TestReport, error wrapping belay.ErrToolchainMissing)`
- A **failed build** (compilation error, missing dependencies) returns `(TestReport{Failed: 1, Failures: [synthesized build-error entry]}, nil)` (not an error)
- A **successful run with test failures** returns `(TestReport{Failed: N, Failures: [...]}, nil)` (not an error; failures are a verdict)
- Total == 0 with nil error is "the suite ran but found no tests"; the graph treats this as suspicious (OK() returns false)

**Implementation Notes** (see `internal/runner/` for Go, Node, Python examples):
- Parse the tool's output to extract pass/fail counts and individual failure details
- For each failure, return a TestFailure{Name, File, Line, Message}
- If the tool does not report file/line for a failure, leave them empty or zero

## Isolator Interface

**Purpose**: Workspace isolation for crash-resume and fanout. Two methods (see `pkg/belay/isolate.go`):

```go
type Isolator interface {
    Create(ctx context.Context, src, id string) (Workspace, error)
    Destroy(ctx context.Context, ws Workspace) error
}
```

- `Create(ctx, src, id)`: Prepare an isolated workspace derived from src, identified by id (a run ID, candidate index, etc.). Returns a Workspace{ID, Dir, Ephemeral}.
- `Destroy(ctx, ws)`: Clean up the workspace. Must idempotently handle a Workspace decoded from the journal after a process restart (no live handles or closures in Workspace).

**Workspace** structure:
- `ID`: the id passed to Create (echoed back for logging)
- `Dir`: absolute path the agent, test runner, linter should treat as the working directory
- `Ephemeral`: whether Destroy actually removes Dir (true) or leaves it alone (false, for debugging modes with no isolation)

**Current Implementation**: `internal/isolate/copy.go` uses `cp -r` to copy the source to a temporary directory under `.belay/runs/<run-id>/candidates/`. Fanout is disabled; future implementations could use git worktrees or containers.

## Custom Adapter Implementation

### Example: Implementing an AgentBackend

Here is a complete skeleton for a Gemini backend (see `internal/agent/claude/` for the real Claude Code implementation):

```go
package gemini

import (
	"context"
	"fmt"
	"github.com/belay-dev/belay/pkg/belay"
)

// Adapter wraps the Gemini API.
type Adapter struct {
	apiKey string
	// ... other config ...
}

// Name returns a stable identifier for this backend.
func (a *Adapter) Name() string {
	return "gemini-api"
}

// Invoke runs the backend once against req.
func (a *Adapter) Invoke(ctx context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
	// 1. Validate the request
	if req.Prompt == "" {
		return belay.AgentResponse{}, fmt.Errorf("gemini: prompt required")
	}
	if req.WorkDir == "" {
		return belay.AgentResponse{}, fmt.Errorf("gemini: work_dir required")
	}

	// 2. Check for unsupported features
	if req.SessionID != "" {
		return belay.AgentResponse{}, fmt.Errorf("gemini backend: %w", belay.ErrUnsupported)
	}

	// 3. Call the Gemini API (example; real implementation uses HTTP or SDK)
	response, err := a.callGemini(ctx, req)
	if err != nil {
		// Wrap known errors with belay sentinels
		if isRateLimited(err) {
			return belay.AgentResponse{}, fmt.Errorf("gemini rate limited: %w", err)
		}
		if isQuotaExceeded(err) {
			return belay.AgentResponse{}, fmt.Errorf("gemini quota exceeded: %w", belay.ErrBudgetExceeded)
		}
		return belay.AgentResponse{}, fmt.Errorf("gemini API error: %w", err)
	}

	// 4. Parse the response and compute cost
	usage := belay.Usage{
		InputTokens:  response.InputTokens,
		OutputTokens: response.OutputTokens,
		USD:          float64(response.InputTokens)*0.00001 + float64(response.OutputTokens)*0.00003, // Gemini pricing
		Estimated:    false, // Gemini reports cost directly
	}

	// 5. Return the result with non-empty Text
	return belay.AgentResponse{
		Text:  response.Text,
		Usage: usage,
	}, nil
}

// Helper functions (stubs; real implementation would use the Gemini SDK or HTTP calls)
func (a *Adapter) callGemini(ctx context.Context, req belay.AgentRequest) (*geminiResponse, error) {
	// TODO: implement
	return nil, fmt.Errorf("not implemented")
}

type geminiResponse struct {
	Text           string
	InputTokens    int64
	OutputTokens   int64
}

func isRateLimited(err error) bool { return false } // TODO
func isQuotaExceeded(err error) bool { return false } // TODO
```

**Key Patterns**:
- Check required fields (Prompt, WorkDir) early
- Return ErrUnsupported for unsupported features (not GateFail or a generic error)
- Wrap known errors with belay sentinels (ErrBudgetExceeded, etc.)
- Always populate Usage, even if estimated
- Ensure AgentResponse.Text is non-empty on success

### Example: Implementing a Custom Reviewer

Here is a skeleton for a custom reviewer using property-based testing:

```go
package property

import (
	"context"
	"github.com/belay-dev/belay/pkg/belay"
)

// Runner executes property-based tests.
type Runner struct{}

func (r *Runner) Review(ctx context.Context, req belay.ReviewRequest) (belay.QualityReport, error) {
	// 1. Validate the request
	if req.WorkDir == "" {
		return zeroReport("property"), fmt.Errorf("work_dir required")
	}

	// 2. Discover and run property tests (example: use Hypothesis or QuickCheck SDK)
	issues, err := r.runTests(ctx, req.WorkDir, req.ChangedFiles)
	if err != nil {
		// Return GateError + error if the tests could not run
		return belay.QualityReport{
			Source:   "property",
			Gate:     belay.GateError,
			Issues:   []belay.Issue{}, // Never nil
			Counts:   belay.Counts{},
		}, fmt.Errorf("property test execution failed: %w", err)
	}

	// 3. Compute gate based on FailOn and Counts
	counts := countIssues(issues)
	gate := belay.GatePass
	if counts.AtOrAbove(req.FailOn) > 0 {
		gate = belay.GateFail
	}

	// 4. Return the report (never nil Issues, even if empty)
	return belay.QualityReport{
		Source:   "property",
		Gate:     gate,
		Counts:   counts,
		Issues:   issues,
		Summary:  fmt.Sprintf("%d property test failures", counts.Total()),
	}, nil
}

// Helper function
func (r *Runner) runTests(ctx context.Context, workDir string, changedFiles []string) ([]belay.Issue, error) {
	// TODO: invoke property-based test framework, parse results
	return nil, nil
}

func countIssues(issues []belay.Issue) belay.Counts { /* TODO */ return belay.Counts{} }
func zeroReport(source string) belay.QualityReport {
	return belay.QualityReport{
		Source: source,
		Issues: []belay.Issue{}, // Never nil
	}
}
```

**Key Patterns**:
- Always return a QualityReport with non-nil Issues (even if empty)
- Compute Gate based on Counts.AtOrAbove(req.FailOn), never reimplementing the threshold test
- Return GateError (not an error) when the tool could not reach a verdict
- Return (report, error) when the tool could not run at all

## Adapter Registration

Adapters are injected at dispatcher creation time via RunContext (see `internal/graph/dispatcher.go`). The Dispatcher sets:
- `Agent belay.AgentBackend`: required, passed to code, fix, and plan nodes
- `Runner belay.TestRunner`: required, passed to the test node
- `Linter belay.Linter`: optional (one), passed to linters node if review.mode = "lint"
- `Reviewer belay.Reviewer`: optional (one), passed to review node if review.mode = "sonar-*" or AI
- `Isolator belay.Isolator`: required, passed to fanout and every node that touches WorkDir

Example construction (in internal/cli, not shown due to task scope):
```go
d := &graph.Dispatcher{
	Agent:     &agent.Claude{...},
	Runner:    &runner.GoTest{...},
	Linter:    &linter.GolangCI{...},
	Reviewer:  nil, // Using Linter instead
	Isolator:  &isolate.Copy{...},
}
err := d.Run(ctx, &graph.RunContext{...})
```

Each node in the graph receives RunContext with all adapters; a node calls only the adapters it needs. The dispatcher never calls adapters directly; nodes do, making adapter selection delegated to node logic (e.g., the linters node auto-detects which linters apply to a repository).

**Error Translation at the Seam**: Adapters must translate errors that originate outside belay into belay sentinels. Most critical: `internal/exec.ErrToolchainMissing` (from process execution) must be wrapped or translated to `belay.ErrToolchainMissing` before returning from the adapter. Failing to do so makes `errors.Is(err, belay.ErrToolchainMissing)` false at the graph, silently defeating the "install software vs. edit code" distinction (see `internal/linter/linter.go` line 263 for the canonical translation pattern).

## Testing Adapters

### Using belaytest Fakes

Every adapter interface has a corresponding fake in `pkg/belay/belaytest/`:
- `FakeAgent` for AgentBackend
- `FakeRunner` for TestRunner
- `FakeLinter` for Linter
- `FakeReviewer` for Reviewer
- `FakeIsolator` for Isolator

Example test using a fake:
```go
import "github.com/belay-dev/belay/pkg/belay/belaytest"

func TestMyNode(t *testing.T) {
	runner := &belaytest.FakeRunner{
		Responses: []belay.TestReport{
			{Total: 5, Passed: 3, Failed: 2, Failures: []belay.TestFailure{
				{Name: "TestFoo", Message: "assertion failed"},
			}},
		},
	}
	// ... call node with runner, assert results ...
	if got := runner.CallCount(); got != 1 {
		t.Fatalf("TestRunner.Test called %d times, want 1", got)
	}
}
```

Fakes record every call, allow scripting responses and errors, and provide access to what was called (CallCount, LastCall, Calls[]). See `pkg/belay/belaytest/doc.go` for complete documentation.

### Cassette Recording

Adapter tests that need real tool output use cassettes (recorded stdout/stderr): YAML files saved in `testdata/cassettes/` (see `docs/adr/0009-record-replay-cassettes.md`). A cassette holds the tool's output and belay's parsing result side-by-side, allowing tests to exercise the parser without invoking the tool.

To record cassettes:
1. Write a test that calls the adapter (see `internal/linter/golangci_test.go` for examples)
2. Run `go test --tags=cassette_record ./...` to capture live tool output
3. Cassettes are saved to `testdata/cassettes/`
4. CI replays cassettes without making live calls

### Conformance Suite

Conformance tests verify all adapters of the same type are interchangeable. Examples:
- All Linter implementations must return QualityReport with identical JSON key structure (even when Issues is empty)
- All Reviewer implementations must compute Gate identically: GatePass iff Counts.AtOrAbove(FailOn) == 0
- All TestRunner implementations must follow the OK() contract: Total > 0 and Failed == 0

Conformance is tested in `pkg/belay/quality_test.go`, `internal/linter/linter_test.go`, etc., using table-driven tests with fixtures exercising every edge case (empty projects, missing toolchains, parse failures, etc.).

## References

- [ADR-0005](adr/0005-claude-code-only-backend-v0.md): Claude Code as only shipped backend
- [ADR-0006](adr/0006-single-quality-report-contract.md): Single QualityReport contract
- [ADR-0009](adr/0009-record-replay-cassettes.md): Cassette recording strategy
