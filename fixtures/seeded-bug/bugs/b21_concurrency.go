package bugs

func init() {
	register(Defect{
		ID:             "B21",
		Class:          "concurrency",
		File:           "ledger.go",
		Symbol:         "Ledger.Total",
		Description:    "Dropped mutex lock on the read path: Total now reads the counter while Record concurrently mutates it, unsynchronized. Requires -race to detect reliably; see B20's note on why this is verified by retrying.",
		Find:           "func (l *Ledger) Total() int {\n\tl.mu.Lock()\n\tdefer l.mu.Unlock()\n\treturn l.total\n}\n",
		Replace:        "func (l *Ledger) Total() int {\n\treturn l.total\n}\n",
		BreaksTests:    []string{"TestLedger_ConcurrentTotal/concurrent_reads_and_writes_are_safe"},
		DetectedBy:     DetectedByTest,
		FlakyDetection: true,
	})
}
