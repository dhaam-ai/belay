//go:build unix

package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/internal/exec"
	"github.com/belay-dev/belay/pkg/belay"
)

func TestName(t *testing.T) {
	t.Parallel()
	b := newBackend(t, &fakeRunner{})
	if got := b.Name(); got != "claude-code" {
		t.Errorf("Name() = %q, want %q", got, "claude-code")
	}
	if BackendName != "claude-code" {
		t.Errorf("BackendName = %q, want it to match config's agent.backend", BackendName)
	}
}

// TestInvokeBuildsArgv pins the AgentRequest-to-argv mapping. Flag spellings
// come from https://code.claude.com/docs/en/cli-reference and the -p examples
// at https://code.claude.com/docs/en/headless
func TestInvokeBuildsArgv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		req      belay.AgentRequest
		permMode string
		want     []string
	}{
		{
			name: "minimal request",
			req:  belay.AgentRequest{Prompt: "fix the bug", WorkDir: "/repo"},
			want: []string{"-p", "fix the bug", "--output-format", "json"},
		},
		{
			name: "model and turn cap",
			req: belay.AgentRequest{
				Prompt: "fix", WorkDir: "/repo",
				Model: "claude-opus-5", MaxTurns: 12,
			},
			want: []string{
				"-p", "fix", "--output-format", "json",
				"--model", "claude-opus-5", "--max-turns", "12",
			},
		},
		{
			name: "allowed tools are one comma-joined argument",
			req: belay.AgentRequest{
				Prompt: "fix", WorkDir: "/repo",
				AllowedTools: []string{"Read", "Edit", "Bash"},
			},
			want: []string{
				"-p", "fix", "--output-format", "json",
				"--allowedTools", "Read,Edit,Bash",
			},
		},
		{
			name: "a tool pattern containing a space stays one argument",
			req: belay.AgentRequest{
				Prompt: "fix", WorkDir: "/repo",
				AllowedTools: []string{"Bash(git diff *)", "Read"},
			},
			want: []string{
				"-p", "fix", "--output-format", "json",
				"--allowedTools", "Bash(git diff *),Read",
			},
		},
		{
			name: "system prompt is appended, not substituted",
			req: belay.AgentRequest{
				Prompt: "fix", WorkDir: "/repo",
				SystemPrompt: "You are a security engineer.",
			},
			want: []string{
				"-p", "fix", "--output-format", "json",
				"--append-system-prompt", "You are a security engineer.",
			},
		},
		{
			name: "session id resumes",
			req: belay.AgentRequest{
				Prompt: "keep going", WorkDir: "/repo",
				SessionID: "3f9c1b52-8a4d-4e21-9c77-2b6f0d5a1e83",
			},
			want: []string{
				"-p", "keep going", "--output-format", "json",
				"--resume", "3f9c1b52-8a4d-4e21-9c77-2b6f0d5a1e83",
			},
		},
		{
			name:     "permission mode comes from the backend, not the request",
			req:      belay.AgentRequest{Prompt: "fix", WorkDir: "/repo"},
			permMode: "acceptEdits",
			want: []string{
				"-p", "fix", "--output-format", "json",
				"--permission-mode", "acceptEdits",
			},
		},
		{
			name: "zero values pass no flag at all",
			req: belay.AgentRequest{
				Prompt: "fix", WorkDir: "/repo",
				Model: "", MaxTurns: 0, AllowedTools: nil, SessionID: "",
			},
			want: []string{"-p", "fix", "--output-format", "json"},
		},
		{
			name: "everything at once",
			req: belay.AgentRequest{
				Prompt: "implement Login", WorkDir: "/repo",
				SystemPrompt: "Be terse.", Model: "sonnet", MaxTurns: 5,
				AllowedTools: []string{"Read", "Write"}, SessionID: "s-1",
			},
			permMode: "dontAsk",
			want: []string{
				"-p", "implement Login", "--output-format", "json",
				"--model", "sonnet", "--max-turns", "5",
				"--allowedTools", "Read,Write",
				"--append-system-prompt", "Be terse.",
				"--resume", "s-1",
				"--permission-mode", "dontAsk",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{Result: okResult(fixture(t, "success_with_cost.json"))}
			b := newBackend(t, fake)
			b.PermissionMode = tt.permMode

			if _, err := b.Invoke(t.Context(), tt.req); err != nil {
				t.Fatalf("Invoke: %v", err)
			}

			got := fake.LastCall(t)
			if diff := cmp.Diff(tt.want, got.Args); diff != "" {
				t.Errorf("argv mismatch (-want +got):\n%s", diff)
			}
			if got.Path != "claude" {
				t.Errorf("Path = %q, want %q", got.Path, "claude")
			}
			if got.Dir != tt.req.WorkDir {
				t.Errorf("Dir = %q, want the request's WorkDir %q", got.Dir, tt.req.WorkDir)
			}
			// --add-dir is for roots beyond the working directory; naming
			// the working directory there would be a no-op.
			if slices.Contains(got.Args, "--add-dir") {
				t.Error("argv should not carry --add-dir for the working directory")
			}
		})
	}
}

func TestInvokeMapsTheResponse(t *testing.T) {
	t.Parallel()

	body := fixture(t, "success_with_cost.json")
	fake := &fakeRunner{Result: okResult(body)}
	b := newBackend(t, fake)

	got, err := b.Invoke(t.Context(), belay.AgentRequest{
		Prompt: "implement Login", WorkDir: "/repo", Model: "claude-opus-5",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if !strings.Contains(got.Text, "Added the Login handler") {
		t.Errorf("Text = %q, want the result field", got.Text)
	}
	if got.SessionID != "3f9c1b52-8a4d-4e21-9c77-2b6f0d5a1e83" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.Turns != 7 {
		t.Errorf("Turns = %d, want 7", got.Turns)
	}
	if !almostEqual(got.Usage.USD, 0.4213) || got.Usage.Estimated {
		t.Errorf("Usage = %+v, want the reported 0.4213 unestimated", got.Usage)
	}
	if string(got.Raw) != strings.TrimSpace(body) {
		t.Error("Raw is not the CLI's own bytes")
	}
}

// TestErrorTranslation is the acceptance test for the two-sentinel trap.
//
// internal/exec and pkg/belay both define ErrToolchainMissing and they are
// different values. Only the missing-binary case may satisfy belay's, because
// that sentinel is what the graph branches on to decide between "a human
// installs software" and "the agent edits code".
func TestErrorTranslation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// what the runner returns
		res exec.Result
		err error
		// what Invoke must produce
		wantBelayToolchain bool
		wantInvocation     bool
		wantTimeout        bool
		wantParse          bool
		wantMsgHas         string
	}{
		{
			name: "claude is not installed",
			res:  exec.Result{ExitCode: -1, Args: []string{"claude"}},
			err: &exec.ToolchainError{
				Tool:   "claude",
				Err:    exec.ErrToolchainMissing,
				Detail: `exec: "claude": executable file not found in $PATH`,
			},
			wantBelayToolchain: true,
			wantMsgHas:         "claude",
		},
		{
			name: "non-zero exit with stderr is not a missing toolchain",
			res: exec.Result{
				ExitCode: 2, Args: []string{"claude"},
				Stderr: "", // filled from the fixture below
			},
			err: &exec.ExitError{
				Args: []string{"claude"}, Code: 2,
				Stderr: "error: unknown option '--allowedTool'",
			},
			wantInvocation: true,
			wantMsgHas:     "unknown option",
		},
		{
			name: "timeout is distinguishable from every other failure",
			res:  exec.Result{ExitCode: 137, Args: []string{"claude"}, TimedOut: true},
			err: &exec.TimeoutError{
				Args: []string{"claude"}, Timeout: 30 * time.Second,
			},
			wantInvocation: true,
			wantTimeout:    true,
			wantMsgHas:     "timed out",
		},
		{
			name:       "a clean exit with an unreadable body is a parse failure",
			res:        okResult(`{"type":"result","result":"cut off`),
			err:        nil,
			wantParse:  true,
			wantMsgHas: "cannot parse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{Result: tt.res, Err: tt.err}
			b := newBackend(t, fake)

			_, err := b.Invoke(t.Context(), validRequest())
			if err == nil {
				t.Fatal("want an error, got nil")
			}

			// The load-bearing assertion.
			gotToolchain := errors.Is(err, belay.ErrToolchainMissing)
			if gotToolchain != tt.wantBelayToolchain {
				t.Errorf("errors.Is(err, belay.ErrToolchainMissing) = %v, want %v (err: %v)",
					gotToolchain, tt.wantBelayToolchain, err)
			}
			if tt.wantBelayToolchain {
				var te *belay.ToolchainError
				if !errors.As(err, &te) {
					t.Fatalf("errors.As(*belay.ToolchainError) = false for %v", err)
				}
				if te.Tool != "claude" {
					t.Errorf("ToolchainError.Tool = %q, want %q", te.Tool, "claude")
				}
			}

			if got := errors.Is(err, ErrInvocation); got != tt.wantInvocation {
				t.Errorf("errors.Is(err, ErrInvocation) = %v, want %v", got, tt.wantInvocation)
			}
			if got := errors.Is(err, ErrTimeout); got != tt.wantTimeout {
				t.Errorf("errors.Is(err, ErrTimeout) = %v, want %v", got, tt.wantTimeout)
			}
			if got := errors.Is(err, ErrUnparsableOutput); got != tt.wantParse {
				t.Errorf("errors.Is(err, ErrUnparsableOutput) = %v, want %v", got, tt.wantParse)
			}
			if tt.wantMsgHas != "" && !strings.Contains(err.Error(), tt.wantMsgHas) {
				t.Errorf("error %q, want it to mention %q", err, tt.wantMsgHas)
			}
		})
	}
}

// TestToolchainMissingSurvivesWrapping proves the translated error keeps
// working after a caller wraps it, which is how it will actually reach the
// graph's branch.
func TestToolchainMissingSurvivesWrapping(t *testing.T) {
	t.Parallel()

	fake := &fakeRunner{
		Result: exec.Result{ExitCode: -1},
		Err: &exec.ToolchainError{
			Tool: "claude", Err: exec.ErrToolchainMissing, Detail: "not found",
		},
	}
	b := newBackend(t, fake)

	_, err := b.Invoke(t.Context(), validRequest())
	wrapped := fmt.Errorf("node %q failed: %w", "code", err)

	if !errors.Is(wrapped, belay.ErrToolchainMissing) {
		t.Error("belay.ErrToolchainMissing did not survive wrapping")
	}
	// The exec-side sentinel remains reachable for anyone who wants it,
	// but it is not what callers above pkg/belay are expected to match.
	if !errors.Is(wrapped, exec.ErrToolchainMissing) {
		t.Error("the underlying exec cause was dropped from the chain")
	}
}

// TestInvokeReportsCLIFlaggedFailures covers the documented case where the CLI
// exits zero but reports the failure inside the JSON body.
func TestInvokeReportsCLIFlaggedFailures(t *testing.T) {
	t.Parallel()

	body := `{"type":"result","subtype":"error_during_execution","is_error":true,` +
		`"result":"Invalid API key. Please run /login.","session_id":"s-9",` +
		`"num_turns":1,"total_cost_usd":0.001,` +
		`"usage":{"input_tokens":10,"output_tokens":2}}`
	fake := &fakeRunner{Result: okResult(body)}
	b := newBackend(t, fake)

	got, err := b.Invoke(t.Context(), validRequest())
	if err == nil {
		t.Fatal("is_error:true must surface as an error")
	}
	if !errors.Is(err, ErrInvocation) {
		t.Errorf("errors.Is(err, ErrInvocation) = false for %v", err)
	}
	if errors.Is(err, belay.ErrToolchainMissing) {
		t.Error("a failed run must not be reported as a missing toolchain")
	}
	if !strings.Contains(err.Error(), "error_during_execution") {
		t.Errorf("error %q should name the CLI's own subtype", err)
	}
	// The contract allows a failed call to carry output the caller may
	// want to journal.
	if got.Text == "" || got.SessionID != "s-9" {
		t.Errorf("the partial response was discarded: %+v", got)
	}
}

func TestInvokeRejectsIncompleteRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  belay.AgentRequest
	}{
		{name: "no prompt", req: belay.AgentRequest{WorkDir: "/repo"}},
		{name: "blank prompt", req: belay.AgentRequest{Prompt: "  \n", WorkDir: "/repo"}},
		{name: "no workdir", req: belay.AgentRequest{Prompt: "fix"}},
		{name: "blank workdir", req: belay.AgentRequest{Prompt: "fix", WorkDir: " "}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{}
			b := newBackend(t, fake)

			if _, err := b.Invoke(t.Context(), tt.req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want one wrapping ErrInvalidRequest", err)
			}
			// The point of validating: no paid call was made.
			if n := len(fake.Calls()); n != 0 {
				t.Errorf("the runner was called %d times for an invalid request, want 0", n)
			}
		})
	}
}

func TestInvokeStagesMCPConfig(t *testing.T) {
	t.Parallel()

	cfg := json.RawMessage(`{"mcpServers":{"sonar":{"command":"sonar-mcp","env":{"SONAR_TOKEN":"squ_secret"}}}}`)

	var (
		seenPath string
		seenBody []byte
		seenMode os.FileMode
	)
	fake := &fakeRunner{Fn: func(c exec.Command) (exec.Result, error) {
		// Inspect the staged file while the "process" is running.
		i := slices.Index(c.Args, "--mcp-config")
		if i < 0 || i+1 >= len(c.Args) {
			return exec.Result{}, errors.New("no --mcp-config in argv")
		}
		seenPath = c.Args[i+1]
		info, err := os.Stat(seenPath)
		if err != nil {
			return exec.Result{}, err
		}
		seenMode = info.Mode().Perm()
		// #nosec G304 -- seenPath is the temp file this test just watched
		// the backend stage into its own argv.
		seenBody, err = os.ReadFile(seenPath)
		if err != nil {
			return exec.Result{}, err
		}
		return okResult(fixture(t, "success_with_cost.json")), nil
	}}

	b := newBackend(t, fake)
	req := validRequest()
	req.MCPConfig = cfg
	if _, err := b.Invoke(t.Context(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if !strings.HasSuffix(seenPath, ".json") {
		t.Errorf("staged path %q should end in .json", seenPath)
	}
	if got := string(seenBody); got != string(cfg) {
		t.Errorf("staged body = %q, want the document verbatim %q", got, cfg)
	}
	if seenMode != 0o600 {
		t.Errorf("staged file mode = %#o, want 0600: it can carry MCP server credentials", seenMode)
	}
	if _, err := os.Stat(seenPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staged file outlived the invocation: stat err = %v", err)
	}
}

func TestInvokeOmitsMCPConfigWhenEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  json.RawMessage
	}{
		{name: "nil", cfg: nil},
		{name: "empty", cfg: json.RawMessage("")},
		{name: "whitespace", cfg: json.RawMessage("  \n ")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{Result: okResult(fixture(t, "success_with_cost.json"))}
			b := newBackend(t, fake)
			req := validRequest()
			req.MCPConfig = tt.cfg

			if _, err := b.Invoke(t.Context(), req); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if slices.Contains(fake.LastCall(t).Args, "--mcp-config") {
				t.Error("argv carries --mcp-config for an empty configuration")
			}
		})
	}
}

// TestAPIKeyIsRequestedAsASecret pins how the credential reaches the child:
// through SecretEnv, which both allowlists it and registers it for redaction,
// and never through argv.
func TestAPIKeyIsRequestedAsASecret(t *testing.T) {
	t.Parallel()

	fake := &fakeRunner{Result: okResult(fixture(t, "success_with_cost.json"))}
	b := newBackend(t, fake)
	b.EnvAllow = []string{"ANTHROPIC_BASE_URL"}

	if _, err := b.Invoke(t.Context(), validRequest()); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	cmd := fake.LastCall(t)
	if !slices.Contains(cmd.SecretEnv, apiKeyEnv) {
		t.Errorf("SecretEnv = %v, want it to carry %s", cmd.SecretEnv, apiKeyEnv)
	}
	if !slices.Contains(cmd.EnvAllow, "ANTHROPIC_BASE_URL") {
		t.Errorf("EnvAllow = %v, want the backend's own passthrough", cmd.EnvAllow)
	}
	// A credential on argv would land in the journal and in ps output.
	for _, a := range cmd.Args {
		if strings.Contains(a, apiKeyEnv) {
			t.Errorf("argv mentions the credential variable: %q", a)
		}
	}
	if cmd.ExtraEnv[apiKeyEnv] != "" {
		t.Error("the credential must be passed through by name, never by value")
	}
}

// TestAPIKeyRedactedByRealRunner is the end-to-end secret-containment test.
//
// It uses the real *exec.Runner against a child that deliberately prints its
// own credential to both stdout and stderr, and asserts the value cannot be
// found in the Result, the response, or any error string. The child is this
// test binary re-executed in helper mode — never the real `claude`.
func TestAPIKeyRedactedByRealRunner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
	}{
		{
			// Shaped like a real key, so the redactor's built-in pattern
			// would catch it even unseeded.
			name: "pattern shaped key",
			key:  "sk-ant-api03-9f3c2a71b8e04d5casdf1234567890abcdefZZ",
		},
		{
			// Deliberately unlike any built-in pattern: only the SecretEnv
			// seeding can redact this one, so it proves the wiring works.
			name: "opaque key that only SecretEnv can catch",
			key:  "belay-test-credential-9f3c2a71b8e04d5c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			exe := testExe(t)
			runner := &exec.Runner{
				Logger: quietLogger(),
				// A fabricated parent environment, so the real one is
				// untouched and the test is safe to run in parallel.
				Environ: func() []string {
					return []string{
						apiKeyEnv + "=" + tt.key,
						helperModeEnv + "=leak",
						"PATH=" + os.Getenv("PATH"),
						"HOME=" + os.Getenv("HOME"),
					}
				},
			}

			b := &Backend{Runner: runner, Logger: quietLogger(), Path: exe}
			// The helper mode must survive exec's deny-by-default filter.
			b.EnvAllow = []string{helperModeEnv}

			// A real working directory: fork/exec fails before the child
			// runs if Dir does not exist.
			req := validRequest()
			req.WorkDir = t.TempDir()

			got, err := b.Invoke(t.Context(), req)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}

			// The child really did try to print it.
			if !strings.Contains(got.Text, "the key is") {
				t.Fatalf("helper did not run as expected: Text = %q", got.Text)
			}
			if !strings.Contains(got.Text, "[REDACTED:") {
				t.Errorf("Text = %q, want a redaction marker where the key was", got.Text)
			}

			// Nothing anywhere may contain the value.
			surfaces := map[string]string{
				"AgentResponse.Text": got.Text,
				"AgentResponse.Raw":  string(got.Raw),
				"SessionID":          got.SessionID,
			}
			for name, s := range surfaces {
				if strings.Contains(s, tt.key) {
					t.Errorf("%s leaked the credential", name)
				}
			}
		})
	}
}

// TestAPIKeyAbsentFromResultAndErrors covers the failure path: a child that
// exits non-zero after printing the credential to stderr must not smuggle it
// out through the error message or the captured Result.
func TestAPIKeyAbsentFromResultAndErrors(t *testing.T) {
	t.Parallel()

	const key = "belay-test-credential-deadbeefcafe"
	exe := testExe(t)

	runner := &exec.Runner{
		Logger: quietLogger(),
		Environ: func() []string {
			return []string{
				apiKeyEnv + "=" + key,
				helperModeEnv + "=leak",
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + os.Getenv("HOME"),
			}
		},
	}

	// Capture the raw Result by wrapping the real runner.
	var captured exec.Result
	spy := &fakeRunner{Fn: func(c exec.Command) (exec.Result, error) {
		res, err := runner.Run(t.Context(), c)
		captured = res
		// Force the error path so the error string is exercised too.
		if err == nil {
			err = &exec.ExitError{Args: res.Args, Code: 2, Stderr: res.Stderr}
		}
		return res, err
	}}

	b := &Backend{Runner: spy, Logger: quietLogger(), Path: exe,
		EnvAllow: []string{helperModeEnv}}

	req := validRequest()
	req.WorkDir = t.TempDir()

	_, err := b.Invoke(t.Context(), req)
	if err == nil {
		t.Fatal("want the forced error path")
	}

	surfaces := map[string]string{
		"Result.Stdout": captured.Stdout,
		"Result.Stderr": captured.Stderr,
		"Result.Args":   strings.Join(captured.Args, " "),
		"error string":  err.Error(),
	}
	for name, s := range surfaces {
		if strings.Contains(s, key) {
			t.Errorf("%s leaked the credential: %q", name, s)
		}
	}
	// The stderr really did carry the attempt, redacted.
	if !strings.Contains(captured.Stderr, "[REDACTED:") {
		t.Errorf("Result.Stderr = %q, want a redaction marker", captured.Stderr)
	}
}

func TestInvokePassesContextThrough(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	fake := &fakeRunner{Fn: func(exec.Command) (exec.Result, error) {
		return exec.Result{ExitCode: -1}, fmt.Errorf("belay/exec: %w", context.Canceled)
	}}
	b := newBackend(t, fake)

	_, err := b.Invoke(ctx, validRequest())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false for %v", err)
	}
	if errors.Is(err, belay.ErrToolchainMissing) {
		t.Error("a cancelled context must not look like a missing toolchain")
	}
}

func TestBackendDefaults(t *testing.T) {
	t.Parallel()

	fake := &fakeRunner{Result: okResult(fixture(t, "success_with_cost.json"))}
	b := &Backend{Runner: fake, Logger: quietLogger()}

	if _, err := b.Invoke(t.Context(), validRequest()); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	cmd := fake.LastCall(t)

	if cmd.Path != DefaultPath {
		t.Errorf("Path = %q, want the default %q", cmd.Path, DefaultPath)
	}
	// The whole reply is one JSON object on stdout, so the capture cap must
	// be well above exec's 1 MiB default or a long run parses as garbage.
	if cmd.MaxOutput != DefaultMaxOutput {
		t.Errorf("MaxOutput = %d, want %d", cmd.MaxOutput, DefaultMaxOutput)
	}
	if cmd.MaxOutput <= exec.DefaultMaxOutput {
		t.Errorf("MaxOutput %d does not exceed exec's default %d",
			cmd.MaxOutput, exec.DefaultMaxOutput)
	}

	// New(nil) must not panic on the logger.
	if got := New(nil).Name(); got != BackendName {
		t.Errorf("New(nil).Name() = %q", got)
	}
}

func TestTruncatedOutputIsReportedAsSuch(t *testing.T) {
	t.Parallel()

	fake := &fakeRunner{Result: exec.Result{
		Args: []string{"claude"}, ExitCode: 0,
		Stdout:    `{"type":"result","result":"a very long transcript that got cu`,
		Truncated: true,
	}}
	b := newBackend(t, fake)

	_, err := b.Invoke(t.Context(), validRequest())
	if !errors.Is(err, ErrUnparsableOutput) {
		t.Fatalf("error = %v, want one wrapping ErrUnparsableOutput", err)
	}
	if !strings.Contains(err.Error(), "truncated at the capture limit") {
		t.Errorf("error %q should explain that the capture was truncated", err)
	}
}
