package bugs

func init() {
	register(Defect{
		ID:          "B18",
		Class:       "integer-overflow-truncation",
		File:        "delay.go",
		Symbol:      "ExponentialDelay",
		Description: "Removed overflow guard: doubling proceeds unconditionally for every attempt, so a large attempt count overflows time.Duration's int64 range instead of saturating at limit.",
		Find:        "\t\tif delay > limit>>1 {\n\t\t\treturn limit\n\t\t}\n",
		Replace:     "",
		BreaksTests: []string{"TestExponentialDelay/large_attempt_does_not_overflow", "TestExponentialDelay/caps_at_max"},
		DetectedBy:  DetectedByTest,
	})
}
