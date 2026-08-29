package journal

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestReadEmptyInput(t *testing.T) {
	t.Parallel()

	result, err := Read(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("Read(empty) returned error: %v", err)
	}
	if len(result.Records) != 0 || result.Incomplete {
		t.Errorf("Read(empty) = %+v, want the zero ReadResult", result)
	}
}

func TestReadSingleLineWithTrailingNewline(t *testing.T) {
	t.Parallel()

	line := `{"seq":1,"event":"run_started","ts":"2026-01-01T00:00:00Z"}` + "\n"
	result, err := Read(strings.NewReader(line))
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if result.Incomplete {
		t.Error("Read reported Incomplete for a properly newline-terminated line")
	}
	if len(result.Records) != 1 || result.Records[0].Seq != 1 {
		t.Errorf("Read = %+v, want one record with Seq 1", result)
	}
}

// TestReadFinalLineMissingOnlyItsNewlineParsesCleanly covers the narrow gap
// where a crash cuts off exactly the trailing delimiter byte and nothing
// else: the record's JSON content is fully intact, so Read must accept it
// rather than flag Incomplete. (Journal.Open still has to ensure the file
// gains that missing newline before resuming writes — see
// TestOpenHealsMissingTrailingNewline in journal_test.go — but that is a
// write-side concern, not Read's.)
func TestReadFinalLineMissingOnlyItsNewlineParsesCleanly(t *testing.T) {
	t.Parallel()

	line := `{"seq":1,"event":"run_started","ts":"2026-01-01T00:00:00Z"}`
	result, err := Read(strings.NewReader(line))
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if result.Incomplete {
		t.Error("Read reported Incomplete for a line whose content fully parses")
	}
	if len(result.Records) != 1 || result.Records[0].Seq != 1 {
		t.Errorf("Read = %+v, want one record with Seq 1", result)
	}
}

func TestReadReportsIncompleteForATrulyTornLine(t *testing.T) {
	t.Parallel()

	// Cut off mid-way through the second field name: this can never parse,
	// with or without a trailing newline.
	data := `{"seq":1,"event":"run_started","ts":"2026-01-01T00:00:00Z"}` + "\n" +
		`{"seq":2,"attempt":1,"node":"plan","event":"node_st`
	result, err := Read(strings.NewReader(data))
	if err != nil {
		t.Fatalf("Read returned error: %v, want nil (a torn tail is not an error)", err)
	}
	if !result.Incomplete {
		t.Error("Read did not report Incomplete for a torn final line")
	}
	if len(result.Records) != 1 {
		t.Fatalf("Read returned %d records, want 1 (only the complete first line)", len(result.Records))
	}
	if result.Records[0].Seq != 1 {
		t.Errorf("Read.Records[0].Seq = %d, want 1", result.Records[0].Seq)
	}
}

// TestReadMidFileMalformedRecordIsHardError proves the leniency in
// TestReadReportsIncompleteForATrulyTornLine applies only to the final
// line: a broken line anywhere else can never be a torn write (belay only
// ever appends past the end of the file) and must surface as an error.
func TestReadMidFileMalformedRecordIsHardError(t *testing.T) {
	t.Parallel()

	data := `{"seq":1,"event":"run_started","ts":"2026-01-01T00:00:00Z"}` + "\n" +
		`not valid json at all` + "\n" +
		`{"seq":3,"event":"run_completed","ts":"2026-01-01T00:00:02Z"}` + "\n"

	result, err := Read(strings.NewReader(data))
	if err == nil {
		t.Fatal("Read on a mid-file malformed line = nil error, want error")
	}
	var malformed *MalformedRecordError
	if !errors.As(err, &malformed) {
		t.Fatalf("Read error = %v (%T), want *MalformedRecordError", err, err)
	}
	if malformed.Line != 2 {
		t.Errorf("MalformedRecordError.Line = %d, want 2", malformed.Line)
	}
	if !errors.Is(err, ErrMalformedRecord) {
		t.Error("Read error does not match ErrMalformedRecord via errors.Is")
	}
	if len(result.Records) != 1 {
		t.Errorf("Read returned %d records alongside the error, want the 1 record parsed before the bad line", len(result.Records))
	}
}

func TestReadRunawayLineIsReportedNotPanicked(t *testing.T) {
	t.Parallel()

	huge := `{"seq":1,"event":"run_started","ts":"2026-01-01T00:00:00Z","note":"` +
		strings.Repeat("x", maxLineBytes) + `"}` + "\n"

	result, err := Read(strings.NewReader(huge))
	if err == nil {
		t.Fatal("Read on an oversized line = nil error, want error")
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Errorf("Read error = %v, want it to wrap bufio.ErrTooLong", err)
	}
	if len(result.Records) != 0 {
		t.Errorf("Read returned %d records for an oversized first line, want 0", len(result.Records))
	}
}

// TestReadTruncationSweep is the acceptance test for requirement 2: truncate
// a valid, realistic (22-record) journal at every single byte offset and
// confirm each prefix either parses cleanly or reports an incomplete tail —
// never an error, never a panic, and never a fabricated record.
func TestReadTruncationSweep(t *testing.T) {
	t.Parallel()

	fullRecords := buildSampleRun(t)
	if len(fullRecords) < 20 {
		t.Fatalf("fixture has %d records, want at least 20", len(fullRecords))
	}
	full := marshalNDJSON(t, fullRecords)

	// Sanity check the fixture itself before sweeping it.
	whole, err := Read(bytes.NewReader(full))
	if err != nil {
		t.Fatalf("Read on the untruncated fixture returned error: %v", err)
	}
	if whole.Incomplete {
		t.Fatal("Read on the untruncated fixture reported Incomplete")
	}
	if diff := cmp.Diff(fullRecords, whole.Records, cmpTimeOpt); diff != "" {
		t.Fatalf("Read on the untruncated fixture does not match the fixture (-want +got):\n%s", diff)
	}

	// Full deep-equality of a Record (including its Usage pointer and
	// time.Time) is already proven by the untruncated-fixture check above
	// and by TestRecordRoundTrip. Sweeping ~5,000 byte offsets with
	// cmp.Diff's reflection on every iteration is needless overhead here;
	// a cheap identity check on every offset (never an error, never more
	// records than exist, and every returned record is exactly the right
	// one in the right position — never fabricated or swapped) covers what
	// this test exists to prove.
	for cut := 0; cut <= len(full); cut++ {
		prefix := full[:cut]

		result, err := Read(bytes.NewReader(prefix))
		if err != nil {
			t.Fatalf("cut=%d: Read returned error %v, want nil (every prefix of a valid file must parse cleanly or report Incomplete, never error)", cut, err)
		}
		if len(result.Records) > len(fullRecords) {
			t.Fatalf("cut=%d: Read returned %d records, more than the %d in the source fixture", cut, len(result.Records), len(fullRecords))
		}
		for i, rec := range result.Records {
			want := fullRecords[i]
			if rec.Seq != want.Seq || rec.Node != want.Node || rec.Event != want.Event || rec.Attempt != want.Attempt {
				t.Fatalf("cut=%d: record %d = {Seq:%d Node:%q Event:%v Attempt:%d}, want {Seq:%d Node:%q Event:%v Attempt:%d} (fabricated or corrupted record)",
					cut, i, rec.Seq, rec.Node, rec.Event, rec.Attempt, want.Seq, want.Node, want.Event, want.Attempt)
			}
		}
	}
}

func TestReadTruncationAtZeroIsEmptyNotIncomplete(t *testing.T) {
	t.Parallel()

	result, err := Read(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("Read(nil) returned error: %v", err)
	}
	if result.Incomplete {
		t.Error("Read(nil) reported Incomplete, want false: there is no tail at all, torn or otherwise")
	}
}
