//go:build unix

// Package conformance is T33: the proof, not merely the claim, that
// internal/sonar/scanner (the deterministic belay.Reviewer, backed by the
// SonarQube scanner CLI) and internal/nodes/aireview (the AI-augmented one,
// backed by an MCP conversation) are interchangeable to everything that
// consumes their output.
//
// # Why this suite exists
//
// ADR 0008 lets belay swap its quality gate between review.mode "sonar" and
// review.mode "ai" without the rest of the graph noticing. That claim is
// only true if both adapters' belay.QualityReport values serialize to the
// same shape and, once projected through state.NewReview, the same
// state.Review shape too — since state.Review is what actually lands in
// state.json and is what a resumed run, a timeline, and a human reading
// the blackboard all depend on. Without a suite driving both reviewers
// through the same inputs and diffing the result, the two adapters can
// drift apart silently: nothing else in the codebase would notice until a
// run's state.json changed shape between review modes.
//
// # What "conformance" means here
//
// Both internal/sonar/scanner and internal/nodes/aireview return the exact
// same Go type, belay.QualityReport, so their top-level JSON key sets are
// identical by construction — the type system already guarantees that much
// for any two conforming belay.Reviewer implementations. What the type
// system does NOT guarantee, and what has already shipped as a real bug in
// this codebase once (see cloneSlice's doc comment in internal/state and
// cloneIssues's here), is that a slice serializes with the same JSON KIND
// on both sides: "issues": [] and "issues": null both satisfy "the key
// issues is present", but only one of them is the non-nil slice
// belay.QualityReport.Issues promises on every path. This suite asserts
// kind, not just key presence, key by key, for exactly that reason.
//
// # Structure
//
// scenarios (scenarios_test.go) is five hand-authored inputs, one per
// category T33 asks this suite to cover: a passing gate, a failing gate, a
// tool that ran but reached no verdict, an explicitly empty issue list, and
// an adapter that could not run at all. Each scenario scripts BOTH
// reviewers — a stub scanner.Execer replaying scripted CLI output for
// internal/sonar/scanner, a fake aireview.Session replaying scripted MCP
// tool responses for internal/nodes/aireview — against the identical
// belay.ReviewRequest, so a difference in the result can only come from the
// adapters themselves, never from divergent input.
//
// reviewersUnderTest (scenarios_test.go) is the fixed, enumerated roster
// this suite holds to that contract. TestReviewersUnderTestIsComplete pins
// its length so that adding a third belay.Reviewer to the codebase without
// adding a matching case here fails the build loudly, rather than letting
// the suite silently stop proving anything about the new adapter.
//
// conformance_test.go is the assertions themselves: per-report structural
// checks (Issues non-nil and serializes as an array, Counts is exactly the
// histogram of Issues, Raw is a non-nil JSON object, Summary is non-empty,
// Gate is never GateUnknown, and the documented
// Gate==GatePass-iff-Counts.AtOrAbove(FailOn)==0 invariant), a pairwise
// shape comparison between the two reviewers' reports, and the same shape
// comparison one level down on their state.Review projections.
//
// # No network, no Docker, no claude CLI
//
// Every scenario is driven through the two packages' own injection seams —
// scanner.WithExecer and aireview.WithSession / aireview.WithDialer — so no
// test in this package starts a subprocess, opens a socket, or spends
// money. The one exception is the "adapter could not run" scenario's
// aireview half, which points aireview.WithDockerBinary at a path inside
// t.TempDir() that provably does not exist: the failure happens in the
// kernel's exec lookup before any process is created, exactly the pattern
// internal/nodes/aireview's own tests use to prove the same thing.
package conformance
