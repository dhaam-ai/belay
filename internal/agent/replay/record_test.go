package replay_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/belay-dev/belay/internal/agent/replay"
	"github.com/belay-dev/belay/pkg/belay"
	"github.com/belay-dev/belay/pkg/belay/belaytest"
)

// TestScrub_TableDriven pins exactly which secret shapes Scrub recognizes
// and proves each one is removed from the output.
func TestScrub_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantAbsent string // substring that must NOT survive scrubbing
	}{
		{
			name:       "anthropic api key",
			input:      "here is the key: sk-ant-api03-abcdefghijklmnopqrstuvwxyz123456",
			wantAbsent: "sk-ant-api03-abcdefghijklmnopqrstuvwxyz123456",
		},
		{
			name:       "bearer token",
			input:      "Authorization: Bearer abcDEF123456.token-value-here",
			wantAbsent: "abcDEF123456.token-value-here",
		},
		{
			name:       "jwt",
			input:      "token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
			wantAbsent: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		},
		{
			name:       "macos home path",
			input:      "wrote output to /Users/alice/projects/secret-repo/notes.md",
			wantAbsent: "/Users/alice",
		},
		{
			name:       "linux home path",
			input:      "wrote output to /home/ci-runner/workspace/notes.md",
			wantAbsent: "/home/ci-runner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := replay.Scrub(tt.input)
			if strings.Contains(got, tt.wantAbsent) {
				t.Errorf("Scrub(%q) = %q, still contains secret %q", tt.input, got, tt.wantAbsent)
			}
		})
	}
}

// TestScrub_EnvironmentKeyValue proves Scrub also redacts the literal
// current ANTHROPIC_API_KEY value, independent of whether it happens to
// look like "sk-ant-...".
func TestScrub_EnvironmentKeyValue(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "totally-opaque-secret-value-987")

	got := replay.Scrub("the backend printed totally-opaque-secret-value-987 by mistake")
	if strings.Contains(got, "totally-opaque-secret-value-987") {
		t.Errorf("Scrub() = %q, still contains the ANTHROPIC_API_KEY value", got)
	}
}

// TestScrub_NoFalsePositivesOnOrdinaryText proves Scrub leaves ordinary
// text alone: a scrubber that over-matches would make a cassette diff
// useless for review.
func TestScrub_NoFalsePositivesOnOrdinaryText(t *testing.T) {
	t.Parallel()
	const text = "implemented the Login handler, added three tests, all green."
	if got := replay.Scrub(text); got != text {
		t.Errorf("Scrub(%q) = %q, want unchanged", text, got)
	}
}

// TestScrub_Empty proves the zero-value input is handled without a panic
// or a spurious allocation-shaped change.
func TestScrub_Empty(t *testing.T) {
	t.Parallel()
	if got := replay.Scrub(""); got != "" {
		t.Errorf(`Scrub("") = %q, want ""`, got)
	}
}

// TestRecord_ScrubsPersistedCassetteButNotTheLiveReturn is the acceptance
// test the task calls out by name: a response containing a secret must
// produce a scrubbed cassette on disk, while the value actually returned
// to the running program (which never gets committed anywhere) is left
// exactly as the inner backend produced it.
func TestRecord_ScrubsPersistedCassetteButNotTheLiveReturn(t *testing.T) {
	t.Parallel()

	// #nosec G101 -- a fake secret shape used to prove Scrub redacts it,
	// not a real credential.
	const secret = "sk-ant-api03-leaked000000000000000000"
	const homePath = "/Users/alice/projects/repo"

	inner := &belaytest.FakeAgent{
		NameValue: "claude-code",
		Responses: []belay.AgentResponse{{
			Text: "applied the fix; by the way here is " + secret + " found in " + homePath,
			Raw:  []byte(`{"note":"contains ` + secret + `"}`),
		}},
	}

	b := replay.Record(inner, replay.Provenance{BelayVersion: "v-test"})

	liveResp, err := b.Invoke(context.Background(), belay.AgentRequest{
		Prompt: "fix it", WorkDir: "/Users/alice/projects/repo/workspace",
	})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}

	// The live, in-process return must be untouched: scrubbing is only
	// for what gets persisted.
	if !strings.Contains(liveResp.Text, secret) {
		t.Errorf("live response was scrubbed; want the original secret to survive in-process: %q", liveResp.Text)
	}

	path := filepath.Join(t.TempDir(), "cassette.json")
	cassette := b.Cassette()
	if err := cassette.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	raw, err := os.ReadFile(path) // #nosec G304 -- test-controlled temp path
	if err != nil {
		t.Fatalf("reading saved cassette: %v", err)
	}
	fileContents := string(raw)
	if strings.Contains(fileContents, secret) {
		t.Errorf("saved cassette file contains the unscrubbed secret %q", secret)
	}
	if strings.Contains(fileContents, "alice") {
		t.Errorf("saved cassette file contains the unscrubbed home path fragment %q", "alice")
	}

	loaded, err := replay.LoadCassette(path)
	if err != nil {
		t.Fatalf("LoadCassette() error = %v", err)
	}
	if len(loaded.Interactions) != 1 {
		t.Fatalf("loaded cassette has %d interactions, want 1", len(loaded.Interactions))
	}
	if strings.Contains(loaded.Interactions[0].Response.Text, secret) {
		t.Errorf("loaded Interaction.Response.Text still contains the secret: %q", loaded.Interactions[0].Response.Text)
	}
	if !strings.Contains(loaded.Interactions[0].Response.Text, "[REDACTED]") {
		t.Errorf("loaded Interaction.Response.Text has no redaction marker: %q", loaded.Interactions[0].Response.Text)
	}
}

// TestRecord_PreservesLiveErrorButScrubsPersistedError proves scrubbing
// also applies to a recorded error message, while the error actually
// returned to the caller (which drives real control flow, such as the
// budget guard or fix loop) is left unscrubbed.
func TestRecord_PreservesLiveErrorButScrubsPersistedError(t *testing.T) {
	t.Parallel()

	// #nosec G101 -- a fake secret shape used to prove Scrub redacts it,
	// not a real credential.
	const secret = "sk-ant-api03-inerrormessage00000000000"
	inner := &belaytest.FakeAgent{
		NameValue: "claude-code",
		Errs:      []error{errors.New("invocation failed, credential was " + secret)},
	}

	b := replay.Record(inner, replay.Provenance{})
	_, err := b.Invoke(context.Background(), belay.AgentRequest{Prompt: "x", WorkDir: "/tmp/x"})
	if err == nil {
		t.Fatal("Invoke() error = nil, want the scripted error")
	}
	if !strings.Contains(err.Error(), secret) {
		t.Errorf("live error was scrubbed; want the original secret to survive in-process: %v", err)
	}

	cassette := b.Cassette()
	if len(cassette.Interactions) != 1 {
		t.Fatalf("cassette has %d interactions, want 1", len(cassette.Interactions))
	}
	if strings.Contains(cassette.Interactions[0].Error, secret) {
		t.Errorf("persisted Interaction.Error still contains the secret: %q", cassette.Interactions[0].Error)
	}
}

// TestRecord_ProvenanceInjected proves every Provenance field (except
// Backend, which Record overwrites — see TestRecord_ProvenanceOverwritesBackend
// in replay_test.go) comes from the caller, never from the system clock
// or any other ambient source inside this package.
func TestRecord_ProvenanceInjected(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	prov := replay.Provenance{
		BelayVersion: "v9.9.9",
		Model:        "claude-opus-4",
		RecordedAt:   fixed,
		Note:         "injected provenance test",
	}

	inner := &belaytest.FakeAgent{NameValue: "claude-code"}
	b := replay.Record(inner, prov)
	if _, err := b.Invoke(context.Background(), belay.AgentRequest{Prompt: "x", WorkDir: "/tmp/x"}); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}

	got := b.Cassette().Provenance
	if got.BelayVersion != prov.BelayVersion {
		t.Errorf("Provenance.BelayVersion = %q, want %q", got.BelayVersion, prov.BelayVersion)
	}
	if got.Model != prov.Model {
		t.Errorf("Provenance.Model = %q, want %q", got.Model, prov.Model)
	}
	if !got.RecordedAt.Equal(fixed) {
		t.Errorf("Provenance.RecordedAt = %v, want %v", got.RecordedAt, fixed)
	}
	if got.Note != prov.Note {
		t.Errorf("Provenance.Note = %q, want %q", got.Note, prov.Note)
	}
}
