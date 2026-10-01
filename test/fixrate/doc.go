//go:build fixrate

// Package fixrate is belay's fix-rate measurement harness (T42): the
// instrument behind the result-driven pillar's headline claim, "belay
// repairs N/M seeded defects."
//
// # What it does
//
// For a chosen subset of the fixtures/seeded-bug defect catalogue, Run:
//
//  1. injects the defects into a fresh temporary copy of the fixture
//     (shelling out to fixtures/seeded-bug/inject.go — see below for why
//     that is a subprocess call rather than an import),
//  2. drives belay's real dispatcher graph (internal/graph +
//     internal/nodes, unmodified) against that copy, using an
//     AgentBackend selected by Mode,
//  3. re-runs the target's own test suite and linter once belay finishes,
//  4. diffs the result against injected.json's documented baseline to
//     decide, per defect, whether it was repaired, and
//  5. runs the cheat detector (see below) before crediting any repair.
//
// The result is a machine-readable Result (JSON) and a human summary
// (Result.HumanSummary), broken down per defect and per bug class — never
// only an aggregate — because "belay repairs 90% of off-by-ones but 20% of
// concurrency bugs" is the useful claim an aggregate hides.
//
// # Why fixtures/seeded-bug is a subprocess, never an import
//
// fixtures/seeded-bug is its own Go module (belay.dev/fixtures/seededbug),
// deliberately outside this module's dependency graph so that catalogued
// defects can never leak into belay's own build. This package is
// forbidden from adding a module dependency or a replace directive to
// reach it (T42's own scope rules), and would not want to even if it
// could: injected.json is fixtures/seeded-bug's public, documented
// contract for exactly this purpose. So this package talks to the fixture
// exactly the way any external tool would — `go run ./inject.go
// --bugs=... --out=<dir>` as a child process, followed by reading
// injected.json — and never imports a single identifier from
// belay.dev/fixtures/seededbug/bugs or /inject.go.
//
// One consequence: this package's "repair" and "cheat" demonstration
// agent (see agent.go) cannot read a Defect's exact Find/Replace text
// either, since that only exists inside the foreign module. It works at
// the file level instead — restoring a known-good file verbatim from
// fixtures/seeded-bug/src, or truncating a *_test.go file — which is both
// simpler and, for a fake standing in for "a coding agent edited this
// file," a more honest simulation than reverse-engineering a byte range.
//
// # Two modes, and why the difference is load-bearing
//
// See Mode. The short version: ModeReplay costs nothing and measures
// belay's graph — its plumbing, not any particular agent's coding skill —
// and ModeLive spends real money against a real agent. A number from one
// mode is not comparable to a number from the other, and Result makes the
// provenance impossible to miss: it is the first thing in the JSON
// (Result.Mode, Result.Provenance) and the first line of the human
// summary. Run also actively verifies the zero-cost promise of replay
// mode rather than merely asserting it — see Run's doc comment.
//
// # The cheat detector
//
// A "fix" that makes a catalogued test pass by deleting or weakening that
// test, rather than by changing the code the test exercises, is not a
// repair. See evaluate.go's classify for the exact rule; in short, a
// defect is credited as repaired only when its target source file (named
// in injected.json) changed and no *_test.go file changed unaccountably
// alongside a newly-green or newly-vanished catalogued test. A defect
// whose catalogued test vanished, or turned green, without its own source
// file changing is reported as invalid_repair — a distinct outcome from
// both repaired and not_repaired — and never counted toward the fix rate.
//
// # Two arms: the claim is a delta
//
// Run is the belay arm. RunControl is the control arm: the same backend,
// invoked exactly once per task with no graph -- no plan, no fix loop, no
// quality gate -- given the same goal, model and tools as belay's code node
// and an allowance at least as large as belay's whole graph could have
// used. Both go through one pipeline (runArm), so they are injected,
// evaluated and cheat-checked by the same code, and both keep every
// refusal described here. A fix rate measured against nothing cannot be a
// claim about belay, because it does not separate belay's orchestration
// from the agent underneath; the paired report (PairedResult) can, and it
// leads with the cost of one resolved task, since spending more is always
// a way to resolve more. See control.go and paired.go, and README.md's
// "Two arms" for what a delta can and cannot claim.
//
// # SWE-bench, offline
//
// swebench.go turns SWE-bench records into Tasks and ports SWE-bench's own
// grading rule. It runs nothing -- no container, no clone, no patch, no
// network -- and every claim it makes about SWE-bench is cited to a primary
// source or marked UNKNOWN. The dataset's licence is UNKNOWN, so no dataset
// content is committed: every fixture under testdata/swebench is
// synthetic, and a test refuses one that is not.
//
// # Build tag
//
// This package is gated behind the "fixrate" build tag. Its default mode
// (ModeReplay) is free and fast enough for CI, but the package pulls in
// belay's full graph/node stack and shells out to `go run`, `go test
// -race` and (opportunistically) golangci-lint per defect set — enough
// weight that T42 asks for it to be opt-in rather than part of the root
// module's default `go build ./...` / `go test ./...`. Build and test it
// explicitly:
//
//	go build -tags fixrate ./test/fixrate/...
//	go test -tags fixrate ./test/fixrate/... -race
package fixrate
