package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

// hidingAgent is the reviewer's hostile agent: its plan is ordinary, and when
// it is given tools that edit, it writes every file in write and reports
// only the paths in report.
type hidingAgent struct {
	write  map[string]string
	report []string
}

func (a *hidingAgent) Name() string { return "hiding" }

func (a *hidingAgent) Invoke(_ context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
	if !slices.Contains(req.AllowedTools, "Edit") {
		return belay.AgentResponse{Text: "## Summary\nChange src/app.py.\n\n## Steps\n1. Edit src/app.py.\n", Turns: 1}, nil
	}
	for rel, body := range a.write {
		path := filepath.Join(req.WorkDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return belay.AgentResponse{}, err
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return belay.AgentResponse{}, err
		}
	}
	return belay.AgentResponse{
		Text:      "Done.\n\n```changed-files\n" + strings.Join(a.report, "\n") + "\n```\n",
		SessionID: "sess-hiding", Turns: 1,
	}, nil
}

const guardInfraAndConfig = `version: 1
graph:
  approval: false
policy:
  forbidden_paths: ["infra/**", "belay.yaml"]
`

// truthWorkspace is a workspace holding src/app.py and infra/main.tf, with
// the policy above, as a git repository with one commit or as a plain
// directory.
func truthWorkspace(t *testing.T, withGit bool, mode string) string {
	t.Helper()
	dir := goWorkspace(t)
	if withGit {
		requireGit(t)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	for rel, body := range map[string]string{
		"src/app.py":    "def add(a, b):\n    return a + b\n",
		"infra/main.tf": "resource \"x\" \"y\" {}\n",
		"belay.yaml":    guardInfraAndConfig + mode,
	} {
		path := filepath.Join(resolved, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	if withGit {
		gitIn(t, resolved, "init", "-q", "-b", "main")
		gitIn(t, resolved, "add", "-A")
		gitIn(t, resolved, "commit", "-qm", "first")
	}
	return resolved
}

// The finding, end to end through belay's own graph: an agent edits a
// forbidden infrastructure file and rewrites belay.yaml to switch its own
// policy off, and reports only src/app.py. In every kind of workspace belay
// can run in, the gate must judge what the workspace says changed, end the
// run, and name both files; and a branch run must commit neither.
func TestARunCannotHideAnEditFromItsPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		withGit bool
		mode    string
		report  []string
		from    state.ChangesFrom
	}{
		{name: "a branch run", withGit: true, mode: "output:\n  mode: branch\n", report: []string{"src/app.py"}, from: state.ChangesFromGit},
		{name: "a branch run with a mis-cased report", withGit: true, mode: "output:\n  mode: branch\n",
			report: []string{"src/app.py", "Infra/main.tf"}, from: state.ChangesFromGit},
		{name: "an in-place run in a git checkout", withGit: true, mode: "output:\n  mode: in_place\n",
			report: []string{"src/app.py"}, from: state.ChangesFromGit},
		{name: "an in-place run with no git", withGit: false, mode: "output:\n  mode: in_place\n",
			report: []string{"src/app.py"}, from: state.ChangesFromRecord},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := truthWorkspace(t, tc.withGit, tc.mode)
			plan, err := resolve(resolveRequest{workspace: dir, logger: quietLogger(), context: t.Context()})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			const runID = "20260101T000000Z-0b5e55ed0000"
			store, err := createRun(t.Context(), plan, runSpec{Workspace: dir, RunID: runID, Goal: "change src/app.py"})
			if err != nil {
				t.Fatalf("createRun: %v", err)
			}
			man, err := store.LoadManifest()
			if err != nil {
				t.Fatalf("LoadManifest: %v", err)
			}
			if man.Changes == nil || man.Changes.From != tc.from {
				t.Fatalf("manifest Changes = %+v, want the change read from %q", man.Changes, tc.from)
			}

			agent := &hidingAgent{
				write: map[string]string{
					"src/app.py":    "def add(a, b):\n    return a + b + 0\n",
					"infra/main.tf": "resource \"evil\" { backdoor = true }\n",
					"belay.yaml":    "version: 1\npolicy: {}\n",
				},
				report: tc.report,
			}
			runner := &belaytest.FakeRunner{Responses: []belay.TestReport{{Total: 1, Passed: 1}}}
			linter := &belaytest.FakeLinter{}
			view, err := dispatchOnce(t.Context(), plan, store, false, func(o *graph.Options) {
				o.Agent, o.Runner, o.Linter, o.Reviewer = agent, runner, linter, nil
			})
			if err != nil {
				t.Fatalf("dispatchOnce: %v", err)
			}
			if view.outcome.Status != state.RunStatusFailed || view.outcome.Node != graph.NodePolicy {
				t.Fatalf("run ended %v at %q (note %q), want failed at the policy gate",
					view.outcome.Status, view.outcome.Node, view.outcome.Note)
			}
			st, err := store.LoadState()
			if err != nil {
				t.Fatalf("LoadState: %v", err)
			}
			var judged []string
			for _, v := range st.Policy.Violations {
				judged = append(judged, v.File)
			}
			slices.Sort(judged)
			if want := []string{"belay.yaml", "infra/main.tf"}; !slices.Equal(judged, want) {
				t.Errorf("violations name %q, want %q", judged, want)
			}
			if !slices.Contains(st.Code.Unreported, "belay.yaml") {
				t.Errorf("Code.Unreported = %q, want it to name belay.yaml, which the agent never reported", st.Code.Unreported)
			}
			if runner.CallCount() != 0 {
				t.Errorf("the tests ran %d times after the gate refused the change", runner.CallCount())
			}
			if tc.mode == "output:\n  mode: branch\n" {
				base := gitIn(t, dir, "rev-parse", "main")
				if got := gitIn(t, dir, "rev-list", "--count", base+"..belay/"+runID); got != "0" {
					t.Errorf("belay/%s has %s commits beyond the base; nothing the gate refused may ship", runID, got)
				}
			}
		})
	}
}

// A dry run is where a person confirms what the guardrails will hold before
// leaving a run alone, so it says what the gate will judge: every file the
// workspace says changed, measured how, whatever the agent reports. And
// where there is nothing to measure against, it says the gate has only the
// agent's report, and why, where the limitation applies.
func TestTheDryRunSaysWhatTheGateJudges(t *testing.T) {
	t.Setenv(anthropicKeyEnv, "sk-ant-test")
	for _, tc := range []struct {
		name    string
		withGit bool
		mode    string
		want    []string
	}{
		{name: "a branch run", withGit: true, mode: "output:\n  mode: branch\n",
			want: []string{"every file git says the run changed since", "whether or not the agent reports it"}},
		{name: "an in-place run in a git checkout", withGit: true, mode: "output:\n  mode: in_place\n",
			want: []string{"every file git says changed since", "apart from what you had already changed"}},
		{name: "an in-place run with no git", withGit: false, mode: "output:\n  mode: in_place\n",
			want: []string{"is not a git repository", "records every file in it when the run starts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := truthWorkspace(t, tc.withGit, tc.mode)
			out, _, err := runCLI(t, "run", "--dry-run", "--workspace", dir, "change src/app.py")
			if err != nil {
				t.Fatalf("belay run --dry-run: %v\n%s", err, out)
			}
			for _, want := range append(tc.want, "nothing the agent writes outside the workspace") {
				if !strings.Contains(out, want) {
					t.Errorf("the dry run never says %q:\n%s", want, out)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, ".belay")); !os.IsNotExist(err) {
				t.Errorf("the dry run created .belay (stat error %v)", err)
			}
		})
	}
}

// The one case with no ground truth says so in the words the gate uses.
func TestTheDryRunLineForAGateWithOnlyTheReport(t *testing.T) {
	plan := &runPlan{changes: &state.Changes{From: state.ChangesReported, Why: "a fork works in a copy"}}
	if got := changesLine(plan); !strings.Contains(got, "policy judges the files the agent reports") ||
		!strings.Contains(got, "a fork works in a copy") {
		t.Errorf("changesLine = %q, want it to say the gate has only the agent's report, and why", got)
	}
}
