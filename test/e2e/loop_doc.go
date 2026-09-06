//go:build e2e

// Package e2e is belay's end-to-end proof that the graph actually completes
// a run: `belay run`'s whole loop, from a fresh run directory to a finished
// one, driven through the real internal/graph.Dispatcher, the real
// internal/nodes.Default registry, and the real internal/state and
// internal/journal packages.
//
// # Why this suite exists
//
// Every other test in this repository proves one package in isolation
// against fakes of its immediate neighbors. None of them proves that the
// pieces actually fit together: that internal/nodes.Default wires a
// registry the dispatcher can drive end to end, that a node's Result
// actually routes to the node its own doc comment claims, that a manifest
// and a journal written by one node execution are exactly what the next one
// reads back. This package is the one place that runs the whole thing.
//
// # What is faked, and why
//
// Per belay's own adapter boundary (pkg/belay), the only thing standing
// between a real run and a live coding agent, a live test toolchain and a
// live linter is the three interfaces in that package: belay.AgentBackend,
// belay.TestRunner and belay.Linter. This suite fakes exactly those three,
// with pkg/belay/belaytest's scriptable fakes, and nothing else. Every
// other moving part — internal/graph.Dispatcher, internal/nodes' seven
// registered nodes, internal/state's Store and Layout, internal/journal's
// append-only log — is the genuine, unmodified package, operating against
// a real temporary directory on disk.
//
// belaytest.FakeAgent is scripted to answer the way a real coding agent
// answers: a plan node call gets a Markdown plan document back, a code node
// call gets a prose summary followed by the fenced "changed-files" block
// the code node's prompt asks for, naming a file this suite seeds into the
// workspace beforehand — so the real write node verifies a real, present
// file on a real filesystem, not a claim taken on faith.
//
// # No network, no Docker, no `claude` CLI
//
// Nothing in this package spawns a subprocess, opens a socket, or invokes
// the claude binary. Every adapter is one of the three belaytest fakes
// above; the workspace a "run" operates on is a t.TempDir() populated with
// a few plain text files. The suite costs nothing to run and needs no
// credentials, in CI or on a laptop with no agent CLI installed at all.
//
// # Build tag
//
// This package is gated behind the e2e build tag, matching the Makefile's
// `e2e` target (`go test ./test/e2e/... -tags=e2e`) and the repository's
// existing test/fixrate convention: `go test ./...` with no tag never runs
// it, because driving the real dispatcher through eight full scenarios is
// slower than the unit suite it complements, not because it is unsafe or
// expensive to run.
package e2e
