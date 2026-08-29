package backoff

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestPolicy_Validate(t *testing.T) {
	valid := Policy{
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		Multiplier:  2.0,
		MaxAttempts: 5,
		MaxElapsed:  0,
		Jitter:      0.2,
	}

	cases := []struct {
		name    string
		mutate  func(Policy) Policy
		wantErr bool
	}{
		{"valid_default", func(p Policy) Policy { return p }, false},
		{"base_delay_zero_is_invalid", func(p Policy) Policy { p.BaseDelay = 0; return p }, true},
		{"base_delay_negative_is_invalid", func(p Policy) Policy { p.BaseDelay = -1; return p }, true},
		{"max_delay_less_than_base_delay_is_invalid", func(p Policy) Policy { p.MaxDelay = p.BaseDelay - 1; return p }, true},
		{"max_delay_equal_to_base_delay_is_valid", func(p Policy) Policy { p.MaxDelay = p.BaseDelay; return p }, false},
		{"multiplier_one_is_invalid", func(p Policy) Policy { p.Multiplier = 1; return p }, true},
		{"multiplier_less_than_one_is_invalid", func(p Policy) Policy { p.Multiplier = 0.5; return p }, true},
		{"max_attempts_zero_is_invalid", func(p Policy) Policy { p.MaxAttempts = 0; return p }, true},
		{"max_attempts_negative_is_invalid", func(p Policy) Policy { p.MaxAttempts = -1; return p }, true},
		{"max_elapsed_negative_is_invalid", func(p Policy) Policy { p.MaxElapsed = -1; return p }, true},
		{"max_elapsed_zero_is_valid", func(p Policy) Policy { p.MaxElapsed = 0; return p }, false},
		{"jitter_below_zero_is_invalid", func(p Policy) Policy { p.Jitter = -0.01; return p }, true},
		{"jitter_above_one_is_invalid", func(p Policy) Policy { p.Jitter = 1.01; return p }, true},
		{"jitter_zero_is_valid", func(p Policy) Policy { p.Jitter = 0; return p }, false},
		{"jitter_one_is_valid", func(p Policy) Policy { p.Jitter = 1; return p }, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mutate(valid).Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want an error wrapping ErrInvalidPolicy")
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidPolicy) {
				t.Fatalf("Validate() = %v, want it to wrap ErrInvalidPolicy", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestPolicy_NextDelay(t *testing.T) {
	base := 100 * time.Millisecond
	maxDelay := 30 * time.Second
	mult := 2.0

	t.Run("attempt_zero_returns_error", func(t *testing.T) {
		p := Policy{BaseDelay: base, MaxDelay: maxDelay, Multiplier: mult, MaxAttempts: 5, Jitter: 0.2}
		if _, err := p.NextDelay(0, seededRand(1)); !errors.Is(err, ErrInvalidAttempt) {
			t.Fatalf("NextDelay(0, ...) error = %v, want ErrInvalidAttempt", err)
		}
	})

	t.Run("negative_attempt_returns_error", func(t *testing.T) {
		p := Policy{BaseDelay: base, MaxDelay: maxDelay, Multiplier: mult, MaxAttempts: 5, Jitter: 0.2}
		if _, err := p.NextDelay(-3, seededRand(1)); !errors.Is(err, ErrInvalidAttempt) {
			t.Fatalf("NextDelay(-3, ...) error = %v, want ErrInvalidAttempt", err)
		}
	})

	t.Run("first_attempt_without_jitter_equals_base_delay", func(t *testing.T) {
		p := Policy{BaseDelay: base, MaxDelay: maxDelay, Multiplier: mult, MaxAttempts: 5, Jitter: 0}
		got, err := p.NextDelay(1, seededRand(1))
		if err != nil {
			t.Fatalf("NextDelay(1, ...) error = %v", err)
		}
		if got != base {
			t.Fatalf("NextDelay(1, ...) = %s, want exactly BaseDelay %s", got, base)
		}
	})

	t.Run("grows_toward_max_delay_without_jitter", func(t *testing.T) {
		p := Policy{BaseDelay: base, MaxDelay: maxDelay, Multiplier: mult, MaxAttempts: 20, Jitter: 0}
		rng := seededRand(1)
		var prev time.Duration
		for attempt := 1; attempt <= 6; attempt++ {
			got, err := p.NextDelay(attempt, rng)
			if err != nil {
				t.Fatalf("attempt %d: NextDelay error = %v", attempt, err)
			}
			if got < prev {
				t.Fatalf("attempt %d: NextDelay = %s, want >= previous attempt's %s (monotonic growth)", attempt, got, prev)
			}
			prev = got
		}
	})

	t.Run("large_attempt_stays_at_max_delay_without_jitter", func(t *testing.T) {
		p := Policy{BaseDelay: base, MaxDelay: maxDelay, Multiplier: mult, MaxAttempts: 30, Jitter: 0}
		got, err := p.NextDelay(20, seededRand(1))
		if err != nil {
			t.Fatalf("NextDelay(20, ...) error = %v", err)
		}
		if got != maxDelay {
			t.Fatalf("NextDelay(20, ...) = %s, want exactly MaxDelay %s", got, maxDelay)
		}
	})

	t.Run("result_never_exceeds_max_delay", func(t *testing.T) {
		p := Policy{BaseDelay: base, MaxDelay: maxDelay, Multiplier: mult, MaxAttempts: 30, Jitter: 0.2}
		for seed := int64(0); seed < 50; seed++ {
			rng := seededRand(seed)
			for attempt := 1; attempt <= 20; attempt++ {
				got, err := p.NextDelay(attempt, rng)
				if err != nil {
					t.Fatalf("seed %d attempt %d: NextDelay error = %v", seed, attempt, err)
				}
				if got > maxDelay {
					t.Fatalf("seed %d attempt %d: NextDelay = %s, want <= MaxDelay %s", seed, attempt, got, maxDelay)
				}
			}
		}
	})

	t.Run("jitter_stays_within_capped_bounds", func(t *testing.T) {
		jitter := 0.2
		p := Policy{BaseDelay: base, MaxDelay: maxDelay, Multiplier: mult, MaxAttempts: 30, Jitter: jitter}
		for attempt := 1; attempt <= 8; attempt++ {
			raw := float64(base) * math.Pow(mult, float64(attempt-1))
			capped := raw
			if capped > float64(maxDelay) {
				capped = float64(maxDelay)
			}
			lo := time.Duration(capped * (1 - jitter))
			hi := time.Duration(capped)

			for seed := int64(0); seed < 50; seed++ {
				got, err := p.NextDelay(attempt, seededRand(seed))
				if err != nil {
					t.Fatalf("attempt %d seed %d: NextDelay error = %v", attempt, seed, err)
				}
				if got < lo || got > hi {
					t.Fatalf("attempt %d seed %d: NextDelay = %s, want within [%s, %s]", attempt, seed, got, lo, hi)
				}
			}
		}
	})

	t.Run("zero_jitter_is_deterministic_regardless_of_rng_state", func(t *testing.T) {
		p := Policy{BaseDelay: base, MaxDelay: maxDelay, Multiplier: mult, MaxAttempts: 30, Jitter: 0}
		a, err := p.NextDelay(4, seededRand(1))
		if err != nil {
			t.Fatalf("NextDelay error = %v", err)
		}
		b, err := p.NextDelay(4, seededRand(999))
		if err != nil {
			t.Fatalf("NextDelay error = %v", err)
		}
		if a != b {
			t.Fatalf("NextDelay(4, ...) = %s and %s across different rng seeds, want equal since Jitter is 0", a, b)
		}
	})

	t.Run("same_seed_produces_same_delay", func(t *testing.T) {
		p := Policy{BaseDelay: base, MaxDelay: maxDelay, Multiplier: mult, MaxAttempts: 30, Jitter: 0.2}
		a, err := p.NextDelay(3, seededRand(42))
		if err != nil {
			t.Fatalf("NextDelay error = %v", err)
		}
		b, err := p.NextDelay(3, seededRand(42))
		if err != nil {
			t.Fatalf("NextDelay error = %v", err)
		}
		if a != b {
			t.Fatalf("NextDelay(3, ...) = %s then %s for the same seed, want equal", a, b)
		}
	})
}

func TestPolicy_ShouldRetry(t *testing.T) {
	base := Policy{
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    time.Second,
		Multiplier:  2.0,
		MaxAttempts: 3,
		MaxElapsed:  0,
		Jitter:      0,
	}
	transient := ErrTransient
	permanent := errors.New("permanent failure")

	t.Run("nil_error_stops", func(t *testing.T) {
		if base.ShouldRetry(1, 0, nil) {
			t.Fatal("ShouldRetry(1, 0, nil) = true, want false")
		}
	})

	t.Run("non_retryable_error_stops", func(t *testing.T) {
		if base.ShouldRetry(1, 0, permanent) {
			t.Fatal("ShouldRetry with a non-retryable error = true, want false")
		}
	})

	t.Run("retryable_error_within_budget_continues", func(t *testing.T) {
		if !base.ShouldRetry(1, 0, transient) {
			t.Fatal("ShouldRetry with a retryable error, attempt 1 of 3 = false, want true")
		}
	})

	t.Run("stops_at_max_attempts", func(t *testing.T) {
		if base.ShouldRetry(base.MaxAttempts, 0, transient) {
			t.Fatalf("ShouldRetry at attempt == MaxAttempts (%d) = true, want false", base.MaxAttempts)
		}
	})

	t.Run("continues_just_below_max_attempts", func(t *testing.T) {
		if !base.ShouldRetry(base.MaxAttempts-1, 0, transient) {
			t.Fatalf("ShouldRetry at attempt == MaxAttempts-1 (%d) = false, want true", base.MaxAttempts-1)
		}
	})

	t.Run("zero_max_elapsed_means_unlimited", func(t *testing.T) {
		p := base
		p.MaxElapsed = 0
		if !p.ShouldRetry(1, 365*24*time.Hour, transient) {
			t.Fatal("ShouldRetry with MaxElapsed 0 (unlimited) and a huge elapsed = false, want true")
		}
	})

	t.Run("nonzero_max_elapsed_stops_once_reached", func(t *testing.T) {
		p := base
		p.MaxElapsed = time.Minute
		if p.ShouldRetry(1, time.Minute, transient) {
			t.Fatal("ShouldRetry with elapsed == MaxElapsed = true, want false")
		}
	})

	t.Run("nonzero_max_elapsed_continues_before_reached", func(t *testing.T) {
		p := base
		p.MaxElapsed = time.Minute
		if !p.ShouldRetry(1, time.Second, transient) {
			t.Fatal("ShouldRetry with elapsed well under MaxElapsed = false, want true")
		}
	})
}

func TestDefaultPolicy(t *testing.T) {
	t.Run("matches_documented_constants", func(t *testing.T) {
		if DefaultPolicy.BaseDelay != 100*time.Millisecond {
			t.Errorf("DefaultPolicy.BaseDelay = %s, want 100ms", DefaultPolicy.BaseDelay)
		}
		if DefaultPolicy.MaxDelay != 30*time.Second {
			t.Errorf("DefaultPolicy.MaxDelay = %s, want 30s", DefaultPolicy.MaxDelay)
		}
		if DefaultPolicy.Multiplier != 2.0 {
			t.Errorf("DefaultPolicy.Multiplier = %v, want 2.0", DefaultPolicy.Multiplier)
		}
		if DefaultPolicy.MaxAttempts != 5 {
			t.Errorf("DefaultPolicy.MaxAttempts = %d, want 5", DefaultPolicy.MaxAttempts)
		}
		if DefaultPolicy.Jitter != 0.2 {
			t.Errorf("DefaultPolicy.Jitter = %v, want 0.2", DefaultPolicy.Jitter)
		}
	})

	t.Run("is_internally_valid", func(t *testing.T) {
		if err := DefaultPolicy.Validate(); err != nil {
			t.Fatalf("DefaultPolicy.Validate() = %v, want nil", err)
		}
	})
}
