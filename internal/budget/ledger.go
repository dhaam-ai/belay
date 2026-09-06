package budget

import (
	"math"
	"sync"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// microsPerUSD is the scale factor between a US dollar and the integer
// unit this package accumulates money in: one micro-dollar, 1/1,000,000
// of a dollar (equivalently, one hundredth of a cent).
//
// # Float discipline
//
// A Ledger never accumulates belay.Usage.USD directly as float64. Each
// Record call converts its USD contribution to the nearest whole
// micro-dollar exactly once (usdToMicros), then adds that int64 to a
// running int64 total. Two things follow from that:
//
//   - Every individual conversion loses at most half a micro-dollar
//     ($0.0000005) to rounding — far below anything a human budgeting in
//     cents or dollars would ever notice.
//   - That rounding happens exactly once per contribution, never again on
//     the way in or the way out: int64 addition is exact and associative,
//     so the accumulated error after N contributions is bounded by
//     N * $0.0000005 and does not compound the way repeated float64 +=
//     does, where each addition's own rounding error can itself be
//     amplified by every addition that follows it. See
//     TestLedger_FloatDrift_BoundedOverManyAdditions for the proof: after
//     10,000 additions of a value with an infinite binary and decimal
//     expansion (one seventh of a dollar), the ledger's total lands within
//     a cent of the exact rational sum — nowhere near the unbounded drift
//     a naive running float64 total risks over that many additions.
//   - Because int64 addition is associative and commutative, the result
//     of many goroutines calling Record concurrently (serialized by
//     Ledger.mu, but in whatever order the scheduler happens to run them)
//     does not depend on that order. A float64 accumulator has no such
//     guarantee: two runs with identical inputs recorded in a different
//     goroutine-scheduling order could legitimately total to two
//     different floats.
//
// USD is converted back to float64 (microsToUSD) only at the boundary,
// when a caller asks for a Total, a Breakdown, or a Snapshot to persist —
// the "accumulate as integer micro-USD, convert at the boundary" choice
// task T18 offered as an alternative to proving float64 accumulation safe.
const microsPerUSD = 1_000_000

// usdToMicros converts a dollar amount to the nearest integer number of
// micro-dollars, rounding half away from zero. Non-finite input (NaN or
// +/-Inf — never produced by a well-behaved belay.Usage, but not something
// this package trusts blindly) converts to zero rather than an
// implementation-defined int64 conversion result.
func usdToMicros(usd float64) int64 {
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0
	}
	return int64(math.Round(usd * microsPerUSD))
}

// microsToUSD converts an integer number of micro-dollars back to a
// dollar float64, at the boundary where a caller needs one.
func microsToUSD(micros int64) float64 {
	return float64(micros) / microsPerUSD
}

// totals is the mutable accumulator behind one Ledger node entry, or a
// Ledger's grand total. Money is held in micro-dollars throughout; usage
// converts back to the public belay.Usage shape only when read.
type totals struct {
	inputTokens  int64
	outputTokens int64
	micros       int64
	estimated    bool
}

// add folds one belay.Usage contribution into t.
func (t *totals) add(u belay.Usage) {
	t.inputTokens += u.InputTokens
	t.outputTokens += u.OutputTokens
	t.micros += usdToMicros(u.USD)
	t.estimated = t.estimated || u.Estimated
}

// usage renders t as the public belay.Usage shape.
func (t totals) usage() belay.Usage {
	return belay.Usage{
		InputTokens:  t.inputTokens,
		OutputTokens: t.outputTokens,
		USD:          microsToUSD(t.micros),
		Estimated:    t.estimated,
	}
}

// Ledger accumulates belay.Usage across a run: one running total per node
// plus a grand total, safe for concurrent use so that fanout candidates —
// which invoke an AgentBackend concurrently — can each report their own
// usage without racing.
//
// The zero value is not usable; construct one with New for a fresh run, or
// Restore when continuing a run that already has a persisted Snapshot.
type Ledger struct {
	mu    sync.Mutex
	nodes map[string]*totals
	order []string // node names in first-seen order, for a stable Breakdown
	total totals
}

// New returns an empty Ledger with no recorded usage, for a fresh run.
//
// Call Restore instead when resuming a run that already has a persisted
// budget Snapshot: New always starts a run's spend at zero, which for a
// resumed run would silently discard everything spent before the crash
// and let the run spend up to the full ceiling all over again.
func New() *Ledger {
	return &Ledger{nodes: make(map[string]*totals)}
}

// Record adds one AgentBackend.Invoke call's usage to node's running total
// and to the ledger's grand total. It is safe to call concurrently,
// including multiple times for the same node — a retried node accumulates
// rather than overwriting — and from different goroutines reporting
// different fanout candidates.
//
// The caller supplies a fully-priced belay.Usage. Pricing deliberately lives
// with the agent backend, which is the only layer that knows which model
// actually ran and how that provider reports cost; a price table here would
// be a second source of truth that drifts silently out of agreement.
func (l *Ledger) Record(node string, u belay.Usage) {
	l.mu.Lock()
	defer l.mu.Unlock()

	t, ok := l.nodes[node]
	if !ok {
		t = &totals{}
		l.nodes[node] = t
		l.order = append(l.order, node)
	}
	t.add(u)
	l.total.add(u)
}

// Total returns the ledger's cumulative usage across every node recorded
// so far.
func (l *Ledger) Total() belay.Usage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total.usage()
}

// NodeUsage pairs a node name with its accumulated belay.Usage, as
// returned by Breakdown.
type NodeUsage struct {
	// Node is the graph node name usage was recorded under, such as
	// "code" or "review".
	Node string
	// Usage is that node's cumulative usage across every Record call made
	// for it — more than one invocation's worth, for a node that was
	// retried.
	Usage belay.Usage
}

// Breakdown returns the ledger's per-node usage in the order each node
// first recorded usage, for rendering — for example by `belay timeline` —
// a cost breakdown alongside the run's total.
func (l *Ledger) Breakdown() []NodeUsage {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]NodeUsage, 0, len(l.order))
	for _, name := range l.order {
		out = append(out, NodeUsage{Node: name, Usage: l.nodes[name].usage()})
	}
	return out
}

// Snapshot is the serializable form of a Ledger's cumulative total: the
// exact shape and field names the run manifest's budget block persists.
// internal/state owns manifest.json's actual encoding; this package
// guarantees only this struct and these json tags, so the two sides agree
// by a documented contract rather than by one importing the other.
type Snapshot struct {
	// LimitUSD is the run's configured budget ceiling (config.Budget.MaxUSD)
	// at the time of the snapshot, carried alongside spend so the manifest
	// records both halves of "how close is this run to its cap" together.
	LimitUSD float64 `json:"limit_usd"`
	// SpentUSD is the ledger's cumulative USD total.
	SpentUSD float64 `json:"spent_usd"`
	// TokensIn is the ledger's cumulative input token total.
	TokensIn int64 `json:"tokens_in"`
	// TokensOut is the ledger's cumulative output token total.
	TokensOut int64 `json:"tokens_out"`
	// Estimated reports whether any contribution to SpentUSD was estimated
	// rather than reported directly by an agent backend — see
	// belay.Usage.Estimated.
	Estimated bool `json:"estimated"`
}

// Snapshot returns the ledger's current cumulative total as a Snapshot
// ready to persist into the manifest's budget block, paired with limitUSD
// — typically the run's config.Budget.MaxUSD — since the manifest records
// the ceiling and the spend together.
func (l *Ledger) Snapshot(limitUSD float64) Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.total.usage()
	return Snapshot{
		LimitUSD:  limitUSD,
		SpentUSD:  u.USD,
		TokensIn:  u.InputTokens,
		TokensOut: u.OutputTokens,
		Estimated: u.Estimated,
	}
}

// Restore rebuilds a Ledger's grand total from a Snapshot read back from
// the manifest, so a resumed run keeps accumulating spend from where a
// crash left off instead of starting over at zero. That is the entire
// point of persisting a Snapshot in the first place: a crash-resume that
// resets the ledger turns a hard budget cap into a suggestion, because
// every crash would hand a runaway run a fresh ceiling to blow through
// again.
//
// Restore recovers only the aggregate total — LimitUSD, SpentUSD,
// TokensIn/Out, Estimated — because that is all the manifest's budget
// block persists (see Snapshot). It cannot recover the original per-node
// Breakdown: the returned Ledger's Breakdown starts empty and grows only
// with nodes Recorded after the resume, while its Total and Check still
// correctly reflect all spend, from both before and after the crash.
func Restore(snap Snapshot) *Ledger {
	l := New()
	l.total = totals{
		inputTokens:  snap.TokensIn,
		outputTokens: snap.TokensOut,
		micros:       usdToMicros(snap.SpentUSD),
		estimated:    snap.Estimated,
	}
	return l
}
