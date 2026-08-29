package bugs

func init() {
	register(Defect{
		ID:          "B09",
		Class:       "wrong-operator",
		File:        "policy.go",
		Symbol:      "Policy.NextDelay",
		Description: "Wrong operator: adding the jitter spread instead of subtracting it first means the result can exceed the capped delay, breaking the documented MaxDelay ceiling.",
		Find:        "jittered := float64(capped) - spread + spread*rng.Float64()",
		Replace:     "jittered := float64(capped) + spread + spread*rng.Float64()",
		BreaksTests: []string{"TestPolicy_NextDelay/result_never_exceeds_max_delay", "TestPolicy_NextDelay/jitter_stays_within_capped_bounds"},
		DetectedBy:  DetectedByTest,
	})
}
