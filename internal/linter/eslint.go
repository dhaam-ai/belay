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

// eslintName is the adapter's Name, and so every report's Source.
const eslintName = "eslint"

// eslintLocalBin is where npm, pnpm, yarn and bun all place a package's own
// eslint executable. It is tried before PATH, because a repository pins the
// ESLint version it lints with and a globally installed one is a different
// tool with different rules.
const eslintLocalBin = "node_modules/.bin/eslint"

// eslintEnv names the parent variables ESLint needs beyond exec.BaseEnvNames.
//
// Node itself needs nothing else to run a local binary; these two exist
// because a repository can legitimately require them (a raised heap for a
// large codebase, a resolution root for a linked workspace). NODE_ENV is
// deliberately absent: it changes which config a project loads, and belay
// should lint what a developer would see by default.
var eslintEnv = []string{"NODE_OPTIONS", "NODE_PATH"}

// eslintSeverities maps ESLint's severity integers to belay severities.
//
// ESLint's whole vocabulary is three integers, documented as: "The severity of
// this message. 1 means warning and 2 means error"
// (https://eslint.org/docs/latest/integrate/nodejs-api). There is no rule
// taxonomy to map: which rules are errors and which are warnings is decided
// entirely by the repository's own config.
//
// That is why this adapter has no per-rule promotion table. Marking, say,
// no-eval as Critical would override a project that deliberately set it to
// "warn", and belay would be second-guessing a judgement the project already
// made explicitly. The one severity belay assigns on its own authority is the
// fatal row below, which is not a rule verdict at all.
var eslintSeverities = map[int]mapping{
	2: {belay.SeverityMajor, "ESLint's strongest ordinary verdict, and the level at which eslint itself exits non-zero"},
	1: {belay.SeverityMinor, "a warning leaves eslint exiting zero, so it must not fail belay's default gate either"},
	0: {belay.SeverityInfo, "severity 0 means the rule is off and is never emitted; mapped so an unexpected one is still counted"},
}

// eslintFatalSeverity is the severity of a message with fatal set, documented
// as "true if this is a fatal error unrelated to a rule, like a parsing error".
//
// The only Blocker this adapter produces, and the only severity it assigns
// without deferring to the repository's configuration. A file ESLint could not
// parse was not linted at all: every rule in it was skipped, so a clean report
// for the rest of the project is measuring less than it appears to, and the
// file itself will not build.
var eslintFatalSeverity = mapping{belay.SeverityBlocker, "the file could not be parsed, so it was not linted and will not build"}

// defaultESLintSeverity catches a severity integer outside ESLint's documented
// range. Info, because an unrecognized verdict is not evidence of a serious
// problem and must not stop a run on its own.
var defaultESLintSeverity = mapping{belay.SeverityInfo, "an undocumented severity value; counted, but not trusted to stop a run"}

// eslintFatalRuleID is the RuleID given to a parse error, which carries a null
// ruleId because it comes from ESLint core rather than a rule.
const eslintFatalRuleID = "parse-error"

// eslintCoreRuleID is the RuleID given to any other message with a null
// ruleId.
const eslintCoreRuleID = "eslint-core"

// ESLint lints Node packages with eslint.
//
// It prefers the repository's own node_modules/.bin/eslint over a global one
// and never reaches for npx: a package runner would download and execute code
// from the network as a side effect of running a quality gate.
type ESLint struct {
	base
}

// NewESLint returns an ESLint adapter. With no options it gates at
// DefaultFailOn and runs commands through internal/exec.
func NewESLint(opts ...Option) *ESLint {
	return &ESLint{base: newBase(opts)}
}

var _ belay.Linter = (*ESLint)(nil)

// Name implements belay.Linter.
func (e *ESLint) Name() string { return eslintName }

// Detect implements belay.Linter. It reports whether internal/detect found a
// Node package under dir that declares or configures ESLint.
func (e *ESLint) Detect(dir string) bool {
	for _, project := range projects(dir) {
		if project.Kind == detect.KindNode && project.Node != nil && project.Node.HasESLint {
			return true
		}
	}
	return false
}

// Lint implements belay.Linter.
//
// Findings are a successful call: ESLint documents exit 1 as "linting was
// successful and there is at least one linting error", which returns a
// GateFail report and a nil error. Exit 2, "linting was unsuccessful due to a
// configuration problem or an internal error", leaves stdout empty and
// therefore surfaces as a GateError report and an *OutputError.
func (e *ESLint) Lint(ctx context.Context, dir string) (belay.QualityReport, error) {
	cmd := exec.Command{
		Path: localTool(dir, eslintName, eslintLocalBin),
		// "." is ESLint's own default target, passed explicitly.
		//
		// --no-error-on-unmatched-pattern is deliberately NOT passed. If
		// ESLint matches no files it exits 2, which belay reports as a
		// gate error and escalates. Suppressing that would report a
		// passing gate for a run that linted nothing at all, which is
		// the one failure mode a quality gate must never have.
		Args:      []string{"--format", "json", "."},
		Dir:       dir,
		EnvAllow:  eslintEnv,
		Timeout:   e.timeout,
		MaxOutput: DefaultMaxOutput,
	}
	return e.lint(ctx, eslintName, eslintName, cmd, eslintParser(dir))
}

// eslintResult is one element of the array the json formatter emits: the
// per-file result object.
type eslintResult struct {
	FilePath string          `json:"filePath"`
	Messages []eslintMessage `json:"messages"`
	// SuppressedMessages is deliberately not modelled. Those findings were
	// silenced by an eslint-disable comment in the repository, which is an
	// explicit decision belay does not overrule by counting them anyway.
}

// eslintMessage is one finding inside a result.
type eslintMessage struct {
	// RuleID is null for messages from ESLint core rather than a rule,
	// which decodes to the empty string.
	RuleID   string `json:"ruleId"`
	Severity int    `json:"severity"`
	Message  string `json:"message"`
	Line     int    `json:"line"`
	Fatal    bool   `json:"fatal"`
}

// eslintParser returns the parser for the json formatter's output, closing
// over dir because ESLint reports absolute file paths.
func eslintParser(dir string) parser {
	return func(stdout []byte) ([]belay.Issue, json.RawMessage, error) {
		return parseESLint(dir, stdout)
	}
}

// parseESLint turns the json formatter's output into issues.
func parseESLint(dir string, stdout []byte) ([]belay.Issue, json.RawMessage, error) {
	var results []eslintResult
	raw, err := decodeFirstJSON(stdout, &results)
	if err != nil {
		return nil, nil, err
	}
	issues := make([]belay.Issue, 0, len(results))
	for _, result := range results {
		file := relPath(dir, result.FilePath)
		for _, msg := range result.Messages {
			severity, _ := eslintSeverity(msg.Severity, msg.Fatal)
			issues = append(issues, belay.Issue{
				RuleID:   eslintRuleID(msg),
				Severity: severity,
				File:     file,
				Line:     msg.Line,
				Message:  strings.TrimSpace(msg.Message),
			})
		}
	}
	return issues, raw, nil
}

// eslintSeverity resolves one message's severity and the rationale behind it.
// Fatal is checked first: a parse error carries severity 2 like any other
// error, and the distinction is exactly what the fatal flag exists to record.
func eslintSeverity(severity int, fatal bool) (belay.Severity, string) {
	if fatal {
		return eslintFatalSeverity.Severity, eslintFatalSeverity.Why
	}
	if m, ok := eslintSeverities[severity]; ok {
		return m.Severity, m.Why
	}
	return defaultESLintSeverity.Severity, defaultESLintSeverity.Why
}

// eslintRuleID names the rule that fired, standing in a marker for the
// messages ESLint core emits with a null ruleId.
func eslintRuleID(msg eslintMessage) string {
	if id := strings.TrimSpace(msg.RuleID); id != "" {
		return id
	}
	if msg.Fatal {
		return eslintFatalRuleID
	}
	return eslintCoreRuleID
}
