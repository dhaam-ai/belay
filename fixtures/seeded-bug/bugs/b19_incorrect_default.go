package bugs

func init() {
	register(Defect{
		ID:          "B19",
		Class:       "incorrect-default",
		File:        "policy.go",
		Symbol:      "Policy.ShouldRetry",
		Description: "Dropped sentinel check: MaxElapsed's documented zero-means-unlimited convention is ignored, so leaving it at its zero value now expires the retry budget immediately instead of never.",
		Find:        "if p.MaxElapsed != 0 && elapsed >= p.MaxElapsed {",
		Replace:     "if elapsed >= p.MaxElapsed {",
		BreaksTests: []string{"TestPolicy_ShouldRetry/zero_max_elapsed_means_unlimited", "TestPolicy_ShouldRetry/continues_just_below_max_attempts", "TestPolicy_ShouldRetry/retryable_error_within_budget_continues", "TestRun/exhausts_max_attempts_and_returns_the_last_error", "TestRun/nil_ledger_is_accepted", "TestRun/retries_until_success", "TestRun/sleep_error_stops_retrying_immediately"},
		DetectedBy:  DetectedByTest,
	})
}
