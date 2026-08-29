package journal

import (
	"encoding/json"
	"testing"
	"time"
)

// buildSampleRun returns a deterministic, fully valid 22-record history for
// a run that loops through the test/fix quality gate twice before passing:
//
//	run_started
//	plan -> approve -> code -> write
//	test (failed) -> fix -> test (failed) -> fix -> test (ok) -> review
//	run_completed
//
// It is shared by the truncation sweep (read_test.go), which needs a
// realistic, non-trivial valid file, and the Stats test (stats_test.go),
// which needs exactly this test/fix/test/fix/test loop shape to prove loop
// iterations are counted correctly.
func buildSampleRun(t *testing.T) []Record {
	t.Helper()

	base := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	var seq uint64
	next := func() (uint64, time.Time) {
		seq++
		// #nosec G115 -- seq is this fixture's own small monotonic counter
		// (bounded by the number of records built below), never near
		// overflowing int64.
		return seq, base.Add(time.Duration(seq) * time.Second)
	}

	var records []Record

	nodeVisit := func(node string, attempt int, status Status, dur int64, nextNode string) {
		s, ts := next()
		records = append(records, Record{Seq: s, Node: node, Attempt: attempt, Event: EventNodeStarted, Time: ts})
		s, ts = next()
		records = append(records, Record{
			Seq: s, Node: node, Attempt: attempt, Event: EventNodeFinished, Time: ts,
			DurationMS: dur, Status: status, Next: nextNode,
			Usage: &Usage{InputTokens: int64(100 * attempt), OutputTokens: int64(40 * attempt), USD: 0.001 * float64(attempt)},
		})
	}

	s, ts := next()
	records = append(records, Record{Seq: s, Event: EventRunStarted, Time: ts})

	nodeVisit("plan", 1, StatusOK, 10, "approve")
	nodeVisit("approve", 1, StatusOK, 20, "code")
	nodeVisit("code", 1, StatusOK, 30, "write")
	nodeVisit("write", 1, StatusOK, 40, "test")
	nodeVisit("test", 1, StatusFailed, 100, "fix")
	nodeVisit("fix", 1, StatusOK, 50, "test")
	nodeVisit("test", 1, StatusFailed, 200, "fix")
	nodeVisit("fix", 1, StatusOK, 60, "test")
	nodeVisit("test", 1, StatusOK, 300, "review")
	nodeVisit("review", 1, StatusOK, 70, "")

	s, ts = next()
	records = append(records, Record{Seq: s, Event: EventRunCompleted, Time: ts})

	return records
}

// marshalNDJSON renders records exactly as Journal.Append would have
// written them: one json.Marshal per line, each terminated by "\n".
func marshalNDJSON(t *testing.T, records []Record) []byte {
	t.Helper()

	var buf []byte
	for _, rec := range records {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshaling fixture record seq=%d: %v", rec.Seq, err)
		}
		buf = append(buf, data...)
		buf = append(buf, '\n')
	}
	return buf
}
