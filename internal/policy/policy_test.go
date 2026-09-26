package policy_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/policy"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/google/go-cmp/cmp"
)

// tree writes files into a fresh directory and returns it. The map is
// path -> content, with slash-separated relative paths.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

// rules returns the Rule of every violation, in order, which is what most
// of these tests assert on.
func rules(r policy.Report) []string {
	out := make([]string, 0, len(r.Violations))
	for _, v := range r.Violations {
		out = append(out, string(v.Rule))
	}
	return out
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		policy   policy.Policy
		baseline policy.Baseline
		files    map[string]string
		changed  []string
		want     []string
	}{
		{
			name:    "an empty policy has nothing to say",
			changed: []string{"infra/main.tf"},
			want:    []string{},
		},
		{
			name:    "a forbidden path",
			policy:  policy.Policy{ForbiddenPaths: []string{"infra/**"}},
			changed: []string{"internal/a.go", "infra/aws/main.tf"},
			want:    []string{"policy:forbidden_path"},
		},
		{
			name:    "a path the policy permits",
			policy:  policy.Policy{ForbiddenPaths: []string{"infra/**"}},
			changed: []string{"internal/a.go"},
			want:    []string{},
		},
		{
			name:    "one violation per forbidden path, so every one is named",
			policy:  policy.Policy{ForbiddenPaths: []string{"infra/**"}},
			changed: []string{"infra/a.tf", "infra/b.tf"},
			want:    []string{"policy:forbidden_path", "policy:forbidden_path"},
		},
		{
			name:    "too many files",
			policy:  policy.Policy{MaxChangedFiles: 2},
			changed: []string{"a.go", "b.go", "c.go"},
			want:    []string{"policy:max_changed_files"},
		},
		{
			name:    "exactly at the file ceiling is allowed",
			policy:  policy.Policy{MaxChangedFiles: 3},
			changed: []string{"a.go", "b.go", "c.go"},
			want:    []string{},
		},
		{
			name:    "zero means no ceiling at all",
			policy:  policy.Policy{MaxChangedFiles: 0, MaxChangedLines: 0},
			changed: []string{"a.go", "b.go", "c.go"},
			want:    []string{},
		},
		{
			name:    "too many lines",
			policy:  policy.Policy{MaxChangedLines: 4},
			files:   map[string]string{"a.go": "1\n2\n3\n", "b.go": "1\n2\n"},
			changed: []string{"a.go", "b.go"},
			want:    []string{"policy:max_changed_lines"},
		},
		{
			name:    "exactly at the line ceiling is allowed",
			policy:  policy.Policy{MaxChangedLines: 5},
			files:   map[string]string{"a.go": "1\n2\n3\n", "b.go": "1\n2\n"},
			changed: []string{"a.go", "b.go"},
			want:    []string{},
		},
		{
			name:    "a deleted file is not on disk and counts no lines",
			policy:  policy.Policy{MaxChangedLines: 1},
			changed: []string{"gone.go"},
			want:    []string{},
		},
		{
			name:     "a dependency the baseline did not have",
			policy:   policy.Policy{NoNewDependencies: true},
			baseline: policy.Baseline{Dependencies: []policy.Dependency{{Manifest: "go.mod", Name: "github.com/spf13/cobra"}}},
			files:    map[string]string{"go.mod": "module x\n\ngo 1.26\n\nrequire (\n\tgithub.com/spf13/cobra v1.8.0\n\tgithub.com/pkg/errors v0.9.1\n)\n"},
			changed:  []string{"go.mod"},
			want:     []string{"policy:no_new_dependencies"},
		},
		{
			name:     "a dependency the baseline already had",
			policy:   policy.Policy{NoNewDependencies: true},
			baseline: policy.Baseline{Dependencies: []policy.Dependency{{Manifest: "go.mod", Name: "github.com/spf13/cobra"}}},
			files:    map[string]string{"go.mod": "module x\n\ngo 1.26\n\nrequire github.com/spf13/cobra v1.9.0\n"},
			changed:  []string{"go.mod"},
			want:     []string{},
		},
		{
			name:    "a change with no test beside it",
			policy:  policy.Policy{RequiredTests: []policy.TestRule{{When: "**/*.go", Require: "**/*_test.go"}}},
			files:   map[string]string{"internal/a.go": "package a\n"},
			changed: []string{"internal/a.go"},
			want:    []string{"policy:required_tests"},
		},
		{
			name:   "a change with a test beside it",
			policy: policy.Policy{RequiredTests: []policy.TestRule{{When: "**/*.go", Require: "**/*_test.go"}}},
			files: map[string]string{
				"internal/a.go":      "package a\n",
				"internal/a_test.go": "package a\n",
			},
			changed: []string{"internal/a.go", "internal/a_test.go"},
			want:    []string{},
		},
		{
			name:    "a rule whose when matches nothing asks for nothing",
			policy:  policy.Policy{RequiredTests: []policy.TestRule{{When: "**/*.go", Require: "**/*_test.go"}}},
			changed: []string{"README.md"},
			want:    []string{},
		},
		{
			name:    "forbidden content",
			policy:  policy.Policy{ForbiddenContent: []policy.ContentRule{{Pattern: `AKIA[0-9A-Z]{16}`, Reason: "an AWS key"}}},
			files:   map[string]string{"a.go": "package a\n\nconst k = \"AKIA0123456789ABCDEF\"\n"},
			changed: []string{"a.go"},
			want:    []string{"policy:forbidden_content"},
		},
		{
			name:    "content the policy permits",
			policy:  policy.Policy{ForbiddenContent: []policy.ContentRule{{Pattern: `AKIA[0-9A-Z]{16}`}}},
			files:   map[string]string{"a.go": "package a\n"},
			changed: []string{"a.go"},
			want:    []string{},
		},
		{
			name: "a content rule scoped to paths it does not cover",
			policy: policy.Policy{ForbiddenContent: []policy.ContentRule{
				{Pattern: "TODO", Paths: []string{"**/*.go"}},
			}},
			files:   map[string]string{"notes.md": "TODO: later\n"},
			changed: []string{"notes.md"},
			want:    []string{},
		},
		{
			name: "every rule can fire at once, in rule order",
			policy: policy.Policy{
				ForbiddenPaths:    []string{"infra/**"},
				MaxChangedFiles:   1,
				MaxChangedLines:   1,
				NoNewDependencies: true,
				RequiredTests:     []policy.TestRule{{When: "**/*.go", Require: "**/*_test.go"}},
				ForbiddenContent:  []policy.ContentRule{{Pattern: "TODO"}},
			},
			baseline: policy.Baseline{},
			files: map[string]string{
				"infra/main.tf": "resource {}\n",
				"a.go":          "package a // TODO\n\nvar x = 1\n",
				"go.mod":        "module x\n\ngo 1.26\n\nrequire github.com/pkg/errors v0.9.1\n",
			},
			changed: []string{"a.go", "go.mod", "infra/main.tf"},
			want: []string{
				"policy:forbidden_path",
				"policy:max_changed_files",
				"policy:max_changed_lines",
				"policy:no_new_dependencies",
				"policy:required_tests",
				"policy:forbidden_content",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := tree(t, tt.files)
			got, err := policy.Evaluate(context.Background(), tt.policy, tt.baseline,
				policy.Change{Workspace: dir, Files: tt.changed})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if diff := cmp.Diff(tt.want, rules(got)); diff != "" {
				t.Errorf("Evaluate violations (-want +got):\n%s", diff)
			}
		})
	}
}

// The split between a violation that aborts the run and one the fix loop may
// repair is a property of the rule, never of how severe it happens to look.
// Asking the agent to un-touch a path it may not touch is not a repair.
func TestEveryRuleDeclaresWhoCanActOnIt(t *testing.T) {
	t.Parallel()

	want := map[policy.Rule]policy.Class{
		policy.RuleForbiddenPath:    policy.ClassFail,
		policy.RuleMaxChangedFiles:  policy.ClassFail,
		policy.RuleMaxChangedLines:  policy.ClassFail,
		policy.RuleNewDependency:    policy.ClassFix,
		policy.RuleRequiredTests:    policy.ClassFix,
		policy.RuleForbiddenContent: policy.ClassFix,
	}
	for _, rule := range policy.Rules() {
		class, known := want[rule]
		if !known {
			t.Errorf("%s is a rule with no declared class in this test; decide whether it aborts the run or routes to the fix loop", rule)
			continue
		}
		if got := rule.Class(); got != class {
			t.Errorf("%s.Class() = %s, want %s", rule, got, class)
		}
	}
	if len(policy.Rules()) != len(want) {
		t.Errorf("policy.Rules() has %d rules, this test knows %d", len(policy.Rules()), len(want))
	}
}

func TestReportVerdict(t *testing.T) {
	t.Parallel()

	fail := policy.Violation{Rule: policy.RuleForbiddenPath, Class: policy.ClassFail, Path: "infra/a.tf"}
	repair := policy.Violation{Rule: policy.RuleRequiredTests, Class: policy.ClassFix, Path: "a.go"}

	tests := []struct {
		name       string
		violations []policy.Violation
		clean      bool
		failed     bool
	}{
		{name: "nothing broken", violations: nil, clean: true},
		{name: "only repairable", violations: []policy.Violation{repair}},
		{name: "a fail-class violation", violations: []policy.Violation{repair, fail}, failed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := policy.Report{Violations: tt.violations}
			if got := r.Clean(); got != tt.clean {
				t.Errorf("Report.Clean() = %t, want %t", got, tt.clean)
			}
			if got := r.Failed(); got != tt.failed {
				t.Errorf("Report.Failed() = %t, want %t", got, tt.failed)
			}
		})
	}
}

// ADR-0006 says a quality verdict is one QualityReport whatever produced it.
// This proves the policy gate's violations really do fit that contract rather
// than merely being asserted to: every field the graph reads is populated,
// Issues is never nil, and a fail-class violation is the severity the
// contract reserves for work that must not ship.
func TestReportIsAConformingQualityReport(t *testing.T) {
	t.Parallel()

	r := policy.Report{Violations: []policy.Violation{
		{Rule: policy.RuleForbiddenPath, Class: policy.ClassFail, Path: "infra/a.tf", Message: "not yours to touch"},
		{Rule: policy.RuleForbiddenContent, Class: policy.ClassFix, Path: "a.go", Line: 3, Message: "an AWS key"},
	}}
	q := r.QualityReport()

	if q.Source == "" {
		t.Error("QualityReport.Source is empty; a report has to say what produced it")
	}
	if q.Gate != belay.GateFail {
		t.Errorf("QualityReport.Gate = %s, want %s", q.Gate, belay.GateFail)
	}
	if q.Issues == nil {
		t.Error("QualityReport.Issues is nil; the contract says an adapter with nothing to report returns an empty, non-nil slice")
	}
	if got, want := q.Counts.Total(), len(r.Violations); got != want {
		t.Errorf("Counts.Total() = %d, want %d", got, want)
	}
	if got := q.Counts.AtOrAbove(belay.SeverityBlocker); got != 1 {
		t.Errorf("Counts.AtOrAbove(blocker) = %d, want 1: a violation that aborts the run must not ship", got)
	}
	if q.Summary == "" {
		t.Error("QualityReport.Summary is empty")
	}
	for i, issue := range q.Issues {
		if issue.RuleID == "" || issue.Message == "" {
			t.Errorf("issue %d has no rule id or no message: %+v", i, issue)
		}
	}
	if len(q.Issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(q.Issues))
	}
	if q.Issues[1].Line != 3 {
		t.Errorf("issue line = %d, want 3: a forbidden-content match knows the line it is on", q.Issues[1].Line)
	}

	clean := policy.Report{}.QualityReport()
	if clean.Gate != belay.GatePass {
		t.Errorf("an empty report's Gate = %s, want %s", clean.Gate, belay.GatePass)
	}
	if clean.Issues == nil {
		t.Error("an empty report's Issues is nil; the contract's identical-shape guarantee breaks first for the emptiest report")
	}
}

func TestEvaluateRefusesAPolicyItCannotHonour(t *testing.T) {
	t.Parallel()

	_, err := policy.Evaluate(context.Background(),
		policy.Policy{ForbiddenPaths: []string{"../escape"}},
		policy.Baseline{}, policy.Change{Workspace: t.TempDir(), Files: []string{"a.go"}})
	if err == nil {
		t.Fatal("Evaluate with an uncompilable glob returned no error; a rule belay cannot evaluate must be no verdict, never a pass")
	}
	if !strings.Contains(err.Error(), "forbidden_paths") {
		t.Errorf("Evaluate error = %v; it must name the setting that cannot be honoured", err)
	}
}

func TestEvaluateIsDeterministic(t *testing.T) {
	t.Parallel()

	dir := tree(t, map[string]string{"a.go": "package a // TODO\n", "b.go": "package b // TODO\n"})
	p := policy.Policy{ForbiddenContent: []policy.ContentRule{{Pattern: "TODO"}}}
	first, err := policy.Evaluate(context.Background(), p, policy.Baseline{},
		policy.Change{Workspace: dir, Files: []string{"b.go", "a.go"}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for i := range 5 {
		again, err := policy.Evaluate(context.Background(), p, policy.Baseline{},
			policy.Change{Workspace: dir, Files: []string{"b.go", "a.go"}})
		if err != nil {
			t.Fatalf("Evaluate %d: %v", i, err)
		}
		if diff := cmp.Diff(first, again); diff != "" {
			t.Fatalf("Evaluate is not deterministic on run %d (-first +again):\n%s", i, diff)
		}
	}
	if len(first.Violations) != 2 {
		t.Fatalf("got %d violations, want 2", len(first.Violations))
	}
	if first.Violations[0].Path != "a.go" {
		t.Errorf("got %q first, want %q: the change is judged in path order, so two agents that reported "+
			"the same files in a different order get the same report", first.Violations[0].Path, "a.go")
	}
}
