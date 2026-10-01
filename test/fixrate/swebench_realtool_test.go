//go:build fixrate

package fixrate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// These tests run this package's Python helpers against the real swebench,
// at the version the live command requires. Everything else in the package
// runs them against fakePython, which can only show that belay calls them as
// it means to; only the real package can show that it means the right thing.
// None of them needs Docker: they exercise the loader, the log reader and the
// command line, never a container.
//
// They skip unless FIXRATE_PYTHON names an interpreter with that swebench
// installed, so the suite still passes on a machine without it.

// sweBenchAgentPrompt is what a skipped test tells its reader to ask a coding
// agent, so the tests run next time.
const sweBenchAgentPrompt = "Create a Python 3.10+ virtualenv, install swebench==" + SWEBenchHarnessVersion +
	" into it, then run `FIXRATE_PYTHON=<that venv>/bin/python go test -tags fixrate ./test/fixrate -run TestRealSWEBench -v`" +
	" and confirm the tests pass instead of skipping."

// requireSWEBench returns the interpreter FIXRATE_PYTHON names, or skips t
// unless it imports swebench at SWEBenchHarnessVersion.
func requireSWEBench(t *testing.T) Python {
	t.Helper()
	py := Python{Path: os.Getenv("FIXRATE_PYTHON")}
	if py.Path == "" {
		t.Skipf("skipping: FIXRATE_PYTHON is not set. It must name a Python with swebench %s installed "+
			"(`python3 -m venv venv && venv/bin/pip install swebench==%s`), or ask your coding agent: %q",
			SWEBenchHarnessVersion, SWEBenchHarnessVersion, sweBenchAgentPrompt)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	out, err := py.output(ctx, "-c", "import swebench; print(swebench.__version__)")
	if version := strings.TrimSpace(string(out)); err != nil || version != SWEBenchHarnessVersion {
		t.Skipf("skipping: %s does not import swebench %s (printed %q, error %v). "+
			"Install it with `%s -m pip install swebench==%s`, or ask your coding agent: %q",
			py.Path, SWEBenchHarnessVersion, version, err, py.Path, SWEBenchHarnessVersion, sweBenchAgentPrompt)
	}
	return py
}

// TestRealSWEBench_ConvertsAParquetFile runs ConvertRecords's script through
// the harness's real loader, over a parquet file built from the synthetic
// fixture records with the pyarrow the harness's datasets dependency brings.
// What comes out must decode against the pinned schema and be the records
// that went in.
func TestRealSWEBench_ConvertsAParquetFile(t *testing.T) {
	py := requireSWEBench(t)
	dir := t.TempDir()

	jsonl := filepath.Join(dir, "synthetic.jsonl")
	if err := os.WriteFile(jsonl, syntheticRecords(t), 0o600); err != nil {
		t.Fatalf("write records: %v", err)
	}
	parquet := filepath.Join(dir, "test-00000-of-00001.parquet")
	const toParquet = "import json, sys\nimport pyarrow as pa, pyarrow.parquet as pq\n" +
		"rows = [json.loads(line) for line in open(sys.argv[1], encoding='utf-8')]\n" +
		"pq.write_table(pa.Table.from_pylist(rows), sys.argv[2])\n"
	if _, err := py.output(t.Context(), "-c", toParquet, jsonl, parquet); err != nil {
		t.Fatalf("building the parquet file: %v", err)
	}

	recs, err := ConvertRecords(t.Context(), py, parquet, filepath.Join(dir, "instances.jsonl"), 2)
	if err != nil {
		t.Fatalf("ConvertRecords() error = %v", err)
	}
	want := swebenchFixture(t, "instances-array.json")
	for i, got := range recs.Instances {
		w := want[i]
		if got.InstanceID != w.InstanceID || got.ProblemStatement != w.ProblemStatement || got.Image != w.Image ||
			!slices.Equal(got.FailToPass, w.FailToPass) || !slices.Equal(got.PassToPass, w.PassToPass) {
			t.Errorf("record %d = %+v\nwant %+v", i, got, w)
		}
	}
}

// TestRealSWEBench_ReadsTheHarnessLog runs the cross-check's script through
// the harness's real log reader, over a log in the shape pytest -rA prints
// between the eval script's markers, with the real parse_log_pytest parser.
func TestRealSWEBench_ReadsTheHarnessLog(t *testing.T) {
	py := requireSWEBench(t)

	inst := swebenchInstance(t, "instances-array.json", 0)
	inst.LogParser = "parse_log_pytest"
	line, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("marshal instance: %v", err)
	}
	dir := t.TempDir()
	dataset := filepath.Join(dir, "instances.jsonl")
	if err := os.WriteFile(dataset, append(line, '\n'), 0o600); err != nil {
		t.Fatalf("write dataset: %v", err)
	}
	task, err := inst.Task(PinnedSource)
	if err != nil {
		t.Fatalf("Task(): %v", err)
	}
	h := Harness{Python: py, Dataset: dataset, Dir: dir, RunID: "real-log"}

	tests := []struct {
		name         string
		halfUp       string
		wantResolved bool
	}{
		{"every expected test passed", "PASSED", true},
		{"the fail-to-pass test still fails", "FAILED", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "test_output.txt")
			content := ">>>>> Start Test Output\n" +
				"============================= test session starts ==============================\n" +
				"collected 3 items\n\n" +
				"=========================== short test summary info ============================\n" +
				tt.halfUp + " tests/test_core.py::test_resize_rounds_half_up\n" +
				"PASSED tests/test_core.py::test_resize_zero\n" +
				"PASSED tests/test_core.py::test_resize_identity\n" +
				">>>>> End Test Output\n"
			if err := os.WriteFile(log, []byte(content), 0o600); err != nil {
				t.Fatalf("write log: %v", err)
			}
			got := h.crossCheck(t.Context(), task, log, tt.wantResolved)
			if !got.Checked || got.Resolved != tt.wantResolved || got.Disagrees {
				t.Errorf("crossCheck() = %+v, want checked, resolved %v, agreeing", got, tt.wantResolved)
			}
		})
	}
}

// TestRealSWEBench_RunEvaluationTakesEveryFlagWePass guards the command line
// Evaluate builds against the real module's own argument parser, without
// running anything: --help lists every flag it accepts.
func TestRealSWEBench_RunEvaluationTakesEveryFlagWePass(t *testing.T) {
	py := requireSWEBench(t)

	out, err := py.output(t.Context(), "-m", "swebench.harness.run_evaluation", "--help")
	if err != nil {
		t.Fatalf("run_evaluation --help: %v", err)
	}
	for _, flag := range []string{
		"--dataset_name", "--split", "--instance_ids", "--predictions_path", "--max_workers", "--run_id", "--timeout",
	} {
		if !strings.Contains(string(out), flag) {
			t.Errorf("run_evaluation --help does not list %s:\n%s", flag, out)
		}
	}
}

// TestRealSWEBench_NamesItsLogDirectory runs the question Evaluate asks
// before every evaluation -- where does the installed harness write its
// logs? -- against the real package. The 5.0.2 release answers
// logs/run_evaluation.
func TestRealSWEBench_NamesItsLogDirectory(t *testing.T) {
	py := requireSWEBench(t)

	dir := t.TempDir()
	got, err := Harness{Python: py, Dir: dir}.logDir(t.Context())
	if err != nil {
		t.Fatalf("logDir() error = %v", err)
	}
	if want := filepath.Join(dir, "logs", "run_evaluation"); got != want {
		t.Errorf("logDir() = %q, want %q", got, want)
	}
}
