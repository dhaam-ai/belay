package bugs

func init() {
	register(Defect{
		ID:          "B05",
		Class:       "inverted-condition",
		File:        "policy.go",
		Symbol:      "Policy.Validate",
		Description: "&& in place of ||: a value can never be both below 0 and above 1 at once, so the range check silently stops rejecting anything.",
		Find:        "case p.Jitter < 0 || p.Jitter > 1:",
		Replace:     "case p.Jitter < 0 && p.Jitter > 1:",
		BreaksTests: []string{"TestPolicy_Validate/jitter_below_zero_is_invalid", "TestPolicy_Validate/jitter_above_one_is_invalid"},
		DetectedBy:  DetectedByTest,
	})
}
