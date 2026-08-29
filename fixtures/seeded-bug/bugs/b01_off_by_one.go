package bugs

func init() {
	register(Defect{
		ID:          "B01",
		Class:       "off-by-one",
		File:        "delay.go",
		Symbol:      "ExponentialDelay",
		Description: "Loop bound off-by-one: <= in place of < performs one extra doubling per call, the classic off-by-one when a count is turned into a loop bound.",
		Find:        "for i := uint(0); i < attempt; i++ {",
		Replace:     "for i := uint(0); i <= attempt; i++ {",
		BreaksTests: []string{"TestExponentialDelay/doubles_each_attempt", "TestExponentialDelay/zero_attempt_returns_base"},
		DetectedBy:  DetectedByTest,
	})
}
