package replay

import "github.com/dhaam-ai/belay/pkg/belay"

// Scoped returns a view of b whose Fingerprints are qualified by scope,
// sharing b's recording.
//
// # What it solves
//
// Fingerprint deliberately excludes WorkDir, so N best-of-N candidates
// given one goal in N workspaces produce ONE fingerprint between them
// (see NormalizedRequest). Recording such a run puts every candidate's
// answers under that single key, and replaying it hands them back in
// recording order — so which candidate receives which answer is decided
// by whichever goroutine happens to ask first. A scope is the missing
// distinguishing field: give each candidate its own, and each one records
// and replays its own interactions regardless of the order they run in.
//
// The scope is the candidate's id — "cand-01" — and not its directory.
// That is the whole point: an id is chosen by belay and is the same on
// every machine, where a workspace path is assigned by the OS afresh on
// every run and is exactly what Fingerprint exists to keep out of the
// hash. A cassette recorded with scopes still replays on a stranger's CI
// runner.
//
// # A view, not a Backend
//
// The returned Backend shares b's recording: in ModeRecord every scope
// appends to the one cassette the run saves at the end, in ModeReplay
// every scope reads the one cassette that was loaded, and Seen on any of
// them reports every fingerprint any of them computed. Only the scope
// differs. Scoping is therefore safe under concurrency for the same
// reason Invoke is — one mutex, held by all of them.
//
// Scoped("") returns an UNSCOPED view: the empty scope is left out of the
// encoded request entirely, so it hashes to the digest that request
// always hashed to. Every cassette recorded before scopes existed stays
// valid, and so does every run belay walks today, none of which scopes
// anything.
//
// Scoping replaces rather than nests: b.Scoped("x").Scoped("y") is
// b.Scoped("y"). A qualifier built by accumulation would depend on which
// code path handed the view along, and a fingerprint that depends on that
// is not a fingerprint.
//
// Logger is copied as it stands at the call, not followed: set b.Logger
// before scoping, or set it on the view.
func (b *Backend) Scoped(scope string) *Backend {
	return &Backend{
		Logger: b.Logger,
		mode:   b.mode,
		inner:  b.inner,
		scope:  scope,
		shared: b.shared,
	}
}

// normalize projects req into the NormalizedRequest this Backend
// fingerprints and records: Normalize's projection, qualified by the
// Backend's own scope.
//
// An unscoped Backend leaves Scope empty, and an empty Scope is omitted
// from the encoding, so this is byte-for-byte Normalize for every caller
// that has no candidate to name.
func (b *Backend) normalize(req belay.AgentRequest) NormalizedRequest {
	n := Normalize(req)
	n.Scope = b.scope
	return n
}
