//go:build unix

// Package claude adapts the Claude Code CLI to belay's AgentBackend contract.
//
// It is the only agent backend belay ships in v0.1 (ADR-0005). It drives the
// `claude` executable in non-interactive mode —
//
//	claude -p "<prompt>" --output-format json [flags...]
//
// — and turns the single JSON object that prints into a belay.AgentResponse.
// Every subprocess goes through internal/exec, so the child gets a
// deny-by-default environment, a deadline, a bounded capture, and redaction of
// its output and argv; this package never touches os/exec.
//
// # The two ErrToolchainMissing sentinels
//
// internal/exec and pkg/belay each define an ErrToolchainMissing, and they are
// different values. Handing an exec error straight back through Invoke would
// make errors.Is(err, belay.ErrToolchainMissing) false, and the graph would
// route "claude isn't installed" into its fix loop — asking an agent that does
// not exist to edit code until the problem goes away. Invoke therefore
// translates at this seam and returns a *belay.ToolchainError. See
// TestErrorTranslation.
//
// # Cost
//
// Cost parsing is load-bearing: the budget ledger (ADR-0007) consumes
// Usage.USD and stops a run when it crosses the configured ceiling. The CLI
// reports total_cost_usd, and when a future version does not, this package
// prices the reported tokens from a static table and sets Usage.Estimated.
// The guard's precision degrades; the guard itself never silently vanishes.
// Note that the CLI's own figure is a client-side estimate of the bill rather
// than authoritative billing data:
// https://code.claude.com/docs/en/agent-sdk/cost-tracking
//
// # Testing
//
// Nothing in this package's tests runs the real `claude` binary — it costs
// money on every call. The Runner field is the seam: tests set it to a fake
// that replays hand-authored fixtures from testdata/.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// BackendName is the stable identifier this backend reports from Name.
//
// It is a wire value, matched against config's agent.backend, and must not
// change between versions.
const BackendName = "claude-code"

// DefaultPath is the executable looked up on PATH when Backend.Path is empty.
const DefaultPath = "claude"

// DefaultMaxOutput caps the CLI's captured stdout and stderr independently.
//
// It is well above internal/exec's own 1 MiB default because the whole reply
// arrives as one JSON object on stdout: a capture that truncates it does not
// lose a log tail, it destroys the only copy of the result. 8 MiB holds a very
// long transcript, and output past it still produces a clear *ParseError
// rather than a plausible-looking partial parse.
const DefaultMaxOutput int64 = 8 << 20

// The credentials the CLI authenticates with: an API key billed per call, or
// the long-lived OAuth token `claude setup-token` issues to a Claude
// subscription. Either may be set; the CLI prefers the API key when both are.
//
// Both are passed through Command.SecretEnv, which allowlists each into the
// child and seeds the redactor with its value, so neither can reach a captured
// Result, an error string, or a log line. Neither is ever placed on argv.
//
// A credential missing from this list does not fail loudly. exec drops it, the
// CLI starts signed out, and it exits 1 within a second at no cost and with
// nothing on stderr (see TestOAuthTokenReachesChildRedacted).
//
// #nosec G101 -- these are the *names* of environment variables, not
// credentials. Their values are never held by this package: they are passed to
// the child by name through Command.SecretEnv.
const (
	apiKeyEnv     = "ANTHROPIC_API_KEY"
	oauthTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"
)

// Sentinel errors. Match them with errors.Is.
var (
	// ErrInvocation reports that the CLI ran and failed: a non-zero exit, a
	// process that could not be started for a reason other than a missing
	// binary, or a run the CLI itself flagged with is_error.
	//
	// It never overlaps with belay.ErrToolchainMissing. That distinction is
	// the point: one means a human installs software, the other means the
	// agent tries again.
	ErrInvocation = errors.New("belay/claude: claude invocation failed")

	// ErrTimeout reports that the CLI exceeded its deadline and its process
	// group was killed. It is reported alongside ErrInvocation so a caller
	// can either treat every failure alike or single out the timeout.
	ErrTimeout = errors.New("belay/claude: claude invocation timed out")

	// ErrInvalidRequest reports an AgentRequest missing something the
	// contract marks required, caught before a paid call is made.
	ErrInvalidRequest = errors.New("belay/claude: invalid agent request")
)

// InvokeError reports that the CLI ran and did not produce a usable result.
//
// It wraps ErrInvocation, plus ErrTimeout when the deadline elapsed, plus the
// underlying cause. Every string field has already passed through internal/exec's
// redactor.
type InvokeError struct {
	// Args is the redacted argv, program first.
	Args []string
	// ExitCode is the child's exit status, or -1 when it never ran.
	ExitCode int
	// Stderr is the redacted tail of the child's standard error.
	Stderr string
	// Message is the CLI's own explanation of a non-zero exit: the result
	// text of the is_error object it printed on stdout, or empty when it
	// printed none. Like Stderr, it has passed through the redactor.
	Message string
	// TimedOut reports that the deadline elapsed.
	TimedOut bool
	// Err is the underlying cause, if any.
	Err error
}

// Error implements error.
func (e *InvokeError) Error() string {
	var b strings.Builder
	b.WriteString("belay/claude: claude")
	if e.TimedOut {
		b.WriteString(" timed out")
	} else {
		fmt.Fprintf(&b, " failed with exit code %d", e.ExitCode)
	}
	switch s := strings.TrimSpace(e.Stderr); {
	case e.Message != "":
		fmt.Fprintf(&b, ": %s", e.Message)
	case s != "":
		fmt.Fprintf(&b, ": %s", lastLines(s, 3))
	case e.Err != nil:
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

// Unwrap reports the sentinels this error satisfies and its underlying cause.
func (e *InvokeError) Unwrap() []error {
	out := make([]error, 0, 3)
	out = append(out, ErrInvocation)
	if e.TimedOut {
		out = append(out, ErrTimeout)
	}
	if e.Err != nil {
		out = append(out, e.Err)
	}
	return out
}

// Runner is the subprocess seam.
//
// It is the one method this package needs from *exec.Runner, named as an
// interface so a test can substitute a fake and no test ever spends money by
// starting the real CLI. Production callers pass an *exec.Runner, which
// satisfies it.
type Runner interface {
	// Run executes c and returns its redacted result. A Result is returned
	// even on failure.
	Run(ctx context.Context, c exec.Command) (exec.Result, error)
}

// Backend is a belay.AgentBackend backed by the Claude Code CLI.
//
// The zero value is usable and runs the real `claude` from PATH; New returns
// one with an explicit logger. Fields are read on every Invoke, so a Backend
// must not be mutated once it is shared.
type Backend struct {
	// Runner executes the CLI. Nil builds one from Logger on first use.
	Runner Runner
	// Logger receives structured events. Nil uses slog.Default.
	Logger *slog.Logger
	// Path is the executable to run. Empty uses DefaultPath.
	Path string
	// PermissionMode is passed to --permission-mode when non-empty, for
	// example "acceptEdits" or "dontAsk". Empty passes no flag, leaving the
	// CLI's own default for -p runs, which is to deny anything not
	// explicitly allowed.
	PermissionMode string
	// EnvAllow names additional parent environment variables to pass to the
	// child. internal/exec is deny-by-default, so a deployment pointing the
	// CLI at Bedrock, Vertex, or an HTTP proxy must name those variables
	// here; ANTHROPIC_API_KEY and CLAUDE_CODE_OAUTH_TOKEN are always passed
	// and never need listing.
	EnvAllow []string
	// Timeout bounds one invocation. Zero uses the Runner's default.
	Timeout time.Duration
	// MaxOutput caps captured stdout and stderr independently, in bytes.
	// Zero uses DefaultMaxOutput.
	MaxOutput int64
	// Prices overrides the fallback price table consulted when the CLI
	// reports no cost of its own. Nil uses the built-in table.
	Prices map[string]Price
}

// New returns a Backend that logs to logger, or to slog.Default when nil.
func New(logger *slog.Logger) *Backend {
	return &Backend{Logger: logger}
}

// Name returns "claude-code", matching config's agent.backend value.
func (b *Backend) Name() string { return BackendName }

func (b *Backend) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

func (b *Backend) path() string {
	if b.Path != "" {
		return b.Path
	}
	return DefaultPath
}

func (b *Backend) runner() Runner {
	if b.Runner != nil {
		return b.Runner
	}
	return exec.New(b.logger())
}

func (b *Backend) prices() map[string]Price {
	if b.Prices != nil {
		return b.Prices
	}
	return defaultPrices
}

// Invoke runs the CLI once against req and returns its response.
//
// The error is:
//
//   - *belay.ToolchainError (errors.Is belay.ErrToolchainMissing) when the
//     `claude` binary is not installed,
//   - *InvokeError (errors.Is ErrInvocation, and ErrTimeout on a deadline)
//     when it ran and failed,
//   - *ParseError (errors.Is ErrUnparsableOutput) when its stdout was not the
//     promised JSON object,
//   - *PriceError (errors.Is ErrUnknownModel) when a cost had to be estimated
//     and no price entry matched,
//   - ctx.Err() when the caller's context was cancelled,
//   - nil on success.
//
// On a run the CLI itself flagged with is_error, Invoke returns both the
// parsed response and an *InvokeError, so a caller can journal what the agent
// managed to say before it failed.
func (b *Backend) Invoke(ctx context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
	if err := validate(req); err != nil {
		return belay.AgentResponse{}, err
	}

	mcpPath, cleanup, err := writeMCPConfig(req.MCPConfig)
	if err != nil {
		return belay.AgentResponse{}, err
	}
	defer cleanup()

	cmd := b.command(req, mcpPath)
	res, runErr := b.runner().Run(ctx, cmd)
	if runErr != nil {
		return belay.AgentResponse{}, b.translate(ctx, res, runErr)
	}

	return b.respond(ctx, req, res)
}

// respond turns a clean exit into an AgentResponse.
func (b *Backend) respond(ctx context.Context, req belay.AgentRequest, res exec.Result) (belay.AgentResponse, error) {
	parsed, raw, err := parseResult(res.Stdout, res.Truncated)
	if err != nil {
		return belay.AgentResponse{}, err
	}

	usage, err := usageOf(parsed, req.Model, b.prices())
	if err != nil {
		return belay.AgentResponse{}, err
	}

	out := belay.AgentResponse{
		Text:      string(parsed.Result),
		SessionID: parsed.SessionID,
		Usage:     usage,
		Turns:     parsed.NumTurns,
		Raw:       raw,
	}

	b.logger().LogAttrs(ctx, slog.LevelDebug, "belay/claude: invocation complete",
		slog.String("session_id", out.SessionID),
		slog.Int("turns", out.Turns),
		slog.Float64("usd", out.Usage.USD),
		slog.Bool("estimated", out.Usage.Estimated),
		slog.Duration("duration", res.Duration))

	// The CLI can exit zero but set is_error when the failure happened inside
	// the run. The response is still returned, because the contract lets a
	// failed call carry output a caller may want. A failure the CLI exits
	// non-zero for never reaches here; translate reports it (see cliMessage).
	if parsed.IsError {
		return out, &InvokeError{
			Args:     res.Args,
			ExitCode: res.ExitCode,
			Stderr:   res.Stderr,
			Err: fmt.Errorf("claude reported subtype %q: %s",
				parsed.Subtype, firstLine(out.Text)),
		}
	}
	return out, nil
}

// translate converts an internal/exec failure into this package's error
// vocabulary.
//
// This is the seam the package doc warns about: exec's ErrToolchainMissing and
// belay's are different sentinels, and only a *belay.ToolchainError makes
// errors.Is(err, belay.ErrToolchainMissing) hold for a caller above pkg/belay.
func (b *Backend) translate(ctx context.Context, res exec.Result, err error) error {
	switch {
	case errors.Is(err, exec.ErrToolchainMissing):
		b.logger().LogAttrs(ctx, slog.LevelError, "belay/claude: CLI not installed",
			slog.String("tool", b.path()))
		return &belay.ToolchainError{Tool: b.path(), Err: err}

	case errors.Is(err, exec.ErrTimeout):
		return &InvokeError{Args: res.Args, ExitCode: res.ExitCode,
			Stderr: res.Stderr, TimedOut: true, Err: err}

	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		// A cancelled parent is the caller's own doing, not a backend
		// failure; pass it through so errors.Is(err, context.Canceled)
		// keeps working.
		return err

	default:
		return &InvokeError{Args: res.Args, ExitCode: res.ExitCode,
			Stderr: res.Stderr, Message: cliMessage(res), Err: err}
	}
}

// cliMessage returns the CLI's own explanation of a non-zero exit, or "".
//
// The CLI reports a failure that stops the run before any model call, such as
// being signed out or holding a rejected credential, as the result of an
// is_error object on stdout. It writes nothing to stderr and exits 1, so
// without this the only thing a person sees is the exit code. A result that is
// not flagged as an error is the agent's answer, not an explanation, and is
// ignored. res.Stdout has already passed through the redactor.
func cliMessage(res exec.Result) string {
	parsed, _, err := parseResult(res.Stdout, res.Truncated)
	if err != nil || !parsed.IsError {
		return ""
	}
	return firstLine(string(parsed.Result))
}

// toolNames returns the distinct tool names in rules, in first-seen order,
// without their permission patterns: "Bash(git diff *)" names Bash.
//
// --tools needs bare names. Claude Code 2.1.274 drops a tool named with a
// pattern from --tools altogether, and ignores a name it does not know.
// "default" is left out, because --tools reads it as every built-in tool. A
// list that yields no names becomes --tools "", which removes every tool:
// the restrictive answer for a request that asked for a restriction.
func toolNames(rules []string) []string {
	names := make([]string, 0, len(rules))
	for _, rule := range rules {
		name, _, _ := strings.Cut(rule, "(")
		name = strings.TrimSpace(name)
		if name != "" && !strings.EqualFold(name, "default") && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// command builds the argv and environment for one invocation.
//
// Flag spellings follow the documented CLI reference:
// https://code.claude.com/docs/en/cli-reference and the non-interactive
// examples at https://code.claude.com/docs/en/headless
func (b *Backend) command(req belay.AgentRequest, mcpPath string) exec.Command {
	args := []string{"-p", req.Prompt, "--output-format", "json"}

	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	if len(req.AllowedTools) > 0 {
		// Documented as a comma-separated single argument:
		// `--allowedTools "Bash,Read,Edit"`. Passing one argument rather
		// than a variadic list keeps a tool pattern containing a space,
		// such as "Bash(git diff *)", from being split into two.
		args = append(args, "--allowedTools", strings.Join(req.AllowedTools, ","))
		// --allowedTools only approves. Every other built-in tool stays
		// available, and the user's or the project's Claude Code settings
		// can approve it too (confirmed on 2.1.274: with --allowedTools
		// Read,Glob,Grep the session still offers Bash, Edit and Write).
		// belay.AgentRequest.AllowedTools promises a restriction, and the
		// plan node relies on it to stay read-only, so --tools makes the
		// named tools the only built-in ones.
		args = append(args, "--tools", strings.Join(toolNames(req.AllowedTools), ","))
		// --tools leaves MCP tools alone, so a server in the user's
		// ~/.claude.json, or in a repository's .mcp.json that its settings
		// enable, would still hand the agent its tools. --strict-mcp-config
		// loads only the servers belay passes with --mcp-config below, and
		// those keep every tool they offer.
		args = append(args, "--strict-mcp-config")
	}
	if req.SystemPrompt != "" {
		// --append-system-prompt, not --system-prompt: the latter replaces
		// Claude Code's default prompt outright, which strips the tool-use
		// instructions a coding agent needs. belay's contract allows a
		// backend to augment rather than override, so it augments.
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if mcpPath != "" {
		args = append(args, "--mcp-config", mcpPath)
	}
	if b.PermissionMode != "" {
		args = append(args, "--permission-mode", b.PermissionMode)
	}

	maxOut := b.MaxOutput
	if maxOut == 0 {
		maxOut = DefaultMaxOutput
	}

	return exec.Command{
		Path: b.path(),
		Args: args,
		// WorkDir is the child's working directory, which is already the
		// root the CLI operates on. --add-dir is for granting access to
		// directories *beyond* that one, so naming the working directory
		// there would be a no-op; belay adds no extra roots.
		Dir: req.WorkDir,
		// SecretEnv, not EnvAllow: it allowlists each credential into the
		// child and registers its value with the redactor in one step, so the
		// value cannot survive into Result.Stdout, Result.Stderr, Result.Args,
		// an error string, or a log line.
		SecretEnv: []string{apiKeyEnv, oauthTokenEnv},
		EnvAllow:  b.EnvAllow,
		Timeout:   b.Timeout,
		MaxOutput: maxOut,
	}
}

// validate rejects a request the contract marks incomplete, before any paid
// call is made.
func validate(req belay.AgentRequest) error {
	if strings.TrimSpace(req.Prompt) == "" {
		return fmt.Errorf("%w: Prompt is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.WorkDir) == "" {
		return fmt.Errorf("%w: WorkDir is required", ErrInvalidRequest)
	}
	return nil
}

// writeMCPConfig spills an MCP configuration document to a private temporary
// file for --mcp-config, and returns a cleanup that removes it.
//
// The document is opaque to belay and is never parsed. The file is mode 0600
// because it routinely carries the credentials of the MCP servers it
// configures, and it is removed as soon as the child has exited. When there is
// no configuration the path is empty and cleanup is a no-op.
func writeMCPConfig(cfg json.RawMessage) (path string, cleanup func(), err error) {
	noop := func() {}
	if len(bytes.TrimSpace(cfg)) == 0 {
		return "", noop, nil
	}

	f, err := os.CreateTemp("", "belay-mcp-*.json")
	if err != nil {
		return "", noop, fmt.Errorf("belay/claude: cannot stage --mcp-config: %w", err)
	}
	name := f.Name()
	remove := func() { _ = os.Remove(name) }

	// CreateTemp already opens at 0600, but the mode is stated rather than
	// inherited: this file can hold MCP server credentials, and a umask
	// change elsewhere must not be able to widen it silently.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		remove()
		return "", noop, fmt.Errorf("belay/claude: cannot secure --mcp-config: %w", err)
	}
	if _, err := f.Write(cfg); err != nil {
		_ = f.Close()
		remove()
		return "", noop, fmt.Errorf("belay/claude: cannot write --mcp-config: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", noop, fmt.Errorf("belay/claude: cannot close --mcp-config: %w", err)
	}
	return name, remove, nil
}

// firstLine returns the first non-empty line of s, for a one-line error.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// lastLines returns the final n lines of s joined by "; ".
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

var _ belay.AgentBackend = (*Backend)(nil)
