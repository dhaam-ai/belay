package journal

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// openTestJournal opens a fresh journal in a temp directory and arranges
// for it to be closed at the end of the test.
func openTestJournal(t *testing.T) (*Journal, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "run.ndjson")
	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q) returned error: %v", path, err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Errorf("Close returned error: %v", err)
		}
	})
	return j, path
}

func TestOpenCreatesFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "run.ndjson")

	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q) returned error: %v", path, err)
	}
	defer func() {
		if err := j.Close(); err != nil {
			t.Errorf("Close returned error: %v", err)
		}
	}()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("journal file was not created: %v", err)
	}
	if got := j.LastSeq(); got != 0 {
		t.Errorf("LastSeq() on a fresh journal = %d, want 0", got)
	}
	if j.RecoveredTornTail() {
		t.Error("RecoveredTornTail() on a fresh journal = true, want false")
	}
	if got := j.Path(); got != path {
		t.Errorf("Path() = %q, want %q", got, path)
	}
}

func TestAppendAssignsSequentialSeq(t *testing.T) {
	t.Parallel()

	j, _ := openTestJournal(t)

	for i, rec := range []Record{
		{Event: EventRunStarted},
		{Node: "plan", Attempt: 1, Event: EventNodeStarted},
		{Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "approve"},
	} {
		got, err := j.Append(rec)
		if err != nil {
			t.Fatalf("Append(#%d) returned error: %v", i, err)
		}
		wantSeq := uint64(i + 1)
		if got.Seq != wantSeq {
			t.Errorf("Append(#%d).Seq = %d, want %d", i, got.Seq, wantSeq)
		}
	}
	if got := j.LastSeq(); got != 3 {
		t.Errorf("LastSeq() = %d, want 3", got)
	}
}

func TestAppendFillsTimeWhenZeroAndPreservesExplicitTime(t *testing.T) {
	t.Parallel()

	j, _ := openTestJournal(t)

	before := time.Now().Add(-time.Second)
	got, err := j.Append(Record{Event: EventRunStarted})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	after := time.Now().Add(time.Second)
	if got.Time.Before(before) || got.Time.After(after) {
		t.Errorf("Append filled Time = %v, want between %v and %v", got.Time, before, after)
	}

	explicit := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	got2, err := j.Append(Record{Event: EventRunPaused, Time: explicit})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if !got2.Time.Equal(explicit) {
		t.Errorf("Append overwrote an explicit Time: got %v, want %v", got2.Time, explicit)
	}
}

func TestAppendDefaultsAttemptToOne(t *testing.T) {
	t.Parallel()

	j, _ := openTestJournal(t)

	got, err := j.Append(Record{Node: "plan", Event: EventNodeStarted})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if got.Attempt != 1 {
		t.Errorf("Append defaulted Attempt to %d, want 1", got.Attempt)
	}
}

func TestAppendRejectsInvalidRecordWithoutConsumingSeq(t *testing.T) {
	t.Parallel()

	j, _ := openTestJournal(t)

	_, err := j.Append(Record{Event: EventUnknown})
	if err == nil {
		t.Fatal("Append(invalid) = nil error, want error")
	}

	got, err := j.Append(Record{Event: EventRunStarted})
	if err != nil {
		t.Fatalf("Append(valid) returned error: %v", err)
	}
	if got.Seq != 1 {
		t.Errorf("Append(valid).Seq = %d, want 1 (the failed append must not consume a Seq)", got.Seq)
	}
}

func TestAppendPersistsDurably(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "run.ndjson")
	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}

	var want []Record
	for _, rec := range []Record{
		{Event: EventRunStarted},
		{Node: "plan", Attempt: 1, Event: EventNodeStarted},
		{
			Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "approve",
			DurationMS: 812, Usage: &Usage{InputTokens: 10, OutputTokens: 5, USD: 0.01},
			Note: "plan looks reasonable",
		},
	} {
		got, err := j.Append(rec)
		if err != nil {
			t.Fatalf("Append returned error: %v", err)
		}
		want = append(want, got)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	result, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if result.Incomplete {
		t.Error("ReadFile reported Incomplete for a cleanly closed journal")
	}
	if diff := cmp.Diff(want, result.Records, cmpTimeOpt); diff != "" {
		t.Errorf("records read back differ from what was appended (-want +got):\n%s", diff)
	}
}

// TestAppendConcurrentGoroutines is the acceptance test for requirement 4:
// 100 concurrent goroutines appending must produce 100 complete,
// well-formed, correctly-ordered lines, and must be -race clean.
func TestAppendConcurrentGoroutines(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "run.ndjson")
	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}

	const n = 100
	type outcome struct {
		rec Record
		err error
	}
	results := make([]outcome, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			rec, err := j.Append(Record{Event: EventRunStarted})
			results[i] = outcome{rec: rec, err: err}
		}(i)
	}
	wg.Wait()

	seen := make(map[uint64]bool, n)
	for i, o := range results {
		if o.err != nil {
			t.Fatalf("goroutine %d: Append returned error: %v", i, o.err)
		}
		if o.rec.Seq < 1 || o.rec.Seq > n {
			t.Fatalf("goroutine %d: Append returned Seq %d, want in [1,%d]", i, o.rec.Seq, n)
		}
		if seen[o.rec.Seq] {
			t.Fatalf("Seq %d was assigned to two different Append calls", o.rec.Seq)
		}
		seen[o.rec.Seq] = true
	}
	if len(seen) != n {
		t.Fatalf("saw %d distinct Seq values, want %d", len(seen), n)
	}

	if err := j.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	result, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if result.Incomplete {
		t.Fatal("ReadFile reported Incomplete after 100 concurrent appends")
	}
	if len(result.Records) != n {
		t.Fatalf("ReadFile returned %d records, want %d", len(result.Records), n)
	}
	for i, rec := range result.Records {
		wantSeq := uint64(i + 1)
		if rec.Seq != wantSeq {
			t.Errorf("record at file position %d has Seq %d, want %d (lines must be in Seq order)", i, rec.Seq, wantSeq)
		}
		if rec.Event != EventRunStarted {
			t.Errorf("record at file position %d has Event %v, want EventRunStarted", i, rec.Event)
		}
	}
	if err := Validate(result.Records); err != nil {
		t.Errorf("Validate on the concurrently-written journal returned error: %v", err)
	}
}

func TestOpenRecoversSeqAcrossReopen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "run.ndjson")

	j1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open returned error: %v", err)
	}
	for _, rec := range []Record{
		{Event: EventRunStarted},
		{Node: "plan", Attempt: 1, Event: EventNodeStarted},
		{Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "approve"},
	} {
		if _, err := j1.Append(rec); err != nil {
			t.Fatalf("Append returned error: %v", err)
		}
	}
	if err := j1.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	j2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open returned error: %v", err)
	}
	defer func() {
		if err := j2.Close(); err != nil {
			t.Errorf("Close returned error: %v", err)
		}
	}()

	if got := j2.LastSeq(); got != 3 {
		t.Errorf("LastSeq() after reopen = %d, want 3", got)
	}
	if j2.RecoveredTornTail() {
		t.Error("RecoveredTornTail() = true for a cleanly closed journal, want false")
	}

	got, err := j2.Append(Record{Node: "approve", Attempt: 1, Event: EventNodeStarted})
	if err != nil {
		t.Fatalf("Append after reopen returned error: %v", err)
	}
	if got.Seq != 4 {
		t.Errorf("Append after reopen got Seq %d, want 4", got.Seq)
	}
}

func TestOpenRefusesCorruptExistingJournal(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "run.ndjson")
	// Two records, both cleanly newline-terminated and individually valid
	// JSON, but both claiming Seq 1: a corrupt history Open must refuse
	// rather than silently build on top of.
	data := `{"seq":1,"event":"run_started","ts":"2026-01-01T00:00:00Z"}` + "\n" +
		`{"seq":1,"event":"run_completed","ts":"2026-01-01T00:00:01Z"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	_, err := Open(path)
	if err == nil {
		t.Fatal("Open on a journal with a duplicate seq = nil error, want error")
	}
	if !errors.Is(err, ErrCorruptJournal) {
		t.Errorf("Open error = %v, want it to wrap ErrCorruptJournal", err)
	}
}

func TestOpenRecoversAfterTornTail(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "run.ndjson")

	j1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open returned error: %v", err)
	}
	for _, rec := range []Record{
		{Event: EventRunStarted},
		{Node: "plan", Attempt: 1, Event: EventNodeStarted},
		{Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "approve"},
	} {
		if _, err := j1.Append(rec); err != nil {
			t.Fatalf("Append returned error: %v", err)
		}
	}
	if err := j1.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	// Simulate a process killed mid-write: append a syntactically broken,
	// un-terminated final line directly, bypassing Journal.Append.
	// #nosec G304 -- path is this test's own t.TempDir() fixture.
	raw, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening fixture for raw torn write: %v", err)
	}
	if _, err := raw.WriteString(`{"seq":4,"attempt":1,"node":"approve","event":"node_st`); err != nil {
		t.Fatalf("writing torn tail: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("closing torn fixture: %v", err)
	}

	// Confirm the fixture really is torn before exercising recovery.
	preCheck, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile on the torn fixture returned error: %v", err)
	}
	if !preCheck.Incomplete || len(preCheck.Records) != 3 {
		t.Fatalf("fixture setup is wrong: Incomplete=%v len(Records)=%d, want Incomplete=true len=3",
			preCheck.Incomplete, len(preCheck.Records))
	}

	j2, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a torn journal returned error: %v", err)
	}
	// j2 is closed explicitly below, once its Append calls are done and
	// before re-reading the file; no deferred close is needed too.

	if !j2.RecoveredTornTail() {
		t.Error("RecoveredTornTail() = false after recovering a torn tail, want true")
	}
	if got := j2.LastSeq(); got != 3 {
		t.Errorf("LastSeq() after recovery = %d, want 3 (the torn record must not count)", got)
	}

	if _, err := j2.Append(Record{Node: "approve", Attempt: 1, Event: EventNodeStarted}); err != nil {
		t.Fatalf("Append after recovery returned error: %v", err)
	}
	if _, err := j2.Append(Record{Node: "approve", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "code"}); err != nil {
		t.Fatalf("second Append after recovery returned error: %v", err)
	}
	if err := j2.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	final, err := ReadFile(path)
	if err != nil {
		t.Fatalf("final ReadFile returned error: %v", err)
	}
	if final.Incomplete {
		t.Error("final ReadFile reports Incomplete: recovery left the journal unreadable")
	}
	if len(final.Records) != 5 {
		t.Fatalf("final ReadFile returned %d records, want 5", len(final.Records))
	}
	if err := Validate(final.Records); err != nil {
		t.Errorf("Validate on the recovered-and-continued journal returned error: %v", err)
	}
	for i, rec := range final.Records {
		wantSeq := uint64(i + 1)
		if rec.Seq != wantSeq {
			t.Errorf("record %d has Seq %d, want %d", i, rec.Seq, wantSeq)
		}
	}
}

func TestOpenHealsMissingTrailingNewline(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "run.ndjson")
	// A crash landing exactly on the final newline byte: the last record's
	// content is fully intact and valid JSON, but the file does not end in
	// "\n". Read must accept it as complete (not Incomplete); Open must
	// still ensure the file is ready for another line to be appended after
	// it without the two merging.
	data := `{"seq":1,"event":"run_started","ts":"2026-01-01T00:00:00Z"}` + "\n" +
		`{"seq":2,"attempt":1,"node":"plan","event":"node_started","ts":"2026-01-01T00:00:01Z"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	preCheck, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile on the fixture returned error: %v", err)
	}
	if preCheck.Incomplete || len(preCheck.Records) != 2 {
		t.Fatalf("fixture setup is wrong: Incomplete=%v len(Records)=%d, want Incomplete=false len=2",
			preCheck.Incomplete, len(preCheck.Records))
	}

	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	// j is closed explicitly below, once its Append call is done and before
	// re-reading the file; no deferred close is needed too.

	if j.RecoveredTornTail() {
		t.Error("RecoveredTornTail() = true for a journal with no torn line, want false")
	}
	if got := j.LastSeq(); got != 2 {
		t.Fatalf("LastSeq() = %d, want 2", got)
	}

	if _, err := j.Append(Record{Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Next: "approve"}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	final, err := ReadFile(path)
	if err != nil {
		t.Fatalf("final ReadFile returned error: %v", err)
	}
	if final.Incomplete {
		t.Fatal("final ReadFile reports Incomplete: the missing delimiter was not healed, so the new record merged with the old one")
	}
	if len(final.Records) != 3 {
		t.Fatalf("final ReadFile returned %d records, want 3", len(final.Records))
	}
}

func TestReadFileOnMissingFileIsEmptyNotError(t *testing.T) {
	t.Parallel()

	result, err := ReadFile(filepath.Join(t.TempDir(), "does-not-exist.ndjson"))
	if err != nil {
		t.Fatalf("ReadFile on a missing file returned error: %v", err)
	}
	if len(result.Records) != 0 || result.Incomplete {
		t.Errorf("ReadFile on a missing file = %+v, want the zero ReadResult", result)
	}
}

func TestAppendAfterCloseReturnsErrorNotPanic(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "run.ndjson")
	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if _, err := j.Append(Record{Event: EventRunStarted}); err == nil {
		t.Error("Append after Close = nil error, want error")
	}
}
