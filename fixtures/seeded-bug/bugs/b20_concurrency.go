package bugs

func init() {
	register(Defect{
		ID:             "B20",
		Class:          "concurrency",
		File:           "ledger.go",
		Symbol:         "Ledger.Record",
		Description:    "Dropped mutex lock: concurrent Record calls now race on the shared map and total counter, the exact bug -race exists to catch. Requires -race to detect reliably; without it, a plain `go test` may still pass or may crash with \"fatal error: concurrent map writes\". Reliably takes TestLedger_ConcurrentReset and TestLedger_ConcurrentTotal down with it too when the whole suite runs together in one `go test -race ./...` process: -race's shadow memory persists across sequential tests in the same run, so a real race in Record's map access can surface while a later test's goroutines happen to reuse the same heap address, even though Reset and Total each still lock correctly on their own. Even so, catching a genuine data race depends on the goroutine interleaving that particular run happens to hit, so this is verified by retrying rather than on a single run.",
		Find:           "func (l *Ledger) Record(op string) int {\n\tl.mu.Lock()\n\tdefer l.mu.Unlock()\n\tl.total++\n",
		Replace:        "func (l *Ledger) Record(op string) int {\n\tl.total++\n",
		BreaksTests:    []string{"TestLedger_ConcurrentRecord/many_goroutines_recording_the_same_op_produce_an_exact_count", "TestLedger_ConcurrentReset/reset_during_concurrent_recording_does_not_corrupt_the_ledger", "TestLedger_ConcurrentTotal/concurrent_reads_and_writes_are_safe"},
		DetectedBy:     DetectedByTest,
		FlakyDetection: true,
	})
}
