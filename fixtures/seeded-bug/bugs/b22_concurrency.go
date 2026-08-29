package bugs

func init() {
	register(Defect{
		ID:             "B22",
		Class:          "concurrency",
		File:           "ledger.go",
		Symbol:         "Ledger.Reset",
		Description:    "Dropped mutex lock: Reset now replaces the map while other goroutines concurrently read or write it, unsynchronized. Requires -race to detect reliably; see B20's note on why this is verified by retrying.",
		Find:           "func (l *Ledger) Reset() {\n\tl.mu.Lock()\n\tdefer l.mu.Unlock()\n\tl.attempts = make(map[string]int)\n\tl.total = 0\n}\n",
		Replace:        "func (l *Ledger) Reset() {\n\tl.attempts = make(map[string]int)\n\tl.total = 0\n}\n",
		BreaksTests:    []string{"TestLedger_ConcurrentReset/reset_during_concurrent_recording_does_not_corrupt_the_ledger"},
		DetectedBy:     DetectedByTest,
		FlakyDetection: true,
	})
}
