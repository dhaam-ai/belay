package bugs

func init() {
	register(Defect{
		ID:          "B15",
		Class:       "error-swallowed",
		File:        "errors.go",
		Symbol:      "Classify",
		Description: "Error swallowed: returning nil instead of the original error silently tells the caller a non-matching operation can be treated as successful.",
		Find:        "\treturn err\n}",
		Replace:     "\treturn nil\n}",
		BreaksTests: []string{"TestClassify/leaves_unchanged_when_predicate_false"},
		DetectedBy:  DetectedByTest,
	})
}
