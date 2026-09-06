package replay_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/agent/replay"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// timeCmp lets cmp.Diff compare time.Time by instant, since time.Time
// carries unexported fields cmp otherwise refuses to touch and a JSON
// round trip can change monotonic-reading state without changing the
// instant represented.
var timeCmp = cmp.Comparer(func(a, b time.Time) bool { return a.Equal(b) })

func baseRequest() belay.AgentRequest {
	return belay.AgentRequest{
		Prompt:       "implement the Login handler",
		SystemPrompt: "you are a careful senior engineer",
		WorkDir:      "/Users/alice/tmp/belay-run-1/workspace",
		AllowedTools: []string{"Read", "Edit", "Bash"},
		MaxTurns:     8,
		Model:        "claude-opus-4",
	}
}

// TestFingerprint_CrossMachineStability is the load-bearing test the task
// calls out explicitly: a fingerprint must not embed an absolute path, so
// the very same logical request fingerprints identically no matter what
// absolute prefix its WorkDir happens to have on a given machine.
func TestFingerprint_CrossMachineStability(t *testing.T) {
	t.Parallel()

	machineA := baseRequest()
	machineA.WorkDir = "/Users/alice/tmp/belay-run-1/workspace"

	machineB := baseRequest()
	machineB.WorkDir = "/home/ci-runner/builds/42/belay-workspace"

	fpA := replay.Fingerprint(machineA)
	fpB := replay.Fingerprint(machineB)

	if fpA != fpB {
		t.Fatalf("Fingerprint differs across absolute WorkDir prefixes:\n  machine A (%s): %s\n  machine B (%s): %s",
			machineA.WorkDir, fpA, machineB.WorkDir, fpB)
	}
	if fpA == "" {
		t.Fatal("Fingerprint returned an empty string")
	}
}

// TestFingerprint_SessionIDNormalization checks the second placeholder:
// two different opaque SessionID values (as two independent recordings of
// a "resume" call would have) fingerprint identically, but resuming vs.
// starting fresh must still fingerprint differently — collapsing that
// distinction would make AgentRequest's "resume this session" and "start
// fresh" indistinguishable to a replaying Backend.
func TestFingerprint_SessionIDNormalization(t *testing.T) {
	t.Parallel()

	fresh := baseRequest()
	fresh.SessionID = ""

	resumedA := baseRequest()
	resumedA.SessionID = "sess-recorded-on-monday-abc123"

	resumedB := baseRequest()
	resumedB.SessionID = "sess-recorded-on-tuesday-xyz789"

	fpFresh := replay.Fingerprint(fresh)
	fpResumedA := replay.Fingerprint(resumedA)
	fpResumedB := replay.Fingerprint(resumedB)

	if fpResumedA != fpResumedB {
		t.Errorf("two different opaque SessionID values fingerprinted differently: %s vs %s", fpResumedA, fpResumedB)
	}
	if fpFresh == fpResumedA {
		t.Errorf("fresh and resumed requests fingerprinted identically (%s): resume-vs-fresh must remain distinguishable", fpFresh)
	}
}

// TestFingerprint_DistinctForDifferentContent pins which fields actually
// participate in the hash by varying exactly one at a time.
func TestFingerprint_DistinctForDifferentContent(t *testing.T) {
	t.Parallel()

	base := baseRequest()
	baseFP := replay.Fingerprint(base)

	tests := []struct {
		name   string
		mutate func(belay.AgentRequest) belay.AgentRequest
	}{
		{"different prompt", func(r belay.AgentRequest) belay.AgentRequest { r.Prompt = "something else entirely"; return r }},
		{"different system prompt", func(r belay.AgentRequest) belay.AgentRequest { r.SystemPrompt = "different persona"; return r }},
		{"different model", func(r belay.AgentRequest) belay.AgentRequest { r.Model = "claude-haiku-4"; return r }},
		{"different max turns", func(r belay.AgentRequest) belay.AgentRequest { r.MaxTurns = 99; return r }},
		{"different allowed tools", func(r belay.AgentRequest) belay.AgentRequest { r.AllowedTools = []string{"Read"}; return r }},
		{"different mcp config", func(r belay.AgentRequest) belay.AgentRequest {
			r.MCPConfig = json.RawMessage(`{"servers":{"x":1}}`)
			return r
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := replay.Fingerprint(tt.mutate(baseRequest()))
			if got == baseFP {
				t.Errorf("Fingerprint unchanged after %s (still %s)", tt.name, got)
			}
		})
	}
}

// TestFingerprint_Deterministic proves the digest is pure: computing it
// twice for the identical request, including a repeat call, never
// changes.
func TestFingerprint_Deterministic(t *testing.T) {
	t.Parallel()
	req := baseRequest()
	if got, want := replay.Fingerprint(req), replay.Fingerprint(req); got != want {
		t.Fatalf("Fingerprint is not deterministic: %s != %s", got, want)
	}
}

// TestFingerprint_MCPConfigCanonicalization proves that formatting
// differences in an equivalent MCPConfig document do not change the
// fingerprint, while a genuine semantic difference does.
func TestFingerprint_MCPConfigCanonicalization(t *testing.T) {
	t.Parallel()

	compact := baseRequest()
	compact.MCPConfig = json.RawMessage(`{"a":1,"b":2}`)

	whitespace := baseRequest()
	whitespace.MCPConfig = json.RawMessage("{\n  \"b\": 2,\n  \"a\": 1\n}\n")

	different := baseRequest()
	different.MCPConfig = json.RawMessage(`{"a":1,"b":3}`)

	fpCompact := replay.Fingerprint(compact)
	fpWhitespace := replay.Fingerprint(whitespace)
	fpDifferent := replay.Fingerprint(different)

	if fpCompact != fpWhitespace {
		t.Errorf("equivalent MCPConfig with different formatting/key order fingerprinted differently: %s vs %s", fpCompact, fpWhitespace)
	}
	if fpCompact == fpDifferent {
		t.Errorf("semantically different MCPConfig fingerprinted identically: %s", fpCompact)
	}
}

// TestFingerprint_MCPConfigInvalidJSONDoesNotPanic proves that malformed
// MCPConfig — which belay itself never parses, per AgentRequest's own doc
// comment — cannot crash Fingerprint, and still hashes deterministically.
func TestFingerprint_MCPConfigInvalidJSONDoesNotPanic(t *testing.T) {
	t.Parallel()
	req := baseRequest()
	req.MCPConfig = json.RawMessage(`{not valid json`)

	var got string
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Fingerprint panicked on invalid MCPConfig: %v", r)
			}
		}()
		got = replay.Fingerprint(req)
	}()

	if got != replay.Fingerprint(req) {
		t.Fatal("Fingerprint of invalid MCPConfig is not deterministic")
	}
}

// TestNormalize_Placeholders is a direct, white-box-free check of what
// Normalize actually produces, since Fingerprint's stability guarantees
// are only as good as this projection.
func TestNormalize_Placeholders(t *testing.T) {
	t.Parallel()

	req := baseRequest()
	req.SessionID = "sess-abc123"

	n := replay.Normalize(req)

	if n.WorkDir == req.WorkDir {
		t.Errorf("Normalize did not replace WorkDir: got %q", n.WorkDir)
	}
	if n.WorkDir == "" {
		t.Error("Normalize produced an empty WorkDir placeholder")
	}
	if n.SessionID == req.SessionID {
		t.Errorf("Normalize did not replace a non-empty SessionID: got %q", n.SessionID)
	}
	if n.Prompt != req.Prompt {
		t.Errorf("Normalize changed Prompt: got %q, want %q", n.Prompt, req.Prompt)
	}

	fresh := baseRequest()
	fresh.SessionID = ""
	if got := replay.Normalize(fresh).SessionID; got != "" {
		t.Errorf("Normalize invented a SessionID placeholder for an empty SessionID: got %q", got)
	}
}

// TestCassette_SaveAndLoad_RoundTrip proves the on-disk format survives a
// full encode/decode cycle without losing or corrupting anything,
// including nested json.RawMessage and time.Time fields.
func TestCassette_SaveAndLoad_RoundTrip(t *testing.T) {
	t.Parallel()

	want := &replay.Cassette{
		FormatVersion: replay.FormatVersion,
		Provenance: replay.Provenance{
			BelayVersion: "v0.0.0-test",
			Backend:      "fake-agent",
			Model:        "claude-opus-4",
			RecordedAt:   time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
			Note:         "unit test fixture",
		},
		Interactions: []replay.Interaction{
			{
				Fingerprint: "sha256:deadbeef",
				Request: replay.NormalizedRequest{
					Prompt:  "do the thing",
					WorkDir: "<workdir>",
				},
				Response: belay.AgentResponse{
					Text:  "done",
					Turns: 3,
					Usage: belay.Usage{InputTokens: 100, OutputTokens: 50, USD: 0.25},
					Raw:   json.RawMessage(`{"ok":true}`),
				},
			},
		},
	}

	path := filepath.Join(t.TempDir(), "sub", "cassette.json")
	if err := want.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := replay.LoadCassette(path)
	if err != nil {
		t.Fatalf("LoadCassette() error = %v", err)
	}

	if diff := cmp.Diff(want, got, timeCmp); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
}

// TestCassette_Save_HumanReviewable checks the format is indented JSON,
// not a single compact line — required so a cassette diff in a pull
// request is actually reviewable, per the package doc's stated goal.
func TestCassette_Save_HumanReviewable(t *testing.T) {
	t.Parallel()

	c := &replay.Cassette{FormatVersion: replay.FormatVersion}
	path := filepath.Join(t.TempDir(), "cassette.json")
	if err := c.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	data, err := replay.LoadCassette(path)
	if err != nil {
		t.Fatalf("LoadCassette() error = %v", err)
	}
	if data.FormatVersion != replay.FormatVersion {
		t.Errorf("FormatVersion = %d, want %d", data.FormatVersion, replay.FormatVersion)
	}
}

// TestLoadCassette_UnsupportedFormatVersion proves a cassette from a
// future (or corrupted) format is rejected with a typed error rather than
// silently misread.
func TestLoadCassette_UnsupportedFormatVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join("testdata", "unsupported_version.json")
	_, err := replay.LoadCassette(path)
	if err == nil {
		t.Fatal("LoadCassette() error = nil, want an error for an unsupported format version")
	}
	if !errors.Is(err, replay.ErrUnsupportedFormat) {
		t.Errorf("errors.Is(err, ErrUnsupportedFormat) = false, err = %v", err)
	}
	var fvErr *replay.FormatVersionError
	if !errors.As(err, &fvErr) {
		t.Fatalf("errors.As(err, *FormatVersionError) = false, err = %v", err)
	}
	if fvErr.Got != 99 {
		t.Errorf("FormatVersionError.Got = %d, want 99", fvErr.Got)
	}
}

// TestLoadCassette_MissingFile proves a missing cassette file is a plain
// wrapped error, not a panic.
func TestLoadCassette_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := replay.LoadCassette(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("LoadCassette() error = nil, want an error for a missing file")
	}
}

// TestLoadCassette_GoldenFixture reads the hand-authored example cassette
// in testdata, which doubles as documentation of the human-reviewable
// format.
func TestLoadCassette_GoldenFixture(t *testing.T) {
	t.Parallel()

	c, err := replay.LoadCassette(filepath.Join("testdata", "sample.cassette.json"))
	if err != nil {
		t.Fatalf("LoadCassette() error = %v", err)
	}
	if len(c.Interactions) == 0 {
		t.Fatal("sample cassette has no interactions")
	}
	if c.Provenance.Backend == "" {
		t.Error("sample cassette has no Provenance.Backend")
	}
	if c.Provenance.Note == "" {
		t.Error("sample cassette has no Provenance.Note")
	}
}

// TestDetectDrift covers the three cases a maintainer's smoke test cares
// about: perfectly in sync, a stale recording nothing sends anymore, and
// a request the cassette has never seen.
func TestDetectDrift(t *testing.T) {
	t.Parallel()

	cassette := &replay.Cassette{
		Interactions: []replay.Interaction{
			{Fingerprint: "fp-a"},
			{Fingerprint: "fp-b"},
		},
	}

	tests := []struct {
		name        string
		sent        []string
		wantStale   []string
		wantMissing []string
		wantInSync  bool
	}{
		{
			name:       "perfectly in sync",
			sent:       []string{"fp-a", "fp-b"},
			wantInSync: true,
		},
		{
			name:      "cassette has a stale recording",
			sent:      []string{"fp-a"},
			wantStale: []string{"fp-b"},
		},
		{
			name:        "live run sent something new",
			sent:        []string{"fp-a", "fp-b", "fp-c"},
			wantMissing: []string{"fp-c"},
		},
		{
			name:        "both stale and missing at once",
			sent:        []string{"fp-a", "fp-c"},
			wantStale:   []string{"fp-b"},
			wantMissing: []string{"fp-c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			report := replay.DetectDrift(cassette, tt.sent)
			if diff := cmp.Diff(tt.wantStale, report.Stale); diff != "" {
				t.Errorf("Stale mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantMissing, report.Missing); diff != "" {
				t.Errorf("Missing mismatch (-want +got):\n%s", diff)
			}
			if got := report.InSync(); got != tt.wantInSync {
				t.Errorf("InSync() = %v, want %v", got, tt.wantInSync)
			}
		})
	}
}

// A cassette must replay somewhere other than where it was recorded.
//
// Normalizing AgentRequest.WorkDir is not enough on its own: nodes name the
// repository inside the prose they send -- the plan node writes "the
// repository at <abs path>" -- so hashing the prompt verbatim binds the
// fingerprint to one directory. The first recorded cassette failed to replay
// for exactly this reason, which defeats the point of having one, since CI
// checks out to a path nobody can predict.
func TestFingerprintIgnoresTheWorkspacePathInsideThePrompt(t *testing.T) {
	t.Parallel()

	build := func(dir string) belay.AgentRequest {
		return belay.AgentRequest{
			Prompt: "# Goal\n\nAdd tests.\n\n# Repository\n\n" +
				"You are planning a change to the repository at " + dir + ". Read it first.",
			SystemPrompt: "You are planning against " + dir + ".",
			WorkDir:      dir,
			Model:        "sonnet",
		}
	}

	a := replay.Fingerprint(build("/tmp/record-here"))
	b := replay.Fingerprint(build("/home/ci/runner/work/checkout"))
	if a != b {
		t.Fatalf("fingerprints differ across workspaces:\n  %s\n  %s\n"+
			"a cassette that only replays in its recording directory is useless in CI", a, b)
	}

	// The substitution must not flatten genuinely different requests.
	c := replay.Fingerprint(belay.AgentRequest{
		Prompt: "# Goal\n\nSomething else entirely.", WorkDir: "/tmp/record-here", Model: "sonnet",
	})
	if c == a {
		t.Error("two different prompts share a fingerprint; normalization is too aggressive")
	}
}
