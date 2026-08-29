package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v3"
)

const snapshotGoldenPath = "testdata/snapshot.golden.txt"

func TestSnapshotMatchesGolden(t *testing.T) {
	got := Default().Snapshot().Canonical()
	if *update {
		if err := os.WriteFile(filepath.FromSlash(snapshotGoldenPath), []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(filepath.FromSlash(snapshotGoldenPath))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if diff := cmp.Diff(string(want), got); diff != "" {
		t.Errorf("snapshot differs from %s (-golden +got):\n%s", snapshotGoldenPath, diff)
	}
}

// TestSnapshotCoversEveryField flattens the YAML rendering of a Config and
// compares its leaves against the snapshot's keys, so a field added to the
// schema cannot silently drop out of the manifest or the digest.
func TestSnapshotCoversEveryField(t *testing.T) {
	encoded, err := yaml.Marshal(Default())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(encoded, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	inYAML := map[string]bool{}
	var walk func(n *yaml.Node, prefix string)
	walk = func(n *yaml.Node, prefix string) {
		switch n.Kind {
		case yaml.DocumentNode:
			walk(n.Content[0], prefix)
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				path := n.Content[i].Value
				if prefix != "" {
					path = prefix + "." + path
				}
				walk(n.Content[i+1], path)
			}
		default:
			inYAML[prefix] = true
		}
	}
	walk(&doc, "")

	snapshot := Default().Snapshot()
	for path := range inYAML {
		if _, ok := snapshot[path]; !ok {
			t.Errorf("%s is in the schema but missing from Snapshot", path)
		}
	}
	for path := range snapshot {
		if !inYAML[path] {
			t.Errorf("%s is in Snapshot but not in the schema", path)
		}
	}
}

func TestDigestIsStableAcrossRepeatedCalls(t *testing.T) {
	cfg := Default()
	want := cfg.Digest()
	for i := range 100 {
		if got := cfg.Digest(); got != want {
			t.Fatalf("call %d returned %s, want %s", i, got, want)
		}
	}
	if !strings.HasPrefix(want, "sha256:") {
		t.Errorf("digest %q is not labelled with its algorithm", want)
	}
	if len(want) != len("sha256:")+64 {
		t.Errorf("digest %q is not a 32-byte hex sum", want)
	}
}

func TestDigestIsStableAcrossGoroutines(t *testing.T) {
	cfg := Default()
	want := cfg.Digest()
	digests := make([]string, 8)
	var wg sync.WaitGroup
	for i := range digests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			digests[i] = cfg.Digest()
		}()
	}
	wg.Wait()
	for i, got := range digests {
		if got != want {
			t.Errorf("goroutine %d returned %s, want %s", i, got, want)
		}
	}
}

func TestDigestChangesWhenAnyFieldChanges(t *testing.T) {
	tests := []struct {
		field  string
		mutate func(*Config)
	}{
		{"version", func(c *Config) { c.Version = 2 }},
		{"agent.backend", func(c *Config) { c.Agent.Backend = "codex" }},
		{"agent.model", func(c *Config) { c.Agent.Model = "opus" }},
		{"agent.max_turns", func(c *Config) { c.Agent.MaxTurns = 31 }},
		{"agent.mode", func(c *Config) { c.Agent.Mode = AgentModeReplay }},
		{"graph.give_up", func(c *Config) { c.Graph.GiveUp = 4 }},
		{"graph.max_steps", func(c *Config) { c.Graph.MaxSteps = 61 }},
		{"graph.node_timeout", func(c *Config) { c.Graph.NodeTimeout = NewDuration(defaultNodeTimeout + 1) }},
		{"graph.approval", func(c *Config) { c.Graph.Approval = false }},
		{"test.runner", func(c *Config) { c.Test.Runner = TestRunnerGo }},
		{"test.custom_cmd", func(c *Config) { c.Test.CustomCmd = "make check" }},
		{"review.mode", func(c *Config) { c.Review.Mode = ReviewModeSonar }},
		{"review.fail_on", func(c *Config) { c.Review.FailOn = SeverityMinor }},
		{"review.sonar.runner", func(c *Config) { c.Review.Sonar.Runner = "local" }},
		{"review.sonar.image", func(c *Config) { c.Review.Sonar.Image = "other/image" }},
		{"review.sonar.quality_gate_wait", func(c *Config) { c.Review.Sonar.QualityGateWait = false }},
		{"review.sonar.quality_gate_timeout", func(c *Config) { c.Review.Sonar.QualityGateTimeout = 301 }},
		{"review.ai.mcp.mode", func(c *Config) { c.Review.AI.MCP.Mode = MCPModeDocker }},
		{"review.ai.mcp.url", func(c *Config) { c.Review.AI.MCP.URL = "https://mcp.example.test" }},
		{"review.ai.mcp.image", func(c *Config) { c.Review.AI.MCP.Image = "other/mcp" }},
		{"budget.max_usd", func(c *Config) { c.Budget.MaxUSD = 5.01 }},
		{"budget.max_tokens", func(c *Config) { c.Budget.MaxTokens = 2_000_001 }},
		{"budget.on_exceed", func(c *Config) { c.Budget.OnExceed = OnExceedWarn }},
		{"fanout.enabled", func(c *Config) { c.Fanout.Enabled = true }},
		{"fanout.candidates", func(c *Config) { c.Fanout.Candidates = 4 }},
		{"fanout.isolator", func(c *Config) { c.Fanout.Isolator = "worktree" }},
		{"fanout.select", func(c *Config) { c.Fanout.Select = SelectFastest }},
	}

	base := Default().Digest()
	seen := map[string]string{base: "defaults"}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			got := cfg.Digest()
			if got == base {
				t.Fatalf("changing %s did not change the digest", tt.field)
			}
			if other, clash := seen[got]; clash {
				t.Fatalf("changing %s collides with changing %s", tt.field, other)
			}
			seen[got] = tt.field
		})
	}
	if len(seen) != len(tests)+1 {
		t.Errorf("covered %d fields, want %d", len(seen)-1, len(tests))
	}
}

func TestDigestIgnoresHowADurationWasWritten(t *testing.T) {
	// "900s" and "15m" drive an identical run, so a resume must not be
	// treated as drift because the file was rewritten more legibly.
	fifteenMinutes := Default()
	nineHundredSeconds := Default()
	if err := nineHundredSeconds.Merge([]byte("graph:\n  node_timeout: 900s\n"), "belay.yaml"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got, want := nineHundredSeconds.Digest(), fifteenMinutes.Digest(); got != want {
		t.Errorf("digest %s != %s for equivalent durations", got, want)
	}
}

func TestDigestSurvivesAFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ProjectFile, "budget:\n  max_usd: 9.25\nfanout:\n  enabled: true\n  candidates: 4\n")
	first, err := Load(Options{Dir: dir, XDGConfigHome: t.TempDir()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	encoded, err := yaml.Marshal(first)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second := Default()
	if err := second.Merge(encoded, "manifest.yaml"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got, want := second.Digest(), first.Digest(); got != want {
		t.Errorf("digest changed across a round trip: %s != %s", got, want)
	}
}

func TestCanonicalQuotesValues(t *testing.T) {
	// An unquoted newline inside a value could otherwise be read as the
	// boundary between two settings and let two configs share a digest.
	withNewline := Default()
	withNewline.Test.CustomCmd = "make check\nfanout.candidates=99"
	if strings.Contains(withNewline.Snapshot().Canonical(), "\nfanout.candidates=99") {
		t.Error("a newline inside a value leaks into the canonical form")
	}
	if withNewline.Digest() == Default().Digest() {
		t.Error("a value containing a newline collides with the defaults")
	}
}
