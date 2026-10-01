package journal

import (
	"encoding/json"
	"fmt"

	"github.com/dhaam-ai/belay/internal/ndjson"
)

// ErrExists reports that Create found something already at the path it was
// asked to write a journal to, and so wrote nothing. It is ndjson.ErrExists
// itself, as os.ErrNotExist is fs.ErrNotExist, so a caller matches it under
// either name without importing the package that does the writing.
var ErrExists = ndjson.ErrExists

// Create writes records as a new journal at path, whole or not at all, and
// refuses with an error wrapping ErrExists if anything is already there.
//
// It exists for a journal that begins with history rather than with an empty
// file: a fork's, which starts as a copy of the records its source run made
// before the visit the fork re-runs. Writing those one at a time through Open
// and Append would hold until a crash part-way, which would leave a journal
// holding part of the inherited history, a well-formed prefix that
// ResolveStart would then resume from as if it were all of it. Create hands
// the whole history to ndjson.CreateNew, which makes the file appear complete
// or not at all.
//
// It refuses rather than replaces, because a journal is a run's record of
// what it did, and writing over one would destroy the evidence a fork exists
// to leave alone. The refusal is atomic: of two processes creating the same
// journal, exactly one writes it.
//
// records must already be a journal. Every record must be one Append would
// have accepted, and together they must pass Validate: Seqs strictly
// increasing from 1, every node execution paired. Both are checked before
// anything is written. Create then writes the records exactly as given. It
// assigns no Seq and fills in no time, unlike Append, because what it is given
// is a history, and a history's Seqs and times are facts about when things
// happened, not about when they were copied.
func Create(path string, records []Record) error {
	lines := make([][]byte, 0, len(records))
	for _, rec := range records {
		if err := rec.validate(); err != nil {
			return fmt.Errorf("journal: create %s: seq %d: %w", path, rec.Seq, err)
		}
		data, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("journal: create %s: encode seq %d: %w", path, rec.Seq, err)
		}
		lines = append(lines, data)
	}
	if err := Validate(records); err != nil {
		return fmt.Errorf("journal: create %s: %w", path, err)
	}
	if err := ndjson.CreateNew(path, lines, 0o600); err != nil {
		return fmt.Errorf("journal: create %s: %w", path, err)
	}
	return nil
}
