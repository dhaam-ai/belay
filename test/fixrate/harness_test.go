//go:build fixrate

package fixrate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeHarness returns a Harness whose Python is this test binary playing
// fakePython, over a dataset file holding the synthetic fixture records,
// and the Task for the first of them.
func fakeHarness(t *testing.T) (Harness, Task) {
	t.Helper()
	dir := t.TempDir()
	instances := swebenchFixture(t, "instances-array.json")
	var lines []byte
	for _, inst := range instances {
		b, err := json.Marshal(inst)
		if err != nil {
			t.Fatalf("marshal fixture instance: %v", err)
		}
		lines = append(append(lines, b...), '\n')
	}
	dataset := filepath.Join(dir, "instances.jsonl")
	if err := os.WriteFile(dataset, lines, 0o600); err != nil {
		t.Fatalf("write dataset: %v", err)
	}
	task, err := instances[0].Task(PinnedSource)
	if err != nil {
		t.Fatalf("Task(): %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return Harness{
		Python:  Python{Path: exe, Env: []string{fakePythonEnv + "=1"}},
		Dataset: dataset,
		Dir:     filepath.Join(dir, "evaluation"),
		RunID:   "fixrate-test",
	}, task
}

// patchWith is a prediction patch carrying one of fakePython's markers.
func patchWith(marker string) string {
	return "diff --git a/widget/core.py b/widget/core.py\n--- a/widget/core.py\n+++ b/widget/core.py\n" +
		"@@ -1 +1 @@\n-    return int(n * f)\n" + marker + "\n"
}

// TestHarness_Evaluate is the authority rule: the official harness's
// per-instance verdict is the score, and this package's port of its grading
// is the cross-check, run on the very test statuses the harness graded. A
// disagreement never changes the score; it is recorded, so the report can
// say it loudly.
func TestHarness_Evaluate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		patch string
		check func(t *testing.T, v Verdict)
	}{
		{
			name:  "a patch the official harness resolves",
			patch: patchWith(markFixed),
			check: func(t *testing.T, v Verdict) {
				if !v.Evaluated || !v.Resolved || !v.PatchApplied {
					t.Errorf("Verdict = %+v, want evaluated, applied and resolved", v)
				}
				if !v.Port.Checked || !v.Port.Resolved || v.Port.Disagrees {
					t.Errorf("Port = %+v, want a check that agrees: resolved", v.Port)
				}
			},
		},
		{
			name:  "a patch it does not resolve",
			patch: patchWith("+still broken"),
			check: func(t *testing.T, v Verdict) {
				if !v.Evaluated || v.Resolved {
					t.Errorf("Verdict = %+v, want evaluated and not resolved", v)
				}
				if !v.Port.Checked || v.Port.Resolved || v.Port.Disagrees {
					t.Errorf("Port = %+v, want a check that agrees: not resolved", v.Port)
				}
				if !strings.Contains(v.Port.Reason, "FAIL_TO_PASS") {
					t.Errorf("Port.Reason = %q, want it to say which set failed", v.Port.Reason)
				}
			},
		},
		{
			name:  "the official verdict is the score, and a disagreement is flagged",
			patch: patchWith(markDisagree),
			check: func(t *testing.T, v Verdict) {
				if !v.Resolved {
					t.Errorf("Resolved = false; the official harness said resolved, and its verdict is the score")
				}
				if !v.Port.Checked || v.Port.Resolved || !v.Port.Disagrees {
					t.Errorf("Port = %+v, want a check that disagrees", v.Port)
				}
			},
		},
		{
			name:  "a patch that does not apply leaves no report and does not resolve",
			patch: patchWith(markUnapplied),
			check: func(t *testing.T, v Verdict) {
				if v.Evaluated || v.Resolved {
					t.Errorf("Verdict = %+v, want not evaluated and not resolved", v)
				}
				if !strings.Contains(v.Error, "Patch Apply Failed") {
					t.Errorf("Error = %q, want the harness's own reason from run_instance.log", v.Error)
				}
				if v.Port.Checked || v.Port.Disagrees {
					t.Errorf("Port = %+v; with no report there is nothing to cross-check", v.Port)
				}
			},
		},
		{
			name:  "a cross-check that cannot run says why, and the score stands",
			patch: patchWith(markFixed) + markNoStatuses + "\n",
			check: func(t *testing.T, v Verdict) {
				if !v.Evaluated || !v.Resolved {
					t.Errorf("Verdict = %+v, want the official verdict to stand", v)
				}
				if v.Port.Checked || v.Port.Disagrees || v.Port.Unavailable == "" {
					t.Errorf("Port = %+v, want unchecked with a reason", v.Port)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, task := fakeHarness(t)
			v, err := h.Evaluate(t.Context(), task, "belay-r1", tt.patch)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			tt.check(t, v)
		})
	}
}

// TestHarness_Evaluate_RunsTheOfficialCommand pins the command line and the
// prediction record against the evaluation guide and run_evaluation.py at
// swebench 5.0.2: the verified dataset file as --dataset_name (load_swebench_dataset
// reads a local .jsonl), one instance, one worker, and a record carrying
// instance_id, model_name_or_path and model_patch.
func TestHarness_Evaluate_RunsTheOfficialCommand(t *testing.T) {
	t.Parallel()

	h, task := fakeHarness(t)
	h.Timeout = 900 * time.Second
	patch := patchWith(markFixed)
	if _, err := h.Evaluate(t.Context(), task, "control-r2", patch); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	preds := filepath.Join(h.Dir, "predictions", "control-r2", task.ID+".jsonl")
	data, err := os.ReadFile(preds) //nolint:gosec // this test's own temp directory
	if err != nil {
		t.Fatalf("reading the predictions file: %v", err)
	}
	var p map[string]string
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("the predictions file is not one JSON record: %v\n%s", err, data)
	}
	want := map[string]string{"instance_id": task.ID, "model_name_or_path": "control-r2", "model_patch": patch}
	if len(p) != len(want) || p["instance_id"] != want["instance_id"] ||
		p["model_name_or_path"] != want["model_name_or_path"] || p["model_patch"] != want["model_patch"] {
		t.Errorf("prediction = %v\nwant %v", p, want)
	}

	var argv []string
	argvData, err := os.ReadFile(filepath.Join(h.Dir, "logs", "fake-argv-"+task.ID+".json")) //nolint:gosec // this test's own temp directory
	if err != nil {
		t.Fatalf("the harness was not run in Harness.Dir: %v", err)
	}
	if err := json.Unmarshal(argvData, &argv); err != nil {
		t.Fatalf("decode argv: %v", err)
	}
	wantArgv := []string{
		"--dataset_name", h.Dataset, "--split", "test", "--instance_ids", task.ID,
		"--predictions_path", preds, "--max_workers", "1", "--run_id", h.RunID, "--timeout", "900",
	}
	if !slices.Equal(argv, wantArgv) {
		t.Errorf("run_evaluation arguments = %q\nwant %q", argv, wantArgv)
	}
}

// TestHarness_Evaluate_AnEmptyPatchIsNotEvaluated mirrors run_evaluation,
// which leaves an empty prediction out of the run and counts it as an empty
// patch: there is nothing to apply, so nothing can resolve.
func TestHarness_Evaluate_AnEmptyPatchIsNotEvaluated(t *testing.T) {
	t.Parallel()

	h, task := fakeHarness(t)
	v, err := h.Evaluate(t.Context(), task, "belay-r1", " \n")
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if !v.EmptyPatch || v.Evaluated || v.Resolved {
		t.Errorf("Verdict = %+v, want an empty patch, not evaluated, not resolved", v)
	}
	if _, err := os.Stat(filepath.Join(h.Dir, "logs")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the harness ran for an empty patch (stat logs: %v)", err)
	}
}

// TestHarness_Evaluate_AFailedHarnessIsAnError: a harness that could not run
// at all -- no Docker, a missing image, a crash -- has no verdict to give,
// and an unresolved verdict in its place would be a wrong number.
func TestHarness_Evaluate_AFailedHarnessIsAnError(t *testing.T) {
	t.Parallel()

	h, task := fakeHarness(t)
	v, err := h.Evaluate(t.Context(), task, "belay-r1", patchWith(markCrash))
	if err == nil {
		t.Fatalf("Evaluate() = %+v, nil error; want an error", v)
	}
	if !strings.Contains(err.Error(), "exit") || !strings.Contains(err.Error(), ".log") {
		t.Errorf("error = %v, want it to give the exit status and the log to read", err)
	}
}

func TestHarness_Evaluate_Refusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Harness, *Task, *string)
	}{
		{"no python", func(h *Harness, _ *Task, _ *string) { h.Python.Path = "" }},
		{"no dataset", func(h *Harness, _ *Task, _ *string) { h.Dataset = "" }},
		{"a relative dataset", func(h *Harness, _ *Task, _ *string) { h.Dataset = "instances.jsonl" }},
		{"no directory", func(h *Harness, _ *Task, _ *string) { h.Dir = "" }},
		{"no run id", func(h *Harness, _ *Task, _ *string) { h.RunID = "" }},
		{"an instance id that is a path", func(_ *Harness, task *Task, _ *string) { task.ID = "../escape" }},
		{"a system name that is a path", func(_ *Harness, _ *Task, system *string) { *system = "a/b" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, task := fakeHarness(t)
			system := "belay-r1"
			tt.mutate(&h, &task, &system)
			if _, err := h.Evaluate(t.Context(), task, system, patchWith(markFixed)); err == nil {
				t.Error("Evaluate() = nil error, want a refusal")
			}
		})
	}
}
