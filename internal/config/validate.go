package config

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// enumField describes one field whose value must come from a closed set.
//
// Every closed-set field in the schema appears here exactly once, so the
// checks, the error messages, and the tests all read the same list.
type enumField struct {
	// name is the field's dotted path.
	name string
	// allowed lists every acceptable value, in the order they are offered
	// to the user.
	allowed []string
	// get reads the field's current value.
	get func(*Config) string
}

// enumFields lists the closed-set fields in the order they appear in the
// schema, so errors are reported in the order the file is written.
var enumFields = []enumField{
	{
		name:    "agent.mode",
		allowed: []string{string(AgentModeLive), string(AgentModeRecord), string(AgentModeReplay)},
		get:     func(c *Config) string { return string(c.Agent.Mode) },
	},
	{
		name: "test.runner",
		allowed: []string{
			string(TestRunnerAuto), string(TestRunnerGo), string(TestRunnerNode),
			string(TestRunnerPython), string(TestRunnerCustom),
		},
		get: func(c *Config) string { return string(c.Test.Runner) },
	},
	{
		name:    "review.mode",
		allowed: []string{string(ReviewModeLint), string(ReviewModeSonar), string(ReviewModeAI)},
		get:     func(c *Config) string { return string(c.Review.Mode) },
	},
	{
		name: "review.fail_on",
		allowed: []string{
			string(SeverityBlocker), string(SeverityCritical), string(SeverityMajor),
			string(SeverityMinor), string(SeverityInfo),
		},
		get: func(c *Config) string { return string(c.Review.FailOn) },
	},
	{
		name:    "review.ai.mcp.mode",
		allowed: []string{string(MCPModeCloud), string(MCPModeDocker), string(MCPModeServer)},
		get:     func(c *Config) string { return string(c.Review.AI.MCP.Mode) },
	},
	{
		name:    "budget.on_exceed",
		allowed: []string{string(OnExceedAbort), string(OnExceedWarn)},
		get:     func(c *Config) string { return string(c.Budget.OnExceed) },
	},
	{
		name:    "fanout.isolator",
		allowed: []string{string(IsolatorAuto), string(IsolatorWorktree), string(IsolatorDircopy)},
		get:     func(c *Config) string { return string(c.Fanout.Isolator) },
	},
	{
		name:    "fanout.select",
		allowed: []string{string(SelectFewestIssues), string(SelectFirstPass), string(SelectFastest)},
		get:     func(c *Config) string { return string(c.Fanout.Select) },
	},
	{
		name:    "output.mode",
		allowed: []string{string(OutputAuto), string(OutputBranch), string(OutputInPlace)},
		get:     func(c *Config) string { return string(c.Output.Mode) },
	},
}

// Validate reports every problem with c at once.
//
// Configuration mistakes cluster: a file written against the wrong schema
// usually has several. Validate therefore collects every failed rule and
// joins them with [errors.Join] rather than returning at the first one, so a
// single run tells the user everything they have to fix. Each joined error
// wraps a sentinel, so errors.Is still answers questions about the whole set
// and errors.As can reach an individual [FieldError].
//
// Fields are checked whether or not the mode that consumes them is active: a
// typo in the Sonar settings is worth reporting before the day someone
// switches review.mode to sonar. Rules that would demand a value the user has
// no reason to supply are the exception, and each names the setting that made
// the value necessary.
func (c Config) Validate() error {
	var errs []error
	if c.Version != Version {
		errs = append(errs, &VersionError{Got: c.Version})
	}
	for _, field := range enumFields {
		if value := field.get(&c); !slices.Contains(field.allowed, value) {
			errs = append(errs, &FieldError{
				Field:   field.name,
				Value:   strconv.Quote(value),
				Allowed: field.allowed,
			})
		}
	}
	errs = append(errs, c.agentErrors()...)
	errs = append(errs, c.modelErrors()...)
	errs = append(errs, c.graphErrors()...)
	errs = append(errs, c.testErrors()...)
	errs = append(errs, c.reviewErrors()...)
	errs = append(errs, c.budgetErrors()...)
	errs = append(errs, c.fanoutErrors()...)
	errs = append(errs, c.planErrors()...)
	return errors.Join(errs...)
}

func (c Config) agentErrors() []error {
	var errs []error
	if c.Agent.Backend == "" {
		errs = append(errs, textError("agent.backend", c.Agent.Backend, "want the name of an agent CLI backend"))
	}
	if c.Agent.Model == "" {
		errs = append(errs, textError("agent.model", c.Agent.Model, "want a model name"))
	}
	if c.Agent.MaxTurns < 1 {
		errs = append(errs, intError("agent.max_turns", c.Agent.MaxTurns, "want at least 1"))
	}
	return errs
}

func (c Config) graphErrors() []error {
	var errs []error
	if c.Graph.GiveUp < 1 {
		errs = append(errs, intError("graph.give_up", c.Graph.GiveUp, "want at least 1"))
	}
	if c.Graph.MaxSteps < 1 {
		errs = append(errs, intError("graph.max_steps", c.Graph.MaxSteps, "want at least 1"))
	}
	switch timeout := c.Graph.NodeTimeout; {
	case !timeout.Valid():
		errs = append(errs, textError("graph.node_timeout", timeout.String(),
			"want a Go duration such as 90s, 15m or 1h30m"))
	case timeout.Duration() <= 0:
		errs = append(errs, textError("graph.node_timeout", timeout.String(),
			"want a duration greater than zero"))
	}
	return errs
}

func (c Config) testErrors() []error {
	if c.Test.Runner == TestRunnerCustom && c.Test.CustomCmd == "" {
		return []error{textError("test.custom_cmd", c.Test.CustomCmd,
			`want a command to run, because test.runner is "custom"`)}
	}
	return nil
}

func (c Config) reviewErrors() []error {
	var errs []error
	if c.Review.Sonar.Runner == "" {
		errs = append(errs, textError("review.sonar.runner", c.Review.Sonar.Runner,
			"want the name of a scanner runner"))
	}
	if c.Review.Sonar.QualityGateTimeout < 1 {
		errs = append(errs, intError("review.sonar.quality_gate_timeout", c.Review.Sonar.QualityGateTimeout,
			"want at least 1 second"))
	}
	if c.Review.Mode == ReviewModeSonar && c.Review.Sonar.Runner == "docker" && c.Review.Sonar.Image == "" {
		errs = append(errs, textError("review.sonar.image", c.Review.Sonar.Image,
			`want a scanner image, because review.sonar.runner is "docker"`))
	}
	// The mcp rules apply only while review.mode is "ai", the one mode
	// that dials the server. The defaults pair review.mode "lint" with
	// review.ai.mcp.mode "cloud" and no url, and must stay valid.
	//
	// "cloud" needs the url as much as "server" does: the reviewer has no
	// built-in endpoint for it (aireview.ErrMissingURL argues why). No run
	// reaches that reviewer today, because belay run refuses review.mode
	// "ai" before it starts: the reviewer is not wired in. The rule is
	// here for the day it is, so that a missing url fails when the
	// configuration loads rather than at the review step, after the run
	// has spent on everything before it.
	if c.Review.Mode == ReviewModeAI {
		switch c.Review.AI.MCP.Mode {
		case MCPModeServer:
			if c.Review.AI.MCP.URL == "" {
				errs = append(errs, textError("review.ai.mcp.url", c.Review.AI.MCP.URL,
					`want the URL of a running MCP server, because review.ai.mcp.mode is "server"`))
			}
		case MCPModeCloud:
			if c.Review.AI.MCP.URL == "" {
				errs = append(errs, textError("review.ai.mcp.url", c.Review.AI.MCP.URL,
					`want the URL of SonarQube Cloud's MCP endpoint, because review.ai.mcp.mode is "cloud"`))
			}
		case MCPModeDocker:
			if c.Review.AI.MCP.Image == "" {
				errs = append(errs, textError("review.ai.mcp.image", c.Review.AI.MCP.Image,
					`want an MCP server image, because review.ai.mcp.mode is "docker"`))
			}
		}
	}
	return errs
}

func (c Config) budgetErrors() []error {
	var errs []error
	switch {
	case c.Budget.MaxUSD < 0:
		errs = append(errs, floatError("budget.max_usd", c.Budget.MaxUSD, "want zero or more"))
	case c.Budget.MaxUSD == 0 && c.Budget.OnExceed != OnExceedWarn:
		// Only "warn" gives a zero ceiling a meaning. Any other value,
		// including one that is itself invalid, makes zero a run that
		// aborts before it starts; reporting both problems together saves
		// the user a second round trip.
		errs = append(errs, floatError("budget.max_usd", c.Budget.MaxUSD,
			fmt.Sprintf("want more than zero, because budget.on_exceed is %q", c.Budget.OnExceed)))
	}
	if c.Budget.MaxTokens < 0 {
		errs = append(errs, &FieldError{
			Field: "budget.max_tokens",
			Value: strconv.FormatInt(c.Budget.MaxTokens, 10),
			Want:  "want zero or more",
		})
	}
	return errs
}

func (c Config) fanoutErrors() []error {
	var errs []error
	if c.Fanout.Enabled && c.Fanout.Candidates < 2 {
		errs = append(errs, intError("fanout.candidates", c.Fanout.Candidates,
			"want at least 2, because fanout.enabled is true"))
	}
	return errs
}

// planErrors checks plan.archive_dir, whether or not plan.archive is on.
//
// The value is a slash-separated path relative to the workspace, and the
// rule is that it names a directory inside the workspace that neither belay
// nor git keeps for itself. An absolute path would ignore the workspace, and
// any ".." segment could climb out of it; the second is refused outright
// rather than cleaned and judged, because a rule a person can check by eye
// ("no ..") is worth more than accepting docs/../plans. Under .belay the plan
// would be deleted with the run directory it was moved out of, and under
// .git it would be in git's own files rather than in the repository. The
// first segment is compared without regard to case, because on the file
// systems macOS uses by default .Belay is the same directory as .belay.
//
// What this cannot see is the disk: a docs directory that is a symbolic link
// out of the workspace passes here and is refused when the plan is moved
// (internal/planarchive), which is the only moment the link can be read.
func (c Config) planErrors() []error {
	dir := c.Plan.ArchiveDir
	cleaned := path.Clean(dir)
	first, _, _ := strings.Cut(cleaned, "/")
	var want string
	switch {
	case strings.ContainsRune(dir, 0):
		want = "want a path with no NUL byte in it"
	case path.IsAbs(dir):
		want = "want a directory relative to the workspace, not an absolute path"
	case slices.Contains(strings.Split(dir, "/"), ".."):
		want = `want a path with no ".." in it, so the directory stays inside the workspace`
	case cleaned == ".":
		want = "want a directory inside the workspace, such as docs/plans"
	case strings.EqualFold(first, ".belay") || strings.EqualFold(first, ".git"):
		want = "want a directory not under .belay or .git, which belay and git keep for themselves"
	default:
		return nil
	}
	return []error{textError("plan.archive_dir", dir, want)}
}

func textError(field, value, want string) error {
	return &FieldError{Field: field, Value: strconv.Quote(value), Want: want}
}

func intError(field string, value int, want string) error {
	return &FieldError{Field: field, Value: strconv.Itoa(value), Want: want}
}

func floatError(field string, value float64, want string) error {
	return &FieldError{Field: field, Value: strconv.FormatFloat(value, 'f', -1, 64), Want: want}
}
