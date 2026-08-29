//go:build unix

package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func quietRunner() *Runner {
	return &Runner{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestRunCapturesOutput(t *testing.T) {
	t.Parallel()
	r := quietRunner()
	res, err := r.Run(t.Context(), Command{
		Path:     testExe(t),
		Args:     []string{"alpha", "beta gamma"},
		ExtraEnv: helperEnvFor("args", ""),
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v (stderr: %s)", err, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if diff := cmp.Diff([]string{"alpha", "beta gamma"}, argLines(res.Stdout)); diff != "" {
		t.Errorf("captured args mismatch (-want +got):\n%s", diff)
	}
	if res.Duration <= 0 {
		t.Error("Duration was not recorded")
	}
	if res.TimedOut || res.Truncated {
		t.Errorf("TimedOut=%v Truncated=%v, want both false", res.TimedOut, res.Truncated)
	}
	if len(res.Args) == 0 || res.Args[0] != testExe(t) {
		t.Errorf("Args[0] = %v, want the program path", res.Args)
	}
}

// TestNoShellInjection is the acceptance test for requirement 1. Each case
// feeds a metacharacter payload through the same path a user-supplied
// test.custom_cmd takes, then proves three things: the payload arrived as
// literal argv, exactly one process ran, and the payload's side effect never
// happened.
func TestNoShellInjection(t *testing.T) {
	exe := testExe(t)
	tests := []struct {
		name string
		// tail is appended after the quoted program path. SENTINEL and EXE
		// are substituted before tokenizing.
		tail string
		want []string
	}{
		{"command substitution", "echo $(whoami)", []string{"echo", "$(whoami)"}},
		{"command substitution with side effect", "x $(touch SENTINEL)", []string{"x", "$(touch", "SENTINEL)"}},
		{"backticks", "echo `touch SENTINEL`", []string{"echo", "`touch", "SENTINEL`"}},
		{"variable interpolation", "echo $HOME", []string{"echo", "$HOME"}},
		{"braced variable", "echo ${HOME}", []string{"echo", "${HOME}"}},
		{"and chain", "a && touch SENTINEL", []string{"a", "&&", "touch", "SENTINEL"}},
		{"or chain", "a || touch SENTINEL", []string{"a", "||", "touch", "SENTINEL"}},
		{"semicolon chain", "a; touch SENTINEL", []string{"a;", "touch", "SENTINEL"}},
		{"pipe", "a | tee SENTINEL", []string{"a", "|", "tee", "SENTINEL"}},
		{"redirect", "a > SENTINEL", []string{"a", ">", "SENTINEL"}},
		{"append redirect", "a >> SENTINEL", []string{"a", ">>", "SENTINEL"}},
		{"background", "a & touch SENTINEL", []string{"a", "&", "touch", "SENTINEL"}},
		{"subshell", "(touch SENTINEL)", []string{"(touch", "SENTINEL)"}},
		{"glob", "ls *.go", []string{"ls", "*.go"}},
		{"newline chain", "a\ntouch SENTINEL", []string{"a", "touch", "SENTINEL"}},
		{"escaped substitution", `\$(touch SENTINEL)`, []string{"$(touch", "SENTINEL)"}},
		{"quoted payload stays one argument", "'a; touch SENTINEL'", []string{"a; touch SENTINEL"}},
		{"second helper invocation", `x ; "EXE" second`, []string{"x", ";", exe, "second"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pidDir := t.TempDir()
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			subst := func(s string) string {
				s = strings.ReplaceAll(s, "SENTINEL", sentinel)
				return strings.ReplaceAll(s, "EXE", exe)
			}

			line := strconv.Quote(exe) + " " + subst(tc.tail)
			cmd, err := ParseCommandLine(line)
			if err != nil {
				t.Fatalf("ParseCommandLine(%q): %v", line, err)
			}
			cmd.ExtraEnv = helperEnvFor("args", pidDir)
			cmd.Timeout = 30 * time.Second

			want := make([]string, len(tc.want))
			for i, w := range tc.want {
				want[i] = subst(w)
			}
			if diff := cmp.Diff(want, cmd.Args); diff != "" {
				t.Fatalf("tokenized argv mismatch (-want +got):\n%s", diff)
			}

			res, err := quietRunner().Run(t.Context(), cmd)
			if err != nil {
				t.Fatalf("Run: %v (stderr: %s)", err, res.Stderr)
			}
			if diff := cmp.Diff(want, argLines(res.Stdout)); diff != "" {
				t.Errorf("argv seen by the child mismatch (-want +got):\n%s", diff)
			}
			if n := countPIDFiles(t, pidDir); n != 1 {
				t.Errorf("%d processes ran, want exactly 1", n)
			}
			if _, err := os.Stat(sentinel); err == nil {
				t.Errorf("payload executed: %s was created", sentinel)
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("stat sentinel: %v", err)
			}
		})
	}
}

// TestNoShellInterpolation proves the $HOME and $(whoami) payloads were not
// merely passed through but never evaluated, by checking the child's output
// against the values a shell would have substituted.
func TestNoShellInterpolation(t *testing.T) {
	t.Parallel()
	res, err := quietRunner().Run(t.Context(), Command{
		Path:     testExe(t),
		Args:     []string{"$HOME", "$(whoami)", "`id`", "$USER"},
		ExtraEnv: helperEnvFor("args", ""),
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := argLines(res.Stdout)
	if diff := cmp.Diff([]string{"$HOME", "$(whoami)", "`id`", "$USER"}, got); diff != "" {
		t.Fatalf("argv mismatch (-want +got):\n%s", diff)
	}
	for _, name := range []string{"HOME", "USER"} {
		if v := os.Getenv(name); v != "" && strings.Contains(res.Stdout, v) {
			t.Errorf("$%s was expanded to %q in child output:\n%s", name, v, res.Stdout)
		}
	}
}

func TestRunEnvIsDenyByDefault(t *testing.T) {
	t.Parallel()
	r := quietRunner()
	r.Environ = func() []string {
		return append(append([]string(nil), fakeParent...), "PATH="+os.Getenv("PATH"))
	}
	res, err := r.Run(t.Context(), Command{
		Path:      testExe(t),
		EnvAllow:  []string{"CI"},
		SecretEnv: []string{"SONAR_TOKEN"},
		ExtraEnv:  helperEnvFor("env", ""),
		Timeout:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v (stderr %s)", err, res.Stderr)
	}
	for _, want := range []string{"env:CI=true", "env:HOME=/home/belay", "env:LANG=en_US.UTF-8"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("allowlisted variable missing: %q not in\n%s", want, res.Stdout)
		}
	}
	for _, leak := range []string{
		"UNRELATED_VAR",
		"unrelated-value-that-must-not-leak",
		"AWS_SECRET_ACCESS_KEY",
		"wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY",
		"DATABASE_URL",
		"ANTHROPIC_API_KEY",
	} {
		if strings.Contains(res.Stdout, leak) {
			t.Errorf("parent variable %q reached the child:\n%s", leak, res.Stdout)
		}
	}
	// The one credential the child was given is present, but its value is
	// redacted on the way back out.
	if !strings.Contains(res.Stdout, "SONAR_TOKEN=[REDACTED:SONAR_TOKEN]") {
		t.Errorf("allowlisted secret value was not redacted in captured output:\n%s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "squ_parenttokenvalue0123456789abcdef012345") {
		t.Errorf("secret value survived into captured output:\n%s", res.Stdout)
	}
}

func TestRunRedactsCapturedSecret(t *testing.T) {
	t.Parallel()
	const opaque = "totally-opaque-not-a-known-pattern-9911"
	res, err := quietRunner().Run(t.Context(), Command{
		Path: testExe(t),
		Args: []string{"SONAR_TOKEN", "BUILD_PROFILE"},
		ExtraEnv: map[string]string{
			helperModeEnv:   "echoenv",
			"SONAR_TOKEN":   opaque,
			"BUILD_PROFILE": "plain-value",
		},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(res.Stdout, opaque) {
		t.Errorf("secret survived into Stdout:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "SONAR_TOKEN=[REDACTED:SONAR_TOKEN]") {
		t.Errorf("expected redaction marker, got:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "BUILD_PROFILE=plain-value") {
		t.Errorf("non-secret value was redacted:\n%s", res.Stdout)
	}
}

// TestRunRedactsArgs covers requirement 7: a token on the command line must
// not survive into the journalled Result.
func TestRunRedactsArgs(t *testing.T) {
	t.Parallel()
	const token = "squ_argumenttoken0123456789abcdefabcdef01"
	res, err := quietRunner().Run(t.Context(), Command{
		Path:     testExe(t),
		Args:     []string{"-Dsonar.host.url=https://sonar.example", "-Dsonar.token=" + token},
		ExtraEnv: helperEnvFor("args", ""),
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	joined := strings.Join(res.Args, " ")
	if strings.Contains(joined, token) {
		t.Errorf("token survived into Result.Args: %v", res.Args)
	}
	if !strings.Contains(joined, "-Dsonar.token=[REDACTED:SONAR_TOKEN]") {
		t.Errorf("Result.Args not redacted as expected: %v", res.Args)
	}
	if !strings.Contains(joined, "-Dsonar.host.url=https://sonar.example") {
		t.Errorf("non-secret argument was mangled: %v", res.Args)
	}
	if strings.Contains(res.Stdout, token) {
		t.Errorf("token survived into Stdout:\n%s", res.Stdout)
	}
}

// TestErrorTextIsRedacted proves the token does not escape through the error
// string either, which is the path most likely to reach a log line.
func TestErrorTextIsRedacted(t *testing.T) {
	t.Parallel()
	//nolint:gosec // synthetic value, shaped like a token so the redactor sees it.
	const token = "squ_errortoken0123456789abcdefabcdef0123"
	res, err := quietRunner().Run(t.Context(), Command{
		Path:     testExe(t),
		Args:     []string{"7", "-Dsonar.token=" + token},
		ExtraEnv: helperEnvFor("exit", ""),
		Timeout:  30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected an error from a non-zero exit")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("token leaked through the error text: %v", err)
	}
	if strings.Contains(strings.Join(res.Args, " "), token) {
		t.Errorf("token leaked through Result.Args: %v", res.Args)
	}
}

// TestRedactionCoversBothStreams checks the two things most easily missed:
// stderr is redacted as thoroughly as stdout, and output that is shorter than
// the redactor's hold-back window and has no trailing newline still arrives in
// full, because Close flushes the withheld tail.
func TestRedactionCoversBothStreams(t *testing.T) {
	t.Parallel()
	//nolint:gosec // synthetic value; the point is that it is opaque.
	const token = "opaque-secret-from-config-77123"
	res, err := quietRunner().Run(t.Context(), Command{
		Path:     testExe(t),
		Args:     []string{"leaked", token},
		ExtraEnv: map[string]string{helperModeEnv: "emit", "SONAR_TOKEN": token},
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	const want = "leaked [REDACTED:SONAR_TOKEN]"
	if res.Stdout != want {
		t.Errorf("Stdout = %q, want %q", res.Stdout, want)
	}
	if res.Stderr != want {
		t.Errorf("Stderr = %q, want %q", res.Stderr, want)
	}
	if got := strings.Join(res.Args, " "); strings.Contains(got, token) {
		t.Errorf("token survived into Result.Args: %q", got)
	}
}

func TestRunNonZeroExit(t *testing.T) {
	t.Parallel()
	res, err := quietRunner().Run(t.Context(), Command{
		Path:     testExe(t),
		Args:     []string{"3"},
		ExtraEnv: helperEnvFor("exit", ""),
		Timeout:  30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("error is %T (%v), want *ExitError", err, err)
	}
	if ee.Code != 3 || res.ExitCode != 3 {
		t.Errorf("ExitError.Code = %d, Result.ExitCode = %d, want 3", ee.Code, res.ExitCode)
	}
	if errors.Is(err, ErrToolchainMissing) {
		t.Error("a non-zero exit must not report as a missing toolchain")
	}
	if !strings.Contains(res.Stderr, "helper failing on purpose") {
		t.Errorf("stderr not captured: %q", res.Stderr)
	}
}

// TestToolchainMissingIsDistinguishable is the acceptance test for
// requirement 8. A later node has to tell "there is no test runner installed"
// apart from "the tests failed".
func TestToolchainMissingIsDistinguishable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		wantIs  []error
		wantNot []error
	}{
		{
			name:   "not on PATH",
			path:   "belay-definitely-not-a-real-binary-9f3c",
			wantIs: []error{ErrToolchainMissing, osexec.ErrNotFound},
		},
		{
			name:   "absolute path does not exist",
			path:   filepath.Join(dir, "missing-binary"),
			wantIs: []error{ErrToolchainMissing, os.ErrNotExist},
		},
		{
			name:   "exists but is not executable",
			path:   notExecutable,
			wantIs: []error{ErrToolchainMissing},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := quietRunner().Run(t.Context(), Command{Path: tc.path, Timeout: 30 * time.Second})
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, target := range tc.wantIs {
				if !errors.Is(err, target) {
					t.Errorf("errors.Is(err, %v) = false; err = %v", target, err)
				}
			}
			var te *ToolchainError
			if !errors.As(err, &te) {
				t.Errorf("error is %T, want *ToolchainError", err)
			}
			var ee *ExitError
			if errors.As(err, &ee) {
				t.Error("a missing toolchain must not surface as *ExitError")
			}
			if res.ExitCode != -1 {
				t.Errorf("ExitCode = %d, want -1 for a process that never ran", res.ExitCode)
			}
		})
	}
}

// TestTimeoutKillsStubbornChild proves SIGKILL escalation: the child ignores
// SIGTERM, so only the second signal can end it.
func TestTimeoutKillsStubbornChild(t *testing.T) {
	t.Parallel()
	start := time.Now()
	res, err := quietRunner().Run(t.Context(), Command{
		Path:     testExe(t),
		ExtraEnv: helperEnvFor("stall", ""),
		Timeout:  700 * time.Millisecond,
		Grace:    200 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if !res.TimedOut {
		t.Error("Result.TimedOut = false, want true")
	}
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("error is %T (%v), want *TimeoutError", err, err)
	}
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout error does not unwrap to both sentinels: %v", err)
	}
	if errors.Is(err, ErrToolchainMissing) {
		t.Error("a timeout must not report as a missing toolchain")
	}
	if want := 128 + int(9); res.ExitCode != want {
		t.Errorf("ExitCode = %d, want %d (SIGKILL)", res.ExitCode, want)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %s; the grace period escalation did not fire", elapsed)
	}
}

// TestTimeoutKillsProcessGroup is the acceptance test for requirement 5. The
// child spawns a grandchild that ignores SIGTERM and holds the stdout pipe,
// which is the shape of an agent CLI's own subprocesses. Killing only the
// direct child would leave the grandchild running: a resource leak, and with a
// metered agent CLI, a cost leak.
func TestTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	res, err := quietRunner().Run(t.Context(), Command{
		Path:     testExe(t),
		Args:     []string{pidFile},
		ExtraEnv: helperEnvFor("grandchild", ""),
		Timeout:  4 * time.Second,
		Grace:    300 * time.Millisecond,
	})
	if !res.TimedOut {
		t.Fatalf("Result.TimedOut = false (err %v, stderr %q)", err, res.Stderr)
	}

	//nolint:gosec // pidFile is a path this test built under t.TempDir().
	raw, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("grandchild never recorded its pid (%v); stderr: %q", readErr, res.Stderr)
	}
	gpid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatalf("bad pid file %q: %v", raw, convErr)
	}
	if gpid == os.Getpid() {
		t.Fatalf("pid file names the test process itself")
	}
	if !waitGone(gpid, 5*time.Second) {
		// Do not leave a stray process behind for the rest of the suite.
		_ = syscallKill(gpid)
		t.Fatalf("grandchild %d survived the timeout: the process group was not killed", gpid)
	}
}

func TestContextCancelStopsCommand(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	res, err := quietRunner().Run(ctx, Command{
		Path:     testExe(t),
		ExtraEnv: helperEnvFor("stall", ""),
		Timeout:  30 * time.Second,
		Grace:    200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected an error after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if res.TimedOut {
		t.Error("TimedOut = true, but the deadline never elapsed")
	}
	if errors.Is(err, ErrTimeout) {
		t.Error("cancellation must not be reported as a timeout")
	}
}

// TestOutputCapTruncates covers requirement 6 at the cap boundary.
func TestOutputCapTruncates(t *testing.T) {
	t.Parallel()
	const lineLen = 64
	tests := []struct {
		name      string
		lines     int
		cap       int64
		wantTrunc bool
	}{
		{"under the cap", 4, lineLen * 8, false},
		{"exactly at the cap", 8, lineLen * 8, false},
		{"one line over the cap", 9, lineLen * 8, true},
		{"far over the cap", 4096, lineLen * 8, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := quietRunner().Run(t.Context(), Command{
				Path:      testExe(t),
				Args:      []string{strconv.Itoa(tc.lines), strconv.Itoa(tc.lines)},
				ExtraEnv:  helperEnvFor("emitbytes", ""),
				MaxOutput: tc.cap,
				Timeout:   30 * time.Second,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Truncated != tc.wantTrunc {
				t.Errorf("Truncated = %v, want %v (stdout %d bytes)", res.Truncated, tc.wantTrunc, len(res.Stdout))
			}
			body, marker, found := strings.Cut(res.Stdout, "\n[belay: output truncated")
			if found != tc.wantTrunc {
				t.Errorf("truncation marker present = %v, want %v", found, tc.wantTrunc)
			}
			if int64(len(body)) > tc.cap {
				t.Errorf("captured %d bytes before the marker, cap is %d", len(body), tc.cap)
			}
			if tc.wantTrunc {
				if !strings.Contains(marker, "dropped") {
					t.Errorf("truncation marker does not report the dropped count: %q", marker)
				}
				if res.Stderr == "" || !strings.Contains(res.Stderr, "[belay: output truncated") {
					t.Errorf("stderr was not capped independently: %q", res.Stderr)
				}
			}
		})
	}
}

// TestOutputCapBoundsMemoryOnRunawayProcess proves a process that writes far
// more than the cap cannot grow the captured result without bound.
func TestOutputCapBoundsMemoryOnRunawayProcess(t *testing.T) {
	t.Parallel()
	const cap64 = 64 << 10
	res, err := quietRunner().Run(t.Context(), Command{
		Path:      testExe(t),
		Args:      []string{"16384", "0"}, // 1 MiB of stdout against a 64 KiB cap
		ExtraEnv:  helperEnvFor("emitbytes", ""),
		MaxOutput: cap64,
		Timeout:   60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Truncated {
		t.Error("Truncated = false on a runaway process")
	}
	if len(res.Stdout) > cap64+256 {
		t.Errorf("captured %d bytes, cap is %d plus the marker", len(res.Stdout), cap64)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d; the child must still run to completion", res.ExitCode)
	}
}

func TestRunValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  Command
		want error
	}{
		{"empty path", Command{}, ErrEmptyCommand},
		{"whitespace path", Command{Path: "   "}, ErrEmptyCommand},
		{"null in path", Command{Path: "a\x00b"}, ErrNullByte},
		{"null in argument", Command{Path: "true", Args: []string{"a\x00b"}}, ErrNullByte},
		{"bad env name", Command{Path: "true", EnvAllow: []string{"A=B"}}, ErrInvalidEnv},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := quietRunner().Run(t.Context(), tc.cmd)
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
			if res.ExitCode != -1 {
				t.Errorf("ExitCode = %d, want -1", res.ExitCode)
			}
		})
	}
}

func TestRunStdinIsForwarded(t *testing.T) {
	t.Parallel()
	res, err := quietRunner().Run(t.Context(), Command{
		Path:     testExe(t),
		Args:     []string{"from-args"},
		Stdin:    strings.NewReader("ignored by the helper"),
		ExtraEnv: helperEnvFor("args", ""),
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if diff := cmp.Diff([]string{"from-args"}, argLines(res.Stdout)); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestRunUsesWorkingDirectory(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	res, err := quietRunner().Run(t.Context(), Command{
		Path:     testExe(t),
		Dir:      dir,
		ExtraEnv: helperEnvFor("pwd", ""),
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "pwd:"+dir {
		t.Errorf("child working directory = %q, want %q", got, "pwd:"+dir)
	}
}

func TestRunnerLimitsFallBack(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		runner      Runner
		cmd         Command
		wantTimeout time.Duration
		wantGrace   time.Duration
		wantMax     int64
	}{
		{"all defaults", Runner{}, Command{}, DefaultTimeout, DefaultGrace, DefaultMaxOutput},
		{"runner overrides", Runner{Timeout: time.Minute, Grace: time.Second, MaxOutput: 10}, Command{}, time.Minute, time.Second, 10},
		{
			name:        "command overrides runner",
			runner:      Runner{Timeout: time.Minute, Grace: time.Second, MaxOutput: 10},
			cmd:         Command{Timeout: 2 * time.Minute, Grace: 2 * time.Second, MaxOutput: 20},
			wantTimeout: 2 * time.Minute, wantGrace: 2 * time.Second, wantMax: 20,
		},
		{"negative cap means unlimited", Runner{}, Command{MaxOutput: -1}, DefaultTimeout, DefaultGrace, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotTimeout, gotGrace, gotMax := tc.runner.limits(tc.cmd)
			if gotTimeout != tc.wantTimeout || gotGrace != tc.wantGrace || gotMax != tc.wantMax {
				t.Errorf("limits = (%v, %v, %d), want (%v, %v, %d)",
					gotTimeout, gotGrace, gotMax, tc.wantTimeout, tc.wantGrace, tc.wantMax)
			}
		})
	}
}

func TestErrorMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"exit", &ExitError{Args: []string{"go", "test"}, Code: 1, Stderr: "FAIL\n"}, `"go" "test" exited with status 1: FAIL`},
		{"timeout", &TimeoutError{Args: []string{"claude"}, Timeout: time.Second}, `"claude" timed out after 1s`},
		{"toolchain", &ToolchainError{Tool: "pytest", Err: osexec.ErrNotFound, Detail: "not found"}, `"pytest": not found`},
		{"start", &StartError{Tool: "go", Err: fmt.Errorf("boom"), Detail: "boom"}, `cannot start "go": boom`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Error(); !strings.Contains(got, tc.want) {
				t.Errorf("Error() = %q, want it to contain %q", got, tc.want)
			}
			if !strings.HasPrefix(tc.err.Error(), "belay/exec: ") {
				t.Errorf("Error() = %q, want a package prefix", tc.err.Error())
			}
		})
	}
}

func syscallKill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
