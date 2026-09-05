package budget

import (
	"errors"
	"testing"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/pkg/belay"
)

// spentLedger builds a Ledger whose Total() reports the given usage, via a
// single Record call, for use as table-test fixtures below.
func spentLedger(u belay.Usage) *Ledger {
	l := New()
	l.Record("fix", u)
	return l
}

// TestLedger_Check_AbortVsWarn is the table test requirement 2 asks for:
// both branches of config.Budget.OnExceed, exercised against both the USD
// and the token ceiling.
func TestLedger_Check_AbortVsWarn(t *testing.T) {
	tests := []struct {
		name    string
		ledger  *Ledger
		cfg     config.Budget
		wantErr bool
	}{
		{
			name:    "under both ceilings, abort policy",
			ledger:  spentLedger(belay.Usage{InputTokens: 10, OutputTokens: 10, USD: 1.00}),
			cfg:     config.Budget{MaxUSD: 5.00, MaxTokens: 1000, OnExceed: config.OnExceedAbort},
			wantErr: false,
		},
		{
			name:    "over USD ceiling, abort policy stops the run",
			ledger:  spentLedger(belay.Usage{USD: 5.01}),
			cfg:     config.Budget{MaxUSD: 5.00, OnExceed: config.OnExceedAbort},
			wantErr: true,
		},
		{
			name:    "over USD ceiling, warn policy continues",
			ledger:  spentLedger(belay.Usage{USD: 5.01}),
			cfg:     config.Budget{MaxUSD: 5.00, OnExceed: config.OnExceedWarn},
			wantErr: false,
		},
		{
			name:    "over token ceiling, abort policy stops the run",
			ledger:  spentLedger(belay.Usage{InputTokens: 600, OutputTokens: 500}),
			cfg:     config.Budget{MaxTokens: 1000, OnExceed: config.OnExceedAbort},
			wantErr: true,
		},
		{
			name:    "over token ceiling, warn policy continues",
			ledger:  spentLedger(belay.Usage{InputTokens: 600, OutputTokens: 500}),
			cfg:     config.Budget{MaxTokens: 1000, OnExceed: config.OnExceedWarn},
			wantErr: false,
		},
		{
			name:    "exactly at USD ceiling is not over",
			ledger:  spentLedger(belay.Usage{USD: 5.00}),
			cfg:     config.Budget{MaxUSD: 5.00, OnExceed: config.OnExceedAbort},
			wantErr: false,
		},
		{
			name:    "zero MaxUSD means uncapped on that dimension",
			ledger:  spentLedger(belay.Usage{USD: 1_000_000}),
			cfg:     config.Budget{MaxUSD: 0, MaxTokens: 0, OnExceed: config.OnExceedWarn},
			wantErr: false,
		},
		{
			name:    "unrecognized OnExceed fails safe as abort",
			ledger:  spentLedger(belay.Usage{USD: 5.01}),
			cfg:     config.Budget{MaxUSD: 5.00, OnExceed: config.OnExceed("bogus")},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ledger.Check(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Check() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestLedger_Check_AbortErrorShape proves the abort path's contract
// exactly: errors.Is against belay.ErrBudgetExceeded, errors.As against
// *belay.BudgetError, and that BudgetError's fields are populated with the
// real spend and limit rather than left zero.
func TestLedger_Check_AbortErrorShape(t *testing.T) {
	l := spentLedger(belay.Usage{USD: 7.50})
	cfg := config.Budget{MaxUSD: 5.00, OnExceed: config.OnExceedAbort}

	err := l.Check(cfg)
	if err == nil {
		t.Fatalf("Check() = nil, want a budget error")
	}
	if !errors.Is(err, belay.ErrBudgetExceeded) {
		t.Errorf("errors.Is(err, belay.ErrBudgetExceeded) = false, want true (err: %v)", err)
	}

	var budgetErr *belay.BudgetError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("errors.As(err, &budgetErr) = false, want true (err: %v)", err)
	}
	if budgetErr.SpentUSD != 7.50 {
		t.Errorf("budgetErr.SpentUSD = %v, want 7.50", budgetErr.SpentUSD)
	}
	if budgetErr.LimitUSD != 5.00 {
		t.Errorf("budgetErr.LimitUSD = %v, want 5.00", budgetErr.LimitUSD)
	}
}

// TestLedger_CanAfford_Boundary is the boundary table test requirement 6
// asks for: a projection that lands exactly on the remaining budget must
// be affordable, and one that lands a single cent over must not be.
func TestLedger_CanAfford_Boundary(t *testing.T) {
	tests := []struct {
		name      string
		spent     belay.Usage
		n         int
		projected belay.Usage
		cfg       config.Budget
		wantErr   bool
	}{
		{
			name:      "exactly affordable at the USD boundary",
			spent:     belay.Usage{USD: 0},
			n:         5,
			projected: belay.Usage{USD: 1.00},
			cfg:       config.Budget{MaxUSD: 5.00},
			wantErr:   false,
		},
		{
			name:      "one cent over the USD boundary",
			spent:     belay.Usage{USD: 0},
			n:         5,
			projected: belay.Usage{USD: 1.002}, // 5 * 1.002 = 5.01
			cfg:       config.Budget{MaxUSD: 5.00},
			wantErr:   true,
		},
		{
			name:      "existing spend plus projection exactly affordable",
			spent:     belay.Usage{USD: 2.50},
			n:         5,
			projected: belay.Usage{USD: 0.50},
			cfg:       config.Budget{MaxUSD: 5.00},
			wantErr:   false,
		},
		{
			name:      "existing spend plus projection one cent over",
			spent:     belay.Usage{USD: 2.51},
			n:         5,
			projected: belay.Usage{USD: 0.50},
			cfg:       config.Budget{MaxUSD: 5.00},
			wantErr:   true,
		},
		{
			name:      "exactly affordable at the token boundary",
			spent:     belay.Usage{},
			n:         2,
			projected: belay.Usage{InputTokens: 250, OutputTokens: 250},
			cfg:       config.Budget{MaxTokens: 1000},
			wantErr:   false,
		},
		{
			name:      "one token over the token boundary",
			spent:     belay.Usage{},
			n:         2,
			projected: belay.Usage{InputTokens: 250, OutputTokens: 251},
			cfg:       config.Budget{MaxTokens: 1000},
			wantErr:   true,
		},
		{
			name:      "n <= 0 always affordable",
			spent:     belay.Usage{USD: 100},
			n:         0,
			projected: belay.Usage{USD: 1000},
			cfg:       config.Budget{MaxUSD: 1},
			wantErr:   false,
		},
		{
			name:      "uncapped dimensions never refuse",
			spent:     belay.Usage{USD: 1_000_000},
			n:         100,
			projected: belay.Usage{USD: 1_000_000, InputTokens: 1_000_000},
			cfg:       config.Budget{},
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := spentLedger(tt.spent)
			err := l.CanAfford(tt.n, tt.projected, tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CanAfford() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, belay.ErrBudgetExceeded) {
				t.Errorf("errors.Is(err, belay.ErrBudgetExceeded) = false, want true (err: %v)", err)
			}
		})
	}
}
