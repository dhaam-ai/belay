package journal

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestCreateWritesExactlyTheRecords: what Create writes reads back as the
// same records, byte for byte the lines Append would have written.
func TestCreateWritesExactlyTheRecords(t *testing.T) {
	t.Parallel()

	sample := buildSampleRun(t)
	// A history that ends inside a node is a valid journal: it is what a
	// crash mid-node leaves, and what a resume heals.
	openAtEnd := sample[:4]

	tests := []struct {
		name    string
		records []Record
	}{
		{name: "a whole run", records: sample},
		{name: "a history that ends inside a node", records: openAtEnd},
		{name: "no records at all", records: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "journal.ndjson")
			if err := Create(path, tt.records); err != nil {
				t.Fatalf("Create() error = %v", err)
			}

			// #nosec G304 -- path is under the test's own temp directory.
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if want := marshalNDJSON(t, tt.records); !bytes.Equal(raw, want) {
				t.Errorf("file =\n%s\nwant the lines Append would have written:\n%s", raw, want)
			}
			read, err := ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if read.Incomplete {
				t.Error("ReadFile().Incomplete = true, want a clean file")
			}
			if diff := cmp.Diff(tt.records, read.Records); diff != "" {
				t.Errorf("records read back (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCreateRefusesAndWritesNothing: every refusal leaves the path exactly as
// it was, which for a history Create will not write means no file at all.
func TestCreateRefusesAndWritesNothing(t *testing.T) {
	t.Parallel()

	sample := buildSampleRun(t)
	gap := append([]Record(nil), sample[:3]...)
	gap[2].Seq = 4
	late := append([]Record(nil), sample[1:3]...) // starts at seq 2
	orphan := []Record{sample[0], {Seq: 2, Node: "plan", Attempt: 1, Event: EventNodeFinished, Status: StatusOK, Time: sample[0].Time}}
	noStatus := []Record{sample[0], sample[1], {Seq: 3, Node: "plan", Attempt: 1, Event: EventNodeFinished, Time: sample[0].Time}}
	unknown := []Record{{Seq: 1, Event: EventUnknown, Time: sample[0].Time}}

	tests := []struct {
		name     string
		existing *string // nil: nothing is at the path beforehand
		records  []Record
		wantErr  error // nil: any error
	}{
		{name: "a journal already there", existing: ptrTo("{\"seq\":1}\n"), records: sample, wantErr: ErrExists},
		{name: "an empty file already there", existing: ptrTo(""), records: sample, wantErr: ErrExists},
		{name: "a gap in seq", records: gap, wantErr: ErrCorruptJournal},
		{name: "seq that does not start at 1", records: late, wantErr: ErrCorruptJournal},
		{name: "a finish with no start", records: orphan, wantErr: ErrCorruptJournal},
		// Validate checks the order of a history, not each record's shape.
		// Create must never write a record Append would refuse.
		{name: "a node_finished with no status", records: noStatus},
		{name: "an event that is no event", records: unknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "journal.ndjson")
			if tt.existing != nil {
				if err := os.WriteFile(path, []byte(*tt.existing), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}

			err := Create(path, tt.records)
			if err == nil {
				t.Fatal("Create() error = nil, want a refusal")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("Create() error = %v, want it to wrap %v", err, tt.wantErr)
			}

			// #nosec G304 -- path is under the test's own temp directory.
			got, err := os.ReadFile(path)
			if tt.existing == nil {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("ReadFile() error = %v, want no file: a refused history is not written at all", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if string(got) != *tt.existing {
				t.Errorf("file = %q after a refusal, want it untouched: %q", got, *tt.existing)
			}
		})
	}
}

// TestCreatedJournalOpensAndContinues: a journal Create wrote is an ordinary
// journal. Open takes it as it is, with nothing to heal, and the next Append
// follows on from its last record.
func TestCreatedJournalOpensAndContinues(t *testing.T) {
	t.Parallel()

	sample := buildSampleRun(t)
	prefix := sample[:9] // ends with code's node_finished
	path := filepath.Join(t.TempDir(), "journal.ndjson")
	if err := Create(path, prefix); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if j.RecoveredTornTail() {
		t.Error("RecoveredTornTail() = true, want false: Create leaves nothing to heal")
	}
	if got, want := j.LastSeq(), uint64(len(prefix)); got != want {
		t.Errorf("LastSeq() = %d, want %d", got, want)
	}
	rec, err := j.Append(Record{Event: EventRunStarted, Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if want := uint64(len(prefix) + 1); rec.Seq != want {
		t.Errorf("appended Seq = %d, want %d", rec.Seq, want)
	}
}

func ptrTo(s string) *string { return &s }
