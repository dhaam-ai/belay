package budget

import (
	"errors"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestPartitionSplitsTheRemainingCeiling is the arithmetic a fanout reserves
// its candidates' envelopes with. It is done in whole micro-dollars, so the
// shares always sum to no more than what was left — the property a float64
// division loses exactly at the boundary that matters.
func TestPartitionSplitsTheRemainingCeiling(t *testing.T) {
	tests := []struct {
		name       string
		spent      Snapshot
		cfg        config.Budget
		n          int
		wantUSD    float64
		wantTokens int64
	}{
		{
			name:    "an even split of what is left",
			spent:   Snapshot{SpentUSD: 1.00},
			cfg:     config.Budget{MaxUSD: 10.00},
			n:       3,
			wantUSD: 3.00,
		},
		{
			name:    "a third of a dollar, with the remainder left unallocated",
			cfg:     config.Budget{MaxUSD: 1.00},
			n:       3,
			wantUSD: 0.333333,
		},
		{
			name:    "one candidate takes the whole remainder",
			spent:   Snapshot{SpentUSD: 0.25},
			cfg:     config.Budget{MaxUSD: 1.00},
			n:       1,
			wantUSD: 0.75,
		},
		{
			name:       "tokens are divided the same way, from what is left of both counts",
			spent:      Snapshot{TokensIn: 100, TokensOut: 100},
			cfg:        config.Budget{MaxUSD: 3.00, MaxTokens: 1000},
			n:          3,
			wantUSD:    1.00,
			wantTokens: 266,
		},
		{
			name:       "no token ceiling divides into no token ceiling",
			cfg:        config.Budget{MaxUSD: 3.00, MaxTokens: 0},
			n:          3,
			wantUSD:    1.00,
			wantTokens: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usd, tokens, err := Partition(tt.spent, tt.cfg, tt.n)
			if err != nil {
				t.Fatalf("Partition: %v", err)
			}
			if usd != tt.wantUSD {
				t.Errorf("usd = %v, want %v", usd, tt.wantUSD)
			}
			if tokens != tt.wantTokens {
				t.Errorf("tokens = %d, want %d", tokens, tt.wantTokens)
			}

			// The property the integer arithmetic exists for: N envelopes
			// never add up to more than the run had left.
			remaining := usdToMicros(tt.cfg.MaxUSD) - usdToMicros(tt.spent.SpentUSD)
			if total := usdToMicros(usd) * int64(tt.n); total > remaining {
				t.Errorf("%d envelopes of %v total %d micro-dollars, more than the %d left",
					tt.n, usd, total, remaining)
			}
			if tt.cfg.MaxTokens > 0 {
				remainingTokens := tt.cfg.MaxTokens - tt.spent.TokensIn - tt.spent.TokensOut
				if total := tokens * int64(tt.n); total > remainingTokens {
					t.Errorf("%d envelopes of %d tokens total %d, more than the %d left",
						tt.n, tokens, total, remainingTokens)
				}
			}
		})
	}
}

// TestPartitionRefusesWhatItCannotSplit. Each refusal has a different
// remedy, so each is a different error: a missing ceiling is a
// configuration to fix, no candidates is a caller's mistake, and an
// exhausted ceiling is money — a *belay.BudgetError, which is what lets the
// dispatcher turn a fanout's refusal into a pause a person can resume after
// raising budget.max_usd.
func TestPartitionRefusesWhatItCannotSplit(t *testing.T) {
	tests := []struct {
		name   string
		spent  Snapshot
		cfg    config.Budget
		n      int
		wantIs error
	}{
		{
			name:   "no ceiling to divide",
			cfg:    config.Budget{MaxUSD: 0},
			n:      3,
			wantIs: ErrNoCeiling,
		},
		{
			name:   "a negative ceiling",
			cfg:    config.Budget{MaxUSD: -1},
			n:      3,
			wantIs: ErrNoCeiling,
		},
		{
			name:   "no candidates",
			cfg:    config.Budget{MaxUSD: 10},
			n:      0,
			wantIs: ErrNoCandidates,
		},
		{
			name:   "the ceiling is already spent",
			spent:  Snapshot{SpentUSD: 10.00},
			cfg:    config.Budget{MaxUSD: 10.00},
			n:      3,
			wantIs: belay.ErrBudgetExceeded,
		},
		{
			name:   "what is left does not reach a micro-dollar each",
			spent:  Snapshot{SpentUSD: 9.999998},
			cfg:    config.Budget{MaxUSD: 10.00},
			n:      3,
			wantIs: belay.ErrBudgetExceeded,
		},
		{
			name:   "the token ceiling is exhausted even though the dollars are not",
			spent:  Snapshot{TokensIn: 999, TokensOut: 0},
			cfg:    config.Budget{MaxUSD: 10.00, MaxTokens: 1000},
			n:      3,
			wantIs: belay.ErrBudgetExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usd, tokens, err := Partition(tt.spent, tt.cfg, tt.n)
			if !errors.Is(err, tt.wantIs) {
				t.Fatalf("Partition error = %v, want one wrapping %v", err, tt.wantIs)
			}
			if usd != 0 || tokens != 0 {
				t.Errorf("a refusal returned an envelope of %v / %d tokens, want nothing", usd, tokens)
			}
		})
	}
}

// TestPartitionNeverHandsOutAnUncappedEnvelope is the case a naive floor
// division gets wrong and nobody notices until the bill arrives: a zero
// ceiling means "uncapped" to Ledger.Check, so a share that rounds down to
// zero would not be a tiny envelope but no envelope at all, N times over.
func TestPartitionNeverHandsOutAnUncappedEnvelope(t *testing.T) {
	usd, _, err := Partition(Snapshot{SpentUSD: 0.999999}, config.Budget{MaxUSD: 1.00}, 2)
	if !errors.Is(err, belay.ErrBudgetExceeded) {
		t.Fatalf("Partition error = %v, want one wrapping belay.ErrBudgetExceeded", err)
	}
	if usd != 0 {
		t.Errorf("usd = %v, want nothing", usd)
	}
}

// TestPartitionAtLeastNeverLowersAnEnvelope is what makes "raise max_usd and
// resume" work for a candidate that aborted on its own envelope. The
// envelope a candidate was promised when it was claimed is already partly
// spent, so taking it back is not something the run can do; raising it to
// the current fair share is, and it is a pure function of the live ceiling,
// the spend before the fanout and N — so two resumes of the same run
// converge on the same envelope, whatever the candidates did in between.
func TestPartitionAtLeastNeverLowersAnEnvelope(t *testing.T) {
	tests := []struct {
		name         string
		persistedUSD float64
		persistedTok int64
		spent        Snapshot
		cfg          config.Budget
		n            int
		wantUSD      float64
		wantTokens   int64
	}{
		{
			name:         "a raised ceiling raises a claimed envelope",
			persistedUSD: 1.00,
			spent:        Snapshot{SpentUSD: 0.10},
			cfg:          config.Budget{MaxUSD: 100.00},
			n:            3,
			wantUSD:      33.30,
		},
		{
			name:         "a lowered ceiling leaves it exactly where it was",
			persistedUSD: 1.00,
			spent:        Snapshot{SpentUSD: 0.10},
			cfg:          config.Budget{MaxUSD: 0.50},
			n:            3,
			wantUSD:      1.00,
		},
		{
			name:         "a ceiling already spent leaves it where it was",
			persistedUSD: 1.00,
			spent:        Snapshot{SpentUSD: 9.00},
			cfg:          config.Budget{MaxUSD: 3.00},
			n:            3,
			wantUSD:      1.00,
		},
		{
			name:         "tokens rise with the token ceiling",
			persistedUSD: 1.00,
			persistedTok: 1000,
			cfg:          config.Budget{MaxUSD: 3.00, MaxTokens: 90000},
			n:            3,
			wantUSD:      1.00,
			wantTokens:   30000,
		},
		{
			name:         "a token ceiling that has gone away leaves the candidate uncapped",
			persistedUSD: 1.00,
			persistedTok: 1000,
			cfg:          config.Budget{MaxUSD: 3.00, MaxTokens: 0},
			n:            3,
			wantUSD:      1.00,
			wantTokens:   0,
		},
		{
			name:         "an uncapped candidate is not capped by a new token ceiling",
			persistedUSD: 1.00,
			persistedTok: 0,
			cfg:          config.Budget{MaxUSD: 3.00, MaxTokens: 90000},
			n:            3,
			wantUSD:      1.00,
			wantTokens:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usd, tokens := PartitionAtLeast(tt.persistedUSD, tt.persistedTok, tt.spent, tt.cfg, tt.n)
			if usd != tt.wantUSD {
				t.Errorf("usd = %v, want %v", usd, tt.wantUSD)
			}
			if tokens != tt.wantTokens {
				t.Errorf("tokens = %d, want %d", tokens, tt.wantTokens)
			}
		})
	}
}

// TestPartitionAtLeastIsDeterministic: an envelope recomputed on a second
// resume, with the same configuration and the same pre-fanout spend, must be
// the same number. Anything else would make a resumed run abort a different
// candidate than the crashed one did.
func TestPartitionAtLeastIsDeterministic(t *testing.T) {
	spent := Snapshot{SpentUSD: 0.10, TokensIn: 500, TokensOut: 500}
	cfg := config.Budget{MaxUSD: 100.00, MaxTokens: 900000}

	first, firstTokens := PartitionAtLeast(1.00, 1000, spent, cfg, 3)
	second, secondTokens := PartitionAtLeast(first, firstTokens, spent, cfg, 3)

	if first != second || firstTokens != secondTokens {
		t.Errorf("a second adoption moved the envelope from %v/%d to %v/%d",
			first, firstTokens, second, secondTokens)
	}
}
