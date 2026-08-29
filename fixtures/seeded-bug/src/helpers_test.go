package backoff

import "math/rand"

// seededRand returns a deterministic pseudo-random source for tests.
// math/rand is intentional here: tests need reproducible jitter draws
// across runs, not cryptographic unpredictability, and this is the only
// place that constructs one so the rest of the suite stays gosec-clean.
func seededRand(seed int64) *rand.Rand {
	return rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test jitter, not security-sensitive
}
