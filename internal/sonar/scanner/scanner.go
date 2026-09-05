//go:build unix

// Package scanner adapts the SonarQube scanner CLI to belay.Reviewer.
//
// It is the review.mode: sonar path of ADR 0008 — the opt-in alternative to
// the local linters in internal/linter. One [Scanner] runs one scan, either
// in the sonarsource/sonar-scanner-cli container ([RunnerDocker], the default)
// or through a locally installed sonar-scanner binary ([RunnerBinary]), and
// turns what the scanner printed into a belay.QualityReport.
//
// # The gate is computed on the server, not here
//
// This is the fact that shapes everything else in the package. A SonarQube
// quality gate is a project-level policy evaluated by the SonarQube server
// after it has ingested the analysis report. The scanner CLI does not analyze
// code locally, does not know the gate's conditions, and never prints the
// individual issues behind a verdict. With sonar.qualitygate.wait=true it
// blocks until the server has a verdict, prints one line naming it, and exits
// non-zero if the verdict is FAILED.
//
// So the CLI gives belay exactly one bit plus a dashboard URL. That has three
// consequences a reader should have in mind before reading any code here:
//
//   - QualityReport.Issues cannot be populated from a CLI scan. See
//     "Report field-population rules" below for what is populated instead and
//     why it is not a fabrication.
//   - ReviewRequest.FailOn cannot be honored as a threshold. The server's
//     gate, not belay's severity ceiling, decides. belay.Reviewer explicitly
//     allows this ("a Reviewer that only supports whole-project analysis may
//     ignore it"), and the report is still constructed so that the documented
//     invariant Gate == GatePass ⟺ Counts.AtOrAbove(FailOn) == 0 holds for
//     every FailOn.
//   - ReviewRequest.ChangedFiles is ignored. Narrowing sonar.sources to a
//     changed-file list would make the server see every unlisted file as
//     deleted, corrupting the project's stored state and the new-code delta of
//     every later scan. Scanning the whole workspace is the only correct
//     option, and belay.Reviewer permits it.
//
// # Three outcomes, and only two of them are errors
//
// This mirrors internal/linter, because the graph must not tell them apart:
//
//   - The gate passed: GatePass, nil error.
//   - The gate failed: GateFail, and a nil error. A failed quality gate is a
//     normal branch to the fix node, exactly like a failing test. The scanner
//     exits non-zero for it, so the exit status alone must never be read as a
//     failure to run — conflating the two is what breaks the fix loop.
//   - The scan never reached a verdict — docker missing, image unpullable,
//     host unreachable, token rejected, the server's gate computation not
//     finished inside sonar.qualitygate.timeout, or belay's own deadline
//     elapsing: GateError and a real error. A timeout is GateError and not
//     GateFail on purpose: "we never learned the answer" is not "the answer
//     was no", and routing it to a fix loop hands an agent a diagnosis that
//     does not exist.
//
// # Report field-population rules
//
// Every report this package returns, on every path, has:
//
//	Source:  "sonarqube" (the Source constant), always.
//	Issues:  non-nil, always. A pass or an error yields []belay.Issue{}.
//	Counts:  the histogram of Issues, so all zero except on a failed gate.
//	Raw:     non-nil JSON, always — an object described by [RawScan].
//	Summary: a one-line human synopsis, never empty.
//	Gate:    GatePass, GateFail or GateError; never GateUnknown.
//
// On a failed gate, and only then, Issues holds exactly one synthesized
// belay.Issue at belay.SeverityBlocker with RuleID [GateRuleID], carrying the
// scanner's own verdict line and the dashboard URL. It is a representation of
// the server's verdict, not an invented finding: the message says so, the rule
// id is belay's own and not a SonarQube rule key, and File and Line are empty
// and zero because the verdict is not attributable to a line.
//
// Blocker, rather than a severity derived from ReviewRequest.FailOn, keeps the
// belay.Reviewer invariant true in both directions for every threshold —
// Counts.AtOrAbove(FailOn) is 1 for any FailOn when the gate failed and 0 when
// it passed — without making a report's contents depend on the caller's
// setting. A SonarQube quality gate is the project's own definition of "must
// not ship", which is what belay.SeverityBlocker means.
//
// A caller wanting per-issue detail from SonarQube wants the AI reviewer built
// on the MCP server (review.mode: ai), which can call
// search_sonar_issues_in_projects. That is a different package.
//
// # Configuration
//
// Options mirror config.Review.Sonar field for field. This package does not
// import internal/config; the wiring layer translates:
//
//	review.sonar.runner               -> WithRunner(RunnerDocker|RunnerBinary)
//	review.sonar.image                -> WithImage
//	review.sonar.quality_gate_wait    -> WithQualityGateWait
//	review.sonar.quality_gate_timeout -> WithQualityGateTimeout (seconds)
//
// # Credentials
//
// SONAR_TOKEN and SONAR_HOST_URL are read from belay's own environment and
// never from configuration or from a -D flag. See the [Scanner] docs for why
// the flag form is refused.
//
// # Subprocesses
//
// Every process starts through internal/exec, never os/exec, for its
// deny-by-default environment, output caps, redaction and process-group
// timeouts. [Execer] is that seam and it is injectable, which is what lets
// this package's tests cover every outcome above without Docker, without a
// SonarQube server, and without a network.
package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/belay-dev/belay/internal/exec"
	"github.com/belay-dev/belay/pkg/belay"
)

// Source is the QualityReport.Source every report from this package carries.
//
// T33 compares this package's serialized reports against the AI reviewer's key
// for key; Source is the one field the two are expected to differ on.
const Source = "sonarqube"

// GateRuleID is the belay.Issue.RuleID of the synthesized issue that
// represents a failed server-side quality gate.
//
// It is deliberately not shaped like a SonarQube rule key (which look like
// "go:S1005"), so nobody reading a report or a journal mistakes it for a
// finding SonarQube reported.
const GateRuleID = "belay:sonar-quality-gate"

// Runner selects how the scanner CLI is executed. It mirrors
// config.Review.Sonar.Runner.
type Runner string

// Supported runners.
const (
	// RunnerDocker runs the scanner in the sonarsource/sonar-scanner-cli
	// container. It is the default because Docker is far more commonly
	// present than a local sonar-scanner install, and the image pins the
	// scanner version so two machines analyze identically.
	RunnerDocker Runner = "docker"
	// RunnerBinary runs a locally installed sonar-scanner from PATH.
	RunnerBinary Runner = "binary"
)

// Defaults applied when an Option leaves a setting unset.
const (
	// DefaultImage is the scanner container image, matching the
	// review.sonar.image default in internal/config.
	DefaultImage = "sonarsource/sonar-scanner-cli"

	// DefaultBinary is the local scanner executable looked up on PATH when
	// the runner is RunnerBinary.
	DefaultBinary = "sonar-scanner"

	// DefaultQualityGateTimeout bounds the scanner's own wait for the
	// server to finish computing the gate. It is SonarQube's documented
	// default for sonar.qualitygate.timeout and matches the
	// review.sonar.quality_gate_timeout default in internal/config.
	DefaultQualityGateTimeout = 300 * time.Second

	// DefaultMaxOutput caps captured stdout and stderr, in bytes. A scanner
	// run prints one INFO line per analyzer plus one per indexed file, so a
	// large repository legitimately produces hundreds of kilobytes; 4 MiB
	// leaves room without letting a pathological run fill the journal.
	DefaultMaxOutput int64 = 4 << 20
)

// scanOverhead is how much wall clock the scan gets on top of the
// quality-gate wait before belay's own deadline fires.
//
// The two deadlines must not be equal, and belay's must be the looser one.
// sonar.qualitygate.timeout bounds only the poll for the server's verdict;
// indexing the workspace, running the analyzers and uploading the report all
// happen first. If belay's deadline fired at the same moment, it would kill
// the scanner just as it was about to print the verdict, and a determinate
// GateFail or GatePass would be reported as an indeterminate GateError.
const scanOverhead = 5 * time.Minute

// Container paths inside the sonarsource/sonar-scanner-cli image.
const (
	// ContainerWorkDir is where the workspace is bind-mounted and where the
	// scan is rooted.
	//
	// The image's own Dockerfile sets SRC_PATH=/usr/src, WORKDIR ${SRC_PATH}
	// and chowns it to the scanner-cli user, and the SonarScanner CLI docs
	// state: "By default, the scanner will run the analysis from the
	// /usr/src folder with /usr/src as the base directory."
	//
	// This constant is load-bearing in a way that is easy to underestimate.
	// Mounting the workspace anywhere else, or leaving the base directory
	// pointing at a path that is empty inside the container, does not fail:
	// the scanner indexes zero files, uploads an empty report, and SonarQube
	// computes a quality gate over nothing — which passes. A wrong mount is
	// therefore silent, and it is silent in the one direction a quality tool
	// must never be wrong in. Scanner.Review defends against it three times
	// over: it refuses a WorkDir that is not an existing directory, it sets
	// the container working directory explicitly with -w, and it passes
	// sonar.projectBaseDir explicitly rather than relying on the image's
	// WORKDIR staying where it is today.
	ContainerWorkDir = "/usr/src"
)

// Environment variables read from belay's own environment.
const (
	// EnvToken names the SonarQube user token variable.
	EnvToken = "SONAR_TOKEN"
	// EnvHostURL names the SonarQube server base URL variable.
	EnvHostURL = "SONAR_HOST_URL"
)

// dockerEnv names the parent variables the docker CLI needs to find and
// authenticate to a daemon, beyond exec.BaseEnvNames.
//
// A developer on a rootless daemon, a remote context, or Colima has the
// connection details in exactly these variables; without them docker talks to
// the default socket and reports a daemon that is not running.
var dockerEnv = []string{
	"DOCKER_CERT_PATH",
	"DOCKER_CONFIG",
	"DOCKER_CONTEXT",
	"DOCKER_HOST",
	"DOCKER_TLS_VERIFY",
	EnvHostURL,
}

// binaryEnv names the parent variables a local sonar-scanner needs beyond
// exec.BaseEnvNames.
//
// sonar-scanner is a shell wrapper around a JVM: it needs to find a JDK, and
// it honors SONAR_SCANNER_OPTS for heap and proxy settings and SONAR_USER_HOME
// for its analyzer cache. Without the cache it re-downloads every language
// analyzer on every run.
var binaryEnv = []string{
	"JAVA_HOME",
	"SONAR_SCANNER_JAVA_OPTS",
	"SONAR_SCANNER_OPTS",
	"SONAR_USER_HOME",
	EnvHostURL,
}

// secretEnv names the parent variables passed to the child whose values also
// seed the exec redactor.
//
// SONAR_TOKEN already matches exec.IsSecretName, so it would be redacted
// anyway. Naming it explicitly makes the intent legible at the call site and
// stops a rename of exec's heuristics from silently unredacting it.
var secretEnv = []string{EnvToken}

// Sentinel errors. Match them with errors.Is.
var (
	// ErrMissingToken reports that SONAR_TOKEN is unset or empty in belay's
	// environment.
	//
	// This is checked before the scanner starts rather than being left to
	// fail server-side, because an unauthenticated scan against a server
	// that allows anonymous analysis succeeds and gates on the wrong
	// project's policy.
	ErrMissingToken = errors.New("belay/sonar: " + EnvToken + " is not set in the environment")

	// ErrMissingHostURL reports that SONAR_HOST_URL is unset or empty.
	ErrMissingHostURL = errors.New("belay/sonar: " + EnvHostURL + " is not set in the environment")

	// ErrMissingProjectKey reports a ReviewRequest with no ProjectKey.
	// SonarQube requires sonar.projectKey and has no useful default for it.
	ErrMissingProjectKey = errors.New("belay/sonar: review request has no project key")

	// ErrBadWorkDir reports a ReviewRequest whose WorkDir is empty, missing,
	// or not a directory.
	//
	// It is a hard error rather than a warning because of what docker does
	// with a bind-mount source that does not exist: it creates it, as an
	// empty directory, and the scan then finds nothing and passes.
	ErrBadWorkDir = errors.New("belay/sonar: review request work directory is unusable")

	// ErrUnknownRunner reports a Runner that is neither RunnerDocker nor
	// RunnerBinary.
	ErrUnknownRunner = errors.New("belay/sonar: unknown scanner runner")

	// ErrNoGateVerdict reports that the scanner ran and exited cleanly but
	// printed no quality-gate verdict.
	//
	// The ordinary cause is sonar.qualitygate.wait being false, which makes
	// the scanner upload its report and exit without waiting for the server:
	// analysis succeeded, but belay has no gate to act on. That is GateError
	// and not GatePass — reporting a pass belay never saw is the same
	// silent-false-negative failure a wrong mount produces.
	ErrNoGateVerdict = errors.New("belay/sonar: scanner reported no quality gate verdict")

	// ErrGateWaitTimeout reports that the scanner gave up waiting for the
	// server to finish computing the gate.
	//
	// GateError, never GateFail: the analysis was accepted and the verdict
	// may well be a pass; belay simply never saw it.
	ErrGateWaitTimeout = errors.New("belay/sonar: timed out waiting for the quality gate verdict")
)

// ScanError reports that the scanner ran but never reached a gate verdict.
//
// It carries the diagnostics needed to tell an unreachable host from a
// rejected token from a server that is still computing: the exit status and
// the redacted tail of the scanner's own error output, which is where
// SonarQube reports every one of those.
//
// It never represents a failed quality gate. A failed gate is
// (QualityReport{Gate: GateFail}, nil).
type ScanError struct {
	// Runner is the runner that was used, RunnerDocker or RunnerBinary.
	Runner Runner
	// Tool is the executable that ran, such as "docker".
	Tool string
	// ExitCode is the process exit status, or -1 if it never ran.
	ExitCode int
	// Stderr is the redacted tail of the scanner's standard error.
	Stderr string
	// Err is the underlying cause, if any.
	Err error
}

// Error implements error.
func (e *ScanError) Error() string {
	var b strings.Builder
	b.WriteString("belay/sonar: ")
	b.WriteString(e.Tool)
	b.WriteString(" (")
	b.WriteString(string(e.Runner))
	b.WriteString(" runner) did not produce a quality gate verdict (exit ")
	b.WriteString(strconv.Itoa(e.ExitCode))
	b.WriteString(")")
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	if tail := lastLine(e.Stderr); tail != "" {
		b.WriteString(": ")
		b.WriteString(tail)
	}
	return b.String()
}

// Unwrap returns the underlying cause, so errors.Is reaches sentinels such as
// ErrGateWaitTimeout, ErrNoGateVerdict and exec.ErrTimeout.
func (e *ScanError) Unwrap() error { return e.Err }

// Execer starts subprocesses. It is the seam this package runs the scanner
// through, satisfied in production by *exec.Runner and in tests by a stub that
// replays captured output without starting a process.
type Execer interface {
	// Run executes c and returns its redacted result. It follows
	// exec.Runner.Run: a Result is returned even on failure, and a non-zero
	// exit yields an *exec.ExitError rather than nil.
	Run(ctx context.Context, c exec.Command) (exec.Result, error)
}

var _ Execer = (*exec.Runner)(nil)

// Option customizes a Scanner at construction.
type Option func(*Scanner)

// WithExecer replaces the subprocess runner. A nil Execer is ignored, so a
// caller can pass one through unconditionally.
func WithExecer(e Execer) Option {
	return func(s *Scanner) {
		if e != nil {
			s.execer = e
		}
	}
}

// WithLogger sets the logger. A nil logger is ignored and slog.Default is used.
func WithLogger(l *slog.Logger) Option {
	return func(s *Scanner) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithRunner selects how the scanner is executed. It mirrors
// config.Review.Sonar.Runner. An unrecognized value is retained rather than
// rejected here, so Review reports it as ErrUnknownRunner alongside the
// request it failed, instead of a constructor panicking.
func WithRunner(r Runner) Option {
	return func(s *Scanner) { s.runner = r }
}

// WithImage sets the scanner container image used by RunnerDocker. An empty
// image is ignored and DefaultImage is used.
func WithImage(image string) Option {
	return func(s *Scanner) {
		if strings.TrimSpace(image) != "" {
			s.image = image
		}
	}
}

// WithBinary sets the local scanner executable used by RunnerBinary. An empty
// name is ignored and DefaultBinary is used.
func WithBinary(bin string) Option {
	return func(s *Scanner) {
		if strings.TrimSpace(bin) != "" {
			s.binary = bin
		}
	}
}

// WithQualityGateWait sets sonar.qualitygate.wait. It mirrors
// config.Review.Sonar.quality_gate_wait and defaults to true.
//
// Setting it false makes the scanner exit as soon as its report is uploaded,
// which leaves belay with no verdict at all: Review then returns GateError
// wrapping ErrNoGateVerdict. It exists so that behavior is explicit and
// testable, not because turning it off is useful to the graph.
func WithQualityGateWait(wait bool) Option {
	return func(s *Scanner) { s.gateWait = wait }
}

// WithQualityGateTimeout sets sonar.qualitygate.timeout, rounded down to whole
// seconds, and with it belay's own deadline for the whole scan. It mirrors
// config.Review.Sonar.quality_gate_timeout, which is expressed in seconds. A
// non-positive duration is ignored.
func WithQualityGateTimeout(d time.Duration) Option {
	return func(s *Scanner) {
		if d > 0 {
			s.gateTimeout = d
		}
	}
}

// WithTimeout overrides belay's deadline for the whole scan, which otherwise
// is the quality-gate timeout plus room for indexing, analysis and upload. A
// non-positive duration is ignored.
//
// A value at or below the quality-gate timeout turns the scanner's own
// determinate timeout into belay killing the process, which reports the same
// GateError with a less useful explanation.
func WithTimeout(d time.Duration) Option {
	return func(s *Scanner) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// WithLookupEnv replaces the environment lookup used to check that SONAR_TOKEN
// and SONAR_HOST_URL are set, defaulting to os.LookupEnv. It matches
// os.LookupEnv's signature so it can be passed directly.
//
// The Scanner reads whether these are set and never retains their values: the
// child receives them from its own environment through exec, which is also
// what seeds the redactor with the token.
func WithLookupEnv(f func(string) (string, bool)) Option {
	return func(s *Scanner) {
		if f != nil {
			s.lookupEnv = f
		}
	}
}

// Scanner reviews a workspace by running the SonarQube scanner CLI and reading
// the quality-gate verdict the SonarQube server computed.
//
// # The token is never an argument
//
// sonar.token is a valid analysis parameter, and passing it as
// -Dsonar.token=... is the most obvious way to authenticate. This package
// refuses to: a value in argv is captured into exec.Result.Args, and
// exec.Result is written verbatim to .belay/runs/<id>/, so the credential
// would be persisted for every scan the tool ever runs. The token reaches the
// scanner only through the environment.
//
// Under RunnerDocker the forwarding is `-e SONAR_TOKEN` with no `=value`;
// Docker's CLI reference states that in that form "the Docker CLI client
// checks the value the variable has in your local environment and passes it to
// the container", so the value never appears in belay's argv. Inside the
// container the image's entrypoint turns $SONAR_TOKEN into -Dsonar.token=... —
// but that argv belongs to a process belay does not spawn and does not
// capture, so it never reaches a Result or the journal.
//
// The Scanner is safe for concurrent use; it holds no per-scan state.
type Scanner struct {
	execer      Execer
	logger      *slog.Logger
	lookupEnv   func(string) (string, bool)
	runner      Runner
	image       string
	binary      string
	gateWait    bool
	gateTimeout time.Duration
	timeout     time.Duration
}

var _ belay.Reviewer = (*Scanner)(nil)

// New returns a Scanner. With no options it runs the DefaultImage container
// through internal/exec, waits for the quality gate, and reads SONAR_TOKEN and
// SONAR_HOST_URL from the process environment.
func New(opts ...Option) *Scanner {
	s := &Scanner{
		lookupEnv:   os.LookupEnv,
		runner:      RunnerDocker,
		image:       DefaultImage,
		binary:      DefaultBinary,
		gateWait:    true,
		gateTimeout: DefaultQualityGateTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.execer == nil {
		s.execer = exec.New(s.logger)
	}
	return s
}

// execTimeout is belay's own deadline for the whole scan.
func (s *Scanner) execTimeout() time.Duration {
	if s.timeout > 0 {
		return s.timeout
	}
	return s.gateTimeout + scanOverhead
}

// tool names the executable the current runner starts, for error messages.
func (s *Scanner) tool() string {
	if s.runner == RunnerBinary {
		return s.binary
	}
	return "docker"
}

// command builds the exec.Command for one scan of workDir, which must already
// be absolute and known to exist.
func (s *Scanner) command(workDir, projectKey string) (exec.Command, error) {
	switch s.runner {
	case RunnerDocker:
		return s.dockerCommand(workDir, projectKey), nil
	case RunnerBinary:
		return s.binaryCommand(workDir, projectKey), nil
	default:
		return exec.Command{}, fmt.Errorf("%w: %q (want %q or %q)",
			ErrUnknownRunner, string(s.runner), RunnerDocker, RunnerBinary)
	}
}

// dockerCommand builds the containerized invocation.
//
// The shape follows the SonarScanner CLI documentation's own example —
//
//	docker run --rm \
//	    -e SONAR_HOST_URL="http://${SONARQUBE_URL}" \
//	    -e SONAR_TOKEN="myAuthenticationToken" \
//	    -v "${YOUR_REPO}:/usr/src" \
//	    sonarsource/sonar-scanner-cli
//
// with three deliberate differences:
//
//   - Both -e flags are bare names, not NAME=value, so neither the token nor
//     the host URL enters belay's argv.
//   - -w ContainerWorkDir is set explicitly. The image already declares
//     WORKDIR /usr/src, so this is redundant against today's image and stays
//     correct against one whose WORKDIR moved.
//   - sonar.projectBaseDir is passed explicitly for the same reason. Both
//     guard the failure described on ContainerWorkDir: a base directory that
//     resolves to an empty path scans nothing and passes.
//
// Analysis parameters are positioned after the image name, which the image's
// entrypoint handles: it tests whether the first argument starts with a dash
// and, when it does, prepends sonar-scanner to it. The properties it derives
// from the environment (sonar.token, sonar.working.directory) are placed
// before these, and the scanner takes the last value for a repeated property,
// so nothing here can be clobbered by them.
func (s *Scanner) dockerCommand(workDir, projectKey string) exec.Command {
	args := []string{
		"run", "--rm",
		// Bare names: value comes from belay's environment, not argv.
		"-e", EnvHostURL,
		"-e", EnvToken,
		"-v", workDir + ":" + ContainerWorkDir,
		"-w", ContainerWorkDir,
		s.image,
	}
	args = append(args, s.properties(ContainerWorkDir, projectKey)...)
	return exec.Command{
		Path:      "docker",
		Args:      args,
		Dir:       workDir,
		EnvAllow:  dockerEnv,
		SecretEnv: secretEnv,
		Timeout:   s.execTimeout(),
		MaxOutput: DefaultMaxOutput,
	}
}

// binaryCommand builds the invocation for a locally installed sonar-scanner.
//
// There is no mount, so the base directory is the workspace itself and the
// scanner's own working directory (.scannerwork) is created inside it. That
// differs from the container path, where the image redirects
// sonar.working.directory to /tmp/.scannerwork and the workspace is left
// untouched.
func (s *Scanner) binaryCommand(workDir, projectKey string) exec.Command {
	return exec.Command{
		Path:      s.binary,
		Args:      s.properties(workDir, projectKey),
		Dir:       workDir,
		EnvAllow:  binaryEnv,
		SecretEnv: secretEnv,
		Timeout:   s.execTimeout(),
		MaxOutput: DefaultMaxOutput,
	}
}

// properties renders the -D analysis parameters shared by both runners.
//
// What is deliberately absent is as important as what is present:
//
//   - sonar.token and sonar.host.url: environment only. See [Scanner].
//   - sonar.sources: left unset so it defaults to the base directory, or to
//     whatever the repository's own sonar-project.properties declares.
//     Forcing it here would override a repository that has already scoped its
//     analysis, which is how a scan ends up indexing vendor/ and node_modules.
//   - sonar.working.directory: set by the image's entrypoint under
//     RunnerDocker, and left at the scanner's default under RunnerBinary.
func (s *Scanner) properties(baseDir, projectKey string) []string {
	props := make([]string, 0, 4)
	props = append(props,
		"-Dsonar.projectKey="+projectKey,
		"-Dsonar.projectBaseDir="+baseDir,
		"-Dsonar.qualitygate.wait="+strconv.FormatBool(s.gateWait),
	)
	if s.gateWait {
		seconds := int64(s.gateTimeout / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		props = append(props, "-Dsonar.qualitygate.timeout="+strconv.FormatInt(seconds, 10))
	}
	return props
}

// validateRequest checks everything that can be known before a process starts
// and returns the absolute workspace path.
func (s *Scanner) validateRequest(req belay.ReviewRequest) (string, error) {
	if strings.TrimSpace(req.ProjectKey) == "" {
		return "", ErrMissingProjectKey
	}
	workDir, err := absDir(req.WorkDir)
	if err != nil {
		return "", err
	}
	if !s.envSet(EnvToken) {
		return "", ErrMissingToken
	}
	if !s.envSet(EnvHostURL) {
		return "", ErrMissingHostURL
	}
	return workDir, nil
}

// envSet reports whether name has a non-blank value. The value itself is
// discarded immediately and never stored, logged, or wrapped into an error.
func (s *Scanner) envSet(name string) bool {
	v, ok := s.lookupEnv(name)
	return ok && strings.TrimSpace(v) != ""
}

// absDir resolves dir and requires it to be an existing directory.
//
// The existence check is the first of the three defenses against the
// silent-empty-scan failure described on ContainerWorkDir: `docker run -v` on
// a source path that does not exist creates it, empty, and the scan that
// follows reports a passing gate over zero files.
func absDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("%w: work directory is empty", ErrBadWorkDir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrBadWorkDir, dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrBadWorkDir, abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", ErrBadWorkDir, abs)
	}
	return abs, nil
}

// lastLine returns the final non-empty line of s, which is where the scanner
// prints the reason it stopped.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
