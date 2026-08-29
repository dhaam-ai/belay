package bugs

func init() {
	register(Defect{
		ID:          "B25",
		Class:       "wrong-constant",
		File:        "policy.go",
		Symbol:      "DefaultMaxAttempts",
		Description: "Wrong constant: the documented default of 5 max attempts was changed to 50, a fat-fingered threshold that would let a default-configured caller retry ten times longer than intended.",
		Find:        "DefaultMaxAttempts = 5",
		Replace:     "DefaultMaxAttempts = 50",
		BreaksTests: []string{"TestDefaultPolicy/matches_documented_constants"},
		DetectedBy:  DetectedByTest,
	})
}
