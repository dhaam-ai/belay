package bugs

func init() {
	register(Defect{
		ID:          "B24",
		Class:       "resource-leak",
		File:        "ledger.go",
		Symbol:      "WaitForEach",
		Description: "Explicit per-iteration timer.Stop()/ticker.Stop() replaced with defer right after creation: still correct Go, and each name still resolves at the same time, but now every name's timer and ticker stay open until WaitForEach itself returns instead of being released as soon as that name is done. gocritic's deferInLoop flags this exact pattern.",
		Find:        "\t\ttimer := time.NewTimer(timeout)\n\t\tticker := time.NewTicker(pollEvery)\n\twaitOne:\n\t\tfor {\n\t\t\tselect {\n\t\t\tcase <-timer.C:\n\t\t\t\ttimedOut = append(timedOut, name)\n\t\t\t\tbreak waitOne\n\t\t\tcase <-ticker.C:\n\t\t\t\tif ledger.Count(name) > 0 {\n\t\t\t\t\tbreak waitOne\n\t\t\t\t}\n\t\t\t}\n\t\t}\n\t\ttimer.Stop()\n\t\tticker.Stop()\n",
		Replace:     "\t\ttimer := time.NewTimer(timeout)\n\t\tdefer timer.Stop()\n\t\tticker := time.NewTicker(pollEvery)\n\t\tdefer ticker.Stop()\n\twaitOne:\n\t\tfor {\n\t\t\tselect {\n\t\t\tcase <-timer.C:\n\t\t\t\ttimedOut = append(timedOut, name)\n\t\t\t\tbreak waitOne\n\t\t\tcase <-ticker.C:\n\t\t\t\tif ledger.Count(name) > 0 {\n\t\t\t\t\tbreak waitOne\n\t\t\t\t}\n\t\t\t}\n\t\t}\n",
		BreaksTests: nil,
		DetectedBy:  DetectedByLint,
		Linter:      "gocritic",
	})
}
