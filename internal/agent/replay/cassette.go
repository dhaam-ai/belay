package replay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/belay-dev/belay/pkg/belay"
)

// FormatVersion is the only cassette schema version this package writes,
// and the only one LoadCassette and Replay accept without a migration.
// Bump it, and teach LoadCassette a migration path, whenever Cassette's
// on-disk shape changes incompatibly.
const FormatVersion = 1

// ErrUnsupportedFormat reports a cassette whose format_version this
// package does not know how to read. Detect it with errors.Is; recover
// the version found with errors.As against *FormatVersionError.
var ErrUnsupportedFormat = errors.New("replay: unsupported cassette format version")

// FormatVersionError explains which unsupported cassette format version
// was found. It wraps ErrUnsupportedFormat.
type FormatVersionError struct {
	// Got is the format_version found in the cassette.
	Got int
}

// Error implements error.
func (e *FormatVersionError) Error() string {
	return fmt.Sprintf("replay: cassette format version %d is not supported (want %d)", e.Got, FormatVersion)
}

// Unwrap reports ErrUnsupportedFormat.
func (e *FormatVersionError) Unwrap() error { return ErrUnsupportedFormat }

// Cassette is belay's record/replay format: a versioned, deterministic,
// human-reviewable sequence of AgentBackend interactions, each keyed by a
// request Fingerprint (see Fingerprint for the exact definition and its
// rationale).
//
// # Why a single JSON document, not NDJSON
//
// internal/state and internal/journal use append-only NDJSON (ADR-0003)
// because a run journal is written incrementally, one record per
// crash-recoverable step, by a long-lived process, and must tolerate a
// torn final line. A cassette is the opposite shape: it is produced once,
// in full, at the end of a recording session (see Record and
// Backend.Cassette), and read once, in full, before a replay run starts.
// There is no partial-write crash-recovery story to design for, and one
// indented JSON document is easier for a human to open, `git diff`, and
// hand-edit in a pull request than NDJSON would be for a format that is
// never appended to line-by-line by a running process.
type Cassette struct {
	// FormatVersion is this document's schema version. LoadCassette and
	// Replay reject any value other than FormatVersion.
	FormatVersion int `json:"format_version"`

	// Provenance records how, by what, and under what belay version this
	// cassette was captured.
	Provenance Provenance `json:"provenance"`

	// Interactions is the recorded request/response pairs, in the order
	// Record observed them. Order matters within a single Fingerprint:
	// see the package doc's "Repeated identical requests" section for how
	// Backend serves them back.
	Interactions []Interaction `json:"interactions"`
}

// Provenance is metadata about one recording, attached to a Cassette so a
// maintainer reviewing a diff — or running ADR-0009's required
// pre-release smoke test — can tell what produced it, when, and against
// which backend.
//
// Every field here is supplied by the caller of Record, not read from the
// system clock, a build-time version variable, or any other ambient
// source inside this package. That is deliberate: it keeps this package
// unit-testable without its output depending on when or where a test
// runs, and the process that performs a REAL recording (a CLI command or
// Makefile target — not this package) is what actually knows the belay
// version and wall-clock time worth stamping.
type Provenance struct {
	// BelayVersion is the belay build that made this recording, such as
	// "v0.3.1" or a commit hash. Injected by the caller.
	BelayVersion string `json:"belay_version"`

	// Backend is the inner belay.AgentBackend.Name() that produced these
	// interactions, such as "claude-code". Record always sets this from
	// inner.Name() itself, overwriting whatever the caller passed in, so
	// it can never drift from the backend that actually ran.
	Backend string `json:"backend"`

	// Model is a human-readable summary of the model(s) this recording
	// used, for a maintainer skimming the cassette. It is not
	// authoritative: an individual Interaction's own Request.Model is —
	// this field is a label, and a cassette may legitimately mix models
	// across interactions while still carrying one summary here.
	Model string `json:"model,omitempty"`

	// RecordedAt is when this recording was made, supplied by the caller
	// rather than obtained from time.Now() inside this package, so that
	// recording the same interaction twice in a test produces
	// byte-identical output.
	RecordedAt time.Time `json:"recorded_at"`

	// Note is a free-text explanation of what this cassette covers, for
	// example "wave-3 fix-loop happy path, recorded against claude-opus-4
	// on 2026-08-01". It exists so a reviewer opening the cassette in a
	// pull request is not left reverse-engineering intent from
	// interaction contents alone — this is the "note that it is a
	// recording" ADR-0009 calls for.
	Note string `json:"note,omitempty"`
}

// Interaction is one recorded AgentBackend.Invoke call: the request that
// produced it and what the backend returned.
type Interaction struct {
	// Fingerprint identifies the logical request this interaction
	// answers. See Fingerprint.
	Fingerprint string `json:"fingerprint"`

	// Request is the normalized, human-reviewable projection of the
	// AgentRequest that produced Fingerprint, stored alongside the hash
	// so a maintainer can see what a fingerprint represents without
	// recomputing it.
	Request NormalizedRequest `json:"request"`

	// Response is the AgentResponse the backend returned, after secret
	// scrubbing (see Scrub). Populated even when Error is set, matching
	// AgentBackend.Invoke's own contract that a failed call may still
	// carry a meaningful partial response.
	Response belay.AgentResponse

	// Error is the scrubbed error message Invoke returned alongside
	// Response, or empty if Invoke returned a nil error. See
	// ReplayedError for what replaying a non-empty Error produces, and
	// its doc comment for why only the message — not the original error's
	// type or sentinel chain — survives the round trip.
	Error string
}

// storedResponse is Interaction's on-disk representation of a
// belay.AgentResponse. It exists to solve one specific problem: Raw is a
// json.RawMessage — nested JSON — and Cassette.Save indents the whole
// document for human review. Go's JSON indenter reformats an EMBEDDED
// json.RawMessage to match the surrounding indentation level every time
// it runs a document through Indent (which MarshalIndent always does),
// which would silently rewrite Raw's whitespace on every save and break
// the exact byte-for-byte replay this package promises (see
// TestBackend_RecordThenReplay_RoundTrip). Storing Raw as a JSON STRING
// instead sidesteps this: a JSON string's content is opaque to the
// indenter — whatever bytes go in as an escaped string come back out
// unchanged.
type storedResponse struct {
	Text      string      `json:"text"`
	SessionID string      `json:"session_id,omitempty"`
	Usage     belay.Usage `json:"usage"`
	Turns     int         `json:"turns,omitempty"`
	Raw       string      `json:"raw,omitempty"`
}

func newStoredResponse(resp belay.AgentResponse) storedResponse {
	return storedResponse{
		Text:      resp.Text,
		SessionID: resp.SessionID,
		Usage:     resp.Usage,
		Turns:     resp.Turns,
		Raw:       string(resp.Raw),
	}
}

func (s storedResponse) toResponse() belay.AgentResponse {
	var raw json.RawMessage
	if s.Raw != "" {
		raw = json.RawMessage(s.Raw)
	}
	return belay.AgentResponse{
		Text:      s.Text,
		SessionID: s.SessionID,
		Usage:     s.Usage,
		Turns:     s.Turns,
		Raw:       raw,
	}
}

// interactionOnDisk is Interaction's JSON shape, used by MarshalJSON and
// UnmarshalJSON. It exists only so Response can be encoded through
// storedResponse instead of embedding belay.AgentResponse (and its raw
// json.RawMessage) directly.
type interactionOnDisk struct {
	Fingerprint string            `json:"fingerprint"`
	Request     NormalizedRequest `json:"request"`
	Response    storedResponse    `json:"response"`
	Error       string            `json:"error,omitempty"`
}

// MarshalJSON implements json.Marshaler. See storedResponse for why
// Response is not encoded by embedding belay.AgentResponse directly.
func (it Interaction) MarshalJSON() ([]byte, error) {
	return json.Marshal(interactionOnDisk{
		Fingerprint: it.Fingerprint,
		Request:     it.Request,
		Response:    newStoredResponse(it.Response),
		Error:       it.Error,
	})
}

// UnmarshalJSON implements json.Unmarshaler, the inverse of MarshalJSON.
func (it *Interaction) UnmarshalJSON(data []byte) error {
	var d interactionOnDisk
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}
	it.Fingerprint = d.Fingerprint
	it.Request = d.Request
	it.Response = d.Response.toResponse()
	it.Error = d.Error
	return nil
}

// workDirPlaceholder replaces AgentRequest.WorkDir in a NormalizedRequest
// and everywhere Fingerprint hashes it.
const workDirPlaceholder = "<workdir>"

// sessionIDPlaceholder replaces a non-empty AgentRequest.SessionID in a
// NormalizedRequest and everywhere Fingerprint hashes it.
const sessionIDPlaceholder = "<resumed>"

// NormalizedRequest is the projection of a belay.AgentRequest that
// Fingerprint hashes, and that a Cassette stores next to each Interaction
// for a human to read.
//
// It differs from belay.AgentRequest in exactly two fields, both replaced
// with a fixed placeholder instead of their real value:
//
//   - WorkDir becomes workDirPlaceholder unconditionally. A
//     belay.Workspace.Dir is always a freshly created temporary
//     directory (see pkg/belay.Isolator); its absolute path is assigned
//     by the OS anew on every single run, even two runs on the very same
//     machine. There is therefore no "relative to some shared root" form
//     that would ever be stable — a constant placeholder is the only
//     value that is stable both across machines and across repeated runs
//     on one machine, which is exactly what a cassette recorded on one
//     contributor's laptop needs in order to replay on a stranger's CI
//     runner.
//   - SessionID becomes sessionIDPlaceholder when non-empty, and stays
//     empty otherwise. A real SessionID is an opaque identifier the
//     backend assigns per conversation and cannot be reproduced
//     byte-for-byte between two independent recordings, but WHETHER a
//     request is resuming a prior session (SessionID != "") versus
//     starting fresh (SessionID == "") is semantically significant —
//     AgentRequest's own doc comment calls out that collapsing "resume"
//     and "fresh" into one behavior is exactly the kind of ambiguity a
//     backend must not create. A two-state placeholder preserves that
//     distinction in the fingerprint without leaking the volatile value.
//
// Every other field is stored and hashed verbatim; MCPConfig is
// additionally canonicalized (see canonicalJSON) so that two JSON
// documents differing only in formatting normalize identically.
//
// NormalizedRequest has custom MarshalJSON/UnmarshalJSON methods (see
// normalizedRequestOnDisk) rather than the struct tags a reader might
// expect: MCPConfig is nested JSON, and for the same reason given on
// storedResponse, it must be stored as a JSON string rather than embedded
// raw JSON so Cassette.Save's indentation cannot reformat it.
type NormalizedRequest struct {
	Prompt       string
	SystemPrompt string
	WorkDir      string
	AllowedTools []string
	MaxTurns     int
	Model        string
	SessionID    string
	MCPConfig    json.RawMessage
}

// normalizedRequestOnDisk is NormalizedRequest's JSON shape. MCPConfig is
// a string here — see NormalizedRequest's doc comment — while every other
// field matches verbatim.
type normalizedRequestOnDisk struct {
	Prompt       string   `json:"prompt"`
	SystemPrompt string   `json:"system_prompt,omitempty"`
	WorkDir      string   `json:"work_dir"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
	MaxTurns     int      `json:"max_turns,omitempty"`
	Model        string   `json:"model,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
	MCPConfig    string   `json:"mcp_config,omitempty"`
}

// MarshalJSON implements json.Marshaler. It is also what Fingerprint
// hashes (via fingerprintNormalized), so any change here changes every
// Fingerprint belay computes; that is safe precisely because Fingerprint
// promises determinism, not any particular wire shape.
func (n NormalizedRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(normalizedRequestOnDisk{
		Prompt:       n.Prompt,
		SystemPrompt: n.SystemPrompt,
		WorkDir:      n.WorkDir,
		AllowedTools: n.AllowedTools,
		MaxTurns:     n.MaxTurns,
		Model:        n.Model,
		SessionID:    n.SessionID,
		MCPConfig:    string(n.MCPConfig),
	})
}

// UnmarshalJSON implements json.Unmarshaler, the inverse of MarshalJSON.
func (n *NormalizedRequest) UnmarshalJSON(data []byte) error {
	var d normalizedRequestOnDisk
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}
	n.Prompt = d.Prompt
	n.SystemPrompt = d.SystemPrompt
	n.WorkDir = d.WorkDir
	n.AllowedTools = d.AllowedTools
	n.MaxTurns = d.MaxTurns
	n.Model = d.Model
	n.SessionID = d.SessionID
	if d.MCPConfig != "" {
		n.MCPConfig = json.RawMessage(d.MCPConfig)
	} else {
		n.MCPConfig = nil
	}
	return nil
}

// Normalize projects req into its NormalizedRequest form. See
// NormalizedRequest for exactly what changes and why.
func Normalize(req belay.AgentRequest) NormalizedRequest {
	sessionID := ""
	if req.SessionID != "" {
		sessionID = sessionIDPlaceholder
	}
	// The workspace path is normalized out of the prompt bodies too, not
	// only out of WorkDir. Nodes legitimately name the repository inside the
	// prose they send -- the plan node writes "the repository at <abs path>"
	// -- so hashing the prompt verbatim binds the fingerprint to the exact
	// directory the recording was made in. A cassette recorded in
	// /tmp/run-a then replays nowhere else, least of all in CI, which checks
	// out to a path nobody can predict. That is the whole purpose of the
	// cassette, so this substitution is load-bearing rather than cosmetic.
	return NormalizedRequest{
		Prompt:       replaceWorkDir(req.Prompt, req.WorkDir),
		SystemPrompt: replaceWorkDir(req.SystemPrompt, req.WorkDir),
		WorkDir:      workDirPlaceholder,
		AllowedTools: append([]string(nil), req.AllowedTools...),
		MaxTurns:     req.MaxTurns,
		Model:        req.Model,
		SessionID:    sessionID,
		MCPConfig:    canonicalJSON(req.MCPConfig),
	}
}

// replaceWorkDir substitutes every mention of the run's workspace directory
// in text with the same placeholder Normalize uses for WorkDir itself.
//
// An empty workDir leaves text untouched: replacing the empty string would
// splice a placeholder between every character.
func replaceWorkDir(text, workDir string) string {
	if workDir == "" || text == "" {
		return text
	}
	out := strings.ReplaceAll(text, workDir, workDirPlaceholder)
	// A symlinked workspace reaches a node by one path and appears in tool
	// output by its resolved one; normalize both so the two record and
	// replay identically.
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil && resolved != workDir {
		out = strings.ReplaceAll(out, resolved, workDirPlaceholder)
	}
	return out
}

// canonicalJSON returns raw re-encoded with map keys sorted and
// insignificant whitespace removed, so two JSON documents that decode to
// the same value hash identically. Invalid or empty input is returned
// unchanged: Fingerprint must never fail on a malformed MCPConfig, since
// belay itself never parses it (see belay.AgentRequest.MCPConfig).
func canonicalJSON(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

// Fingerprint returns the stable identifier a Cassette's Interactions are
// keyed by.
//
// # Definition
//
// Fingerprint is "sha256:" followed by the lowercase hex SHA-256 digest
// of the JSON encoding of Normalize(req) — that is, of req with WorkDir
// and a non-empty SessionID replaced by fixed placeholders (see
// NormalizedRequest's doc comment for exactly why those two, and only
// those two) and MCPConfig canonicalized. Every other field (Prompt,
// SystemPrompt, AllowedTools, MaxTurns, Model) is hashed exactly as
// given.
//
// # Why this is deterministic
//
// json.Marshal of a struct always emits its fields in the struct's
// declared order, so there is no map in the hashed value whose iteration
// order could vary between processes or machines. Every field NOT
// replaced by Normalize is either the caller's own text (Prompt,
// SystemPrompt) or caller-chosen configuration (AllowedTools, MaxTurns,
// Model) — none of it is time-, machine-, or run-dependent. This is what
// lets a cassette recorded on one contributor's laptop replay correctly
// on a stranger's CI runner: fingerprinting the same logical request
// under two different absolute WorkDir prefixes — or on two entirely
// different machines — produces the same Fingerprint.
func Fingerprint(req belay.AgentRequest) string {
	return fingerprintNormalized(Normalize(req))
}

func fingerprintNormalized(n NormalizedRequest) string {
	// The marshal error is unreachable: every field of NormalizedRequest
	// is a string, a []string of strings, an int, or a json.RawMessage
	// that canonicalJSON has already proven either marshals cleanly or
	// was left as raw bytes json.Unmarshal itself accepted moments ago.
	data, _ := json.Marshal(n) //nolint:errchkjson // see comment above
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// LoadCassette reads and decodes the cassette at path.
//
// It rejects a cassette whose FormatVersion is not FormatVersion with a
// *FormatVersionError, rather than guessing at an incompatible shape.
func LoadCassette(path string) (*Cassette, error) {
	// #nosec G304 -- path names a cassette file belay's own caller chose
	// (a CLI flag, a test's testdata path, or a config value), the same
	// trust level as any other file belay is told to open.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replay: read cassette %s: %w", path, err)
	}
	var c Cassette
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("replay: decode cassette %s: %w", path, err)
	}
	if c.FormatVersion != FormatVersion {
		return nil, &FormatVersionError{Got: c.FormatVersion}
	}
	return &c, nil
}

// Save writes c to path as indented JSON, creating or truncating it.
//
// The write is atomic: Save writes to a temporary file in path's own
// directory, then renames it over path, so a concurrent reader (or a
// `git diff` running at the wrong moment) never observes a half-written
// cassette. Save does not fsync the way internal/state does for
// run-critical files (ADR-0003): a cassette is a checked-in test fixture,
// rebuilt by re-running the recording process on demand, not a
// durability-critical artifact — losing an in-progress recording to a
// crash costs a re-run, never user data.
//
// The file is created world-readable (0644), matching how git itself
// stores tracked files: a cassette is meant to be opened, diffed, and
// reviewed by anyone with the repository checked out, unlike the
// per-run, per-user files internal/state writes at 0600.
func (c *Cassette) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("replay: encode cassette: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("replay: create directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("replay: create temp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("replay: write %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("replay: close %s: %w", tmpPath, err)
	}
	// #nosec G302 -- a cassette is a checked-in test fixture meant to be
	// world-readable and diffable, not a private run artifact; 0644
	// matches how git itself stores tracked files.
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("replay: set permissions on %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replay: install %s: %w", path, err)
	}
	renamed = true
	return nil
}

// DriftReport summarizes how a cassette's recorded fingerprints compare
// against the fingerprints a set of live requests actually produced.
//
// It is the mechanism behind ADR-0009's required pre-release smoke test:
// run the real suite through a Backend in ModeLive (or ModeRecord)
// against a release candidate, collect Backend.Seen(), and compare it to
// the checked-in cassette with DetectDrift. A non-empty report means
// either the cassette no longer matches what the code actually sends, or
// the code stopped sending something the cassette still carries — either
// way, that is drift a maintainer should review and re-record before
// tagging, rather than discover from a customer after release.
type DriftReport struct {
	// Stale lists fingerprints the cassette has interactions for that the
	// live run never produced — recordings that may no longer correspond
	// to anything the code sends.
	Stale []string
	// Missing lists fingerprints the live run produced that the cassette
	// has no interaction for at all. Replaying this exact run from the
	// cassette instead of live would fail every one of these with a
	// *MissingInteractionError.
	Missing []string
}

// InSync reports whether report contains no drift at all.
func (r DriftReport) InSync() bool {
	return len(r.Stale) == 0 && len(r.Missing) == 0
}

// DetectDrift compares the fingerprints cassette has recorded
// interactions for against sentFingerprints — typically a Backend.Seen()
// from a live (or recording) run of the same logical test suite.
func DetectDrift(cassette *Cassette, sentFingerprints []string) DriftReport {
	recorded := make(map[string]bool, len(cassette.Interactions))
	for _, it := range cassette.Interactions {
		recorded[it.Fingerprint] = true
	}
	produced := make(map[string]bool, len(sentFingerprints))
	for _, fp := range sentFingerprints {
		produced[fp] = true
	}

	var report DriftReport
	for fp := range recorded {
		if !produced[fp] {
			report.Stale = append(report.Stale, fp)
		}
	}
	for fp := range produced {
		if !recorded[fp] {
			report.Missing = append(report.Missing, fp)
		}
	}
	sort.Strings(report.Stale)
	sort.Strings(report.Missing)
	return report
}
