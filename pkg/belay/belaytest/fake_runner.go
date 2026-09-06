package belaytest

import (
	"context"
	"sync"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// FakeRunner is a scriptable belay.TestRunner for tests.
//
// The zero value is usable: Detect returns false (see DetectValue) and
// Test records the call and returns a zero belay.TestReport and a nil
// error. See the package doc for the Responses/Errs/Func scripting model.
type FakeRunner struct {
	// NameValue is returned by Name. Defaults to "fake-runner" if empty.
	NameValue string

	// DetectValue is returned by Detect, unless DetectFunc is set. It
	// defaults to false — the zero value — so a fake runner claims no
	// directory until a test opts it in explicitly.
	DetectValue bool

	// DetectFunc, if non-nil, is called instead of returning DetectValue —
	// for tests where whether a directory matches depends on the
	// directory (for example only paths ending in a known suffix).
	DetectFunc func(dir string) bool

	// Responses are returned in order, one per Test call; the last entry
	// repeats once exhausted.
	Responses []belay.TestReport

	// Errs are returned in order, one per Test call, independently of
	// Responses; the last entry repeats once exhausted.
	Errs []error

	// Func, if non-nil, is called instead of consulting Responses/Errs.
	// The call is still recorded either way.
	Func func(ctx context.Context, dir string) (belay.TestReport, error)

	mu          sync.Mutex
	testCalls   []string
	detectCalls []string
}

// Name implements belay.TestRunner.
func (f *FakeRunner) Name() string {
	if f.NameValue != "" {
		return f.NameValue
	}
	return "fake-runner"
}

// Detect implements belay.TestRunner. It records dir, then returns
// DetectFunc(dir) if DetectFunc is set, or otherwise DetectValue.
func (f *FakeRunner) Detect(dir string) bool {
	f.mu.Lock()
	f.detectCalls = append(f.detectCalls, dir)
	f.mu.Unlock()

	if f.DetectFunc != nil {
		return f.DetectFunc(dir)
	}
	return f.DetectValue
}

// Test implements belay.TestRunner. It records dir, then returns Func's
// result if Func is set, or otherwise the Responses/Errs entry for this
// call number.
func (f *FakeRunner) Test(ctx context.Context, dir string) (belay.TestReport, error) {
	n := f.recordTest(dir)

	if f.Func != nil {
		return f.Func(ctx, dir)
	}
	return pick(f.Responses, n), pick(f.Errs, n)
}

func (f *FakeRunner) recordTest(dir string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.testCalls = append(f.testCalls, dir)
	return len(f.testCalls) - 1
}

// TestCalls returns every directory Test has been called with, in call
// order. The returned slice is a copy.
func (f *FakeRunner) TestCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.testCalls))
	copy(out, f.testCalls)
	return out
}

// DetectCalls returns every directory Detect has been called with, in call
// order. The returned slice is a copy.
func (f *FakeRunner) DetectCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.detectCalls))
	copy(out, f.detectCalls)
	return out
}

// CallCount returns how many times Test has been called.
func (f *FakeRunner) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.testCalls)
}

var _ belay.TestRunner = (*FakeRunner)(nil)
