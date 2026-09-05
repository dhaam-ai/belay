//go:build unix

package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	bexec "github.com/belay-dev/belay/internal/exec"
	"github.com/belay-dev/belay/pkg/belay"
)

func TestCustomName(t *testing.T) {
	if got := NewCustom("make test", nil, quiet()).Name(); got != "custom" {
		t.Errorf("Name() = %q, want %q", got, "custom")
	}
}

func TestCustomDetect(t *testing.T) {
	tests := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{"configured", "make test", true},
		{"empty", "", false},
		{"whitespace only", "   \t ", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NewCustom(tc.cmdline, nil, quiet()).Detect(writeFiles(t, nil))
			if got != tc.want {
				t.Errorf("Detect() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCustomTest(t *testing.T) {
	tests := []struct {
		name     string
		cmdline  string
		fake     *fakeExec
		wantArgv []string
		want     wantReport
		wantOK   bool
	}{
		{
			name: "exit zero is a pass", cmdline: "make test",
			fake: execOK("ok\n"), wantArgv: []string{"make", "test"},
			want: wantReport{total: 1, passed: 1}, wantOK: true,
		},
		{
			name: "exit non-zero is one coarse failure", cmdline: "make test",
			fake: execExit("boom\n", 2), wantArgv: []string{"make", "test"},
			want: wantReport{
				total: 1, failed: 1,
				failures: []belay.TestFailure{{Name: "custom-command"}},
			},
		},
		{
			name:    "quotes are removed, metacharacters are not expanded",
			cmdline: `./run.sh "a b" $HOME && rm -rf /`,
			fake:    execOK(""),
			wantArgv: []string{
				"./run.sh", "a b", "$HOME", "&&", "rm", "-rf", "/",
			},
			want: wantReport{total: 1, passed: 1}, wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, nil)
			report, err := NewCustom(tc.cmdline, tc.fake, quiet()).Test(context.Background(), dir)
			if err != nil {
				t.Fatalf("Test() error = %v, want nil", err)
			}
			if got := strings.Join(tc.fake.argv(t), "|"); got != strings.Join(tc.wantArgv, "|") {
				t.Errorf("argv = %v, want %v", tc.fake.argv(t), tc.wantArgv)
			}
			assertReport(t, report, tc.want)
			if got := report.OK(); got != tc.wantOK {
				t.Errorf("report.OK() = %v, want %v", got, tc.wantOK)
			}
			if IsBuildFailure(report) {
				t.Error("IsBuildFailure() = true; the custom runner cannot know that")
			}
		})
	}
}

// TestCustomFailingCommandIsNotAnError is acceptance criterion 5 for custom.
func TestCustomFailingCommandIsNotAnError(t *testing.T) {
	report, err := NewCustom("make test", execExit("boom\n", 1), quiet()).
		Test(context.Background(), writeFiles(t, nil))
	if err != nil {
		t.Fatalf("Test() on a failing command returned error %v, want nil", err)
	}
	if report.Failed != 1 || report.OK() {
		t.Errorf("report = %+v, want one failure and OK() false", report)
	}
	if !strings.Contains(report.Failures[0].Message, "no per-test detail") {
		t.Errorf("failure message does not admit its limitation: %q", report.Failures[0].Message)
	}
	if !strings.Contains(report.Failures[0].Message, "boom") {
		t.Errorf("failure message dropped the command output: %q", report.Failures[0].Message)
	}
}

func TestCustomToolchainMissing(t *testing.T) {
	_, err := NewCustom("belay-no-such-tool --run", execMissing("belay-no-such-tool"), quiet()).
		Test(context.Background(), writeFiles(t, nil))
	assertToolchainMissing(t, err, "belay-no-such-tool")
}

func TestCustomTestErrors(t *testing.T) {
	tests := []struct {
		name    string
		cmdline string
		fake    *fakeExec
		wantErr error
	}{
		{name: "empty command", cmdline: "  ", fake: execOK(""), wantErr: ErrNoCommand},
		{name: "unterminated quote", cmdline: `make "test`, fake: execOK(""), wantErr: bexec.ErrUnterminatedQuote},
		{name: "command could not start", cmdline: "make test", fake: execStartFailure()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCustom(tc.cmdline, tc.fake, quiet()).
				Test(context.Background(), writeFiles(t, nil))
			var runErr *RunError
			if !errors.As(err, &runErr) {
				t.Fatalf("Test() error = %v (%T), want *RunError", err, err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(err, %v) = false; err = %v", tc.wantErr, err)
			}
			if errors.Is(err, belay.ErrToolchainMissing) {
				t.Error("a run failure must not masquerade as a missing toolchain")
			}
		})
	}
}
