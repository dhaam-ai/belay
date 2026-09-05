package budget

import (
	"encoding/json"
	"math/big"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/pkg/belay"
)

func TestNew_IsEmpty(t *testing.T) {
	l := New()

	if diff := cmp.Diff(belay.Usage{}, l.Total()); diff != "" {
		t.Errorf("New().Total() mismatch (-want +got):\n%s", diff)
	}
	if got := l.Breakdown(); len(got) != 0 {
		t.Errorf("New().Breakdown() = %v, want empty", got)
	}
}

func TestLedger_Record_AccumulatesPerNodeAndTotal(t *testing.T) {
	l := New()

	l.Record("plan", belay.Usage{InputTokens: 100, OutputTokens: 50, USD: 0.10})
	l.Record("code", belay.Usage{InputTokens: 200, OutputTokens: 300, USD: 0.40})
	// A second call for "plan" (a retry) must accumulate, not overwrite.
	l.Record("plan", belay.Usage{InputTokens: 10, OutputTokens: 5, USD: 0.01})

	wantTotal := belay.Usage{InputTokens: 310, OutputTokens: 355, USD: 0.51}
	if diff := cmp.Diff(wantTotal, l.Total()); diff != "" {
		t.Errorf("Total() mismatch (-want +got):\n%s", diff)
	}

	wantBreakdown := []NodeUsage{
		{Node: "plan", Usage: belay.Usage{InputTokens: 110, OutputTokens: 55, USD: 0.11}},
		{Node: "code", Usage: belay.Usage{InputTokens: 200, OutputTokens: 300, USD: 0.40}},
	}
	if diff := cmp.Diff(wantBreakdown, l.Breakdown()); diff != "" {
		t.Errorf("Breakdown() mismatch (-want +got):\n%s", diff)
	}
}

func TestLedger_Breakdown_OrderIsFirstSeen(t *testing.T) {
	l := New()
	l.Record("review", belay.Usage{USD: 1})
	l.Record("plan", belay.Usage{USD: 1})
	l.Record("review", belay.Usage{USD: 1}) // second call, must not move "review"

	got := l.Breakdown()
	var order []string
	for _, nu := range got {
		order = append(order, nu.Node)
	}
	want := []string{"review", "plan"}
	if diff := cmp.Diff(want, order); diff != "" {
		t.Errorf("Breakdown() order mismatch (-want +got):\n%s", diff)
	}
}

// TestLedger_Estimated_StickyOnce proves requirement 3: one estimated
// contribution among many exact ones flags the total (and that node) as
// estimated. A user must never see a confident number built partly from a
// guess.
func TestLedger_Estimated_StickyOnce(t *testing.T) {
	l := New()

	for i := 0; i < 9; i++ {
		l.Record("code", belay.Usage{InputTokens: 100, USD: 0.05, Estimated: false})
	}
	l.Record("code", belay.Usage{InputTokens: 100, USD: 0.05, Estimated: true})

	total := l.Total()
	if !total.Estimated {
		t.Errorf("Total().Estimated = false, want true after one estimated contribution among ten")
	}

	breakdown := l.Breakdown()
	if len(breakdown) != 1 || !breakdown[0].Usage.Estimated {
		t.Errorf("Breakdown()[0].Usage.Estimated = %+v, want Estimated=true", breakdown)
	}
}

// TestLedger_Estimated_DoesNotLeakAcrossNodes checks the flip side: an
// estimated contribution to one node must not falsely mark an unrelated
// node's own totals as estimated. Only the run-wide Total is expected to
// be contaminated by any single estimate; each node's own figure reflects
// only its own contributions.
func TestLedger_Estimated_DoesNotLeakAcrossNodes(t *testing.T) {
	l := New()
	l.Record("plan", belay.Usage{USD: 1, Estimated: false})
	l.Record("review", belay.Usage{USD: 1, Estimated: true})

	for _, nu := range l.Breakdown() {
		want := nu.Node == "review"
		if nu.Usage.Estimated != want {
			t.Errorf("node %q Estimated = %v, want %v", nu.Node, nu.Usage.Estimated, want)
		}
	}
	if !l.Total().Estimated {
		t.Errorf("Total().Estimated = false, want true (one node was estimated)")
	}
}

// TestLedger_FloatDrift_BoundedOverManyAdditions proves the float
// discipline claim in ledger.go: accumulating USD as integer micro-dollars
// bounds the total rounding error to at most N * $0.0000005 over N
// additions, regardless of the input's decimal expansion. One seventh of a
// dollar (an infinitely repeating binary and decimal fraction) recorded
// 10,000 times is the adversarial case: it cannot be represented exactly
// in float64, in decimal, or in micro-dollars.
func TestLedger_FloatDrift_BoundedOverManyAdditions(t *testing.T) {
	const n = 10_000
	perCall := 1.0 / 7.0

	l := New()
	for i := 0; i < n; i++ {
		l.Record("fix", belay.Usage{USD: perCall})
	}

	got := l.Total().USD

	// Exact rational sum, computed independently of the ledger's own
	// arithmetic via math/big, then rounded to the nearest float64 for
	// comparison.
	exact := new(big.Rat).SetFrac64(n, 7)
	want, _ := exact.Float64()

	diff := got - want
	if diff < 0 {
		diff = -diff
	}

	// Theoretical bound: each of the n contributions can lose at most half
	// a micro-dollar to rounding, and micro-dollar summation is exact
	// (integer, associative) after that, so total error cannot exceed
	// n * 0.0000005 USD. Add a small slack for the float64 boundary
	// conversions on both sides of this comparison.
	bound := n*0.0000005 + 1e-9
	if diff > bound {
		t.Errorf("Total().USD = %.10f, want within %.10f of exact %.10f (diff %.10f)", got, bound, want, diff)
	}
	// The bound itself should be tiny in absolute terms — this is the
	// "bounded", not just "not obviously wrong", part of the proof.
	if bound > 0.01 {
		t.Fatalf("test bound %.10f is not tight enough to prove anything; fix the test", bound)
	}
	t.Logf("exact=%.10f ledger=%.10f diff=%.10f bound=%.10f", want, got, diff, bound)
}

// TestLedger_ConcurrentRecord_RaceCleanAndExact proves requirement 8: 100
// goroutines reporting usage concurrently produce an exact total (run this
// test with -race to additionally prove there is no data race).
func TestLedger_ConcurrentRecord_RaceCleanAndExact(t *testing.T) {
	const goroutines = 100
	const perGoroutine = 10
	const usdPerCall = 0.01

	l := New()
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				l.Record("fanout-candidate", belay.Usage{InputTokens: 1, OutputTokens: 2, USD: usdPerCall})
			}
		}()
	}
	wg.Wait()

	want := belay.Usage{
		InputTokens:  goroutines * perGoroutine,
		OutputTokens: 2 * goroutines * perGoroutine,
		USD:          float64(goroutines*perGoroutine) * usdPerCall,
	}
	if diff := cmp.Diff(want, l.Total()); diff != "" {
		t.Errorf("Total() after concurrent Record mismatch (-want +got):\n%s", diff)
	}
}

// TestSnapshot_JSONFieldNames locks in the manifest budget block's
// contract: internal/state serializes exactly these field names
// (limit_usd, spent_usd, tokens_in, tokens_out, estimated), and this
// package must never rename them without updating that agreement.
func TestSnapshot_JSONFieldNames(t *testing.T) {
	snap := Snapshot{
		LimitUSD:  5,
		SpentUSD:  1.23,
		TokensIn:  100,
		TokensOut: 200,
		Estimated: true,
	}

	got, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	want := `{"limit_usd":5,"spent_usd":1.23,"tokens_in":100,"tokens_out":200,"estimated":true}`
	if string(got) != want {
		t.Errorf("json.Marshal(Snapshot) = %s, want %s", got, want)
	}
}

// TestLedger_Snapshot_RoundTrip proves requirement 7's round-trip half: a
// Snapshot taken from a Ledger, persisted, and Restored produces an
// identical Snapshot back.
func TestLedger_Snapshot_RoundTrip(t *testing.T) {
	l := New()
	l.Record("plan", belay.Usage{InputTokens: 1000, OutputTokens: 500, USD: 0.75})
	l.Record("code", belay.Usage{InputTokens: 5000, OutputTokens: 4000, USD: 2.10, Estimated: true})

	want := l.Snapshot(5.00)

	// Round-trip through JSON too, since that is how it actually crosses
	// the manifest.json boundary in production.
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	restored := Restore(decoded)
	got := restored.Snapshot(5.00)

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Snapshot round trip mismatch (-want +got):\n%s", diff)
	}
}

// TestLedger_Restore_ContinuesAccumulating proves requirement 7's other
// half: a resumed run's ledger keeps adding to the spend it had before the
// crash, rather than resetting to zero. Resetting on resume is exactly the
// bug that turns a hard budget cap into a suggestion (docs/adr/0007).
func TestLedger_Restore_ContinuesAccumulating(t *testing.T) {
	before := New()
	before.Record("plan", belay.Usage{InputTokens: 100, OutputTokens: 100, USD: 1.00})
	before.Record("code", belay.Usage{InputTokens: 200, OutputTokens: 200, USD: 2.00})

	snap := before.Snapshot(10.00)
	if snap.SpentUSD != 3.00 {
		t.Fatalf("setup: snap.SpentUSD = %v, want 3.00", snap.SpentUSD)
	}

	resumed := Restore(snap)

	// A fresh Restore must not silently start at zero.
	if got := resumed.Total(); got.USD != 3.00 || got.InputTokens != 300 || got.OutputTokens != 300 {
		t.Fatalf("Restore(snap).Total() = %+v, want spend carried over from before the crash", got)
	}

	// Recording new usage after resume must ADD to the restored baseline.
	resumed.Record("test", belay.Usage{InputTokens: 50, OutputTokens: 50, USD: 0.50})

	want := belay.Usage{InputTokens: 350, OutputTokens: 350, USD: 3.50}
	if diff := cmp.Diff(want, resumed.Total()); diff != "" {
		t.Errorf("Total() after resume+Record mismatch (-want +got):\n%s", diff)
	}
}
