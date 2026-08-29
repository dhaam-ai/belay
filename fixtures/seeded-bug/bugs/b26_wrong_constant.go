package bugs

func init() {
	register(Defect{
		ID:          "B26",
		Class:       "wrong-constant",
		File:        "policy.go",
		Symbol:      "DefaultBaseDelay",
		Description: "Wrong constant: the base delay's unit was changed from Millisecond to Second, a 1000x unit mistake that is one of the most common real-world constant bugs.",
		Find:        "DefaultBaseDelay   = 100 * time.Millisecond",
		Replace:     "DefaultBaseDelay   = 100 * time.Second",
		BreaksTests: []string{"TestDefaultPolicy/matches_documented_constants", "TestDefaultPolicy/is_internally_valid"},
		DetectedBy:  DetectedByTest,
	})
}
