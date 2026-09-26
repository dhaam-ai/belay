// Package policy is belay's rule engine: it judges the change a run has made
// against the rules declared in belay.yaml, whatever the tests say.
//
// It exists because "the tests pass" is not the whole of "this change is
// acceptable". A change that passes every test can still edit the deployment
// pipeline, add a dependency nobody vetted, grow by four thousand lines, or
// leave a credential in a source file. belay points a coding agent at a
// repository and walks away; the rules here are what makes that safe to do.
//
// # This package decides nothing about the run
//
// Evaluate returns a Report. It does not stop a run, route one, or write
// anything. internal/nodes/policy turns a Report into a graph.Result, and
// that split is deliberate: the rules are then a pure function of (rules,
// baseline, change, files on disk) and every one of them is table-testable
// without a dispatcher, a workspace under a run directory, or an agent.
//
// # Two classes of violation, decided by the rule and never by severity
//
// A violation either ends the run or is handed to the fix loop, and which one
// is a property of the rule (see Rule.Class), not of how serious the
// violation looks. Asking an agent to repair "you touched infra/, which you
// may not touch" is asking it to un-touch a file; the only honest answer is
// to stop and tell a person. Whereas a missing test, a new dependency or a
// forbidden string in a file are all things the next attempt can genuinely
// put right.
//
// Encoding that in belay.Severity instead was considered and rejected.
// Severity answers "how bad is this", the fix/fail split answers "who can act
// on it", and the two come apart immediately: a missing test is serious and
// repairable, a one-file overrun of a size ceiling is trivial and not.
//
// # What a baseline is, and what it deliberately is not
//
// Only one rule needs to know what the workspace looked like before the run:
// no_new_dependencies, which cannot tell a dependency the agent added from
// one that was always there without a record taken first. That record is
// Baseline, it is captured when the run is created, and it is pinned in the
// run's manifest — see internal/state and ADR-0014 for why it may not be
// re-read from a belay.yaml that has changed since.
//
// belay keeps no copy of the tree from before the agent ran, so no rule here
// can compute a true line-level diff, and max_changed_lines deliberately does
// not pretend to. It counts the whole of every file the change touches. That
// over-counts a one-line edit to a long file and never under-counts, which is
// the safe direction for a ceiling: it is a limit on how much of the
// repository one unattended run may rewrite, and how much a person has to
// read afterwards, not a diffstat.
package policy

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/dhaam-ai/belay/internal/glob"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Class says who can act on a violation.
//
// ClassFail is the zero value on purpose. A rule added without a class stops
// the run rather than quietly joining the repairable ones: the cost of that
// mistake is a run a person has to look at, and the cost of the other is a
// guardrail that lets the change through after the agent shrugs at it.
type Class int

const (
	// ClassFail ends the run. The change broke a rule no repair can undo,
	// and a person has to decide what happens next.
	ClassFail Class = iota
	// ClassFix routes the run into the fix loop, which asks the agent to
	// put the violation right and runs the gate again.
	ClassFix
)

// String returns a short, stable, lowercase label suitable for a log field
// and a journal note.
func (c Class) String() string {
	switch c {
	case ClassFail:
		return "fail"
	case ClassFix:
		return "fix"
	default:
		return fmt.Sprintf("Class(%d)", int(c))
	}
}

// Rule names one policy rule. It is also the RuleID a violation carries into
// belay.Issue, which is why it is namespaced: a reader of a quality report
// can tell belay's own rules from a linter's at a glance.
type Rule string

// The rules, in the order Evaluate applies them.
const (
	// RuleForbiddenPath fired because the change touched a path
	// policy.forbidden_paths covers.
	RuleForbiddenPath Rule = "policy:forbidden_path"
	// RuleMaxChangedFiles fired because the change touched more files than
	// policy.max_changed_files allows.
	RuleMaxChangedFiles Rule = "policy:max_changed_files"
	// RuleMaxChangedLines fired because the files the change touched hold
	// more lines than policy.max_changed_lines allows.
	RuleMaxChangedLines Rule = "policy:max_changed_lines"
	// RuleNewDependency fired because the change declares a dependency the
	// run's baseline did not have.
	RuleNewDependency Rule = "policy:no_new_dependencies"
	// RuleRequiredTests fired because the change touched a file a
	// policy.required_tests rule covers without touching one it requires.
	RuleRequiredTests Rule = "policy:required_tests"
	// RuleForbiddenContent fired because a changed file matches a
	// policy.forbidden_content pattern.
	RuleForbiddenContent Rule = "policy:forbidden_content"
)

// allRules is every rule, in the order Evaluate applies them. Evaluate walks
// its own checks in this order, and TestEveryRuleDeclaresWhoCanActOnIt holds
// this list to the classes, so a rule added without deciding whether it ends
// a run fails the build's tests rather than shipping.
var allRules = []Rule{
	RuleForbiddenPath,
	RuleMaxChangedFiles,
	RuleMaxChangedLines,
	RuleNewDependency,
	RuleRequiredTests,
	RuleForbiddenContent,
}

// Rules returns every rule this package can report, in the order Evaluate
// applies them. The result is a fresh slice.
func Rules() []Rule { return slices.Clone(allRules) }

// Class reports who can act on a violation of r: see Class.
func (r Rule) Class() Class {
	switch r {
	case RuleForbiddenPath, RuleMaxChangedFiles, RuleMaxChangedLines:
		return ClassFail
	case RuleNewDependency, RuleRequiredTests, RuleForbiddenContent:
		return ClassFix
	default:
		return ClassFail
	}
}

// Severity is how a violation of r ranks in a belay.QualityReport.
//
// A fail-class violation is a blocker, which the contract defines as work
// that must not ship — exactly what a run that ends over one means. Every
// other violation is major: a real defect the next attempt should repair.
// Nothing reads the severity to decide what the run does; Class does that.
func (r Rule) Severity() belay.Severity {
	if r.Class() == ClassFail {
		return belay.SeverityBlocker
	}
	return belay.SeverityMajor
}

// TestRule requires that a change touching a file matching When also touches
// a file matching Require. Both are internal/glob patterns.
type TestRule struct {
	// When selects the changed files this rule is about, such as "**/*.go".
	When string `json:"when"`
	// Require is what the change must also touch, such as "**/*_test.go".
	Require string `json:"require"`
}

// ContentRule forbids a regular expression in the changed files it covers.
type ContentRule struct {
	// Pattern is a Go regular expression (regexp/syntax), matched line by
	// line so a violation can name the line it is on.
	Pattern string `json:"pattern"`
	// Paths limits the rule to the changed files matching one of these
	// internal/glob patterns. Empty means every changed file.
	Paths []string `json:"paths,omitempty"`
	// Reason is what a person is told when the rule fires, in place of the
	// pattern itself. A bare regular expression in a failure message
	// explains nothing to whoever has to act on it.
	Reason string `json:"reason,omitempty"`
}

// Policy is a run's rule set, as pinned in its manifest.
//
// Its JSON tags matter: this is written into manifest.json when a run is
// created and read back on every resume, so it is a persisted shape, not a
// view of the configuration file. internal/config owns the YAML the person
// writes, and From converts one into the other.
//
// A zero-valued ceiling means no ceiling, matching how belay reads every
// other limit: a person who has not set a number has not asked for one.
type Policy struct {
	// ForbiddenPaths are internal/glob patterns the change may not touch.
	ForbiddenPaths []string `json:"forbidden_paths,omitempty"`
	// MaxChangedFiles is the most files one run's change may touch. Zero
	// means no ceiling.
	MaxChangedFiles int `json:"max_changed_files,omitempty"`
	// MaxChangedLines is the most lines the files a change touches may hold
	// together. Zero means no ceiling; see the package documentation for
	// what is counted and why it over-counts rather than guessing.
	MaxChangedLines int `json:"max_changed_lines,omitempty"`
	// NoNewDependencies forbids declaring a dependency the run's Baseline
	// did not have.
	NoNewDependencies bool `json:"no_new_dependencies,omitempty"`
	// RequiredTests are the test-beside-the-change rules.
	RequiredTests []TestRule `json:"required_tests,omitempty"`
	// ForbiddenContent are the content rules.
	ForbiddenContent []ContentRule `json:"forbidden_content,omitempty"`
}

// Empty reports whether p asks for nothing at all, which is the common case:
// most repositories declare no policy, and a run whose policy is empty gets
// no gate in its graph.
func (p Policy) Empty() bool {
	return len(p.ForbiddenPaths) == 0 &&
		p.MaxChangedFiles == 0 &&
		p.MaxChangedLines == 0 &&
		!p.NoNewDependencies &&
		len(p.RequiredTests) == 0 &&
		len(p.ForbiddenContent) == 0
}

// Change is the change one evaluation judges.
type Change struct {
	// Workspace is the absolute path of the tree the run works in — a
	// candidate's isolated copy or a run's own worktree, not necessarily
	// the person's checkout.
	Workspace string
	// Files are the change's paths, relative to Workspace and
	// slash-separated, as the write node verified them against disk. A path
	// that is not on disk is a deletion.
	Files []string
}

// Violation is one way a change breaks a policy.
type Violation struct {
	// Rule is the rule that fired.
	Rule Rule `json:"rule"`
	// Class is Rule.Class, carried on the violation so that anything
	// reading a stored report does not have to re-derive it from a rule
	// name a later belay may have retired.
	Class Class `json:"class"`
	// Path is the file the violation is about, relative to the workspace,
	// or empty for a violation about the change as a whole.
	Path string `json:"path,omitempty"`
	// Line is the 1-indexed line, for a violation a line can be pinned on.
	// Zero otherwise.
	Line int `json:"line,omitempty"`
	// Message is what a person — or the agent being asked to repair it — is
	// told. It names the setting that fired, so nobody has to read the
	// whole of belay.yaml to find out which line stopped their run.
	Message string `json:"message"`
}

// Report is every violation one evaluation found, in rule order.
type Report struct {
	// Violations is what the change broke. Empty, never nil, for a clean
	// change.
	Violations []Violation `json:"violations"`
}

// Clean reports whether the change broke nothing.
func (r Report) Clean() bool { return len(r.Violations) == 0 }

// Failed reports whether the change broke a rule that ends the run. A report
// that is not Clean and not Failed is one the fix loop can be given.
func (r Report) Failed() bool {
	return slices.ContainsFunc(r.Violations, func(v Violation) bool { return v.Class == ClassFail })
}

// Source is belay.QualityReport.Source for every report this package
// produces, so a stored report says what judged it.
const Source = "belay-policy"

// Summary is a short human-readable synopsis, for a journal note and for the
// prompt that asks an agent to repair a violation.
func (r Report) Summary() string {
	if r.Clean() {
		return "no policy violations"
	}
	var fails, fixes int
	for _, v := range r.Violations {
		if v.Class == ClassFail {
			fails++
			continue
		}
		fixes++
	}
	switch {
	case fails == 0:
		return fmt.Sprintf("%s to repair", plural(fixes, "policy violation"))
	case fixes == 0:
		return fmt.Sprintf("%s the run cannot continue past", plural(fails, "policy violation"))
	default:
		return fmt.Sprintf("%s, %d of which the run cannot continue past",
			plural(fails+fixes, "policy violation"), fails)
	}
}

// plural renders "1 thing" or "3 things".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// QualityReport projects r onto belay's one quality report contract
// (ADR-0006).
//
// Reusing that contract rather than inventing a second one is what lets
// everything downstream of the gate stay reviewer-agnostic: the blackboard
// keeps one shape for a verdict about code, the fix node's prompt builder
// already renders a belay.Issue, and a policy violation reaches a person in
// the same words a lint finding does. The fields line up exactly — a rule is
// a RuleID, a violation has a file and often a line, and a message is a
// message — and the one thing the contract has no room for, which of two
// classes a violation is, is a property of the rule and so is recoverable
// from the RuleID without a field of its own.
//
// Issues is never nil and Counts is always populated, including for a clean
// report, because the contract's identical-shape guarantee breaks first for
// the emptiest report.
func (r Report) QualityReport() belay.QualityReport {
	issues := make([]belay.Issue, 0, len(r.Violations))
	var counts belay.Counts
	for _, v := range r.Violations {
		severity := v.Rule.Severity()
		issues = append(issues, belay.Issue{
			RuleID:   string(v.Rule),
			Severity: severity,
			File:     v.Path,
			Line:     v.Line,
			Message:  v.Message,
		})
		switch severity {
		case belay.SeverityBlocker:
			counts.Blocker++
		case belay.SeverityCritical:
			counts.Critical++
		case belay.SeverityMajor:
			counts.Major++
		case belay.SeverityMinor:
			counts.Minor++
		case belay.SeverityInfo:
			counts.Info++
		}
	}
	gate := belay.GateFail
	if r.Clean() {
		gate = belay.GatePass
	}
	return belay.QualityReport{
		Source:  Source,
		Gate:    gate,
		Counts:  counts,
		Issues:  issues,
		Summary: r.Summary(),
	}
}

// Evaluate judges ch against p and returns every violation it found.
//
// It is a verdict, not a decision: a change that breaks every rule comes back
// as a Report with a nil error, exactly as a clean one does. A non-nil error
// means no verdict was reachable — a rule belay cannot compile, a file it
// cannot read — and the caller must never read the Report as a pass. Every
// error names the setting or the file responsible, because the only person
// who can clear one is the person who wrote it.
//
// The change is judged in path order, not in the order the agent happened to
// report its files, so two runs over the same change produce the same report.
// That is what makes a re-run of the gate after a crash land on the verdict
// the attempt that crashed had already reached.
func Evaluate(ctx context.Context, p Policy, base Baseline, ch Change) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("policy: %w", err)
	}
	files := slices.Compact(slices.Sorted(slices.Values(ch.Files)))
	report := Report{Violations: []Violation{}}

	forbidden, err := compileSet("policy.forbidden_paths", p.ForbiddenPaths)
	if err != nil {
		return Report{}, err
	}
	for _, rel := range files {
		if pattern, hit := forbidden.Match(rel); hit {
			report.add(RuleForbiddenPath, rel, 0, fmt.Sprintf(
				"%s is covered by policy.forbidden_paths %q, and this run changed it", rel, pattern))
		}
	}

	if p.MaxChangedFiles > 0 && len(files) > p.MaxChangedFiles {
		report.add(RuleMaxChangedFiles, "", 0, fmt.Sprintf(
			"this change touches %d files, and policy.max_changed_files allows %d",
			len(files), p.MaxChangedFiles))
	}

	// Files are read at most once, and only when a rule needs their
	// contents: a policy of nothing but path globs never opens a file.
	scanned, err := scanChange(ctx, p, ch, files)
	if err != nil {
		return Report{}, err
	}
	if p.MaxChangedLines > 0 && scanned.lines > p.MaxChangedLines {
		report.add(RuleMaxChangedLines, "", 0, fmt.Sprintf(
			"the files this change touches hold %d lines together, and policy.max_changed_lines allows %d",
			scanned.lines, p.MaxChangedLines))
	}

	if p.NoNewDependencies {
		added, err := newDependencies(ctx, base, ch.Workspace)
		if err != nil {
			return Report{}, err
		}
		for _, dep := range added {
			report.add(RuleNewDependency, dep.Manifest, 0, fmt.Sprintf(
				"%s declares the dependency %s, which this run's baseline did not have, and "+
					"policy.no_new_dependencies forbids adding one", dep.Manifest, dep.Name))
		}
	}

	for _, rule := range p.RequiredTests {
		v, err := requiredTest(rule, files)
		if err != nil {
			return Report{}, err
		}
		if v != nil {
			report.Violations = append(report.Violations, *v)
		}
	}

	report.Violations = append(report.Violations, scanned.content...)
	return report, nil
}

// add appends one violation, taking its class from its rule so no caller can
// classify a violation differently from every other of the same rule.
func (r *Report) add(rule Rule, path string, line int, message string) {
	r.Violations = append(r.Violations, Violation{
		Rule: rule, Class: rule.Class(), Path: path, Line: line, Message: message,
	})
}

// requiredTest reports whether the change satisfies one required-tests rule.
//
// A rule whose When matches nothing asks for nothing: the rule is about
// changes of a kind this change did not make. The violation names one file
// that triggered it rather than all of them, because the repair is the same
// whichever triggered it and the agent needs an example, not an inventory.
func requiredTest(rule TestRule, files []string) (*Violation, error) {
	when, err := compileOne("policy.required_tests.when", rule.When)
	if err != nil {
		return nil, err
	}
	require, err := compileOne("policy.required_tests.require", rule.Require)
	if err != nil {
		return nil, err
	}
	var triggered []string
	for _, rel := range files {
		if require.Match(rel) {
			return nil, nil
		}
		if when.Match(rel) {
			triggered = append(triggered, rel)
		}
	}
	if len(triggered) == 0 {
		return nil, nil
	}
	return &Violation{
		Rule: RuleRequiredTests, Class: RuleRequiredTests.Class(), Path: triggered[0],
		Message: fmt.Sprintf("this change touches %s matching policy.required_tests %q (%s), "+
			"and nothing in it matches the %q it requires",
			plural(len(triggered), "file"), when, strings.Join(triggered, ", "), require),
	}, nil
}

// compileSet compiles a list of globs, naming the setting they came from.
//
// internal/config refuses an uncompilable glob when the configuration loads,
// so reaching this with one means the run's pinned policy was written by a
// belay that allowed it, or edited by hand. Either way the honest answer is
// no verdict rather than a pass: a rule belay cannot evaluate has not been
// satisfied, it has been skipped.
func compileSet(setting string, patterns []string) (glob.Set, error) {
	set, err := glob.CompileAll(patterns)
	if err != nil {
		return glob.Set{}, fmt.Errorf("policy: %s: %w", setting, err)
	}
	return set, nil
}

// compileOne compiles one glob, naming the setting it came from.
func compileOne(setting, pattern string) (glob.Pattern, error) {
	p, err := glob.Compile(pattern)
	if err != nil {
		return glob.Pattern{}, fmt.Errorf("policy: %s: %w", setting, err)
	}
	return p, nil
}
