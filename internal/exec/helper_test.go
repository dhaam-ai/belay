//go:build unix

package exec

import (
	"bufio"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests in this package need real child processes but must run with no
// external toolchain, no Docker, and no network. They therefore re-execute the
// test binary itself as the child: TestMain dispatches to helperMain whenever
// helperModeEnv is set in the environment. Because the package under test
// drops every parent variable it was not asked for, the mode reaches the child
// only through Command.ExtraEnv, which is itself part of what is being tested.
const (
	helperModeEnv = "BELAY_EXEC_HELPER"
	helperPIDEnv  = "BELAY_TEST_PIDDIR"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnv); mode != "" {
		helperMain(mode, os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

func helperMain(mode string, args []string) {
	// Recorded before anything else so that every helper invocation is
	// counted, which is how the injection tests prove only one process ran.
	if dir := os.Getenv(helperPIDEnv); dir != "" {
		//nolint:gosec // dir is a t.TempDir() the test itself chose.
		_ = os.WriteFile(filepath.Join(dir, "pid-"+strconv.Itoa(os.Getpid())),
			[]byte(mode+"\n"), 0o600)
	}

	switch mode {
	case "args":
		for _, a := range args {
			fmt.Printf("arg:%s\n", a)
		}
	case "env":
		env := os.Environ()
		slices.Sort(env)
		for _, kv := range env {
			fmt.Printf("env:%s\n", kv)
		}
	case "echoenv":
		for _, name := range args {
			fmt.Printf("%s=%s\n", name, os.Getenv(name))
		}
	case "emit":
		// No trailing newline, and shorter than the redactor hold-back, so
		// the parent only sees this if Close flushes the withheld tail.
		_, _ = fmt.Fprint(os.Stdout, strings.Join(args, " "))
		_, _ = fmt.Fprint(os.Stderr, strings.Join(args, " "))
	case "emitbytes":
		// emitbytes <stdoutLines> <stderrLines>: fixed 64-byte lines, written
		// through a buffer so the parent sees pipe-sized chunks rather than
		// one write syscall per line.
		line := strings.Repeat("x", 63) + "\n"
		emitLines(os.Stdout, line, arg(args, 0))
		emitLines(os.Stderr, line, arg(args, 1))
	case "pwd":
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "getwd:", err)
			os.Exit(2)
		}
		fmt.Printf("pwd:%s\n", wd)
	case "exit":
		code, err := strconv.Atoi(arg(args, 0))
		if err != nil {
			code = 99
		}
		fmt.Fprintln(os.Stderr, "helper failing on purpose")
		os.Exit(code)
	case "stall":
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		time.Sleep(10 * time.Minute)
	case "grandchild":
		helperGrandchild(arg(args, 0))
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

// helperGrandchild spawns a second process that ignores SIGTERM, records its
// pid, and then stalls itself. The grandchild inherits both the process group
// and the stdout pipe, which is the exact shape of a leaked agent CLI subtree.
func helperGrandchild(pidFile string) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "executable:", err)
		os.Exit(2)
	}
	// This is the point of the test: the child spawns its own child, so the
	// runner has a process group to kill rather than a single process.
	//nolint:gosec // exe is this test binary, from os.Executable.
	sub := osexec.Command(exe, "stall")
	sub.Env = []string{helperModeEnv + "=stall"}
	sub.Stdout = os.Stdout
	sub.Stderr = os.Stderr
	if err := sub.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "spawn:", err)
		os.Exit(2)
	}
	tmp := pidFile + ".tmp"
	//nolint:gosec // pidFile is a t.TempDir() path passed by the test.
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(sub.Process.Pid)), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write pid:", err)
		os.Exit(2)
	}
	//nolint:gosec // pidFile is a t.TempDir() path passed by the test.
	if err := os.Rename(tmp, pidFile); err != nil {
		fmt.Fprintln(os.Stderr, "rename pid:", err)
		os.Exit(2)
	}
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	time.Sleep(10 * time.Minute)
}

func emitLines(w io.Writer, line, count string) {
	n, err := strconv.Atoi(count)
	if err != nil || n <= 0 {
		return
	}
	bw := bufio.NewWriterSize(w, 32<<10)
	for range n {
		if _, err := bw.WriteString(line); err != nil {
			return
		}
	}
	_ = bw.Flush()
}

func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

// testExe returns the absolute path of the test binary, used as the child.
func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// helperEnv returns the ExtraEnv that puts a child into the given helper mode.
func helperEnvFor(mode, pidDir string) map[string]string {
	env := map[string]string{helperModeEnv: mode}
	if pidDir != "" {
		env[helperPIDEnv] = pidDir
	}
	return env
}

// countPIDFiles reports how many helper processes recorded themselves.
func countPIDFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read pid dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pid-") {
			n++
		}
	}
	return n
}

// argLines extracts the values printed by the "args" helper mode.
func argLines(stdout string) []string {
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		if v, ok := strings.CutPrefix(line, "arg:"); ok {
			out = append(out, v)
		}
	}
	return out
}

// processAlive reports whether pid still exists.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// waitGone polls until pid disappears or the deadline passes.
func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !processAlive(pid)
}
