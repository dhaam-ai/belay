package backoff

import "time"

// Clamp returns d restricted to the inclusive range [lo, hi]. It panics
// if lo > hi.
func Clamp(d, lo, hi time.Duration) time.Duration {
	if lo > hi {
		panic("backoff: Clamp: lo > hi")
	}
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

// ExponentialDelay doubles base, attempt times, capped at limit. It is a
// cheap integer-only alternative to Policy.NextDelay for callers that
// don't need jitter or a full Policy value.
//
// The doubling is guarded so it can never overflow time.Duration
// (int64 nanoseconds), regardless of how large attempt is: once one more
// doubling would exceed limit, ExponentialDelay stops and returns limit.
func ExponentialDelay(base time.Duration, attempt uint, limit time.Duration) time.Duration {
	if base <= 0 || limit <= 0 {
		return 0
	}
	if base >= limit {
		return limit
	}
	delay := base
	for i := uint(0); i < attempt; i++ {
		if delay > limit>>1 {
			return limit
		}
		delay <<= 1
	}
	return delay
}

// MinDelay returns the smallest of the given delays and true. If delays
// is empty, it returns 0 and false.
func MinDelay(delays []time.Duration) (time.Duration, bool) {
	if len(delays) == 0 {
		return 0, false
	}
	smallest := delays[0]
	for _, d := range delays[1:] {
		if d < smallest {
			smallest = d
		}
	}
	return smallest, true
}
