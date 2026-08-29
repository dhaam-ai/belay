package belay

import "context"

// Linter adapts a deterministic static-analysis tool (golangci-lint,
// eslint, ruff, a sonar-scanner CLI run, ...) to the graph runner.
//
// Linter and Reviewer both produce a QualityReport and are interchangeable
// wherever the graph asks a quality gate a yes/no question — see the
// QualityReport doc comment for the contract that makes that substitution
// safe. Linter additionally follows the same auto-detection pattern as
// TestRunner: the dispatcher asks each registered Linter whether it
// recognizes a directory before running it.
//
// Because Lint takes only a directory — no ReviewRequest, no caller-
// supplied FailOn — a Linter has no per-call threshold to gate against.
// QualityReport.Gate from a Linter therefore reflects that specific tool's
// own configured or built-in default threshold (for example, golangci-lint
// run with its own .golangci.yml treats any reported issue as a failure
// unless configured otherwise). A caller that needs a specific,
// caller-chosen threshold should either use a Reviewer instead, or ignore
// the Linter's Gate and compute its own from Counts.AtOrAbove.
type Linter interface {
	// Name returns a short, stable, lowercase identifier, such as
	// "golangci-lint" or "eslint". Name becomes QualityReport.Source.
	Name() string

	// Detect reports whether dir looks like a project this linter knows
	// how to check. Like TestRunner.Detect, it must be side-effect free
	// and must not error.
	Detect(dir string) bool

	// Lint runs the tool against dir and returns a QualityReport.
	//
	// If the underlying tool itself is not installed, Lint returns a zero
	// QualityReport and an error wrapping ErrToolchainMissing.
	Lint(ctx context.Context, dir string) (QualityReport, error)
}
