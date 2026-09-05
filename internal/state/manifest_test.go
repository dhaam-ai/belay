package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/pkg/belay"
)

func TestRunStatus_StringParseRoundTrip(t *testing.T) {
	all := []RunStatus{
		RunStatusUnknown, RunStatusRunning, RunStatusPaused,
		RunStatusCompleted, RunStatusFailed, RunStatusAborted,
	}
	for _, s := range all {
		t.Run(s.String(), func(t *testing.T) {
			got, err := ParseRunStatus(s.String())
			if err != nil {
				t.Fatalf("ParseRunStatus(%q): %v", s.String(), err)
			}
			if got != s {
				t.Errorf("ParseRunStatus(%q) = %v, want %v", s.String(), got, s)
			}
		})
	}
}

func TestParseRunStatus_CaseAndWhitespaceInsensitive(t *testing.T) {
	got, err := ParseRunStatus("  RUNNING \t")
	if err != nil {
		t.Fatalf("ParseRunStatus: %v", err)
	}
	if got != RunStatusRunning {
		t.Errorf("got %v, want RunStatusRunning", got)
	}
}

func TestParseRunStatus_Unknown(t *testing.T) {
	_, err := ParseRunStatus("sleeping")
	if !errors.Is(err, ErrUnknownRunStatus) {
		t.Errorf("error does not wrap ErrUnknownRunStatus: %v", err)
	}
}

func TestRunStatus_MarshalUnmarshalJSON(t *testing.T) {
	type wrapper struct {
		Status RunStatus `json:"status"`
	}
	data, err := json.Marshal(wrapper{Status: RunStatusPaused})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(data, []byte(`"paused"`)) {
		t.Fatalf("expected textual status in %s", data)
	}
	var w wrapper
	if err := json.Unmarshal(data, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if w.Status != RunStatusPaused {
		t.Errorf("got %v, want RunStatusPaused", w.Status)
	}
}

// TestRunStatus_ZeroValueMarshalsAsUnknown documents that the zero value,
// like belay.GateStatus's GateUnknown and internal/journal's EventUnknown,
// is itself a named, marshalable value ("unknown") rather than one
// MarshalText refuses — only a RunStatus outside the declared range is
// rejected (see TestRunStatus_OutOfRangeRejectedByMarshalText).
func TestRunStatus_ZeroValueMarshalsAsUnknown(t *testing.T) {
	text, err := RunStatusUnknown.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if string(text) != "unknown" {
		t.Errorf("MarshalText = %q, want %q", text, "unknown")
	}
}

func TestRunStatus_OutOfRangeRejectedByMarshalText(t *testing.T) {
	if _, err := RunStatus(99).MarshalText(); err == nil {
		t.Error("expected MarshalText to reject an out-of-range value")
	} else if !errors.Is(err, ErrUnknownRunStatus) {
		t.Errorf("error does not wrap ErrUnknownRunStatus: %v", err)
	}
}

func TestRunStatus_StringOfOutOfRangeValue(t *testing.T) {
	got := RunStatus(99).String()
	if !strings.Contains(got, "99") {
		t.Errorf("String() = %q, want it to mention the out-of-range value", got)
	}
}

func TestNewRunIDAt_Deterministic(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	random := bytes.NewReader([]byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02})

	got, err := NewRunIDAt(now, random)
	if err != nil {
		t.Fatalf("NewRunIDAt: %v", err)
	}
	want := "20260102T030405Z-deadbeef0102"
	if got != want {
		t.Errorf("NewRunIDAt = %q, want %q", got, want)
	}

	// Calling again with fresh, identical inputs must reproduce the same
	// ID: determinism, not just a plausible shape.
	random2 := bytes.NewReader([]byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02})
	got2, err := NewRunIDAt(now, random2)
	if err != nil {
		t.Fatalf("NewRunIDAt (second call): %v", err)
	}
	if got2 != want {
		t.Errorf("NewRunIDAt (second call) = %q, want %q", got2, want)
	}
}

func TestNewRunIDAt_SortsChronologically(t *testing.T) {
	earlier, err := NewRunIDAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), bytes.NewReader(bytes.Repeat([]byte{0xff}, 6)))
	if err != nil {
		t.Fatalf("NewRunIDAt: %v", err)
	}
	later, err := NewRunIDAt(time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC), bytes.NewReader(bytes.Repeat([]byte{0x00}, 6)))
	if err != nil {
		t.Fatalf("NewRunIDAt: %v", err)
	}
	ids := []string{later, earlier}
	sort.Strings(ids)
	if ids[0] != earlier || ids[1] != later {
		t.Errorf("lexicographic sort %v did not match chronological order [%q, %q]", ids, earlier, later)
	}
}

func TestNewRunIDAt_FilesystemSafe(t *testing.T) {
	id, err := NewRunIDAt(time.Now(), strings.NewReader(strings.Repeat("a", 32)))
	if err != nil {
		t.Fatalf("NewRunIDAt: %v", err)
	}
	if strings.ContainsAny(id, "/:\\ ") {
		t.Errorf("run id %q contains a filesystem-unsafe character", id)
	}
	if err := validateSegment("run id", id); err != nil {
		t.Errorf("generated run id fails its own validation: %v", err)
	}
}

func TestNewRunIDAt_ExhaustedRandomSource(t *testing.T) {
	_, err := NewRunIDAt(time.Now(), bytes.NewReader(nil))
	if err == nil {
		t.Fatal("expected an error when the random source has no bytes")
	}
}

func TestNewRunID_ProducesDistinctIDs(t *testing.T) {
	a, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	b, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	if a == b {
		t.Errorf("two real calls to NewRunID produced the same ID: %q", a)
	}
}

func TestNewLayout_RejectsEscapingRunIDs(t *testing.T) {
	tests := []struct {
		name  string
		runID string
	}{
		{"parent traversal", "../escape"},
		{"absolute path", "/etc/passwd"},
		{"embedded separator", "a/b"},
		{"empty", ""},
		{"bare dotdot", ".."},
		{"embedded backslash", `a\b`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewLayout(t.TempDir(), tt.runID)
			if err == nil {
				t.Fatalf("NewLayout(%q) succeeded, want a rejection", tt.runID)
			}
			if !errors.Is(err, ErrInvalidPathSegment) {
				t.Errorf("error does not wrap ErrInvalidPathSegment: %v", err)
			}
		})
	}
}

func TestNewLayout_AcceptsGeneratedRunID(t *testing.T) {
	id, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	if _, err := NewLayout(t.TempDir(), id); err != nil {
		t.Errorf("NewLayout rejected a NewRunID output %q: %v", id, err)
	}
}

func TestLayout_Paths(t *testing.T) {
	workspace := t.TempDir()
	layout, err := NewLayout(workspace, "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	root := filepath.Join(workspace, ".belay", "runs", "run-1")
	cases := map[string]string{
		"RunDir":        layout.RunDir(),
		"ManifestPath":  layout.ManifestPath(),
		"StatePath":     layout.StatePath(),
		"JournalPath":   layout.JournalPath(),
		"ArtifactsDir":  layout.ArtifactsDir(),
		"NodesDir":      layout.NodesDir(),
		"CandidatesDir": layout.CandidatesDir(),
	}
	want := map[string]string{
		"RunDir":        root,
		"ManifestPath":  filepath.Join(root, "manifest.json"),
		"StatePath":     filepath.Join(root, "state.json"),
		"JournalPath":   filepath.Join(root, "journal.ndjson"),
		"ArtifactsDir":  filepath.Join(root, "artifacts"),
		"NodesDir":      filepath.Join(root, "nodes"),
		"CandidatesDir": filepath.Join(root, "candidates"),
	}
	for k, got := range cases {
		if got != want[k] {
			t.Errorf("%s = %q, want %q", k, got, want[k])
		}
	}
}

func TestLayout_ArtifactPath(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	got, err := layout.ArtifactPath("diff-0007.patch")
	if err != nil {
		t.Fatalf("ArtifactPath: %v", err)
	}
	want := filepath.Join(layout.ArtifactsDir(), "diff-0007.patch")
	if got != want {
		t.Errorf("ArtifactPath = %q, want %q", got, want)
	}

	if _, err := layout.ArtifactPath("../escape.txt"); err == nil {
		t.Error("ArtifactPath accepted a traversal name")
	}
}

func TestLayout_NodeDir(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	got, err := layout.NodeDir(3, "code")
	if err != nil {
		t.Fatalf("NodeDir: %v", err)
	}
	want := filepath.Join(layout.NodesDir(), "003-code")
	if got != want {
		t.Errorf("NodeDir = %q, want %q", got, want)
	}

	if _, err := layout.NodeDir(-1, "code"); err == nil {
		t.Error("NodeDir accepted a negative seq")
	}
	if _, err := layout.NodeDir(1, "../escape"); err == nil {
		t.Error("NodeDir accepted a traversal name")
	}
}

func TestLayout_CandidateDir(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	got, err := layout.CandidateDir("cand-a")
	if err != nil {
		t.Fatalf("CandidateDir: %v", err)
	}
	want := filepath.Join(layout.CandidatesDir(), "cand-a")
	if got != want {
		t.Errorf("CandidateDir = %q, want %q", got, want)
	}

	if _, err := layout.CandidateDir("/etc/passwd"); err == nil {
		t.Error("CandidateDir accepted an absolute id")
	}
}

func TestRedactSnapshot_KeyShapedSecret(t *testing.T) {
	snap := config.Snapshot{
		"agent.backend":       "claude-code",
		"review.ai.mcp.token": "not-even-random-but-the-key-says-token",
	}
	got, redacted := RedactSnapshot(snap)
	if got["agent.backend"] != "claude-code" {
		t.Errorf("unrelated key was touched: %q", got["agent.backend"])
	}
	if got["review.ai.mcp.token"] != redactedValue {
		t.Errorf("token-named key not redacted: %q", got["review.ai.mcp.token"])
	}
	if len(redacted) != 1 || redacted[0] != "review.ai.mcp.token" {
		t.Errorf("redacted keys = %v, want [review.ai.mcp.token]", redacted)
	}
}

func TestRedactSnapshot_ValueShapedSecret(t *testing.T) {
	snap := config.Snapshot{
		"test.custom_cmd": "sk-abcdefghijklmnopqrstuvwxyz0123456789",
		"agent.model":     "sonnet",
	}
	got, redacted := RedactSnapshot(snap)
	if got["test.custom_cmd"] != redactedValue {
		t.Errorf("secret-shaped value not redacted: %q", got["test.custom_cmd"])
	}
	if got["agent.model"] != "sonnet" {
		t.Errorf("unrelated value was touched: %q", got["agent.model"])
	}
	if len(redacted) != 1 || redacted[0] != "test.custom_cmd" {
		t.Errorf("redacted keys = %v, want [test.custom_cmd]", redacted)
	}
}

func TestRedactSnapshot_OrdinaryValuesUntouched(t *testing.T) {
	snap := config.Default().Snapshot()
	got, redacted := RedactSnapshot(snap)
	if len(redacted) != 0 {
		t.Errorf("default config's snapshot was redacted: %v", redacted)
	}
	if diff := cmp.Diff(map[string]string(snap), map[string]string(got)); diff != "" {
		t.Errorf("ordinary snapshot changed by RedactSnapshot (-want +got):\n%s", diff)
	}
}

func TestRedactSnapshot_DoesNotMutateInput(t *testing.T) {
	snap := config.Snapshot{"agent.token": "whatever"}
	_, _ = RedactSnapshot(snap)
	if snap["agent.token"] != "whatever" {
		t.Errorf("RedactSnapshot mutated its input: %q", snap["agent.token"])
	}
}

func TestNewManifest_ScrubsSecretLookingSnapshotValues(t *testing.T) {
	cfg := config.Default()
	cfg.Test.CustomCmd = "sk-abcdefghijklmnopqrstuvwxyz0123456789"

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := NewManifest(now, "run-1", "/work", "goal", cfg)

	if m.ConfigSnapshot["test.custom_cmd"] != redactedValue {
		t.Errorf("secret-shaped config value leaked into manifest: %q", m.ConfigSnapshot["test.custom_cmd"])
	}
	// Every other field must survive untouched.
	if m.ConfigSnapshot["agent.backend"] != cfg.Agent.Backend {
		t.Errorf("unrelated snapshot field was altered: %q", m.ConfigSnapshot["agent.backend"])
	}
}

func TestNewManifest_Defaults(t *testing.T) {
	cfg := config.Default()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m := NewManifest(now, "run-1", "/work/repo", "ship the widget", cfg)

	if m.SchemaVersion != ManifestSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", m.SchemaVersion, ManifestSchemaVersion)
	}
	if m.RunID != "run-1" || m.Workspace != "/work/repo" || m.Goal != "ship the widget" {
		t.Errorf("identity fields wrong: %+v", m)
	}
	if !m.CreatedAt.Equal(now) || !m.UpdatedAt.Equal(now) {
		t.Errorf("CreatedAt/UpdatedAt = %v/%v, want both %v", m.CreatedAt, m.UpdatedAt, now)
	}
	if m.Status != RunStatusRunning {
		t.Errorf("Status = %v, want RunStatusRunning", m.Status)
	}
	if m.ConfigDigest != cfg.Digest() {
		t.Errorf("ConfigDigest = %q, want %q", m.ConfigDigest, cfg.Digest())
	}
	if m.Budget.LimitUSD != cfg.Budget.MaxUSD {
		t.Errorf("Budget.LimitUSD = %v, want %v", m.Budget.LimitUSD, cfg.Budget.MaxUSD)
	}
	if m.Agent.Backend != cfg.Agent.Backend || m.Agent.Model != cfg.Agent.Model || m.Agent.Mode != string(cfg.Agent.Mode) {
		t.Errorf("Agent = %+v, want backend=%q model=%q mode=%q", m.Agent, cfg.Agent.Backend, cfg.Agent.Model, cfg.Agent.Mode)
	}
}

func TestManifest_Touch(t *testing.T) {
	m := NewManifest(time.Unix(0, 0), "run-1", "/work", "goal", config.Default())
	later := time.Unix(1000, 0)
	m.Touch(later)
	if !m.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt = %v, want %v", m.UpdatedAt, later)
	}
}

func TestBudget_AccumulateAndRemaining(t *testing.T) {
	b := Budget{LimitUSD: 10}
	b.Accumulate(belay.Usage{InputTokens: 100, OutputTokens: 50, USD: 1.5, Estimated: false})
	b.Accumulate(belay.Usage{InputTokens: 10, OutputTokens: 5, USD: 0.25, Estimated: true})

	if b.SpentUSD != 1.75 {
		t.Errorf("SpentUSD = %v, want 1.75", b.SpentUSD)
	}
	if b.TokensIn != 110 || b.TokensOut != 55 {
		t.Errorf("tokens = in:%d out:%d, want in:110 out:55", b.TokensIn, b.TokensOut)
	}
	if !b.Estimated {
		t.Error("Estimated should be true once any contribution was estimated")
	}
	if got, want := b.Remaining(), 8.25; got != want {
		t.Errorf("Remaining = %v, want %v", got, want)
	}
}

func TestManifest_SaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	want := NewManifest(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "run-1", "/work/repo", "goal", config.Default())
	want.CurrentNode = "code"
	want.Step = 3
	want.Budget.Accumulate(belay.Usage{USD: 0.5, InputTokens: 100, OutputTokens: 20})

	if err := SaveManifest(path, want); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	got, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
}

func TestLoadManifest_UnknownSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":7,"run_id":"x"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := LoadManifest(path)
	if !errors.Is(err, ErrUnsupportedManifestVersion) {
		t.Errorf("error does not wrap ErrUnsupportedManifestVersion: %v", err)
	}
	var ve *ManifestVersionError
	if !errors.As(err, &ve) || ve.Got != 7 {
		t.Errorf("error is not a *ManifestVersionError with Got=7: %v", err)
	}
}

func TestManifest_JSONKeySetMatchesSpec(t *testing.T) {
	m := NewManifest(time.Now(), "run-1", "/work", "goal", config.Default())
	want := []string{
		"schema_version", "run_id", "created_at", "updated_at", "workspace", "goal",
		"status", "current_node", "step", "config_digest", "config_snapshot", "budget", "agent",
	}
	assertExactJSONKeys(t, m, want)
	assertExactJSONKeys(t, m.Budget, []string{"limit_usd", "spent_usd", "tokens_in", "tokens_out", "estimated"})
	assertExactJSONKeys(t, m.Agent, []string{"backend", "model", "mode"})
}

func TestManifestGolden(t *testing.T) {
	cfg := config.Default()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := NewManifest(now, "20260101T000000Z-c0ffee000000", "/work/repo", "ship the widget", cfg)
	m.CurrentNode = "code"
	m.Step = 2
	m.Budget.Accumulate(belay.Usage{USD: 0.42, InputTokens: 1000, OutputTokens: 200})
	m.Touch(now.Add(time.Minute))

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')

	goldenPath := filepath.Join("testdata", "manifest.golden.json")
	if *update {
		if err := os.WriteFile(goldenPath, data, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(goldenPath) //nolint:gosec // fixed testdata path
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if diff := cmp.Diff(string(want), string(data)); diff != "" {
		t.Errorf("manifest.json shape differs from %s (-golden +got):\n%s", goldenPath, diff)
	}
}

func TestStore_StateRoundTrip(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	store := NewStore(layout)

	want := NewState("goal")
	if err := store.SaveState(want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
}

func TestStore_ManifestRoundTrip(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	store := NewStore(layout)

	want := NewManifest(time.Now(), "run-1", "/work", "goal", config.Default())
	if err := store.SaveManifest(want); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	got, err := store.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
}

func TestStore_ApplyPatch(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	store := NewStore(layout)
	if err := store.SaveState(NewState("goal")); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	goal := "new goal"
	got, err := store.ApplyPatch(Patch{Goal: &goal})
	if err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if got.Goal != goal {
		t.Errorf("returned State.Goal = %q, want %q", got.Goal, goal)
	}

	reloaded, err := store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if reloaded.Goal != goal {
		t.Errorf("persisted State.Goal = %q, want %q", reloaded.Goal, goal)
	}
}

func TestStore_ApplyPatch_RejectsInvalidPatch(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	store := NewStore(layout)
	if err := store.SaveState(NewState("goal")); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	_, err = store.ApplyPatch(Patch{History: &HistoryEntry{Seq: 0}})
	if !errors.Is(err, ErrInvalidPatch) {
		t.Errorf("error does not wrap ErrInvalidPatch: %v", err)
	}

	// A rejected patch must not have been written.
	reloaded, err := store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(reloaded.History) != 0 {
		t.Errorf("History = %+v, want empty after a rejected patch", reloaded.History)
	}
}

// TestStore_ApplyPatch_ConcurrentGoroutinesWithinOneProcess demonstrates
// the requirement that a *Store is safe for concurrent use by multiple
// goroutines within one process: many goroutines each append a distinct,
// uniquely-keyed HistoryEntry concurrently, and every one of them must
// land — none lost to an interleaved read-modify-write.
func TestStore_ApplyPatch_ConcurrentGoroutinesWithinOneProcess(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	store := NewStore(layout)
	if err := store.SaveState(NewState("goal")); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			entry := HistoryEntry{Seq: seq, Node: "node", Status: "ok"}
			_, err := store.ApplyPatch(Patch{History: &entry})
			errs[seq-1] = err
		}(uint64(i))
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("ApplyPatch %d: %v", i+1, err)
		}
	}

	final, err := store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(final.History) != n {
		t.Fatalf("len(History) = %d, want %d (no update should be lost)", len(final.History), n)
	}
	seen := make(map[uint64]bool, n)
	for _, e := range final.History {
		seen[e.Seq] = true
	}
	for i := 1; i <= n; i++ {
		if !seen[uint64(i)] {
			t.Errorf("history entry for seq %d is missing", i)
		}
	}
}
