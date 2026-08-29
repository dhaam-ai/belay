package bugs

func init() {
	register(Defect{
		ID:          "B02",
		Class:       "off-by-one",
		File:        "policy.go",
		Symbol:      "Policy.ShouldRetry",
		Description: "Boundary comparison off-by-one: > in place of >= lets one extra retry attempt through past the configured MaxAttempts.",
		Find:        "if attempt >= p.MaxAttempts {",
		Replace:     "if attempt > p.MaxAttempts {",
		BreaksTests: []string{"TestPolicy_ShouldRetry/stops_at_max_attempts", "TestRun/exhausts_max_attempts_and_returns_the_last_error"},
		DetectedBy:  DetectedByTest,
	})
}
