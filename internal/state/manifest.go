package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/pkg/belay"
)

// ManifestSchemaVersion is the schema version SaveManifest writes and the
// only version LoadManifest accepts without a registered migration.
const ManifestSchemaVersion = 1

// ErrUnsupportedManifestVersion reports a manifest.json whose
// schema_version LoadManifest does not know how to read and has no
// migration registered for.
var ErrUnsupportedManifestVersion = errors.New("state: unsupported manifest schema version")

// ManifestVersionError explains which unsupported manifest.json schema
// version was found. It wraps ErrUnsupportedManifestVersion.
type ManifestVersionError struct {
	// Got is the schema_version found on disk.
	Got int
}

// Error implements error.
func (e *ManifestVersionError) Error() string {
	return fmt.Sprintf("state: manifest.json schema version %d is not supported (want %d)", e.Got, ManifestSchemaVersion)
}

// Unwrap reports ErrUnsupportedManifestVersion.
func (e *ManifestVersionError) Unwrap() error { return ErrUnsupportedManifestVersion }

// ManifestMigration upgrades a raw manifest.json document exactly one
// schema version forward, analogous to Migration.
type ManifestMigration func(raw []byte) ([]byte, error)

// manifestMigrations holds no entries today: ManifestSchemaVersion is
// manifest.json's only version so far.
var manifestMigrations = map[int]ManifestMigration{}

// ErrUnknownRunStatus reports a string that does not name a RunStatus.
var ErrUnknownRunStatus = errors.New("state: unknown run status")

// RunStatus is a run's lifecycle state, as recorded in manifest.json.
type RunStatus int

const (
	// RunStatusUnknown is the zero value. It never appears in a manifest
	// NewManifest produces; its only legitimate appearance is a RunStatus
	// nobody has set yet.
	RunStatusUnknown RunStatus = iota
	// RunStatusRunning is a run the dispatcher is actively advancing.
	RunStatusRunning
	// RunStatusPaused is a run suspended for human input — typically plan
	// approval — that "belay resume" can continue.
	RunStatusPaused
	// RunStatusCompleted is a run that reached a terminal success node.
	RunStatusCompleted
	// RunStatusFailed is a run that reached a terminal failure, such as the
	// fix loop giving up.
	RunStatusFailed
	// RunStatusAborted is a run stopped before it could finish, such as by
	// user request or a budget ceiling.
	RunStatusAborted
)

var runStatusNames = [...]string{
	RunStatusUnknown:   "unknown",
	RunStatusRunning:   "running",
	RunStatusPaused:    "paused",
	RunStatusCompleted: "completed",
	RunStatusFailed:    "failed",
	RunStatusAborted:   "aborted",
}

// String returns the canonical lowercase spelling of s, such as "running".
// An out-of-range RunStatus renders as RunStatus(n) rather than panicking.
func (s RunStatus) String() string {
	if !s.valid() {
		return fmt.Sprintf("RunStatus(%d)", int(s))
	}
	return runStatusNames[s]
}

func (s RunStatus) valid() bool { return s >= 0 && int(s) < len(runStatusNames) }

// ParseRunStatus is the inverse of RunStatus.String. It returns an error
// wrapping ErrUnknownRunStatus for any input other than "unknown",
// "running", "paused", "completed", "failed" or "aborted".
func ParseRunStatus(s string) (RunStatus, error) {
	normalized := strings.ToLower(strings.TrimSpace(s))
	for i, name := range runStatusNames {
		if name == normalized {
			return RunStatus(i), nil
		}
	}
	return RunStatusUnknown, fmt.Errorf("%w: %q", ErrUnknownRunStatus, s)
}

// MarshalText implements encoding.TextMarshaler.
func (s RunStatus) MarshalText() ([]byte, error) {
	if !s.valid() {
		return nil, fmt.Errorf("%w: %d", ErrUnknownRunStatus, int(s))
	}
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *RunStatus) UnmarshalText(text []byte) error {
	parsed, err := ParseRunStatus(string(text))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// Budget is a run's spend ledger, as recorded in manifest.json.
type Budget struct {
	// LimitUSD is the configured spend ceiling (internal/config's
	// Budget.MaxUSD) this run started with.
	LimitUSD float64 `json:"limit_usd"`
	// SpentUSD is cumulative reported-or-estimated spend across every
	// AgentBackend.Invoke call so far.
	SpentUSD float64 `json:"spent_usd"`
	// TokensIn is cumulative input tokens across every invoke call.
	TokensIn int64 `json:"tokens_in"`
	// TokensOut is cumulative output tokens across every invoke call.
	TokensOut int64 `json:"tokens_out"`
	// Estimated becomes true once any contributing belay.Usage was itself
	// estimated rather than reported, and stays true: a run's total is
	// only as precise as its least precise contributor.
	Estimated bool `json:"estimated"`
}

// Accumulate folds one AgentBackend.Invoke call's belay.Usage into the
// ledger.
func (b *Budget) Accumulate(u belay.Usage) {
	b.SpentUSD += u.USD
	b.TokensIn += u.InputTokens
	b.TokensOut += u.OutputTokens
	b.Estimated = b.Estimated || u.Estimated
}

// Remaining returns the ledger's headroom before LimitUSD. It is negative
// once the run has gone over budget.
func (b Budget) Remaining() float64 { return b.LimitUSD - b.SpentUSD }

// AgentInfo records which agent backend, model and mode a run used, as
// recorded in manifest.json.
type AgentInfo struct {
	// Backend is the AgentBackend.Name the run was configured with.
	Backend string `json:"backend"`
	// Model is the model identifier passed as AgentRequest.Model.
	Model string `json:"model"`
	// Mode is the run's execution mode: live, record or replay (see
	// internal/config's AgentMode).
	Mode string `json:"mode"`
}

// Manifest is a run's identity, lifecycle and budget ledger, as persisted
// to manifest.json.
//
// The zero Manifest is not a valid on-disk document (SchemaVersion is 0);
// construct one with NewManifest.
type Manifest struct {
	// SchemaVersion is this document's schema version.
	SchemaVersion int `json:"schema_version"`
	// RunID is this run's identifier — also the name of its directory
	// under .belay/runs/. See NewRunID.
	RunID string `json:"run_id"`
	// CreatedAt is when the run started.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when this manifest was last written. See Touch.
	UpdatedAt time.Time `json:"updated_at"`
	// Workspace is the absolute path to the target repository this run
	// operates on.
	Workspace string `json:"workspace"`
	// Goal is the run's objective, as given by the caller that started it.
	Goal string `json:"goal"`
	// Status is the run's current lifecycle state.
	Status RunStatus `json:"status"`
	// CurrentNode is the graph node the dispatcher is on, or was last on.
	CurrentNode string `json:"current_node"`
	// Step is the total number of node executions so far, counted against
	// internal/config's Graph.MaxSteps.
	Step int `json:"step"`
	// ConfigDigest is Config.Digest() at the moment this run started,
	// letting a resumed run detect that its configuration changed
	// underneath it.
	ConfigDigest string `json:"config_digest"`
	// ConfigSnapshot is Config.Snapshot() at the moment this run started,
	// redacted through RedactSnapshot before being embedded here.
	ConfigSnapshot config.Snapshot `json:"config_snapshot"`
	// Budget is the run's spend ledger.
	Budget Budget `json:"budget"`
	// Agent identifies the backend, model and mode this run used.
	Agent AgentInfo `json:"agent"`
}

// NewManifest returns the manifest for a freshly started run: SchemaVersion
// current, Status running, Step zero, CreatedAt and UpdatedAt both now, and
// Budget seeded from cfg.Budget.MaxUSD.
//
// cfg's Snapshot is passed through RedactSnapshot before being embedded.
// internal/config's own documentation states that a configuration file
// never legitimately carries a secret — credentials are read from the
// environment and rejected outright if written to belay.yaml — so this is
// defense in depth against a future config field leaking one into a
// manifest.json that fixtures/, examples/ and ordinary debugging expect to
// be safe to read and share, not a claim that today's schema needs it. Any
// value RedactSnapshot touches is logged at Warn via slog.Default so the
// redaction itself is never silent.
func NewManifest(now time.Time, runID, workspace, goal string, cfg config.Config) Manifest {
	snapshot, redacted := RedactSnapshot(cfg.Snapshot())
	if len(redacted) > 0 {
		slog.Default().Warn("state: redacted secret-shaped config values from manifest snapshot",
			slog.String("run_id", runID), slog.Any("keys", redacted))
	}
	return Manifest{
		SchemaVersion:  ManifestSchemaVersion,
		RunID:          runID,
		CreatedAt:      now,
		UpdatedAt:      now,
		Workspace:      workspace,
		Goal:           goal,
		Status:         RunStatusRunning,
		ConfigDigest:   cfg.Digest(),
		ConfigSnapshot: snapshot,
		Budget:         Budget{LimitUSD: cfg.Budget.MaxUSD},
		Agent: AgentInfo{
			Backend: cfg.Agent.Backend,
			Model:   cfg.Agent.Model,
			Mode:    string(cfg.Agent.Mode),
		},
	}
}

// Touch sets UpdatedAt to now. The dispatcher calls it every time it
// persists this manifest after a node.
func (m *Manifest) Touch(now time.Time) { m.UpdatedAt = now }

// LoadManifest reads and decodes the manifest.json at path, applying any
// registered migration exactly as LoadState does for state.json.
func LoadManifest(path string) (Manifest, error) {
	raw, err := readFile(path)
	if err != nil {
		return Manifest{}, err
	}
	return decodeManifest(raw)
}

// decodeManifest is LoadManifest's migration-aware core, split out so
// tests can exercise it directly against in-memory fixtures.
func decodeManifest(raw []byte) (Manifest, error) {
	for {
		var probe struct {
			SchemaVersion int `json:"schema_version"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return Manifest{}, fmt.Errorf("state: decode manifest.json: %w", err)
		}
		if probe.SchemaVersion == ManifestSchemaVersion {
			break
		}
		migrate, ok := manifestMigrations[probe.SchemaVersion]
		if !ok {
			return Manifest{}, &ManifestVersionError{Got: probe.SchemaVersion}
		}
		migrated, err := migrate(raw)
		if err != nil {
			return Manifest{}, fmt.Errorf("state: migrate manifest.json from version %d: %w", probe.SchemaVersion, err)
		}
		raw = migrated
	}

	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("state: decode manifest.json: %w", err)
	}
	return m, nil
}

// SaveManifest atomically writes m to path as indented JSON.
func SaveManifest(path string, m Manifest) error {
	return writeJSON(path, m, filePerm)
}

// secretKeyWords are whole words (as split by secretKeyFields) whose
// presence in a config.Snapshot key name alone suggests it holds a
// credential, independent of whatever value ended up there.
//
// Matching is on whole words, not substrings, deliberately: a naive
// substring match on "token" also matches "tokens" — and internal/config's
// own budget.max_tokens field counts LLM tokens, not credentials. A
// snapshot key is dotted-and-underscored (see Config.Snapshot), so
// splitting on non-alphanumeric runs and comparing whole words avoids that
// class of false positive entirely.
var secretKeyWords = map[string]bool{
	"token":       true,
	"secret":      true,
	"password":    true,
	"passwd":      true,
	"credential":  true,
	"credentials": true,
}

// secretKeyCombos are two-word splits (such as "api"+"key") that together
// suggest a credential even though neither word alone would.
var secretKeyCombos = map[string]bool{
	"apikey":     true,
	"privatekey": true,
}

// looksLikeSecretKey reports whether key's own name — split into words on
// every run of non-alphanumeric characters — suggests it holds a
// credential.
func looksLikeSecretKey(key string) bool {
	words := strings.FieldsFunc(key, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for i, w := range words {
		lower := strings.ToLower(w)
		if secretKeyWords[lower] {
			return true
		}
		if i+1 < len(words) && secretKeyCombos[lower+strings.ToLower(words[i+1])] {
			return true
		}
	}
	return false
}

// secretValuePattern matches a value shaped like a credential regardless of
// its key name: a well-known token prefix, or a long run of base64/hex-ish
// characters with no whitespace (the shape of a bearer token, API key or
// secret hash, and not the shape of any value internal/config's schema
// otherwise produces).
var secretValuePattern = regexp.MustCompile(
	`^(sk-|sk_|gh[pousr]_|github_pat_|xox[baprs]-|AKIA|ASIA|AIza|eyJ|-----BEGIN )|^[A-Za-z0-9+/_=.-]{32,}$`,
)

// redactedValue replaces any snapshot value RedactSnapshot flags.
const redactedValue = "REDACTED"

// RedactSnapshot returns a copy of snap with every value that looks like a
// secret — by its key name or by the value's own shape — replaced with
// "REDACTED", together with the sorted list of keys it touched.
//
// internal/config's own documentation states that a configuration should
// never legitimately carry a secret: credentials are read from the
// environment, and a file that names one is rejected at load time.
// RedactSnapshot enforces that same rule at the boundary where a snapshot
// is about to be written to disk in manifest.json, instead of merely
// assuming upstream got it right — a hand-built config.Snapshot (in a test,
// or a future caller) has no way to smuggle a real token into a committed
// run directory.
func RedactSnapshot(snap config.Snapshot) (config.Snapshot, []string) {
	out := make(config.Snapshot, len(snap))
	var redacted []string
	for k, v := range snap {
		if looksLikeSecretKey(k) || looksLikeSecretValue(v) {
			out[k] = redactedValue
			redacted = append(redacted, k)
			continue
		}
		out[k] = v
	}
	sort.Strings(redacted)
	return out, redacted
}

func looksLikeSecretValue(v string) bool {
	if v == "" {
		return false
	}
	return secretValuePattern.MatchString(v)
}

// runIDTimeLayout is deliberately colon-free (unlike RFC3339's "15:04:05")
// so a run ID is always a safe filename component on macOS and Linux.
const runIDTimeLayout = "20060102T150405Z"

// runIDRandomBytes is how many random bytes suffix a run ID's timestamp.
// Six bytes (twelve hex characters) makes two runs started within the same
// second collide with negligible probability without making IDs unwieldy.
const runIDRandomBytes = 6

// NewRunID returns a new run ID: a colon-free, UTC, second-resolution
// timestamp followed by a random hex suffix. Because the timestamp is
// fixed-width and comes first, run IDs sort chronologically as plain
// strings, and the random suffix means two runs started in the same second
// still cannot collide. It uses the real clock and crypto/rand; see
// NewRunIDAt for a deterministic variant.
func NewRunID() (string, error) {
	return NewRunIDAt(time.Now().UTC(), rand.Reader)
}

// NewRunIDAt is NewRunID with an injectable clock and randomness source, so
// a test can assert an exact, deterministic run ID.
func NewRunIDAt(now time.Time, random io.Reader) (string, error) {
	buf := make([]byte, runIDRandomBytes)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", fmt.Errorf("state: generate run id: %w", err)
	}
	return fmt.Sprintf("%s-%s", now.UTC().Format(runIDTimeLayout), hex.EncodeToString(buf)), nil
}

// Layout owns every path under one run directory, so no other package
// string-concatenates ".belay/runs/...". Construct one with NewLayout.
type Layout struct {
	workspace string
	root      string
}

// NewLayout returns the Layout for runID's run directory under
// workspaceDir (typically a belay.Workspace's Dir).
//
// It rejects runID outright — see validateSegment — rather than
// sanitizing it: silently "cleaning up" an unsafe run ID could make two
// different, invalid IDs collapse onto the same directory, corrupting
// whichever run got there first.
func NewLayout(workspaceDir, runID string) (Layout, error) {
	if err := validateSegment("run id", runID); err != nil {
		return Layout{}, err
	}
	// The workspace must be absolute. A relative or empty value silently
	// resolves against whatever directory belay happens to be running in,
	// which would point a target repository's test suite -- and an agent
	// with write access -- at belay's own tree.
	if !filepath.IsAbs(workspaceDir) {
		return Layout{}, fmt.Errorf("%w: workspace %q is not an absolute path",
			ErrInvalidPathSegment, workspaceDir)
	}
	return Layout{
		workspace: workspaceDir,
		root:      filepath.Join(workspaceDir, ".belay", "runs", runID),
	}, nil
}

// WorkspaceDir returns the workspace this run belongs to: the repository
// belay is operating on, and the root a node hands an agent.
//
// It is stored rather than derived. Reconstructing it by walking up from
// RunDir couples every caller to the ".belay/runs/<id>" shape, and a caller
// that gets the depth wrong points an agent at the wrong tree.
func (l Layout) WorkspaceDir() string { return l.workspace }

// RunID returns the run's identifier, the last segment of RunDir.
func (l Layout) RunID() string {
	if l.root == "" {
		return ""
	}
	return filepath.Base(l.root)
}

// RunDir returns the run's root directory.
func (l Layout) RunDir() string { return l.root }

// ManifestPath returns the path to manifest.json.
func (l Layout) ManifestPath() string { return filepath.Join(l.root, "manifest.json") }

// StatePath returns the path to state.json.
func (l Layout) StatePath() string { return filepath.Join(l.root, "state.json") }

// JournalPath returns the path to journal.ndjson. internal/journal owns
// that file's contents; Layout only knows where it lives.
func (l Layout) JournalPath() string { return filepath.Join(l.root, "journal.ndjson") }

// ArtifactsDir returns the run's artifacts/ directory.
func (l Layout) ArtifactsDir() string { return filepath.Join(l.root, "artifacts") }

// ArtifactPath returns the path to a named artifact under artifacts/, such
// as "plan.md", "review.json" or "diff-0007.patch". It rejects a name that
// could escape artifacts/, and refuses a zero Layout outright.
func (l Layout) ArtifactPath(name string) (string, error) {
	if err := l.check(); err != nil {
		return "", err
	}
	return joinSafe("artifact name", l.ArtifactsDir(), name)
}

// check refuses a Layout that never went through NewLayout.
//
// The zero value's root is the empty string, so every filepath.Join in this
// type silently produces a path relative to whatever directory the process
// happens to be in. Under `go test` that is the package's own source
// directory, which is how a run's artifacts once landed inside this
// repository. In production it would scatter a run's state across the
// user's shell cwd. Refusing is the only safe reading of a zero Layout.
func (l Layout) check() error {
	if l.root == "" {
		return fmt.Errorf("%w: zero Layout has no run directory; construct one with NewLayout",
			ErrInvalidPathSegment)
	}
	return nil
}

// NodesDir returns the run's nodes/ directory.
func (l Layout) NodesDir() string { return filepath.Join(l.root, "nodes") }

// NodeDir returns the path to one node execution's directory,
// nodes/<seq>-<name>/, which holds that execution's prompt, response and
// logs. seq is zero-padded to three digits. It rejects a negative seq or a
// name that could escape nodes/.
func (l Layout) NodeDir(seq int, name string) (string, error) {
	if err := l.check(); err != nil {
		return "", err
	}
	if seq < 0 {
		return "", &PathSegmentError{Kind: "node seq", Segment: strconv.Itoa(seq), Reason: "must not be negative"}
	}
	return joinSafe("node name", l.NodesDir(), fmt.Sprintf("%03d-%s", seq, name))
}

// CandidatesDir returns the run's candidates/ directory.
func (l Layout) CandidatesDir() string { return filepath.Join(l.root, "candidates") }

// CandidateDir returns the path to one best-of-N fanout candidate's
// directory, candidates/<id>/, which holds its own workspace/, state.json
// and journal.ndjson. It rejects an id that could escape candidates/.
func (l Layout) CandidateDir(id string) (string, error) {
	if err := l.check(); err != nil {
		return "", err
	}
	return joinSafe("candidate id", l.CandidatesDir(), id)
}

// Store persists one run's manifest and state.json, guarding every read
// and write with a mutex.
//
// A *Store assumes exactly one dispatcher process owns a run at a time: it
// is safe for many goroutines within that one process, never for two
// processes sharing a run directory. Nothing here arbitrates across
// process boundaries — the same assumption internal/journal's Journal
// documents for the file this package does not own.
type Store struct {
	mu     sync.Mutex
	layout Layout
}

// NewStore returns a Store for the run at layout.
func NewStore(layout Layout) *Store {
	return &Store{layout: layout}
}

// Layout returns the Layout this Store persists to.
func (s *Store) Layout() Layout { return s.layout }

// LoadState reads state.json.
func (s *Store) LoadState() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return LoadState(s.layout.StatePath())
}

// SaveState atomically writes state.json.
func (s *Store) SaveState(st State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SaveState(s.layout.StatePath(), st)
}

// ApplyPatch loads state.json, applies p, atomically saves the result, and
// returns the new State.
//
// It holds the Store's lock for the entire read-modify-write cycle, so two
// concurrent callers within one process cannot interleave and silently
// drop one of their patches — one call's write is always fully visible to
// the next call's read.
func (s *Store) ApplyPatch(p Patch) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := LoadState(s.layout.StatePath())
	if err != nil {
		return State{}, err
	}
	if err := p.Apply(&st); err != nil {
		return State{}, err
	}
	if err := SaveState(s.layout.StatePath(), st); err != nil {
		return State{}, err
	}
	return st, nil
}

// LoadManifest reads manifest.json.
func (s *Store) LoadManifest() (Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return LoadManifest(s.layout.ManifestPath())
}

// SaveManifest atomically writes manifest.json.
func (s *Store) SaveManifest(m Manifest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SaveManifest(s.layout.ManifestPath(), m)
}
