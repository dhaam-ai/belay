package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenPath is the canonical rendering of Default(). It is checked in so a
// change to any default shows up as a reviewable diff.
const goldenPath = "testdata/defaults.golden.yaml"

func TestDefaultMatchesGolden(t *testing.T) {
	got, err := yaml.Marshal(Default())
	if err != nil {
		t.Fatalf("marshal defaults: %v", err)
	}
	if *update {
		if err := os.WriteFile(filepath.FromSlash(goldenPath), got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(filepath.FromSlash(goldenPath))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if diff := cmp.Diff(string(want), string(got)); diff != "" {
		t.Errorf("Default() rendering differs from %s (-golden +got):\n%s", goldenPath, diff)
	}
}

func TestDefaultFields(t *testing.T) {
	c := Default()
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"version", c.Version, 1},
		{"agent.backend", c.Agent.Backend, "claude-code"},
		{"agent.model", c.Agent.Model, "sonnet"},
		{"agent.max_turns", c.Agent.MaxTurns, 30},
		{"agent.mode", c.Agent.Mode, AgentModeLive},
		{"graph.give_up", c.Graph.GiveUp, 3},
		{"graph.max_steps", c.Graph.MaxSteps, 60},
		{"graph.node_timeout", c.Graph.NodeTimeout.String(), "15m0s"},
		{"graph.approval", c.Graph.Approval, true},
		{"test.runner", c.Test.Runner, TestRunnerAuto},
		{"test.custom_cmd", c.Test.CustomCmd, ""},
		{"review.mode", c.Review.Mode, ReviewModeLint},
		{"review.fail_on", c.Review.FailOn, SeverityMajor},
		{"review.sonar.runner", c.Review.Sonar.Runner, "docker"},
		{"review.sonar.image", c.Review.Sonar.Image, "sonarsource/sonar-scanner-cli"},
		{"review.sonar.quality_gate_wait", c.Review.Sonar.QualityGateWait, true},
		{"review.sonar.quality_gate_timeout", c.Review.Sonar.QualityGateTimeout, 300},
		{"review.ai.mcp.mode", c.Review.AI.MCP.Mode, MCPModeCloud},
		{"review.ai.mcp.url", c.Review.AI.MCP.URL, ""},
		{"review.ai.mcp.image", c.Review.AI.MCP.Image, "sonarsource/sonar-mcp-server"},
		{"budget.max_usd", c.Budget.MaxUSD, 5.00},
		{"budget.max_tokens", c.Budget.MaxTokens, int64(2_000_000)},
		{"budget.on_exceed", c.Budget.OnExceed, OnExceedAbort},
		{"fanout.enabled", c.Fanout.Enabled, false},
		{"fanout.candidates", c.Fanout.Candidates, 3},
		{"fanout.isolator", c.Fanout.Isolator, "dircopy"},
		{"fanout.select", c.Fanout.Select, SelectFewestIssues},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, tt.got); diff != "" {
				t.Errorf("%s (-want +got):\n%s", tt.name, diff)
			}
		})
	}
}

func TestDurationRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    string
		valid   bool
		wantDur string
	}{
		{name: "minutes", yaml: "15m", want: "15m0s", valid: true},
		{name: "compound", yaml: "1h30m", want: "1h30m0s", valid: true},
		{name: "seconds normalise to the same canonical form", yaml: "900s", want: "15m0s", valid: true},
		{name: "bare number is not a duration", yaml: "900", want: "900", valid: false},
		{name: "prose is not a duration", yaml: "\"15 minutes\"", want: "15 minutes", valid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Duration
			if err := yaml.Unmarshal([]byte(tt.yaml), &got); err != nil {
				t.Fatalf("unmarshal %q: %v", tt.yaml, err)
			}
			if got.Valid() != tt.valid {
				t.Errorf("Valid() = %v, want %v", got.Valid(), tt.valid)
			}
			if got.String() != tt.want {
				t.Errorf("String() = %q, want %q", got.String(), tt.want)
			}
			out, err := yaml.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back Duration
			if err := yaml.Unmarshal(out, &back); err != nil {
				t.Fatalf("unmarshal round trip: %v", err)
			}
			if !back.Equal(got) {
				t.Errorf("round trip = %#v, want %#v", back, got)
			}
		})
	}
}

func TestDurationNullKeepsExistingValue(t *testing.T) {
	got := NewDuration(defaultNodeTimeout)
	if err := yaml.Unmarshal([]byte("~"), &got); err != nil {
		t.Fatalf("unmarshal null: %v", err)
	}
	if want := NewDuration(defaultNodeTimeout); !got.Equal(want) {
		t.Errorf("null overwrote the existing value: got %s, want %s", got, want)
	}
}

func TestDurationRejectsNonScalar(t *testing.T) {
	var got Duration
	err := yaml.Unmarshal([]byte("[1, 2]"), &got)
	if err == nil {
		t.Fatal("want an error for a list, got nil")
	}
	if !strings.Contains(err.Error(), "duration must be a string") {
		t.Errorf("error %q does not explain the expected form", err)
	}
}
