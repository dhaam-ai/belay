package bugs

func init() {
	register(Defect{
		ID:          "B08",
		Class:       "wrong-operator",
		File:        "policy.go",
		Symbol:      "Policy.NextDelay",
		Description: "Wrong operator: the jitter spread is added to the capped delay instead of scaled by it, so the Jitter fraction stops controlling how much randomness is applied.",
		Find:        "spread := float64(capped) * p.Jitter",
		Replace:     "spread := float64(capped) + p.Jitter",
		BreaksTests: []string{"TestPolicy_NextDelay/jitter_stays_within_capped_bounds", "TestPolicy_NextDelay/first_attempt_without_jitter_equals_base_delay", "TestPolicy_NextDelay/large_attempt_stays_at_max_delay_without_jitter", "TestPolicy_NextDelay/zero_jitter_is_deterministic_regardless_of_rng_state"},
		DetectedBy:  DetectedByTest,
	})
}
