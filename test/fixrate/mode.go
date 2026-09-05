//go:build fixrate

package fixrate

import (
	"errors"
	"fmt"
	"strings"
)

// Mode selects how Run drives belay's agent for a fix-rate measurement.
//
// This is the single most important knob in this package, because a
// fix-rate number's meaning depends entirely on it, and a mislabeled one
// is worse than none: it reads as a claim about the wrong thing.
type Mode string

const (
	// ModeReplay drives the graph with a zero-cost AgentBackend — the
	// built-in scripted repair/cheat/noop agent (see NewScriptedAgent),
	// or a pre-recorded internal/agent/replay cassette. No process this
	// package starts talks to the network or spends a token; Run also
	// verifies this after the fact by refusing to report a result whose
	// cumulative belay.Usage is non-zero (see Run).
	//
	// A ModeReplay result measures belay's dispatcher, graph routing, the
	// fix loop, the write node's verification, and the review gate — the
	// GRAPH — not any coding agent's actual capability. Every Result this
	// mode produces says so, in Result.Provenance and as the first line
	// of Result.HumanSummary, so it can never be quoted as an agent
	// capability number by accident.
	ModeReplay Mode = "replay"

	// ModeLive drives the graph with a real belay.AgentBackend the caller
	// constructs and supplies (RunOptions.Agent) — ordinarily one backed
	// by a real coding-agent CLI. It spends real money and measures real
	// agent capability.
	//
	// This package never constructs a live backend itself and imports no
	// agent-CLI adapter, so there is no code path inside this package
	// capable of starting one on its own; the caller alone decides to
	// spend. Run additionally refuses ModeLive unless RunOptions.Confirm
	// exactly equals ConfirmLiveSpend, a deliberate tripwire against a
	// stray default flipping a CI job from free to billed.
	ModeLive Mode = "live"
)

// ConfirmLiveSpend is the exact string RunOptions.Confirm must equal for
// Run to accept Mode: ModeLive. It exists so that switching to live mode
// requires a caller to type out, in code, that they mean it — a Mode
// field alone is one accidental default away from spending money.
const ConfirmLiveSpend = "I understand this spends real money"

// ErrUnknownMode reports a string that names neither ModeReplay nor
// ModeLive.
var ErrUnknownMode = errors.New("fixrate: unknown mode")

// String returns m's canonical spelling.
func (m Mode) String() string { return string(m) }

// Valid reports whether m is ModeReplay or ModeLive.
func (m Mode) Valid() bool { return m == ModeReplay || m == ModeLive }

// ParseMode parses s (case-insensitive, surrounding whitespace ignored)
// into a Mode, returning an error wrapping ErrUnknownMode for anything
// other than "replay" or "live". There is deliberately no default: an
// empty or unrecognized value must never silently resolve to either mode.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(ModeReplay):
		return ModeReplay, nil
	case string(ModeLive):
		return ModeLive, nil
	default:
		return "", fmt.Errorf("%w: %q (want %q or %q)", ErrUnknownMode, s, ModeReplay, ModeLive)
	}
}

// provenance returns the mandatory, human-readable disclosure of what a
// result produced under m actually measures. It is stored verbatim in
// Result.Provenance and repeated as the first line of HumanSummary.
func (m Mode) provenance() string {
	switch m {
	case ModeReplay:
		return "mode: replay — produced with NO real coding agent (a scripted, zero-cost stand-in " +
			"drove belay's graph). This measures belay's dispatcher/graph plumbing, not agent capability, " +
			"and cost nothing. It is NOT a measure of what a real coding agent can fix."
	case ModeLive:
		return "mode: live — produced by a real coding agent backend and reflects real spend " +
			"(see usage_usd). This measures agent capability, mediated by belay's graph."
	default:
		return "mode: " + string(m) + " — unrecognized; this result's provenance cannot be vouched for"
	}
}
