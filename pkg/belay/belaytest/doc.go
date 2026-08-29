// Package belaytest provides scriptable fakes for every interface in
// pkg/belay, for use in tests of code that consumes those interfaces —
// which, in belay, is nearly everything above the adapter layer.
//
// Each fake (FakeAgent for belay.AgentBackend, FakeRunner for
// belay.TestRunner, FakeLinter for belay.Linter, FakeIsolator for
// belay.Isolator, FakeReviewer for belay.Reviewer) follows the same shape:
//
//   - The zero value is usable: it records every call and returns zero
//     results with a nil error.
//   - A Responses (or CreateResponses) slice scripts return values across
//     successive calls, one entry per call; once exhausted, the last entry
//     repeats for every later call, so a fake configured with a single
//     entry behaves the same on call one and call one hundred.
//   - An Errs slice scripts errors the same way, independently of
//     Responses — index N of each is used for call N, so "fail every call"
//     is a one-element Errs and "fail once, then succeed" is a two-element
//     Errs whose second entry is nil.
//   - A Func field, when set, is consulted instead of Responses/Errs, for
//     tests that need to react to what a call actually contains.
//   - Every call is recorded and readable back out (Calls, CallCount,
//     LastCall, or the fake-specific equivalent), so tests can assert on
//     what the code under test actually sent, not just on what came back.
//
// Concurrent calls into a fake's interface method are safe: a fake may be
// shared across goroutines by code under test that fans work out (belay's
// best-of-N pillar makes this the common case, not an edge case). Setting
// Responses, Errs or Func is not itself concurrency-safe — configure a fake
// fully before handing it to concurrent code, the same way you would
// populate a slice before starting goroutines that read it.
//
// # Example
//
//	agent := &belaytest.FakeAgent{
//		Responses: []belay.AgentResponse{
//			{Text: "first attempt"},
//			{Text: "second attempt, after feedback"},
//		},
//	}
//	// ... run code under test that calls agent.Invoke twice ...
//	if got := agent.CallCount(); got != 2 {
//		t.Fatalf("Invoke called %d times, want 2", got)
//	}
package belaytest
