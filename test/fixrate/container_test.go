//go:build fixrate

package fixrate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// fakeDocker stands in for the docker CLI. It records every command, plays
// `docker cp` by copying template -- a committed repository standing in for
// an instance image's /testbed -- to the destination, and answers `docker
// exec` with whatever exec scripts.
type fakeDocker struct {
	t        *testing.T
	template string
	exec     func(args []string) (string, int)

	mu    sync.Mutex
	calls [][]string
}

// newFakeDocker returns a fake whose image holds files, committed, and the
// commit they are in: the base_commit a record would name.
func newFakeDocker(t *testing.T, files map[string]string) (*fakeDocker, string) {
	t.Helper()
	template := t.TempDir()
	base := commitRepo(t, template, files)
	return &fakeDocker{t: t, template: template}, base
}

func (f *fakeDocker) run(_ context.Context, name string, args ...string) (string, string, int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	if name != "docker" || len(args) == 0 {
		return "", "", -1, errors.New("fake docker: unexpected program " + name)
	}
	switch args[0] {
	case "image":
		return "[]", "", 0, nil
	case "cp":
		if err := os.CopyFS(args[len(args)-1], os.DirFS(f.template)); err != nil {
			return "", err.Error(), 1, nil
		}
		return "", "", 0, nil
	case "exec":
		if f.exec != nil {
			out, code := f.exec(args)
			return out, "", code, nil
		}
		return "", "", 0, nil
	default:
		return "", "", 0, nil
	}
}

func (f *fakeDocker) commands(sub string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c) > 1 && c[1] == sub {
			out = append(out, c)
		}
	}
	return out
}

func containerTask(repo, base string) Task {
	return Task{
		ID: "example__widget-7", Repo: repo, BaseCommit: base, Image: "example.invalid/sweb.eval.x86_64.widget-7:latest",
		FailToPass: []string{"tests/test_core.py::test_x"}, EvalType: "pass_and_fail",
	}
}

const pyRepoCore = "def resize(n, f):\n    return int(n * f)\n"

// TestContainerProvisioner_Provision pins how a SWE-bench cell gets its
// workspace: /testbed copied out of the record's own instance image, so the
// arms start from exactly the tree the official evaluation applies their
// patch to; the patch measured from the image's HEAD, which may sit on top of
// base_commit; and, for the belay arm only, a long-lived container of the same
// image with the workspace mounted at /testbed, for its tests to run in.
func TestContainerProvisioner_Provision(t *testing.T) {
	t.Parallel()

	docker, base := newFakeDocker(t, map[string]string{"widget/core.py": pyRepoCore})
	// The image's setup adds a commit on top of base_commit, as SWE-bench's
	// own setup script did ("git commit --allow-empty -am SWE-bench").
	head := commitEmpty(t, docker.template, "SWE-bench")
	p := &ContainerProvisioner{Docker: "docker", RunID: "pilot", run: docker.run}

	dir := t.TempDir()
	cell := Cell{Arm: ArmBelay, Replicate: 1, Task: containerTask("example/widget", base)}
	ws, err := p.Provision(t.Context(), cell, dir, true)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	if ws.Dir != filepath.Join(dir, "workspace") {
		t.Errorf("Dir = %q, want the cell's workspace directory", ws.Dir)
	}
	if ws.Base != head {
		t.Errorf("Base = %q, want the image's HEAD %q: the patch applies to what the image holds", ws.Base, head)
	}
	if _, ok := ws.Runner.(*containerRunner); !ok {
		t.Errorf("Runner = %T, want the container runner", ws.Runner)
	}
	if ws.Linter == nil || ws.Linter.Name() != "ruff" {
		t.Errorf("Linter = %v, want belay's shipped ruff adapter", ws.Linter)
	}
	if !strings.Contains(ws.Adapters, "ruff") || !strings.Contains(ws.Adapters, "container") {
		t.Errorf("Adapters = %q, want it to name both adapters", ws.Adapters)
	}

	runs := docker.commands("run")
	if len(runs) != 1 {
		t.Fatalf("docker run was called %d time(s), want once: %v", len(runs), runs)
	}
	run := strings.Join(runs[0], " ")
	for _, want := range []string{"--detach", "--name " + p.containerName(cell), "--volume " + ws.Dir + ":/testbed", cell.Task.Image} {
		if !strings.Contains(run, want) {
			t.Errorf("docker run %q lacks %q", run, want)
		}
	}
	if cps := docker.commands("cp"); len(cps) != 1 || !strings.HasSuffix(strings.Join(cps[0], " "), ":/testbed "+ws.Dir) {
		t.Errorf("docker cp = %v, want /testbed copied to the workspace", cps)
	}
}

// TestContainerProvisioner_RefusesAnImageThatIsNotTheRecords: base_commit
// must be in the image's history, or the arms would work on a tree the
// record does not describe.
func TestContainerProvisioner_RefusesAnImageThatIsNotTheRecords(t *testing.T) {
	t.Parallel()

	docker, _ := newFakeDocker(t, map[string]string{"widget/core.py": pyRepoCore})
	p := &ContainerProvisioner{Docker: "docker", RunID: "pilot", run: docker.run}
	cell := Cell{Arm: ArmControl, Replicate: 1, Task: containerTask("example/widget", strings.Repeat("e", 40))}
	_, err := p.Provision(t.Context(), cell, t.TempDir(), true)
	if err == nil || !strings.Contains(err.Error(), "base_commit") {
		t.Fatalf("Provision() error = %v, want a refusal naming base_commit", err)
	}
}

// TestContainerProvisioner_ControlArmNeedsNoContainer: the control arm runs no
// tests, so it gets the workspace and nothing that would run.
func TestContainerProvisioner_ControlArmNeedsNoContainer(t *testing.T) {
	t.Parallel()

	docker, base := newFakeDocker(t, map[string]string{"widget/core.py": pyRepoCore})
	p := &ContainerProvisioner{Docker: "docker", RunID: "pilot", run: docker.run}
	cell := Cell{Arm: ArmControl, Replicate: 1, Task: containerTask("example/widget", base)}
	if _, err := p.Provision(t.Context(), cell, t.TempDir(), true); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if runs := docker.commands("run"); len(runs) != 0 {
		t.Errorf("the control arm got a container: %v", runs)
	}
}

// TestContainerProvisioner_ResumeKeepsTheWorkspace: a resumed belay cell must
// find its own edits. The container is recreated -- whatever ran in the old
// one may still be running -- but the workspace is not touched.
func TestContainerProvisioner_ResumeKeepsTheWorkspace(t *testing.T) {
	t.Parallel()

	docker, base := newFakeDocker(t, map[string]string{"widget/core.py": pyRepoCore})
	p := &ContainerProvisioner{Docker: "docker", RunID: "pilot", run: docker.run}
	dir := t.TempDir()
	cell := Cell{Arm: ArmBelay, Replicate: 1, Task: containerTask("example/widget", base)}
	ws, err := p.Provision(t.Context(), cell, dir, true)
	if err != nil {
		t.Fatalf("first Provision() error = %v", err)
	}
	edit := filepath.Join(ws.Dir, "widget", "core.py")
	if err := os.WriteFile(edit, []byte("# the agent's edit\n"), 0o600); err != nil {
		t.Fatalf("edit: %v", err)
	}
	again, err := p.Provision(t.Context(), cell, dir, false)
	if err != nil {
		t.Fatalf("second Provision() error = %v", err)
	}
	if again.Base != ws.Base {
		t.Errorf("Base moved from %s to %s on resume", ws.Base, again.Base)
	}
	if data := readFile(t, edit); string(data) != "# the agent's edit\n" {
		t.Errorf("the resumed workspace lost the agent's edit: %q", data)
	}
	if cps := docker.commands("cp"); len(cps) != 1 {
		t.Errorf("the image was copied out %d time(s), want once", len(cps))
	}
	if runs := docker.commands("run"); len(runs) != 2 {
		t.Errorf("docker run was called %d time(s), want 2: the container is recreated on resume", len(runs))
	}
}

// TestContainerRunner pins the belay arm's test runner, the one adapter the
// harness supplies in place of belay's own. It runs, in the instance's own
// container and environment, only the test files the run changed -- the
// regression test the shared goal asks for, and any test file it edited --
// with the repository's own test command. It never sees the dataset's tests:
// those are the answer key, and the control arm has no such help.
func TestContainerRunner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		repo    string
		change  map[string]string
		exit    int
		wantCmd string
		wantErr bool
		wantOK  bool
		wantMsg string
	}{
		{
			name:    "the changed test files, with pytest",
			repo:    "example/widget",
			change:  map[string]string{"tests/test_resize.py": "def test_x():\n    pass\n", "widget/core.py": "x = 1\n"},
			wantCmd: "pytest -rA 'tests/test_resize.py'",
			wantOK:  true,
		},
		{
			name:    "a failing run is a failing report, with the output",
			repo:    "example/widget",
			change:  map[string]string{"widget/test_core.py": "def test_x():\n    assert False\n"},
			exit:    1,
			wantCmd: "pytest -rA 'widget/test_core.py'",
			wantMsg: "FAILED widget/test_core.py::test_x",
		},
		{
			name:    "django's own runner, by module label",
			repo:    "django/django",
			change:  map[string]string{"tests/admin_views/test_forms.py": "pass\n"},
			wantCmd: "./tests/runtests.py --verbosity 2 --settings=test_sqlite --parallel 1 'admin_views.test_forms'",
			wantOK:  true,
		},
		{
			name:    "sympy's own runner",
			repo:    "sympy/sympy",
			change:  map[string]string{"sympy/core/tests/test_basic.py": "pass\n"},
			wantCmd: "bin/test -C --verbose 'sympy/core/tests/test_basic.py'",
			wantOK:  true,
		},
		{
			name:    "no test file changed is a failure that says why",
			repo:    "example/widget",
			change:  map[string]string{"widget/core.py": "x = 2\n"},
			wantMsg: "regression test",
		},
		{
			name:    "a container that is not there is no test run at all",
			repo:    "example/widget",
			change:  map[string]string{"tests/test_resize.py": "pass\n"},
			exit:    125,
			wantErr: true,
		},
		{
			name:    "an environment that would not activate is no test run either",
			repo:    "example/widget",
			change:  map[string]string{"tests/test_resize.py": "pass\n"},
			exit:    127,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := t.TempDir()
			commitRepo(t, ws, map[string]string{"widget/core.py": pyRepoCore, "README.md": "w\n"})
			writeTree(t, ws, tt.change)

			var got []string
			r := &containerRunner{
				docker: "docker", container: "fixrate-cell", repo: tt.repo, prologue: DefaultContainerPrologue,
				timeout: time.Minute,
				run: func(_ context.Context, name string, args ...string) (string, string, int, error) {
					got = append([]string{name}, args...)
					out := "1 passed in 0.01s\n"
					if tt.exit == 1 {
						out = "FAILED widget/test_core.py::test_x - assert False\n1 failed in 0.02s\n"
					}
					return out, "", tt.exit, nil
				},
			}
			report, err := r.Test(t.Context(), ws)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Test() = %+v, nil error; want an error", report)
				}
				return
			}
			if err != nil {
				t.Fatalf("Test() error = %v", err)
			}
			if report.OK() != tt.wantOK {
				t.Errorf("OK() = %v, want %v (%+v)", report.OK(), tt.wantOK, report)
			}
			if tt.wantCmd != "" {
				if len(got) < 6 || got[1] != "exec" || !slices.Contains(got, "fixrate-cell") {
					t.Fatalf("ran %q, want docker exec in the cell's container", got)
				}
				script := got[len(got)-1]
				if !strings.HasPrefix(script, DefaultContainerPrologue+" && ") || !strings.HasSuffix(script, tt.wantCmd) {
					t.Errorf("script = %q\nwant the prologue, then %q", script, tt.wantCmd)
				}
			}
			if tt.wantMsg != "" {
				var msgs []string
				for _, f := range report.Failures {
					msgs = append(msgs, f.Message)
				}
				if !strings.Contains(strings.Join(msgs, "\n"), tt.wantMsg) {
					t.Errorf("failures = %q, want one mentioning %q", msgs, tt.wantMsg)
				}
			}
		})
	}
}

var _ belay.TestRunner = (*containerRunner)(nil)

// commitEmpty adds an empty commit to the repository in dir, and returns it.
func commitEmpty(t *testing.T, dir, msg string) string {
	t.Helper()
	gitIn(t, dir, "commit", "--quiet", "--allow-empty", "--message", msg)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// requireDocker skips t unless a Docker daemon answers.
func requireDocker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("skipping: needs a running Docker daemon (docker info: %v). Install Docker Engine "+
			"(https://docs.docker.com/engine/install/) or Docker Desktop and start it, or ask your coding agent: %q",
			err, "Install and start Docker, then run `go test -tags fixrate ./test/fixrate -run TestRealDocker -v` "+
				"and confirm the tests pass instead of skipping.")
	}
}

// TestRealDocker_ContainerRunner runs the container runner against a real
// Docker daemon, with a small public image standing in for an instance image:
// a workspace mounted at /testbed, a changed test file, the test command run
// inside the container. FIXRATE_DOCKER_TEST_IMAGE overrides the image.
func TestRealDocker_ContainerRunner(t *testing.T) {
	requireDocker(t)
	image := os.Getenv("FIXRATE_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "python:3.12-slim"
	}
	ws := t.TempDir()
	commitRepo(t, ws, map[string]string{"pkg/__init__.py": "", "pkg/core.py": "def two():\n    return 2\n"})
	writeTree(t, ws, map[string]string{"tests/test_core.py": "import unittest\n\nfrom pkg.core import two\n\n\n" +
		"class T(unittest.TestCase):\n    def test_two(self):\n        self.assertEqual(two(), 2)\n"})

	name := "belay-fixrate-dockertest-" + strings.ReplaceAll(filepath.Base(ws), "_", "-")
	ctx := t.Context()
	if out, err := exec.CommandContext(ctx, "docker", "run", "--detach", "--name", name, //nolint:gosec // fixed arguments
		"--volume", ws+":/testbed", image, "tail", "-f", "/dev/null").CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rm", "--force", name).CombinedOutput(); err != nil { //nolint:gosec // fixed arguments
			t.Errorf("docker rm: %v\n%s", err, out)
		}
	})
	r := &containerRunner{
		docker: "docker", container: name, repo: "example/real", prologue: "cd /testbed",
		commands: map[string]string{"example/real": "python -m unittest -v"}, timeout: 5 * time.Minute, run: execRun,
	}
	report, err := r.Test(ctx, ws)
	if err != nil {
		t.Fatalf("Test() error = %v", err)
	}
	if !report.OK() {
		t.Errorf("Test() = %+v, want a passing run of tests/test_core.py inside the container", report)
	}
}
