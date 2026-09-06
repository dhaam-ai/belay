package belay_test

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/dhaam-ai/belay/pkg/belay"
)

func TestSentinelErrorsAreDistinguishable(t *testing.T) {
	t.Parallel()

	sentinels := map[string]error{
		"ErrToolchainMissing":  belay.ErrToolchainMissing,
		"ErrBudgetExceeded":    belay.ErrBudgetExceeded,
		"ErrGateFailed":        belay.ErrGateFailed,
		"ErrUnsupported":       belay.ErrUnsupported,
		"ErrUnknownSeverity":   belay.ErrUnknownSeverity,
		"ErrUnknownGateStatus": belay.ErrUnknownGateStatus,
	}

	for name, sentinel := range sentinels {
		wrapped := fmt.Errorf("adapter x: %w", sentinel)
		if !errors.Is(wrapped, sentinel) {
			t.Errorf("errors.Is(fmt.Errorf(%%w, %s), %s) = false, want true", name, name)
		}
		for otherName, other := range sentinels {
			if otherName == name {
				continue
			}
			if errors.Is(wrapped, other) {
				t.Errorf("errors.Is(wrapped %s, %s) = true, want false — sentinels must not be confused with each other", name, otherName)
			}
		}
	}
}

func TestToolchainErrorWrapsSentinel(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err *belay.ToolchainError
	}{
		"no underlying cause": {
			err: &belay.ToolchainError{Tool: "golangci-lint"},
		},
		"with underlying lookup failure": {
			err: &belay.ToolchainError{Tool: "pytest", Err: &exec.Error{Name: "pytest", Err: exec.ErrNotFound}},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var err error = tt.err

			if !errors.Is(err, belay.ErrToolchainMissing) {
				t.Errorf("errors.Is(err, ErrToolchainMissing) = false, want true")
			}

			var got *belay.ToolchainError
			if !errors.As(err, &got) {
				t.Fatalf("errors.As(err, &*ToolchainError) = false, want true")
			}
			if got.Tool != tt.err.Tool {
				t.Errorf("recovered Tool = %q, want %q", got.Tool, tt.err.Tool)
			}

			if tt.err.Err != nil && !errors.Is(err, exec.ErrNotFound) {
				t.Errorf("errors.Is(err, exec.ErrNotFound) = false, want true when Err wraps it")
			}

			if got := err.Error(); got == "" {
				t.Error("Error() returned an empty string")
			}
		})
	}
}

func TestToolchainErrorViaFmtErrorfAlsoSatisfiesErrorsIs(t *testing.T) {
	t.Parallel()

	// The doc comment on ToolchainError promises that an adapter that
	// chooses not to use the type at all still satisfies errors.Is by
	// wrapping the sentinel directly.
	err := fmt.Errorf("%w: golangci-lint", belay.ErrToolchainMissing)
	if !errors.Is(err, belay.ErrToolchainMissing) {
		t.Error("errors.Is(err, ErrToolchainMissing) = false, want true for a plain fmt.Errorf wrap")
	}
}

func TestBudgetErrorWrapsSentinel(t *testing.T) {
	t.Parallel()

	var err error = &belay.BudgetError{SpentUSD: 12.5, LimitUSD: 10}

	if !errors.Is(err, belay.ErrBudgetExceeded) {
		t.Error("errors.Is(err, ErrBudgetExceeded) = false, want true")
	}

	var got *belay.BudgetError
	if !errors.As(err, &got) {
		t.Fatal("errors.As(err, &*BudgetError) = false, want true")
	}
	if got.SpentUSD != 12.5 || got.LimitUSD != 10 {
		t.Errorf("recovered BudgetError = %+v, want SpentUSD=12.5 LimitUSD=10", got)
	}
	if msg := err.Error(); msg == "" {
		t.Error("Error() returned an empty string")
	}
}
