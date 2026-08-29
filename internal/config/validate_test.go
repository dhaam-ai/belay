package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default() must be valid, got: %v", err)
	}
}

// enumMutator sets one closed-set field, so the enum test can reach every
// entry in enumFields without exporting a setter from production code.
type enumMutator struct {
	name string
	set  func(*Config, string)
}

var enumMutators = []enumMutator{
	{"agent.mode", func(c *Config, v string) { c.Agent.Mode = AgentMode(v) }},
	{"test.runner", func(c *Config, v string) { c.Test.Runner = TestRunner(v) }},
	{"review.mode", func(c *Config, v string) { c.Review.Mode = ReviewMode(v) }},
	{"review.fail_on", func(c *Config, v string) { c.Review.FailOn = Severity(v) }},
	{"review.ai.mcp.mode", func(c *Config, v string) { c.Review.AI.MCP.Mode = MCPMode(v) }},
	{"budget.on_exceed", func(c *Config, v string) { c.Budget.OnExceed = OnExceed(v) }},
	{"fanout.select", func(c *Config, v string) { c.Fanout.Select = SelectStrategy(v) }},
}

// TestEveryEnumIsCovered fails if a closed-set field is added to the schema
// without a mutator here, which would let TestEnumRejectsUnknownValue skip it.
func TestEveryEnumIsCovered(t *testing.T) {
	declared := make([]string, 0, len(enumFields))
	for _, field := range enumFields {
		declared = append(declared, field.name)
	}
	covered := make([]string, 0, len(enumMutators))
	for _, mutator := range enumMutators {
		covered = append(covered, mutator.name)
	}
	slices.Sort(declared)
	slices.Sort(covered)
	if !slices.Equal(declared, covered) {
		t.Fatalf("enum coverage mismatch:\n  validated: %v\n  tested:    %v", declared, covered)
	}
}

func TestEnumRejectsUnknownValue(t *testing.T) {
	for _, mutator := range enumMutators {
		field, ok := findEnumField(mutator.name)
		if !ok {
			t.Fatalf("%s is not a validated enum", mutator.name)
		}
		for _, bad := range []string{"nonsense", "", "LINT", field.allowed[0] + "x"} {
			if slices.Contains(field.allowed, bad) {
				continue
			}
			t.Run(fmt.Sprintf("%s=%q", mutator.name, bad), func(t *testing.T) {
				cfg := Default()
				mutator.set(&cfg, bad)
				err := cfg.Validate()
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("error = %v, want one wrapping ErrInvalid", err)
				}
				var fieldErr *FieldError
				if !errors.As(err, &fieldErr) {
					t.Fatalf("error %v is not a *FieldError", err)
				}
				want := fmt.Sprintf("%s: %q is not valid (want one of: %s)",
					mutator.name, bad, strings.Join(field.allowed, ", "))
				if got := err.Error(); !strings.Contains(got, want) {
					t.Errorf("message:\n  got  %s\n  want it to contain %s", got, want)
				}
			})
		}
	}
}

func TestEnumAcceptsEveryDeclaredValue(t *testing.T) {
	for _, mutator := range enumMutators {
		field, _ := findEnumField(mutator.name)
		for _, good := range field.allowed {
			t.Run(fmt.Sprintf("%s=%s", mutator.name, good), func(t *testing.T) {
				cfg := Default()
				mutator.set(&cfg, good)
				// Some values make another field mandatory; supply it so this
				// test only measures whether the enum value itself is accepted.
				cfg.Test.CustomCmd = "make check"
				cfg.Review.AI.MCP.URL = "https://mcp.example.test"
				if err := cfg.Validate(); err != nil {
					t.Errorf("%s = %q must be accepted, got: %v", mutator.name, good, err)
				}
			})
		}
	}
}

func findEnumField(name string) (enumField, bool) {
	for _, field := range enumFields {
		if field.name == name {
			return field, true
		}
	}
	return enumField{}, false
}

func TestValidateRules(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		want    string
		wantErr bool
	}{
		{
			name:   "version must be 1",
			mutate: func(c *Config) { c.Version = 3 },
			want:   "version: 3 is not supported (want one of: 1)", wantErr: true,
		},
		{
			name:   "version zero is rejected",
			mutate: func(c *Config) { c.Version = 0 },
			want:   "version: 0 is not supported", wantErr: true,
		},
		{
			name:   "empty backend",
			mutate: func(c *Config) { c.Agent.Backend = "" },
			want:   `agent.backend: "" is not valid (want the name of an agent CLI backend)`, wantErr: true,
		},
		{
			name:   "empty model",
			mutate: func(c *Config) { c.Agent.Model = "" },
			want:   `agent.model: "" is not valid (want a model name)`, wantErr: true,
		},
		{
			name:   "zero max turns",
			mutate: func(c *Config) { c.Agent.MaxTurns = 0 },
			want:   "agent.max_turns: 0 is not valid (want at least 1)", wantErr: true,
		},
		{
			name:   "give_up below one",
			mutate: func(c *Config) { c.Graph.GiveUp = 0 },
			want:   "graph.give_up: 0 is not valid (want at least 1)", wantErr: true,
		},
		{
			name:   "negative give_up",
			mutate: func(c *Config) { c.Graph.GiveUp = -1 },
			want:   "graph.give_up: -1 is not valid (want at least 1)", wantErr: true,
		},
		{
			name:   "max_steps below one",
			mutate: func(c *Config) { c.Graph.MaxSteps = 0 },
			want:   "graph.max_steps: 0 is not valid (want at least 1)", wantErr: true,
		},
		{
			name:   "unparsable node_timeout",
			mutate: func(c *Config) { c.Graph.NodeTimeout = Duration{bad: "15 minutes"} },
			want:   `graph.node_timeout: "15 minutes" is not valid (want a Go duration such as 90s, 15m or 1h30m)`, wantErr: true,
		},
		{
			name:   "zero node_timeout",
			mutate: func(c *Config) { c.Graph.NodeTimeout = NewDuration(0) },
			want:   `graph.node_timeout: "0s" is not valid (want a duration greater than zero)`, wantErr: true,
		},
		{
			name:   "negative node_timeout",
			mutate: func(c *Config) { c.Graph.NodeTimeout = NewDuration(-1) },
			want:   "want a duration greater than zero", wantErr: true,
		},
		{
			name:   "custom runner without a command",
			mutate: func(c *Config) { c.Test.Runner = TestRunnerCustom },
			want:   `test.custom_cmd: "" is not valid (want a command to run, because test.runner is "custom")`, wantErr: true,
		},
		{
			name: "custom runner with a command",
			mutate: func(c *Config) {
				c.Test.Runner = TestRunnerCustom
				c.Test.CustomCmd = "make test"
			},
		},
		{
			name:   "empty sonar runner",
			mutate: func(c *Config) { c.Review.Sonar.Runner = "" },
			want:   `review.sonar.runner: "" is not valid (want the name of a scanner runner)`, wantErr: true,
		},
		{
			name:   "quality gate timeout below one second",
			mutate: func(c *Config) { c.Review.Sonar.QualityGateTimeout = 0 },
			want:   "review.sonar.quality_gate_timeout: 0 is not valid (want at least 1 second)", wantErr: true,
		},
		{
			name: "sonar review without a scanner image",
			mutate: func(c *Config) {
				c.Review.Mode = ReviewModeSonar
				c.Review.Sonar.Image = ""
			},
			want: `review.sonar.image: "" is not valid (want a scanner image, because review.sonar.runner is "docker")`, wantErr: true,
		},
		{
			name: "an unused empty scanner image is not an error",
			mutate: func(c *Config) {
				c.Review.Mode = ReviewModeLint
				c.Review.Sonar.Image = ""
			},
		},
		{
			name: "ai review against a server without a url",
			mutate: func(c *Config) {
				c.Review.Mode = ReviewModeAI
				c.Review.AI.MCP.Mode = MCPModeServer
			},
			want: `review.ai.mcp.url: "" is not valid (want the URL of a running MCP server, because review.ai.mcp.mode is "server")`, wantErr: true,
		},
		{
			name: "ai review against a server with a url",
			mutate: func(c *Config) {
				c.Review.Mode = ReviewModeAI
				c.Review.AI.MCP.Mode = MCPModeServer
				c.Review.AI.MCP.URL = "https://mcp.example.test"
			},
		},
		{
			name: "a server mcp mode without a url is fine while review.mode is lint",
			mutate: func(c *Config) {
				c.Review.Mode = ReviewModeLint
				c.Review.AI.MCP.Mode = MCPModeServer
			},
		},
		{
			name: "ai review in docker without an image",
			mutate: func(c *Config) {
				c.Review.Mode = ReviewModeAI
				c.Review.AI.MCP.Mode = MCPModeDocker
				c.Review.AI.MCP.Image = ""
			},
			want: `review.ai.mcp.image: "" is not valid (want an MCP server image, because review.ai.mcp.mode is "docker")`, wantErr: true,
		},
		{
			name:   "zero budget while aborting",
			mutate: func(c *Config) { c.Budget.MaxUSD = 0 },
			want:   `budget.max_usd: 0 is not valid (want more than zero, because budget.on_exceed is "abort")`, wantErr: true,
		},
		{
			name: "zero budget while only warning",
			mutate: func(c *Config) {
				c.Budget.MaxUSD = 0
				c.Budget.OnExceed = OnExceedWarn
			},
		},
		{
			name:   "negative budget",
			mutate: func(c *Config) { c.Budget.MaxUSD = -1.5 },
			want:   "budget.max_usd: -1.5 is not valid (want zero or more)", wantErr: true,
		},
		{
			name: "negative budget while only warning is still rejected",
			mutate: func(c *Config) {
				c.Budget.MaxUSD = -1.5
				c.Budget.OnExceed = OnExceedWarn
			},
			want: "budget.max_usd: -1.5 is not valid (want zero or more)", wantErr: true,
		},
		{
			name:   "negative token ceiling",
			mutate: func(c *Config) { c.Budget.MaxTokens = -1 },
			want:   "budget.max_tokens: -1 is not valid (want zero or more)", wantErr: true,
		},
		{
			name:   "empty isolator",
			mutate: func(c *Config) { c.Fanout.Isolator = "" },
			want:   `fanout.isolator: "" is not valid (want the name of a workspace isolator)`, wantErr: true,
		},
		{
			name: "fanout with too few candidates",
			mutate: func(c *Config) {
				c.Fanout.Enabled = true
				c.Fanout.Candidates = 1
			},
			want: "fanout.candidates: 1 is not valid (want at least 2, because fanout.enabled is true)", wantErr: true,
		},
		{
			name: "one candidate is fine while fanout is off",
			mutate: func(c *Config) {
				c.Fanout.Enabled = false
				c.Fanout.Candidates = 1
			},
		},
		{
			name: "fanout with enough candidates",
			mutate: func(c *Config) {
				c.Fanout.Enabled = true
				c.Fanout.Candidates = 2
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("want no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error containing %q, got nil", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("message:\n  got  %s\n  want it to contain %s", err, tt.want)
			}
		})
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	cfg := Default()
	cfg.Version = 9
	cfg.Agent.Backend = ""
	cfg.Agent.MaxTurns = 0
	cfg.Graph.GiveUp = 0
	cfg.Graph.MaxSteps = -4
	cfg.Graph.NodeTimeout = Duration{bad: "soon"}
	cfg.Test.Runner = TestRunnerCustom
	cfg.Review.Mode = "sonarqube"
	cfg.Budget.MaxUSD = 0
	cfg.Budget.OnExceed = "explode"
	cfg.Fanout.Enabled = true
	cfg.Fanout.Candidates = 1
	cfg.Fanout.Select = "cheapest"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	want := []string{
		"version: 9 is not supported (want one of: 1)",
		`review.mode: "sonarqube" is not valid (want one of: lint, sonar, ai)`,
		`budget.on_exceed: "explode" is not valid (want one of: abort, warn)`,
		`fanout.select: "cheapest" is not valid (want one of: fewest_issues, first_pass, fastest)`,
		`agent.backend: "" is not valid`,
		"agent.max_turns: 0 is not valid",
		"graph.give_up: 0 is not valid",
		"graph.max_steps: -4 is not valid",
		`graph.node_timeout: "soon" is not valid`,
		`test.custom_cmd: "" is not valid`,
		"budget.max_usd: 0 is not valid",
		"fanout.candidates: 1 is not valid",
	}
	got := err.Error()
	for _, line := range want {
		if !strings.Contains(got, line) {
			t.Errorf("missing %q from:\n%s", line, got)
		}
	}
	if lines := strings.Count(got, "\n") + 1; lines != len(want) {
		t.Errorf("reported %d problems, want %d:\n%s", lines, len(want), got)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Error("joined error does not wrap ErrInvalid")
	}
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Error("joined error does not wrap ErrUnsupportedVersion")
	}
}

func TestValidateReportsOneErrorPerField(t *testing.T) {
	// A negative budget with on_exceed=abort breaks two rules on one field;
	// the user should be told once.
	cfg := Default()
	cfg.Budget.MaxUSD = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if count := strings.Count(err.Error(), "budget.max_usd"); count != 1 {
		t.Errorf("budget.max_usd reported %d times, want 1:\n%s", count, err)
	}
}

func TestLoadValidatesTheMergedConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ProjectFile, "review:\n  mode: sonarqube\ngraph:\n  give_up: 0\n")
	cfg, err := Load(Options{Dir: dir, XDGConfigHome: t.TempDir()})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want one wrapping ErrInvalid", err)
	}
	for _, want := range []string{
		`review.mode: "sonarqube" is not valid (want one of: lint, sonar, ai)`,
		"graph.give_up: 0 is not valid (want at least 1)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q from:\n%s", want, err)
		}
	}
	if cfg.Review.Mode != "sonarqube" {
		t.Errorf("Load must return what it read: review.mode = %q", cfg.Review.Mode)
	}
}
