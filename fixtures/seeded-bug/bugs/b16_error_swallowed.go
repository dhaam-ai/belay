package bugs

func init() {
	register(Defect{
		ID:          "B16",
		Class:       "error-swallowed",
		File:        "orchestrate.go",
		Symbol:      "Run",
		Description: "Error swallowed: the sleep error (e.g. context cancellation) is dropped entirely instead of stopping the retry loop, and errcheck also flags the now-unchecked return value.",
		Find:        "\t\tif serr := sleep(ctx, delay); serr != nil {\n\t\t\treturn serr\n\t\t}\n",
		Replace:     "\t\tsleep(ctx, delay)\n",
		BreaksTests: []string{"TestRun/sleep_error_stops_retrying_immediately"},
		DetectedBy:  DetectedByBoth,
		Linter:      "errcheck",
	})
}
