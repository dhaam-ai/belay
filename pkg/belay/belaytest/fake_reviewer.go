package belaytest

import (
	"context"
	"sync"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// FakeReviewer is a scriptable belay.Reviewer for tests.
//
// The zero value is usable: Review records the call and returns a zero
// belay.QualityReport and a nil error. See the package doc for the
// Responses/Errs/Func scripting model. FakeReviewer is a general-purpose
// test double for code that calls a Reviewer; it is not a substitute for
// the cross-adapter conformance suite that exercises two real Reviewer
// implementations against the QualityReport contract.
type FakeReviewer struct {
	// Responses are returned in order, one per Review call; the last entry
	// repeats once exhausted.
	Responses []belay.QualityReport

	// Errs are returned in order, one per Review call, independently of
	// Responses; the last entry repeats once exhausted.
	Errs []error

	// Func, if non-nil, is called instead of consulting Responses/Errs —
	// for tests that need Gate to depend on req.FailOn, for instance. The
	// call is still recorded either way.
	Func func(ctx context.Context, req belay.ReviewRequest) (belay.QualityReport, error)

	mu    sync.Mutex
	calls []belay.ReviewRequest
}

// Review implements belay.Reviewer. It records req, then returns Func's
// result if Func is set, or otherwise the Responses/Errs entry for this
// call number.
func (f *FakeReviewer) Review(ctx context.Context, req belay.ReviewRequest) (belay.QualityReport, error) {
	n := f.record(req)

	if f.Func != nil {
		return f.Func(ctx, req)
	}
	return pick(f.Responses, n), pick(f.Errs, n)
}

func (f *FakeReviewer) record(req belay.ReviewRequest) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	return len(f.calls) - 1
}

// Calls returns every request Review has received, in call order. The
// returned slice is a copy.
func (f *FakeReviewer) Calls() []belay.ReviewRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]belay.ReviewRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// CallCount returns how many times Review has been called.
func (f *FakeReviewer) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var _ belay.Reviewer = (*FakeReviewer)(nil)
