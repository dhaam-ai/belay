package bugs

func init() {
	register(Defect{
		ID:          "B23",
		Class:       "concurrency",
		File:        "ledger.go",
		Symbol:      "Ledger.Count",
		Description: "Pointer receiver changed to value receiver: every call now copies the Ledger's embedded sync.Mutex, which go vet's copylocks check flags and which silently breaks the mutual-exclusion contract for concurrent callers.",
		Find:        "func (l *Ledger) Count(op string) int {",
		Replace:     "func (l Ledger) Count(op string) int {",
		BreaksTests: nil,
		DetectedBy:  DetectedByLint,
		Linter:      "govet",
	})
}
