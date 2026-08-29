// Package bugs is the seeded-bug defect catalogue: a registry of known,
// hand-verified defects that inject.go can apply to a copy of ../src.
//
// Each bNN_*.go file in this package registers exactly one Defect via
// init(). Nothing outside this package enumerates them by hand: All
// returns every registered defect, sorted by ID, so adding a new bNN
// file is enough to add it to the catalogue.
package bugs

import (
	"fmt"
	"sort"
	"strings"
)

// Channel identifies how a defect is expected to be caught.
type Channel string

const (
	// DetectedByTest means the defect makes one or more of the target
	// package's own tests fail (or panic), and no linter is expected to
	// flag it.
	DetectedByTest Channel = "test"
	// DetectedByLint means a linter flags the defect, but the target
	// package's existing tests still all pass unchanged.
	DetectedByLint Channel = "lint"
	// DetectedByBoth means both a test and a linter catch the defect.
	DetectedByBoth Channel = "both"
)

// Defect is one catalogued, hand-verified mistake: an exact text
// transformation applied to a single file in a copy of src/.
type Defect struct {
	// ID is the defect's stable identifier, e.g. "B01". IDs are matched
	// case-sensitively by --bugs and must be unique across the
	// catalogue.
	ID string
	// Class is the bug taxonomy this defect belongs to, e.g.
	// "off-by-one" or "concurrency". See the README for the full list.
	Class string
	// File is the target file's path relative to src/, e.g.
	// "policy.go".
	File string
	// Symbol is the function, method, or declaration the defect
	// targets.
	Symbol string
	// Description is a one-line account of the real-world mistake this
	// defect models.
	Description string
	// Find is the exact, byte-for-byte text this defect replaces. It
	// must appear in File exactly once; inject.go refuses to apply a
	// Defect whose Find is missing or ambiguous in the file it targets.
	Find string
	// Replace is the text that takes Find's place. It may be shorter,
	// longer, or the same length as Find.
	Replace string
	// BreaksTests lists the target package's test names, in the form
	// go test -v prints them (e.g.
	// "TestPolicy_ShouldRetry/stops_at_max_attempts"), that this defect
	// is expected to make fail when applied alone. Empty for a
	// lint-only defect.
	BreaksTests []string
	// DetectedBy records which channel(s) are expected to catch this
	// defect.
	DetectedBy Channel
	// Linter names the golangci-lint linter expected to flag this
	// defect, required when DetectedBy is DetectedByLint or
	// DetectedByBoth.
	Linter string
	// FlakyDetection marks a defect whose BreaksTests failure is only
	// probabilistically observed: a genuine data race's detection
	// depends on the actual goroutine interleaving -race happens to
	// observe on a given run, not merely on the bug being present, so a
	// single clean run is not proof the defect is miscatalogued. Verify
	// these by retrying rather than by treating one miss as failure.
	// False for every non-concurrency defect, where a single miss does
	// mean the catalogue entry is wrong.
	FlakyDetection bool
}

var registry []Defect

// register adds d to the catalogue. It panics on a duplicate ID, since
// that is always an authoring mistake in this package, never a runtime
// condition callers should handle.
func register(d Defect) {
	for _, existing := range registry {
		if existing.ID == d.ID {
			panic(fmt.Sprintf("bugs: duplicate defect ID %q (already registered for %s)", d.ID, existing.Symbol))
		}
	}
	registry = append(registry, d)
}

// All returns every registered defect, sorted by ID.
func All() []Defect {
	out := make([]Defect, len(registry))
	copy(out, registry)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ByID returns the defect with the given ID, and whether it was found.
func ByID(id string) (Defect, bool) {
	for _, d := range registry {
		if d.ID == id {
			return d, true
		}
	}
	return Defect{}, false
}

// Classes returns the distinct Class values present in the catalogue,
// sorted alphabetically.
func Classes() []string {
	seen := make(map[string]bool)
	for _, d := range registry {
		seen[d.Class] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Validate reports every internal-consistency problem with d: missing
// required fields, a Find/Replace pair that can't possibly be a real
// edit, or a DetectedBy value whose supporting fields (BreaksTests,
// Linter) don't match what that channel requires.
func (d Defect) Validate() []error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("defect %s: "+format, append([]any{d.ID}, args...)...))
	}

	if d.ID == "" {
		fail("ID is empty")
	}
	if d.Class == "" {
		fail("Class is empty")
	}
	if d.File == "" {
		fail("File is empty")
	}
	if strings.Contains(d.File, "..") {
		fail("File %q must not contain \"..\"", d.File)
	}
	if d.Symbol == "" {
		fail("Symbol is empty")
	}
	if d.Description == "" {
		fail("Description is empty")
	}
	if d.Find == "" {
		fail("Find is empty")
	}
	if d.Find == d.Replace {
		fail("Find and Replace are identical; this defect would be a no-op")
	}

	switch d.DetectedBy {
	case DetectedByTest:
		if len(d.BreaksTests) == 0 {
			fail("DetectedBy is %q but BreaksTests is empty", d.DetectedBy)
		}
		if d.Linter != "" {
			fail("DetectedBy is %q but Linter is set to %q", d.DetectedBy, d.Linter)
		}
	case DetectedByLint:
		if len(d.BreaksTests) != 0 {
			fail("DetectedBy is %q but BreaksTests is non-empty: %v", d.DetectedBy, d.BreaksTests)
		}
		if d.Linter == "" {
			fail("DetectedBy is %q but Linter is empty", d.DetectedBy)
		}
	case DetectedByBoth:
		if len(d.BreaksTests) == 0 {
			fail("DetectedBy is %q but BreaksTests is empty", d.DetectedBy)
		}
		if d.Linter == "" {
			fail("DetectedBy is %q but Linter is empty", d.DetectedBy)
		}
	default:
		fail("DetectedBy is %q, want %q, %q, or %q", d.DetectedBy, DetectedByTest, DetectedByLint, DetectedByBoth)
	}

	return errs
}

// ValidateAll runs Validate across every registered defect and also
// checks catalogue-wide invariants (currently: ID uniqueness, which
// register already enforces at load time). It is meant for tests and for
// inject.go's own startup self-check.
func ValidateAll() []error {
	var errs []error
	for _, d := range All() {
		errs = append(errs, d.Validate()...)
	}
	return errs
}
