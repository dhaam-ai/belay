package belaytest

import (
	"context"
	"sync"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// FakeLinter is a scriptable belay.Linter for tests.
//
// The zero value is usable: Detect returns false (see DetectValue) and
// Lint records the call and returns a zero belay.QualityReport and a nil
// error. See the package doc for the Responses/Errs/Func scripting model.
type FakeLinter struct {
	// NameValue is returned by Name, and becomes QualityReport.Source for
	// any scripted response that leaves Source empty. Defaults to
	// "fake-linter" if empty.
	NameValue string

	// DetectValue is returned by Detect, unless DetectFunc is set. It
	// defaults to false — the zero value — so a fake linter claims no
	// directory until a test opts it in explicitly.
	DetectValue bool

	// DetectFunc, if non-nil, is called instead of returning DetectValue.
	DetectFunc func(dir string) bool

	// Responses are returned in order, one per Lint call; the last entry
	// repeats once exhausted.
	Responses []belay.QualityReport

	// Errs are returned in order, one per Lint call, independently of
	// Responses; the last entry repeats once exhausted.
	Errs []error

	// Func, if non-nil, is called instead of consulting Responses/Errs.
	// The call is still recorded either way.
	Func func(ctx context.Context, dir string) (belay.QualityReport, error)

	mu          sync.Mutex
	lintCalls   []string
	detectCalls []string
}

// Name implements belay.Linter.
func (f *FakeLinter) Name() string {
	if f.NameValue != "" {
		return f.NameValue
	}
	return "fake-linter"
}

// Detect implements belay.Linter. It records dir, then returns
// DetectFunc(dir) if DetectFunc is set, or otherwise DetectValue.
func (f *FakeLinter) Detect(dir string) bool {
	f.mu.Lock()
	f.detectCalls = append(f.detectCalls, dir)
	f.mu.Unlock()

	if f.DetectFunc != nil {
		return f.DetectFunc(dir)
	}
	return f.DetectValue
}

// Lint implements belay.Linter. It records dir, then returns Func's result
// if Func is set, or otherwise the Responses/Errs entry for this call
// number.
func (f *FakeLinter) Lint(ctx context.Context, dir string) (belay.QualityReport, error) {
	n := f.recordLint(dir)

	if f.Func != nil {
		return f.Func(ctx, dir)
	}
	return pick(f.Responses, n), pick(f.Errs, n)
}

func (f *FakeLinter) recordLint(dir string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lintCalls = append(f.lintCalls, dir)
	return len(f.lintCalls) - 1
}

// LintCalls returns every directory Lint has been called with, in call
// order. The returned slice is a copy.
func (f *FakeLinter) LintCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.lintCalls))
	copy(out, f.lintCalls)
	return out
}

// DetectCalls returns every directory Detect has been called with, in call
// order. The returned slice is a copy.
func (f *FakeLinter) DetectCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.detectCalls))
	copy(out, f.detectCalls)
	return out
}

// CallCount returns how many times Lint has been called.
func (f *FakeLinter) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lintCalls)
}

var _ belay.Linter = (*FakeLinter)(nil)
