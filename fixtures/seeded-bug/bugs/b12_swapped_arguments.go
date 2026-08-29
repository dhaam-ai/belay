package bugs

func init() {
	register(Defect{
		ID:          "B12",
		Class:       "swapped-arguments",
		File:        "policy.go",
		Symbol:      "Policy.NextDelay",
		Description: "Swapped call-site arguments: Clamp's first two parameters (value, lower bound) are transposed, so BaseDelay is clamped against the computed delay instead of the other way around. This attempt panics (Clamp's lo > hi precondition), which crashes the test binary before later-ordered subtests such as result_never_exceeds_max_delay get a chance to run; only the first subtest hit by that crash shows up as a reported failure.",
		Find:        "capped := Clamp(time.Duration(raw), p.BaseDelay, p.MaxDelay)",
		Replace:     "capped := Clamp(p.BaseDelay, time.Duration(raw), p.MaxDelay)",
		BreaksTests: []string{"TestPolicy_NextDelay/large_attempt_stays_at_max_delay_without_jitter"},
		DetectedBy:  DetectedByTest,
	})
}
