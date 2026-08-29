// Package detect identifies what kind of project a directory tree contains.
//
// belay drives polyglot target repositories. Before it can run a test or lint
// node it must know whether it is looking at a Go module, a Node package, a
// Python project, or several at once. Detect answers that question from the
// filesystem alone.
//
// # Design
//
// Detect returns every project it finds, ranked by confidence, rather than a
// single answer. A repository can legitimately be both Go and Node (a Go
// service with a frontend), and only the caller knows which one it meant.
//
// # Cost
//
// Detection is cheap and side-effect free: it never executes a process, never
// opens a network connection, and never writes to disk. The work is one
// os.ReadDir per visited directory plus one bounded read per marker file
// found. The walk is bounded by a maximum depth (DefaultMaxDepth) and a skip
// list of directories that are large and never interesting (node_modules,
// vendor, .git, ...), so cost scales with the shape of the source tree rather
// than with the size of dependency directories.
//
// # Symlinks
//
// Symbolic links are never followed. The walk descends only into entries that
// are real directories, so a link pointing at "/" or back at an ancestor
// cannot produce an unbounded or cyclic walk.
package detect

import (
	"cmp"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// ErrUnknownKind reports a string that does not name a Kind.
var ErrUnknownKind = errors.New("unknown project kind")

// Kind classifies a project by its language ecosystem.
//
// Kind doubles as the value space of the test.runner configuration key, so it
// also carries the two configuration-only values KindAuto and KindCustom.
// Detect never returns those two: they describe what a user asked for, not
// what was found on disk.
type Kind int

const (
	// KindUnknown means no marker file identified the directory.
	KindUnknown Kind = iota
	// KindAuto asks for detection. Configuration-only.
	KindAuto
	// KindGo identifies a Go module, proven by go.mod.
	KindGo
	// KindNode identifies a Node package, proven by package.json.
	KindNode
	// KindPython identifies a Python project, proven by pyproject.toml,
	// setup.py, requirements.txt or tox.ini.
	KindPython
	// KindCustom asks for a user-supplied runner. Configuration-only.
	KindCustom
)

// kindNames maps each Kind to its canonical configuration spelling. The array
// is indexed by Kind, so String and ParseKind cannot drift apart.
var kindNames = [...]string{
	KindUnknown: "unknown",
	KindAuto:    "auto",
	KindGo:      "go",
	KindNode:    "node",
	KindPython:  "python",
	KindCustom:  "custom",
}

// String returns the canonical configuration spelling of k, such as "go" or
// "python". An out-of-range Kind renders as Kind(n) rather than panicking.
func (k Kind) String() string {
	if !k.valid() {
		return fmt.Sprintf("Kind(%d)", int(k))
	}
	return kindNames[k]
}

// valid reports whether k is one of the declared Kind constants.
func (k Kind) valid() bool { return k >= 0 && int(k) < len(kindNames) }

// ParseKind is the inverse of Kind.String. It accepts the configuration
// spellings "auto", "go", "node", "python", "custom" and "unknown",
// ignoring surrounding whitespace and letter case.
//
// ParseKind returns an error wrapping ErrUnknownKind for any other input.
func ParseKind(s string) (Kind, error) {
	normalized := strings.ToLower(strings.TrimSpace(s))
	if normalized == "" {
		return KindUnknown, fmt.Errorf("%w: %q", ErrUnknownKind, s)
	}
	for i, name := range kindNames {
		if name == normalized {
			return Kind(i), nil
		}
	}
	return KindUnknown, fmt.Errorf("%w: %q", ErrUnknownKind, s)
}

// MarshalText implements encoding.TextMarshaler so a Kind round-trips through
// configuration and JSON as its canonical spelling.
func (k Kind) MarshalText() ([]byte, error) {
	if !k.valid() {
		return nil, fmt.Errorf("%w: %d", ErrUnknownKind, int(k))
	}
	return []byte(k.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (k *Kind) UnmarshalText(text []byte) error {
	parsed, err := ParseKind(string(text))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// Confidence ranks how strongly a marker proves a project's kind. Detect sorts
// its results by descending Confidence.
type Confidence int

const (
	// ConfidenceNone is reported only for KindUnknown.
	ConfidenceNone Confidence = iota
	// ConfidenceLow means a marker file was present but could not be parsed.
	// The kind is trustworthy; the details alongside it are not.
	ConfidenceLow
	// ConfidenceMedium means a secondary marker proved the kind, such as a
	// requirements.txt with no pyproject.toml beside it.
	ConfidenceMedium
	// ConfidenceHigh means a primary marker parsed cleanly.
	ConfidenceHigh
)

var confidenceNames = [...]string{
	ConfidenceNone:   "none",
	ConfidenceLow:    "low",
	ConfidenceMedium: "medium",
	ConfidenceHigh:   "high",
}

// String returns a lowercase name for c, such as "high".
func (c Confidence) String() string {
	if c < 0 || int(c) >= len(confidenceNames) {
		return fmt.Sprintf("Confidence(%d)", int(c))
	}
	return confidenceNames[c]
}

// PackageManager names the tool that installs and runs scripts for a Node
// package. The runner adapter needs it to build a command line: "pnpm test"
// is not "npm test".
type PackageManager int

const (
	// PackageManagerUnknown means no package manager could be inferred.
	PackageManagerUnknown PackageManager = iota
	// PackageManagerNPM is npm. It is also the documented fallback when a
	// package.json carries neither a lockfile nor a packageManager field.
	PackageManagerNPM
	// PackageManagerYarn is yarn, proven by yarn.lock.
	PackageManagerYarn
	// PackageManagerPNPM is pnpm, proven by pnpm-lock.yaml.
	PackageManagerPNPM
	// PackageManagerBun is bun, proven by bun.lockb or bun.lock.
	PackageManagerBun
)

var packageManagerNames = [...]string{
	PackageManagerUnknown: "unknown",
	PackageManagerNPM:     "npm",
	PackageManagerYarn:    "yarn",
	PackageManagerPNPM:    "pnpm",
	PackageManagerBun:     "bun",
}

// String returns the executable name of the package manager, such as "pnpm".
func (p PackageManager) String() string {
	if p < 0 || int(p) >= len(packageManagerNames) {
		return fmt.Sprintf("PackageManager(%d)", int(p))
	}
	return packageManagerNames[p]
}

// TestFramework names the JavaScript test framework a Node package uses. The
// runner adapter needs it to parse results and to pass the right flags.
type TestFramework int

const (
	// TestFrameworkUnknown means no framework could be inferred.
	TestFrameworkUnknown TestFramework = iota
	// TestFrameworkJest is jest.
	TestFrameworkJest
	// TestFrameworkVitest is vitest.
	TestFrameworkVitest
	// TestFrameworkMocha is mocha.
	TestFrameworkMocha
	// TestFrameworkNodeTest is the built-in node:test runner (node --test).
	TestFrameworkNodeTest
)

var testFrameworkNames = [...]string{
	TestFrameworkUnknown:  "unknown",
	TestFrameworkJest:     "jest",
	TestFrameworkVitest:   "vitest",
	TestFrameworkMocha:    "mocha",
	TestFrameworkNodeTest: "node:test",
}

// String returns a lowercase name for t, such as "vitest".
func (t TestFramework) String() string {
	if t < 0 || int(t) >= len(testFrameworkNames) {
		return fmt.Sprintf("TestFramework(%d)", int(t))
	}
	return testFrameworkNames[t]
}

// ErrNotDirectory reports that Detect was pointed at something that is not a
// directory.
var ErrNotDirectory = errors.New("not a directory")

// ErrMalformedMarker reports a marker file that exists but could not be fully
// understood. Detect never returns it: it appears as the Err of a Problem
// attached to the degraded Project, so callers can still act on the kind.
var ErrMalformedMarker = errors.New("malformed marker file")

// DefaultMaxDepth is how many directory levels below the scan root Detect
// descends by default. The root is depth 0, so the default reaches projects at
// dir, dir/a, dir/a/b and dir/a/b/c.
const DefaultMaxDepth = 3

// defaultSkipDirs are directory names Detect never descends into. They are
// large, machine-generated, or both; node_modules alone is the difference
// between a scan that takes milliseconds and one that takes minutes.
var defaultSkipDirs = []string{
	".belay",
	".git",
	".venv",
	"__pycache__",
	"build",
	"dist",
	"node_modules",
	"target",
	"vendor",
}

// DefaultSkipDirs returns a copy of the directory names Detect skips by
// default. The copy is safe to modify and pass back through WithSkipDirs.
func DefaultSkipDirs() []string { return slices.Clone(defaultSkipDirs) }

// Problem records a marker file that was found but not fully understood.
//
// A Problem never stops detection. The Project it is attached to still carries
// the Kind the marker proved, at a reduced Confidence. Err wraps
// ErrMalformedMarker for content that could not be parsed, so callers can test
// it with errors.Is.
type Problem struct {
	// File is the marker file the problem concerns, relative to the
	// Project's Root. A problem about several files at once, such as two
	// lockfiles that disagree, names them comma-separated.
	File string
	// Err is the cause.
	Err error
}

// String renders the problem as "file: cause".
func (p Problem) String() string {
	if p.Err == nil {
		return p.File
	}
	return p.File + ": " + p.Err.Error()
}

// Project is one detected project: a directory whose marker files identify it
// as belonging to a language ecosystem.
//
// Exactly one of Go, Node and Python is non-nil, matching Kind. All three are
// nil when Kind is KindUnknown.
type Project struct {
	// Kind is the ecosystem the markers proved.
	Kind Kind
	// Root is the directory holding the markers. It is formed by joining the
	// directory passed to Detect with the path below it, so an absolute
	// argument yields absolute roots.
	Root string
	// Markers are the base names of the files that proved the Kind, in a
	// stable canonical order.
	Markers []string
	// Confidence ranks this detection against the others in the result.
	Confidence Confidence
	// Problems lists marker files that could not be fully parsed.
	Problems []Problem

	// Go carries Go module detail. Non-nil exactly when Kind is KindGo.
	Go *GoInfo
	// Node carries Node package detail. Non-nil exactly when Kind is KindNode.
	Node *NodeInfo
	// Python carries Python project detail. Non-nil exactly when Kind is
	// KindPython.
	Python *PythonInfo
}

// GoInfo describes a Go module.
type GoInfo struct {
	// Module is the module path from the module directive, empty if go.mod
	// had none.
	Module string
	// GoVersion is the version from the go directive, such as "1.26.3",
	// empty if go.mod had none.
	GoVersion string
}

// NodeInfo describes a Node package. PackageManager and TestFramework are what
// the runner adapter needs to build a command line.
type NodeInfo struct {
	// Name is the package.json name field.
	Name string
	// PackageManager is the tool that runs scripts for this package.
	PackageManager PackageManager
	// PackageManagerVersion is the version pinned by the packageManager
	// field, such as "8.6.0", empty when the field is absent.
	PackageManagerVersion string
	// TestFramework is the framework the test script invokes.
	TestFramework TestFramework
	// TestScript is the raw scripts.test string, empty when unset.
	TestScript string
	// HasESLint reports whether eslint is declared or configured.
	HasESLint bool
}

// PythonInfo describes a Python project.
type PythonInfo struct {
	// HasPytest reports whether pytest appears in the project's declared
	// dependencies or tool configuration.
	HasPytest bool
	// HasRuff reports whether ruff is declared or configured.
	HasRuff bool
}

// Option customizes Detect.
type Option func(*config)

// WithMaxDepth bounds how far below the scan root Detect descends. The root is
// depth 0, so WithMaxDepth(0) inspects only the root directory. A negative
// depth is clamped to 0.
func WithMaxDepth(depth int) Option {
	return func(c *config) { c.maxDepth = max(depth, 0) }
}

// WithSkipDirs replaces the default skip list with names. Calling it with no
// names disables skipping entirely, which will descend into node_modules and
// .git; do that only on a tree known to be small.
//
// The list governs descent only. The directory passed to Detect is always
// scanned, even when its own name is on the list.
func WithSkipDirs(names ...string) Option {
	return func(c *config) {
		c.skip = make(map[string]struct{}, len(names))
		for _, name := range names {
			c.skip[name] = struct{}{}
		}
	}
}

// config is the resolved effect of the options passed to Detect.
type config struct {
	maxDepth int
	skip     map[string]struct{}
}

func newConfig(opts []Option) config {
	cfg := config{maxDepth: DefaultMaxDepth, skip: make(map[string]struct{}, len(defaultSkipDirs))}
	for _, name := range defaultSkipDirs {
		cfg.skip[name] = struct{}{}
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// Detect reports every project under dir, ranked strongest first.
//
// The result is never empty: a directory with no recognizable marker yields a
// single Project with Kind KindUnknown. Results are ordered by descending
// Confidence, then by ascending Root, then by ascending Kind, so two runs over
// the same tree always agree.
//
// Detect returns an error only when dir itself cannot be used: it does not
// exist, or is not a directory. A marker file that cannot be read or parsed
// degrades that project's Confidence and is reported in its Problems.
//
// Detection executes no process and opens no network connection. The cost is
// one directory read per visited directory plus one bounded file read per
// marker found, with the walk bounded by WithMaxDepth and WithSkipDirs.
func Detect(dir string, opts ...Option) ([]Project, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("detect %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("detect %s: %w", dir, ErrNotDirectory)
	}

	root := filepath.Clean(dir)
	s := &scanner{cfg: newConfig(opts)}
	s.walk(root, 0)
	return s.result(root), nil
}

// scanner accumulates projects while walking a tree. It counts the directories
// it reads so tests can prove the skip list is honored without timing anything.
type scanner struct {
	cfg      config
	projects []Project
	dirsRead int
}

// walk reads dir, records any projects rooted there, and descends into real
// subdirectories until the depth bound is reached.
//
// Symbolic links are never followed, in either direction: a linked directory is
// not descended into and a linked marker file is not read. A link pointing at
// "/" or back at an ancestor therefore cannot produce an unbounded or cyclic
// walk. Directories that cannot be read are skipped.
func (s *scanner) walk(dir string, depth int) {
	s.dirsRead++

	entries, _ := os.ReadDir(dir)
	files := make(map[string]struct{}, len(entries))
	var subdirs []string
	for _, entry := range entries {
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			continue
		case entry.IsDir():
			if _, skipped := s.cfg.skip[entry.Name()]; !skipped {
				subdirs = append(subdirs, entry.Name())
			}
		case entry.Type().IsRegular():
			files[entry.Name()] = struct{}{}
		}
	}

	s.inspect(dir, files)

	if depth >= s.cfg.maxDepth {
		return
	}
	for _, name := range subdirs {
		s.walk(filepath.Join(dir, name), depth+1)
	}
}

// inspect records every project rooted at dir, given the regular files there.
// A directory can yield more than one project: a Go service with its frontend
// checked in beside it is both a Go and a Node project.
func (s *scanner) inspect(dir string, files map[string]struct{}) {
	if project, ok := detectGo(dir, files); ok {
		s.projects = append(s.projects, project)
	}
	if project, ok := detectNode(dir, files); ok {
		s.projects = append(s.projects, project)
	}
	if project, ok := detectPython(dir, files); ok {
		s.projects = append(s.projects, project)
	}
}

// maxMarkerBytes bounds how much of a marker file is read. Real markers are
// kilobytes; the bound keeps a pathological file from costing memory. A file
// truncated at the bound simply fails to parse and degrades like any other
// malformed marker.
const maxMarkerBytes = 1 << 20

// readMarker reads at most maxMarkerBytes of path.
func readMarker(path string) ([]byte, error) {
	// #nosec G304 -- path is a marker file name from the fixed tables in this
	// package, joined onto a directory the walk reached below the root the
	// caller named. The walk records only regular files, so path can never be
	// a symbolic link out of that tree.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, maxMarkerBytes))
}

// goMarker is the file that proves a Go module.
const goMarker = "go.mod"

// detectGo reports a Go module rooted at dir, if go.mod is there.
//
// go.mod is read as text, never by shelling out to the go command: detection
// must work without a Go toolchain installed, and must not cost a process
// spawn per candidate directory.
func detectGo(dir string, files map[string]struct{}) (Project, bool) {
	if _, ok := files[goMarker]; !ok {
		return Project{}, false
	}

	project := Project{
		Kind:       KindGo,
		Root:       dir,
		Markers:    []string{goMarker},
		Confidence: ConfidenceHigh,
		Go:         &GoInfo{},
	}

	content, err := readMarker(filepath.Join(dir, goMarker))
	if err != nil {
		project.Confidence = ConfidenceLow
		project.Problems = append(project.Problems, Problem{File: goMarker, Err: err})
		return project, true
	}

	project.Go.Module, project.Go.GoVersion = parseGoMod(string(content))
	if project.Go.Module == "" {
		project.Confidence = ConfidenceLow
		project.Problems = append(project.Problems, Problem{
			File: goMarker,
			Err:  fmt.Errorf("%w: go.mod has no module directive", ErrMalformedMarker),
		})
	}
	return project, true
}

// parseGoMod extracts the module path and the go directive version from go.mod
// source. Missing directives come back empty rather than as an error, which is
// what lets a go.mod with no module line still count as a Go project.
//
// The grammar handled is the one in the Go modules reference:
//
//	ModuleDirective = "module" ( ModulePath | "(" newline ModulePath newline ")" ) newline .
//	GoDirective     = "go" GoVersion newline .
//
// Both operands may be quoted, and comments start with "//" and run to the end
// of the line. Directives this package does not need are ignored.
func parseGoMod(content string) (module, goVersion string) {
	inModuleBlock := false
	for line := range strings.Lines(content) {
		if comment := strings.Index(line, "//"); comment >= 0 {
			line = line[:comment]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		if inModuleBlock {
			if fields[0] == ")" {
				inModuleBlock = false
			} else if module == "" {
				module = unquoteToken(fields[0])
			}
			continue
		}

		switch fields[0] {
		case "module":
			if len(fields) < 2 {
				continue
			}
			if fields[1] == "(" {
				inModuleBlock = true
			} else if module == "" {
				module = unquoteToken(fields[1])
			}
		case "go":
			if len(fields) >= 2 && goVersion == "" {
				goVersion = unquoteToken(fields[1])
			}
		}
	}
	return module, goVersion
}

// unquoteToken removes Go string quoting from a go.mod operand, leaving an
// unquotable token as it found it.
func unquoteToken(token string) string {
	if len(token) < 2 {
		return token
	}
	if token[0] != '"' && token[0] != '`' {
		return token
	}
	if unquoted, err := strconv.Unquote(token); err == nil {
		return unquoted
	}
	return strings.Trim(token, "\"`")
}

// result sorts the accumulated projects, substituting the unknown placeholder
// when nothing was found.
func (s *scanner) result(root string) []Project {
	if len(s.projects) == 0 {
		return []Project{{Kind: KindUnknown, Root: root, Confidence: ConfidenceNone}}
	}
	slices.SortStableFunc(s.projects, compareProjects)
	return s.projects
}

// compareProjects orders by descending Confidence, then ascending Root, then
// ascending Kind. No two projects share a Root and a Kind, so the order is
// total and independent of filesystem iteration order.
func compareProjects(a, b Project) int {
	if c := cmp.Compare(b.Confidence, a.Confidence); c != 0 {
		return c
	}
	if c := strings.Compare(filepath.ToSlash(a.Root), filepath.ToSlash(b.Root)); c != 0 {
		return c
	}
	return cmp.Compare(a.Kind, b.Kind)
}

// nodeMarker is the file that proves a Node package.
const nodeMarker = "package.json"

// lockfiles maps each lockfile to the package manager it proves, in the order
// markers are reported. Sources: pnpm commits pnpm-lock.yaml
// (https://pnpm.io/git), yarn keeps yarn.lock
// (https://classic.yarnpkg.com/lang/en/docs/yarn-lock/), bun writes the text
// bun.lock since v1.2 and the binary bun.lockb before it
// (https://bun.com/docs/pm/lockfile), and npm writes package-lock.json or the
// publishable npm-shrinkwrap.json
// (https://docs.npmjs.com/cli/v11/configuring-npm/package-json).
var lockfiles = []struct {
	file    string
	manager PackageManager
}{
	{file: "bun.lock", manager: PackageManagerBun},
	{file: "bun.lockb", manager: PackageManagerBun},
	{file: "npm-shrinkwrap.json", manager: PackageManagerNPM},
	{file: "package-lock.json", manager: PackageManagerNPM},
	{file: "pnpm-lock.yaml", manager: PackageManagerPNPM},
	{file: "yarn.lock", manager: PackageManagerYarn},
}

// frameworkTokens maps a dependency name, which is also the command a test
// script runs, to the framework it identifies. node:test is absent because it
// ships with Node and can only be recognized from the script.
var frameworkTokens = []struct {
	token     string
	framework TestFramework
}{
	{token: "jest", framework: TestFrameworkJest},
	{token: "vitest", framework: TestFrameworkVitest},
	{token: "mocha", framework: TestFrameworkMocha},
}

// packageJSON is the subset of package.json this package reads.
type packageJSON struct {
	Name            string            `json:"name"`
	PackageManager  string            `json:"packageManager"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

// detectNode reports a Node package rooted at dir, if package.json is there.
//
// The package manager and the test framework are both resolved here because
// the runner adapter cannot build a command line without them: "pnpm test" is
// not "npm test", and a vitest run is not a jest run.
func detectNode(dir string, files map[string]struct{}) (Project, bool) {
	if _, ok := files[nodeMarker]; !ok {
		return Project{}, false
	}

	info := &NodeInfo{}
	project := Project{
		Kind:       KindNode,
		Root:       dir,
		Markers:    []string{nodeMarker},
		Confidence: ConfidenceHigh,
		Node:       info,
	}

	// Lockfiles are read from the directory listing, so a package.json that
	// does not parse still yields a package manager.
	locked, lockMarkers := managersFromLockfiles(files)
	project.Markers = append(project.Markers, lockMarkers...)
	if len(locked) > 1 {
		project.Problems = append(project.Problems, Problem{
			File: strings.Join(lockMarkers, ", "),
			Err:  fmt.Errorf("%w: lockfiles disagree on the package manager", ErrMalformedMarker),
		})
	}

	var pkg packageJSON
	if content, err := readMarker(filepath.Join(dir, nodeMarker)); err != nil {
		project.Confidence = ConfidenceLow
		project.Problems = append(project.Problems, Problem{File: nodeMarker, Err: err})
	} else if err := json.Unmarshal(content, &pkg); err != nil {
		project.Confidence = ConfidenceLow
		project.Problems = append(project.Problems, Problem{
			File: nodeMarker,
			Err:  fmt.Errorf("%w: %w", ErrMalformedMarker, err),
		})
	}

	info.Name = pkg.Name
	info.TestScript = pkg.Scripts["test"]
	info.TestFramework = frameworkFor(info.TestScript, pkg.DevDependencies, pkg.Dependencies)
	info.HasESLint = hasESLint(pkg, files)

	declared, version := parsePackageManagerField(pkg.PackageManager)
	switch {
	case declared != PackageManagerUnknown:
		info.PackageManager = declared
		info.PackageManagerVersion = version
		if len(locked) > 0 && !slices.Contains(locked, declared) {
			project.Problems = append(project.Problems, Problem{
				File: nodeMarker,
				Err: fmt.Errorf("%w: packageManager names %s but %s is present",
					ErrMalformedMarker, declared, strings.Join(lockMarkers, ", ")),
			})
		}
	case len(locked) > 0:
		info.PackageManager = locked[0]
	default:
		info.PackageManager = PackageManagerNPM
	}

	return project, true
}

// managersFromLockfiles reports the distinct package managers proven by the
// lockfiles in files, and the lockfile names themselves as markers.
func managersFromLockfiles(files map[string]struct{}) ([]PackageManager, []string) {
	var managers []PackageManager
	var markers []string
	for _, lock := range lockfiles {
		if _, ok := files[lock.file]; !ok {
			continue
		}
		markers = append(markers, lock.file)
		if !slices.Contains(managers, lock.manager) {
			managers = append(managers, lock.manager)
		}
	}
	return managers, markers
}

// parsePackageManagerField reads the package.json packageManager field, whose
// value is a name, a version, and an optional integrity hash, as in
// "yarn@3.2.3+sha224.953c8233...". Corepack defines the field for npm, yarn
// and pnpm (https://github.com/nodejs/corepack#readme); bun is accepted here
// too because it names itself the same way.
func parsePackageManagerField(field string) (PackageManager, string) {
	name, version, _ := strings.Cut(strings.TrimSpace(field), "@")
	version, _, _ = strings.Cut(version, "+")

	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || name == packageManagerNames[PackageManagerUnknown] {
		return PackageManagerUnknown, ""
	}
	for i, known := range packageManagerNames {
		if known == name {
			return PackageManager(i), strings.TrimSpace(version)
		}
	}
	return PackageManagerUnknown, ""
}

// frameworkFor resolves the test framework from the test script first, because
// that is the command that actually runs, and falls back to the declared
// dependencies when the script names no framework it recognizes.
func frameworkFor(testScript string, devDependencies, dependencies map[string]string) TestFramework {
	if framework := frameworkFromScript(testScript); framework != TestFrameworkUnknown {
		return framework
	}
	for _, deps := range []map[string]string{devDependencies, dependencies} {
		for _, entry := range frameworkTokens {
			if _, ok := deps[entry.token]; ok {
				return entry.framework
			}
		}
	}
	return TestFrameworkUnknown
}

// frameworkFromScript matches the commands in a package script against the
// known frameworks. Matching whole command words rather than substrings keeps
// a path such as jest.config.js from being mistaken for an invocation. The
// built-in runner is recognized as node with --test
// (https://nodejs.org/api/test.html). The first framework named wins.
func frameworkFromScript(script string) TestFramework {
	sawNode := false
	for _, field := range strings.FieldsFunc(script, isScriptSeparator) {
		command := path.Base(field)
		if command == "node" || command == "node.exe" {
			sawNode = true
		}
		if sawNode && (field == "--test" || strings.HasPrefix(field, "--test=")) {
			return TestFrameworkNodeTest
		}
		for _, entry := range frameworkTokens {
			if command == entry.token {
				return entry.framework
			}
		}
	}
	return TestFrameworkUnknown
}

// isScriptSeparator reports whether r separates two words of a shell command.
func isScriptSeparator(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("&|;\"'", r)
}

// hasESLint reports whether eslint is declared as a dependency or configured
// by a file in the package directory. ESLint reads eslint.config.js and its
// .mjs, .cjs, .ts, .mts and .cts variants
// (https://eslint.org/docs/latest/use/configure/configuration-files), and the
// legacy .eslintrc family before that, so both name prefixes count.
func hasESLint(pkg packageJSON, files map[string]struct{}) bool {
	const eslint = "eslint"
	if _, ok := pkg.DevDependencies[eslint]; ok {
		return true
	}
	if _, ok := pkg.Dependencies[eslint]; ok {
		return true
	}
	for file := range files {
		if strings.HasPrefix(file, "eslint.config.") || strings.HasPrefix(file, ".eslintrc") {
			return true
		}
	}
	return false
}

// pythonMarkers are the files that prove a Python project, strongest first. A
// primary marker declares a project; requirements.txt and tox.ini only imply
// one, so on their own they yield ConfidenceMedium.
var pythonMarkers = []struct {
	file    string
	primary bool
}{
	{file: "pyproject.toml", primary: true},
	{file: "setup.py", primary: true},
	{file: "requirements.txt"},
	{file: "tox.ini"},
}

// ruffConfigFiles are the standalone ruff configuration files. Ruff is
// configured by pyproject.toml, ruff.toml or .ruff.toml
// (https://docs.astral.sh/ruff/configuration/); the pyproject.toml case is
// covered by the [tool.ruff] table found in the text scan.
var ruffConfigFiles = []string{".ruff.toml", "ruff.toml"}

// detectPython reports a Python project rooted at dir, if any Python marker is
// there.
//
// The markers are scanned as text rather than parsed: there is no TOML parser
// in the standard library, and the two questions the linter and runner
// adapters ask - is pytest in play, is ruff configured - are answered by the
// presence of a name. pytest is configured by a [tool.pytest.ini_options]
// table in pyproject.toml or a [pytest] section in tox.ini
// (https://docs.pytest.org/en/stable/reference/customize.html), and both forms
// mention the name.
func detectPython(dir string, files map[string]struct{}) (Project, bool) {
	var markers []string
	primary := false
	for _, marker := range pythonMarkers {
		if _, ok := files[marker.file]; ok {
			markers = append(markers, marker.file)
			primary = primary || marker.primary
		}
	}
	if len(markers) == 0 {
		return Project{}, false
	}

	info := &PythonInfo{}
	project := Project{
		Kind:       KindPython,
		Root:       dir,
		Markers:    markers,
		Confidence: ConfidenceMedium,
		Python:     info,
	}
	if primary {
		project.Confidence = ConfidenceHigh
	}

	for _, marker := range markers {
		content, err := readMarker(filepath.Join(dir, marker))
		if err != nil {
			project.Confidence = ConfidenceLow
			project.Problems = append(project.Problems, Problem{File: marker, Err: err})
			continue
		}
		text := string(content)
		info.HasPytest = info.HasPytest || mentionsName(text, "pytest")
		info.HasRuff = info.HasRuff || mentionsName(text, "ruff")
	}

	for _, config := range ruffConfigFiles {
		if _, ok := files[config]; ok {
			info.HasRuff = true
		}
	}

	return project, true
}

// mentionsName reports whether text names the given tool.
//
// A name matches only where it is not adjacent to another letter or digit, so
// "pytest-cov", "pytest_asyncio" and "[tool.pytest.ini_options]" all count as
// pytest while "mypytest" does not. Everything after a '#' on a line is
// ignored, which is the comment syntax of all four Python markers; a '#'
// inside a quoted string therefore hides the rest of that line.
func mentionsName(text, name string) bool {
	for line := range strings.Lines(text) {
		if comment := strings.IndexByte(line, '#'); comment >= 0 {
			line = line[:comment]
		}
		for offset := 0; offset <= len(line)-len(name); {
			index := strings.Index(line[offset:], name)
			if index < 0 {
				break
			}
			start := offset + index
			if isNameBoundary(line, start-1) && isNameBoundary(line, start+len(name)) {
				return true
			}
			offset = start + 1
		}
	}
	return false
}

// isNameBoundary reports whether index falls outside line or on a byte that
// cannot be part of a package name.
func isNameBoundary(line string, index int) bool {
	if index < 0 || index >= len(line) {
		return true
	}
	c := line[index]
	return (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9')
}

// Compile-time proof that Kind round-trips through the stdlib text interfaces.
var (
	_ encoding.TextMarshaler   = KindGo
	_ encoding.TextUnmarshaler = (*Kind)(nil)
	_ fmt.Stringer             = KindGo
	_ fmt.Stringer             = ConfidenceHigh
	_ fmt.Stringer             = PackageManagerNPM
	_ fmt.Stringer             = TestFrameworkJest
)
