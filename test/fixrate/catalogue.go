//go:build fixrate

package fixrate

// This file mirrors fixtures/seeded-bug's injected.json schema, documented
// in fixtures/seeded-bug/README.md under "injected.json schema". It is a
// deliberate, hand-kept duplicate of that shape rather than an import: see
// doc.go for why this package never imports belay.dev/fixtures/seededbug.
// A schema drift between the two shows up as a JSON decode error the first
// time Run parses a real injected.json, which is a loud, immediate failure
// rather than a silent misread.

// InjectedManifest is fixtures/seeded-bug/inject.go's injected.json,
// exactly as it writes it.
type InjectedManifest struct {
	GeneratedAt  string            `json:"generated_at"`
	Src          string            `json:"src"`
	Out          string            `json:"out"`
	Selection    InjectedSelection `json:"selection"`
	Defects      []InjectedDefect  `json:"defects"`
	Verification InjectedVerify    `json:"verification"`
}

// InjectedSelection is injected.json's "selection" object.
type InjectedSelection struct {
	Mode   string   `json:"mode"` // "bugs" or "random"
	Bugs   []string `json:"bugs,omitempty"`
	Random int      `json:"random,omitempty"`
	Seed   *int64   `json:"seed,omitempty"`
}

// InjectedDefect is one entry of injected.json's "defects" array: a
// catalogued defect that was actually applied to this copy.
type InjectedDefect struct {
	ID                   string   `json:"id"`
	Class                string   `json:"class"`
	File                 string   `json:"file"`
	Symbol               string   `json:"symbol"`
	Description          string   `json:"description"`
	Line                 int      `json:"line"`
	DetectedBy           string   `json:"detected_by"` // "test", "lint", or "both"
	Linter               string   `json:"linter,omitempty"`
	ExpectedFailingTests []string `json:"expected_failing_tests,omitempty"`
}

// InjectedVerify is injected.json's "verification" object: inject.go's own
// self-check that the copy behaves exactly as its defects say it should,
// computed BEFORE belay (or this harness) ever touches the copy. Run uses
// ActualFailingTests as its pre-run baseline rather than recomputing one,
// per the README's own guidance: "a T42-style harness computing its own
// fix-rate number ... should re-run this same command and compare against
// this same field, not re-derive it from scratch."
type InjectedVerify struct {
	BuildOK                bool     `json:"build_ok"`
	ExpectedFailingTests   []string `json:"expected_failing_tests"`
	ActualFailingTests     []string `json:"actual_failing_tests"`
	MissingFailingTests    []string `json:"missing_failing_tests"`
	UnexpectedFailingTests []string `json:"unexpected_failing_tests"`
	LintAvailable          bool     `json:"lint_available"`
	LintExpectedDefects    []string `json:"lint_expected_defects,omitempty"`
	LintMatchedDefects     []string `json:"lint_matched_defects,omitempty"`
	LintUnmatchedDefects   []string `json:"lint_unmatched_defects,omitempty"`
}

// OK reports whether the injected copy behaved exactly as its defects'
// catalogue entries said it would — the same predicate inject.go's own
// exit code is driven by. Run refuses to measure a fix rate against a
// copy that failed this check: a fixture that does not behave as
// catalogued cannot produce a meaningful diff.
func (v InjectedVerify) OK() bool {
	return v.BuildOK && len(v.MissingFailingTests) == 0 && len(v.UnexpectedFailingTests) == 0 &&
		(!v.LintAvailable || len(v.LintUnmatchedDefects) == 0)
}

// detectsByTest reports whether d's DetectedBy channel includes belay's
// own test suite ("test" or "both").
func (d InjectedDefect) detectsByTest() bool {
	return d.DetectedBy == "test" || d.DetectedBy == "both"
}

// detectsByLint reports whether d's DetectedBy channel includes a linter
// ("lint" or "both").
func (d InjectedDefect) detectsByLint() bool {
	return d.DetectedBy == "lint" || d.DetectedBy == "both"
}
