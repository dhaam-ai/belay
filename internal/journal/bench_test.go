package journal

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// buildBenchJournal renders n valid, alternating node_started/node_finished
// records as NDJSON, the way a long-running loopy node would accumulate
// them over a very long run.
func buildBenchJournal(tb testing.TB, n int) []byte {
	tb.Helper()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	for i := 1; i <= n; i++ {
		seq := uint64(i)
		ts := base.Add(time.Duration(i) * time.Millisecond)

		var rec Record
		if i%2 == 1 {
			rec = Record{Seq: seq, Node: "bench", Attempt: 1, Event: EventNodeStarted, Time: ts}
		} else {
			rec = Record{
				Seq: seq, Node: "bench", Attempt: 1, Event: EventNodeFinished, Time: ts,
				DurationMS: 5, Status: StatusOK, Next: "bench",
				Usage: &Usage{InputTokens: 12, OutputTokens: 34, USD: 0.0001},
				Note:  "ordinary iteration",
			}
		}
		if err := enc.Encode(rec); err != nil {
			tb.Fatalf("encoding benchmark fixture record %d: %v", i, err)
		}
	}
	return buf.Bytes()
}

// BenchmarkRead100kRecords is the acceptance benchmark for requirement 4:
// reading a 100,000-record journal, reporting ns/op and B/op (via
// b.ReportAllocs) so memory use is visible, not just wall time.
func BenchmarkRead100kRecords(b *testing.B) {
	const n = 100_000
	data := buildBenchJournal(b, n)

	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		result, err := Read(bytes.NewReader(data))
		if err != nil {
			b.Fatalf("Read returned error: %v", err)
		}
		if len(result.Records) != n {
			b.Fatalf("Read returned %d records, want %d", len(result.Records), n)
		}
	}
}

// BenchmarkResolveStart100kRecords benchmarks the other end-to-end hot path
// over the same size journal: validating and resolving a resume decision,
// which is what belay actually does with a journal's records on every run
// and resume.
func BenchmarkResolveStart100kRecords(b *testing.B) {
	const n = 100_000
	data := buildBenchJournal(b, n)

	result, err := Read(bytes.NewReader(data))
	if err != nil {
		b.Fatalf("Read returned error: %v", err)
	}
	records := result.Records

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := ResolveStart(records); err != nil {
			b.Fatalf("ResolveStart returned error: %v", err)
		}
	}
}
