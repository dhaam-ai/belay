package belay

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by AgentBackend, TestRunner, Linter, Reviewer and
// Isolator implementations. Callers detect them with errors.Is; the two
// that carry structured data (ErrToolchainMissing, ErrBudgetExceeded) also
// have a companion *Error type recoverable with errors.As — see
// ToolchainError and BudgetError.
var (
	// ErrToolchainMissing indicates that a TestRunner, Linter or Reviewer
	// could not run because the external toolchain it wraps (go, npm,
	// pytest, golangci-lint, sonar-scanner, ...) is not installed or not on
	// PATH. It is never returned for a toolchain that ran and reported
	// failures — that is TestReport.Failures or a QualityReport with
	// Gate == GateFail, not an error at all.
	//
	// errors.Is(err, ErrToolchainMissing) is how a caller tells "the tool
	// doesn't exist, a human needs to install it" apart from "the tool ran
	// and your code doesn't pass" — a distinction the fix loop must get
	// right, since the remedy for one is installing software and the
	// remedy for the other is the agent editing code.
	ErrToolchainMissing = errors.New("belay: required toolchain is not installed")

	// ErrBudgetExceeded indicates that a run's cumulative Usage.USD crossed
	// its configured ceiling.
	//
	// An AgentBackend may return an error wrapping ErrBudgetExceeded from
	// Invoke if it tracks its own per-call or per-session budget and
	// refuses to start work that would exceed it. More commonly the budget
	// guard lives above AgentBackend — a graph runner watching cumulative
	// Usage across many Invoke calls — and this sentinel is what that
	// caller wraps into the run's own failure. Either way,
	// errors.Is(err, ErrBudgetExceeded) tells "ran out of money" apart from
	// every other failure mode, since that one is never worth retrying
	// with the same budget.
	ErrBudgetExceeded = errors.New("belay: run exceeded its budget")

	// ErrGateFailed indicates that a QualityReport's Gate was GateFail,
	// for callers that want a failed quality gate to be a Go error (for
	// example at a CLI boundary that must exit non-zero) rather than a
	// value they branch on.
	//
	// Linter and Reviewer implementations never return this from
	// Lint/Review: a failed gate is a successful, informative call —
	// (QualityReport{Gate: GateFail, ...}, nil) — not a Go error. Wrap
	// ErrGateFailed only at a layer that is deliberately converting that
	// result into control flow.
	ErrGateFailed = errors.New("belay: quality gate failed")

	// ErrUnsupported indicates that a call was well-formed but asked an
	// implementation to do something it fundamentally cannot — for example
	// AgentRequest.SessionID set on a backend with no session-resumption
	// support, or an Isolator asked to isolate a source it requires to be
	// a git repository when it is not.
	//
	// errors.Is(err, ErrUnsupported) tells "this implementation cannot do
	// that, ever" apart from a transient failure worth retrying.
	ErrUnsupported = errors.New("belay: operation not supported")
)

// ToolchainError reports that a specific external tool is required but was
// not found. It wraps ErrToolchainMissing, so callers can use either
//
//	errors.Is(err, belay.ErrToolchainMissing)
//
// to detect the condition generically, or
//
//	var te *belay.ToolchainError
//	errors.As(err, &te)
//
// to recover which tool (te.Tool) and, if available, the underlying lookup
// failure (te.Err — typically an *exec.Error from exec.LookPath).
//
// Adapters are not required to use ToolchainError: returning
// fmt.Errorf("%w: golangci-lint", belay.ErrToolchainMissing) already
// satisfies errors.Is. Adapters that want callers to recover the tool name
// programmatically — for example to print "install golangci-lint and
// retry" — should return a *ToolchainError instead.
type ToolchainError struct {
	// Tool is the executable or package name that could not be found, such
	// as "golangci-lint" or "pytest".
	Tool string
	// Err is the underlying lookup failure, if any.
	Err error
}

// Error implements error.
func (e *ToolchainError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("belay: toolchain %q not found: %v", e.Tool, e.Err)
	}
	return fmt.Sprintf("belay: toolchain %q not found", e.Tool)
}

// Unwrap returns ErrToolchainMissing, so errors.Is(err, ErrToolchainMissing)
// succeeds for any *ToolchainError, and — when present — Err, so
// errors.Is/errors.As can also reach the underlying lookup failure.
func (e *ToolchainError) Unwrap() []error {
	if e.Err != nil {
		return []error{ErrToolchainMissing, e.Err}
	}
	return []error{ErrToolchainMissing}
}

// BudgetError reports that a run's cumulative cost crossed its configured
// ceiling. It wraps ErrBudgetExceeded; recover it with errors.As to report
// exactly how far over budget the run went.
type BudgetError struct {
	// SpentUSD is the cumulative cost at the point the run was stopped.
	SpentUSD float64
	// LimitUSD is the configured ceiling that was crossed.
	LimitUSD float64
}

// Error implements error.
func (e *BudgetError) Error() string {
	return fmt.Sprintf("belay: budget exceeded: spent $%.4f of $%.4f limit", e.SpentUSD, e.LimitUSD)
}

// Unwrap returns ErrBudgetExceeded, so errors.Is(err, ErrBudgetExceeded)
// succeeds for any *BudgetError.
func (e *BudgetError) Unwrap() error { return ErrBudgetExceeded }
