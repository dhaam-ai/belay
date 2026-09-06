//go:build unix

package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// request returns a valid ReviewRequest rooted at a real temporary directory,
// because Review refuses a WorkDir that does not exist.
func request(t *testing.T) belay.ReviewRequest {
	t.Helper()
	return belay.ReviewRequest{
		WorkDir:      t.TempDir(),
		ProjectKey:   testProjectKey,
		ChangedFiles: []string{"main.go"},
		FailOn:       belay.SeverityMajor,
	}
}

// TestDockerArgv pins the containerized invocation exactly.
//
// This is the highest-value table in the package. A wrong mount, a wrong
// container working directory, or a wrong sonar.projectBaseDir does not fail:
// the scanner indexes zero files and SonarQube computes a quality gate over
// nothing, which passes. There is no downstream check that would catch it, so
// the argv is pinned byte for byte rather than spot-checked.
func TestDockerArgv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
		want func(dir string) []string
	}{
		{
			name: "defaults",
			want: func(dir string) []string {
				return []string{
					"run", "--rm",
					"-e", "SONAR_HOST_URL",
					"-e", "SONAR_TOKEN",
					"-v", dir + ":/usr/src",
					"-w", "/usr/src",
					"sonarsource/sonar-scanner-cli",
					"-Dsonar.projectKey=belay-demo",
					"-Dsonar.projectBaseDir=/usr/src",
					"-Dsonar.qualitygate.wait=true",
					"-Dsonar.qualitygate.timeout=300",
				}
			},
		},
		{
			name: "pinned image and shorter gate timeout",
			opts: []Option{
				WithImage("sonarsource/sonar-scanner-cli:11.1"),
				WithQualityGateTimeout(90 * time.Second),
			},
			want: func(dir string) []string {
				return []string{
					"run", "--rm",
					"-e", "SONAR_HOST_URL",
					"-e", "SONAR_TOKEN",
					"-v", dir + ":/usr/src",
					"-w", "/usr/src",
					"sonarsource/sonar-scanner-cli:11.1",
					"-Dsonar.projectKey=belay-demo",
					"-Dsonar.projectBaseDir=/usr/src",
					"-Dsonar.qualitygate.wait=true",
					"-Dsonar.qualitygate.timeout=90",
				}
			},
		},
		{
			// With the wait off there is nothing to time out on, so the
			// timeout property is omitted rather than sent and ignored.
			name: "gate wait disabled drops the timeout property",
			opts: []Option{WithQualityGateWait(false)},
			want: func(dir string) []string {
				return []string{
					"run", "--rm",
					"-e", "SONAR_HOST_URL",
					"-e", "SONAR_TOKEN",
					"-v", dir + ":/usr/src",
					"-w", "/usr/src",
					"sonarsource/sonar-scanner-cli",
					"-Dsonar.projectKey=belay-demo",
					"-Dsonar.projectBaseDir=/usr/src",
					"-Dsonar.qualitygate.wait=false",
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := scanRunner(readFixture(t, "gate-pass.txt"), 0)
			s := newTestScanner(stub, tc.opts...)
			req := request(t)

			if _, err := s.Review(context.Background(), req); err != nil {
				t.Fatalf("Review: %v", err)
			}
			got := stub.lastCall(t)
			if got.Path != "docker" {
				t.Errorf("Path = %q, want %q", got.Path, "docker")
			}
			if diff := cmp.Diff(tc.want(req.WorkDir), got.Args); diff != "" {
				t.Errorf("argv mismatch (-want +got):\n%s", diff)
			}
			if got.Dir != req.WorkDir {
				t.Errorf("Dir = %q, want %q", got.Dir, req.WorkDir)
			}
		})
	}
}

// TestDockerMountSemantics states the mount contract in one place, in the
// terms the failure would be diagnosed in.
func TestDockerMountSemantics(t *testing.T) {
	t.Parallel()
	stub := scanRunner(readFixture(t, "gate-pass.txt"), 0)
	s := newTestScanner(stub)
	req := request(t)
	if _, err := s.Review(context.Background(), req); err != nil {
		t.Fatalf("Review: %v", err)
	}
	args := stub.lastCall(t).Args

	mount := flagValue(t, args, "-v")
	host, container, ok := strings.Cut(mount, ":")
	if !ok {
		t.Fatalf("mount %q is not HOST:CONTAINER", mount)
	}
	if host != req.WorkDir {
		t.Errorf("mount source = %q, want the request work directory %q", host, req.WorkDir)
	}
	if !filepath.IsAbs(host) {
		t.Errorf("mount source %q is not absolute; docker resolves a relative source against its own cwd", host)
	}
	if container != ContainerWorkDir {
		t.Errorf("mount target = %q, want %q", container, ContainerWorkDir)
	}
	if strings.HasSuffix(mount, ":ro") {
		t.Error("mount is read-only; the documented configuration is read-write")
	}
	if got := flagValue(t, args, "-w"); got != ContainerWorkDir {
		t.Errorf("container workdir = %q, want %q", got, ContainerWorkDir)
	}
	if got := property(t, args, "sonar.projectBaseDir"); got != ContainerWorkDir {
		t.Errorf("sonar.projectBaseDir = %q, want the container mount point %q", got, ContainerWorkDir)
	}
	// The base directory must name the container path, never the host path:
	// the host path does not exist inside the container, and a base directory
	// that resolves to nothing scans nothing and passes.
	if strings.Contains(strings.Join(args, " "), "-Dsonar.projectBaseDir="+req.WorkDir) {
		t.Error("sonar.projectBaseDir names the host path; inside the container that path is empty")
	}
}

// TestBinaryArgv pins the local-binary invocation.
func TestBinaryArgv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
		path string
		want func(dir string) []string
	}{
		{
			name: "defaults",
			opts: []Option{WithRunner(RunnerBinary)},
			path: "sonar-scanner",
			want: func(dir string) []string {
				return []string{
					"-Dsonar.projectKey=belay-demo",
					"-Dsonar.projectBaseDir=" + dir,
					"-Dsonar.qualitygate.wait=true",
					"-Dsonar.qualitygate.timeout=300",
				}
			},
		},
		{
			name: "explicit binary",
			opts: []Option{WithRunner(RunnerBinary), WithBinary("/opt/sonar/bin/sonar-scanner")},
			path: "/opt/sonar/bin/sonar-scanner",
			want: func(dir string) []string {
				return []string{
					"-Dsonar.projectKey=belay-demo",
					"-Dsonar.projectBaseDir=" + dir,
					"-Dsonar.qualitygate.wait=true",
					"-Dsonar.qualitygate.timeout=300",
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := scanRunner(readFixture(t, "gate-pass.txt"), 0)
			s := newTestScanner(stub, tc.opts...)
			req := request(t)

			if _, err := s.Review(context.Background(), req); err != nil {
				t.Fatalf("Review: %v", err)
			}
			got := stub.lastCall(t)
			if got.Path != tc.path {
				t.Errorf("Path = %q, want %q", got.Path, tc.path)
			}
			if diff := cmp.Diff(tc.want(req.WorkDir), got.Args); diff != "" {
				t.Errorf("argv mismatch (-want +got):\n%s", diff)
			}
			// Unlike the docker runner, the base directory is the host
			// path, because there is no mount.
			if got.Dir != req.WorkDir {
				t.Errorf("Dir = %q, want %q", got.Dir, req.WorkDir)
			}
		})
	}
}

// TestCommandEnvironment checks that the credential reaches the scanner
// through the environment allowlist and never any other way.
func TestCommandEnvironment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		opts        []Option
		wantAllow   []string
		wantNoAllow []string
	}{
		{
			name:        "docker",
			wantAllow:   []string{"DOCKER_HOST", "DOCKER_CONTEXT", EnvHostURL},
			wantNoAllow: []string{EnvToken},
		},
		{
			name:        "binary",
			opts:        []Option{WithRunner(RunnerBinary)},
			wantAllow:   []string{"JAVA_HOME", "SONAR_SCANNER_OPTS", EnvHostURL},
			wantNoAllow: []string{EnvToken},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := scanRunner(readFixture(t, "gate-pass.txt"), 0)
			s := newTestScanner(stub, tc.opts...)
			if _, err := s.Review(context.Background(), request(t)); err != nil {
				t.Fatalf("Review: %v", err)
			}
			cmd := stub.lastCall(t)

			if diff := cmp.Diff([]string{EnvToken}, cmd.SecretEnv); diff != "" {
				t.Errorf("SecretEnv mismatch (-want +got):\n%s", diff)
			}
			for _, name := range tc.wantAllow {
				if !slices.Contains(cmd.EnvAllow, name) {
					t.Errorf("EnvAllow is missing %q: %v", name, cmd.EnvAllow)
				}
			}
			for _, name := range tc.wantNoAllow {
				if slices.Contains(cmd.EnvAllow, name) {
					t.Errorf("%q is in EnvAllow; it belongs in SecretEnv so its value seeds the redactor", name)
				}
			}
			if len(cmd.ExtraEnv) != 0 {
				t.Errorf("ExtraEnv is set (%v); credentials must come from the parent environment, not be injected", cmd.ExtraEnv)
			}
			if len(cmd.Secrets) != 0 {
				t.Errorf("Secrets is set (%v); the Scanner must never hold a credential value", cmd.Secrets)
			}
		})
	}
}

// TestTokenNeverLeaks is the credential-containment test.
//
// It sweeps every value the Scanner produces or hands to exec for the literal
// token bytes: the argv, the command struct, the returned report including its
// Raw payload, and the text of the error. The Execer is a stub, so exec's
// redactor never runs — a leak would appear verbatim rather than as a
// [REDACTED:...] marker, which makes this stricter than production.
func TestTokenNeverLeaks(t *testing.T) {
	t.Parallel()
	fixtures := []struct {
		name     string
		stdout   string
		stderr   string
		exitCode int
	}{
		{"gate pass", readFixture(t, "gate-pass.txt"), "", 0},
		{"gate fail", readFixture(t, "gate-fail.txt"), "", 1},
		{"auth failure", readFixture(t, "auth-failure.txt"), readFixture(t, "auth-failure.stderr.txt"), 1},
		{"unreachable host", readFixture(t, "unreachable-host.txt"), "", 1},
	}
	runners := []struct {
		name string
		opts []Option
	}{
		{"docker", nil},
		{"binary", []Option{WithRunner(RunnerBinary)}},
	}
	for _, r := range runners {
		for _, f := range fixtures {
			t.Run(r.name+"/"+f.name, func(t *testing.T) {
				t.Parallel()
				stub := &stubExecer{stdout: f.stdout, stderr: f.stderr, exitCode: f.exitCode}
				s := newTestScanner(stub, r.opts...)

				report, err := s.Review(context.Background(), request(t))
				cmd := stub.lastCall(t)

				assertNoToken(t, "Command.Path", cmd.Path)
				assertNoToken(t, "Command.Args", strings.Join(cmd.Args, " "))
				assertNoToken(t, "Command.Dir", cmd.Dir)
				assertNoToken(t, "Command.ExtraEnv", spew(cmd.ExtraEnv))
				for _, sec := range cmd.Secrets {
					assertNoToken(t, "Command.Secrets", sec.Value)
				}
				assertNoToken(t, "report.Summary", report.Summary)
				assertNoToken(t, "report.Raw", string(report.Raw))
				encoded, marshalErr := json.Marshal(report)
				if marshalErr != nil {
					t.Fatalf("marshal report: %v", marshalErr)
				}
				assertNoToken(t, "serialized report", string(encoded))
				if err != nil {
					assertNoToken(t, "error", err.Error())
				}
			})
		}
	}
}

// TestTokenNeverPassedAsProperty guards the specific mistake the exec
// redactor has a pattern for: -Dsonar.token=... in argv.
//
// The redactor would catch it in production, but Result.Args is not the only
// place an argv travels, and relying on redaction to undo a design mistake is
// weaker than not making it. The same applies to sonar.login and
// sonar.password, the deprecated spellings.
func TestTokenNeverPassedAsProperty(t *testing.T) {
	t.Parallel()
	for _, opts := range [][]Option{nil, {WithRunner(RunnerBinary)}} {
		stub := scanRunner(readFixture(t, "gate-pass.txt"), 0)
		s := newTestScanner(stub, opts...)
		if _, err := s.Review(context.Background(), request(t)); err != nil {
			t.Fatalf("Review: %v", err)
		}
		joined := strings.ToLower(strings.Join(stub.lastCall(t).Args, " "))
		for _, banned := range []string{"sonar.token", "sonar.login", "sonar.password", "sonar.host.url"} {
			if strings.Contains(joined, banned) {
				t.Errorf("%s appears in argv; it must be passed through the environment: %s", banned, joined)
			}
		}
	}
}

// TestExecDeadlineExceedsGateTimeout pins the relationship between the two
// deadlines.
//
// If belay's deadline were not the looser one, it would kill the scanner at
// the moment the scanner was about to report its own determinate gate-wait
// timeout, converting a diagnosable failure into a process kill.
func TestExecDeadlineExceedsGateTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		opts        []Option
		want        time.Duration
		wantGateSec string
	}{
		{
			name:        "default",
			want:        DefaultQualityGateTimeout + scanOverhead,
			wantGateSec: "300",
		},
		{
			name:        "custom gate timeout",
			opts:        []Option{WithQualityGateTimeout(90 * time.Second)},
			want:        90*time.Second + scanOverhead,
			wantGateSec: "90",
		},
		{
			name:        "explicit override",
			opts:        []Option{WithTimeout(42 * time.Minute)},
			want:        42 * time.Minute,
			wantGateSec: "300",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := scanRunner(readFixture(t, "gate-pass.txt"), 0)
			s := newTestScanner(stub, tc.opts...)
			if _, err := s.Review(context.Background(), request(t)); err != nil {
				t.Fatalf("Review: %v", err)
			}
			cmd := stub.lastCall(t)

			if cmd.Timeout != tc.want {
				t.Errorf("Command.Timeout = %s, want %s", cmd.Timeout, tc.want)
			}
			// The property the whole design rests on: belay's deadline is
			// strictly the looser of the two, so the scanner always gets to
			// report its own determinate gate-wait timeout rather than being
			// killed a moment before it can.
			if cmd.Timeout <= s.gateTimeout {
				t.Errorf("belay deadline %s does not exceed the gate wait %s; the scanner would be killed before it could report a verdict",
					cmd.Timeout, s.gateTimeout)
			}
			// The same value must reach the scanner as its own gate wait.
			if got := property(t, cmd.Args, "sonar.qualitygate.timeout"); got != tc.wantGateSec {
				t.Errorf("sonar.qualitygate.timeout = %q, want %q", got, tc.wantGateSec)
			}
			if cmd.MaxOutput != DefaultMaxOutput {
				t.Errorf("Command.MaxOutput = %d, want %d", cmd.MaxOutput, DefaultMaxOutput)
			}
		})
	}
}

// TestReviewRejectsBadRequests covers everything that can be decided before a
// process starts. Every case must fail without the Execer being called at all.
func TestReviewRejectsBadRequests(t *testing.T) {
	t.Parallel()
	existingFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(existingFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*belay.ReviewRequest)
		env     map[string]string
		opts    []Option
		wantErr error
	}{
		{
			name:    "no project key",
			mutate:  func(r *belay.ReviewRequest) { r.ProjectKey = "  " },
			wantErr: ErrMissingProjectKey,
		},
		{
			name:    "empty work dir",
			mutate:  func(r *belay.ReviewRequest) { r.WorkDir = "" },
			wantErr: ErrBadWorkDir,
		},
		{
			// The catastrophic one: docker would create this path as an
			// empty directory and the scan would pass over zero files.
			name:    "work dir does not exist",
			mutate:  func(r *belay.ReviewRequest) { r.WorkDir = filepath.Join(r.WorkDir, "absent") },
			wantErr: ErrBadWorkDir,
		},
		{
			name:    "work dir is a file",
			mutate:  func(r *belay.ReviewRequest) { r.WorkDir = existingFile },
			wantErr: ErrBadWorkDir,
		},
		{
			name:    "token unset",
			env:     map[string]string{EnvHostURL: testHostURL},
			wantErr: ErrMissingToken,
		},
		{
			name:    "token blank",
			env:     map[string]string{EnvToken: "   ", EnvHostURL: testHostURL},
			wantErr: ErrMissingToken,
		},
		{
			name:    "host url unset",
			env:     map[string]string{EnvToken: testToken},
			wantErr: ErrMissingHostURL,
		},
		{
			name:    "unknown runner",
			opts:    []Option{WithRunner("podman")},
			wantErr: ErrUnknownRunner,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubExecer{}
			opts := append([]Option{WithExecer(stub), WithLogger(quietLogger())}, tc.opts...)
			if tc.env != nil {
				opts = append(opts, WithLookupEnv(envWith(tc.env)))
			} else {
				opts = append(opts, WithLookupEnv(testEnv()))
			}
			s := New(opts...)

			req := request(t)
			if tc.mutate != nil {
				tc.mutate(&req)
			}

			report, err := s.Review(context.Background(), req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if stub.callCount() != 0 {
				t.Errorf("the scanner was started despite an invalid request (%d calls)", stub.callCount())
			}
			assertReportShape(t, report)
			if report.Gate != belay.GateError {
				t.Errorf("Gate = %s, want %s", report.Gate, belay.GateError)
			}
		})
	}
}

// flagValue returns the argument following the first occurrence of flag.
func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("flag %q not found in %v", flag, args)
	return ""
}

// property returns the value of the -D<name>= argument.
func property(t *testing.T, args []string, name string) string {
	t.Helper()
	prefix := "-D" + name + "="
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return strings.TrimPrefix(a, prefix)
		}
	}
	t.Fatalf("property %q not found in %v", name, args)
	return ""
}

// assertNoToken fails if the test token appears anywhere in value.
func assertNoToken(t *testing.T, where, value string) {
	t.Helper()
	if strings.Contains(value, testToken) {
		t.Errorf("SONAR_TOKEN leaked into %s: %s", where, value)
	}
}

// spew renders a map deterministically enough for a containment check.
func spew(m map[string]string) string {
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(v)
		b.WriteString(" ")
	}
	return b.String()
}
