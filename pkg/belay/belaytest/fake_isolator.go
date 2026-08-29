package belaytest

import (
	"context"
	"sync"

	"github.com/belay-dev/belay/pkg/belay"
)

// CreateCall records one belay.Isolator.Create call a FakeIsolator
// received.
type CreateCall struct {
	Src string
	ID  string
}

// FakeIsolator is a scriptable belay.Isolator for tests.
//
// The zero value is usable: Create returns Workspace{ID: id, Dir: src} —
// effectively "no isolation" — and Destroy succeeds silently. See the
// package doc for the Responses/Errs/Func scripting model.
type FakeIsolator struct {
	// CreateResponses are returned in order, one per Create call; the last
	// entry repeats once exhausted. If both CreateResponses and
	// CreateFunc are unset, Create returns Workspace{ID: id, Dir: src} — a
	// reasonable default for tests that do not care about isolation
	// semantics.
	CreateResponses []belay.Workspace

	// CreateErrs are returned in order, one per Create call, independently
	// of CreateResponses; the last entry repeats once exhausted.
	CreateErrs []error

	// CreateFunc, if non-nil, is called instead of consulting
	// CreateResponses/CreateErrs. The call is still recorded either way.
	CreateFunc func(ctx context.Context, src, id string) (belay.Workspace, error)

	// DestroyErrs are returned in order, one per Destroy call; the last
	// entry repeats once exhausted.
	DestroyErrs []error

	// DestroyFunc, if non-nil, is called instead of consulting
	// DestroyErrs. The call is still recorded either way.
	DestroyFunc func(ctx context.Context, ws belay.Workspace) error

	mu           sync.Mutex
	createCalls  []CreateCall
	created      []belay.Workspace // successful Create returns only
	destroyCalls []belay.Workspace
}

// Create implements belay.Isolator.
func (f *FakeIsolator) Create(ctx context.Context, src, id string) (belay.Workspace, error) {
	n := f.recordCreateCall(src, id)

	var ws belay.Workspace
	var err error
	switch {
	case f.CreateFunc != nil:
		ws, err = f.CreateFunc(ctx, src, id)
	case len(f.CreateResponses) == 0:
		ws, err = belay.Workspace{ID: id, Dir: src}, pick(f.CreateErrs, n)
	default:
		ws, err = pick(f.CreateResponses, n), pick(f.CreateErrs, n)
	}

	if err == nil {
		f.recordCreated(ws)
	}
	return ws, err
}

func (f *FakeIsolator) recordCreateCall(src, id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls = append(f.createCalls, CreateCall{Src: src, ID: id})
	return len(f.createCalls) - 1
}

func (f *FakeIsolator) recordCreated(ws belay.Workspace) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, ws)
}

// Destroy implements belay.Isolator.
func (f *FakeIsolator) Destroy(ctx context.Context, ws belay.Workspace) error {
	n := f.recordDestroy(ws)

	if f.DestroyFunc != nil {
		return f.DestroyFunc(ctx, ws)
	}
	return pick(f.DestroyErrs, n)
}

func (f *FakeIsolator) recordDestroy(ws belay.Workspace) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyCalls = append(f.destroyCalls, ws)
	return len(f.destroyCalls) - 1
}

// CreateCalls returns every (src, id) pair Create has been called with, in
// call order. The returned slice is a copy.
func (f *FakeIsolator) CreateCalls() []CreateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]CreateCall, len(f.createCalls))
	copy(out, f.createCalls)
	return out
}

// DestroyCalls returns every Workspace Destroy has been called with, in
// call order. The returned slice is a copy.
func (f *FakeIsolator) DestroyCalls() []belay.Workspace {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]belay.Workspace, len(f.destroyCalls))
	copy(out, f.destroyCalls)
	return out
}

// Leaked returns every Workspace a successful Create call returned that has
// not since been passed to Destroy, matched by Workspace.ID — the leak an
// Isolator's caller must not produce. Call it at the end of a test to
// assert cleanup actually happened:
//
//	if leaked := isolator.Leaked(); len(leaked) > 0 {
//		t.Errorf("workspace(s) never destroyed: %v", leaked)
//	}
func (f *FakeIsolator) Leaked() []belay.Workspace {
	f.mu.Lock()
	defer f.mu.Unlock()

	destroyed := make(map[string]bool, len(f.destroyCalls))
	for _, ws := range f.destroyCalls {
		destroyed[ws.ID] = true
	}

	var leaked []belay.Workspace
	for _, ws := range f.created {
		if !destroyed[ws.ID] {
			leaked = append(leaked, ws)
		}
	}
	return leaked
}

var _ belay.Isolator = (*FakeIsolator)(nil)
