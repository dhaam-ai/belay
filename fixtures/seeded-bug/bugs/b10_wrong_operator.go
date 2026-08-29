package bugs

func init() {
	register(Defect{
		ID:          "B10",
		Class:       "wrong-operator",
		File:        "ledger.go",
		Symbol:      "Ledger.Record",
		Description: "== flipped to !=: recording now skips every named operation and only tracks the empty-string key, inverting the guard's intent.",
		Find:        "if op == \"\" {",
		Replace:     "if op != \"\" {",
		BreaksTests: []string{"TestLedger_Record/named_op_increments_its_own_counter", "TestLedger_Record/different_ops_are_counted_independently", "TestLedger_Record/empty_op_is_not_recorded_per_name", "TestClassifyAndCount/records_and_reports_non_retryable_errors", "TestClassifyAndCount/records_and_reports_retryable_errors", "TestClassifyAndCount/records_even_when_error_is_nil", "TestLedger_ConcurrentRecord/many_goroutines_recording_the_same_op_produce_an_exact_count", "TestLedger_Reset/ledger_is_usable_after_reset", "TestRun/retries_until_success", "TestRun/succeeds_on_first_try", "TestWaitForEach/only_reports_the_names_that_were_never_recorded", "TestWaitForEach/returns_empty_when_all_names_are_already_recorded"},
		DetectedBy:  DetectedByTest,
	})
}
