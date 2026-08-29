package bugs

func init() {
	register(Defect{
		ID:          "B13",
		Class:       "nil-empty-handling",
		File:        "delay.go",
		Symbol:      "MinDelay",
		Description: "Missing empty-slice guard: indexing delays[0] on an empty slice panics instead of returning the documented (0, false).",
		Find:        "\tif len(delays) == 0 {\n\t\treturn 0, false\n\t}\n",
		Replace:     "",
		BreaksTests: []string{"TestMinDelay/empty_input_returns_false"},
		DetectedBy:  DetectedByTest,
	})
}
