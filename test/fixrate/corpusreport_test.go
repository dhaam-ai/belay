//go:build fixrate

package fixrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// secret returns a marker no aggregate could ever contain, for one kind of
// per-instance content.
func secret(kind string) string { return "LEAK-" + kind + "-5d1f" }

// leakyCells returns scored cells whose every per-instance field -- the task
// id, the issue, the test names, the patch, what the arm said, how it ended,
// the harness's own words -- carries a secret marker.
func leakyCells(t *testing.T) ([]scoredCell, []string) {
	t.Helper()
	var cells []scoredCell
	var ids []string
	for i, resolved := range []bool{true, false, true} {
		id := "leakorg__leakrepo-" + secret("ID") + string(rune('a'+i))
		ids = append(ids, id)
		task := Task{
			ID:         id,
			Corpus:     PinnedSource.String(),
			Goal:       "Resolve " + secret("ISSUE"),
			FailToPass: []string{"tests/test_x.py::" + secret("F2P")},
			PassToPass: []string{"tests/test_x.py::" + secret("P2P")},
			Repo:       "leakorg/" + secret("REPO"),
			BaseCommit: strings.Repeat("a", 40),
			EvalType:   "pass_and_fail",
		}
		for _, arm := range []Arm{ArmBelay, ArmControl} {
			for rep := 1; rep <= 2; rep++ {
				cells = append(cells, scoredCell{
					cell: Cell{Task: task, Arm: arm, Replicate: rep},
					result: CellResult{
						TaskID:      id,
						Arm:         arm,
						Replicate:   rep,
						Attempts:    1,
						Patch:       "diff --git a/x.py b/x.py\n+" + secret("PATCH") + "\n",
						UsageUSD:    0.5,
						Note:        secret("TRAJECTORY"),
						Adapters:    "test: changed test files in the instance container; gate: ruff",
						ShippedGate: "belay found a Python project in /corpus/cells/" + id + "/belay-r1/workspace",
					},
					verdict: &Verdict{
						Evaluated:    true,
						Resolved:     resolved == (arm == ArmBelay),
						PatchApplied: true,
						Error:        secret("HARNESS"),
						Port: PortCheck{
							Checked: true, Resolved: resolved == (arm == ArmBelay),
							Reason: "FAIL_TO_PASS not passing: tests/test_x.py::" + secret("F2P"),
						},
					},
				})
			}
		}
	}
	return cells, ids
}

// TestCorpusReport_PublishesAggregatesOnly enforces the rule the dataset's
// unknown licence imposes: the report may carry numbers about the corpus,
// never anything of an instance -- no patch, no trajectory, no issue text,
// no test name, not even a task id. It plants a marker in every
// per-instance field a cell can hold and fails if any of them, or any task
// id, reaches the report's JSON or its human summary.
//
// It also checks the report is not empty, so it cannot pass by publishing
// nothing.
func TestCorpusReport_PublishesAggregatesOnly(t *testing.T) {
	t.Parallel()

	cells, ids := leakyCells(t)
	m := CorpusManifest{
		Mode: ModeLive, Corpus: PinnedSource.String(), Selection: "test", Tasks: ids,
		Arms: []Arm{ArmBelay, ArmControl}, Replicates: 2,
	}
	report, err := buildCorpusReport(m, m.Arms, len(ids), cells, corpusLedger{recorded: 6}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("buildCorpusReport() error = %v", err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}

	for name, text := range map[string]string{"report.json": string(data), "report.txt": report.HumanSummary()} {
		if strings.Contains(text, "LEAK-") {
			i := strings.Index(text, "LEAK-")
			t.Errorf("%s carries per-instance content near %q", name, text[max(0, i-60):min(len(text), i+40)])
		}
		for _, id := range ids {
			if strings.Contains(text, id) {
				t.Errorf("%s names task %q; the report is aggregates only", name, id)
			}
		}
	}

	if report.Belay.Tasks != 3 || report.Belay.Resolved != 2 || report.Control.Resolved != 1 {
		t.Errorf("Belay = %+v, Control = %+v; the report must still carry the aggregates", report.Belay, report.Control)
	}
	if len(report.Replicates) != 2 || report.ShippedGateRefused != 3 || len(report.Evaluation) != 4 {
		t.Errorf("Replicates = %+v, ShippedGateRefused = %d, Evaluation = %+v; want both arms' A/A, 3 refusals, 4 systems",
			report.Replicates, report.ShippedGateRefused, report.Evaluation)
	}
}

// TestRunCorpus_WritesOnlyAggregatesToItsReportFiles checks the files a corpus
// run leaves for publishing, not just the type: report.json and report.txt
// in the corpus directory hold no task id and nothing of any patch.
func TestRunCorpus_WritesOnlyAggregatesToItsReportFiles(t *testing.T) {
	t.Parallel()

	tasks := widgetTasks(t, 1)
	dir := t.TempDir()
	opts := corpusOptions(t, dir, tasks, map[string]*cellAgent{"example__widget-1/control-r1": {repairFrom: 1}},
		&fakeEvaluator{}, newTemplateProvisioner())
	opts.Arms = []Arm{ArmControl}
	if _, err := RunCorpus(t.Context(), opts); err != nil {
		t.Fatalf("RunCorpus() error = %v", err)
	}
	for _, name := range []string{"report.json", "report.txt"} {
		data := readFile(t, filepath.Join(dir, name))
		for _, private := range []string{"example__widget-1", "rounding half up", "math.Floor", "Resize rounds down"} {
			if strings.Contains(string(data), private) {
				t.Errorf("%s carries %q, which is a cell's, not an aggregate", name, private)
			}
		}
	}
	// The private record is where it belongs.
	if _, err := os.Stat(filepath.Join(dir, "cells", "example__widget-1", "control-r1", "result.json")); err != nil {
		t.Errorf("the cell's private result is missing: %v", err)
	}
}
