package replay_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/internal/agent/replay"
	"github.com/belay-dev/belay/pkg/belay"
	"github.com/belay-dev/belay/pkg/belay/belaytest"
)

func req(prompt string) belay.AgentRequest {
	return belay.AgentRequest{
		Prompt:  prompt,
		WorkDir: "/Users/tester/tmp/belay-run/workspace",
		Model:   "claude-opus-4",
	}
}

var _ belay.AgentBackend = (*replay.Backend)(nil)

// TestBackend_Live_PassesThrough proves ModeLive is a pure pass-through:
// the inner backend answers, and Backend adds nothing and hides nothing.
func TestBackend_Live_PassesThrough(t *testing.T) {
	t.Parallel()

	inner := &belaytest.FakeAgent{
		NameValue: "fake-agent",
		Responses: []belay.AgentResponse{{Text: "live answer"}},
	}
	b := replay.Live(inner)

	if got, want := b.Name(), "fake-agent"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}

	resp, err := b.Invoke(context.Background(), req("do it"))
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if resp.Text != "live answer" {
		t.Errorf("Invoke() Text = %q, want %q", resp.Text, "live answer")
	}
	if inner.CallCount() != 1 {
		t.Errorf("inner.CallCount() = %d, want 1", inner.CallCount())
	}
}

// TestBackend_RecordThenReplay_RoundTrip is the acceptance test at the
// heart of this package: record two distinct requests against a fake
// backend, save the cassette, load it back in a completely separate
// Backend, and prove replay reproduces byte-identical AgentResponses
// while never touching any inner backend.
func TestBackend_RecordThenReplay_RoundTrip(t *testing.T) {
	t.Parallel()

	inner := &belaytest.FakeAgent{
		NameValue: "claude-code",
		Func: func(_ context.Context, r belay.AgentRequest) (belay.AgentResponse, error) {
			switch r.Prompt {
			case "plan the feature":
				return belay.AgentResponse{
					Text: "here is the plan", SessionID: "sess-1", Turns: 2,
					Usage: belay.Usage{InputTokens: 100, OutputTokens: 40, USD: 0.12},
					Raw:   []byte(`{"subtype":"success","turns":2}`),
				}, nil
			case "write the code":
				return belay.AgentResponse{
					Text: "code written", SessionID: "sess-2", Turns: 5,
					Usage: belay.Usage{InputTokens: 500, OutputTokens: 300, USD: 0.85, Estimated: true},
					Raw:   []byte(`{"subtype":"success","turns":5}`),
				}, nil
			default:
				t.Fatalf("unexpected prompt %q", r.Prompt)
				return belay.AgentResponse{}, nil
			}
		},
	}

	rec := replay.Record(inner, replay.Provenance{BelayVersion: "v-test", Note: "round trip test"})

	requests := []belay.AgentRequest{req("plan the feature"), req("write the code")}
	var recorded []belay.AgentResponse
	for _, r := range requests {
		resp, err := rec.Invoke(context.Background(), r)
		if err != nil {
			t.Fatalf("record Invoke(%q) error = %v", r.Prompt, err)
		}
		recorded = append(recorded, resp)
	}

	cassette := rec.Cassette()
	path := filepath.Join(t.TempDir(), "cassette.json")
	if err := cassette.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := replay.LoadCassette(path)
	if err != nil {
		t.Fatalf("LoadCassette() error = %v", err)
	}

	rep, err := replay.Replay(loaded)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}

	for i, r := range requests {
		got, err := rep.Invoke(context.Background(), r)
		if err != nil {
			t.Fatalf("replay Invoke(%q) error = %v", r.Prompt, err)
		}
		if diff := cmp.Diff(recorded[i], got); diff != "" {
			t.Errorf("replay of %q not byte-identical to recorded response (-recorded +replayed):\n%s", r.Prompt, diff)
		}
	}

	if got := inner.CallCount(); got != len(requests) {
		t.Errorf("inner.CallCount() = %d after record+replay, want %d (replay must never call inner)", got, len(requests))
	}
}

// TestBackend_Replay_MissingInteraction proves a request with no
// matching interaction produces a loud, typed, fingerprint-naming error —
// and, critically, never falls through to any inner backend.
func TestBackend_Replay_MissingInteraction(t *testing.T) {
	t.Parallel()

	cassette := &replay.Cassette{
		FormatVersion: replay.FormatVersion,
		Interactions: []replay.Interaction{
			{Fingerprint: replay.Fingerprint(req("a recorded request")), Response: belay.AgentResponse{Text: "recorded"}},
		},
	}

	poisoned := &belaytest.FakeAgent{
		Func: func(_ context.Context, r belay.AgentRequest) (belay.AgentResponse, error) {
			t.Fatalf("inner backend was called during replay with prompt %q — this must never happen", r.Prompt)
			return belay.AgentResponse{}, nil
		},
	}

	// New's ModeReplay branch must discard inner entirely: passing a
	// poisoned fake here proves it, rather than merely asserting on a
	// Backend built via Replay (which cannot even reference inner).
	b, err := replay.New(replay.ModeReplay, poisoned, cassette, replay.Provenance{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	miss := req("a request that was never recorded")
	_, err = b.Invoke(context.Background(), miss)
	if err == nil {
		t.Fatal("Invoke() error = nil, want a MissingInteractionError")
	}

	if !errors.Is(err, replay.ErrNoInteraction) {
		t.Errorf("errors.Is(err, ErrNoInteraction) = false, err = %v", err)
	}
	var mie *replay.MissingInteractionError
	if !errors.As(err, &mie) {
		t.Fatalf("errors.As(err, *MissingInteractionError) = false, err = %v", err)
	}
	wantFP := replay.Fingerprint(miss)
	if mie.Fingerprint != wantFP {
		t.Errorf("MissingInteractionError.Fingerprint = %q, want %q", mie.Fingerprint, wantFP)
	}
	if mie.Recorded != 0 {
		t.Errorf("MissingInteractionError.Recorded = %d, want 0", mie.Recorded)
	}
	if !strings.Contains(err.Error(), wantFP) {
		t.Errorf("error message %q does not name the fingerprint %q", err.Error(), wantFP)
	}

	if got := poisoned.CallCount(); got != 0 {
		t.Errorf("poisoned inner CallCount() = %d, want 0: a miss must never fall through to a live call", got)
	}
}

// TestBackend_RepeatedIdenticalRequests exercises the fingerprint-keyed-
// with-repeat-count design documented in the package doc: N identical
// requests are served their recorded responses IN ORDER, and a request
// beyond what was recorded is a MissingInteractionError naming how many
// were actually recorded.
func TestBackend_RepeatedIdenticalRequests(t *testing.T) {
	t.Parallel()

	same := req("attempt the fix again")
	fp := replay.Fingerprint(same)

	cassette := &replay.Cassette{
		FormatVersion: replay.FormatVersion,
		Interactions: []replay.Interaction{
			{Fingerprint: fp, Response: belay.AgentResponse{Text: "attempt 1 output"}},
			{Fingerprint: fp, Response: belay.AgentResponse{Text: "attempt 2 output"}},
			{Fingerprint: fp, Response: belay.AgentResponse{Text: "attempt 3 output"}},
		},
	}

	b, err := replay.Replay(cassette)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}

	wantOrder := []string{"attempt 1 output", "attempt 2 output", "attempt 3 output"}
	for i, want := range wantOrder {
		resp, err := b.Invoke(context.Background(), same)
		if err != nil {
			t.Fatalf("call %d: Invoke() error = %v", i+1, err)
		}
		if resp.Text != want {
			t.Errorf("call %d: Text = %q, want %q", i+1, resp.Text, want)
		}
	}

	// A 4th call for the same fingerprint has nothing left to serve.
	_, err = b.Invoke(context.Background(), same)
	if err == nil {
		t.Fatal("4th call: error = nil, want MissingInteractionError")
	}
	var mie *replay.MissingInteractionError
	if !errors.As(err, &mie) {
		t.Fatalf("4th call: errors.As(err, *MissingInteractionError) = false, err = %v", err)
	}
	if mie.Attempt != 4 {
		t.Errorf("4th call: Attempt = %d, want 4", mie.Attempt)
	}
	if mie.Recorded != 3 {
		t.Errorf("4th call: Recorded = %d, want 3", mie.Recorded)
	}
}

// TestBackend_RepeatedIdenticalRequests_DoesNotInterfereWithOtherFingerprints
// proves per-fingerprint cursors are independent: consuming one
// fingerprint's queue must not disturb another's.
func TestBackend_RepeatedIdenticalRequests_DoesNotInterfereWithOtherFingerprints(t *testing.T) {
	t.Parallel()

	reqA := req("prompt A")
	reqB := req("prompt B")

	cassette := &replay.Cassette{
		FormatVersion: replay.FormatVersion,
		Interactions: []replay.Interaction{
			{Fingerprint: replay.Fingerprint(reqA), Response: belay.AgentResponse{Text: "A-1"}},
			{Fingerprint: replay.Fingerprint(reqB), Response: belay.AgentResponse{Text: "B-1"}},
			{Fingerprint: replay.Fingerprint(reqA), Response: belay.AgentResponse{Text: "A-2"}},
		},
	}

	b, err := replay.Replay(cassette)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}

	tests := []struct {
		req  belay.AgentRequest
		want string
	}{
		{reqA, "A-1"},
		{reqB, "B-1"},
		{reqA, "A-2"},
	}
	for i, tt := range tests {
		resp, err := b.Invoke(context.Background(), tt.req)
		if err != nil {
			t.Fatalf("call %d: Invoke() error = %v", i, err)
		}
		if resp.Text != tt.want {
			t.Errorf("call %d: Text = %q, want %q", i, resp.Text, tt.want)
		}
	}
}

// TestBackend_Replay_ErrorInteraction proves a recorded failure replays
// as a ReplayedError carrying the scrubbed message, alongside whatever
// partial response was recorded, matching AgentBackend.Invoke's contract
// that a failed call may still carry a meaningful response.
func TestBackend_Replay_ErrorInteraction(t *testing.T) {
	t.Parallel()

	r := req("a call that failed")
	cassette := &replay.Cassette{
		FormatVersion: replay.FormatVersion,
		Interactions: []replay.Interaction{
			{
				Fingerprint: replay.Fingerprint(r),
				Response:    belay.AgentResponse{Text: "partial output before failure"},
				Error:       "claude reported subtype error_max_turns",
			},
		},
	}

	b, err := replay.Replay(cassette)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}

	resp, err := b.Invoke(context.Background(), r)
	if err == nil {
		t.Fatal("Invoke() error = nil, want a ReplayedError")
	}
	var re *replay.ReplayedError
	if !errors.As(err, &re) {
		t.Fatalf("errors.As(err, *ReplayedError) = false, err = %v", err)
	}
	if re.Message != "claude reported subtype error_max_turns" {
		t.Errorf("ReplayedError.Message = %q, want the recorded message", re.Message)
	}
	if resp.Text != "partial output before failure" {
		t.Errorf("Invoke() still returned Text = %q, want the partial recorded response", resp.Text)
	}
}

// TestBackend_Name checks Name's mode-dependent identity: the inner
// backend's name in Live/Record, and the cassette's recorded Provenance
// in Replay (where there is no inner backend to ask).
func TestBackend_Name(t *testing.T) {
	t.Parallel()

	t.Run("live", func(t *testing.T) {
		t.Parallel()
		b := replay.Live(&belaytest.FakeAgent{NameValue: "claude-code"})
		if got := b.Name(); got != "claude-code" {
			t.Errorf("Name() = %q, want %q", got, "claude-code")
		}
	})

	t.Run("record", func(t *testing.T) {
		t.Parallel()
		b := replay.Record(&belaytest.FakeAgent{NameValue: "claude-code"}, replay.Provenance{Backend: "ignored-should-be-overwritten"})
		if got := b.Name(); got != "claude-code" {
			t.Errorf("Name() = %q, want %q", got, "claude-code")
		}
	})

	t.Run("replay", func(t *testing.T) {
		t.Parallel()
		cassette := &replay.Cassette{
			FormatVersion: replay.FormatVersion,
			Provenance:    replay.Provenance{Backend: "claude-code"},
		}
		b, err := replay.Replay(cassette)
		if err != nil {
			t.Fatalf("Replay() error = %v", err)
		}
		if got := b.Name(); got != "claude-code" {
			t.Errorf("Name() = %q, want %q", got, "claude-code")
		}
	})
}

// TestRecord_ProvenanceOverwritesBackend proves Record always tags the
// cassette with inner.Name(), never trusting the caller's own Provenance.Backend.
func TestRecord_ProvenanceOverwritesBackend(t *testing.T) {
	t.Parallel()

	inner := &belaytest.FakeAgent{NameValue: "claude-code"}
	b := replay.Record(inner, replay.Provenance{Backend: "totally-wrong-name"})

	got := b.Cassette().Provenance.Backend
	if got != "claude-code" {
		t.Errorf("Provenance.Backend = %q, want %q (from inner.Name())", got, "claude-code")
	}
}

// TestBackend_Seen proves every Invoke's fingerprint is tracked
// regardless of mode, which is what DetectDrift needs from a live or
// recording smoke-test run.
func TestBackend_Seen(t *testing.T) {
	t.Parallel()

	inner := &belaytest.FakeAgent{}
	b := replay.Live(inner)

	r1, r2 := req("first"), req("second")
	if _, err := b.Invoke(context.Background(), r1); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if _, err := b.Invoke(context.Background(), r2); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}

	want := []string{replay.Fingerprint(r1), replay.Fingerprint(r2)}
	if diff := cmp.Diff(want, b.Seen()); diff != "" {
		t.Errorf("Seen() mismatch (-want +got):\n%s", diff)
	}
}

// TestBackend_New_InvalidMode proves an unrecognized Mode is a plain
// error, not a panic, and that it wraps ErrInvalidMode.
func TestBackend_New_InvalidMode(t *testing.T) {
	t.Parallel()
	_, err := replay.New(replay.Mode("bogus"), nil, nil, replay.Provenance{})
	if !errors.Is(err, replay.ErrInvalidMode) {
		t.Errorf("errors.Is(err, ErrInvalidMode) = false, err = %v", err)
	}
}

// TestBackend_New_RequiresInnerForLiveAndRecord proves a nil inner is
// rejected up front with an error, instead of deferring to a nil-pointer
// panic on the first Invoke.
func TestBackend_New_RequiresInnerForLiveAndRecord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode replay.Mode
	}{
		{"live", replay.ModeLive},
		{"record", replay.ModeRecord},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := replay.New(tt.mode, nil, nil, replay.Provenance{})
			if err == nil {
				t.Fatalf("New(%s, nil, ...) error = nil, want an error", tt.mode)
			}
		})
	}
}

// TestBackend_ZeroValueIsNotUsable proves the documented contract: a
// Backend{} constructed without Live/Record/Replay/New fails loudly
// through the normal error return, not a panic.
func TestBackend_ZeroValueIsNotUsable(t *testing.T) {
	t.Parallel()
	var b replay.Backend
	_, err := b.Invoke(context.Background(), req("anything"))
	if !errors.Is(err, replay.ErrInvalidMode) {
		t.Errorf("errors.Is(err, ErrInvalidMode) = false, err = %v", err)
	}
}

// TestBackend_Replay_RespectsCanceledContext proves ModeReplay still
// honors AgentBackend.Invoke's contract to respect ctx, even though a
// replay has no subprocess or network call to abort.
func TestBackend_Replay_RespectsCanceledContext(t *testing.T) {
	t.Parallel()

	cassette := &replay.Cassette{FormatVersion: replay.FormatVersion}
	b, err := replay.Replay(cassette)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = b.Invoke(ctx, req("anything"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, err = %v", err)
	}
}

// TestReplay_RejectsNilAndBadVersion covers Replay's own guard rails,
// independent of LoadCassette's.
func TestReplay_RejectsNilAndBadVersion(t *testing.T) {
	t.Parallel()

	if _, err := replay.Replay(nil); err == nil {
		t.Error("Replay(nil) error = nil, want an error")
	}

	_, err := replay.Replay(&replay.Cassette{FormatVersion: replay.FormatVersion + 1})
	if !errors.Is(err, replay.ErrUnsupportedFormat) {
		t.Errorf("errors.Is(err, ErrUnsupportedFormat) = false, err = %v", err)
	}
}
