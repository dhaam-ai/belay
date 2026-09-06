//go:build unix

package conformance

import (
	"context"
	"io"
	"log/slog"
	"sync"

	"github.com/belay-dev/belay/internal/exec"
	"github.com/belay-dev/belay/internal/sonar/mcp"
)

// quietLogger discards everything, so a failing assertion's own output is
// what a test run shows, not a wall of structured logging from the
// reviewer under test.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// noEnv reports every environment variable as unset. It is used for every
// aireview reviewer this suite builds, so a developer's real SONAR_TOKEN
// (or lack of one) can never influence a test.
func noEnv(string) (string, bool) { return "", false }

// --- internal/sonar/scanner's seam: scanner.Execer ---

// stubExecer replays a scripted subprocess result without starting a
// process, standing in for internal/sonar/scanner's own stubExecer (which
// this package cannot import, being a _test.go helper of a different
// package). Its return values mirror exec.Runner.Run exactly, because
// scanner's classify leans on those semantics: a Result comes back even on
// failure, and a non-zero exit is reported as *exec.ExitError rather than
// nil.
type stubExecer struct {
	stdout   string
	exitCode int
	// err, when set, is returned verbatim instead of the exitCode-derived
	// error — this is how the "adapter could not run" scenario replays a
	// missing docker binary as *exec.ToolchainError without ever invoking
	// exec.LookPath for real.
	err error

	mu    sync.Mutex
	calls int
}

func (s *stubExecer) Run(_ context.Context, c exec.Command) (exec.Result, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()

	res := exec.Result{
		Args:     append([]string{c.Path}, c.Args...),
		ExitCode: s.exitCode,
		Stdout:   s.stdout,
	}
	switch {
	case s.err != nil:
		return res, s.err
	case s.exitCode != 0:
		return res, &exec.ExitError{Args: res.Args, Code: s.exitCode}
	default:
		return res, nil
	}
}

// --- internal/nodes/aireview's seam: aireview.Session ---

// fakeSession is a scripted aireview.Session: it replays a fixed quality
// gate status and a fixed page of issues, mirroring
// internal/nodes/aireview's own fakeSession (which, like stubExecer above,
// this package cannot import).
type fakeSession struct {
	status mcp.ProjectStatus
	issues []mcp.SonarIssue

	mu         sync.Mutex
	closeCalls int
}

func (s *fakeSession) GetProjectQualityGateStatus(context.Context, mcp.GetProjectQualityGateStatusRequest) (mcp.GetProjectQualityGateStatusResponse, error) {
	return mcp.GetProjectQualityGateStatusResponse{
		ProjectStatus: s.status,
		Raw:           []byte(`{"projectStatus":{}}`),
	}, nil
}

func (s *fakeSession) SearchSonarIssuesInProjects(context.Context, mcp.SearchSonarIssuesInProjectsRequest) (mcp.SearchSonarIssuesInProjectsResponse, error) {
	return mcp.SearchSonarIssuesInProjectsResponse{
		Total:  len(s.issues),
		Issues: s.issues,
		Raw:    []byte(`{"issues":[]}`),
	}, nil
}

func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	return nil
}
