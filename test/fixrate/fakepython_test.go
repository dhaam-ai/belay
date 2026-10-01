//go:build fixrate

package fixrate

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePythonEnv switches this test binary into fakePython. A test that needs
// the official harness hands the binary itself to Harness.Python, with this
// variable in Harness.Env: the harness's real exec path runs, and nothing
// needs Docker, Python or swebench.
const fakePythonEnv = "FIXRATE_FAKE_PYTHON"

// TestMain lets this test binary stand in for the Python interpreter the
// official harness runs under. See fakePython.
func TestMain(m *testing.M) {
	if os.Getenv(fakePythonEnv) != "" {
		os.Exit(fakePython(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// Markers a test writes into a prediction's patch to choose what the fake
// harness does with it. They are added lines, as an agent's edit would be.
const (
	markFixed      = "+FIXED"       // every expected test passes
	markDisagree   = "+DISAGREE"    // the report says resolved; the test log says a FAIL_TO_PASS test failed
	markUnapplied  = "+UNAPPLIABLE" // the patch does not apply, so no report is written
	markCrash      = "+CRASH"       // the harness process itself fails
	markNoStatuses = "+NO-STATUSES" // the status-map helper prints something that is not its answer
)

// fakePython imitates exactly the three ways this package runs Python:
// swebench.harness.run_evaluation, the status-map helper (sweBenchStatusScript)
// and the version probe. It writes what swebench 5.0.2 writes, where it
// writes it, from what it was given -- report.json, test_output.txt and
// run_instance.log under logs/evaluation/<run_id>/<model>/<instance_id>/ in
// its working directory -- and grades by the markers above instead of by
// running tests in a container.
func fakePython(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) >= 2 && args[0] == "-m" && args[1] == "swebench.harness.run_evaluation":
		return fakeRunEvaluation(args[2:], stdout, stderr)
	case len(args) >= 2 && args[0] == "-c" && strings.Contains(args[1], "get_logs_eval"):
		return fakeStatusMap(args[2:], stdout, stderr)
	case len(args) >= 2 && args[0] == "-c" && strings.Contains(args[1], "os.replace(partial, target)"):
		return fakeConvert(args[2:], stderr)
	case len(args) >= 2 && args[0] == "-c" && strings.Contains(args[1], "__version__"):
		_, err := fmt.Fprintln(stdout, SWEBenchHarnessVersion)
		return exitFor(err, stderr)
	default:
		_, err := fmt.Fprintf(stderr, "fake python: unexpected arguments %q\n", args)
		return 2 + exitFor(err, stderr)
	}
}

func exitFor(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	if _, werr := fmt.Fprintln(stderr, "fake python:", err); werr != nil {
		return 3
	}
	return 1
}

// fakeRunEvaluation is run_evaluation, graded by markers.
func fakeRunEvaluation(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run_evaluation", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataset := fs.String("dataset_name", "", "")
	split := fs.String("split", "test", "")
	preds := fs.String("predictions_path", "", "")
	runID := fs.String("run_id", "", "")
	fs.Int("max_workers", 4, "")
	fs.Int("timeout", 1800, "")
	var ids instanceIDs
	fs.Var(&ids, "instance_ids", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *split != "test" || *dataset == "" || *preds == "" || *runID == "" {
		return exitFor(fmt.Errorf("missing or wrong flags: %q", args), stderr)
	}
	if err := writeJSONFile(filepath.Join("logs", "fake-argv-"+strings.Join(ids, ",")+".json"), args); err != nil {
		return exitFor(err, stderr)
	}

	instances, err := readJSONL[map[string]any](*dataset)
	if err != nil {
		return exitFor(err, stderr)
	}
	predictions, err := readJSONL[Prediction](*preds)
	if err != nil {
		return exitFor(err, stderr)
	}
	for _, p := range predictions {
		if len(ids) > 0 && !ids.has(p.InstanceID) {
			continue
		}
		inst, ok := findInstance(instances, p.InstanceID)
		if !ok {
			return exitFor(fmt.Errorf("Some prediction IDs not found in dataset!\nMissing IDs:\n%s", p.InstanceID), stderr)
		}
		if strings.Contains(p.ModelPatch, markCrash) {
			return exitFor(errors.New("Traceback (most recent call last): docker.errors.DockerException"), stderr)
		}
		if err := fakeEvaluateOne(*runID, p, inst); err != nil {
			return exitFor(err, stderr)
		}
	}
	_, err = fmt.Fprintln(stdout, "All instances run.")
	return exitFor(err, stderr)
}

// fakeEvaluateOne writes one instance's evaluation logs.
func fakeEvaluateOne(runID string, p Prediction, inst map[string]any) error {
	dir := filepath.Join("logs", "evaluation", runID, strings.ReplaceAll(p.ModelNameOrPath, "/", "__"), p.InstanceID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if strings.Contains(p.ModelPatch, markUnapplied) {
		return os.WriteFile(filepath.Join(dir, "run_instance.log"),
			[]byte("Failed to apply patch to container: git apply --verbose\n>>>>> Patch Apply Failed:\nerror: patch failed: a.py:1\n"), 0o600)
	}
	f2p, p2p := fakeTestNames(inst["FAIL_TO_PASS"]), fakeTestNames(inst["PASS_TO_PASS"])
	fixed := strings.Contains(p.ModelPatch, markFixed)
	disagree := strings.Contains(p.ModelPatch, markDisagree)

	var log strings.Builder
	log.WriteString(">>>>> Start Test Output\n")
	status := map[string][]string{"FAIL_TO_PASS.success": {}, "FAIL_TO_PASS.failure": {}, "PASS_TO_PASS.success": {}, "PASS_TO_PASS.failure": {}}
	for i, name := range f2p {
		passed := fixed || (disagree && i > 0)
		if passed {
			status["FAIL_TO_PASS.success"] = append(status["FAIL_TO_PASS.success"], name)
			fmt.Fprintf(&log, "PASSED %s\n", name)
		} else {
			status["FAIL_TO_PASS.failure"] = append(status["FAIL_TO_PASS.failure"], name)
			fmt.Fprintf(&log, "FAILED %s\n", name)
		}
	}
	for _, name := range p2p {
		status["PASS_TO_PASS.success"] = append(status["PASS_TO_PASS.success"], name)
		fmt.Fprintf(&log, "PASSED %s\n", name)
	}
	log.WriteString(">>>>> End Test Output\n")
	if strings.Contains(p.ModelPatch, markNoStatuses) {
		log.WriteString("NO-STATUSES\n")
	}

	report := map[string]any{p.InstanceID: map[string]any{
		"patch_is_None":              false,
		"patch_exists":               true,
		"patch_successfully_applied": true,
		"resolved":                   fixed || disagree,
		"infra_failure":              false,
		"tests_status": map[string]any{
			"FAIL_TO_PASS": map[string]any{"success": status["FAIL_TO_PASS.success"], "failure": status["FAIL_TO_PASS.failure"]},
			"PASS_TO_PASS": map[string]any{"success": status["PASS_TO_PASS.success"], "failure": status["PASS_TO_PASS.failure"]},
			"FAIL_TO_FAIL": map[string]any{"success": []string{}, "failure": []string{}},
			"PASS_TO_FAIL": map[string]any{"success": []string{}, "failure": []string{}},
		},
	}}
	if err := os.WriteFile(filepath.Join(dir, "test_output.txt"), []byte(log.String()), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "run_instance.log"), []byte(">>>>> Applied Patch\n"), 0o600); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(dir, "report.json"), report)
}

// fakeStatusMap is the status-map helper: it reads back the statuses
// fakeEvaluateOne wrote into test_output.txt.
func fakeStatusMap(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 {
		return exitFor(fmt.Errorf("status helper wants dataset, instance and log, got %q", args), stderr)
	}
	data, err := os.ReadFile(args[2]) //nolint:gosec // the test log path the harness under test passed to its own helper
	if err != nil {
		return exitFor(err, stderr)
	}
	if strings.Contains(string(data), "NO-STATUSES") {
		_, err := fmt.Fprintln(stdout, "this is not the helper's answer")
		return exitFor(err, stderr)
	}
	statuses := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		if status, name, ok := strings.Cut(sc.Text(), " "); ok && (status == SWEBenchPassed || status == SWEBenchFailed) {
			statuses[name] = status
		}
	}
	return exitFor(json.NewEncoder(stdout).Encode(map[string]any{"found": len(statuses) > 0, "status_map": statuses}), stderr)
}

// fakeConvert is the conversion helper. The "parquet" file a test hands it is
// really JSON lines already, so converting is copying; each run is counted in
// a file beside the target, so a test can tell whether it ran at all.
func fakeConvert(args []string, stderr io.Writer) int {
	if len(args) != 2 {
		return exitFor(fmt.Errorf("convert wants a source and a target, got %q", args), stderr)
	}
	data, err := os.ReadFile(args[0]) //nolint:gosec // the source path the code under test passed
	if err != nil {
		return exitFor(err, stderr)
	}
	if strings.Contains(string(data), "CONVERT-FAILS") {
		return exitFor(errors.New("pyarrow.lib.ArrowInvalid: Could not open Parquet input source"), stderr)
	}
	runs := args[1] + ".runs"
	count, err := os.ReadFile(runs) //nolint:gosec // beside the target the code under test passed
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return exitFor(err, stderr)
	}
	if err := os.WriteFile(runs, append(count, 'x'), 0o600); err != nil { //nolint:gosec // beside the target the code under test passed
		return exitFor(err, stderr)
	}
	return exitFor(os.WriteFile(args[1], data, 0o600), stderr) //nolint:gosec // the target the code under test passed
}

// instanceIDs collects run_evaluation's --instance_ids, which takes one or
// more values; the fake is only ever given one per flag.
type instanceIDs []string

func (i *instanceIDs) String() string     { return strings.Join(*i, " ") }
func (i *instanceIDs) Set(v string) error { *i = append(*i, v); return nil }
func (i instanceIDs) has(id string) bool {
	for _, x := range i {
		if x == id {
			return true
		}
	}
	return false
}

func readJSONL[T any](path string) ([]T, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a path the harness under test passed
	if err != nil {
		return nil, err
	}
	var out []T
	dec := json.NewDecoder(strings.NewReader(string(data)))
	for {
		var v T
		if err := dec.Decode(&v); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, v)
	}
}

func findInstance(instances []map[string]any, id string) (map[string]any, bool) {
	for _, inst := range instances {
		if inst["instance_id"] == id {
			return inst, true
		}
	}
	return nil, false
}

func fakeTestNames(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func writeJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600) //nolint:gosec // a path under the fake harness's own working directory
}
