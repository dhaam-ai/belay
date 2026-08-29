package bugs

func init() {
	register(Defect{
		ID:          "B14",
		Class:       "nil-empty-handling",
		File:        "orchestrate.go",
		Symbol:      "Run",
		Description: "Missing nil check: Run documents ledger as optional (nil means don't track), but dropping the guard makes a nil ledger panic on the very first attempt.",
		Find:        "\t\tif ledger != nil {\n\t\t\tledger.Record(name)\n\t\t}\n",
		Replace:     "\t\tledger.Record(name)\n",
		BreaksTests: []string{"TestRun/nil_ledger_is_accepted"},
		DetectedBy:  DetectedByTest,
	})
}
