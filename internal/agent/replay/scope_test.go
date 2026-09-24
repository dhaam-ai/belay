package replay_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dhaam-ai/belay/internal/agent/replay"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// candidateRequest is the request every candidate of one best-of-N fanout
// receives: the same goal, the same model, the same everything Fingerprint
// hashes, differing only in the workspace each candidate was given — which
// NormalizedRequest deliberately replaces with a fixed placeholder. It is
// the exact shape the package doc used to record as a known limitation.
func candidateRequest(workspace string) belay.AgentRequest {
	return belay.AgentRequest{
		Prompt:  "implement the Login handler",
		WorkDir: workspace,
		Model:   "claude-opus-4",
	}
}

// candidateAgent answers according to the workspace it was pointed at,
// because that is the whole reason two candidates are worth running: two
// agents given the same goal in two working copies come back with two
// different pieces of work.
func candidateAgent() *belaytest.FakeAgent {
	return &belaytest.FakeAgent{
		NameValue: "claude-code",
		Func: func(_ context.Context, r belay.AgentRequest) (belay.AgentResponse, error) {
			return belay.AgentResponse{Text: "work from " + filepath.Base(r.WorkDir)}, nil
		},
	}
}

// TestScopedBackendsDoNotShareFingerprints is 7b's load-bearing test: two
// candidates replaying one goal must each get their own recorded answer
// back, in whichever order they ask for it.
//
// Replay deliberately runs in the OPPOSITE order to the recording. A
// concurrent fanout settles its candidates in whatever order the scheduler
// picks, so a cassette that only replays in recording order is not a
// cassette a fanout can use.
func TestScopedBackendsDoNotShareFingerprints(t *testing.T) {
	t.Parallel()

	const (
		dirOne = "/tmp/belay-run/candidates/cand-01"
		dirTwo = "/tmp/belay-run/candidates/cand-02"
	)

	inner := candidateAgent()
	rec := replay.Record(inner, replay.Provenance{BelayVersion: "v-test", Note: "two candidates, one goal"})

	if _, err := rec.Scoped("cand-01").Invoke(t.Context(), candidateRequest(dirOne)); err != nil {
		t.Fatalf("record cand-01 Invoke() error = %v", err)
	}
	if _, err := rec.Scoped("cand-02").Invoke(t.Context(), candidateRequest(dirTwo)); err != nil {
		t.Fatalf("record cand-02 Invoke() error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "cassette.json")
	cassette := rec.Cassette()
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

	two, err := rep.Scoped("cand-02").Invoke(t.Context(), candidateRequest(dirTwo))
	if err != nil {
		t.Fatalf("replay cand-02 Invoke() error = %v", err)
	}
	if got, want := two.Text, "work from cand-02"; got != want {
		t.Errorf("cand-02 replayed %q, want %q: the two candidates share one fingerprint", got, want)
	}

	one, err := rep.Scoped("cand-01").Invoke(t.Context(), candidateRequest(dirOne))
	if err != nil {
		t.Fatalf("replay cand-01 Invoke() error = %v", err)
	}
	if got, want := one.Text, "work from cand-01"; got != want {
		t.Errorf("cand-01 replayed %q, want %q: the two candidates share one fingerprint", got, want)
	}
}

// TestScopedViewsShareOneRecording proves a scoped view is a view and not
// a separate Backend: every scope records into the one cassette the run
// saves at the end, and the parent's Seen reports what its scopes sent.
//
// If each scope accumulated its own cassette, a fanout run would save one
// candidate's interactions and silently drop the rest.
func TestScopedViewsShareOneRecording(t *testing.T) {
	t.Parallel()

	rec := replay.Record(candidateAgent(), replay.Provenance{BelayVersion: "v-test"})
	for _, id := range []string{"cand-01", "cand-02", "cand-03"} {
		if _, err := rec.Scoped(id).Invoke(t.Context(), candidateRequest("/tmp/belay-run/candidates/"+id)); err != nil {
			t.Fatalf("record %s Invoke() error = %v", id, err)
		}
	}

	cassette := rec.Cassette()
	if got, want := len(cassette.Interactions), 3; got != want {
		t.Fatalf("the parent's cassette holds %d interaction(s), want %d: a scoped view must record into the run's own cassette", got, want)
	}
	if got, want := len(rec.Seen()), 3; got != want {
		t.Errorf("Seen() reports %d fingerprint(s), want %d", got, want)
	}

	seen := map[string]bool{}
	for _, it := range cassette.Interactions {
		if seen[it.Fingerprint] {
			t.Errorf("fingerprint %s recorded twice: the scopes collapsed onto one another", it.Fingerprint)
		}
		seen[it.Fingerprint] = true
	}

	// The scope is stored beside each interaction, not only folded into
	// the hash, so a maintainer reading a cassette diff can tell which
	// candidate an answer belongs to.
	scopes := map[string]bool{}
	for _, it := range cassette.Interactions {
		scopes[it.Request.Scope] = true
	}
	for _, id := range []string{"cand-01", "cand-02", "cand-03"} {
		if !scopes[id] {
			t.Errorf("no interaction carries Request.Scope = %q; got scopes %v", id, scopes)
		}
	}
}

// TestScopedReplacesRatherThanNests pins the one composition rule: a scope
// of a scope is the inner one, so a caller that re-scopes a view it was
// handed cannot build a fingerprint no recording ever used.
func TestScopedReplacesRatherThanNests(t *testing.T) {
	t.Parallel()

	rec := replay.Record(candidateAgent(), replay.Provenance{BelayVersion: "v-test"})
	if _, err := rec.Scoped("cand-01").Scoped("cand-02").Invoke(t.Context(), candidateRequest("/tmp/a")); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if _, err := rec.Scoped("cand-02").Invoke(t.Context(), candidateRequest("/tmp/b")); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}

	got := rec.Seen()
	if len(got) != 2 {
		t.Fatalf("Seen() = %v, want 2 fingerprints", got)
	}
	if got[0] != got[1] {
		t.Errorf("Scoped(%q).Scoped(%q) fingerprinted %s, but Scoped(%q) fingerprinted %s: a scope must replace, not nest",
			"cand-01", "cand-02", got[0], "cand-02", got[1])
	}
}

// unscopedFingerprints are the digests belay computed for these requests
// before scoping existed, captured by running Fingerprint against the code
// as it stood at 9aeb04f.
//
// They are pinned as literals rather than recomputed, and that is the
// entire point of this guard: every cassette already checked in — and
// every one a user has recorded — is keyed by these digests. An
// implementation that folded an EMPTY scope into the hash would still be
// self-consistent, would still pass every other test in this package, and
// would silently invalidate all of them. Only a literal catches that.
var unscopedFingerprints = []struct {
	name string
	req  belay.AgentRequest
	want string
}{
	{
		name: "a request with every field a node sets",
		req: belay.AgentRequest{
			Prompt:       "implement the Login handler",
			SystemPrompt: "you are a careful senior engineer",
			WorkDir:      "/Users/alice/tmp/belay-run-1/workspace",
			AllowedTools: []string{"Read", "Edit", "Bash"},
			MaxTurns:     8,
			Model:        "claude-opus-4",
		},
		want: "sha256:2b8f1ec03de14750fabfee94cf9c37cfdb0173020b85814d854c4905a4aedd85",
	},
	{
		name: "a bare prompt and nothing else",
		req:  belay.AgentRequest{Prompt: "hello"},
		want: "sha256:33ab2a8f8ba88d8c280ec7402390a2cc7a3b0e1ce0ac3e6f499e82fe43f4aab3",
	},
}

// TestUnscopedFingerprintIsUnchanged is a regression guard, not a
// behaviour this change introduces: an unscoped Backend must fingerprint
// exactly as it did before Scoped existed.
func TestUnscopedFingerprintIsUnchanged(t *testing.T) {
	t.Parallel()

	for _, tt := range unscopedFingerprints {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := replay.Fingerprint(tt.req); got != tt.want {
				t.Errorf("Fingerprint() = %s, want %s\n"+
					"this changes the key every existing cassette is recorded under", got, tt.want)
			}
		})
	}
}

// TestScopedWithAnEmptyScopeIsUnscoped proves the same guard through the
// Backend rather than through Fingerprint: the empty scope is not a scope,
// so a caller that has no candidate to name — every run belay walks today
// — records under the digests cassettes already hold.
func TestScopedWithAnEmptyScopeIsUnscoped(t *testing.T) {
	t.Parallel()

	tt := unscopedFingerprints[0]
	rec := replay.Record(candidateAgent(), replay.Provenance{BelayVersion: "v-test"})

	if _, err := rec.Invoke(t.Context(), tt.req); err != nil {
		t.Fatalf("unscoped Invoke() error = %v", err)
	}
	if _, err := rec.Scoped("").Invoke(t.Context(), tt.req); err != nil {
		t.Fatalf(`Scoped("") Invoke() error = %v`, err)
	}

	for i, fp := range rec.Seen() {
		if fp != tt.want {
			t.Errorf("Seen()[%d] = %s, want %s: an empty scope must not reach the hash", i, fp, tt.want)
		}
	}
	for _, it := range rec.Cassette().Interactions {
		if it.Request.Scope != "" {
			t.Errorf("an unscoped interaction stored Scope = %q, want empty", it.Request.Scope)
		}
	}
}
