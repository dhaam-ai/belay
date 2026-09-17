//go:build unix

package linter

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/dhaam-ai/belay/internal/detect"
	"github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// golangciName is the adapter's Name, and so every report's Source.
const golangciName = "golangci-lint"

// golangciEnv names the parent variables golangci-lint needs beyond
// exec.BaseEnvNames.
//
// golangci-lint type-checks the code it lints, so it runs the Go toolchain: it
// needs to find GOROOT, reach a module cache, and honour whatever GOFLAGS the
// repository builds with. Without the caches it still works, but every run
// pays a cold compile of the whole dependency graph.
var golangciEnv = []string{
	"CGO_ENABLED",
	"GOCACHE",
	"GOFLAGS",
	"GOLANGCI_LINT_CACHE",
	"GOMODCACHE",
	"GOPATH",
	"GOPRIVATE",
	"GOPROXY",
	"GOROOT",
	"GOTOOLCHAIN",
	"GOWORK",
}

// golangciSeverities maps a golangci-lint linter name to a belay.Severity.
//
// # Why the linter name and not the Severity field
//
// golangci-lint's own Severity string is empty for most findings. Verified
// against v2.12.2: an errcheck issue comes back as "Severity":"" while a revive
// issue in the same run comes back as "warning", because only some linters
// supply one. The field is populated wholesale only when a repository
// configures severity.default, which stamps one value onto every issue in the
// run — mapping through it would flatten a gosec finding and a misspelled
// comment to the same severity. The linter that fired is the stable, per-issue
// signal, so it is the key. golangciSeverityStrings is the fallback for a
// linter this table has not heard of.
//
// # Where the line is drawn
//
// Major and above fails the default gate (fail_on: major), so a row at Major
// is a claim that an agent should be sent back to fix it. Rows at Minor and
// Info are reported and counted but do not, on their own, stop a run.
var golangciSeverities = map[string]mapping{
	// The one Blocker. golangci-lint reports compilation failures through a
	// pseudo-linter called "typecheck" (verified against v2.12.2: a
	// "declared and not used" error arrives as FromLinter "typecheck").
	// Code that does not compile cannot ship and cannot be tested, so it
	// must fail every threshold including fail_on: blocker.
	"typecheck": {belay.SeverityBlocker, "the package does not compile; nothing downstream of this is meaningful"},

	// The one Critical. gosec is off by default, so its presence in a
	// repository's configuration is a deliberate request to gate on
	// security; a hardcoded credential or a command injection is worth
	// stopping a run for even when the caller has relaxed fail_on to
	// critical.
	"gosec": {belay.SeverityCritical, "a security finding, from a linter a repository has to opt into"},

	// Major: correctness defects. Each of these reports code that is wrong
	// rather than untidy — it leaks, hangs, or silently discards a failure.
	"govet":         {belay.SeverityMajor, "the standard toolchain's own correctness checks (printf, lostcancel, copylocks)"},
	"staticcheck":   {belay.SeverityMajor, "bug-class analysis: impossible conditions, misused stdlib, dead branches"},
	"errcheck":      {belay.SeverityMajor, "an unchecked error is a failure the program silently continues past"},
	"errorlint":     {belay.SeverityMajor, "comparing errors without errors.Is defeats wrapping, which belay itself depends on"},
	"bodyclose":     {belay.SeverityMajor, "an unclosed response body leaks a connection under load"},
	"sqlclosecheck": {belay.SeverityMajor, "an unclosed sql.Rows leaks a database connection"},
	"rowserrcheck":  {belay.SeverityMajor, "an unchecked rows.Err silently truncates a result set"},
	"nilerr":        {belay.SeverityMajor, "returning nil when err is non-nil hides the failure from every caller"},
	"nilnesserr":    {belay.SeverityMajor, "a nil error returned where the value is known to be non-nil"},
	"noctx":         {belay.SeverityMajor, "a request with no context cannot be cancelled and can hang forever"},
	"contextcheck":  {belay.SeverityMajor, "a dropped context breaks cancellation across a call chain"},
	"makezero":      {belay.SeverityMajor, "append to a make([]T, n) slice silently produces leading zero values"},
	"durationcheck": {belay.SeverityMajor, "multiplying two durations produces a nonsense value"},
	"exhaustive":    {belay.SeverityMajor, "an unhandled enum case is a branch nobody wrote"},

	// Minor: worth fixing, not worth stopping a run for. These are real
	// findings about code health rather than behaviour.
	"unused":        {belay.SeverityMinor, "dead code is a maintenance cost, not a defect"},
	"ineffassign":   {belay.SeverityMinor, "an ineffectual assignment is usually a leftover, occasionally a bug"},
	"revive":        {belay.SeverityMinor, "configurable style and lint rules, dominated by naming and comments"},
	"gocritic":      {belay.SeverityMinor, "a mixed bag of style and correctness diagnostics; the style half dominates"},
	"unconvert":     {belay.SeverityMinor, "a redundant conversion is noise"},
	"unparam":       {belay.SeverityMinor, "an always-identical parameter is a design smell"},
	"prealloc":      {belay.SeverityMinor, "a preallocation hint, not a defect"},
	"gocyclo":       {belay.SeverityMinor, "a complexity budget the repository chose"},
	"cyclop":        {belay.SeverityMinor, "a complexity budget the repository chose"},
	"gocognit":      {belay.SeverityMinor, "a complexity budget the repository chose"},
	"funlen":        {belay.SeverityMinor, "a length budget the repository chose"},
	"maintidx":      {belay.SeverityMinor, "a maintainability index the repository chose"},
	"dupl":          {belay.SeverityMinor, "duplicated code is a refactoring opportunity"},
	"goconst":       {belay.SeverityMinor, "a repeated literal is a refactoring opportunity"},
	"nakedret":      {belay.SeverityMinor, "a naked return is a readability rule"},
	"predeclared":   {belay.SeverityMinor, "shadowing a predeclared identifier is confusing, rarely wrong"},
	"usestdlibvars": {belay.SeverityMinor, "a literal that has a stdlib constant is a readability rule"},

	// Info: mechanically fixable. A formatter or a spell-checker can
	// resolve every one of these without a judgement call, so they must
	// never be the reason a run is sent back to an agent.
	"gofmt":      {belay.SeverityInfo, "formatting, fixable by running gofmt"},
	"gofumpt":    {belay.SeverityInfo, "formatting, fixable by running gofumpt"},
	"goimports":  {belay.SeverityInfo, "import grouping, fixable by running goimports"},
	"gci":        {belay.SeverityInfo, "import ordering, mechanically fixable"},
	"golines":    {belay.SeverityInfo, "line length, mechanically fixable"},
	"lll":        {belay.SeverityInfo, "line length, mechanically fixable"},
	"whitespace": {belay.SeverityInfo, "whitespace, mechanically fixable"},
	"wsl":        {belay.SeverityInfo, "whitespace and blank-line placement"},
	"wsl_v5":     {belay.SeverityInfo, "whitespace and blank-line placement"},
	"nlreturn":   {belay.SeverityInfo, "a blank line before return; purely cosmetic"},
	"godot":      {belay.SeverityInfo, "comment punctuation"},
	"misspell":   {belay.SeverityInfo, "a misspelling in a comment or string"},
	"dupword":    {belay.SeverityInfo, "a repeated word in a comment"},
	"godoclint":  {belay.SeverityInfo, "doc-comment formatting"},
}

// golangciSeverityStrings maps golangci-lint's own severity vocabulary, used
// only for a linter golangciSeverities does not list.
//
// The values are whatever a repository put in severity.default or
// severity.rules, plus the handful of linters that emit their own; there is no
// fixed set, so unrecognized text falls through to defaultGolangCISeverity.
var golangciSeverityStrings = map[string]mapping{
	"error":   {belay.SeverityMajor, "the repository called it an error, which is its strongest ordinary verdict"},
	"err":     {belay.SeverityMajor, "an abbreviation of error seen in hand-written severity rules"},
	"warning": {belay.SeverityMinor, "the repository called it a warning, which does not fail its own build"},
	"warn":    {belay.SeverityMinor, "an abbreviation of warning seen in hand-written severity rules"},
	"info":    {belay.SeverityInfo, "the repository called it informational"},
}

// defaultGolangCISeverity is the severity for a finding this package cannot
// place at all: a linter missing from golangciSeverities that also carried no
// usable severity string.
//
// Minor, deliberately. golangci-lint ships well over a hundred linters and
// most of them are style; a repository that enables a new one must not
// discover it by having every belay run hard-stop at fail_on: major on a rule
// belay has never heard of. The finding is still parsed, counted and reported
// — it simply does not, by itself, send an agent back into the fix loop.
var defaultGolangCISeverity = mapping{belay.SeverityMinor, "an unrecognized linter; reported and counted, but not trusted to stop a run"}

// GolangCI lints Go modules with golangci-lint.
//
// It targets golangci-lint v2 and its --output.json.path=stdout flag. The v1
// spelling, --out-format json, was removed in v2 and is not passed: v2.12.2's
// run command has no --out-format flag at all, so sending it would make every
// invocation fail to start rather than fall back.
type GolangCI struct {
	base
}

// NewGolangCI returns a GolangCI adapter. With no options it gates at
// DefaultFailOn and runs commands through internal/exec.
func NewGolangCI(opts ...Option) *GolangCI {
	return &GolangCI{base: newBase(opts)}
}

var _ belay.Linter = (*GolangCI)(nil)

// Name implements belay.Linter.
func (g *GolangCI) Name() string { return golangciName }

// Detect implements belay.Linter. It reports whether internal/detect found a
// Go module anywhere under dir.
func (g *GolangCI) Detect(dir string) bool {
	for _, project := range projects(dir) {
		if project.Kind == detect.KindGo {
			return true
		}
	}
	return false
}

// golangciScoped and golangciUnscoped are the flags that decide which findings
// golangci-lint reports. Both pin every issues.new* and whole-files setting,
// because a repository's .golangci.yml could otherwise count committed
// findings again, or hide the run's own. They differ only in --new-from-rev.
var (
	golangciScoped   = []string{"--new=false", "--new-from-rev=HEAD", "--new-from-merge-base=", "--new-from-patch=", "--whole-files=false"}
	golangciUnscoped = []string{"--new=false", "--new-from-rev=", "--new-from-merge-base=", "--new-from-patch=", "--whole-files=false"}
)

// Bounds for the git command committed runs. It lists one directory level,
// so it is quick and short; the cap only has to leave output non-empty.
const (
	gitCheckTimeout         = 30 * time.Second
	gitCheckMaxOutput int64 = 64 << 10
)

// Lint implements belay.Linter.
//
// It runs golangci-lint over every package under dir and, where git allows,
// reports only the findings on lines that differ from HEAD, which is where a
// run's uncommitted edits are. Findings are a successful call: golangci-lint
// exits 1 when it has any, and that returns a GateFail report with a nil
// error. Only a golangci-lint that could not run at all — missing, timed out,
// or refusing its own configuration, which it signals with exit 3 and an
// empty stdout — produces an error.
//
// # Why only changed lines
//
// The gate decides whether a run's change can leave the fix loop. Findings
// that were already committed say nothing about that change. Counting them
// would fail every run in a repository with existing debt, whatever the run
// did, and send the fix node to repair code the run never touched.
//
// --new-from-rev=HEAD is golangci-lint's own way to scope a run: it diffs the
// working tree against HEAD with git, and counts a new file as changed.
// Scoping needs a HEAD that holds dir, so Lint asks git first (see committed).
// Without one — no repository, no commit yet, git missing, or a directory the
// repository ignores, such as a fanout candidate under .belay — Lint clears
// the scope, so every finding counts, and says so in a warning and in the
// report's Summary. Passing the scope there would hide every finding in an
// ignored directory.
//
// # What scoping by line does not see
//
//   - A finding the change causes on a line it did not touch, such as an
//     unchecked error where a function that now returns one is called. A
//     compile error is the exception: golangci-lint always reports those, so
//     committed code that does not compile fails the gate too.
//   - A file git ignores, which never counts as changed.
//   - A commit made during the run. HEAD is read when the gate runs, and
//     belay never commits, so it is normally the commit the run started from;
//     a commit made since hides what it contains. Uncommitted edits made
//     before the run count as part of it.
//   - Findings from another directory that holds byte-identical changed
//     code and was linted with the same cache. golangci-lint keys its cache
//     by file content but stores absolute paths
//     (golangci/golangci-lint#3502), so this run is handed that directory's
//     findings, and the diff drops them because their paths are not in it.
//
// Scoping runs git in dir, so repository-local git configuration, such as
// core.fsmonitor, can run commands during the gate.
// That adds no trust the run had not already given: the test node has run
// the repository's code by then.
func (g *GolangCI) Lint(ctx context.Context, dir string) (belay.QualityReport, error) {
	scoped := g.committed(ctx, dir)
	scope := golangciUnscoped
	if scoped {
		scope = golangciScoped
	}
	args := append([]string{"run", "--output.json.path=stdout"}, scope...)
	cmd := exec.Command{
		Path: golangciName,
		// --output.json.path=stdout replaces the default text format.
		// No --timeout: exec already bounds the run and kills the whole
		// process group, and two competing deadlines only make the
		// failure harder to read.
		Args:      append(args, "./..."),
		Dir:       dir,
		EnvAllow:  golangciEnv,
		Timeout:   g.timeout,
		MaxOutput: DefaultMaxOutput,
	}
	report, err := g.lint(ctx, golangciName, golangciName, cmd, golangciParser(dir))
	if err == nil && !scoped {
		// The warning committed logs is on screen only with --verbose.
		// The summary reaches state.json, the report artifact and the fix
		// prompt, where it explains findings the run did not cause.
		report.Summary += "; not scoped to the run's change, so findings committed before it count too"
	}
	return report, err
}

// committed reports whether git's HEAD holds dir, which scoping findings to
// the lines that differ from HEAD requires. Any other answer is logged,
// because it means every finding in dir counts toward the gate.
func (g *GolangCI) committed(ctx context.Context, dir string) bool {
	res, err := g.runner.Run(ctx, exec.Command{
		Path: "git",
		// ls-tree lists dir's entries in HEAD, relative to dir. It prints
		// nothing for a directory the repository ignores or has not
		// committed, and fails with no repository or no commit.
		Args:      []string{"ls-tree", "--name-only", "HEAD"},
		Dir:       dir,
		Timeout:   min(gitCheckTimeout, g.timeout),
		MaxOutput: gitCheckMaxOutput,
	})
	var reason string
	switch {
	case err != nil:
		reason = err.Error()
	case strings.TrimSpace(res.Stdout) == "":
		reason = "HEAD holds nothing in this directory: git ignores it, or it is not committed yet"
	default:
		return true
	}
	if ctx.Err() == nil {
		g.logger.LogAttrs(ctx, slog.LevelWarn, "belay/linter: cannot scope golangci-lint to the run's change; every finding counts",
			slog.String("dir", dir),
			slog.String("reason", reason))
	}
	return false
}

// golangciReport is the top-level document golangci-lint's json output emits.
// Report, which lists every known linter and whether it ran, is deliberately
// not modelled: it is large, it changes between releases, and nothing in belay
// reads it.
type golangciReport struct {
	Issues []golangciIssue `json:"Issues"`
}

// golangciIssue is one entry of golangciReport.Issues.
//
// The field names are golangci-lint's Go struct names: its printer encodes
// pkg/result.Issue with no json tags, so the wire keys are capitalized.
type golangciIssue struct {
	FromLinter string      `json:"FromLinter"`
	Text       string      `json:"Text"`
	Severity   string      `json:"Severity"`
	Pos        golangciPos `json:"Pos"`
}

// golangciPos is the go/token.Position golangci-lint embeds in each issue.
type golangciPos struct {
	Filename string `json:"Filename"`
	Line     int    `json:"Line"`
	Column   int    `json:"Column"`
}

// golangciParser returns the parser for golangci-lint's json output.
//
// It closes over dir because belay.Issue.File is relative to the linted
// directory. golangci-lint already reports relative paths, but a repository
// that sets output.path-mode can change that, and relPath leaves an
// already-relative path alone.
func golangciParser(dir string) parser {
	return func(stdout []byte) ([]belay.Issue, json.RawMessage, error) {
		return parseGolangCI(dir, stdout)
	}
}

// parseGolangCI turns golangci-lint's json output into issues.
func parseGolangCI(dir string, stdout []byte) ([]belay.Issue, json.RawMessage, error) {
	var doc golangciReport
	raw, err := decodeFirstJSON(stdout, &doc)
	if err != nil {
		return nil, nil, err
	}
	issues := make([]belay.Issue, 0, len(doc.Issues))
	for _, in := range doc.Issues {
		severity, _ := golangciSeverity(in.FromLinter, in.Severity)
		issues = append(issues, belay.Issue{
			// The linter name is the whole rule identity golangci-lint
			// exposes as a field. Some linters repeat their sub-rule at
			// the front of Text ("package-comments: should have..."),
			// but most do not, so splitting it out would invent a
			// namespace that only sometimes exists. RuleID is opaque to
			// belay, so the tool's own name is carried through verbatim.
			RuleID:   in.FromLinter,
			Severity: severity,
			File:     relPath(dir, in.Pos.Filename),
			Line:     in.Pos.Line,
			Message:  strings.TrimSpace(in.Text),
		})
	}
	return issues, raw, nil
}

// golangciSeverity resolves one finding's severity and the rationale behind
// it: the linter name first, that finding's own severity string second, and
// defaultGolangCISeverity last.
func golangciSeverity(fromLinter, severity string) (belay.Severity, string) {
	if m, ok := golangciSeverities[strings.ToLower(strings.TrimSpace(fromLinter))]; ok {
		return m.Severity, m.Why
	}
	if m, ok := golangciSeverityStrings[strings.ToLower(strings.TrimSpace(severity))]; ok {
		return m.Severity, m.Why
	}
	return defaultGolangCISeverity.Severity, defaultGolangCISeverity.Why
}
