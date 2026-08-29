// Package belay defines belay's public, semver-stable extension contract:
// the interfaces and types a third-party backend or adapter author compiles
// against to plug into the graph runner, and nothing else.
//
// belay itself — the dispatcher, the state machine, the journal, the CLI —
// lives outside this package, in internal/. pkg/belay is deliberately the
// only part of belay an external module can import, and every exported
// symbol here is a promise: changing one is a breaking change for every
// adapter author who compiled against it, whether or not belay's own
// maintainers remember they made the promise. That is Hyrum's Law, and it
// is the reason this package stays small.
//
// # Design
//
// pkg/belay is a deep module: five interfaces, each with one or two
// methods, sitting in front of everything a backend, test runner, linter,
// reviewer or workspace isolator actually has to do. Nothing here — a
// timeout strategy, a retry policy, how a candidate is scored, how state is
// persisted — is part of the contract; those live in the graph runner,
// which drives these interfaces without needing to know which concrete
// adapter it is holding.
//
// Where a design decision was genuinely ambiguous, this package resolves it
// once, in a doc comment, rather than leaving every adapter author to guess
// differently: what an empty AllowedTools means, whether a zero-count
// TestReport is a pass, how a Sonar-flavored Reviewer must not confuse
// Sonar's own "ERROR" gate string with belay's GateError. Read the doc
// comment on the type or field before implementing it — the reasoning
// usually matters as much as the shape.
//
// None of the JSON struct tags in this package use `omitempty`: every field
// always serializes, so a consumer reading the run journal never has to
// special-case a missing key. One consequence of that, worth knowing before
// it surprises a round-trip test: encoding/json's json.RawMessage marshals
// a nil value as the literal 4-byte JSON value null (that is
// json.RawMessage's own behavior, not something this package adds), so an
// opaque field such as AgentResponse.Raw or QualityReport.Raw that starts
// as Go nil comes back from a JSON round trip as json.RawMessage("null"),
// not nil again. Nothing in belay treats that difference as meaningful.
//
// # The interfaces
//
//   - AgentBackend wraps a coding-agent CLI or API: one Invoke call,
//     AgentRequest in, AgentResponse out. This is the whole of belay's
//     CLI-agnostic orchestration pillar — swap backends without touching
//     graph logic.
//   - TestRunner and Linter both follow a Name/Detect/Do pattern: the
//     dispatcher auto-detects which one applies to a directory before
//     running it.
//   - Reviewer performs a holistic, threshold-aware quality review — the
//     counterpart to Linter for tools (or an AI reviewer) that need a
//     caller-supplied severity threshold and a broader view of what
//     changed, rather than a bare directory.
//   - Isolator creates and destroys the isolated workspace each run or
//     best-of-N candidate works in.
//
// TestRunner, Linter and Reviewer converge on two shared result contracts:
// TestReport (see runner.go) and QualityReport (see quality.go).
// QualityReport in particular is designed so a deterministic tool
// (golangci-lint, sonar-scanner) and an AI-driven reviewer are fully
// interchangeable to the graph — read its doc comment before implementing
// either side.
//
// # Extension model
//
// A third-party backend implements AgentBackend and nothing else — no
// interface embeds it, no base struct is required, and nothing in belay
// needs to know the concrete type exists beyond this interface:
//
//	type shellBackend struct {
//		bin string // path to some third-party agent CLI
//	}
//
//	func (b *shellBackend) Name() string { return "shell-agent" }
//
//	func (b *shellBackend) Invoke(ctx context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
//		cmd := exec.CommandContext(ctx, b.bin, "-p", req.Prompt)
//		cmd.Dir = req.WorkDir
//		out, err := cmd.Output()
//		if err != nil {
//			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
//				return belay.AgentResponse{}, fmt.Errorf("shell-agent: %w", ctx.Err())
//			}
//			return belay.AgentResponse{}, fmt.Errorf("shell-agent: invoke: %w", err)
//		}
//		return belay.AgentResponse{
//			Text: string(out),
//			Raw:  json.RawMessage(out),
//		}, nil
//	}
//
// A caller wires it in purely through the interface:
//
//	var backend belay.AgentBackend = &shellBackend{bin: "some-agent-cli"}
//	resp, err := backend.Invoke(ctx, belay.AgentRequest{Prompt: "...", WorkDir: dir})
//
// TestRunner, Linter, Reviewer and Isolator follow the same shape: a small
// concrete type, the interface's methods, no other dependency on belay. See
// this package's Example, and pkg/belay/belaytest, for runnable
// implementations.
//
// # Errors
//
// Every sentinel in this package (ErrToolchainMissing, ErrBudgetExceeded,
// ErrGateFailed, ErrUnsupported, and the two enum-parsing sentinels
// ErrUnknownSeverity and ErrUnknownGateStatus) is meant to be checked with
// errors.Is, and the two that carry adapter-useful data have a companion
// struct — ToolchainError, BudgetError — recoverable with errors.As. See
// errors.go.
//
// # Stability
//
// This package imports nothing from belay's internal/ tree — that is
// enforced mechanically by an internal_boundary_test.go that walks the real
// build graph, not by convention — so an adapter author never transitively
// depends on code that carries no compatibility promise. Within pkg/belay
// itself, evolution is additive only: new fields land as optional (their
// zero value must be a sensible default, since old callers won't set them)
// and new interface methods are never added to an interface that already
// ships, because that would break every existing implementation the moment
// they upgrade. A method that must be added later goes on a new, separate
// interface instead.
package belay
