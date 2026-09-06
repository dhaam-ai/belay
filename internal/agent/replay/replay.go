// Package replay decorates a belay.AgentBackend with record and replay
// behavior, so belay's own test suite — and any fork's — can exercise the
// full plan/code/test/review graph without spending money or touching the
// network on every run (ADR-0009).
//
// # Modes
//
// Backend has three modes, matching config.Agent.Mode's three values.
// This package deliberately does not import internal/config — a
// backend-agnostic recording decorator has no business depending on
// belay's configuration schema — so it defines its own Mode with
// identical underlying strings; a caller that already has a
// config.AgentMode converts it directly: replay.Mode(cfg.Agent.Mode).
//
//   - ModeLive passes every Invoke straight through to an inner
//     belay.AgentBackend and does nothing else. Construct with Live.
//   - ModeRecord passes Invoke through to inner AND appends a scrubbed
//     record of the request and response to an in-memory Cassette.
//     Construct with Record, and persist the result with
//     Backend.Cassette().Save(path) once the run finishes. See record.go
//     for the scrubbing rules.
//   - ModeReplay never calls any inner backend — Backend does not even
//     hold a reference to one in this mode — and instead serves Invoke
//     entirely from a Cassette loaded ahead of time. Construct with
//     Replay. A request with no matching recorded interaction produces a
//     *MissingInteractionError, never a silent fallthrough to a live
//     call: see MissingInteractionError.
//
// # Cassette format and fingerprinting
//
// See cassette.go: Cassette is the on-disk format, Fingerprint is the key
// interactions are looked up by, and NormalizedRequest documents exactly
// which two AgentRequest fields are replaced with placeholders before
// hashing, and why those two specifically cannot be hashed as recorded.
//
// # Repeated identical requests
//
// belay's fix loop can legitimately send the same logical request — same
// prompt, same everything Fingerprint hashes — more than once across
// retry attempts. Cassette interactions are looked up FINGERPRINT-KEYED
// WITH REPEAT COUNT, not by one global sequence number: for a given
// Fingerprint, the Nth Invoke call carrying that Fingerprint is served
// the Nth Interaction recorded under it, in the order Record observed
// them. This is deliberately unlike belaytest.FakeAgent's flat global
// Responses[callNumber] indexing — a global index would break the moment
// a narrower test replayed only some of a larger cassette's
// interactions, since every earlier skipped call would shift every later
// index. Per-fingerprint indexing tolerates that: it only requires each
// individual fingerprint's own interactions to be consumed in the order
// they were recorded, not that the whole cassette be replayed start to
// finish.
//
// A request whose Fingerprint has no interactions left to serve — either
// because it never appeared in the cassette at all, or because it has
// now been requested more times than it was recorded — is a
// *MissingInteractionError. Both cases are the same failure mode from a
// caller's perspective (the cassette does not have an answer for this
// call) and both name the fingerprint and the attempt number so a
// maintainer can tell, at a glance, which is which.
//
// # A known limitation: identical prompts with no other distinguishing
// field
//
// Fingerprint deliberately excludes WorkDir (see NormalizedRequest), so
// two logically DIFFERENT requests that happen to be byte-identical in
// every field Fingerprint hashes — for example, best-of-N fanout
// candidates that all receive the exact same prompt and differ only in
// which isolated workspace they run in — collapse onto one Fingerprint.
// Recording and later replaying such a run correctly depends on both runs
// invoking that fingerprint's candidates in the same order, which a
// concurrent fanout does not by itself guarantee. AgentRequest carries no
// field a decorator could use to disambiguate fanout candidates without
// reintroducing exactly the machine- and run-specific instability
// Fingerprint exists to avoid, so this is accepted as a residual risk
// rather than solved here — a maintainer recording a cassette that
// exercises fanout should record it with fanout concurrency disabled (or
// width 1) if reproducible replay matters for that test.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// Mode selects how a Backend drives its inner belay.AgentBackend.
//
// Its three values are the same strings as config.AgentMode
// (config.AgentModeLive, config.AgentModeRecord, config.AgentModeReplay).
// See the package doc for why this package defines its own type instead
// of importing internal/config.
type Mode string

// Modes.
const (
	// ModeLive passes every Invoke straight through to the inner backend.
	ModeLive Mode = "live"
	// ModeRecord passes Invoke through to the inner backend and records it.
	ModeRecord Mode = "record"
	// ModeReplay serves Invoke from a Cassette and calls no inner backend.
	ModeReplay Mode = "replay"
)

// ErrInvalidMode reports a Mode value that is none of ModeLive,
// ModeRecord or ModeReplay.
var ErrInvalidMode = errors.New("replay: invalid mode")

// ErrNoInteraction indicates that a ModeReplay Backend received a request
// with no matching recorded interaction left to serve. Detect it with
// errors.Is; recover the fingerprint and call counts with errors.As
// against *MissingInteractionError.
var ErrNoInteraction = errors.New("replay: no recorded interaction for this request")

// MissingInteractionError reports exactly which request a ModeReplay
// Backend could not serve, so a maintainer can find — or re-record — the
// right cassette entry instead of guessing.
//
// It is returned instead of ever falling through to a live call: a
// fallthrough would let a stale cassette fail silently, and expensively,
// in CI, at the first request it cannot answer, rather than loudly and
// for free.
type MissingInteractionError struct {
	// Fingerprint is the request's computed Fingerprint.
	Fingerprint string
	// Request is the normalized request that produced Fingerprint, for a
	// human-readable error message.
	Request NormalizedRequest
	// Attempt is the 1-based count of how many times this Fingerprint has
	// now been requested from this Backend.
	Attempt int
	// Recorded is how many interactions the cassette actually holds for
	// Fingerprint. Zero means the fingerprint is entirely absent from the
	// cassette; a value less than Attempt means it was recorded fewer
	// times than it is now being requested.
	Recorded int
}

// Error implements error.
func (e *MissingInteractionError) Error() string {
	if e.Recorded == 0 {
		return fmt.Sprintf(
			"replay: no recorded interaction for fingerprint %s (prompt %q) — re-record the cassette",
			e.Fingerprint, truncate(e.Request.Prompt, 80))
	}
	return fmt.Sprintf(
		"replay: fingerprint %s has %d recorded interaction(s) but was requested a %s time — re-record the cassette",
		e.Fingerprint, e.Recorded, ordinal(e.Attempt))
}

// Unwrap reports ErrNoInteraction.
func (e *MissingInteractionError) Unwrap() error { return ErrNoInteraction }

// ReplayedError is returned by a ModeReplay Backend's Invoke when the
// interaction being served itself recorded a non-nil error.
//
// It carries only the original error's scrubbed message, not its
// original Go type or sentinel chain: a cassette is portable JSON, and an
// arbitrary error recorded during a prior live run cannot be decoded back
// into whatever concrete type it originally was (a *belay.ToolchainError,
// an *InvokeError from internal/agent/claude, ...). errors.Is against
// that ORIGINAL sentinel therefore does not hold for a ReplayedError.
// This is an accepted limitation, not an oversight: a cassette exists to
// make CI deterministic and free, not to preserve exact error identity
// across a JSON boundary. Code whose control flow depends on a specific
// sentinel from a specific node must be exercised in ModeLive or
// ModeRecord, where the real error type flows through untouched.
type ReplayedError struct {
	// Fingerprint is the request's computed Fingerprint.
	Fingerprint string
	// Message is the original error's scrubbed message.
	Message string
}

// Error implements error.
func (e *ReplayedError) Error() string {
	return fmt.Sprintf("replay: recorded interaction for fingerprint %s returned an error: %s", e.Fingerprint, e.Message)
}

// Backend decorates an inner belay.AgentBackend with the record/replay
// behavior selected by its mode. Construct one with Live, Record, Replay,
// or New; the zero Backend is not usable — Invoke on it returns
// ErrInvalidMode rather than panicking.
type Backend struct {
	// Logger receives structured events. Nil uses slog.Default.
	Logger *slog.Logger

	mode  Mode
	inner belay.AgentBackend

	mu       sync.Mutex
	cassette *Cassette                // record: accumulating; replay: source
	index    map[string][]Interaction // replay only: fingerprint -> recorded interactions, in order
	cursor   map[string]int           // replay only: fingerprint -> next index into index[fp]
	seen     []string                 // every fingerprint Invoke has computed, any mode
}

// Live returns a Backend that passes every Invoke straight through to
// inner and does nothing else. inner must not be nil.
func Live(inner belay.AgentBackend) *Backend {
	return &Backend{mode: ModeLive, inner: inner}
}

// Record returns a Backend that passes every Invoke through to inner and
// additionally appends a scrubbed record of the request and response to
// an in-memory Cassette, tagged with prov. inner must not be nil.
//
// prov.Backend is overwritten with inner.Name(), so a cassette's recorded
// backend identity can never drift from the backend that actually
// produced its interactions, regardless of what the caller passed in
// prov.
//
// Retrieve the accumulated recording with Backend.Cassette, and persist
// it with Cassette.Save once the run finishes — typically in a defer at
// the call site that constructed this Backend.
func Record(inner belay.AgentBackend, prov Provenance) *Backend {
	prov.Backend = inner.Name()
	return &Backend{
		mode:  ModeRecord,
		inner: inner,
		cassette: &Cassette{
			FormatVersion: FormatVersion,
			Provenance:    prov,
		},
	}
}

// Replay returns a Backend that serves every Invoke from cassette and
// calls no inner backend whatsoever. Backend stores no reference to an
// inner belay.AgentBackend in this mode, so there is no code path by
// which ModeReplay could reach a subprocess or the network — there is
// simply nothing here capable of starting one.
//
// Replay rejects a nil cassette and a cassette whose FormatVersion is not
// FormatVersion.
func Replay(cassette *Cassette) (*Backend, error) {
	if cassette == nil {
		return nil, errors.New("replay: cassette must not be nil")
	}
	if cassette.FormatVersion != FormatVersion {
		return nil, &FormatVersionError{Got: cassette.FormatVersion}
	}
	index := make(map[string][]Interaction, len(cassette.Interactions))
	for _, it := range cassette.Interactions {
		index[it.Fingerprint] = append(index[it.Fingerprint], it)
	}
	return &Backend{
		mode:     ModeReplay,
		cassette: cassette,
		index:    index,
		cursor:   make(map[string]int),
	}, nil
}

// New constructs a Backend for mode, wiring inner, cassette and prov as
// that mode requires:
//
//   - ModeLive: inner is required; cassette and prov are ignored.
//   - ModeRecord: inner is required; prov tags the recording; cassette is
//     ignored (Record always starts a fresh in-memory cassette).
//   - ModeReplay: cassette is required; inner and prov are ignored — and,
//     critically, inner is never stored or called. Passing a non-nil
//     inner here does not create a fallthrough path; it is simply
//     discarded.
//
// New exists for a caller that selects Mode dynamically, such as
// internal/cli wiring config.Agent.Mode straight through. A caller that
// knows its mode statically should prefer Live, Record or Replay
// directly.
func New(mode Mode, inner belay.AgentBackend, cassette *Cassette, prov Provenance) (*Backend, error) {
	switch mode {
	case ModeLive:
		if inner == nil {
			return nil, fmt.Errorf("replay: %s mode requires an inner backend", ModeLive)
		}
		return Live(inner), nil
	case ModeRecord:
		if inner == nil {
			return nil, fmt.Errorf("replay: %s mode requires an inner backend", ModeRecord)
		}
		return Record(inner, prov), nil
	case ModeReplay:
		return Replay(cassette)
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidMode, mode)
	}
}

// Name reports the identity of the backend actually answering Invoke.
//
// In ModeLive and ModeRecord it is inner.Name(). In ModeReplay, where
// there is no inner backend, it is the cassette's own recorded
// Provenance.Backend — the name captured when the cassette was made — so
// a caller logging Name() sees the same backend identity regardless of
// which mode produced the response.
func (b *Backend) Name() string {
	if b.mode == ModeReplay {
		if b.cassette != nil && b.cassette.Provenance.Backend != "" {
			return b.cassette.Provenance.Backend
		}
		return "replay"
	}
	if b.inner == nil {
		return "replay"
	}
	return b.inner.Name()
}

func (b *Backend) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

// Invoke implements belay.AgentBackend, dispatching to the inner backend,
// to recording, or to replay according to Mode.
func (b *Backend) Invoke(ctx context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
	fp := Fingerprint(req)
	b.mu.Lock()
	b.seen = append(b.seen, fp)
	b.mu.Unlock()

	switch b.mode {
	case ModeLive:
		return b.inner.Invoke(ctx, req)
	case ModeRecord:
		return b.recordInvoke(ctx, req, fp)
	case ModeReplay:
		return b.replayInvoke(ctx, req, fp)
	default:
		return belay.AgentResponse{}, fmt.Errorf("%w: %q", ErrInvalidMode, b.mode)
	}
}

// replayInvoke serves req entirely from b.cassette. It never touches
// b.inner — a ModeReplay Backend does not even hold one (see Replay) —
// so there is no way for a replay run to reach a subprocess or the
// network.
func (b *Backend) replayInvoke(ctx context.Context, req belay.AgentRequest, fp string) (belay.AgentResponse, error) {
	if err := ctx.Err(); err != nil {
		return belay.AgentResponse{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	list := b.index[fp]
	attempt := b.cursor[fp] + 1

	if b.cursor[fp] >= len(list) {
		b.logger().Error("replay: no recorded interaction",
			slog.String("fingerprint", fp), slog.Int("attempt", attempt), slog.Int("recorded", len(list)))
		return belay.AgentResponse{}, &MissingInteractionError{
			Fingerprint: fp,
			Request:     Normalize(req),
			Attempt:     attempt,
			Recorded:    len(list),
		}
	}

	it := list[b.cursor[fp]]
	b.cursor[fp]++

	b.logger().Debug("replay: served recorded interaction",
		slog.String("fingerprint", fp), slog.Int("attempt", attempt))

	resp := cloneResponse(it.Response)
	if it.Error != "" {
		return resp, &ReplayedError{Fingerprint: fp, Message: it.Error}
	}
	return resp, nil
}

// recordInvoke calls inner, appends a scrubbed record of the call to
// b.cassette, and returns inner's result to the caller UNMODIFIED: only
// what gets persisted to disk is scrubbed, never what a live-running
// dispatcher actually sees or acts on.
func (b *Backend) recordInvoke(ctx context.Context, req belay.AgentRequest, fp string) (belay.AgentResponse, error) {
	resp, err := b.inner.Invoke(ctx, req)

	it := Interaction{
		Fingerprint: fp,
		Request:     scrubRequest(Normalize(req)),
		Response:    scrubResponse(resp),
	}
	if err != nil {
		it.Error = Scrub(err.Error())
	}

	b.mu.Lock()
	b.cassette.Interactions = append(b.cassette.Interactions, it)
	b.mu.Unlock()

	b.logger().Debug("replay: recorded interaction", slog.String("fingerprint", fp))
	return resp, err
}

// Cassette returns a copy of the recording accumulated so far.
//
// The returned value's Interactions slice is independent of the
// Backend's own: appending to it, or calling Cassette again after more
// Invoke calls, never affects a previously returned value. Interaction
// elements are not additionally deep-cloned beyond that — a returned
// Interaction's own byte slices (Response.Raw, Request.MCPConfig) are
// shared with the Backend's copy, the same aliasing convention
// belay.AgentResponse uses elsewhere.
//
// Calling Cassette in ModeLive or ModeReplay returns Provenance without
// Interactions: ModeLive never accumulates any, and ModeReplay's own
// cassette is a read-only source this method does not mutate or copy the
// (potentially large) Interactions slice of.
func (b *Backend) Cassette() Cassette {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cassette == nil {
		return Cassette{FormatVersion: FormatVersion}
	}
	out := Cassette{FormatVersion: b.cassette.FormatVersion, Provenance: b.cassette.Provenance}
	if b.mode == ModeRecord {
		out.Interactions = append([]Interaction(nil), b.cassette.Interactions...)
	}
	return out
}

// Seen returns the Fingerprint of every request Invoke has computed so
// far, in call order, regardless of Mode. It is the input DetectDrift
// expects from a live or recording run being checked against an existing
// cassette.
func (b *Backend) Seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.seen...)
}

// cloneResponse returns a copy of resp whose Raw byte slice is
// independent of resp.Raw, so a caller mutating a replayed response's Raw
// in place cannot corrupt the cassette's own copy for a later replay of
// the same fingerprint.
func cloneResponse(resp belay.AgentResponse) belay.AgentResponse {
	out := resp
	if resp.Raw != nil {
		out.Raw = append(json.RawMessage(nil), resp.Raw...)
	}
	return out
}

// truncate returns s unchanged if it is at most n bytes, or its first n
// bytes followed by an ellipsis otherwise. It exists only to keep
// MissingInteractionError's message readable for a very long prompt.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ordinal renders n as "1st", "2nd", "3rd", "4th", ... for
// MissingInteractionError's message.
func ordinal(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return fmt.Sprintf("%dth", n)
	}
	switch n % 10 {
	case 1:
		return fmt.Sprintf("%dst", n)
	case 2:
		return fmt.Sprintf("%dnd", n)
	case 3:
		return fmt.Sprintf("%drd", n)
	default:
		return fmt.Sprintf("%dth", n)
	}
}

var _ belay.AgentBackend = (*Backend)(nil)
