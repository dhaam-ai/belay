// Package config defines belay's on-disk configuration, its built-in
// defaults, and the rules that decide whether a configuration is usable.
//
// # Layers
//
// A configuration is always the result of merging layers, lowest priority
// first:
//
//	built-in defaults
//	$XDG_CONFIG_HOME/belay/config.yaml
//	./belay.yaml
//	--config <path>
//
// Merging is per field, not per document. A file that sets only
// budget.max_usd keeps every other default, including its siblings inside
// budget. An absent or empty file therefore yields exactly Default().
//
// # Strictness
//
// Decoding is strict: a key that does not name a field is an error rather
// than a silently ignored typo, and a document that names a credential
// field is rejected outright. Secrets are read from the environment
// (SONAR_TOKEN, ANTHROPIC_API_KEY) and never from a file that people commit.
//
// # Drift
//
// Digest returns a stable fingerprint of a configuration so a resumed run
// can prove it is resuming under the same settings it started with.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Version is the only configuration schema version belay understands.
const Version = 1

// Sentinel errors reported by this package. Every error returned by Load,
// Decode and Validate wraps exactly one of them, so callers can branch with
// errors.Is even when several errors are joined together.
var (
	// ErrInvalid reports a field whose value cannot be used.
	ErrInvalid = errors.New("invalid configuration")
	// ErrUnsupportedVersion reports a version key belay cannot read.
	ErrUnsupportedVersion = errors.New("unsupported configuration version")
	// ErrSecret reports a credential written into a configuration file.
	ErrSecret = errors.New("secret in configuration file")
	// ErrUnknownField reports a key that does not name a configuration field.
	ErrUnknownField = errors.New("unknown configuration field")
	// ErrMalformed reports a file that is not a readable YAML document.
	ErrMalformed = errors.New("malformed configuration file")
	// ErrNotFound reports an explicitly requested file that does not exist.
	ErrNotFound = errors.New("configuration file not found")
)

// AgentMode selects how the agent backend is driven.
type AgentMode string

// Agent modes.
const (
	// AgentModeLive invokes the real agent CLI.
	AgentModeLive AgentMode = "live"
	// AgentModeRecord invokes the real agent CLI and records its traffic.
	AgentModeRecord AgentMode = "record"
	// AgentModeReplay serves previously recorded traffic and invokes nothing.
	AgentModeReplay AgentMode = "replay"
)

// TestRunner selects which test command a run executes.
type TestRunner string

// Test runners.
const (
	// TestRunnerAuto picks a runner from the shape of the target repository.
	TestRunnerAuto TestRunner = "auto"
	// TestRunnerGo runs the Go toolchain.
	TestRunnerGo TestRunner = "go"
	// TestRunnerNode runs the Node toolchain.
	TestRunnerNode TestRunner = "node"
	// TestRunnerPython runs the Python toolchain.
	TestRunnerPython TestRunner = "python"
	// TestRunnerCustom runs Test.CustomCmd verbatim.
	TestRunnerCustom TestRunner = "custom"
)

// ReviewMode selects which reviewer gates a run.
type ReviewMode string

// Review modes.
const (
	// ReviewModeLint reviews with the target repository's linters.
	ReviewModeLint ReviewMode = "lint"
	// ReviewModeSonar reviews with a SonarQube scan.
	ReviewModeSonar ReviewMode = "sonar"
	// ReviewModeAI reviews with an agent driving a Sonar MCP server.
	ReviewModeAI ReviewMode = "ai"
)

// Severity names an issue severity in Sonar's ordering, worst first.
type Severity string

// Severities, from worst to least severe.
const (
	// SeverityBlocker is the worst severity.
	SeverityBlocker Severity = "blocker"
	// SeverityCritical is the second-worst severity.
	SeverityCritical Severity = "critical"
	// SeverityMajor is the default gate.
	SeverityMajor Severity = "major"
	// SeverityMinor is a low severity.
	SeverityMinor Severity = "minor"
	// SeverityInfo is the least severe level.
	SeverityInfo Severity = "info"
)

// MCPMode selects how the Sonar MCP server is reached.
type MCPMode string

// MCP transports.
const (
	// MCPModeCloud talks to SonarQube Cloud's hosted MCP endpoint.
	MCPModeCloud MCPMode = "cloud"
	// MCPModeDocker starts the MCP server from a container image.
	MCPModeDocker MCPMode = "docker"
	// MCPModeServer talks to an MCP server already running at MCP.URL.
	MCPModeServer MCPMode = "server"
)

// OnExceed selects what a run does when it crosses a budget ceiling.
type OnExceed string

// Budget policies.
const (
	// OnExceedAbort stops the run at the ceiling.
	OnExceedAbort OnExceed = "abort"
	// OnExceedWarn records the overrun and continues.
	OnExceedWarn OnExceed = "warn"
)

// IsolatorName selects how a fanout candidate's workspace is separated from
// the run's own.
type IsolatorName string

// Workspace isolators.
const (
	// IsolatorAuto lets belay choose: a git worktree when the run works on
	// a branch, a directory copy otherwise.
	IsolatorAuto IsolatorName = "auto"
	// IsolatorWorktree gives each candidate a git worktree on a branch of
	// its own, so a winner can be merged.
	IsolatorWorktree IsolatorName = "worktree"
	// IsolatorDircopy gives each candidate a directory copy, which works on
	// a target that is not a git repository (ADR-0004).
	IsolatorDircopy IsolatorName = "dircopy"
)

// OutputMode selects how a run delivers its result.
//
// It is not state.OutputMode, which records what a run actually does. Only
// this one has "auto", which is a question rather than an answer, and the
// question is asked once, when a run is created. state's own documentation
// says why a resumed run must never re-ask it.
type OutputMode string

// Output modes.
const (
	// OutputAuto lets belay choose: a branch when the workspace can take
	// one, editing in place otherwise.
	OutputAuto OutputMode = "auto"
	// OutputBranch works in the run's own git worktree, on its own branch,
	// and commits once the quality gate has passed.
	OutputBranch OutputMode = "branch"
	// OutputInPlace edits the workspace directly, which is what every belay
	// before branch output did.
	OutputInPlace OutputMode = "in_place"
)

// SelectStrategy selects which fanout candidate wins.
type SelectStrategy string

// Candidate selection strategies.
const (
	// SelectFewestIssues picks the candidate with the fewest review issues.
	SelectFewestIssues SelectStrategy = "fewest_issues"
	// SelectFirstPass picks the first candidate that clears the gate.
	SelectFirstPass SelectStrategy = "first_pass"
	// SelectFastest picks the candidate that finished soonest.
	SelectFastest SelectStrategy = "fastest"
)

// Config is a complete belay configuration.
//
// The zero Config is not usable; start from Default or Load.
type Config struct {
	// Version is the schema version and must equal [Version].
	Version int `yaml:"version"`
	// Agent configures the coding agent belay drives.
	Agent Agent `yaml:"agent"`
	// Graph configures the dispatcher's state machine.
	Graph Graph `yaml:"graph"`
	// Test configures the test node.
	Test Test `yaml:"test"`
	// Review configures the review gate.
	Review Review `yaml:"review"`
	// Budget configures the spend ceilings.
	Budget Budget `yaml:"budget"`
	// Fanout configures best-of-N candidate generation.
	Fanout Fanout `yaml:"fanout"`
	// Output configures how a run delivers its result.
	Output Output `yaml:"output"`
	// Plan configures what becomes of a run's plan once the run is over.
	Plan Plan `yaml:"plan"`
	// Policy configures the guardrails a run's change must obey.
	Policy Policy `yaml:"policy"`
}

// Policy is the rules a run's change must obey whatever its tests say: the
// paths it may not touch, how big it may be, and what it may not contain.
//
// It is last in Config, and so last in a rendered belay.yaml, for the reason
// Output gives: sections are appended in the order they arrived.
//
// Every field defaults to nothing, so a repository that declares no policy
// runs exactly as it always did and never reaches a gate. The rules are read
// once, when a run is created, and pinned in the run's manifest; ADR-0014
// records why a guardrail may not be read live when a model route may.
type Policy struct {
	// ForbiddenPaths are path globs the change may not touch. See
	// internal/glob for what a pattern means.
	ForbiddenPaths []string `yaml:"forbidden_paths"`
	// MaxChangedFiles is the most files one run's change may touch. Zero
	// means no ceiling.
	MaxChangedFiles int `yaml:"max_changed_files"`
	// MaxChangedLines is the most lines the files a change touches may hold
	// together. Zero means no ceiling.
	MaxChangedLines int `yaml:"max_changed_lines"`
	// NoNewDependencies forbids declaring a dependency the workspace did not
	// declare when the run was created.
	NoNewDependencies bool `yaml:"no_new_dependencies"`
	// RequiredTests are the rules that make a change of one kind bring a
	// change of another with it.
	RequiredTests []TestRule `yaml:"required_tests"`
	// ForbiddenContent are the regular expressions a changed file may not
	// match.
	ForbiddenContent []ContentRule `yaml:"forbidden_content"`
}

// TestRule requires that a change touching a file matching When also touches
// a file matching Require -- typically, that a change to a source file brings
// a change to a test.
type TestRule struct {
	// When selects the changed files the rule is about, such as "**/*.go".
	When string `yaml:"when"`
	// Require is what the change must also touch, such as "**/*_test.go".
	Require string `yaml:"require"`
}

// ContentRule forbids a regular expression in the changed files it covers.
type ContentRule struct {
	// Pattern is a Go regular expression, matched line by line.
	Pattern string `yaml:"pattern"`
	// Paths limits the rule to the changed files matching one of these path
	// globs. Empty means every changed file.
	Paths []string `yaml:"paths"`
	// Reason is what a person is told when the rule fires, in place of the
	// pattern. A bare regular expression explains nothing to whoever has to
	// act on it.
	Reason string `yaml:"reason"`
}

// Plan configures what becomes of a run's plan once the run is over.
//
// It follows Output in Config, and so in a rendered belay.yaml, for the
// reason Output gives: sections are appended in the order they arrived.
//
// Both settings are read once, when a run is created, and recorded in the
// run's manifest (internal/state's ArchivedPlan), so a resumed run files its
// plan where the run was told it would when it started. ADR-0016 records why
// archival is on by default and where it happens.
type Plan struct {
	// Archive moves a finished run's approved plan out of the run directory
	// and into the repository.
	Archive bool `yaml:"archive"`
	// ArchiveDir is where it goes: a slash-separated directory relative to
	// the workspace, which Validate keeps inside it.
	ArchiveDir string `yaml:"archive_dir"`
}

// Output configures how a run delivers its result.
//
// It is last in Config, and so last in a rendered belay.yaml, because every
// section before it was there first and a reader's eye should not have to
// re-learn the file's order to find a setting they already knew.
type Output struct {
	// Mode selects branch output, in-place editing, or letting belay choose.
	Mode OutputMode `yaml:"mode"`
}

// Agent configures the coding agent CLI belay drives.
type Agent struct {
	// Backend names the agent CLI adapter, for example "claude-code".
	Backend string `yaml:"backend"`
	// Model names the model the backend should use.
	Model string `yaml:"model"`
	// Models routes individual nodes to models of their own.
	Models Models `yaml:"models,omitempty"`
	// MaxTurns caps the agent turns allowed inside a single node.
	MaxTurns int `yaml:"max_turns"`
	// Mode selects live, record or replay execution.
	Mode AgentMode `yaml:"mode"`
}

// ModelFor returns the model the backend is told to use when the node named
// node calls the agent: the one agent.models routes it to, or agent.model.
//
// It exists so that routing, sending different nodes of one run to
// different models, has exactly one place to live. Every node that builds an
// agent request asks here instead of reading a.Model itself, so routing
// changed this body and no node. It takes the node's name rather than a graph
// type because internal/graph imports this package; callers pass the
// graph.Node* constants.
//
// It deliberately does not validate node. An unknown name, the empty string
// included, gets a.Model like any other rather than an empty model, which is
// how an agent request asks for the backend's own default: a model the user
// never chose, and one the cost estimator has no name to price by. The name
// belay cannot route has already been refused by [Config.Validate], before
// the run started; failing here instead would fail it mid-run, after the
// steps before this one had been paid for.
//
// The result depends only on a and node, so a node the dispatcher re-runs on
// resume asks for the same model the interrupted attempt did, and two nodes
// of one run may hold different answers at the same time.
func (a Agent) ModelFor(node string) string {
	if model := a.Models[node]; model != "" {
		return model
	}
	return a.Model
}

// Graph configures the dispatcher's state machine.
type Graph struct {
	// GiveUp caps how many times the fix loop may retry a failing gate.
	GiveUp int `yaml:"give_up"`
	// MaxSteps caps total node executions in one run.
	MaxSteps int `yaml:"max_steps"`
	// NodeTimeout caps the wall-clock time of a single node.
	NodeTimeout Duration `yaml:"node_timeout"`
	// Approval requires a human to approve the plan before coding starts.
	Approval bool `yaml:"approval"`
}

// Test configures how a run proves the code works.
type Test struct {
	// Runner selects the test toolchain, or "auto" to detect one.
	Runner TestRunner `yaml:"runner"`
	// CustomCmd is the command run when Runner is "custom".
	CustomCmd string `yaml:"custom_cmd"`
}

// Review configures the quality gate that guards a run's output.
type Review struct {
	// Mode selects the reviewer.
	Mode ReviewMode `yaml:"mode"`
	// FailOn is the least severity that fails the gate.
	FailOn Severity `yaml:"fail_on"`
	// Sonar configures the SonarQube scanner used by ReviewModeSonar.
	Sonar Sonar `yaml:"sonar"`
	// AI configures the agent reviewer used by ReviewModeAI.
	AI AI `yaml:"ai"`
}

// Sonar configures the SonarQube scanner.
type Sonar struct {
	// Runner names how the scanner is executed, for example "docker".
	Runner string `yaml:"runner"`
	// Image is the scanner container image used by the docker runner.
	Image string `yaml:"image"`
	// QualityGateWait blocks until SonarQube reports a gate verdict.
	QualityGateWait bool `yaml:"quality_gate_wait"`
	// QualityGateTimeout caps that wait, in seconds.
	QualityGateTimeout int `yaml:"quality_gate_timeout"`
}

// AI configures the agent reviewer.
type AI struct {
	// MCP configures the Sonar MCP server the reviewer talks to.
	MCP MCP `yaml:"mcp"`
}

// MCP configures a Sonar MCP server connection.
type MCP struct {
	// Mode selects the transport.
	Mode MCPMode `yaml:"mode"`
	// URL addresses the server over HTTP. MCPModeCloud and MCPModeServer
	// require it while Review.Mode is ReviewModeAI.
	URL string `yaml:"url"`
	// Image is the server container image used by MCPModeDocker.
	Image string `yaml:"image"`
}

// Budget configures the ceilings that stop a runaway run.
type Budget struct {
	// MaxUSD caps estimated spend in US dollars.
	MaxUSD float64 `yaml:"max_usd"`
	// MaxTokens caps total tokens across the run.
	MaxTokens int64 `yaml:"max_tokens"`
	// OnExceed selects what happens at a ceiling.
	OnExceed OnExceed `yaml:"on_exceed"`
}

// Fanout configures best-of-N candidate generation.
type Fanout struct {
	// Enabled turns candidate generation on.
	Enabled bool `yaml:"enabled"`
	// Candidates is how many candidates run in parallel.
	Candidates int `yaml:"candidates"`
	// Isolator names how candidate workspaces are separated.
	Isolator IsolatorName `yaml:"isolator"`
	// Select names the strategy that picks the winning candidate.
	Select SelectStrategy `yaml:"select"`
}

// Duration is a time.Duration written in YAML as a Go duration string such
// as "90s", "15m" or "1h30m".
//
// A value that does not parse is retained as written rather than rejected
// during decoding, so [Config.Validate] can report it alongside every other
// problem in the file instead of aborting on the first one.
type Duration struct {
	d   time.Duration
	bad string
}

// NewDuration returns a Duration holding d.
func NewDuration(d time.Duration) Duration { return Duration{d: d} }

// Duration returns the parsed duration, or zero if the value did not parse.
func (d Duration) Duration() time.Duration { return d.d }

// Valid reports whether the value parsed as a Go duration.
func (d Duration) Valid() bool { return d.bad == "" }

// String returns the canonical Go duration form, or the text as written when
// that text did not parse.
func (d Duration) String() string {
	if d.bad != "" {
		return d.bad
	}
	return d.d.String()
}

// Equal reports whether d and o hold the same duration and the same
// unparsed text. It is the equality [github.com/google/go-cmp/cmp] uses.
func (d Duration) Equal(o Duration) bool { return d.d == o.d && d.bad == o.bad }

// MarshalYAML writes the canonical Go duration form.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML reads a Go duration string. An explicit null leaves the
// receiver untouched, so an empty key keeps the default it was merged onto.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: duration must be a string such as \"15m\", not %s", n.Line, kindName(n.Kind))
	}
	if n.ShortTag() == "!!null" {
		return nil
	}
	parsed, err := time.ParseDuration(n.Value)
	if err != nil {
		*d = Duration{bad: n.Value}
		return nil
	}
	*d = Duration{d: parsed}
	return nil
}

// kindName names a YAML node shape for an error message. Aliases never
// reach here: yaml resolves them to their target before unmarshalling.
func kindName(k yaml.Kind) string {
	switch k {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	default:
		return "a value of that shape"
	}
}

// FieldError reports one configuration field that cannot be used.
//
// Its message names the field by its dotted path, echoes the offending
// value, and states what would have been acceptable:
//
//	review.mode: "sonarqube" is not valid (want one of: lint, sonar, ai)
type FieldError struct {
	// Field is the dotted path of the field, such as "graph.give_up".
	Field string
	// Value is the offending value, already quoted if it is textual.
	Value string
	// Want describes the acceptable values, without a leading "want".
	Want string
	// Allowed lists the valid values when Field is a closed set.
	Allowed []string
}

// Error implements error.
func (e *FieldError) Error() string {
	want := e.Want
	if len(e.Allowed) > 0 {
		want = "want one of: " + strings.Join(e.Allowed, ", ")
	}
	return fmt.Sprintf("%s: %s is not valid (%s)", e.Field, e.Value, want)
}

// Unwrap reports ErrInvalid.
func (e *FieldError) Unwrap() error { return ErrInvalid }

// VersionError reports a configuration written for a schema belay cannot read.
type VersionError struct {
	// Got is the version found in the file.
	Got int
}

// Error implements error.
func (e *VersionError) Error() string {
	return fmt.Sprintf("version: %d is not supported (want one of: %d)", e.Got, Version)
}

// Unwrap reports ErrUnsupportedVersion.
func (e *VersionError) Unwrap() error { return ErrUnsupportedVersion }

// SecretError reports a credential written into a configuration file.
type SecretError struct {
	// Source names the file the key was found in.
	Source string
	// Line is the line the key appears on, 1-based.
	Line int
	// Key is the offending key as written.
	Key string
}

// Error implements error.
func (e *SecretError) Error() string {
	return fmt.Sprintf("%s:%d: %q must not appear in configuration; belay reads secrets "+
		"from the environment only (export SONAR_TOKEN or ANTHROPIC_API_KEY instead)",
		e.Source, e.Line, e.Key)
}

// Unwrap reports ErrSecret.
func (e *SecretError) Unwrap() error { return ErrSecret }

// UnknownFieldError reports a key that does not name a configuration field.
type UnknownFieldError struct {
	// Source names the file the key was found in.
	Source string
	// Line is the line the key appears on, 1-based.
	Line int
	// Field is the offending key as written.
	Field string
}

// Error implements error.
func (e *UnknownFieldError) Error() string {
	return fmt.Sprintf("%s:%d: unknown field %q", e.Source, e.Line, e.Field)
}

// Unwrap reports ErrUnknownField.
func (e *UnknownFieldError) Unwrap() error { return ErrUnknownField }

// File names used to discover configuration.
const (
	// ProjectFile is the configuration file belay looks for in a project.
	ProjectFile = "belay.yaml"
	// UserFile is the per-user configuration, relative to $XDG_CONFIG_HOME.
	UserFile = "belay/config.yaml"
)

// Options tells [Load] where to look for configuration files.
//
// The zero Options searches the working directory and the user's XDG
// configuration directory, which is what the CLI passes when no --config
// flag is given.
type Options struct {
	// Path is an explicitly requested file, from --config. It is the
	// highest-priority layer and, unlike the discovered layers, must exist.
	Path string
	// Dir is the project directory searched for belay.yaml. Empty means ".".
	Dir string
	// XDGConfigHome overrides the $XDG_CONFIG_HOME lookup. Empty means
	// consult the environment, falling back to $HOME/.config as the XDG
	// Base Directory Specification requires.
	XDGConfigHome string
}

// layer is one configuration file in the merge chain.
type layer struct {
	path     string
	required bool
}

// layers returns the files to merge, lowest priority first.
func (o Options) layers() []layer {
	var ls []layer
	if home := o.configHome(); home != "" {
		ls = append(ls, layer{path: filepath.Join(home, filepath.FromSlash(UserFile))})
	}
	dir := o.Dir
	if dir == "" {
		dir = "."
	}
	ls = append(ls, layer{path: filepath.Join(dir, ProjectFile)})
	if o.Path != "" {
		ls = append(ls, layer{path: o.Path, required: true})
	}
	return ls
}

func (o Options) configHome() string {
	if o.XDGConfigHome != "" {
		return o.XDGConfigHome
	}
	if env := os.Getenv("XDG_CONFIG_HOME"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config")
}

// Load builds a configuration by merging every layer opts names onto the
// built-in defaults, field by field, lowest priority first.
//
// A discovered file that does not exist is skipped; an explicitly requested
// one that does not exist is an error wrapping [ErrNotFound]. The merged
// configuration is returned even when it fails validation, so callers can
// report what was loaded alongside what was wrong with it.
func Load(opts Options) (Config, error) {
	cfg := Default()
	for _, l := range opts.layers() {
		src, err := os.ReadFile(l.path)
		switch {
		case err == nil:
		case errors.Is(err, fs.ErrNotExist) && !l.required:
			continue
		case errors.Is(err, fs.ErrNotExist):
			return cfg, fmt.Errorf("%s: %w", l.path, ErrNotFound)
		default:
			return cfg, fmt.Errorf("read %s: %w", l.path, err)
		}
		if err := cfg.Merge(src, l.path); err != nil {
			return cfg, err
		}
	}
	return cfg, cfg.Validate()
}

// Merge decodes one YAML document onto c, overriding only the fields the
// document actually sets. Absent keys keep the value c already holds, so
// merging a file that sets budget.max_usd leaves every other field alone.
//
// Decoding is strict. A key that names no field, a key written twice, a
// value of the wrong type, a second YAML document, or any key that looks
// like a credential all fail the merge and leave c untouched. Source names
// the document in error messages; it is normally a file path.
func (c *Config) Merge(src []byte, source string) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return malformed(source, err)
	}
	if err := scanSecrets(&doc, source); err != nil {
		return err
	}

	// Config holds only scalars and nested structs, so this copy is a deep
	// copy: c stays untouched unless the whole document decodes cleanly.
	merged := *c

	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	switch err := dec.Decode(&merged); {
	case errors.Is(err, io.EOF):
		return nil // An empty document changes nothing.
	case err != nil:
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			return decodeErrors(source, typeErr, fieldPaths(&doc))
		}
		return malformed(source, err)
	}

	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w: belay reads one YAML document per file", source, ErrMalformed)
	}

	*c = merged
	return nil
}

// malformed reports a document yaml could not parse, restating yaml's own
// message with belay's file:line prefix.
func malformed(source string, err error) error {
	line, detail := splitLine(strings.TrimPrefix(err.Error(), "yaml: "))
	return fmt.Errorf("%s: %w: %s", at(source, line), ErrMalformed, detail)
}

// decodeErrors turns yaml's type errors, which quote Go type names and a bare
// leaf key, into messages phrased in terms of the file the user wrote. Paths
// maps a line to the dotted path written on it, so an error can name the
// field rather than only the line it sits on.
func decodeErrors(source string, typeErr *yaml.TypeError, paths map[int]string) error {
	errs := make([]error, 0, len(typeErr.Errors))
	for _, entry := range typeErr.Errors {
		line, detail := splitLine(entry)
		if field, _, found := strings.Cut(strings.TrimPrefix(detail, "field "), " not found in type "); found {
			if path, ok := paths[line]; ok {
				field = path
			}
			errs = append(errs, &UnknownFieldError{Source: source, Line: line, Field: field})
			continue
		}
		if path, ok := paths[line]; ok {
			errs = append(errs, fmt.Errorf("%s: %s: %w: %s", at(source, line), path, ErrMalformed, detail))
			continue
		}
		errs = append(errs, fmt.Errorf("%s: %w: %s", at(source, line), ErrMalformed, detail))
	}
	return errors.Join(errs...)
}

// at renders a source location, omitting a line number yaml did not report.
func at(source string, line int) string {
	if line <= 0 {
		return source
	}
	return fmt.Sprintf("%s:%d", source, line)
}

// fieldPaths maps each line of a document to the dotted path of the deepest
// key written on it. yaml reports type errors by line and unknown fields by
// bare leaf name; this is what turns either into a path a user can search for.
func fieldPaths(doc *yaml.Node) map[int]string {
	paths := make(map[int]string)
	var walk func(n *yaml.Node, prefix string)
	walk = func(n *yaml.Node, prefix string) {
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, child := range n.Content {
				walk(child, prefix)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, value := n.Content[i], n.Content[i+1]
				path := key.Value
				if prefix != "" {
					path = prefix + "." + key.Value
				}
				// Deeper keys overwrite shallower ones on the same line, so a
				// line resolves to the most specific path written on it.
				paths[key.Line] = path
				paths[value.Line] = path
				walk(value, path)
			}
		}
	}
	walk(doc, "")
	return paths
}

// splitLine peels yaml's "line N: " prefix off an error entry.
func splitLine(entry string) (int, string) {
	rest, ok := strings.CutPrefix(entry, "line ")
	if !ok {
		return 0, entry
	}
	number, detail, ok := strings.Cut(rest, ": ")
	if !ok {
		return 0, entry
	}
	line, err := strconv.Atoi(number)
	if err != nil {
		return 0, entry
	}
	return line, detail
}

// secretWords are the words that mark a key as credential-shaped. A key is
// credential-shaped when one of its words, or one pair of adjacent words run
// together, appears here: "sonar_token", "SONAR_TOKEN", "apiKey" and
// "anthropic_api_key" all match, while "max_tokens" does not.
var secretWords = map[string]bool{
	"token": true, "secret": true, "secrets": true,
	"password": true, "passwd": true, "passphrase": true,
	"credential": true, "credentials": true,
	"apikey": true, "accesskey": true, "privatekey": true,
}

// scanSecrets reports every credential-shaped key anywhere in doc.
func scanSecrets(doc *yaml.Node, source string) error {
	var errs []error
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, child := range n.Content {
				walk(child)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, value := n.Content[i], n.Content[i+1]
				if isSecretKey(key.Value) {
					errs = append(errs, &SecretError{Source: source, Line: key.Line, Key: key.Value})
				}
				walk(value)
			}
		}
	}
	walk(doc)
	return errors.Join(errs...)
}

// isSecretKey reports whether key names a credential rather than a setting.
//
// Matching is by word, not by substring: a substring rule would reject the
// schema's own budget.max_tokens, and a rule that misses "githubToken" would
// let a credential reach version control. Both mistakes are expensive, so the
// key is split into words first and each word, and each adjacent pair, is
// looked up whole.
func isSecretKey(key string) bool {
	words := splitIdentifier(key)
	for i, word := range words {
		if secretWords[word] {
			return true
		}
		if i+1 < len(words) && secretWords[word+words[i+1]] {
			return true
		}
	}
	return false
}

// splitIdentifier breaks a key into lowercase words, splitting on every
// non-alphanumeric character and on each camelCase boundary.
func splitIdentifier(key string) []string {
	var words []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			words = append(words, strings.ToLower(word.String()))
			word.Reset()
		}
	}
	runes := []rune(key)
	for i, r := range runes {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && !unicode.IsUpper(runes[i-1]):
			flush()
			word.WriteRune(r)
		default:
			word.WriteRune(r)
		}
	}
	flush()
	return words
}

// Snapshot is a flat, plainly serializable view of a [Config]: one entry per
// setting, keyed by its dotted path, valued by its canonical text form.
//
// It is what a run manifest embeds. Being flat and textual, it survives
// schema changes that would break a typed round trip, it diffs readably when
// a resumed run disagrees with the run it is resuming, and it gives [Digest]
// a form to hash that does not depend on Go types or map ordering.
type Snapshot map[string]string

// Canonical renders s as sorted "key=\"value\"" lines, one per setting.
//
// Keys are sorted rather than ranged over, so the result does not depend on
// Go's map iteration order, and values are quoted, so a value containing a
// newline cannot be confused with two settings.
func (s Snapshot) Canonical() string {
	var b strings.Builder
	for _, key := range slices.Sorted(maps.Keys(s)) {
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(strconv.Quote(s[key]))
		b.WriteByte('\n')
	}
	return b.String()
}

// Snapshot returns every setting in c keyed by its dotted path.
//
// Each top-level section reports its own settings through its own snapshot
// method, and Snapshot only gathers them. A new section therefore adds one
// method and one line to the list below, rather than rows in a shared map
// literal that every change to the schema would otherwise edit. Each method
// writes only keys under its own section's name, so no two can collide;
// TestSnapshotCoversEveryField checks that together they cover the schema.
func (c Config) Snapshot() Snapshot {
	s := Snapshot{"version": strconv.Itoa(c.Version)}
	for _, section := range []Snapshot{
		c.Agent.snapshot(),
		c.Graph.snapshot(),
		c.Test.snapshot(),
		c.Review.snapshot(),
		c.Budget.snapshot(),
		c.Fanout.snapshot(),
		c.Output.snapshot(),
		c.Plan.snapshot(),
		c.Policy.snapshot(),
	} {
		maps.Copy(s, section)
	}
	return s
}

// snapshot returns the agent.* settings for [Config.Snapshot].
//
// Each routed node contributes a key of its own, agent.models.<node>, rather
// than one key holding the whole map rendered somehow. Every other setting is
// one key, a resumed run that reports drift names the setting that drifted,
// and a node moved to another model is exactly that: one setting changed.
// A run with no routing adds nothing, so its snapshot is what it always was.
func (a Agent) snapshot() Snapshot {
	s := Snapshot{
		"agent.backend":   a.Backend,
		"agent.model":     a.Model,
		"agent.max_turns": strconv.Itoa(a.MaxTurns),
		"agent.mode":      string(a.Mode),
	}
	for node, model := range a.Models {
		s["agent.models."+node] = model
	}
	return s
}

// snapshot returns the graph.* settings for [Config.Snapshot].
func (g Graph) snapshot() Snapshot {
	return Snapshot{
		"graph.give_up":      strconv.Itoa(g.GiveUp),
		"graph.max_steps":    strconv.Itoa(g.MaxSteps),
		"graph.node_timeout": g.NodeTimeout.String(),
		"graph.approval":     strconv.FormatBool(g.Approval),
	}
}

// snapshot returns the test.* settings for [Config.Snapshot].
func (t Test) snapshot() Snapshot {
	return Snapshot{
		"test.runner":     string(t.Runner),
		"test.custom_cmd": t.CustomCmd,
	}
}

// snapshot returns the review.* settings, including the nested sonar and
// ai.mcp ones, for [Config.Snapshot].
func (r Review) snapshot() Snapshot {
	return Snapshot{
		"review.mode":                       string(r.Mode),
		"review.fail_on":                    string(r.FailOn),
		"review.sonar.runner":               r.Sonar.Runner,
		"review.sonar.image":                r.Sonar.Image,
		"review.sonar.quality_gate_wait":    strconv.FormatBool(r.Sonar.QualityGateWait),
		"review.sonar.quality_gate_timeout": strconv.Itoa(r.Sonar.QualityGateTimeout),
		"review.ai.mcp.mode":                string(r.AI.MCP.Mode),
		"review.ai.mcp.url":                 r.AI.MCP.URL,
		"review.ai.mcp.image":               r.AI.MCP.Image,
	}
}

// snapshot returns the budget.* settings for [Config.Snapshot].
func (b Budget) snapshot() Snapshot {
	return Snapshot{
		"budget.max_usd":    strconv.FormatFloat(b.MaxUSD, 'f', -1, 64),
		"budget.max_tokens": strconv.FormatInt(b.MaxTokens, 10),
		"budget.on_exceed":  string(b.OnExceed),
	}
}

// snapshot returns the output.* settings for [Config.Snapshot].
func (o Output) snapshot() Snapshot {
	return Snapshot{
		"output.mode": string(o.Mode),
	}
}

// snapshot returns the plan.* settings for [Config.Snapshot].
func (p Plan) snapshot() Snapshot {
	return Snapshot{
		"plan.archive":     strconv.FormatBool(p.Archive),
		"plan.archive_dir": p.ArchiveDir,
	}
}

// snapshot returns the policy.* settings for [Config.Snapshot].
//
// A list is rendered as one line rather than one key per element, unlike
// agent.models. The reason is what a reader does with the difference: a model
// moved to another node is one setting changed, and a rule added to a list is
// the list changed -- there is no "policy.forbidden_paths.2" a person could
// act on, and numbering them would make inserting a rule at the top read as
// every rule having changed.
//
// A run with no policy renders every key empty or zero, so its digest is what
// it was before this section existed.
func (p Policy) snapshot() Snapshot {
	tests := make([]string, 0, len(p.RequiredTests))
	for _, rule := range p.RequiredTests {
		tests = append(tests, rule.When+" requires "+rule.Require)
	}
	content := make([]string, 0, len(p.ForbiddenContent))
	for _, rule := range p.ForbiddenContent {
		where := "every changed file"
		if len(rule.Paths) > 0 {
			where = strings.Join(rule.Paths, ", ")
		}
		content = append(content, rule.Pattern+" in "+where)
	}
	return Snapshot{
		"policy.forbidden_paths":     strings.Join(p.ForbiddenPaths, ", "),
		"policy.max_changed_files":   strconv.Itoa(p.MaxChangedFiles),
		"policy.max_changed_lines":   strconv.Itoa(p.MaxChangedLines),
		"policy.no_new_dependencies": strconv.FormatBool(p.NoNewDependencies),
		"policy.required_tests":      strings.Join(tests, "; "),
		"policy.forbidden_content":   strings.Join(content, "; "),
	}
}

// snapshot returns the fanout.* settings for [Config.Snapshot].
func (f Fanout) snapshot() Snapshot {
	return Snapshot{
		"fanout.enabled":    strconv.FormatBool(f.Enabled),
		"fanout.candidates": strconv.Itoa(f.Candidates),
		"fanout.isolator":   string(f.Isolator),
		"fanout.select":     string(f.Select),
	}
}

// Digest returns a stable fingerprint of c as "sha256:" followed by hex.
//
// A resumed run compares its digest against the one recorded in its manifest
// to detect that the configuration changed underneath it. The fingerprint is
// taken over [Snapshot.Canonical], so it depends only on the settings
// themselves: it is identical across processes and runs, and two
// configurations that differ only in how a value was written — "900s" against
// "15m" — share a digest, because they would drive an identical run.
func (c Config) Digest() string {
	sum := sha256.Sum256([]byte(c.Snapshot().Canonical()))
	return "sha256:" + hex.EncodeToString(sum[:])
}
