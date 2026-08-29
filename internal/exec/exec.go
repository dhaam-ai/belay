//go:build unix

// Package exec runs the external processes belay orchestrates, and is the only
// place in belay that starts one.
//
// Every tool a run touches goes through here: the coding-agent CLI, go test,
// golangci-lint, eslint, ruff, a containerised scanner. Their stdout and
// stderr are captured, written to .belay/runs/<id>/nodes/<seq>-<name>/, and
// fed back into later prompts, which makes this package a security boundary
// rather than a convenience wrapper. Three properties are structural, not
// advisory:
//
//   - No shell, ever. Commands are an argv slice handed to os/exec. A
//     user-supplied command string is turned into argv by [Tokenize], which
//     performs quote removal and nothing else: no expansion, no globbing, no
//     substitution, no operators. A metacharacter in configuration is an
//     argument, and cannot become a second process.
//   - Deny-by-default environment. A child sees [BaseEnvNames] plus what the
//     caller named, and nothing else of the parent's. See env.go.
//   - Redaction on capture. Captured output, the argv recorded in [Result],
//     and the text of every error this package returns pass through a
//     [Redactor] seeded with the values of the secret-bearing variables the
//     child was given. See redact.go.
//
// # Platform
//
// Timeout handling kills the child's whole process group, because an agent CLI
// spawns children of its own and a leaked group is both a resource leak and a
// runaway-cost risk. That requires Setpgid, so this package targets unix
// (macOS, Linux). It does not build on Windows.
//
// # Limits
//
// These are the gaps a reviewer should know about rather than rediscover.
//
//   - Redaction matches a secret's literal bytes. A child that base64s,
//     URL-encodes, or otherwise transforms a credential before printing it
//     defeats the seeded values; only the built-in patterns still apply.
//   - Process-group termination happens on the timeout and cancellation paths,
//     where the child has not yet been reaped and its group id is still
//     unambiguously ours. A child that exits cleanly while leaving descendants
//     behind is not chased: after Wait reaps the child, the pid may be reused,
//     and signalling a recycled group is worse than the leak it would prevent.
//     When descendants outlive the child and still hold its pipes, Run logs it
//     and closes the descriptors after the grace period.
//   - Redaction runs at roughly 20 MB/s on pipe-sized chunks, so it is capped
//     work rather than free work; output past MaxOutput is drained without
//     being scanned, since it cannot reach a Result, the journal, or a prompt.
package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	osexec "os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Defaults applied when a Command or Runner leaves a limit unset.
const (
	// DefaultTimeout bounds a single command. There is no unbounded mode.
	DefaultTimeout = 15 * time.Minute
	// DefaultGrace is how long a child gets between SIGTERM and SIGKILL.
	DefaultGrace = 5 * time.Second
	// DefaultMaxOutput caps captured stdout and stderr independently.
	DefaultMaxOutput int64 = 1 << 20
)

// Sentinel errors. Match them with errors.Is.
var (
	// ErrToolchainMissing reports that the binary could not be executed at
	// all. It is deliberately distinct from a non-zero exit: a missing go or
	// pytest is an environment problem, and a caller that reports it as a
	// failing test suite sends the graph down the wrong branch.
	ErrToolchainMissing = errors.New("toolchain binary not available")
	// ErrTimeout reports that the command exceeded its deadline and was killed.
	ErrTimeout = errors.New("command timed out")
	// ErrEmptyCommand reports a command with no program to run.
	ErrEmptyCommand = errors.New("empty command")
	// ErrUnterminatedQuote reports an unbalanced quote in a command string.
	ErrUnterminatedQuote = errors.New("unterminated quote")
	// ErrNullByte reports a NUL in an argument, name, or value. A NUL would be
	// silently truncated by execve, turning a checked string into a different
	// one, so it is rejected instead.
	ErrNullByte = errors.New("null byte")
	// ErrInvalidEnv reports a malformed environment variable name.
	ErrInvalidEnv = errors.New("invalid environment variable")
)

// ToolchainError reports that a binary could not be started because it is
// missing or not executable.
//
// It unwraps to both ErrToolchainMissing and the underlying cause, so
// errors.Is(err, ErrToolchainMissing) and errors.Is(err, exec.ErrNotFound)
// both hold.
type ToolchainError struct {
	// Tool is the redacted program name that could not be run.
	Tool string
	// Err is the underlying cause, such as exec.ErrNotFound.
	Err error
	// Detail is the redacted rendering of Err.
	Detail string
}

func (e *ToolchainError) Error() string {
	return fmt.Sprintf("belay/exec: %s: %q: %s", ErrToolchainMissing.Error(), e.Tool, e.Detail)
}

// Unwrap reports both the sentinel and the underlying cause.
func (e *ToolchainError) Unwrap() []error { return []error{ErrToolchainMissing, e.Err} }

// StartError reports a failure to start a process that is not a missing
// toolchain, such as an unusable working directory.
type StartError struct {
	// Tool is the redacted program name.
	Tool string
	// Err is the underlying cause.
	Err error
	// Detail is the redacted rendering of Err.
	Detail string
}

func (e *StartError) Error() string {
	return fmt.Sprintf("belay/exec: cannot start %q: %s", e.Tool, e.Detail)
}

// Unwrap reports the underlying cause.
func (e *StartError) Unwrap() error { return e.Err }

// ExitError reports that the program ran and exited non-zero.
//
// This is the "the tool did its job and disagreed" case: a failing test suite,
// a linter with findings. It never satisfies errors.Is(err, ErrToolchainMissing).
type ExitError struct {
	// Args is the redacted argv, program first.
	Args []string
	// Code is the exit status, or 128+signal when the process was signalled.
	Code int
	// Stderr is the redacted tail of the child's standard error.
	Stderr string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("belay/exec: %s exited with status %d", quoteArgv(e.Args), e.Code)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + lastLines(s, 3)
	}
	return msg
}

// TimeoutError reports that a command exceeded its deadline and its process
// group was terminated.
type TimeoutError struct {
	// Args is the redacted argv, program first.
	Args []string
	// Timeout is the deadline that elapsed.
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("belay/exec: %s timed out after %s", quoteArgv(e.Args), e.Timeout)
}

// Unwrap reports both the sentinel and context.DeadlineExceeded.
func (e *TimeoutError) Unwrap() []error { return []error{ErrTimeout, context.DeadlineExceeded} }

// Command describes one process to run.
//
// The zero value is not runnable: Path is required. Everything else has a safe
// default, and every default is restrictive.
type Command struct {
	// Path is the program to run. It is resolved through PATH when it contains
	// no separator. It is never interpreted by a shell.
	Path string
	// Args are the arguments after the program name.
	Args []string
	// Dir is the working directory. Empty means the parent's.
	Dir string
	// EnvAllow names parent environment variables to pass through, in addition
	// to BaseEnvNames. Everything else in the parent environment is dropped.
	EnvAllow []string
	// SecretEnv names parent variables that are passed through and whose
	// values seed the redactor. Names matching IsSecretName are treated this
	// way automatically; SecretEnv is for the ones that do not.
	SecretEnv []string
	// ExtraEnv sets variables explicitly, overriding the parent's value.
	// Values of secret-looking names seed the redactor.
	ExtraEnv map[string]string
	// Secrets seeds the redactor with values that never appear in the
	// environment, such as a token read from configuration.
	Secrets []Secret
	// Stdin is the child's standard input. It is never captured or logged.
	Stdin io.Reader
	// Timeout bounds this command. Zero uses the Runner default.
	Timeout time.Duration
	// Grace is the delay between SIGTERM and SIGKILL. Zero uses the Runner
	// default.
	Grace time.Duration
	// MaxOutput caps captured stdout and stderr independently, in bytes. Zero
	// uses the Runner default. Negative means no cap, which is only ever
	// appropriate in a test.
	MaxOutput int64
}

func (c Command) validate() error {
	if strings.TrimSpace(c.Path) == "" {
		return ErrEmptyCommand
	}
	if strings.ContainsRune(c.Path, 0) {
		return fmt.Errorf("%w: in program name", ErrNullByte)
	}
	for i, a := range c.Args {
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("%w: in argument %d", ErrNullByte, i)
		}
	}
	return nil
}

// Result is the outcome of one command, safe to persist to the run journal.
//
// Every string field has passed through the redactor, including Args: a token
// handed to a scanner as -Dsonar.token=... must not survive into the journal
// just because it was on the command line rather than in the output.
type Result struct {
	// Args is the redacted argv, program first.
	Args []string
	// ExitCode is the exit status, 128+signal when signalled, or -1 when the
	// process never ran.
	ExitCode int
	// Stdout is the redacted, capped standard output.
	Stdout string
	// Stderr is the redacted, capped standard error.
	Stderr string
	// Duration is the wall-clock time from start to reap.
	Duration time.Duration
	// TimedOut reports that the deadline elapsed and the process group was
	// terminated.
	TimedOut bool
	// Truncated reports that stdout or stderr hit MaxOutput and was cut.
	Truncated bool
}

// Runner executes commands with shared defaults.
//
// The zero value is usable; New returns one with an explicit logger.
type Runner struct {
	// Logger receives structured events. Nil uses slog.Default. Argv is logged
	// redacted; environment values are never logged.
	Logger *slog.Logger
	// Environ snapshots the parent environment. Nil uses os.Environ.
	Environ func() []string
	// Timeout is the default Command timeout. Zero uses DefaultTimeout.
	Timeout time.Duration
	// Grace is the default SIGTERM-to-SIGKILL delay. Zero uses DefaultGrace.
	Grace time.Duration
	// MaxOutput is the default per-stream capture cap. Zero uses
	// DefaultMaxOutput.
	MaxOutput int64
}

// New returns a Runner using the given logger, or slog.Default when nil.
func New(logger *slog.Logger) *Runner {
	return &Runner{Logger: logger}
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *Runner) environ() []string {
	if r.Environ != nil {
		return r.Environ()
	}
	return os.Environ()
}

func (r *Runner) limits(c Command) (timeout, grace time.Duration, maxOut int64) {
	timeout = firstPositive(c.Timeout, r.Timeout, DefaultTimeout)
	grace = firstPositive(c.Grace, r.Grace, DefaultGrace)
	switch {
	case c.MaxOutput != 0:
		maxOut = c.MaxOutput
	case r.MaxOutput != 0:
		maxOut = r.MaxOutput
	default:
		maxOut = DefaultMaxOutput
	}
	return timeout, grace, maxOut
}

// Run executes c and returns its redacted result.
//
// A Result is returned even on failure, so a caller can journal what happened
// before inspecting the error. The error is:
//
//   - *ToolchainError (errors.Is ErrToolchainMissing) when the binary is
//     missing or not executable,
//   - *TimeoutError (errors.Is ErrTimeout) when the deadline elapsed,
//   - ctx.Err() when the caller's context was cancelled,
//   - *ExitError when the program ran and exited non-zero,
//   - nil on a clean zero exit.
func (r *Runner) Run(ctx context.Context, c Command) (Result, error) {
	if err := c.validate(); err != nil {
		return Result{ExitCode: -1}, err
	}
	env, envSecrets, err := buildEnv(r.environ(), c)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	red := NewRedactor(append(slices.Clone(envSecrets), c.Secrets...)...)

	argv := append([]string{c.Path}, c.Args...)
	res := Result{Args: redactArgv(red, argv), ExitCode: -1}

	timeout, grace, maxOut := r.limits(c)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// #nosec G204 -- constructing a subprocess from variable argv is this
	// package's entire purpose. Safety comes from never invoking a shell: Path
	// and Args are a pre-tokenized argv slice, so metacharacters are inert.
	cmd := osexec.Command(c.Path, c.Args...)
	if cmd.Err != nil {
		return res, classifyStart(red, res.Args[0], cmd.Err)
	}
	cmd.Dir = c.Dir
	cmd.Env = env
	cmd.Stdin = c.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Backstop: if a surviving grandchild still holds the pipes after the
	// child is reaped, close them rather than block Wait forever.
	cmd.WaitDelay = grace

	outCap := newCapture(maxOut)
	errCap := newCapture(maxOut)
	outW := red.Writer(outCap)
	errW := red.Writer(errCap)
	cmd.Stdout = &sink{cap: outCap, red: outW}
	cmd.Stderr = &sink{cap: errCap, red: errW}

	log := r.logger()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		res.Duration = time.Since(start)
		return res, classifyStart(red, res.Args[0], err)
	}
	pid := cmd.Process.Pid
	log.LogAttrs(ctx, slog.LevelDebug, "belay/exec: started",
		slog.String("program", res.Args[0]), slog.Int("pid", pid),
		slog.Duration("timeout", timeout))

	var (
		timedOut atomic.Bool
		exited   atomic.Bool
		watcher  sync.WaitGroup
	)
	done := make(chan struct{})
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		select {
		case <-done:
		case <-runCtx.Done():
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				timedOut.Store(true)
			}
			terminateGroup(pid, grace, done, &exited)
			log.LogAttrs(ctx, slog.LevelWarn, "belay/exec: terminated process group",
				slog.String("program", res.Args[0]), slog.Int("pgid", pid),
				slog.Bool("timeout", timedOut.Load()))
		}
	}()

	waitErr := cmd.Wait()
	exited.Store(true)
	close(done)
	watcher.Wait()

	res.Duration = time.Since(start)
	_ = outW.Close()
	_ = errW.Close()
	res.Stdout, res.Stderr = outCap.String(), errCap.String()
	res.Truncated = outCap.truncated() || errCap.truncated()
	res.TimedOut = timedOut.Load()
	res.ExitCode = exitCode(cmd.ProcessState)

	log.LogAttrs(ctx, slog.LevelDebug, "belay/exec: finished",
		slog.String("program", res.Args[0]), slog.Int("exit_code", res.ExitCode),
		slog.Duration("duration", res.Duration), slog.Bool("timed_out", res.TimedOut),
		slog.Bool("truncated", res.Truncated))

	switch {
	case res.TimedOut:
		return res, &TimeoutError{Args: res.Args, Timeout: timeout}
	case ctx.Err() != nil:
		return res, fmt.Errorf("belay/exec: %s: %w", quoteArgv(res.Args), ctx.Err())
	case waitErr != nil && errors.Is(waitErr, osexec.ErrWaitDelay):
		// The process exited but a descendant held the pipes open. Output may
		// be short; the exit status is still authoritative.
		log.LogAttrs(ctx, slog.LevelWarn, "belay/exec: output pipes force-closed",
			slog.String("program", res.Args[0]))
	case waitErr != nil:
		var ee *osexec.ExitError
		if !errors.As(waitErr, &ee) {
			return res, &StartError{Tool: res.Args[0], Err: waitErr, Detail: red.Redact(waitErr.Error())}
		}
	}
	if res.ExitCode != 0 {
		return res, &ExitError{Args: res.Args, Code: res.ExitCode, Stderr: res.Stderr}
	}
	return res, nil
}

// terminateGroup escalates SIGTERM to SIGKILL across the child's whole process
// group.
//
// The negative pid targets the group created by Setpgid, so a coding agent's
// own children die with it. Killing only the direct child would leave those
// running: a resource leak, and with a metered agent CLI, a cost leak.
func terminateGroup(pid int, grace time.Duration, done <-chan struct{}, exited *atomic.Bool) {
	signalGroup(pid, syscall.SIGTERM, exited)
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C:
	}
	signalGroup(pid, syscall.SIGKILL, exited)
}

func signalGroup(pid int, sig syscall.Signal, exited *atomic.Bool) {
	if pid <= 0 || exited.Load() {
		return
	}
	if err := syscall.Kill(-pid, sig); err == nil {
		return
	}
	// The group may not exist if Setpgid was refused; fall back to the child.
	_ = syscall.Kill(pid, sig)
}

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

func classifyStart(red *Redactor, tool string, err error) error {
	detail := red.Redact(err.Error())
	if errors.Is(err, osexec.ErrNotFound) ||
		errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, syscall.ENOEXEC) {
		return &ToolchainError{Tool: tool, Err: err, Detail: detail}
	}
	return &StartError{Tool: tool, Err: err, Detail: detail}
}

func redactArgv(red *Redactor, argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = red.Redact(a)
	}
	return out
}

func firstPositive(vals ...time.Duration) time.Duration {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

func quoteArgv(argv []string) string {
	if len(argv) == 0 {
		return `""`
	}
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = fmt.Sprintf("%q", a)
	}
	return strings.Join(parts, " ")
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// sink is the child's end of one captured stream.
//
// It drains the pipe unconditionally, because a child that blocks writing to a
// full pipe never exits, but it stops paying for redaction once the capture is
// full. Those bytes cannot reach the Result, the journal, or a prompt, so
// scanning them buys nothing and would let a runaway process turn a bounded
// capture into unbounded CPU.
type sink struct {
	cap *capture
	red *RedactWriter
}

func (s *sink) Write(p []byte) (int, error) {
	if s.cap.saturated() {
		s.cap.drop(len(p))
		return len(p), nil
	}
	return s.red.Write(p)
}

// capture accumulates output up to a byte cap, counting what it drops.
//
// Reads from the child's pipes continue after the cap is reached: a child that
// blocks on a full pipe never exits, so the bytes are consumed and discarded
// rather than left unread. Memory and the run directory stay bounded.
type capture struct {
	mu      sync.Mutex
	buf     []byte
	limit   int64
	written int64
	dropped int64
}

func newCapture(limit int64) *capture {
	return &capture{limit: limit}
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit < 0 {
		c.buf = append(c.buf, p...)
		c.written += int64(len(p))
		return len(p), nil
	}
	room := c.limit - c.written
	if room > int64(len(p)) {
		room = int64(len(p))
	}
	if room > 0 {
		c.buf = append(c.buf, p[:room]...)
		c.written += room
	} else {
		room = 0
	}
	c.dropped += int64(len(p)) - room
	return len(p), nil
}

func (c *capture) saturated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limit >= 0 && c.written >= c.limit
}

func (c *capture) drop(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropped += int64(n)
}

func (c *capture) truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped > 0
}

// String returns the captured bytes, with an explicit marker appended when
// output was dropped so a reader never mistakes a cut stream for a short one.
func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped == 0 {
		return string(c.buf)
	}
	return fmt.Sprintf("%s\n[belay: output truncated at %d bytes, %d dropped]\n",
		c.buf, c.limit, c.dropped)
}

// Tokenize splits a user-supplied command string into an argv slice.
//
// It is a quote parser, not a shell. It removes quoting and backslash escapes
// and does nothing else, so every construct that would make a shell start a
// second process survives as ordinary argument text:
//
//	echo $(whoami)   -> ["echo", "$(whoami)"]
//	echo `id`        -> ["echo", "`id`"]
//	echo $HOME       -> ["echo", "$HOME"]
//	a && b           -> ["a", "&&", "b"]
//	a; b             -> ["a;", "b"]
//	a | b            -> ["a", "|", "b"]
//	ls *.go          -> ["ls", "*.go"]
//
// There is no expansion, globbing, command substitution, variable
// interpolation, redirection, operator handling, or comment handling. Quoting
// rules: single quotes are literal to the closing quote; inside double quotes
// a backslash escapes only " and \, and is otherwise literal; outside quotes a
// backslash escapes the next character, including a space. An empty quoted
// string is a real empty argument.
//
// It returns ErrUnterminatedQuote for an unbalanced quote or a trailing
// backslash, and ErrNullByte for a NUL.
func Tokenize(line string) ([]string, error) {
	if strings.ContainsRune(line, 0) {
		return nil, fmt.Errorf("%w: in command string", ErrNullByte)
	}
	const (
		bare = iota
		single
		double
	)
	var (
		out   []string
		cur   strings.Builder
		state = bare
		open  bool
		esc   bool
	)
	for _, ch := range line {
		if esc {
			if state == double && ch != '"' && ch != '\\' {
				cur.WriteRune('\\')
			}
			cur.WriteRune(ch)
			esc = false
			continue
		}
		switch state {
		case single:
			if ch == '\'' {
				state = bare
				continue
			}
			cur.WriteRune(ch)
		case double:
			switch ch {
			case '"':
				state = bare
			case '\\':
				esc = true
			default:
				cur.WriteRune(ch)
			}
		default:
			switch {
			case ch == '\'':
				state, open = single, true
			case ch == '"':
				state, open = double, true
			case ch == '\\':
				esc, open = true, true
			// #nosec G115 -- the ch < 0x80 guard proves the conversion is lossless.
			case ch < 0x80 && isSpaceByte(byte(ch)):
				if open {
					out = append(out, cur.String())
					cur.Reset()
					open = false
				}
			default:
				cur.WriteRune(ch)
				open = true
			}
		}
	}
	if esc {
		return nil, fmt.Errorf("%w: trailing backslash", ErrUnterminatedQuote)
	}
	if state != bare {
		return nil, fmt.Errorf("%w: in %q", ErrUnterminatedQuote, line)
	}
	if open {
		out = append(out, cur.String())
	}
	return out, nil
}

// ParseCommandLine turns a configured command string such as test.custom_cmd
// into a Command whose argv came from Tokenize.
//
// The result still carries no environment allowlist; the caller adds one.
func ParseCommandLine(line string) (Command, error) {
	argv, err := Tokenize(line)
	if err != nil {
		return Command{}, err
	}
	if len(argv) == 0 {
		return Command{}, ErrEmptyCommand
	}
	return Command{Path: argv[0], Args: argv[1:]}, nil
}
