package policy_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	gate "github.com/dhaam-ai/belay/internal/nodes/policy"
	"github.com/dhaam-ai/belay/internal/policy"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// listed is a graph.ChangeSource answering from a fixed list.
type listed struct {
	paths []string
	err   error
}

func (l listed) Changes(context.Context, string) ([]string, error) { return l.paths, l.err }

// The reviewer's reproduction, at the gate: forbidden_paths infra/** and
// belay.yaml, an agent that edited both and reported only src/app.py. The
// gate judges what the workspace says changed, so the run ends here, and
// the verdict names what the agent left out.
func TestTheGateJudgesWhatTheWorkspaceSaysChangedNotTheReport(t *testing.T) {
	t.Parallel()
	rc := newRC(t, pin(policy.Policy{ForbiddenPaths: []string{"infra/**", "belay.yaml"}}, policy.Baseline{}),
		map[string]string{
			"src/app.py":    "x = 1\n",
			"infra/main.tf": "resource \"evil\" { backdoor = true }\n",
			"belay.yaml":    "version: 1\npolicy: {}\n",
		}, []string{"src/app.py"})
	rc.Changes = listed{paths: []string{"belay.yaml", "infra/main.tf", "src/app.py"}}
	rc.State.Code.Unreported = []string{"belay.yaml", "infra/main.tf"}

	res, err := gate.New().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != journal.StatusFailed {
		t.Fatalf("Status = %s, want %s: the change touched two forbidden paths (note %q)", res.Status, journal.StatusFailed, res.Note)
	}
	var files []string
	for _, v := range res.Patch.Policy.Violations {
		files = append(files, v.File)
	}
	if got := strings.Join(files, ","); got != "belay.yaml,infra/main.tf" {
		t.Errorf("violations name %q, want belay.yaml and infra/main.tf", got)
	}
	for _, fragment := range []string{"3 files checked", "2 the agent did not report", "belay.yaml, infra/main.tf"} {
		if !strings.Contains(res.Note, fragment) {
			t.Errorf("Note %q never says %q", res.Note, fragment)
		}
	}
}

// A gate that only has the agent's report says so, in the verdict itself,
// so the limitation is visible exactly where it applies.
func TestTheGateSaysWhenItJudgesOnlyTheAgentsReport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		changes graph.ChangeSource
		why     string
	}{
		{name: "a run that keeps no record", why: "a fork's copy keeps no record of where its source run began",
			changes: listed{err: fmt.Errorf("%w: a fork's copy keeps no record of where its source run began", graph.ErrUnrecorded)}},
		{name: "a run wired with no source", why: "", changes: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rc := newRC(t, pin(policy.Policy{ForbiddenPaths: []string{"infra/**"}}, policy.Baseline{}),
				map[string]string{"src/app.py": "x = 1\n"}, []string{"src/app.py"})
			rc.Changes = tc.changes
			res, err := gate.New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !strings.Contains(res.Note, "policy judges the files the agent reports") {
				t.Errorf("Note %q never says it judged only the agent's report", res.Note)
			}
			if !strings.Contains(res.Note, tc.why) {
				t.Errorf("Note %q never says why: %q", res.Note, tc.why)
			}
		})
	}
}

// A workspace that cannot answer is no verdict. Falling back to the report
// would let an agent that breaks the answer on purpose — a rewritten .git
// file — be judged on its own word.
func TestTheGateReachesNoVerdictWhenTheWorkspaceCannotAnswer(t *testing.T) {
	t.Parallel()
	rc := newRC(t, pin(policy.Policy{ForbiddenPaths: []string{"infra/**"}}, policy.Baseline{}),
		map[string]string{"src/app.py": "x = 1\n"}, []string{"src/app.py"})
	rc.Changes = listed{err: errors.New("git: the working tree's git directory is not this repository's")}

	res, err := gate.New().Run(context.Background(), rc)
	if err == nil {
		t.Fatalf("Run returned no error and %+v; a change nobody could check has not passed", res)
	}
	if res.Patch.Policy != nil {
		t.Errorf("Patch.Policy = %+v, want none: no verdict was reached", res.Patch.Policy)
	}
}

// On a disk that folds case, Infra/evil.tf and infra/evil.tf are one file,
// so a rule about one is a rule about the other — including for a new path
// git reports in the spelling the agent chose.
func TestTheGateFoldsCaseWhenTheDiskDoes(t *testing.T) {
	t.Parallel()
	for _, fold := range []bool{true, false} {
		t.Run(fmt.Sprintf("fold=%v", fold), func(t *testing.T) {
			t.Parallel()
			p := pin(policy.Policy{
				ForbiddenPaths:   []string{"infra/**"},
				RequiredTests:    []policy.TestRule{{When: "src/**", Require: "tests/**"}},
				ForbiddenContent: []policy.ContentRule{{Pattern: "SECRET", Paths: []string{"config/**"}}},
			}, policy.Baseline{})
			p.FoldCase = fold
			files := map[string]string{"INFRA/evil.tf": "x\n", "SRC/app.py": "x\n", "Config/app.ini": "SECRET\n"}
			changed := []string{"Config/app.ini", "INFRA/evil.tf", "SRC/app.py"}
			rc := newRC(t, p, files, changed)
			rc.Changes = listed{paths: changed}

			res, err := gate.New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			rules := map[string]bool{}
			for _, v := range res.Patch.Policy.Violations {
				rules[v.RuleID] = true
			}
			for _, rule := range []policy.Rule{policy.RuleForbiddenPath, policy.RuleRequiredTests, policy.RuleForbiddenContent} {
				if rules[string(rule)] != fold {
					t.Errorf("%s fired = %v, want %v when the disk folds case = %v", rule, rules[string(rule)], fold, fold)
				}
			}
			if fold && res.Patch.Policy.Gate != belay.GateFail {
				t.Errorf("Gate = %v, want fail", res.Patch.Policy.Gate)
			}
		})
	}
}
