package bugs

func init() {
	register(Defect{
		ID:          "B17",
		Class:       "early-return-before-required-work",
		File:        "ledger.go",
		Symbol:      "ClassifyAndCount",
		Description: "Early return moved before required bookkeeping: the nil-error fast path now returns before recording the attempt, silently breaking the documented 'always records' contract.",
		Find:        "\tledger.Record(name)\n\tif err == nil {\n\t\treturn false\n\t}\n",
		Replace:     "\tif err == nil {\n\t\treturn false\n\t}\n\tledger.Record(name)\n",
		BreaksTests: []string{"TestClassifyAndCount/records_even_when_error_is_nil"},
		DetectedBy:  DetectedByTest,
	})
}
