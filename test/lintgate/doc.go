//go:build unix

// Package lintgate runs belay's lint gate against the real tools it wraps.
//
// internal/linter's own tests replay captured reports through a stub runner
// and never start a process, and TestNoRealLinterInvocation keeps it that
// way. Some behaviour only exists when the real tools run together, such as
// golangci-lint calling git to scope its findings to a run's change, so it is
// tested here. Each test skips when a tool it needs is not on PATH, so the
// suite still passes on a machine without them. The skip message says how to
// install what is missing; go test prints it only with -v.
package lintgate
