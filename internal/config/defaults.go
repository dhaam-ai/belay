package config

import "time"

// Built-in defaults. These are the values a run uses when no configuration
// file exists, and the base every file is merged onto field by field.
const (
	defaultBackend            = "claude-code"
	defaultModel              = "sonnet"
	defaultMaxTurns           = 30
	defaultGiveUp             = 3
	defaultMaxSteps           = 60
	defaultNodeTimeout        = 15 * time.Minute
	defaultApproval           = true
	defaultSonarRunner        = "docker"
	defaultSonarImage         = "sonarsource/sonar-scanner-cli"
	defaultQualityGateWait    = true
	defaultQualityGateTimeout = 300
	defaultMCPImage           = "sonarsource/sonar-mcp-server"
	defaultMaxUSD             = 5.00
	defaultMaxTokens          = 2_000_000
	defaultCandidates         = 3
	defaultIsolator           = "dircopy"
)

// Default returns belay's built-in configuration.
//
// It is the base layer of every load and is always valid: Default().Validate()
// returns nil.
func Default() Config {
	return Config{
		Version: Version,
		Agent: Agent{
			Backend:  defaultBackend,
			Model:    defaultModel,
			MaxTurns: defaultMaxTurns,
			Mode:     AgentModeLive,
		},
		Graph: Graph{
			GiveUp:      defaultGiveUp,
			MaxSteps:    defaultMaxSteps,
			NodeTimeout: NewDuration(defaultNodeTimeout),
			Approval:    defaultApproval,
		},
		Test: Test{
			Runner:    TestRunnerAuto,
			CustomCmd: "",
		},
		Review: Review{
			Mode:   ReviewModeLint,
			FailOn: SeverityMajor,
			Sonar: Sonar{
				Runner:             defaultSonarRunner,
				Image:              defaultSonarImage,
				QualityGateWait:    defaultQualityGateWait,
				QualityGateTimeout: defaultQualityGateTimeout,
			},
			AI: AI{
				MCP: MCP{
					Mode:  MCPModeCloud,
					URL:   "",
					Image: defaultMCPImage,
				},
			},
		},
		Budget: Budget{
			MaxUSD:    defaultMaxUSD,
			MaxTokens: defaultMaxTokens,
			OnExceed:  OnExceedAbort,
		},
		Fanout: Fanout{
			Enabled:    false,
			Candidates: defaultCandidates,
			Isolator:   defaultIsolator,
			Select:     SelectFewestIssues,
		},
	}
}
