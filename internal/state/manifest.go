package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// ManifestSchemaVersion is the schema version SaveManifest writes and the
// only version LoadManifest accepts without a registered migration. It is 2
// for the same reason StateSchemaVersion is.
const ManifestSchemaVersion = 2

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

// Error implements error. Like VersionError's, it says to upgrade belay
// only for a version above ManifestSchemaVersion.
func (e *ManifestVersionError) Error() string {
	if e.Newer() {
		return fmt.Sprintf("state: manifest.json schema version %d is newer than this belay reads (%d); upgrade belay to continue this run",
			e.Got, ManifestSchemaVersion)
	}
	return fmt.Sprintf("state: manifest.json schema version %d is not supported (want %d)", e.Got, ManifestSchemaVersion)
}

// Unwrap reports ErrUnsupportedManifestVersion.
func (e *ManifestVersionError) Unwrap() error { return ErrUnsupportedManifestVersion }

// Newer reports whether the version found is above ManifestSchemaVersion,
// which means a newer belay wrote the document.
func (e *ManifestVersionError) Newer() bool { return e.Got > ManifestSchemaVersion }

// ManifestMigration upgrades a raw manifest.json document exactly one
// schema version forward, analogous to Migration.
type ManifestMigration func(raw []byte) ([]byte, error)

// manifestMigrations holds every registered ManifestMigration, keyed by
// the version it upgrades from. Version 1, which every released belay
// wrote, upgrades to 2 by migrateFromVersion1, the same step state.json
// takes.
var manifestMigrations = map[int]ManifestMigration{1: migrateFromVersion1}

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
	// RunStatusPaused is a run paused for human input — typically plan
	// approval — or because its caller cancelled it and asked for a pause, as
	// Ctrl-C does under `belay run` and `belay resume`. "belay resume" can
	// continue it.
	RunStatusPaused
	// RunStatusCompleted is a run that reached a terminal success node.
	RunStatusCompleted
	// RunStatusFailed is a run that reached a terminal failure, such as the
	// fix loop giving up.
	RunStatusFailed
	// RunStatusAborted is a run stopped before it could finish, such as at a
	// budget ceiling, or by a cancellation its caller did not ask to pause.
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
	// Requires lists the capabilities this document's fields depend on,
	// sorted and without duplicates; see "Capabilities" in the package
	// documentation. Empty, never nil, for a run that depends on none.
	Requires []string `json:"requires"`
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
	// Output is how this run delivers its result, pinned when the run was
	// created and never read from live configuration afterwards. The zero
	// Output is an in-place run, which is what every manifest written
	// before this field existed decodes as. Set it with SetOutput, which
	// also records the capability a branch run depends on.
	Output Output `json:"output"`
	// ArchivedPlan is where the run files its approved plan once it
	// completes: the directory NewManifest pins from plan.archive and
	// plan.archive_dir, and, once the plan is there, the file itself.
	ArchivedPlan ArchivedPlan `json:"archived_plan"`
	// Fanout is the shape of this run's fanout, pinned when the run was
	// created, or nil for a run that does not fan out — which is what every
	// manifest written before this field existed decodes as. Set it with
	// SetFanout; see CapabilityFanoutCandidateRuns.
	Fanout *FanoutPin `json:"fanout,omitempty"`
	// Candidate is what makes this run one fanout candidate's own run: its
	// place in the parent run, its workspace and its spend envelope. Nil for
	// every run a person started. Set it with SetCandidate.
	Candidate *CandidateRun `json:"candidate,omitempty"`
	// Policy is the guardrails this run's change must obey and the baseline
	// they are measured against, pinned when the run was created, or nil for
	// a run that declared none.
	Policy *PolicyPin `json:"policy,omitempty"`
}

// ArchivedPlan is where a run files its approved plan once it completes
// (ADR-0016): the decision, made once when the run is created, and the
// record of where the plan went.
//
// Both paths are slash-separated and relative to the tree the run's nodes
// work in: the workspace for a run that edits it in place, or, for a run on
// a branch, the run's worktree at the run's prefix — which is the same path
// in the person's own checkout once they take the branch. They are never
// absolute, because a repository can be moved or cloned and the record
// should still name the file.
//
// It is not a control field, and names no capability (ADR-0011). An older
// belay that dropped Dir would leave the plan in the run directory, where it
// already is, and change nothing about the code the run delivers; one that
// dropped Path would lose only the pointer, and no belay rewrites the
// manifest of a run that has completed, which is the only kind that has a
// Path. Neither loss changes what a run does, which is the test for a
// capability.
type ArchivedPlan struct {
	// Dir is where the run files its plan, as plan.archive_dir said when
	// the run was created, cleaned. Empty when plan.archive was false then,
	// or when the run was created by a belay that did not archive plans:
	// either way the run was never told its plan would move, so it does
	// not.
	Dir string `json:"dir"`
	// Path is the archived plan itself, Dir joined with the file's name,
	// recorded once the plan is there and before the copy in the run's
	// artifacts is removed. Empty until then. internal/planarchive is the
	// only writer.
	Path string `json:"path"`
}

// archivedPlanFor pins what cfg says about a new run's plan: the directory
// it will be filed in, cleaned, or nothing when archival is off.
func archivedPlanFor(cfg config.Plan) ArchivedPlan {
	if !cfg.Archive {
		return ArchivedPlan{}
	}
	return ArchivedPlan{Dir: path.Clean(cfg.ArchiveDir)}
}

// OutputMode names one way a run delivers its result. It is deliberately
// not internal/config's own output mode: configuration also accepts
// "auto", which is a question ("branch if this workspace can take one")
// rather than an answer. Only the answer reaches a manifest, so a resumed
// run can never re-ask it and get a different one.
type OutputMode string

const (
	// OutputInPlace edits the workspace directly, which is what every
	// belay before branch output did.
	OutputInPlace OutputMode = "in_place"
	// OutputBranch works in the run's own git worktree, on its own
	// branch, and commits once the quality gate has passed.
	OutputBranch OutputMode = "branch"
)

// CapabilityOutputBranch is the capability a branch-output run's manifest
// declares in its requires list (ADR-0011).
//
// Output is a control field: a belay that did not understand it would
// decode a branch-mode manifest, drop the block, and resume the run
// editing the user's checkout in place — the one thing the run was
// started not to do — and then write the manifest back without it. Naming
// the capability turns that into a refusal that says to upgrade.
//
// Only a run that depends on the field declares it, so an in-place run,
// whose behaviour is unchanged, requires nothing and stays resumable by
// any belay that reads schema version 2.
const CapabilityOutputBranch = "output.branch"

// Output is how a run delivers its result: the mode, and, for a branch
// run, everything about the branch it works on that outlives the process
// that created it.
//
// It is plain data and holds no live handle, for the same reason
// belay.Workspace does: a resumed run reconstructs its git repository,
// isolator and branch from these fields alone.
type Output struct {
	// Mode is how the run delivers its result. Empty means OutputInPlace;
	// see Branched.
	Mode OutputMode `json:"mode"`
	// Repo is the absolute path to the top of the git repository the run
	// works in. Empty for an in-place run.
	Repo string `json:"repo"`
	// Branch is the branch the run commits to, such as
	// "belay/20260101T000000Z-c0ffee000000".
	Branch string `json:"branch"`
	// Base is the commit Branch was created from: the run's "before", and
	// what a promotion's changed-file count is measured against.
	Base string `json:"base"`
	// BaseRef is the branch that was checked out when the run started,
	// which is what a pull request proposes to merge into.
	BaseRef string `json:"base_ref"`
	// WorkTree is the absolute path of the run's own git worktree, the
	// tree its agent edits and its tests run in.
	WorkTree string `json:"work_tree"`
	// Prefix is the run's path within the repository, as git reports it
	// ("sub/" or ""), so a run started in a subdirectory works on that
	// subdirectory inside WorkTree rather than on the repository top.
	Prefix string `json:"prefix"`
	// PullRequest records that the caller asked for a draft pull request
	// when the run was started, so a resume that was given no flags still
	// opens one.
	PullRequest bool `json:"pull_request"`
}

// Branched reports whether o delivers its result on a branch.
//
// It asks for OutputBranch rather than "not OutputInPlace" deliberately.
// The zero Output is what a manifest written before this field existed
// decodes as, and reading that as a branch would send every such run into
// a promote node it was never started with — and, on resume, into a
// worktree nothing ever created.
func (o Output) Branched() bool { return o.Mode == OutputBranch }

// SetOutput pins how this run delivers its result, and records the
// capability that pin depends on.
//
// It is one call rather than two because the field and its capability are
// one decision: a caller that assigned Output directly would write a
// manifest an older belay silently misreads, and nothing would catch it
// until that run was resumed. See CapabilityOutputBranch.
func (m *Manifest) SetOutput(o Output) {
	m.Output = o
	if o.Branched() {
		m.Requires = addCapability(m.Requires, CapabilityOutputBranch)
	}
}

// NewManifest returns the manifest for a freshly started run: SchemaVersion
// current, Requires empty, Status running, Step zero, CreatedAt and
// UpdatedAt both now, Budget seeded from cfg.Budget.MaxUSD, and
// ArchivedPlan.Dir pinned from cfg.Plan. Pinning here, rather than in each
// caller, is what makes every way of creating a run make the same promise
// about its plan.
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
		Requires:       []string{},
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
		ArchivedPlan: archivedPlanFor(cfg.Plan),
	}
}

// Touch sets UpdatedAt to now. The dispatcher calls it every time it
// persists this manifest after a node.
func (m *Manifest) Touch(now time.Time) { m.UpdatedAt = now }

// LoadManifest reads and decodes the manifest.json at path, applying any
// registered migration and checking the document's requires list exactly
// as LoadState does for state.json.
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

	if err := checkRequires("manifest.json", raw); err != nil {
		return Manifest{}, err
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
// that gets the depth wrong points an agent at the wrong tree. Since
// WithWorkTree, that reconstruction is not merely fragile but wrong: a branch
// run's workspace is its own git worktree, which sits inside RunDir rather
// than three parents above it.
func (l Layout) WorkspaceDir() string { return l.workspace }

// WorkTreeDir returns where a branch run's own git worktree is created:
// <run>/worktree, inside the run directory.
//
// It lives under the run rather than beside the repository because it is the
// run's, and because deleting a run directory should leave nothing of that
// run behind. A zero Layout returns "", the same answer every other path
// method gives it, rather than a path relative to whatever directory belay
// happens to be running in.
func (l Layout) WorkTreeDir() string {
	if l.root == "" {
		return ""
	}
	return filepath.Join(l.root, "worktree")
}

// WithWorkTree returns l with dir as the workspace its nodes work in, leaving
// every other path — the manifest, the journal, the artifacts — where it was.
//
// This is what makes a branch run work on its own tree. WorkspaceDir is the
// root a node hands an agent, so redirecting it here redirects every node at
// once, and nothing else in belay has to learn that a run can work somewhere
// other than the directory it was started in.
//
// It takes a directory rather than deriving WorkTreeDir because a run started
// in a subdirectory of a repository works on that subdirectory inside the
// worktree, which is the caller's to compose from the run's recorded prefix.
// dir must be absolute for the reason NewLayout's workspace must be: a
// relative path resolves against belay's own working directory, and an agent
// with write access would then be pointed at the wrong tree. A zero Layout is
// refused outright, as check describes.
func (l Layout) WithWorkTree(dir string) (Layout, error) {
	if err := l.check(); err != nil {
		return Layout{}, err
	}
	if !filepath.IsAbs(dir) {
		return Layout{}, fmt.Errorf("%w: work tree %q is not an absolute path",
			ErrInvalidPathSegment, dir)
	}
	l.workspace = dir
	return l, nil
}

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

// CandidatesDir returns the run's candidates/ directory, which is the root
// the CLI builds this run's isolator at, so every candidate workspace it
// creates belongs to this run and no other.
func (l Layout) CandidatesDir() string { return filepath.Join(l.root, "candidates") }

// CandidateDir returns the path to one best-of-N fanout candidate's
// workspace, candidates/<id>/. It rejects an id that could escape
// candidates/.
//
// The directory is the workspace itself — the tree the candidate's agent
// edits and its tests run in — and not a container holding one. The CLI
// roots both isolators at CandidatesDir (internal/cli's runPlan.isolator),
// so Isolator.Create(src, id) puts the copy or the git worktree exactly
// here. A candidate's own run files — manifest, blackboard, journal,
// snapshots and node evidence — are not under here at all: they live under
// CandidateRunsDir, so that destroying a workspace cannot take the record of
// what was spent making it.
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

// Preflight establishes that belay may work on this run, and makes the run
// safe for it to write to. Every caller about to write anything to an
// existing run calls it first: the dispatcher before it opens the journal,
// `belay resume` before it records an approval.
//
// It does two things, in this order.
//
// It reads state.json. A capability the run requires that this belay does not
// have, or a schema version it cannot read, is a refusal — and a capability
// may live in state.json alone (ADR-0011), so a caller that loaded only the
// manifest has established nothing. Refusing here is what keeps belay from
// healing a journal, appending to it and ending a run it was never allowed to
// read in the first place.
//
// It then saves manifest.json at the current schema version, if it is below
// it. A run's documents are upgraded as they are loaded, and whichever is
// written first decides what an older belay makes of a crash in between.
// Every reader of a run — `belay runs`, `belay resume`, the dispatcher —
// reads the manifest first, so upgrading the manifest first leaves an older
// belay the version it refuses (ADR-0011) rather than a version-1 manifest
// over a version-2 state.json, which it accepts and resumes. Nothing else
// about the manifest changes: a preflight is not a lifecycle event, so
// UpdatedAt is left alone.
//
// A manifest at a version above this belay's is left untouched, and is not
// this function's refusal to make: the caller loaded it, or chose not to,
// and LoadManifest is where that error belongs.
//
// It is not the CLI's resumePreflight (internal/cli), which `belay resume`
// runs earlier, dry run included: that one writes nothing, and turns a pin
// in the manifest the resuming configuration cannot honour into a blocker
// reported beside the others. Preflight is the last check before a write,
// refuses with an error, and saves the manifest itself when it is below the
// current schema version.
func (s *Store) Preflight() error {
	if _, err := s.LoadState(); err != nil {
		return err
	}
	return s.upgradeManifest()
}

// upgradeManifest saves manifest.json at the current schema version when the
// document on disk is below it. See Preflight for why it goes first.
func (s *Store) upgradeManifest() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.layout.ManifestPath()
	raw, err := readFile(path)
	if err != nil {
		return err
	}
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("state: decode manifest.json: %w", err)
	}
	if probe.SchemaVersion >= ManifestSchemaVersion {
		return nil
	}
	m, err := decodeManifest(raw)
	if err != nil {
		return err
	}
	return SaveManifest(path, m)
}
