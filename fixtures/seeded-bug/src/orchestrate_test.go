package backoff

import (
	"context"
	"errors"
	"testing"
	"time"
)

func fastPolicy(maxAttempts int) Policy {
	return Policy{
		BaseDelay:   time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
		Multiplier:  2,
		MaxAttempts: maxAttempts,
		Jitter:      0,
	}
}

func noopSleep(context.Context, time.Duration) error { return nil }

func TestRun(t *testing.T) {
	t.Run("succeeds_on_first_try", func(t *testing.T) {
		l := NewLedger()
		calls := 0
		op := func(context.Context) error {
			calls++
			return nil
		}

		err := Run(context.Background(), "op", fastPolicy(5), l, seededRand(1), noopSleep, op)
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		if calls != 1 {
			t.Fatalf("op was called %d times, want 1", calls)
		}
		if got := l.Count("op"); got != 1 {
			t.Fatalf("ledger Count(\"op\") = %d, want 1", got)
		}
	})

	t.Run("retries_until_success", func(t *testing.T) {
		l := NewLedger()
		calls := 0
		op := func(context.Context) error {
			calls++
			if calls < 3 {
				return ErrTransient
			}
			return nil
		}

		err := Run(context.Background(), "op", fastPolicy(5), l, seededRand(1), noopSleep, op)
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		if calls != 3 {
			t.Fatalf("op was called %d times, want 3", calls)
		}
		if got := l.Count("op"); got != 3 {
			t.Fatalf("ledger Count(\"op\") = %d, want 3 (every attempt recorded)", got)
		}
	})

	t.Run("exhausts_max_attempts_and_returns_the_last_error", func(t *testing.T) {
		l := NewLedger()
		calls := 0
		op := func(context.Context) error {
			calls++
			return ErrTransient
		}

		err := Run(context.Background(), "op", fastPolicy(3), l, seededRand(1), noopSleep, op)
		if err == nil {
			t.Fatal("Run() error = nil, want a wrapped ErrTransient")
		}
		if !errors.Is(err, ErrTransient) {
			t.Fatalf("Run() error = %v, want it to wrap ErrTransient", err)
		}
		if calls != 3 {
			t.Fatalf("op was called %d times, want exactly MaxAttempts (3)", calls)
		}
	})

	t.Run("non_retryable_error_stops_immediately", func(t *testing.T) {
		calls := 0
		permanent := errors.New("permission denied")
		op := func(context.Context) error {
			calls++
			return permanent
		}

		err := Run(context.Background(), "op", fastPolicy(5), NewLedger(), seededRand(1), noopSleep, op)
		if !errors.Is(err, permanent) {
			t.Fatalf("Run() error = %v, want it to wrap the permanent error", err)
		}
		if calls != 1 {
			t.Fatalf("op was called %d times, want 1 (non-retryable error should not be retried)", calls)
		}
	})

	t.Run("nil_ledger_is_accepted", func(t *testing.T) {
		calls := 0
		op := func(context.Context) error {
			calls++
			if calls < 2 {
				return ErrTransient
			}
			return nil
		}

		err := Run(context.Background(), "op", fastPolicy(5), nil, seededRand(1), noopSleep, op)
		if err != nil {
			t.Fatalf("Run() with a nil ledger error = %v, want nil", err)
		}
		if calls != 2 {
			t.Fatalf("op was called %d times, want 2", calls)
		}
	})

	t.Run("sleep_error_stops_retrying_immediately", func(t *testing.T) {
		errSleepFailed := errors.New("sleep failed")
		calls := 0
		op := func(context.Context) error {
			calls++
			return ErrTransient
		}
		sleep := func(context.Context, time.Duration) error {
			return errSleepFailed
		}

		err := Run(context.Background(), "op", fastPolicy(5), NewLedger(), seededRand(1), sleep, op)
		if !errors.Is(err, errSleepFailed) {
			t.Fatalf("Run() error = %v, want errSleepFailed", err)
		}
		if calls != 1 {
			t.Fatalf("op was called %d times, want 1 (should stop as soon as sleep fails)", calls)
		}
	})
}
