//go:build fixrate

package fixrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dhaam-ai/belay/internal/linter"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// This file gives a SWE-bench cell its workspace, from the instance's own
// official image, and gives the belay arm's test node somewhere to run.
//
// # The workspace is the image's /testbed, copied out
//
// Each record names its instance image (Task.Image), and the official
// harness evaluates a prediction by applying it inside that image at
// /testbed (swebench 5.0.2: run_evaluation.py's create_container, the
// record's image, CONTAINER_WORKDIR "/testbed"). So both arms start from that
// same tree: Provision copies /testbed out of the image into the cell's
// workspace, with its history, and the agent edits the copy on the host,
// with the tools belay's code node allows -- Read, Grep, Glob, Edit, Write, no
// shell. The cell's patch is measured from the copy's HEAD, which is what the
// harness will apply it to; that HEAD may sit above the record's base_commit
// -- SWE-bench's own image setup committed its installation on top of it, as
// "git commit --allow-empty -am SWE-bench" at v3.0.17 -- so base_commit is
// checked to be in the copy's history instead, and an image that does not
// hold it is refused: the arms would be working on a tree the record does not
// describe.
//
// # The belay arm's tests run inside the image
//
// belay's test node needs the repository's own environment, which only the
// image has. For a belay cell, Provision starts a container of the image
// with the workspace mounted at /testbed, and the test runner runs in it
// with docker exec. The control arm runs no tests, so it gets no container.
//
// That runner is the one part of the belay arm the harness supplies instead
// of belay. belay's shipped pytest runner runs a whole suite, which is the
// wrong tool for django's and sympy's own runners and hours of work on the
// rest. containerRunner runs only the test files the run changed -- the
// regression test the shared goal asks for, and any test it edited -- with
// the repository's own test command, and nothing of the dataset's: the
// FAIL_TO_PASS and PASS_TO_PASS tests are the answer key, and the control
// arm has no such help. A run that changed no test file fails the test step
// with a message that says so, which is the goal's own requirement held to.
// Every result names these adapters (Workspace.Adapters), so a reader knows
// what in the belay arm was not shipped belay.
//
// The quality gate is belay's shipped ruff adapter, run on the host against
// the workspace and scoped to the run's changed lines. Most SWE-bench
// repositories configure no ruff, and belay as shipped would refuse to start
// there (linter.Explain); the harness runs ruff anyway, with its default
// rules, which is what adding an empty [tool.ruff] table would do. The
// corpus counts those refusals so the report can say how many tasks belay
// could not have run as shipped.
//
// # What is not verified here
//
// No container has run on this machine; Docker is not installed where this
// was written. The environment prologue (/opt/miniconda3, the "testbed"
// conda environment) is taken from SWE-bench's own eval scripts at v3.0.17,
// and 5.0.2's images may differ; a wrong prologue fails every belay cell's
// first test step with exit 127, which the runner reports as an
// infrastructure failure and the corpus stops on. The scripted pilot (the
// live command's -mode replay) exists to find exactly that before money is
// spent.

// DefaultContainerPrologue sets up an instance container's environment
// before a test command runs: SWE-bench's own eval scripts at v3.0.17 open
// with these three steps (swebench/harness/test_spec/python.py,
// make_eval_script_list_py), and CONTAINER_ENV_NAME and CONTAINER_WORKDIR
// are still "testbed" and "/testbed" in 5.0.2.
const DefaultContainerPrologue = "source /opt/miniconda3/bin/activate && conda activate testbed && cd /testbed"

// defaultTestTimeout bounds one test step inside a container, matching the
// official harness's own default --timeout of 1800 seconds.
const defaultTestTimeout = 30 * time.Minute

// containerPlatform is the platform the official images are built for.
const containerPlatform = "linux/amd64"

// sweBenchTestCommands are the repositories whose tests do not run under a
// plain `pytest -rA`, with the command SWE-bench's own specs use for them
// (swebench/harness/constants/python.py at v3.0.17: TEST_DJANGO, TEST_SYMPY_VERBOSE,
// TEST_SPHINX). Every other repository gets defaultTestCommand. These are
// repository-level facts, the same for every instance, and carry nothing of
// any instance's answer.
var sweBenchTestCommands = map[string]string{
	"django/django":     "./tests/runtests.py --verbosity 2 --settings=test_sqlite --parallel 1",
	"sympy/sympy":       "bin/test -C --verbose",
	"sphinx-doc/sphinx": "tox --current-env -epy39 -v --",
}

// defaultTestCommand is SWE-bench's TEST_PYTEST at v3.0.17.
const defaultTestCommand = "pytest -rA"

// commander runs a program and returns what it printed and its exit status.
// err is only for a program that could not be run at all.
type commander func(ctx context.Context, name string, args ...string) (stdout, stderr string, exit int, err error)

// execRun is the real commander.
func execRun(ctx context.Context, name string, args ...string) (string, string, int, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // docker or git, with this file's own arguments
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), stderr.String(), 0, nil
	case errors.As(err, &exitErr):
		return stdout.String(), stderr.String(), exitErr.ExitCode(), nil
	default:
		return stdout.String(), stderr.String(), -1, err
	}
}

// ContainerProvisioner is the Provisioner for SWE-bench cells; see this
// file's header comment.
type ContainerProvisioner struct {
	// Docker is the docker CLI to run. Empty means "docker".
	Docker string
	// RunID scopes container names to one corpus. Required.
	RunID string
	// Prologue sets up the container's environment before a test command.
	// Empty means DefaultContainerPrologue.
	Prologue string
	// Commands overrides the test command per repository ("owner/name").
	Commands map[string]string
	// TestTimeout bounds one test step. Zero means 30 minutes.
	TestTimeout time.Duration

	run commander
}

var _ Provisioner = (*ContainerProvisioner)(nil)

// containerNameUnsafe matches what a docker container name may not hold.
var containerNameUnsafe = regexp.MustCompile(`[^a-z0-9_.-]+`)

// containerName is the cell's container, unique to the corpus and the cell.
func (p *ContainerProvisioner) containerName(c Cell) string {
	return containerNameUnsafe.ReplaceAllString(strings.ToLower("belay-fixrate-"+p.RunID+"-"+c.Task.ID+"-"+c.System()), "-")
}

func (p *ContainerProvisioner) docker() string {
	if p.Docker == "" {
		return "docker"
	}
	return p.Docker
}

func (p *ContainerProvisioner) commander() commander {
	if p.run == nil {
		return execRun
	}
	return p.run
}

// dockerDo runs one docker command and fails on a non-zero exit.
func (p *ContainerProvisioner) dockerDo(ctx context.Context, args ...string) (string, error) {
	out, errOut, exit, err := p.commander()(ctx, p.docker(), args...)
	if err != nil {
		return "", fmt.Errorf("fixrate: docker %s: %w", args[0], err)
	}
	if exit != 0 {
		return "", fmt.Errorf("fixrate: docker %s exited %d: %s", strings.Join(args, " "), exit, lastLines(errOut+out, 3))
	}
	return out, nil
}

// removeContainer removes the container name, whether it is running or not,
// and succeeds when there is none: some docker releases answer `rm --force`
// for a missing container with an error and exit 1.
func (p *ContainerProvisioner) removeContainer(ctx context.Context, name string) error {
	out, errOut, exit, err := p.commander()(ctx, p.docker(), "rm", "--force", name)
	switch {
	case err != nil:
		return fmt.Errorf("fixrate: docker rm: %w", err)
	case exit != 0 && !strings.Contains(errOut+out, "No such container"):
		return fmt.Errorf("fixrate: docker rm --force %s exited %d: %s", name, exit, lastLines(errOut+out, 3))
	default:
		return nil
	}
}

// Provision implements Provisioner; see this file's header comment.
func (p *ContainerProvisioner) Provision(ctx context.Context, cell Cell, dir string, fresh bool) (Workspace, error) {
	if p.RunID == "" {
		return Workspace{}, fmt.Errorf("%w: a ContainerProvisioner needs a RunID", errCorpusConfig)
	}
	if cell.Task.Image == "" {
		return Workspace{}, fmt.Errorf("fixrate: task %s names no instance image", cell.Task.ID)
	}
	ws := filepath.Join(dir, "workspace")
	if fresh {
		if err := p.Release(ctx, cell, dir); err != nil {
			return Workspace{}, err
		}
	}
	if !exists(ws) {
		if err := p.copyOut(ctx, cell, ws); err != nil {
			return Workspace{}, err
		}
	}
	base, err := p.base(ctx, cell, ws)
	if err != nil {
		return Workspace{}, err
	}
	w := Workspace{
		Dir:    ws,
		Base:   base,
		Linter: linter.NewRuff(),
		Adapters: "test node: the test files the run changed, run with the repository's own test command inside " +
			"the instance's official image (the harness's containerRunner, not shipped with belay); quality gate: " +
			"belay's shipped ruff adapter, scoped to the run's changed lines, with ruff's default rules where the " +
			"repository configures none",
	}
	if cell.Arm != ArmBelay {
		return w, nil
	}

	// A container a previous attempt left may still be running a test, so
	// it is replaced; the workspace it mounted is the host's, and stays.
	name := p.containerName(cell)
	if err := p.removeContainer(ctx, name); err != nil {
		return Workspace{}, err
	}
	if _, err := p.dockerDo(ctx, "run", "--detach", "--name", name, "--platform", containerPlatform,
		"--volume", ws+":/testbed", cell.Task.Image, "tail", "-f", "/dev/null"); err != nil {
		return Workspace{}, err
	}
	timeout := p.TestTimeout
	if timeout <= 0 {
		timeout = defaultTestTimeout
	}
	prologue := p.Prologue
	if prologue == "" {
		prologue = DefaultContainerPrologue
	}
	w.Runner = &containerRunner{
		docker: p.docker(), container: name, repo: cell.Task.Repo, prologue: prologue,
		commands: p.Commands, timeout: timeout, run: p.commander(),
	}
	return w, nil
}

// copyOut copies /testbed out of the cell's instance image into ws, pulling
// the image first if it is not there.
func (p *ContainerProvisioner) copyOut(ctx context.Context, cell Cell, ws string) error {
	image := cell.Task.Image
	if _, err := p.dockerDo(ctx, "image", "inspect", image); err != nil {
		if _, err := p.dockerDo(ctx, "pull", "--platform", containerPlatform, image); err != nil {
			return err
		}
	}
	tmp := p.containerName(cell) + "-copy"
	if err := p.removeContainer(ctx, tmp); err != nil {
		return err
	}
	if _, err := p.dockerDo(ctx, "create", "--name", tmp, "--platform", containerPlatform, image); err != nil {
		return err
	}
	_, cpErr := p.dockerDo(ctx, "cp", tmp+":/testbed", ws)
	if err := errors.Join(cpErr, p.removeContainer(ctx, tmp)); err != nil {
		return errors.Join(err, os.RemoveAll(ws))
	}
	return nil
}

// base returns the workspace's HEAD, after checking the record's base_commit
// is in its history.
func (p *ContainerProvisioner) base(ctx context.Context, cell Cell, ws string) (string, error) {
	head, err := gitOutput(ctx, ws, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("fixrate: %s's workspace has no HEAD: %w", cell.Key(), err)
	}
	head = strings.TrimSpace(head)
	_, errOut, exit, err := execRun(ctx, "git", "-C", ws, "merge-base", "--is-ancestor", cell.Task.BaseCommit, head)
	switch {
	case err != nil:
		return "", fmt.Errorf("fixrate: checking %s's base_commit: %w", cell.Key(), err)
	case exit == 1:
		return "", fmt.Errorf("fixrate: the image %s does not hold %s's base_commit %s in its history, so it is not "+
			"the tree the record describes", cell.Task.Image, cell.Task.ID, cell.Task.BaseCommit)
	case exit != 0:
		return "", fmt.Errorf("fixrate: checking %s's base_commit %s: %s", cell.Key(), cell.Task.BaseCommit, lastLines(errOut, 2))
	}
	return head, nil
}

// Release implements Provisioner. Before removing the container it gives the
// workspace back to this user: a test run inside it ran as root and may have
// left root's files on the host, which this user could not delete.
func (p *ContainerProvisioner) Release(ctx context.Context, cell Cell, dir string) error {
	name := p.containerName(cell)
	if cell.Arm == ArmBelay {
		owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
		// Best effort: a container already gone has nothing to give back.
		if _, _, _, err := p.commander()(ctx, p.docker(), "exec", "--user", "0", name, "chown", "-R", owner, "/testbed"); err != nil {
			return fmt.Errorf("fixrate: docker exec: %w", err)
		}
		if err := p.removeContainer(ctx, name); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, "workspace")); err != nil {
		return fmt.Errorf("fixrate: remove %s's workspace: %w", cell.Key(), err)
	}
	return nil
}

// containerRunner is the belay arm's test runner for a SWE-bench cell; see
// this file's header comment.
type containerRunner struct {
	docker    string
	container string
	repo      string
	prologue  string
	commands  map[string]string
	timeout   time.Duration
	run       commander
}

var _ belay.TestRunner = (*containerRunner)(nil)

// Name implements belay.TestRunner.
func (r *containerRunner) Name() string { return "swebench-container" }

// Detect implements belay.TestRunner. The runner is chosen by the harness,
// never detected.
func (r *containerRunner) Detect(string) bool { return true }

// Test implements belay.TestRunner.
//
// A test command that ran is a report, passing or failing on its exit
// status, with the end of its output as the failure the fix step reads. A
// command that could not run at all -- docker's own 125, 126 or 127, which is
// also bash's "command not found" when the environment would not activate --
// is an error, which the corpus treats as the harness's failure, not the
// arm's.
func (r *containerRunner) Test(ctx context.Context, dir string) (belay.TestReport, error) {
	files, err := changedTestFiles(ctx, dir)
	if err != nil {
		return belay.TestReport{}, err
	}
	if len(files) == 0 {
		return belay.TestReport{Total: 1, Failed: 1, Failures: []belay.TestFailure{{
			Name: "regression-test",
			Message: "the goal asks for a regression test that fails without the change and passes with it, " +
				"and this run has changed no test file (test_*.py, *_test.py or tests.py), so there is nothing to run",
		}}, Raw: json.RawMessage(`{"command":null}`)}, nil
	}

	command := r.command(files)
	script := r.prologue + " && " + command
	start := time.Now()
	stdout, stderr, exit, err := r.run(ctx, r.docker, "exec", "--workdir", "/testbed", "--env", "PYTHONDONTWRITEBYTECODE=1",
		r.container, "timeout", "--kill-after=30", strconv.Itoa(int(r.timeout.Seconds())), "bash", "-c", script)
	if err != nil {
		return belay.TestReport{}, fmt.Errorf("fixrate: running tests in %s: %w", r.container, err)
	}
	output := stdout + stderr
	if exit == 125 || exit == 126 || exit == 127 {
		return belay.TestReport{}, fmt.Errorf("fixrate: the tests could not run in container %s (exit %d): %s",
			r.container, exit, lastLines(output, 3))
	}
	tail := lastLinesRaw(output, 60)
	raw, err := json.Marshal(map[string]any{"command": command, "exit": exit, "output_tail": tail})
	if err != nil {
		return belay.TestReport{}, fmt.Errorf("fixrate: encode the test report: %w", err)
	}
	report := belay.TestReport{Total: 1, Duration: time.Since(start), Raw: raw}
	switch exit {
	case 0:
		report.Passed = 1
	case 124:
		report.Failed = 1
		report.Failures = []belay.TestFailure{{Name: command, Message: fmt.Sprintf("timed out after %s", r.timeout)}}
	default:
		report.Failed = 1
		report.Failures = []belay.TestFailure{{Name: command, Message: tail}}
	}
	return report, nil
}

// command is the repository's test command over files.
func (r *containerRunner) command(files []string) string {
	base, ok := r.commands[r.repo]
	if !ok {
		base, ok = sweBenchTestCommands[r.repo]
	}
	if !ok {
		base = defaultTestCommand
	}
	args := make([]string, 0, len(files))
	for _, f := range files {
		if r.repo == "django/django" {
			// django's runner takes module labels, as SWE-bench's own
			// get_test_directives builds them at v3.0.17.
			f = strings.ReplaceAll(strings.TrimPrefix(strings.TrimSuffix(f, ".py"), "tests/"), "/", ".")
		}
		args = append(args, shellQuote(f))
	}
	return base + " " + strings.Join(args, " ")
}

// changedTestFiles lists the test files the run changed or created in dir,
// leaving out ones it deleted.
func changedTestFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := gitOutput(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("fixrate: listing the run's changes in %s: %w", dir, err)
	}
	var files []string
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		status, path := e[:2], e[3:]
		if status[0] == 'R' || status[0] == 'C' {
			i++ // the entry after a rename or copy is its source
		}
		if strings.ContainsRune(status, 'D') || !isTestFile(path) {
			continue
		}
		files = append(files, path)
	}
	return files, nil
}

// gitOutput runs git in dir and returns what it printed, failing on a
// non-zero exit with the end of what it printed on stderr.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	out, errOut, exit, err := execRun(ctx, "git", append([]string{"-C", dir}, args...)...)
	switch {
	case err != nil:
		return "", err
	case exit != 0:
		return "", fmt.Errorf("git %s exited %d: %s", args[0], exit, lastLines(errOut, 2))
	default:
		return out, nil
	}
}

// isTestFile reports whether path is a Python test module by the naming
// pytest, django and sympy all collect: test_*.py, *_test.py or tests.py.
func isTestFile(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, ".py") &&
		(strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") || base == "tests.py")
}

// shellQuote quotes s for bash.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// lastLinesRaw returns the last n lines of s, as they were.
func lastLinesRaw(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// RemoveImage removes a task's instance image, for CorpusOptions.AfterTask:
// a full corpus touches 500 images of gigabytes each, and once a task's cells
// are scored nothing needs its image again. An image already gone is freed.
func (p *ContainerProvisioner) RemoveImage(ctx context.Context, task Task) error {
	out, errOut, exit, err := p.commander()(ctx, p.docker(), "image", "rm", task.Image)
	switch {
	case err != nil:
		return fmt.Errorf("fixrate: docker image rm: %w", err)
	case exit != 0 && !strings.Contains(errOut+out, "No such image"):
		return fmt.Errorf("fixrate: docker image rm %s exited %d: %s", task.Image, exit, lastLines(errOut+out, 3))
	default:
		return nil
	}
}
