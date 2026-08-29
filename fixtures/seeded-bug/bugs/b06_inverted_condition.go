package bugs

func init() {
	register(Defect{
		ID:          "B06",
		Class:       "inverted-condition",
		File:        "errors.go",
		Symbol:      "Classify",
		Description: "Dropped negation on the classification predicate: errors the caller wanted marked retryable are left alone, and everything else is marked retryable instead.",
		Find:        "if predicate(err) {",
		Replace:     "if !predicate(err) {",
		BreaksTests: []string{"TestClassify/wraps_when_predicate_true", "TestClassify/leaves_unchanged_when_predicate_false"},
		DetectedBy:  DetectedByTest,
	})
}
