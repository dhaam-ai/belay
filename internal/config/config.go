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
}

// Agent configures the coding agent CLI belay drives.
type Agent struct {
	// Backend names the agent CLI adapter, for example "claude-code".
	Backend string `yaml:"backend"`
	// Model names the model the backend should use.
	Model string `yaml:"model"`
	// MaxTurns caps the agent turns allowed inside a single node.
	MaxTurns int `yaml:"max_turns"`
	// Mode selects live, record or replay execution.
	Mode AgentMode `yaml:"mode"`
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
	// URL addresses an already-running server. Required by MCPModeServer.
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
	Isolator string `yaml:"isolator"`
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
func (c Config) Snapshot() Snapshot {
	return Snapshot{
		"version":                           strconv.Itoa(c.Version),
		"agent.backend":                     c.Agent.Backend,
		"agent.model":                       c.Agent.Model,
		"agent.max_turns":                   strconv.Itoa(c.Agent.MaxTurns),
		"agent.mode":                        string(c.Agent.Mode),
		"graph.give_up":                     strconv.Itoa(c.Graph.GiveUp),
		"graph.max_steps":                   strconv.Itoa(c.Graph.MaxSteps),
		"graph.node_timeout":                c.Graph.NodeTimeout.String(),
		"graph.approval":                    strconv.FormatBool(c.Graph.Approval),
		"test.runner":                       string(c.Test.Runner),
		"test.custom_cmd":                   c.Test.CustomCmd,
		"review.mode":                       string(c.Review.Mode),
		"review.fail_on":                    string(c.Review.FailOn),
		"review.sonar.runner":               c.Review.Sonar.Runner,
		"review.sonar.image":                c.Review.Sonar.Image,
		"review.sonar.quality_gate_wait":    strconv.FormatBool(c.Review.Sonar.QualityGateWait),
		"review.sonar.quality_gate_timeout": strconv.Itoa(c.Review.Sonar.QualityGateTimeout),
		"review.ai.mcp.mode":                string(c.Review.AI.MCP.Mode),
		"review.ai.mcp.url":                 c.Review.AI.MCP.URL,
		"review.ai.mcp.image":               c.Review.AI.MCP.Image,
		"budget.max_usd":                    strconv.FormatFloat(c.Budget.MaxUSD, 'f', -1, 64),
		"budget.max_tokens":                 strconv.FormatInt(c.Budget.MaxTokens, 10),
		"budget.on_exceed":                  string(c.Budget.OnExceed),
		"fanout.enabled":                    strconv.FormatBool(c.Fanout.Enabled),
		"fanout.candidates":                 strconv.Itoa(c.Fanout.Candidates),
		"fanout.isolator":                   c.Fanout.Isolator,
		"fanout.select":                     string(c.Fanout.Select),
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
