package bugs

func init() {
	register(Defect{
		ID:          "B04",
		Class:       "inverted-condition",
		File:        "policy.go",
		Symbol:      "Policy.ShouldRetry",
		Description: "Dropped negation: retryable errors now stop the retry loop and non-retryable errors now continue it, exactly backwards.",
		Find:        "if !IsRetryable(err) {",
		Replace:     "if IsRetryable(err) {",
		BreaksTests: []string{"TestPolicy_ShouldRetry/non_retryable_error_stops", "TestPolicy_ShouldRetry/retryable_error_within_budget_continues", "TestPolicy_ShouldRetry/continues_just_below_max_attempts", "TestPolicy_ShouldRetry/nonzero_max_elapsed_continues_before_reached", "TestPolicy_ShouldRetry/zero_max_elapsed_means_unlimited", "TestRun/exhausts_max_attempts_and_returns_the_last_error", "TestRun/nil_ledger_is_accepted", "TestRun/non_retryable_error_stops_immediately", "TestRun/retries_until_success", "TestRun/sleep_error_stops_retrying_immediately"},
		DetectedBy:  DetectedByTest,
	})
}
