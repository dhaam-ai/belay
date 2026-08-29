package belaytest

import (
	"context"
	"sync"

	"github.com/belay-dev/belay/pkg/belay"
)

// FakeAgent is a scriptable belay.AgentBackend for tests.
//
// The zero value is usable: Invoke records the request and returns a zero
// belay.AgentResponse and a nil error. See the package doc for the
// Responses/Errs/Func scripting model shared by every fake here.
type FakeAgent struct {
	// NameValue is returned by Name. Defaults to "fake-agent" if empty.
	NameValue string

	// Responses are returned in order, one per Invoke call; the last entry
	// repeats once exhausted. If empty, Invoke returns the zero
	// belay.AgentResponse.
	Responses []belay.AgentResponse

	// Errs are returned in order, one per Invoke call, independently of
	// Responses; the last entry repeats once exhausted. If empty, Invoke
	// never errors.
	Errs []error

	// Func, if non-nil, is called instead of consulting Responses/Errs —
	// for tests that need to react to the request's content. The call is
	// still recorded either way.
	Func func(ctx context.Context, req belay.AgentRequest) (belay.AgentResponse, error)

	mu    sync.Mutex
	calls []belay.AgentRequest
}

// Name implements belay.AgentBackend.
func (f *FakeAgent) Name() string {
	if f.NameValue != "" {
		return f.NameValue
	}
	return "fake-agent"
}

// Invoke implements belay.AgentBackend. It records req, then returns
// Func's result if Func is set, or otherwise the Responses/Errs entry for
// this call number.
func (f *FakeAgent) Invoke(ctx context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
	n := f.record(req)

	if f.Func != nil {
		return f.Func(ctx, req)
	}
	return pick(f.Responses, n), pick(f.Errs, n)
}

func (f *FakeAgent) record(req belay.AgentRequest) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	return len(f.calls) - 1
}

// Calls returns every request Invoke has received, in call order. The
// returned slice is a copy: mutating it does not affect the fake.
func (f *FakeAgent) Calls() []belay.AgentRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]belay.AgentRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// CallCount returns how many times Invoke has been called.
func (f *FakeAgent) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// LastCall returns the most recent request Invoke received, and false if
// Invoke has never been called.
func (f *FakeAgent) LastCall() (belay.AgentRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return belay.AgentRequest{}, false
	}
	return f.calls[len(f.calls)-1], true
}

var _ belay.AgentBackend = (*FakeAgent)(nil)
