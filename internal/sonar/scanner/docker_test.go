//go:build unix && sonardocker

// This file is excluded from every ordinary build. It is compiled only with
//
//	go test -tags sonardocker ./internal/sonar/scanner/...
//
// and even then it skips unless a real Docker daemon and a real SonarQube
// server are both reachable. ADR 0008 promises a contributor can run belay's
// suite with neither installed, so the end-to-end scan cannot be part of the
// default run — but the wiring it covers (a genuinely missing binary, a real
// container, a real gate verdict) is exactly the part a stub cannot prove, so
// it is kept runnable rather than deleted.
package scanner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	belayexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// requireDocker skips unless the docker binary is resolvable.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("skipping: docker is not on PATH (%v)", err)
	}
}

// requireSonarServer skips unless the SonarQube connection details are set.
func requireSonarServer(t *testing.T) {
	t.Helper()
	for _, name := range []string{EnvHostURL, EnvToken} {
		if os.Getenv(name) == "" {
			t.Skipf("skipping: %s is not set", name)
		}
	}
}

// TestDockerScanEndToEnd runs a real containerized scan against a real server.
//
// It asserts only what is true regardless of the server's quality-gate
// configuration: the scan reaches a verdict, and that verdict is a pass or a
// fail rather than an error. Asserting a specific gate would make the test a
// property of somebody's SonarQube project rather than of this package.
func TestDockerScanEndToEnd(t *testing.T) {
	requireDocker(t)
	requireSonarServer(t)

	dir := t.TempDir()
	src := "package main\n\nfunc main() { println(\"belay\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	projectKey := os.Getenv("BELAY_SONAR_TEST_PROJECT_KEY")
	if projectKey == "" {
		projectKey = "belay-scanner-e2e"
	}

	s := New(WithExecer(belayexec.New(nil)), WithQualityGateTimeout(120*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	report, err := s.Review(ctx, belay.ReviewRequest{
		WorkDir:    dir,
		ProjectKey: projectKey,
		FailOn:     belay.SeverityMajor,
	})
	if report.Gate == belay.GateError {
		t.Fatalf("scan did not reach a verdict: %v\nsummary: %s", err, report.Summary)
	}
	if err != nil {
		t.Fatalf("Review returned an error alongside gate %s: %v", report.Gate, err)
	}
	if report.Source != Source {
		t.Errorf("Source = %q, want %q", report.Source, Source)
	}
	if report.Issues == nil {
		t.Error("Issues is nil")
	}
	t.Logf("gate=%s summary=%s", report.Gate, report.Summary)
}

// TestDockerMissingBinaryIsToolchainError proves the translation against a
// genuinely absent binary rather than a stubbed error value.
//
// This is the one assertion a stub cannot make honestly: the stub returns an
// *exec.ToolchainError because the test told it to, whereas here internal/exec
// classifies a real failed lookup and the Scanner translates whatever it
// actually produced.
func TestDockerMissingBinaryIsToolchainError(t *testing.T) {
	dir := t.TempDir()
	s := New(
		WithExecer(belayexec.New(nil)),
		WithRunner(RunnerBinary),
		WithBinary("belay-sonar-scanner-that-does-not-exist"),
		WithLookupEnv(func(string) (string, bool) { return "set-for-this-test", true }),
	)

	report, err := s.Review(context.Background(), belay.ReviewRequest{
		WorkDir:    dir,
		ProjectKey: "belay-scanner-e2e",
		FailOn:     belay.SeverityMajor,
	})
	if !errors.Is(err, belay.ErrToolchainMissing) {
		t.Fatalf("errors.Is(err, belay.ErrToolchainMissing) = false; err = %v (%T)", err, err)
	}
	if report.Gate != belay.GateError {
		t.Errorf("Gate = %s, want %s", report.Gate, belay.GateError)
	}
}
