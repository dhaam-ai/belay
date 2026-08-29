package bugs

func init() {
	register(Defect{
		ID:          "B03",
		Class:       "off-by-one",
		File:        "policy.go",
		Symbol:      "Policy.Validate",
		Description: "Boundary comparison off-by-one: <= in place of < wrongly rejects the valid edge case where MaxDelay equals BaseDelay.",
		Find:        "case p.MaxDelay < p.BaseDelay:",
		Replace:     "case p.MaxDelay <= p.BaseDelay:",
		BreaksTests: []string{"TestPolicy_Validate/max_delay_equal_to_base_delay_is_valid"},
		DetectedBy:  DetectedByTest,
	})
}
