package fanout_test

import (
	"errors"
	"testing"

	"github.com/dhaam-ai/belay/internal/budget"
	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/nodes/fanout"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/google/go-cmp/cmp"
)

// A candidate's envelope is the share of what the run may still spend that
// it is allowed to spend. The arithmetic belongs to internal/budget; what
// this node owns is turning it into the type a candidate manifest persists,
// and refusing rather than handing out a share that means "uncapped".
func TestEnvelopes(t *testing.T) {
	tests := []struct {
		name  string
		spent budget.Snapshot
		cfg   config.Budget
		n     int
		want  state.Envelope
	}{
		{
			name:  "an even split of what is left",
			spent: budget.Snapshot{SpentUSD: 1.00},
			cfg:   config.Budget{MaxUSD: 10.00},
			n:     3,
			want:  state.Envelope{LimitUSD: 3.00},
		},
		{
			name: "no token ceiling leaves the token half uncapped",
			cfg:  config.Budget{MaxUSD: 3.00},
			n:    3,
			want: state.Envelope{LimitUSD: 1.00},
		},
		{
			name:  "both dimensions are capped when both have a ceiling",
			spent: budget.Snapshot{TokensIn: 100, TokensOut: 100},
			cfg:   config.Budget{MaxUSD: 3.00, MaxTokens: 1000},
			n:     3,
			want:  state.Envelope{LimitUSD: 1.00, LimitTokens: 266},
		},
		{
			name: "the remainder of fewer than n micro-dollars is left unallocated",
			cfg:  config.Budget{MaxUSD: 1.00},
			n:    3,
			want: state.Envelope{LimitUSD: 0.333333},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fanout.Envelopes(tt.spent, tt.cfg, tt.n)
			if err != nil {
				t.Fatalf("Envelopes() error = %v, want nil", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Envelopes() mismatch (-want +got):\n%s", diff)
			}
			// The property the whole reservation rests on: N candidates
			// together may not be promised more than the run had left.
			if total := got.LimitUSD * float64(tt.n); total > tt.cfg.MaxUSD-tt.spent.SpentUSD+1e-9 {
				t.Errorf("%d envelopes of $%v total $%v, more than the $%v left",
					tt.n, got.LimitUSD, total, tt.cfg.MaxUSD-tt.spent.SpentUSD)
			}
		})
	}
}

// Every refusal is the ledger's own, so that the dispatcher can turn a
// fanout that cannot afford its candidates into a pause a person resumes
// after raising budget.max_usd.
func TestEnvelopesRefusals(t *testing.T) {
	tests := []struct {
		name  string
		spent budget.Snapshot
		cfg   config.Budget
		n     int
		want  error
	}{
		{
			name: "no ceiling to divide is a wiring bug, because an uncapped envelope is an uncapped run",
			cfg:  config.Budget{MaxUSD: 0},
			n:    3,
			want: budget.ErrNoCeiling,
		},
		{
			name: "no candidates to divide among",
			cfg:  config.Budget{MaxUSD: 10.00},
			n:    0,
			want: budget.ErrNoCandidates,
		},
		{
			name:  "a ceiling already reached is a budget refusal, not an arithmetic one",
			spent: budget.Snapshot{SpentUSD: 10.00},
			cfg:   config.Budget{MaxUSD: 10.00},
			n:     3,
			want:  belay.ErrBudgetExceeded,
		},
		{
			name:  "a share that rounds down to zero is refused, because zero reads as uncapped",
			spent: budget.Snapshot{SpentUSD: 9.999999},
			cfg:   config.Budget{MaxUSD: 10.00},
			n:     3,
			want:  belay.ErrBudgetExceeded,
		},
		{
			name:  "the token ceiling refuses on its own",
			spent: budget.Snapshot{TokensIn: 900, TokensOut: 100},
			cfg:   config.Budget{MaxUSD: 10.00, MaxTokens: 1000},
			n:     3,
			want:  belay.ErrBudgetExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fanout.Envelopes(tt.spent, tt.cfg, tt.n)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Envelopes() error = %v, want it to wrap %v", err, tt.want)
			}
			if got != (state.Envelope{}) {
				t.Errorf("Envelopes() = %+v on a refusal, want the zero envelope", got)
			}
		})
	}
}
