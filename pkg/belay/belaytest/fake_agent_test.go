package belaytest_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/belay-dev/belay/pkg/belay"
	"github.com/belay-dev/belay/pkg/belay/belaytest"
)

func TestFakeAgentZeroValue(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeAgent{}

	if got := f.Name(); got != "fake-agent" {
		t.Errorf("zero-value Name() = %q, want %q", got, "fake-agent")
	}
	if _, ok := f.LastCall(); ok {
		t.Error("LastCall() ok = true before any Invoke, want false")
	}

	resp, err := f.Invoke(context.Background(), belay.AgentRequest{Prompt: "hi"})
	if err != nil {
		t.Errorf("zero-value Invoke returned error: %v", err)
	}
	if !reflect.DeepEqual(resp, belay.AgentResponse{}) {
		t.Errorf("zero-value Invoke returned %+v, want zero value", resp)
	}
	if got := f.CallCount(); got != 1 {
		t.Errorf("CallCount() = %d, want 1", got)
	}
}

func TestFakeAgentNameValue(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeAgent{NameValue: "claude-code"}
	if got := f.Name(); got != "claude-code" {
		t.Errorf("Name() = %q, want %q", got, "claude-code")
	}
}

func TestFakeAgentScriptedResponsesStickyOnLast(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	f := &belaytest.FakeAgent{
		Responses: []belay.AgentResponse{
			{Text: "first"},
			{Text: "second"},
		},
		Errs: []error{errBoom, nil},
	}

	wantText := []string{"first", "second", "second", "second"}
	wantErr := []error{errBoom, nil, nil, nil}

	for i, want := range wantText {
		resp, err := f.Invoke(context.Background(), belay.AgentRequest{Prompt: "p"})
		if resp.Text != want {
			t.Errorf("call %d: Text = %q, want %q", i, resp.Text, want)
		}
		if !errors.Is(err, wantErr[i]) {
			t.Errorf("call %d: err = %v, want %v", i, err, wantErr[i])
		}
	}
}

func TestFakeAgentFuncOverridesResponses(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeAgent{
		Responses: []belay.AgentResponse{{Text: "ignored"}},
		Func: func(_ context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
			return belay.AgentResponse{Text: "echo: " + req.Prompt}, nil
		},
	}

	resp, err := f.Invoke(context.Background(), belay.AgentRequest{Prompt: "hello"})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if want := "echo: hello"; resp.Text != want {
		t.Errorf("Text = %q, want %q", resp.Text, want)
	}
}

func TestFakeAgentRecordsCalls(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeAgent{}
	ctx := context.Background()

	if _, err := f.Invoke(ctx, belay.AgentRequest{Prompt: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Invoke(ctx, belay.AgentRequest{Prompt: "two"}); err != nil {
		t.Fatal(err)
	}

	calls := f.Calls()
	if len(calls) != 2 {
		t.Fatalf("Calls() has %d entries, want 2", len(calls))
	}
	if calls[0].Prompt != "one" || calls[1].Prompt != "two" {
		t.Errorf("Calls() = %+v, want prompts [one two]", calls)
	}

	last, ok := f.LastCall()
	if !ok || last.Prompt != "two" {
		t.Errorf("LastCall() = (%+v, %v), want (Prompt: two, true)", last, ok)
	}

	// Calls() returns a copy: mutating it must not affect the fake.
	calls[0].Prompt = "mutated"
	if again := f.Calls(); again[0].Prompt != "one" {
		t.Error("mutating the slice returned by Calls() affected the fake's internal state")
	}
}

// TestFakeAgentConcurrentInvoke proves FakeAgent is safe to share across
// goroutines, the way a best-of-N fanout would use it.
func TestFakeAgentConcurrentInvoke(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeAgent{Responses: []belay.AgentResponse{{Text: "ok"}}}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := f.Invoke(context.Background(), belay.AgentRequest{Prompt: "concurrent"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if got := f.CallCount(); got != n {
		t.Errorf("CallCount() = %d, want %d", got, n)
	}
}

var _ belay.AgentBackend = (*belaytest.FakeAgent)(nil)
