//go:build unix

package linter

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/dhaam-ai/belay/internal/detect"
	"github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// ruffName is the adapter's Name, and so every report's Source.
const ruffName = "ruff"

// ruffLocalBins are the conventional virtualenv locations for a project's own
// ruff, tried before PATH for the same reason ESLint prefers
// node_modules/.bin: the repository pins the version its rules were written
// against.
var ruffLocalBins = []string{".venv/bin/ruff", "venv/bin/ruff"}

// ruffEnv names the parent variables ruff needs beyond exec.BaseEnvNames.
// ruff never imports the code it checks, so it needs almost nothing: an
// active virtualenv to resolve a project layout, and its own cache directory.
var ruffEnv = []string{"RUFF_CACHE_DIR", "VIRTUAL_ENV"}

// ruffRule is one row of the ruff severity table.
type ruffRule struct {
	// Prefix is a rule-code prefix such as "F", "E9" or "S101". Matching
	// requires the letters to be equal and the digits to be a prefix, so
	// "I" covers I001 but not ISC001.
	Prefix string
	// Severity is the belay.Severity codes under Prefix map to.
	Severity belay.Severity
	// Why records the rationale, in one sentence.
	Why string
}

// ruffSeverities maps ruff rule codes to belay severities.
//
// # Why the code and not the severity field
//
// ruff's JSON output does carry a "severity" key, and it is useless for this:
// outside preview mode the renderer hardcodes it. From ruff's own JSON
// renderer, the non-preview branch is literally
//
//	(Some(diagnostic.secondary_code_or_id()), Severity::Error)
//
// so every diagnostic in a stable run — an unused import and a hardcoded
// password alike — reports "severity": "error". Mapping through it would
// collapse the whole tool to one belay severity. The rule code is the only
// per-finding signal ruff actually varies, so it is the key. The severity
// string is consulted only when there is no code at all, which happens in
// preview mode where it is populated for real.
//
// # Matching
//
// Longest matching prefix wins, with letters compared exactly: "E999" matches
// "E9" over "E", and "S101" matches "S101" over "S", but "ISC001" does not
// match "I" — its letters are "ISC". That exactness matters, because ruff has
// over fifty rule sets whose codes share leading letters.
//
// # Where the line is drawn
//
// Major and above fails the default gate. Note what that means for the
// opt-in rule sets: ruff enables only E4, E7, E9 and F by default, so a
// repository that selects S (flake8-bandit) has explicitly asked to be gated
// on security, which is why S sits at Critical.
var ruffSeverities = []ruffRule{
	// Blocker: the file was never analysed.
	{"E9", belay.SeverityBlocker, "a syntax or I/O error (E902, E999): ruff could not parse the file, so nothing in it was checked"},

	// Critical: near-certain runtime failure, or an opted-into security
	// rule set.
	{"F82", belay.SeverityCritical, "an undefined name (F821-F823) is a NameError waiting for that line to execute"},
	{"S", belay.SeverityCritical, "a flake8-bandit security finding, from a rule set the repository had to opt into"},

	// Major: real defects.
	{"F", belay.SeverityMajor, "pyflakes: genuine defects such as duplicate arguments, unreachable code and broken f-strings"},
	{"B", belay.SeverityMajor, "flake8-bugbear: legal constructs that are almost always wrong, such as a mutable default argument"},
	{"ASYNC", belay.SeverityMajor, "a blocking call inside async code stalls the entire event loop"},
	{"PLE", belay.SeverityMajor, "pylint's error category: code pylint considers outright broken"},
	{"W6", belay.SeverityMajor, "a deprecation or invalid escape sequence (W605), which newer Python escalates to a SyntaxWarning"},
	{"T10", belay.SeverityMajor, "a debugger import or breakpoint() left behind will hang the process in production"},

	// Minor: worth fixing, not worth stopping a run for. The four
	// demotions here are carve-outs from the families above.
	{"F401", belay.SeverityMinor, "an unused import is untidy, not dangerous"},
	{"F841", belay.SeverityMinor, "an unused local variable is untidy, not dangerous"},
	{"S101", belay.SeverityMinor, "assert used: a testing idiom that fires on every line of a pytest suite, not a vulnerability"},
	{"E", belay.SeverityMinor, "pycodestyle: whitespace, indentation and line length"},
	{"W", belay.SeverityMinor, "pycodestyle warnings: trailing whitespace and blank lines"},
	{"C4", belay.SeverityMinor, "flake8-comprehensions: a clearer way to write the same thing"},
	{"C90", belay.SeverityMinor, "mccabe complexity, a budget the repository chose"},
	{"SIM", belay.SeverityMinor, "flake8-simplify: a shorter equivalent of working code"},
	{"N", belay.SeverityMinor, "pep8-naming"},
	{"UP", belay.SeverityMinor, "pyupgrade: modernisation, not correctness"},
	{"RUF", belay.SeverityMinor, "ruff's own rules: a mix, dominated by lint hygiene"},
	{"PLR", belay.SeverityMinor, "pylint refactor suggestions"},
	{"PLW", belay.SeverityMinor, "pylint warnings"},
	{"PLC", belay.SeverityMinor, "pylint conventions"},
	{"PT", belay.SeverityMinor, "flake8-pytest-style: test-suite conventions"},

	// Info: mechanically fixable by ruff itself.
	{"I", belay.SeverityInfo, "isort import ordering, fixed mechanically by ruff check --fix"},
	{"D", belay.SeverityInfo, "pydocstyle: docstring presence and formatting"},
	{"Q", belay.SeverityInfo, "flake8-quotes: quote style, fixed mechanically by the formatter"},
	{"COM", belay.SeverityInfo, "flake8-commas: trailing commas, fixed mechanically by the formatter"},
}

// ruffSeverityStrings maps ruff's Severity enum, used only for a diagnostic
// that carries no code at all — ruff's preview output makes the code optional,
// and populates the severity honestly in exchange.
var ruffSeverityStrings = map[string]mapping{
	"fatal":   {belay.SeverityBlocker, "ruff's own worst level; analysis stopped"},
	"error":   {belay.SeverityMajor, "ruff's ordinary verdict for a violation"},
	"warning": {belay.SeverityMinor, "ruff explicitly ranked this below an error"},
	"info":    {belay.SeverityInfo, "ruff explicitly ranked this as informational"},
}

// defaultRuffSeverity is the severity for a code no row matches.
//
// Minor, for the same reason as golangci-lint's default: ruff ships over eight
// hundred rules across more than fifty rule sets, and a repository that
// selects a new one must not discover it by having every belay run hard-stop
// on a code belay has never seen. The finding is still parsed, counted and
// reported.
var defaultRuffSeverity = mapping{belay.SeverityMinor, "an unrecognized rule code; reported and counted, but not trusted to stop a run"}

// ruffFallbackRuleID names a diagnostic that carries neither a code nor a
// rule name.
const ruffFallbackRuleID = "ruff"

// Ruff lints Python projects with ruff.
//
// It prefers a project's virtualenv-local ruff over a global one and never
// reaches for a package runner, which would download and execute code from
// the network as a side effect of running a quality gate.
type Ruff struct {
	base
}

// NewRuff returns a Ruff adapter. With no options it gates at DefaultFailOn
// and runs commands through internal/exec.
func NewRuff(opts ...Option) *Ruff {
	return &Ruff{base: newBase(opts)}
}

var _ belay.Linter = (*Ruff)(nil)

// Name implements belay.Linter.
func (r *Ruff) Name() string { return ruffName }

// Detect implements belay.Linter. It reports whether internal/detect found a
// Python project under dir that declares or configures ruff.
func (r *Ruff) Detect(dir string) bool {
	for _, project := range projects(dir) {
		if project.Kind == detect.KindPython && project.Python != nil && project.Python.HasRuff {
			return true
		}
	}
	return false
}

// Lint implements belay.Linter.
//
// Findings are a successful call: ruff documents exit 1 as "violations were
// found", which returns a GateFail report and a nil error. Exit 2, "ruff
// terminates abnormally due to invalid configuration, invalid CLI options, or
// an internal error", leaves stdout empty and surfaces as a GateError report
// and an *OutputError.
func (r *Ruff) Lint(ctx context.Context, dir string) (belay.QualityReport, error) {
	cmd := exec.Command{
		Path: localTool(dir, ruffName, ruffLocalBins...),
		// "." is ruff's own default path argument, passed explicitly.
		Args:      []string{"check", "--output-format", "json", "."},
		Dir:       dir,
		EnvAllow:  ruffEnv,
		Timeout:   r.timeout,
		MaxOutput: DefaultMaxOutput,
	}
	return r.lint(ctx, ruffName, ruffName, cmd, ruffParser(dir))
}

// ruffDiagnostic is one element of the array ruff's json output emits.
//
// Only the fields belay uses are modelled. Older ruff releases omit name and
// severity entirely, which decode to empty strings and change nothing: the
// rule code is the signal.
type ruffDiagnostic struct {
	// Code is the rule code, such as "F401". Null in preview mode.
	Code string `json:"code"`
	// Name is the rule's long name, such as "unused-import".
	Name string `json:"name"`
	// Severity is ruff's own level, hardcoded to "error" outside preview.
	Severity string `json:"severity"`
	// Filename is the absolute path to the offending file.
	Filename string `json:"filename"`
	// Message is the human-readable description.
	Message string `json:"message"`
	// Location is the start of the violation. Null when the diagnostic is
	// not attributable to one position.
	Location *ruffLocation `json:"location"`
}

// ruffLocation is a one-indexed source position.
type ruffLocation struct {
	Row    int `json:"row"`
	Column int `json:"column"`
}

// ruffParser returns the parser for ruff's json output, closing over dir
// because ruff reports absolute file paths.
func ruffParser(dir string) parser {
	return func(stdout []byte) ([]belay.Issue, json.RawMessage, error) {
		return parseRuff(dir, stdout)
	}
}

// parseRuff turns ruff's json output into issues.
func parseRuff(dir string, stdout []byte) ([]belay.Issue, json.RawMessage, error) {
	var diagnostics []ruffDiagnostic
	raw, err := decodeFirstJSON(stdout, &diagnostics)
	if err != nil {
		return nil, nil, err
	}
	issues := make([]belay.Issue, 0, len(diagnostics))
	for _, d := range diagnostics {
		severity, _ := ruffSeverity(d.Code, d.Severity)
		line := 0
		if d.Location != nil {
			line = d.Location.Row
		}
		issues = append(issues, belay.Issue{
			RuleID:   ruffRuleID(d),
			Severity: severity,
			File:     relPath(dir, d.Filename),
			Line:     line,
			Message:  strings.TrimSpace(d.Message),
		})
	}
	return issues, raw, nil
}

// ruffSeverity resolves one diagnostic's severity and the rationale behind it.
//
// The code decides whenever there is one. ruff's severity string is consulted
// only for a diagnostic with no code, because in the output belay actually
// receives it is a constant.
func ruffSeverity(code, severity string) (belay.Severity, string) {
	normalized := strings.ToUpper(strings.TrimSpace(code))
	if normalized != "" {
		if rule, ok := matchRuffCode(normalized); ok {
			return rule.Severity, rule.Why
		}
		return defaultRuffSeverity.Severity, defaultRuffSeverity.Why
	}
	if m, ok := ruffSeverityStrings[strings.ToLower(strings.TrimSpace(severity))]; ok {
		return m.Severity, m.Why
	}
	return defaultRuffSeverity.Severity, defaultRuffSeverity.Why
}

// matchRuffCode finds the longest row matching code: equal letters, and digits
// that prefix the code's digits.
func matchRuffCode(code string) (ruffRule, bool) {
	letters, digits := splitRuffCode(code)
	best := ruffRule{}
	bestDigits := -1
	for _, rule := range ruffSeverities {
		ruleLetters, ruleDigits := splitRuffCode(rule.Prefix)
		if ruleLetters != letters || !strings.HasPrefix(digits, ruleDigits) {
			continue
		}
		if len(ruleDigits) > bestDigits {
			best, bestDigits = rule, len(ruleDigits)
		}
	}
	return best, bestDigits >= 0
}

// splitRuffCode splits a rule code into its letter and digit halves, so
// "PLR0913" becomes "PLR" and "0913".
func splitRuffCode(code string) (letters, digits string) {
	i := 0
	for i < len(code) && (code[i] < '0' || code[i] > '9') {
		i++
	}
	return code[:i], code[i:]
}

// ruffRuleID names the rule that fired, preferring the code and falling back
// to the long name that preview output carries instead.
func ruffRuleID(d ruffDiagnostic) string {
	if code := strings.TrimSpace(d.Code); code != "" {
		return code
	}
	if name := strings.TrimSpace(d.Name); name != "" {
		return name
	}
	return ruffFallbackRuleID
}
