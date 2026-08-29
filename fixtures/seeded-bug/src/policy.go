// Package backoff computes retry delays and retry decisions for a bounded
// exponential-backoff policy. It performs no I/O and starts no goroutines
// on the caller's behalf beyond WatchTotal: callers own the actual sleep
// and the actual retry loop (or use Run, which owns a simple one for
// them); this package only calculates numbers and tracks bookkeeping.
package backoff

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"time"
)

// Policy configures an exponential backoff schedule with optional jitter,
// a maximum per-attempt delay, a maximum attempt count, and an optional
// ceiling on total elapsed retry time.
//
// MaxElapsed is a sentinel field: zero means "no elapsed-time limit," not
// "expire immediately." Callers that want a real zero-duration budget are
// not supported by this policy; use MaxAttempts: 1 instead.
type Policy struct {
	// BaseDelay is the delay used for the first retry attempt (attempt 1),
	// before jitter is applied. Must be > 0.
	BaseDelay time.Duration
	// MaxDelay caps every computed delay, including jitter. Must be >=
	// BaseDelay.
	MaxDelay time.Duration
	// Multiplier is the exponential growth factor applied per attempt.
	// Must be > 1.
	Multiplier float64
	// MaxAttempts is the maximum number of retry attempts allowed. Must
	// be > 0.
	MaxAttempts int
	// MaxElapsed is the maximum total elapsed time across all retries.
	// Zero means unlimited.
	MaxElapsed time.Duration
	// Jitter is the fraction of the capped delay that is randomized, in
	// [0, 1]. 0 disables jitter; 1 randomizes the full delay.
	Jitter float64
}

// Default policy constants, used by DefaultPolicy.
const (
	DefaultBaseDelay   = 100 * time.Millisecond
	DefaultMaxDelay    = 30 * time.Second
	DefaultMultiplier  = 2.0
	DefaultMaxAttempts = 5
	DefaultJitter      = 0.2
)

// DefaultPolicy is a reasonable general-purpose policy: a 100ms base delay
// that doubles each attempt, capped at 30s, up to 5 attempts, with 20%
// jitter and no elapsed-time limit.
var DefaultPolicy = Policy{
	BaseDelay:   DefaultBaseDelay,
	MaxDelay:    DefaultMaxDelay,
	Multiplier:  DefaultMultiplier,
	MaxAttempts: DefaultMaxAttempts,
	MaxElapsed:  0,
	Jitter:      DefaultJitter,
}

// ErrInvalidPolicy is returned by Validate when a Policy field is out of
// its documented range.
var ErrInvalidPolicy = errors.New("backoff: invalid policy")

// Validate reports whether p's fields are within the ranges documented on
// Policy. It returns an error wrapping ErrInvalidPolicy describing the
// first invalid field found, or nil if p is valid.
func (p Policy) Validate() error {
	switch {
	case p.BaseDelay <= 0:
		return fmt.Errorf("%w: BaseDelay must be > 0, got %s", ErrInvalidPolicy, p.BaseDelay)
	case p.MaxDelay < p.BaseDelay:
		return fmt.Errorf("%w: MaxDelay (%s) must be >= BaseDelay (%s)", ErrInvalidPolicy, p.MaxDelay, p.BaseDelay)
	case p.Multiplier <= 1:
		return fmt.Errorf("%w: Multiplier must be > 1, got %v", ErrInvalidPolicy, p.Multiplier)
	case p.MaxAttempts <= 0:
		return fmt.Errorf("%w: MaxAttempts must be > 0, got %d", ErrInvalidPolicy, p.MaxAttempts)
	case p.MaxElapsed < 0:
		return fmt.Errorf("%w: MaxElapsed must be >= 0, got %s", ErrInvalidPolicy, p.MaxElapsed)
	case p.Jitter < 0 || p.Jitter > 1:
		return fmt.Errorf("%w: Jitter must be in [0,1], got %v", ErrInvalidPolicy, p.Jitter)
	}
	return nil
}

// ErrInvalidAttempt is returned by NextDelay when attempt is less than 1.
var ErrInvalidAttempt = errors.New("backoff: attempt must be >= 1")

// NextDelay computes the delay to wait before the given retry attempt.
// attempt is 1-based: attempt 1 is the first retry, and its delay before
// jitter equals p.BaseDelay. The result is always within [p.BaseDelay,
// p.MaxDelay] for attempt 1, and never exceeds p.MaxDelay for any attempt.
//
// rng supplies jitter randomness. Passing an *rand.Rand built from the
// same seed for the same inputs yields the same delay, which callers rely
// on in tests.
func (p Policy) NextDelay(attempt int, rng *rand.Rand) (time.Duration, error) {
	if attempt < 1 {
		return 0, fmt.Errorf("%w: got %d", ErrInvalidAttempt, attempt)
	}

	raw := float64(p.BaseDelay) * math.Pow(p.Multiplier, float64(attempt-1))
	capped := Clamp(time.Duration(raw), p.BaseDelay, p.MaxDelay)

	spread := float64(capped) * p.Jitter
	jittered := float64(capped) - spread + spread*rng.Float64()
	return time.Duration(jittered), nil
}

// ShouldRetry reports whether another attempt should be made, given the
// attempt number that just finished (1-based), the elapsed time since the
// first attempt, and the error that attempt returned.
//
// It returns false when err is nil (nothing to retry), when err is not
// retryable, when attempt has reached MaxAttempts, or when elapsed has
// reached MaxElapsed (unless MaxElapsed is zero, which means unlimited).
func (p Policy) ShouldRetry(attempt int, elapsed time.Duration, err error) bool {
	if err == nil {
		return false
	}
	if !IsRetryable(err) {
		return false
	}
	if attempt >= p.MaxAttempts {
		return false
	}
	if p.MaxElapsed != 0 && elapsed >= p.MaxElapsed {
		return false
	}
	return true
}
