package bugs

func init() {
	register(Defect{
		ID:          "B07",
		Class:       "wrong-operator",
		File:        "delay.go",
		Symbol:      "ExponentialDelay",
		Description: "Wrong operator: left-shift-by-one (double) replaced with add-one, turning exponential growth into linear growth.",
		Find:        "delay <<= 1",
		Replace:     "delay += 1",
		BreaksTests: []string{"TestExponentialDelay/doubles_each_attempt", "TestExponentialDelay/caps_at_max"},
		DetectedBy:  DetectedByTest,
	})
}
