//go:build fixrate

package fixrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/belay-dev/belay/pkg/belay"
)

func TestIsReadOnlyRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  belay.AgentRequest
		want bool
	}{
		{"plan node's read-only tools", belay.AgentRequest{AllowedTools: []string{"Read", "Glob", "Grep"}}, true},
		{"code node's edit tools", belay.AgentRequest{AllowedTools: []string{"Read", "Grep", "Glob", "Edit", "Write"}}, false},
		{"fix node's empty tool list (backend default)", belay.AgentRequest{AllowedTools: nil}, false},
		{"fix node's explicitly empty slice", belay.AgentRequest{AllowedTools: []string{}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isReadOnlyRequest(tt.req); got != tt.want {
				t.Errorf("isReadOnlyRequest(%v) = %v, want %v", tt.req.AllowedTools, got, tt.want)
			}
		})
	}
}

func TestChangedFilesReply(t *testing.T) {
	t.Parallel()

	empty := changedFilesReply(nil)
	if strings.Contains(empty, "```"+changedFilesTag) {
		t.Errorf("changedFilesReply(nil) = %q, must not emit a fenced block when nothing changed", empty)
	}

	reply := changedFilesReply([]string{"policy.go", "delay.go"})
	if !strings.Contains(reply, "```"+changedFilesTag+"\n") {
		t.Errorf("changedFilesReply(...) = %q, missing the fenced block open", reply)
	}
	for _, f := range []string{"policy.go", "delay.go"} {
		if !strings.Contains(reply, f) {
			t.Errorf("changedFilesReply(...) = %q, missing %q", reply, f)
		}
	}
}

func TestPackageNameOf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "x.go", "// comment\n\npackage backoff\n\nimport \"time\"\n")
	name, err := packageNameOf(filepath.Join(dir, "x.go"))
	if err != nil {
		t.Fatalf("packageNameOf() error = %v", err)
	}
	if name != "backoff" {
		t.Errorf("packageNameOf() = %q, want %q", name, "backoff")
	}
}

func TestSafeJoin_RefusesEscape(t *testing.T) {
	t.Parallel()
	if _, err := safeJoin("/work", "../etc/passwd"); err == nil {
		t.Error("safeJoin with \"..\" = nil error, want a refusal")
	}
	if _, err := safeJoin("/work", "/etc/passwd"); err == nil {
		t.Error("safeJoin with an absolute path = nil error, want a refusal")
	}
	got, err := safeJoin("/work", "policy.go")
	if err != nil {
		t.Fatalf("safeJoin() error = %v", err)
	}
	if want := filepath.Join("/work", "policy.go"); got != want {
		t.Errorf("safeJoin() = %q, want %q", got, want)
	}
}

func TestScriptedAgent_RepairFile_IsIdempotentAndByteIdentical(t *testing.T) {
	t.Parallel()
	pristine := t.TempDir()
	writeFile(t, pristine, "policy.go", "package backoff\n// correct\n")

	work := t.TempDir()
	writeFile(t, work, "policy.go", "package backoff\n// BROKEN\n")

	a := &scriptedAgent{pristineSrcDir: pristine, plan: ScriptedPlan{FileStrategy: map[string]Strategy{"policy.go": StrategyRepair}}}
	if err := a.repairFile(work, "policy.go"); err != nil {
		t.Fatalf("repairFile() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(work, "policy.go")) //nolint:gosec // work is this test's own t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "package backoff\n// correct\n" {
		t.Errorf("repairFile() left content %q, want the pristine content", got)
	}

	// Idempotent: applying again does not error and leaves the same bytes.
	if err := a.repairFile(work, "policy.go"); err != nil {
		t.Fatalf("repairFile() (2nd call) error = %v", err)
	}
	got2, err := os.ReadFile(filepath.Join(work, "policy.go")) //nolint:gosec // work is this test's own t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(got2) {
		t.Errorf("repairFile() is not idempotent: %q != %q", got, got2)
	}
}

func TestScriptedAgent_CheatFile_EmptiesTestFileButNotSource(t *testing.T) {
	t.Parallel()
	pristine := t.TempDir()
	writeFile(t, pristine, "ledger.go", "package backoff\n")

	work := t.TempDir()
	writeFile(t, work, "ledger.go", "package backoff\n// still broken\n")
	writeFile(t, work, "ledger_test.go", "package backoff\nfunc TestClassifyAndCount(t *testing.T){}\n")

	a := &scriptedAgent{pristineSrcDir: pristine, plan: ScriptedPlan{FileStrategy: map[string]Strategy{"ledger.go": StrategyCheat}}}
	testFile, err := a.cheatFile(work, "ledger.go")
	if err != nil {
		t.Fatalf("cheatFile() error = %v", err)
	}
	if testFile != "ledger_test.go" {
		t.Errorf("cheatFile() returned %q, want ledger_test.go", testFile)
	}

	gotTest, err := os.ReadFile(filepath.Join(work, "ledger_test.go")) //nolint:gosec // work is this test's own t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile(ledger_test.go) error = %v", err)
	}
	if strings.Contains(string(gotTest), "TestClassifyAndCount") {
		t.Errorf("ledger_test.go still contains the test after cheatFile: %q", gotTest)
	}
	if strings.TrimSpace(string(gotTest)) != "package backoff" {
		t.Errorf("ledger_test.go = %q, want just the package clause", gotTest)
	}

	gotSrc, err := os.ReadFile(filepath.Join(work, "ledger.go")) //nolint:gosec // work is this test's own t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile(ledger.go) error = %v", err)
	}
	if string(gotSrc) != "package backoff\n// still broken\n" {
		t.Errorf("cheatFile touched the source file: %q", gotSrc)
	}
}

func TestScriptedAgent_CheatFile_MissingTestFileIsAnError(t *testing.T) {
	t.Parallel()
	pristine := t.TempDir()
	writeFile(t, pristine, "ledger.go", "package backoff\n")
	work := t.TempDir()
	writeFile(t, work, "ledger.go", "package backoff\n")
	// No ledger_test.go.

	a := &scriptedAgent{pristineSrcDir: pristine, plan: ScriptedPlan{FileStrategy: map[string]Strategy{"ledger.go": StrategyCheat}}}
	if _, err := a.cheatFile(work, "ledger.go"); err == nil {
		t.Error("cheatFile() with no test file present = nil error, want one")
	}
}

func TestScriptedAgent_Noop_TouchesNothing(t *testing.T) {
	t.Parallel()
	pristine := t.TempDir()
	writeFile(t, pristine, "errors.go", "package backoff\n// correct\n")
	work := t.TempDir()
	writeFile(t, work, "errors.go", "package backoff\n// broken\n")

	a := &scriptedAgent{pristineSrcDir: pristine, plan: ScriptedPlan{FileStrategy: map[string]Strategy{"errors.go": StrategyNoop}}}
	touched, err := a.applyPlan(work)
	if err != nil {
		t.Fatalf("applyPlan() error = %v", err)
	}
	if len(touched) != 0 {
		t.Errorf("applyPlan() touched = %v, want none for StrategyNoop", touched)
	}
	got, err := os.ReadFile(filepath.Join(work, "errors.go")) //nolint:gosec // work is this test's own t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "package backoff\n// broken\n" {
		t.Errorf("StrategyNoop modified errors.go: %q", got)
	}
}

func TestScriptedAgent_Invoke_PlanCallReturnsNonEmptyText(t *testing.T) {
	t.Parallel()
	agent := NewScriptedAgent(t.TempDir(), ScriptedPlan{})
	resp, err := agent.Invoke(context.Background(), belay.AgentRequest{
		Prompt: "plan it", AllowedTools: []string{"Read", "Glob", "Grep"}, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if strings.TrimSpace(resp.Text) == "" {
		t.Error("Invoke() for a read-only (plan) request returned empty Text")
	}
	if resp.Usage != (belay.Usage{}) {
		t.Errorf("Invoke() Usage = %+v, want the zero value", resp.Usage)
	}
}

func TestScriptedAgent_Invoke_EditCallReportsChangedFiles(t *testing.T) {
	t.Parallel()
	pristine := t.TempDir()
	writeFile(t, pristine, "policy.go", "package backoff\n// correct\n")

	work := t.TempDir()
	writeFile(t, work, "policy.go", "package backoff\n// broken\n")

	agent := NewScriptedAgent(pristine, ScriptedPlan{FileStrategy: map[string]Strategy{"policy.go": StrategyRepair}})
	resp, err := agent.Invoke(context.Background(), belay.AgentRequest{
		Prompt: "implement it", AllowedTools: []string{"Read", "Grep", "Glob", "Edit", "Write"}, WorkDir: work,
	})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if !strings.Contains(resp.Text, "policy.go") {
		t.Errorf("Invoke() reply %q does not name policy.go", resp.Text)
	}
	if resp.Usage != (belay.Usage{}) {
		t.Errorf("Invoke() Usage = %+v, want the zero value (this is the replay-mode zero-cost guarantee's source of truth)", resp.Usage)
	}
	got, err := os.ReadFile(filepath.Join(work, "policy.go")) //nolint:gosec // work is this test's own t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "package backoff\n// correct\n" {
		t.Errorf("Invoke() did not repair policy.go on disk: %q", got)
	}
}
