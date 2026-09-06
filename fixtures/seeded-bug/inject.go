// Command inject applies catalogued defects from ./bugs onto a fresh copy
// of ./src, verifies the result behaves exactly as the catalogue says it
// should (the copy still compiles, the catalogued tests fail, the
// catalogued linters fire, and nothing else breaks), and writes
// injected.json describing exactly what it did.
//
// Usage:
//
//	go run ./inject.go --bugs=B03,B07 --out=/tmp/seeded-01
//	go run ./inject.go --random=5 --seed=42 --out=/tmp/seeded-02
//
// See README.md for the full flag reference and the injected.json
// schema.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"belay.dev/fixtures/seededbug/bugs"
)

const (
	// injectedGoVersion is the go directive written into the injected
	// copy's own go.mod, so it builds standalone wherever --out points.
	injectedGoVersion = "1.26.3"
	buildTimeout      = 2 * time.Minute
	testTimeout       = 3 * time.Minute
	lintTimeout       = 2 * time.Minute
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "inject:", err)
		os.Exit(1)
	}
}

// options are the resolved, validated command-line inputs.
type options struct {
	bugs    []string
	random  int
	seed    int64
	seedSet bool
	src     string
	out     string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	fset := flag.NewFlagSet("inject", flag.ContinueOnError)
	fset.SetOutput(stderr)
	bugsFlag := fset.String("bugs", "", "comma-separated defect IDs to inject, e.g. B03,B07")
	outFlag := fset.String("out", "", "destination directory for the injected copy (required)")
	randomFlag := fset.Int("random", 0, "number of defects to select at random, instead of --bugs")
	seedFlag := fset.String("seed", "", "seed for --random; required whenever --random is set")
	srcFlag := fset.String("src", "src", "path to the known-correct source tree to copy from")
	fset.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage:")
		_, _ = fmt.Fprintln(stderr, "  go run ./inject.go --bugs=B03,B07 --out=<dir>")
		_, _ = fmt.Fprintln(stderr, "  go run ./inject.go --random=N --seed=<int> --out=<dir>")
		_, _ = fmt.Fprintln(stderr)
		fset.PrintDefaults()
	}
	if err := fset.Parse(args); err != nil {
		return options{}, err
	}

	var opt options
	opt.out = *outFlag
	opt.src = *srcFlag
	opt.random = *randomFlag

	for _, id := range strings.Split(*bugsFlag, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		opt.bugs = append(opt.bugs, id)
	}

	if *seedFlag != "" {
		seed, err := strconv.ParseInt(*seedFlag, 10, 64)
		if err != nil {
			return options{}, fmt.Errorf("--seed: %w", err)
		}
		opt.seed = seed
		opt.seedSet = true
	}

	if opt.out == "" {
		return options{}, errors.New("--out is required")
	}
	if len(opt.bugs) > 0 && opt.random > 0 {
		return options{}, errors.New("--bugs and --random are mutually exclusive")
	}
	if len(opt.bugs) == 0 && opt.random == 0 {
		return options{}, errors.New("specify either --bugs=ID,ID,... or --random=N (with --seed)")
	}
	if opt.random < 0 {
		return options{}, fmt.Errorf("--random must be >= 0, got %d", opt.random)
	}
	if opt.random > 0 && !opt.seedSet {
		return options{}, errors.New("--random requires --seed, so the selection is reproducible")
	}
	return opt, nil
}

// selectDefects resolves opt against the full catalogue (already sorted
// by ID) into the ordered list of defects to inject.
func selectDefects(all []bugs.Defect, opt options) ([]bugs.Defect, error) {
	if opt.random > 0 {
		return randomDefects(all, opt.random, opt.seed)
	}
	return lookupDefects(all, opt.bugs)
}

func lookupDefects(all []bugs.Defect, ids []string) ([]bugs.Defect, error) {
	byID := make(map[string]bugs.Defect, len(all))
	for _, d := range all {
		byID[d.ID] = d
	}

	seen := make(map[string]bool, len(ids))
	out := make([]bugs.Defect, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			return nil, fmt.Errorf("--bugs: duplicate defect id %q", id)
		}
		seen[id] = true
		d, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("--bugs: unknown defect id %q", id)
		}
		out = append(out, d)
	}
	return out, nil
}

// randomDefects picks exactly n defects out of all using a PRNG seeded
// from seed. The same (all, n, seed) always yields the same n IDs: the
// permutation is deterministic given the seed, and all is sorted by ID
// before Perm ever sees it, so the catalogue's own init-registration
// order can never influence the result.
func randomDefects(all []bugs.Defect, n int, seed int64) ([]bugs.Defect, error) {
	if n > len(all) {
		return nil, fmt.Errorf("--random=%d exceeds the catalogue size (%d defects)", n, len(all))
	}
	//nolint:gosec // deterministic, reproducible defect selection is the entire point; crypto/rand cannot be seeded.
	rng := rand.New(rand.NewSource(seed))
	perm := rng.Perm(len(all))
	selected := make([]bugs.Defect, 0, n)
	for _, idx := range perm[:n] {
		selected = append(selected, all[idx])
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
	return selected, nil
}

// checkSafeDestination refuses any --out that is src itself, nested
// inside src, or an ancestor of src (which os.RemoveAll(out) would then
// delete on the way to a clean copy).
func checkSafeDestination(src, out string) error {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("resolving --src: %w", err)
	}
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("resolving --out: %w", err)
	}
	if withinOrEqual(srcAbs, outAbs) {
		return fmt.Errorf("refusing to write into src: --out (%s) is src (%s) itself or nested inside it", outAbs, srcAbs)
	}
	if withinOrEqual(outAbs, srcAbs) {
		return fmt.Errorf("refusing to write into src: --out (%s) is an ancestor of src (%s); clearing it would delete src", outAbs, srcAbs)
	}
	return nil
}

// withinOrEqual reports whether target is base itself or a descendant of
// base. Both arguments must already be absolute, cleaned paths.
func withinOrEqual(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// copyTree recursively copies src to dst. Every path it touches is
// derived from filepath.WalkDir(src, ...) and target is always inside
// dst (via filepath.Join with a filepath.Rel-computed suffix of a path
// WalkDir itself produced under src), so this never reads or writes
// outside the two directory trees the caller already validated via
// checkSafeDestination.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755) //nolint:gosec // fixture output must stay readable by whatever build/test/lint process runs next, possibly under a different user or container
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // path comes from WalkDir(src, ...), not external input
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm()) //nolint:gosec // target is dst joined with a WalkDir-derived relative path under src, per the doc comment above
	})
}

func writeGoMod(dir string) error {
	content := fmt.Sprintf("module belay.dev/fixtures/seededbug/injected\n\ngo %s\n", injectedGoVersion)
	return os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644) //nolint:gosec // fixture output must stay group/world readable for whatever process builds it next
}

// injectedLintConfig mirrors the linter set (and the one non-default
// gocritic check, deferInLoop) the catalogue was authored and verified
// against, so a defect's DetectedBy: lint claim holds no matter where
// --out places the injected copy. Without this, a copy placed outside
// this repo would fall back to golangci-lint's much smaller built-in
// default linter set (errcheck, govet, ineffassign, staticcheck,
// unused), silently under-reporting several catalogued defects.
const injectedLintConfig = `version: "2"
linters:
  enable:
    - bodyclose
    - errcheck
    - errorlint
    - gocritic
    - gosec
    - govet
    - ineffassign
    - misspell
    - revive
    - staticcheck
    - unused
  settings:
    gocritic:
      enabled-checks:
        - deferInLoop
`

func writeLintConfig(dir string) error {
	return os.WriteFile(filepath.Join(dir, ".golangci.yml"), []byte(injectedLintConfig), 0o644) //nolint:gosec // same rationale as writeGoMod
}

// plannedEdit is one defect's Find/Replace resolved to a concrete byte
// range within one file of the copy about to be edited.
type plannedEdit struct {
	defect bugs.Defect
	file   string
	start  int
	end    int
	line   int
}

// planEdits locates every defect's Find text within dir (the copy) and
// verifies each occurs exactly once, then checks that no two defects
// touch overlapping byte ranges in the same file. It does not modify
// anything; call applyEdits with its result to do that.
func planEdits(dir string, defects []bugs.Defect) ([]plannedEdit, error) {
	byFile := make(map[string][]bugs.Defect)
	var fileOrder []string
	for _, d := range defects {
		if _, ok := byFile[d.File]; !ok {
			fileOrder = append(fileOrder, d.File)
		}
		byFile[d.File] = append(byFile[d.File], d)
	}

	var edits []plannedEdit
	for _, file := range fileOrder {
		ds := byFile[file]
		path := filepath.Join(dir, file)
		data, err := os.ReadFile(path) //nolint:gosec // path is dir (already validated by the caller) joined with a Defect.File from our own catalogue, not external input
		if err != nil {
			return nil, fmt.Errorf("defect %s targets %s: %w", ds[0].ID, file, err)
		}
		content := string(data)

		var fileEdits []plannedEdit
		for _, d := range ds {
			count := strings.Count(content, d.Find)
			switch {
			case count == 0:
				return nil, fmt.Errorf("defect %s: Find text not found in %s (the catalogue is stale relative to src/)", d.ID, file)
			case count > 1:
				return nil, fmt.Errorf("defect %s: Find text appears %d times in %s, want exactly 1", d.ID, count, file)
			}
			start := strings.Index(content, d.Find)
			fileEdits = append(fileEdits, plannedEdit{
				defect: d,
				file:   file,
				start:  start,
				end:    start + len(d.Find),
				line:   1 + strings.Count(content[:start], "\n"),
			})
		}

		sort.Slice(fileEdits, func(i, j int) bool { return fileEdits[i].start < fileEdits[j].start })
		for i := 1; i < len(fileEdits); i++ {
			prev, cur := fileEdits[i-1], fileEdits[i]
			if cur.start < prev.end {
				return nil, fmt.Errorf(
					"overlapping edits in %s: %s (bytes [%d,%d)) and %s (bytes [%d,%d)) target overlapping text; these two defects cannot be injected together",
					file, prev.defect.ID, prev.start, prev.end, cur.defect.ID, cur.start, cur.end,
				)
			}
		}
		edits = append(edits, fileEdits...)
	}
	return edits, nil
}

// applyEdits rewrites each affected file in dir in a single pass per
// file, in ascending byte-offset order, so earlier edits never
// invalidate later ones' offsets (all offsets were computed against the
// pre-edit content by planEdits).
func applyEdits(dir string, edits []plannedEdit) error {
	byFile := make(map[string][]plannedEdit)
	var fileOrder []string
	for _, e := range edits {
		if _, ok := byFile[e.file]; !ok {
			fileOrder = append(fileOrder, e.file)
		}
		byFile[e.file] = append(byFile[e.file], e)
	}
	for _, file := range fileOrder {
		es := byFile[file]
		sort.Slice(es, func(i, j int) bool { return es[i].start < es[j].start })
		path := filepath.Join(dir, file)
		data, err := os.ReadFile(path) //nolint:gosec // path is dir joined with a Defect.File from our own catalogue, not external input
		if err != nil {
			return err
		}
		content := string(data)
		var b strings.Builder
		prev := 0
		for _, e := range es {
			b.WriteString(content[prev:e.start])
			b.WriteString(e.defect.Replace)
			prev = e.end
		}
		b.WriteString(content[prev:])
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil { //nolint:gosec // same rationale as writeGoMod; path is validated as above
			return err
		}
	}
	return nil
}

func runGoBuild(ctx context.Context, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

type testEvent struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
}

// runGoTestJSON runs `go test -race -json ./...` in dir and returns the
// final action ("pass", "fail", or "skip") for every test name go test
// printed a result for. A test name absent from the result did not run
// to completion — including every test that a panic in an earlier test
// prevented from starting, since a panic aborts the whole test binary.
// That is deliberate: it lets callers distinguish "this test failed" from
// "this test never got a chance to," rather than conflating them.
func runGoTestJSON(ctx context.Context, dir string) (map[string]string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-race", "-json", "./...")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	results := make(map[string]string)
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var ev testEvent
		if err := dec.Decode(&ev); err != nil {
			break
		}
		if ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			results[ev.Test] = ev.Action
		}
	}

	// A defect can be severe enough to kill the test binary outright --
	// "fatal error: concurrent map writes" is the canonical example, and it
	// is exactly what a dropped mutex produces. The process dies before any
	// test can report, so test2json emits no fail event and the catalogued
	// tests simply go missing. Read literally that looks identical to "the
	// defect did nothing", which is the opposite of the truth: crashing the
	// suite is a stronger detection than failing one test in it.
	//
	// Detect it from the runtime's own markers rather than from a bare
	// non-zero exit, which ordinary test failures also produce.
	crashed := false
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return results, false, fmt.Errorf("running go test: %w (stderr: %s)", runErr, stderr.String())
		}
		combined := stdout.String() + stderr.String()
		for _, marker := range []string{"fatal error:", "panic:", "DATA RACE"} {
			if strings.Contains(combined, marker) {
				crashed = true
				break
			}
		}
	}
	return results, crashed, nil
}

// leafResults returns the subset of results whose test name is not a
// t.Run ancestor of any other name in results, i.e. tests with no
// subtests of their own. A top-level test with subtests always inherits
// their pass/fail status, so keeping only leaves avoids reporting the
// same underlying failure twice under two different names.
func leafResults(results map[string]string) map[string]string {
	hasChild := make(map[string]bool, len(results))
	for a := range results {
		for b := range results {
			if a != b && strings.HasPrefix(b, a+"/") {
				hasChild[a] = true
				break
			}
		}
	}
	leaves := make(map[string]string, len(results))
	for t, action := range results {
		if !hasChild[t] {
			leaves[t] = action
		}
	}
	return leaves
}

// runGolangciLint runs golangci-lint against dir if it is on PATH, using
// the .golangci.yml writeLintConfig placed there. It returns available:
// false, with no error, when golangci-lint isn't installed: lint
// verification is best-effort, never a hard requirement, since the
// underlying go build and go test checks are what every environment can
// run.
func runGolangciLint(ctx context.Context, dir string) (output string, available bool) {
	path, err := exec.LookPath("golangci-lint")
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, lintTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "run", "--max-same-issues=0", "--max-issues-per-linter=0", "./...") //nolint:gosec // path comes from exec.LookPath("golangci-lint") just above, not external input
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput() // non-zero exit means issues were found, which is expected, not a tool failure
	return string(out), true
}

func lintMatches(lintOutput string, d bugs.Defect) bool {
	return strings.Contains(lintOutput, d.File) && strings.Contains(lintOutput, "("+d.Linter+")")
}

// verifyReport is the result of self-checking an injected copy against
// what its defects' catalogue entries claim should happen.
type verifyReport struct {
	BuildOK        bool
	BuildOutput    string
	TestResults    map[string]string
	ExpectedFail   []string
	ActualFail     []string
	MissingFail    []string // catalogued to fail, but didn't: the catalogue is wrong
	CrashedProcess bool     // the defect killed the test binary outright
	CrashedOutFail []string // catalogued to fail, and never got to run because the binary died
	UnexpectedFail []string // failed, but no defect claimed it would: collateral damage
	LintAvailable  bool
	LintOutput     string
	LintExpected   []string // defect IDs whose DetectedBy includes lint
	LintMatched    []string
	LintUnmatched  []string
}

// OK reports whether the injected copy behaved exactly as its defects'
// catalogue entries said it would.
func (r verifyReport) OK() bool {
	return r.BuildOK &&
		len(r.MissingFail) == 0 &&
		len(r.UnexpectedFail) == 0 &&
		len(r.LintUnmatched) == 0
}

func verify(ctx context.Context, dir string, defects []bugs.Defect) (verifyReport, error) {
	report := verifyReport{
		// Always non-nil, even when nothing ends up appended, so
		// injected.json renders these as "[]" rather than "null": an
		// empty-but-present list means "checked, found none," which is
		// different information from a list that was never computed.
		ExpectedFail:   []string{},
		ActualFail:     []string{},
		MissingFail:    []string{},
		UnexpectedFail: []string{},
	}

	buildOut, buildErr := runGoBuild(ctx, dir)
	report.BuildOutput = buildOut
	report.BuildOK = buildErr == nil
	if buildErr != nil {
		return report, fmt.Errorf("go build failed in %s (a defect that breaks the build is a bad defect):\n%s", dir, buildOut)
	}

	results, crashed, err := runGoTestJSON(ctx, dir)
	if err != nil {
		return report, err
	}
	report.TestResults = results
	report.CrashedProcess = crashed

	expected := make(map[string]bool)
	for _, d := range defects {
		for _, t := range d.BreaksTests {
			expected[t] = true
		}
	}
	for t := range expected {
		report.ExpectedFail = append(report.ExpectedFail, t)
	}
	sort.Strings(report.ExpectedFail)

	for _, t := range report.ExpectedFail {
		if results[t] == "fail" {
			continue
		}
		// Absent because the binary died is detection, not a silent
		// catalogue error -- see runGoTestJSON. Absent with a clean exit
		// means the catalogue really is wrong about this defect.
		if crashed && results[t] == "" {
			report.CrashedOutFail = append(report.CrashedOutFail, t)
			continue
		}
		report.MissingFail = append(report.MissingFail, t)
	}
	// go test reports a "fail" action for a container test (e.g.
	// "TestFoo") whenever any of its subtests fail, purely because that
	// is how it aggregates results up the t.Run tree. That aggregate
	// entry carries no information beyond what its leaf subtests already
	// say, so it is excluded here rather than double-counted as its own
	// unexpected failure.
	for t, action := range leafResults(results) {
		if action == "fail" {
			report.ActualFail = append(report.ActualFail, t)
			if !expected[t] {
				report.UnexpectedFail = append(report.UnexpectedFail, t)
			}
		}
	}
	sort.Strings(report.ActualFail)
	sort.Strings(report.UnexpectedFail)

	lintOut, lintAvailable := runGolangciLint(ctx, dir)
	report.LintAvailable = lintAvailable
	report.LintOutput = lintOut
	if lintAvailable {
		for _, d := range defects {
			if d.DetectedBy != bugs.DetectedByLint && d.DetectedBy != bugs.DetectedByBoth {
				continue
			}
			report.LintExpected = append(report.LintExpected, d.ID)
			if lintMatches(lintOut, d) {
				report.LintMatched = append(report.LintMatched, d.ID)
			} else {
				report.LintUnmatched = append(report.LintUnmatched, d.ID)
			}
		}
	}

	return report, nil
}

// injectedDefect is one entry in injected.json's "defects" array.
type injectedDefect struct {
	ID                   string   `json:"id"`
	Class                string   `json:"class"`
	File                 string   `json:"file"`
	Symbol               string   `json:"symbol"`
	Description          string   `json:"description"`
	Line                 int      `json:"line"`
	DetectedBy           string   `json:"detected_by"`
	Linter               string   `json:"linter,omitempty"`
	ExpectedFailingTests []string `json:"expected_failing_tests,omitempty"`
}

// selectionInfo records how the defects in injected.json were chosen, so
// a --random run can be reproduced exactly.
type selectionInfo struct {
	Mode   string   `json:"mode"` // "bugs" or "random"
	Bugs   []string `json:"bugs,omitempty"`
	Random int      `json:"random,omitempty"`
	Seed   *int64   `json:"seed,omitempty"`
}

// verificationInfo is injected.json's record of inject.go's own
// self-check, described in the README's injected.json schema section.
type verificationInfo struct {
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

type injectedManifest struct {
	GeneratedAt  string           `json:"generated_at"`
	Src          string           `json:"src"`
	Out          string           `json:"out"`
	Selection    selectionInfo    `json:"selection"`
	Defects      []injectedDefect `json:"defects"`
	Verification verificationInfo `json:"verification"`
}

func buildManifest(opt options, srcAbs, outAbs string, defects []bugs.Defect, edits []plannedEdit, report verifyReport) injectedManifest {
	lineByID := make(map[string]int, len(edits))
	for _, e := range edits {
		lineByID[e.defect.ID] = e.line
	}

	m := injectedManifest{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Src:         srcAbs,
		Out:         outAbs,
	}

	if opt.random > 0 {
		seed := opt.seed
		m.Selection = selectionInfo{Mode: "random", Random: opt.random, Seed: &seed}
	} else {
		m.Selection = selectionInfo{Mode: "bugs", Bugs: opt.bugs}
	}

	for _, d := range defects {
		m.Defects = append(m.Defects, injectedDefect{
			ID:                   d.ID,
			Class:                d.Class,
			File:                 d.File,
			Symbol:               d.Symbol,
			Description:          d.Description,
			Line:                 lineByID[d.ID],
			DetectedBy:           string(d.DetectedBy),
			Linter:               d.Linter,
			ExpectedFailingTests: d.BreaksTests,
		})
	}

	m.Verification = verificationInfo{
		BuildOK:                report.BuildOK,
		ExpectedFailingTests:   report.ExpectedFail,
		ActualFailingTests:     report.ActualFail,
		MissingFailingTests:    report.MissingFail,
		UnexpectedFailingTests: report.UnexpectedFail,
		LintAvailable:          report.LintAvailable,
		LintExpectedDefects:    report.LintExpected,
		LintMatchedDefects:     report.LintMatched,
		LintUnmatchedDefects:   report.LintUnmatched,
	}
	return m
}

func writeManifest(dir string, m injectedManifest) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // this is a fixture-inspection file, not HTML output; keep "<" and "&" readable
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "injected.json"), buf.Bytes(), 0o644) //nolint:gosec // same rationale as writeGoMod
}

func printSummary(w io.Writer, defects []bugs.Defect, report verifyReport) {
	_, _ = fmt.Fprintf(w, "injected %d defect(s):\n", len(defects))
	for _, d := range defects {
		_, _ = fmt.Fprintf(w, "  %-4s [%-28s] %-24s in %-16s detected by: %s\n", d.ID, d.Class, d.Symbol, d.File, d.DetectedBy)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "build:  %s\n", passFail(report.BuildOK))
	_, _ = fmt.Fprintf(w, "tests:  %d expected failing, %d observed failing\n", len(report.ExpectedFail), len(report.ActualFail))
	if len(report.MissingFail) > 0 {
		_, _ = fmt.Fprintf(w, "  MISSING (catalogued to fail, but passed or did not run): %v\n", report.MissingFail)
	}
	if len(report.UnexpectedFail) > 0 {
		_, _ = fmt.Fprintf(w, "  UNEXPECTED (failed, but no defect claims it): %v\n", report.UnexpectedFail)
	}
	if !report.LintAvailable {
		_, _ = fmt.Fprintln(w, "lint:   golangci-lint not found on PATH; skipped")
	} else {
		_, _ = fmt.Fprintf(w, "lint:   %d expected, %d matched\n", len(report.LintExpected), len(report.LintMatched))
		if len(report.LintUnmatched) > 0 {
			_, _ = fmt.Fprintf(w, "  UNMATCHED (catalogued as lint-detected, but the linter did not fire): %v\n", report.LintUnmatched)
		}
	}
	_, _ = fmt.Fprintln(w)
	if report.OK() {
		_, _ = fmt.Fprintln(w, "result: every injected defect behaved exactly as catalogued")
	} else {
		_, _ = fmt.Fprintln(w, "result: one or more defects did NOT behave as catalogued (see above)")
	}
}

func passFail(ok bool) string {
	if ok {
		return "OK"
	}
	return "FAILED"
}

func run(args []string, stdout, stderr io.Writer) error {
	opt, err := parseArgs(args, stderr)
	if err != nil {
		return err
	}

	if errs := bugs.ValidateAll(); len(errs) > 0 {
		return fmt.Errorf("defect catalogue is internally inconsistent, refusing to run: %w", errors.Join(errs...))
	}
	all := bugs.All()

	selected, err := selectDefects(all, opt)
	if err != nil {
		return err
	}

	if err := checkSafeDestination(opt.src, opt.out); err != nil {
		return err
	}
	srcAbs, err := filepath.Abs(opt.src)
	if err != nil {
		return err
	}
	outAbs, err := filepath.Abs(opt.out)
	if err != nil {
		return err
	}

	if err := os.RemoveAll(outAbs); err != nil {
		return fmt.Errorf("clearing --out: %w", err)
	}
	if err := copyTree(srcAbs, outAbs); err != nil {
		return fmt.Errorf("copying %s to %s: %w", srcAbs, outAbs, err)
	}
	if err := writeGoMod(outAbs); err != nil {
		return fmt.Errorf("writing go.mod into --out: %w", err)
	}
	if err := writeLintConfig(outAbs); err != nil {
		return fmt.Errorf("writing .golangci.yml into --out: %w", err)
	}

	edits, err := planEdits(outAbs, selected)
	if err != nil {
		return err
	}
	if err := applyEdits(outAbs, edits); err != nil {
		return fmt.Errorf("applying edits: %w", err)
	}

	report, verifyErr := verify(context.Background(), outAbs, selected)

	manifest := buildManifest(opt, srcAbs, outAbs, selected, edits, report)
	if writeErr := writeManifest(outAbs, manifest); writeErr != nil {
		return fmt.Errorf("writing injected.json: %w", writeErr)
	}

	if verifyErr != nil {
		return verifyErr
	}

	printSummary(stdout, selected, report)
	if !report.OK() {
		return errors.New("one or more defects did not behave as catalogued; see the summary above and injected.json")
	}
	return nil
}
